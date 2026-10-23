// Package mail 实现 zMail 的邮件引擎：IMAP 增量同步、IDLE 实时通知、MIME 解析、
// SMTP 发信与历史回填。
package mail

import "strings"

// Provider 描述一类邮箱服务商的连接参数。
type Provider struct {
	Key        string
	Name       string
	IMAPHost   string
	IMAPPort   int
	SMTPHost   string
	SMTPPort   int
	SMTPSecure bool
	Notes      string
}

// 服务商 Key 常量。
const (
	ProviderGmail   = "gmail"
	ProviderOutlook = "outlook"
	ProviderQQ      = "qq"
	Provider163     = "163"
	ProviderCustom  = "imap"
)

var providers = []Provider{
	{
		Key:        ProviderGmail,
		Name:       "Gmail",
		IMAPHost:   "imap.gmail.com",
		IMAPPort:   993,
		SMTPHost:   "smtp.gmail.com",
		SMTPPort:   465,
		SMTPSecure: true,
		Notes:      "需开启两步验证并使用应用专用密码；同步时跳过 [Gmail]/All Mail，避免与收件箱重复。",
	},
	{
		Key:        ProviderOutlook,
		Name:       "Outlook / Microsoft 365",
		IMAPHost:   "outlook.office365.com",
		IMAPPort:   993,
		SMTPHost:   "smtp-mail.outlook.com",
		SMTPPort:   587,
		SMTPSecure: false,
		Notes:      "SMTP 使用 587 STARTTLS；企业版 Microsoft 365 需管理员同意，部分账号仅支持 OAuth2。",
	},
	{
		Key:        ProviderQQ,
		Name:       "QQ 邮箱",
		IMAPHost:   "imap.qq.com",
		IMAPPort:   993,
		SMTPHost:   "smtp.qq.com",
		SMTPPort:   465,
		SMTPSecure: true,
		Notes:      "密码处填写授权码，并先在 QQ 邮箱设置中开启 IMAP/SMTP 服务。",
	},
	{
		Key:        Provider163,
		Name:       "163 邮箱",
		IMAPHost:   "imap.163.com",
		IMAPPort:   993,
		SMTPHost:   "smtp.163.com",
		SMTPPort:   465,
		SMTPSecure: true,
		Notes:      "密码处填写授权码；首次连接可能需要先发送 IMAP ID 命令，部分账号会限制并发连接数。",
	},
	{
		Key:        ProviderCustom,
		Name:       "自定义 IMAP",
		IMAPHost:   "",
		IMAPPort:   993,
		SMTPHost:   "",
		SMTPPort:   465,
		SMTPSecure: true,
		Notes:      "自建 Dovecot/Postfix 或未列出的服务商，需要手动填写 IMAP/SMTP 主机与端口。",
	},
}

// Providers 返回内置服务商列表（含自定义 IMAP 占位项）。
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// LookupProvider 按 key 查找服务商，key 不区分大小写。
// "custom" 与 "imap" 等价，均为自定义 IMAP 占位项。
func LookupProvider(key string) (Provider, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "custom" {
		key = ProviderCustom
	}
	for _, p := range providers {
		if equalFoldASCII(p.Key, key) {
			return p, true
		}
	}
	return Provider{}, false
}

// equalFoldASCII 做 ASCII 大小写无关比较，避免为一个小功能引入额外依赖。
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
