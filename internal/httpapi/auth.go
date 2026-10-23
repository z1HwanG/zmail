package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionData 是签名 Cookie 中携带的会话信息。
type sessionData struct {
	Subject   string `json:"s"`
	CSRF      string `json:"c"`
	IssuedAt  int64  `json:"i"`
	ExpiresAt int64  `json:"e"`
}

func (s *sessionData) expired(now time.Time) bool {
	return s.ExpiresAt <= now.Unix()
}

type ctxKey int

const (
	sessionCtxKey ctxKey = iota
	clientIPCtxKey
)

func withSession(ctx context.Context, sess *sessionData) context.Context {
	return context.WithValue(ctx, sessionCtxKey, sess)
}

func sessionFromContext(ctx context.Context) *sessionData {
	sess, _ := ctx.Value(sessionCtxKey).(*sessionData)
	return sess
}

func withClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPCtxKey, ip)
}

func clientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPCtxKey).(string)
	return ip
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// signSession 生成 base64(payload).base64(hmac) 形式的 Cookie 值。
func (s *Server) signSession(sess sessionData) (string, error) {
	payload, err := json.Marshal(sess)
	if err != nil {
		return "", fmt.Errorf("序列化会话失败: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.cfg.SessionKey)
	if _, err := mac.Write([]byte(body)); err != nil {
		return "", err
	}
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig, nil
}

// parseSession 校验签名与有效期。
func (s *Server) parseSession(value string) (*sessionData, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, errors.New("会话格式非法")
	}
	mac := hmac.New(sha256.New, s.cfg.SessionKey)
	if _, err := mac.Write([]byte(parts[0])); err != nil {
		return nil, err
	}
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(got, want) {
		return nil, errors.New("会话签名不匹配")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("会话编码非法")
	}
	var sess sessionData
	if err := json.Unmarshal(payload, &sess); err != nil {
		return nil, fmt.Errorf("解析会话失败: %w", err)
	}
	if sess.ExpiresAt == 0 || sess.Subject == "" {
		return nil, errors.New("会话内容不完整")
	}
	if sess.expired(time.Now()) {
		return nil, errors.New("会话已过期")
	}
	return &sess, nil
}

func (s *Server) sessionFromRequest(r *http.Request) (*sessionData, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, err
	}
	return s.parseSession(c.Value)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, sess sessionData, ttl time.Duration) error {
	value, err := s.signSession(sess)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.IsHTTPS(),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(sess.ExpiresAt, 0),
		MaxAge:   int(ttl / time.Second),
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.IsHTTPS(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func isPublicPath(p string) bool {
	switch p {
	case "/login", "/healthz", "/favicon.ico", "/robots.txt":
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

// authenticate 校验会话；API 返回 401，页面重定向到 /login。
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := s.sessionFromRequest(r)
		if err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "需要登录")
				return
			}
			target := "/login"
			if r.Method == http.MethodGet && r.URL.RequestURI() != "/" {
				target += "?next=" + urlQueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	})
}

// verifyCSRF 校验写操作：X-CSRF-Token 匹配会话内 token，或 Origin/Referer 与 PUBLIC_URL 一致。
func (s *Server) verifyCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// 登录请求尚无会话，只做同源校验（允许无 Origin 的非浏览器客户端，失败次数受限速保护）。
		if r.URL.Path == "/login" {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin != "" && !isOpaqueOrigin(origin) && !s.sameOrigin(r, origin) {
				s.log.Warn("登录请求来源校验失败",
					"origin", origin, "host", r.Host, "publicUrl", s.cfg.PublicURL)
				s.rejectCrossOrigin(w, r, origin)
				return
			}
			if isOpaqueOrigin(origin) {
				// Origin: null 常见于浏览器隐私模式/沙箱 iframe/Referrer-Policy: no-referrer。
				// 登录本身有密码校验与限速，允许通过，避免用户被永久挡在门外。
				s.log.Debug("登录请求 Origin 为 null，按无 Origin 处理", "host", r.Host)
			}
			next.ServeHTTP(w, r)
			return
		}
		sess := sessionFromContext(r.Context())
		if sess == nil {
			writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "需要登录")
			return
		}
		if token := strings.TrimSpace(r.Header.Get(csrfHeaderName)); token != "" {
			if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
				writeError(w, r, http.StatusForbidden, codeForbidden, "CSRF token 不匹配")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !isOpaqueOrigin(origin) {
			if !s.sameOrigin(r, origin) {
				s.log.Warn("写请求来源校验失败", "origin", origin, "host", r.Host, "publicUrl", s.cfg.PublicURL, "path", r.URL.Path)
				s.rejectCrossOrigin(w, r, origin)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
			if !s.refererAllowed(r, ref) {
				s.log.Warn("写请求 Referer 校验失败", "referer", ref, "host", r.Host, "publicUrl", s.cfg.PublicURL, "path", r.URL.Path)
				s.rejectCrossOrigin(w, r, ref)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, r, http.StatusForbidden, codeForbidden, "缺少 CSRF 校验：请携带 X-CSRF-Token 或同源 Origin/Referer")
	})
}

