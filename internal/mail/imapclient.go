package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
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

// validateIMAPHost 做 SSRF 防护：未开启 AllowPrivateIMAP 时拒绝内网/环回/链路本地地址。
func (m *Manager) validateIMAPHost(ctx context.Context, host string) error {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return fmt.Errorf("%w: IMAP 主机为空", errInvalidAccount)
	}
	if m.cfg != nil && m.cfg.AllowPrivateIMAP {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkPublicIP(host, ip)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("mail: 解析 IMAP 主机 %q 失败: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("mail: IMAP 主机 %q 没有可用的 A/AAAA 记录", host)
	}
	for _, a := range addrs {
		if err := checkPublicIP(host, a.IP); err != nil {
			return err
		}
	}
	return nil
}

// checkPublicIP 拒绝内网、环回、链路本地、组播与未指定地址。
func checkPublicIP(host string, ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("mail: IMAP 主机 %q 的地址无法识别", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("%w: 邮箱服务器主机 %q 指向内网地址 %s（如需自建内网邮箱请设置 ALLOW_PRIVATE_IMAP=true）",
			errPrivateHost, host, ip.String())
	}
	return nil
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
	if err := m.validateIMAPHost(ctx, host); err != nil {
		return nil, err
	}

	port := acc.IMAPPort
	if port <= 0 {
		port = 993
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
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

	caps, capErr := client.Capability()
	if capErr != nil {
		// 少数服务器在 LOGIN 后 CAPABILITY 返回异常，不影响后续 SELECT/FETCH。
		caps = make(map[string]bool)
	}
	ic := &imapConn{client: client, caps: caps, raw: conn, done: make(chan struct{})}
	ic.watchContext(ctx)
	return ic, nil
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
		"account is disabled", "no such user", "bad credentials",
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
