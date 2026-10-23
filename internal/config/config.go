// Package config 负责从环境变量加载并校验 zMail 的运行配置。
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 保存进程启动所需的全部配置。
type Config struct {
	ListenAddr          string
	PublicURL           string
	PublicHost          string
	DBPath              string
	RawDir              string
	AttachDir           string
	AppPassword         string
	SessionKey          []byte
	CredentialsKey      []byte
	MaxFetchConcurrency int
	IdleRefresh         time.Duration
	BackfillEnabled     bool
	AllowPrivateIMAP    bool
	TrustedProxies      []*net.IPNet
	LogLevel            slog.Level
	SessionTTL          time.Duration
	// EnvFile 是本次启动实际加载的 .env 路径（未使用 .env 时为空）。
	EnvFile string
}

// Load 读取环境变量并返回配置。缺少必填项时返回清晰的错误。
//
// 读取顺序：先加载 .env（见 LoadDotEnv），再读取进程环境变量；进程环境变量优先。
func Load() (*Config, error) {
	envFile, err := LoadDotEnv()
	if err != nil {
		return nil, err
	}
	c := &Config{
		EnvFile:             envFile,
		ListenAddr:          env("LISTEN_ADDR", "127.0.0.1:7080"),
		DBPath:              env("DB_PATH", "./data/zmail.db"),
		RawDir:              env("RAW_DIR", "./data/raw"),
		BackfillEnabled:     envBool("BACKFILL_ENABLED", true),
		AllowPrivateIMAP:    envBool("ALLOW_PRIVATE_IMAP", false),
		MaxFetchConcurrency: envInt("MAX_FETCH_CONCURRENCY", 4),
		IdleRefresh:         time.Duration(envInt("IDLE_REFRESH_SECONDS", 1500)) * time.Second,
		SessionTTL:          30 * 24 * time.Hour,
	}

	var missing []string
	c.PublicURL = strings.TrimRight(env("PUBLIC_URL", ""), "/")
	if c.PublicURL == "" {
		missing = append(missing, "PUBLIC_URL")
	} else {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("config: PUBLIC_URL 不是合法 URL: %q", c.PublicURL)
		}
		c.PublicHost = u.Host
	}

	c.AppPassword = env("APP_PASSWORD", "")
	if c.AppPassword == "" {
		missing = append(missing, "APP_PASSWORD")
	}

	c.SessionKey, err = keyFromEnv("SESSION_KEY")
	if err != nil {
		return nil, err
	}
	if c.SessionKey == nil {
		missing = append(missing, "SESSION_KEY")
	}
	c.CredentialsKey, err = keyFromEnv("CREDENTIALS_KEY")
	if err != nil {
		return nil, err
	}
	if c.CredentialsKey == nil {
		missing = append(missing, "CREDENTIALS_KEY")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("config: 缺少必填环境变量: %s", strings.Join(missing, ", "))
	}

	if c.MaxFetchConcurrency < 1 {
		c.MaxFetchConcurrency = 1
	}
	if c.IdleRefresh < 60*time.Second {
		c.IdleRefresh = 1500 * time.Second
	}
	if c.AttachDir == "" {
		c.AttachDir = c.RawDir
	}

	c.LogLevel = parseLevel(env("LOG_LEVEL", "info"))

	c.TrustedProxies, err = parseCIDRs(env("TRUSTED_PROXY_CIDRS", "127.0.0.1/32,::1/128"))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// PublicPath 返回 PUBLIC_URL 携带的路径前缀（通常为空）。
func (c *Config) PublicPath() string {
	if c.PublicURL == "" {
		return ""
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// IsHTTPS 报告公开访问地址是否使用 HTTPS。
func (c *Config) IsHTTPS() bool {
	return strings.HasPrefix(c.PublicURL, "https://")
}

// TrustsProxy 判断给定远端地址是否为受信代理。
func (c *Config) TrustsProxy(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range c.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func keyFromEnv(name string) ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if b2, err2 := base64.RawURLEncoding.DecodeString(raw); err2 == nil {
			b = b2
		} else {
			return nil, fmt.Errorf("config: %s 必须是 base64 编码: %v", name, err)
		}
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("config: %s 解码后必须是 32 字节（AES-256），当前 %d 字节", name, len(b))
	}
	return b, nil
}

func parseCIDRs(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			if ip := net.ParseIP(part); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				part = fmt.Sprintf("%s/%d", part, bits)
			}
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("config: TRUSTED_PROXY_CIDRS 含非法 CIDR %q: %v", part, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
