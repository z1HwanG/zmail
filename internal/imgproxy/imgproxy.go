// Package imgproxy 提供邮件正文远程图片的安全代理。
//
// 目的：远程图片默认不加载，用户选择「显示图片」后由服务端代理拉取，
// 避免把客户端 IP 暴露给第三方追踪服务器，同时通过严格的地址校验防止 SSRF。
//
// 安全策略：
//   - 只允许 http/https，拒绝 file/gopher 等协议与带用户信息的 URL；
//   - 只允许公网 IP（含 IPv4/IPv6、NAT64、6to4），解析结果全部校验，防 DNS 重绑定；
//   - 端口仅允许 80/443/8080（可配置）；
//   - 响应体上限 5MB、整体超时 10s、最多 3 次重定向且逐跳校验；
//   - 只转发 image/* 响应，拒绝 SVG，响应使用私有缓存。
package imgproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMaxBytes 是图片响应体默认上限（5MB）。
	DefaultMaxBytes int64 = 5 << 20
	// DefaultTimeout 是单次代理请求的整体超时。
	DefaultTimeout = 10 * time.Second
	// DefaultMaxRedirects 是允许跟随的最大重定向次数。
	DefaultMaxRedirects = 3
	// DefaultCacheMaxAge 是响应私有缓存时间。
	DefaultCacheMaxAge = 24 * time.Hour
	// maxURLLength 是允许的图片地址最大长度。
	maxURLLength = 8 << 10
	// defaultUserAgent 是代理请求使用的 UA。
	defaultUserAgent = "zMail/1.0 (+image proxy)"
)

// 代理错误分类，便于 Handler 映射为合适的 HTTP 状态码。
var (
	// ErrInvalidURL 表示图片地址格式或协议不合法（→ 400）。
	ErrInvalidURL = errors.New("imgproxy: 图片地址非法")
	// ErrBlockedAddress 表示目标地址/端口被 SSRF 策略拒绝（→ 403）。
	ErrBlockedAddress = errors.New("imgproxy: 目标地址被拒绝")
	// ErrNotImage 表示上游返回的不是允许的图片类型（→ 502）。
	ErrNotImage = errors.New("imgproxy: 上游返回的不是图片")
	// ErrTooLarge 表示图片超过大小上限（→ 502）。
	ErrTooLarge = errors.New("imgproxy: 图片超过大小限制")
	// ErrUpstream 表示拉取失败（网络错误、非 200、重定向过多等，→ 502）。
	ErrUpstream = errors.New("imgproxy: 上游请求失败")
)

// Options 是图片代理配置；零值字段使用安全默认。
type Options struct {
	// MaxBytes 响应体上限，默认 5MB。
	MaxBytes int64
	// Timeout 整体超时，默认 10s。
	Timeout time.Duration
	// MaxRedirects 最大重定向次数，默认 3。
	MaxRedirects int
	// AllowedPorts 允许的显式端口，默认 80/443/8080。
	AllowedPorts []string
	// CacheMaxAge 私有缓存时间，默认 24h。
	CacheMaxAge time.Duration
	// UserAgent 拉取图片时使用的 UA。
	UserAgent string
	// Logger 日志器，默认 slog.Default()。
	Logger *slog.Logger
}

// Proxy 是远程图片代理。创建后并发安全，可长期复用。
type Proxy struct {
	maxBytes     int64
	timeout      time.Duration
	maxRedirects int
	allowedPorts map[string]bool
	cacheMaxAge  time.Duration
	userAgent    string
	log          *slog.Logger
	client       *http.Client
	resolveIP    func(ctx context.Context, host string) ([]net.IP, error)
}

