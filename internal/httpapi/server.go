// Package httpapi 实现 zMail 的 HTTP 层：路由、会话、CSRF、SSE 与页面渲染。
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zmail/internal/config"
	"zmail/internal/imgproxy"
	"zmail/internal/mail"
	"zmail/internal/realtime"
	"zmail/internal/secret"
	"zmail/internal/store"
	"zmail/internal/web"
)

const (
	sessionCookieName = "zmail_session"
	csrfHeaderName    = "X-CSRF-Token"
	maxJSONBody       = 1 << 20
)

// Deps 是构造 Server 所需的全部依赖。
type Deps struct {
	Cfg   *config.Config
	Store *store.Store
	Hub   *realtime.Hub
	Mail  *mail.Manager
	Log   *slog.Logger
}

// Server 是 HTTP API 与页面服务的入口。
type Server struct {
	cfg  *config.Config
	st   *store.Store
	hub  *realtime.Hub
	mail *mail.Manager
	log  *slog.Logger
	box  *secret.Box
	img  *imgproxy.Proxy

	mux           *http.ServeMux
	handler       http.Handler
	staticHandler http.Handler
	templates     map[string]*template.Template
	loginLimiter  *loginLimiter
}

// New 构造 Server。Cfg/Store/Hub/Log 必填；Mail 允许为 nil（仅用于单测），
// 此时依赖 mail 的接口会返回 INTERNAL。
func New(d Deps) (*Server, error) {
	if d.Cfg == nil {
		return nil, errors.New("httpapi: Cfg 不能为空")
	}
	if d.Store == nil {
		return nil, errors.New("httpapi: Store 不能为空")
	}
	if d.Hub == nil {
		return nil, errors.New("httpapi: Hub 不能为空")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if len(d.Cfg.SessionKey) != 32 {
		return nil, fmt.Errorf("httpapi: SESSION_KEY 必须是 32 字节，当前 %d", len(d.Cfg.SessionKey))
	}
	box, err := secret.New(d.Cfg.CredentialsKey)
	if err != nil {
		return nil, fmt.Errorf("httpapi: 初始化凭据加密器失败: %w", err)
	}
	tmpls, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	if d.Mail == nil {
		d.Log.Warn("httpapi: Mail 未注入，账号与发信接口将不可用")
	}

	s := &Server{
		cfg:           d.Cfg,
		st:            d.Store,
		hub:           d.Hub,
		mail:          d.Mail,
		log:           d.Log,
		box:           box,
		img:           imgproxy.New(imgproxy.Options{Logger: d.Log}),
		staticHandler: http.FileServerFS(web.Static()),
		templates:     tmpls,
		loginLimiter:  newLoginLimiter(5, 5*time.Minute),
	}
	s.mux = s.routes()
	// 中间件顺序：recover -> 请求日志 -> 安全响应头 -> 客户端 IP -> 鉴权 -> CSRF。
	s.handler = s.recoverPanics(s.logRequests(s.securityHeaders(s.resolveClientIP(s.authenticate(s.verifyCSRF(s.mux))))))
	return s, nil
}

// Handler 返回装配好中间件的 http.Handler。
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// 公共端点。
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// 页面。
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /mail", s.handleMailPage)
	mux.HandleFunc("GET /settings/accounts", s.handleSettingsAccounts)
	mux.HandleFunc("GET /settings/general", s.handleSettingsGeneral)

	// 静态资源（embed 自 internal/web）。
	static := s.staticHandler
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sw.js") || r.URL.Path == "/sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		// Windows 上 FileServer 对 .webmanifest 可能回退成 octet-stream，这里显式指定。
		if strings.HasSuffix(strings.ToLower(r.URL.Path), ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		// 带版本指纹的 URL 长期缓存；不带版本时每次校验，避免升级后仍用旧 app.js。
		if r.URL.Query().Get("v") == web.Version() {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(w, r)
	})))

	// 账号。
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /api/accounts/{id}", s.handleGetAccount)
	mux.HandleFunc("PUT /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/test", s.handleTestAccount)
	mux.HandleFunc("POST /api/accounts/{id}/sync", s.handleSyncAccount)

	// 文件夹 / 邮件。
	mux.HandleFunc("GET /api/folders", s.handleListFolders)
	mux.HandleFunc("GET /api/messages", s.handleListMessages)
	mux.HandleFunc("GET /api/messages/{id}", s.handleGetMessage)
	mux.HandleFunc("PATCH /api/messages/{id}", s.handlePatchMessage)
	mux.HandleFunc("DELETE /api/messages/{id}", s.handleDeleteMessage)
	mux.HandleFunc("POST /api/messages/send", s.handleSendMessage)
	mux.HandleFunc("POST /api/messages/backfill", s.handleBackfill)
	mux.HandleFunc("GET /api/attachments/{id}", s.handleGetAttachment)
	mux.HandleFunc("GET /api/search", s.handleSearch)

	// 实时事件与图片代理。
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/proxy/image", s.handleProxyImage)

	// API 路径收到不支持的方法时返回统一的 JSON 错误（无方法模式是上面具体路由的兜底）。
	for _, pattern := range []string{
		"/api/accounts", "/api/accounts/{id}", "/api/accounts/{id}/test", "/api/accounts/{id}/sync",
		"/api/folders", "/api/messages", "/api/messages/{id}", "/api/attachments/{id}",
		"/api/search", "/api/events", "/api/proxy/image",
	} {
		mux.HandleFunc(pattern, s.handleMethodNotAllowed)
	}
	mux.HandleFunc("/", s.handleNotFound)

	return mux
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, r, http.StatusNotFound, codeNotFound, "接口不存在")
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusMethodNotAllowed, codeBadRequest, "不支持的 HTTP 方法")
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	status := "ok"
	code := http.StatusOK
	if err := s.st.Ping(ctx); err != nil {
		status = "error"
		code = http.StatusServiceUnavailable
		s.log.Warn("健康检查失败", "err", err)
	}
	writeJSON(w, code, map[string]any{"status": status, "time": time.Now().UTC()})
}

