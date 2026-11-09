package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zmail/internal/config"
)

func TestSessionSignatureExpirationAndSafeNext(t *testing.T) {
	s := &Server{cfg: &config.Config{SessionKey: []byte("01234567890123456789012345678901")}}
	sess := sessionData{Subject: "owner", CSRF: "token", IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix()}
	value, err := s.signSession(sess)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := s.parseSession(value)
	if err != nil || parsed.CSRF != sess.CSRF {
		t.Fatalf("parsed=%#v err=%v", parsed, err)
	}
	if _, err = s.parseSession(value + "x"); err == nil {
		t.Fatal("tampered cookie accepted")
	}
	if got := safeNext("https://evil.example/path"); got != "" {
		t.Fatalf("external redirect allowed: %q", got)
	}
	if got := safeNext("/mail?folder=inbox"); got != "/mail?folder=inbox" {
		t.Fatalf("safe path changed: %q", got)
	}
	expired := sess
	expired.ExpiresAt = time.Now().Add(-time.Second).Unix()
	old, err := s.signSession(expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.parseSession(old); err == nil {
		t.Fatal("expired cookie accepted")
	}
}

func TestVerifyCSRFRejectsMissingAndAcceptsToken(t *testing.T) {
	s := &Server{cfg: &config.Config{PublicURL: "https://mail.example", SessionKey: []byte("01234567890123456789012345678901")}}
	sess := sessionData{Subject: "owner", CSRF: "csrf-token", IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
	handler := s.verifyCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodPost, "https://mail.example/api/messages", strings.NewReader("{}"))
	req = req.WithContext(withSession(req.Context(), &sess))
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, req)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("missing token status=%d", blocked.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "https://mail.example/api/messages", strings.NewReader("{}"))
	req.Header.Set(csrfHeaderName, sess.CSRF)
	req = req.WithContext(withSession(req.Context(), &sess))
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, req)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("valid token status=%d", allowed.Code)
	}
}