// New 按配置创建代理；未填写的字段使用安全默认值。
func New(opts Options) *Proxy {
	p := &Proxy{
		maxBytes:     opts.MaxBytes,
		timeout:      opts.Timeout,
		maxRedirects: opts.MaxRedirects,
		cacheMaxAge:  opts.CacheMaxAge,
		userAgent:    opts.UserAgent,
		log:          opts.Logger,
	}
	if p.maxBytes <= 0 {
		p.maxBytes = DefaultMaxBytes
	}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	if p.maxRedirects <= 0 {
		p.maxRedirects = DefaultMaxRedirects
	}
	if p.cacheMaxAge <= 0 {
		p.cacheMaxAge = DefaultCacheMaxAge
	}
	if p.userAgent == "" {
		p.userAgent = defaultUserAgent
	}
	if p.log == nil {
		p.log = slog.Default()
	}

	p.allowedPorts = make(map[string]bool, 4)
	if len(opts.AllowedPorts) == 0 {
		for _, port := range []string{"80", "443", "8080"} {
			p.allowedPorts[port] = true
		}
	} else {
		for _, port := range opts.AllowedPorts {
			if trimmed := strings.TrimSpace(port); trimmed != "" {
				p.allowedPorts[trimmed] = true
			}
		}
	}

	p.resolveIP = defaultResolveIP
	p.client = &http.Client{
		// 禁止读环境代理：代理可能绕过地址校验。
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           p.dialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: p.timeout,
			ExpectContinueTimeout: time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		Timeout: p.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > p.maxRedirects {
				return fmt.Errorf("%w: 重定向超过 %d 次", ErrUpstream, p.maxRedirects)
			}
			// 逐跳校验重定向目标，防止跳转到内网或非 http(s)。
			if _, err := p.validateTarget(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	return p
}

// defaultProxy 是使用默认配置的共享代理实例。
var defaultProxy = New(Options{})

// Default 返回使用安全默认配置的共享代理。
func Default() *Proxy { return defaultProxy }

// Handler 是使用默认配置的包级处理器，可直接挂到 /api/proxy/image 路由。
func Handler(w http.ResponseWriter, r *http.Request) { defaultProxy.Handler(w, r) }

// Handler 处理 GET /api/proxy/image?u=<remote url>。
func (p *Proxy) Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "仅支持 GET/HEAD")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("u"))
	if raw == "" {
		writeError(w, http.StatusBadRequest, "缺少图片地址参数 u")
		return
	}
	p.ServeImage(w, r, raw)
}

// ServeImage 校验并代理指定远程图片，把结果直接写入响应。
func (p *Proxy) ServeImage(w http.ResponseWriter, r *http.Request, rawURL string) {
	ctx := r.Context()

	target, err := p.validateTarget(ctx, rawURL)
	if err != nil {
		p.writeFetchError(w, rawURL, err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "图片地址非法")
		return
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "image/*")

	resp, err := p.client.Do(req)
	if err != nil {
		p.writeFetchError(w, rawURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		p.log.Warn("图片代理: 上游返回非 200", "host", target.Host, "status", resp.StatusCode)
		writeError(w, http.StatusBadGateway, "图片拉取失败")
		return
	}

	contentType, err := imageContentType(resp.Header.Get("Content-Type"))
	if err != nil {
		p.log.Warn("图片代理: 上游类型不允许", "host", target.Host, "contentType", resp.Header.Get("Content-Type"))
		p.writeFetchError(w, rawURL, err)
		return
	}
	if resp.ContentLength > p.maxBytes {
		p.writeFetchError(w, rawURL, fmt.Errorf("%w: %d 字节", ErrTooLarge, resp.ContentLength))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBytes+1))
	if err != nil {
		p.writeFetchError(w, rawURL, fmt.Errorf("%w: %v", ErrUpstream, err))
		return
	}
	if int64(len(body)) > p.maxBytes {
		p.writeFetchError(w, rawURL, fmt.Errorf("%w: 超过 %d 字节", ErrTooLarge, p.maxBytes))
		return
	}

	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int64(p.cacheMaxAge.Seconds())))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		p.log.Warn("图片代理: 写出响应失败", "err", err)
	}
}

// writeFetchError 按错误分类写回状态码。
func (p *Proxy) writeFetchError(w http.ResponseWriter, rawURL string, err error) {
	switch {
	case errors.Is(err, ErrInvalidURL):
		writeError(w, http.StatusBadRequest, "图片地址必须是 http(s) URL")
	case errors.Is(err, ErrBlockedAddress):
		writeError(w, http.StatusForbidden, "图片地址被安全策略拒绝")
	default:
		p.log.Warn("图片代理: 拉取失败", "err", err, "length", len(rawURL))
		writeError(w, http.StatusBadGateway, "图片拉取失败")
	}
}

// imageContentType 校验并归一化上游 Content-Type，只放行非 SVG 的 image/*。
func imageContentType(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%w: 缺少 Content-Type", ErrNotImage)
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", fmt.Errorf("%w: Content-Type 非法", ErrNotImage)
	}
	mediaType = strings.ToLower(mediaType)
	if !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("%w: %s", ErrNotImage, mediaType)
	}
	// SVG 可以内嵌脚本，代理到同源后存在 XSS 风险，直接拒绝。
	if mediaType == "image/svg+xml" || strings.Contains(mediaType, "svg") {
		return "", fmt.Errorf("%w: 不支持 SVG", ErrNotImage)
	}
	return mediaType, nil
}

// writeError 写回纯文本错误并禁止缓存。
func writeError(w http.ResponseWriter, status int, message string) {
	header := w.Header()
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message)
}
