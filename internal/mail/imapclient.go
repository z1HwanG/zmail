package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"zmail/internal/store"
)

// IMAP 连接相关的超时与批量参数。
const (
	imapDialTimeout      = 10 * time.Second // 拨号超时
	imapHandshakeTimeout = 15 * time.Second // TLS 握手/读问候超时
	imapOpTimeout        = 60 * time.Second // 单条命令超时
	testConnTimeout      = 8 * time.Second  // 「测试连接」用的短超时
	imapFetchBatch       = 50               // 每批 FETCH/写入的邮件数
	maxRawMessageBytes   = 64 << 20         // 单封原始邮件上限
)

// imapConn 是一条已登录的 IMAP 连接及其能力快照。
type imapConn struct {
	client *imapclient.Client
	caps   map[string]bool
	raw    net.Conn

	closeOnce sync.Once
	done      chan struct{}
}

// hasCap 判断服务器是否声明了某项能力（如 IDLE / MOVE）。
func (c *imapConn) hasCap(name string) bool {
	if c == nil || c.caps == nil {
		return false
	}
	return c.caps[strings.ToUpper(name)]
}

// close 尽力优雅登出，失败时直接断开，避免泄漏连接。
func (c *imapConn) close() {
	if c == nil || c.client == nil {
		return
	}
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
		c.client.Timeout = 5 * time.Second
		if err := c.client.Logout(); err != nil {
			_ = c.client.Terminate()
		}
	})
}

// forceClose 立即中断连接上的读写，用于 Stop/ctx 取消时快速解除 IDLE/命令阻塞。
func (c *imapConn) forceClose() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
		if c.raw != nil {
			_ = c.raw.SetDeadline(time.Now())
			_ = c.raw.Close()
		}
		if c.client != nil {
			_ = c.client.Terminate()
		}
	})
}

// watchContext 在 ctx 取消时立即中断连接，保证 Stop 不必等待网络超时。
func (c *imapConn) watchContext(ctx context.Context) {
	if c == nil || c.done == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			c.forceClose()
		case <-c.done:
		}
	}()
}

// byteLiteral 让 []byte 满足 imap.Literal（io.Reader + Len）。
type byteLiteral struct {
	r *bytes.Reader
}

func newLiteral(data []byte) *byteLiteral {
	return &byteLiteral{r: bytes.NewReader(data)}
}

func (l *byteLiteral) Len() int { return l.r.Len() }

func (l *byteLiteral) Read(p []byte) (int, error) { return l.r.Read(p) }

// imapDebugLogger 把 go-imap 的内部日志桥接到 slog（debug 级别，不含凭据）。
type imapDebugLogger struct {
	log       *slog.Logger
	accountID string
}

func (l imapDebugLogger) Printf(format string, v ...interface{}) {
	if l.log == nil {
		return
	}
	l.log.Debug("imap: "+strings.TrimSpace(fmt.Sprintf(format, v...)), "account", l.accountID)
}

// Println 满足 go-imap 的 imap.Logger 接口。
func (l imapDebugLogger) Println(v ...interface{}) {
	if l.log == nil {
		return
	}
	l.log.Debug("imap: "+strings.TrimSpace(fmt.Sprintln(v...)), "account", l.accountID)
}

// decryptPassword 解密账号凭据；解密失败不打印任何密文内容。
func (m *Manager) decryptPassword(acc *store.Account) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("mail: 账号为空")
	}
	if len(acc.PasswordEnc) == 0 {
		return "", fmt.Errorf("mail: 账号 %s 没有保存密码", acc.ID)
	}
	pass, err := m.box.DecryptString(acc.PasswordEnc)
	if err != nil {
		return "", fmt.Errorf("mail: 解密账号 %s 的密码失败: %w", acc.ID, err)
	}
	return pass, nil
}