// isOpaqueOrigin 判断 Origin 是否为浏览器给出的不透明来源（"null"）。
func isOpaqueOrigin(origin string) bool {
	v := strings.TrimSpace(origin)
	return v == "" || strings.EqualFold(v, "null")
}

// rejectCrossOrigin 拒绝跨站写请求。页面表单（登录页）返回可读的 HTML 提示，
// API 请求仍然返回统一 JSON 错误。
func (s *Server) rejectCrossOrigin(w http.ResponseWriter, r *http.Request, got string) {
	if r.URL.Path == "/login" && !wantsJSON(r) {
		s.renderPage(w, r, "login.html", pageData{
			Title:  "登录 · zMail",
			Error:  "来源校验失败：浏览器地址与 PUBLIC_URL 不一致（Origin: " + got + "，PUBLIC_URL: " + s.cfg.PublicURL + "）。请把 PUBLIC_URL 配置成实际访问的地址后重启，或直接用 PUBLIC_URL 中的地址打开。",
			Status: http.StatusForbidden,
		})
		return
	}
	writeError(w, r, http.StatusForbidden, codeForbidden, "跨站请求被拒绝：Origin/Referer 与 PUBLIC_URL 或当前请求 Host 不一致（请检查 PUBLIC_URL 配置）")
}

// ---------- 登录限速 ----------

type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, fails: make(map[string][]time.Time)}
}

func (l *loginLimiter) recent(ip string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	items := l.fails[ip]
	kept := items[:0]
	for _, t := range items {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}

func (l *loginLimiter) allowed(ip string) bool {
	if ip == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip, time.Now())) < l.max
}

func (l *loginLimiter) fail(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.fails) > 10000 {
		l.fails = make(map[string][]time.Time)
	}
	l.fails[ip] = append(l.recent(ip, now), now)
}

func (l *loginLimiter) reset(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// ---------- 登录 / 退出 ----------

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if sess, err := s.sessionFromRequest(r); err == nil && sess != nil {
		http.Redirect(w, r, "/mail", http.StatusSeeOther)
		return
	}
	errText := ""
	if r.URL.Query().Get("error") != "" {
		errText = "密码错误或登录已过期，请重试"
	}
	s.renderPage(w, r, "login.html", pageData{
		Title: "登录 · zMail",
		Next:  safeNext(r.URL.Query().Get("next")),
		Error: errText,
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := clientIPFrom(r.Context())
	if !s.loginLimiter.allowed(ip) {
		s.log.Warn("登录被限速", "ip", ip)
		if wantsJSON(r) {
			writeError(w, r, http.StatusTooManyRequests, codeRateLimited, "登录尝试过于频繁，请 5 分钟后再试")
			return
		}
		http.Error(w, "登录尝试过于频繁，请 5 分钟后再试", http.StatusTooManyRequests)
		return
	}

	password := ""
	next := ""
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var req struct {
			Password string `json:"password"`
			Next     string `json:"next"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "请求体格式错误")
			return
		}
		password = req.Password
		next = req.Next
	} else {
		if err := r.ParseForm(); err != nil {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "表单格式错误")
			return
		}
		password = r.PostFormValue("password")
		next = r.PostFormValue("next")
	}
	next = safeNext(next)
	if next == "" {
		next = "/mail"
	}

	expected := sha256.Sum256([]byte(s.cfg.AppPassword))
	got := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
		s.loginLimiter.fail(ip)
		s.log.Warn("登录失败", "ip", ip)
		if wantsJSON(r) {
			writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "密码错误")
			return
		}
		s.renderPage(w, r, "login.html", pageData{
			Title:  "登录 · zMail",
			Next:   next,
			Error:  "密码错误，请重试",
			Status: http.StatusUnauthorized,
		})
		return
	}
	s.loginLimiter.reset(ip)

	csrf, err := randomToken(32)
	if err != nil {
		s.log.Error("生成 CSRF token 失败", "err", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "无法创建会话")
		return
	}
	ttl := s.cfg.SessionTTL
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	now := time.Now()
	sess := sessionData{
		Subject:   "owner",
		CSRF:      csrf,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	}
	if err := s.setSessionCookie(w, sess, ttl); err != nil {
		s.log.Error("写入会话 Cookie 失败", "err", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "无法创建会话")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrfToken": csrf, "redirect": next})
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	if wantsJSON(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if sess, err := s.sessionFromRequest(r); err == nil && sess != nil {
		http.Redirect(w, r, "/mail", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext 只允许站内绝对路径，避免开放重定向。
func safeNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return ""
	}
	if strings.ContainsAny(next, "\\\r\n") {
		return ""
	}
	return next
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~', r == '/':
			b.WriteByte(r)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}
