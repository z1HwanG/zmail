package config

import (
	"encoding/base64"
	"net"
	"os"
	"testing"
	"time"
)

func setConfigEnv(t *testing.T, values map[string]string) {
	t.Helper()
	keys := []string{"ENV_FILE", "PUBLIC_URL", "APP_PASSWORD", "SESSION_KEY", "CREDENTIALS_KEY", "TRUSTED_PROXY_CIDRS", "MAX_FETCH_CONCURRENCY", "IDLE_REFRESH_SECONDS", "ALLOW_PRIVATE_IMAP"}
	for _, key := range keys {
		old, ok := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	_ = os.Setenv("ENV_FILE", "-")
	for k, v := range values {
		_ = os.Setenv(k, v)
	}
}

func TestLoadConfigValidatesKeysAndProxyCIDRs(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	setConfigEnv(t, map[string]string{"PUBLIC_URL": "https://mail.example.test", "APP_PASSWORD": "secret", "SESSION_KEY": key, "CREDENTIALS_KEY": key, "TRUSTED_PROXY_CIDRS": "127.0.0.1/32", "MAX_FETCH_CONCURRENCY": "0", "IDLE_REFRESH_SECONDS": "20"})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxFetchConcurrency != 1 || cfg.IdleRefresh != 1500*time.Second || !cfg.TrustsProxy("127.0.0.1:1234") || cfg.TrustsProxy("10.0.0.1:1234") {
		t.Fatalf("unexpected config %#v", cfg)
	}
}

func TestLoadRejectsMalformedKeyAndCIDR(t *testing.T) {
	setConfigEnv(t, map[string]string{"PUBLIC_URL": "http://mail.example.test", "APP_PASSWORD": "x", "SESSION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 8)), "CREDENTIALS_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32))})
	if _, err := Load(); err == nil {
		t.Fatal("malformed session key accepted")
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	setConfigEnv(t, map[string]string{"PUBLIC_URL": "http://mail.example.test", "APP_PASSWORD": "x", "SESSION_KEY": key, "CREDENTIALS_KEY": key, "TRUSTED_PROXY_CIDRS": "bad-cidr"})
	if _, err := Load(); err == nil {
		t.Fatal("malformed CIDR accepted")
	}
}

func TestTrustsProxyIPv6(t *testing.T) {
	_, network, _ := net.ParseCIDR("::1/128")
	cfg := &Config{TrustedProxies: []*net.IPNet{network}}
	if !cfg.TrustsProxy("[::1]:8080") {
		t.Fatal("IPv6 loopback should be trusted")
	}
}