// ---------- 中间件 ----------

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	start       time.Time
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	if w.start.IsZero() {
		w.start = time.Now()
	}
	w.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.1f", float64(time.Since(w.start).Microseconds())/1000))
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Flush 让 SSE 可以立即把首帧推给客户端。
func (w *statusRecorder) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 支持 http.ResponseController。
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &statusRecorder{ResponseWriter: w, start: time.Now()}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("HTTP handler panic", "method", r.Method, "path", r.URL.Path, "panic", rec)
				if !rw.wroteHeader {
					writeError(w, r, http.StatusInternalServerError, codeInternal, "服务器内部错误")
				}
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, start: start}
		next.ServeHTTP(rw, r)
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		args := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rw.bytes,
			"dur_ms", float64(time.Since(start).Microseconds()) / 1000,
			"ip", clientIPFrom(r.Context()),
		}
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			s.log.Debug("http", args...)
			return
		}
		s.log.Info("http", args...)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// 注意：页面必须用 same-origin，不能用 no-referrer —— 后者会让 Chrome 把表单 POST
		// 的 Origin 头降级为 "null"，导致同源 CSRF 校验误判（登录报 403 跨站请求被拒绝）。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Referrer-Policy", "no-referrer")
		} else {
			h.Set("Referrer-Policy", "same-origin")
		}
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "public, max-age=3600")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		} else {
			h.Set("Content-Security-Policy", apiCSP)
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

const apiCSP = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

func pageCSP(nonce string) string {
	return "default-src 'none'; script-src 'self' 'nonce-" + nonce + "'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

func (s *Server) resolveClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		next.ServeHTTP(w, r.WithContext(withClientIP(r.Context(), ip)))
	})
}

