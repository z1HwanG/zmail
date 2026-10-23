package imgproxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// 内网/保留地址判断参考：RFC 1918、RFC 6598（CGNAT）、RFC 5737（文档网段）、
// RFC 3849（IPv6 文档前缀）、RFC 4193（唯一本地地址）等。

// isPublicIP 判断 IP 是否属于可安全访问的公网单播地址。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		return isPublicIPv4(ip4)
	}
	// NAT64 64:ff9b::/96 可能内嵌私网 IPv4。
	if len(ip) == net.IPv6len && ip[0] == 0 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b {
		return isPublicIPv4(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
	}
	// 6to4 2002::/16 前 4 字节之后内嵌 IPv4。
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x02 {
		return isPublicIPv4(net.IPv4(ip[2], ip[3], ip[4], ip[5]))
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if len(ip) == net.IPv6len {
		// 2001:db8::/32 文档前缀；fec0::/10 已废弃的站点本地地址。
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
			return false
		}
		if ip[0] == 0xfe && (ip[1]&0xc0) == 0xc0 {
			return false
		}
	}
	return true
}

func isPublicIPv4(ip net.IP) bool {
	if len(ip) != net.IPv4len {
		return false
	}
	switch {
	case ip[0] == 0: // 0.0.0.0/8
		return false
	case ip[0] == 10: // 10.0.0.0/8
		return false
	case ip[0] == 127: // 127.0.0.0/8
		return false
	case ip[0] == 169 && ip[1] == 254: // 169.254.0.0/16 链路本地
		return false
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31: // 172.16.0.0/12
		return false
	case ip[0] == 192 && ip[1] == 168: // 192.168.0.0/16
		return false
	case ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127: // 100.64.0.0/10 CGNAT
		return false
	case ip[0] == 192 && ip[1] == 0 && ip[2] == 0: // 192.0.0.0/24
		return false
	case ip[0] == 192 && ip[1] == 0 && ip[2] == 2: // 192.0.2.0/24 文档
		return false
	case ip[0] == 192 && ip[1] == 88 && ip[2] == 99: // 192.88.99.0/24 6to4 中继
		return false
	case ip[0] == 198 && (ip[1] == 18 || ip[1] == 19): // 198.18.0.0/15 基准测试
		return false
	case ip[0] == 198 && ip[1] == 51 && ip[2] == 100: // 198.51.100.0/24 文档
		return false
	case ip[0] == 203 && ip[1] == 0 && ip[2] == 113: // 203.0.113.0/24 文档
		return false
	case ip[0] >= 224: // 224.0.0.0/4 组播 + 240.0.0.0/4 保留 + 广播
		return false
	}
	return true
}

// isLocalHostname 快速拦截明显指向本机/内网的主机名，减少无谓的 DNS 查询。
func isLocalHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case h == "", h == "localhost":
		return true
	case strings.HasSuffix(h, ".localhost"),
		strings.HasSuffix(h, ".local"),
		strings.HasSuffix(h, ".internal"),
		strings.HasSuffix(h, ".home.arpa"):
		return true
	}
	return false
}

// bareHost 去掉 IPv6 地址可能携带的 zone 后缀（如 fe80::1%25eth0）。
func bareHost(host string) string {
	if i := strings.LastIndex(host, "%"); i >= 0 {
		return host[:i]
	}
	return host
}

// defaultResolveIP 使用系统解析器解析域名。
func defaultResolveIP(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

// portAllowed 校验显式端口；未显式指定端口时由 scheme 限定为 80/443。
func (p *Proxy) portAllowed(u *url.URL) bool {
	port := u.Port()
	if port == "" {
		return true
	}
	return p.allowedPorts[port]
}

// validateTarget 解析并校验目标 URL：协议、用户信息、端口、主机名与全部解析 IP。
func (p *Proxy) validateTarget(ctx context.Context, rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: 缺少图片地址", ErrInvalidURL)
	}
	if len(rawURL) > maxURLLength {
		return nil, fmt.Errorf("%w: 地址过长", ErrInvalidURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	u.Fragment = ""

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: 仅允许 http/https 协议", ErrInvalidURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: 不允许携带用户信息", ErrInvalidURL)
	}

	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: 缺少主机名", ErrInvalidURL)
	}
	if !p.portAllowed(u) {
		return nil, fmt.Errorf("%w: 端口 %s 不在允许范围", ErrBlockedAddress, u.Port())
	}
	if isLocalHostname(host) {
		return nil, fmt.Errorf("%w: 主机 %s", ErrBlockedAddress, host)
	}

	ipLiteral := bareHost(host)
	if ip := net.ParseIP(ipLiteral); ip != nil {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("%w: 地址 %s 不是公网地址", ErrBlockedAddress, ip)
		}
		return u, nil
	}

	ips, err := p.resolveIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法解析域名 %s: %v", ErrBlockedAddress, host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: 域名 %s 没有解析结果", ErrBlockedAddress, host)
	}
	// DNS 重绑定防护：所有解析结果都必须是公网地址。
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("%w: 域名 %s 解析到非公网地址 %s", ErrBlockedAddress, host, ip)
		}
	}
	return u, nil
}

// dialContext 是 Transport 的安全拨号器：自行解析、校验全部 IP，并直连校验过的 IP，
// 避免“校验后 DNS 变化”造成 TOCTOU 重绑定。
func (p *Proxy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: 地址 %s 非法", ErrBlockedAddress, addr)
	}
	host = bareHost(host)

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("%w: 地址 %s 不是公网地址", ErrBlockedAddress, ip)
		}
		ips = []net.IP{ip}
	} else {
		resolved, resolveErr := p.resolveIP(ctx, host)
		if resolveErr != nil {
			return nil, fmt.Errorf("imgproxy: 解析域名 %s 失败: %w", host, resolveErr)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("%w: 域名 %s 没有解析结果", ErrBlockedAddress, host)
		}
		for _, ip := range resolved {
			if !isPublicIP(ip) {
				return nil, fmt.Errorf("%w: 域名 %s 解析到非公网地址 %s", ErrBlockedAddress, host, ip)
			}
		}
		ips = resolved
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("imgproxy: 没有可用地址")
	}
	return nil, lastErr
}