// resolveMailHost 校验目标全部地址，并返回供本次连接固定使用的 DNS 快照。
func (m *Manager) resolveMailHost(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return nil, fmt.Errorf("%w: IMAP 主机为空", errInvalidAccount)
	}
	if ip := net.ParseIP(host); ip != nil {
		if m.cfg == nil || !m.cfg.AllowPrivateIMAP {
			if err := checkPublicIP(host, ip); err != nil {
				return nil, err
			}
		}
		return []net.IP{ip}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("mail: 解析邮件主机 %q 失败: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("mail: 邮件主机 %q 没有可用的 A/AAAA 记录", host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if m.cfg == nil || !m.cfg.AllowPrivateIMAP {
			if err := checkPublicIP(host, a.IP); err != nil {
				return nil, err
			}
		}
		if a.IP.To4() != nil {
			ips = append(ips, append(net.IP(nil), a.IP.To4()...))
		} else {
			ips = append(ips, append(net.IP(nil), a.IP.To16()...))
		}
	}
	return ips, nil
}

// validateIMAPHost 保留用于兼容配置校验；拨号路径必须直接调用 resolveMailHost 并固定结果。
func (m *Manager) validateIMAPHost(ctx context.Context, host string) error {
	_, err := m.resolveMailHost(ctx, host)
	return err
}

// dialPinned 在不重新解析 DNS 的情况下，按已校验地址拨号。TLS 仍使用原主机名校验证书。
func dialPinned(ctx context.Context, port int, ips []net.IP, timeout time.Duration) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, fmt.Errorf("mail: 邮件主机没有已校验的 IP 地址")
	}
	dialer := &net.Dialer{Timeout: timeout}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// checkPublicIP 拒绝内网、环回、链路本地、组播与未指定地址。
func checkPublicIP(host string, ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("mail: 邮件主机 %q 的地址无法识别", host)
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("mail: 邮件主机 %q 的地址无法识别", host)
	}
	addr = addr.Unmap()
	// net.IP.IsGlobalUnicast 也会将部分特殊用途地址视为单播；仅放行 IANA 公网单播范围。
	if !mailPublicUnicast(addr) {
		return fmt.Errorf("%w: 邮箱服务器主机 %q 指向非公网地址 %s（如需自建内网邮箱请设置 ALLOW_PRIVATE_IMAP=true）",
			errPrivateHost, host, addr)
	}
	return nil
}

var mailNonPublicPrefixes = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "2001:db8::/32", "2001::/23",
		"2002::/16", "64:ff9b::/96",
	}
	prefixes := make([]netip.Prefix, 0, len(raw))
	for _, cidr := range raw {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	return prefixes
}()

func mailPublicUnicast(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range mailNonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// tlsConfigFor 生成用于 IMAP/SMTP 的 TLS 配置。
func tlsConfigFor(host string) *tls.Config {
	return &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
}

// openIMAP 建立 IMAP 连接并完成登录，返回带能力快照的连接。
//
// timeout 同时用于拨号、TLS 握手与读问候；单条命令的超时由 client.Timeout 控制。
func (m *Manager) openIMAP(ctx context.Context, acc *store.Account, timeout time.Duration) (*imapConn, error) {
	if acc == nil {
		return nil, fmt.Errorf("mail: 账号为空")
	}
	host := strings.TrimSpace(acc.IMAPHost)
	if host == "" {
		return nil, fmt.Errorf("%w: 账号 %s 的 IMAP 主机为空", errInvalidAccount, acc.ID)
	}
	if timeout <= 0 {
		timeout = imapDialTimeout
	}
	ips, err := m.resolveMailHost(ctx, host)
	if err != nil {
		return nil, err
	}

	port := acc.IMAPPort
	if port <= 0 {
		port = 993
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := dialPinned(ctx, port, ips, timeout)
	if err != nil {
		return nil, fmt.Errorf("mail: 连接 IMAP 服务器 %s 失败: %w", addr, err)
	}

	// 读服务器问候与 TLS 握手都设置短超时，避免连接挂死。
	_ = conn.SetDeadline(time.Now().Add(imapHandshakeTimeout))
	if acc.IMAPSecure {
		tlsConn := tls.Client(conn, tlsConfigFor(host))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("mail: IMAP TLS 握手失败: %w", err)
		}
		conn = tlsConn
	}
	client, err := imapclient.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mail: IMAP 服务器未返回有效问候: %w", err)
	}
	client.Timeout = imapOpTimeout
	if m.log != nil {
		client.ErrorLog = imapDebugLogger{log: m.log, accountID: acc.ID}
	}

	if !acc.IMAPSecure {
		ok, capErr := client.SupportStartTLS()
		switch {
		case capErr != nil:
			_ = client.Terminate()
			return nil, fmt.Errorf("mail: 探测 STARTTLS 能力失败: %w", capErr)
		case ok:
			if err := client.StartTLS(tlsConfigFor(host)); err != nil {
				_ = client.Terminate()
				return nil, fmt.Errorf("mail: IMAP STARTTLS 失败: %w", err)
			}
		default:
			if m.log != nil {
				m.log.Warn("IMAP 服务器不支持 STARTTLS，连接保持明文", "account", acc.ID)
			}
		}
	}
	_ = conn.SetDeadline(time.Time{})

	pass, err := m.decryptPassword(acc)
	if err != nil {
		_ = client.Terminate()
		return nil, err
	}
	if err := client.Login(acc.Username, pass); err != nil {
		_ = client.Logout()
		return nil, fmt.Errorf("mail: IMAP 登录失败: %w", err)
	}
	if needsIMAPID(acc) {
		if err := sendIMAPID(client); err != nil {
			_ = client.Logout()
			return nil, err
		}
	}

	caps, capErr := client.Capability()
	if capErr != nil {
		// 少数服务器在 LOGIN 后 CAPABILITY 返回异常，不影响后续 SELECT/FETCH。
		caps = make(map[string]bool)
	}
	ic := &imapConn{client: client, caps: caps, raw: conn, done: make(chan struct{})}
	ic.watchContext(ctx)
	return ic, nil
}