// clientIP 解析真实客户端 IP：仅当直连方是受信代理时才采信 X-Forwarded-For。
func (s *Server) clientIP(r *http.Request) string {
	remote := hostOnly(r.RemoteAddr)
	if remote == "" {
		return ""
	}
	if !s.cfg.TrustsProxy(r.RemoteAddr) {
		return remote
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	chain := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			chain = append(chain, v)
		}
	}
	chain = append(chain, remote)
	for i := len(chain) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(chain[i])
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			continue
		}
		if s.cfg.TrustsProxy(ip + ":0") {
			continue
		}
		return ip
	}
	return remote
}

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

// requestScheme 返回请求实际使用的 scheme（受信代理下读取 X-Forwarded-Proto）。
func (s *Server) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.cfg.TrustsProxy(r.RemoteAddr) {
		if p := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); p == "http" || p == "https" {
			return p
		}
	}
	return "http"
}

// sameOrigin 判断 Origin 是否属于本站：要么等于 PUBLIC_URL，要么等于当前请求自身的 Host。
//
// 这样即使 PUBLIC_URL 与浏览器实际地址的主机名不同（例如 PUBLIC_URL 配的是 localhost、
// 用户却用 127.0.0.1 打开），同源写操作依然可用；跨站 Origin 仍然会被拒绝。
func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	got, err := url.Parse(origin)
	if err != nil || got.Scheme == "" || got.Host == "" {
		return false
	}
	if want, err := url.Parse(s.cfg.PublicURL); err == nil && want.Host != "" {
		if originMatches(got.Scheme, got.Host, want.Scheme, want.Host) {
			return true
		}
	}
	return originMatches(got.Scheme, got.Host, s.requestScheme(r), r.Host)
}

func (s *Server) refererAllowed(r *http.Request, referer string) bool {
	referer = strings.TrimSpace(referer)
	if referer == "" {
		return false
	}
	u, err := url.Parse(referer)
	if err != nil || u.Host == "" {
		return false
	}
	return s.sameOrigin(r, u.Scheme+"://"+u.Host)
}

// originMatches 比较 scheme 与 host:port；localhost / 127.0.0.1 / [::1] 在端口一致时视为等价。
func originMatches(schemeA, hostA, schemeB, hostB string) bool {
	if !strings.EqualFold(strings.TrimSpace(schemeA), strings.TrimSpace(schemeB)) {
		return false
	}
	ha, pa := splitHostPortDefault(hostA, schemeA)
	hb, pb := splitHostPortDefault(hostB, schemeB)
	if pa != pb {
		return false
	}
	if strings.EqualFold(ha, hb) {
		return true
	}
	return isLoopbackHost(ha) && isLoopbackHost(hb)
}

func splitHostPortDefault(hostport, scheme string) (string, string) {
	hostport = strings.TrimSpace(hostport)
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if port == "" {
		if strings.EqualFold(scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ---------- 响应辅助 ----------

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// 统一错误码。
const (
	codeBadRequest    = "BAD_REQUEST"
	codeUnauthorized  = "UNAUTHORIZED"
	codeForbidden     = "FORBIDDEN"
	codeNotFound      = "NOT_FOUND"
	codeConflict      = "CONFLICT"
	codeRateLimited   = "RATE_LIMITED"
	codeUpstreamError = "UPSTREAM_ERROR"
	codeInternal      = "INTERNAL"
)

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(errorEnvelope{Error: errorBody{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if v == nil {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeJSONBodyLimit(w, r, v, maxJSONBody)
}

// decodeJSONBodyLimit 与 decodeJSONBody 相同，但允许调用方指定更大的请求体上限（如发信附件）。
func decodeJSONBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		base := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
		if base != "application/json" && base != "application/x-json" {
			return fmt.Errorf("Content-Type 必须是 application/json")
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

func wantsJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/json") {
		return true
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(ct, "application/json")
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func queryInt(r *http.Request, name string, def, min, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def
	}
	n := 0
	ok := true
	for _, c := range raw {
		if c < '0' || c > '9' {
			ok = false
			break
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			ok = false
			break
		}
	}
	if !ok {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