// needsIMAPID 判断网易邮箱账号是否要求登录后发送客户端标识。
func needsIMAPID(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(acc.Provider), Provider163) {
		return true
	}
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(acc.IMAPHost), ".")) {
	case "imap.163.com", "imap.126.com", "imap.yeah.net":
		return true
	default:
		return false
	}
}

// sendIMAPID 按网易邮箱要求在登录后声明客户端信息。
func sendIMAPID(client *imapclient.Client) error {
	cmd := &imap.Command{
		Name: "ID",
		Arguments: []interface{}{[]interface{}{
			"name", "zMail", "version", "1.0", "vendor", "zMail",
		}},
	}
	status, err := client.Execute(cmd, nil)
	if err != nil {
		return fmt.Errorf("mail: 发送 IMAP ID 客户端标识失败: %w", err)
	}
	if err := status.Err(); err != nil {
		return fmt.Errorf("mail: 发送 IMAP ID 客户端标识失败: %w", err)
	}
	return nil
}

// isAuthError 判断错误是否为认证失败（需要标记 auth_failed 并停止重试）。
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, k := range []string{
		"authenticationfailed", "authentication failed", "authentication failure",
		"invalid credentials", "invalid username", "invalid password",
		"username or password", "login failed", "auth failed", "authorization failed",
		"account is disabled", "no such user", "bad credentials", "unsafe login",
	} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// isTimeoutError 判断错误是否为网络超时。
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if ok := asNetError(err, &ne); ok && ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "i/o deadline") ||
		strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection closed")
}

// asNetError 是 errors.As 的薄封装，避免在各处重复导入 errors。
func asNetError(err error, target *net.Error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok {
			*target = ne
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// hasFlag 判断 IMAP FLAGS 中是否包含指定标记（大小写不敏感）。
func hasFlag(flags []string, target string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, target) {
			return true
		}
	}
	return false
}

// folderPriority 返回文件夹角色的默认同步优先级（数值越小越优先）。
func folderPriority(role string) int {
	switch role {
	case store.RoleInbox:
		return 10
	case store.RoleSent:
		return 20
	case store.RoleDrafts:
		return 30
	case store.RoleArchive:
		return 60
	case store.RoleSpam:
		return 80
	case store.RoleTrash:
		return 90
	default:
		return 100
	}
}

// defaultEnabled 返回文件夹角色默认是否参与同步（垃圾箱/垃圾邮件默认关闭）。
func defaultEnabled(role string) bool {
	switch role {
	case store.RoleTrash, store.RoleSpam:
		return false
	default:
		return true
	}
}

var _ = imap.InboxName
