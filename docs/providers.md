# zMail 邮箱服务商接入指南

本文列出 Gmail、Outlook/Microsoft 365、QQ 邮箱、163 邮箱与自建 Dovecot 的 IMAP/SMTP 参数、授权码申请步骤、
IDLE 支持情况和常见错误。zMail 的账号表单与 `POST /api/accounts` 使用同一组字段：

```json
{
  "provider": "gmail",
  "email": "me@gmail.com",
  "displayName": "个人 Gmail",
  "username": "me@gmail.com",
  "password": "应用专用密码或授权码",
  "imapHost": "imap.gmail.com",
  "imapPort": 993,
  "imapSecure": true,
  "smtpHost": "smtp.gmail.com",
  "smtpPort": 465,
  "smtpSecure": true
}
```

> 通用原则：`password` 存的是**应用专用密码/授权码**，不是网页登录密码；zMail 会用它做 AES-256-GCM 加密后落库。

## 1. 参数速查表

| 服务商 | IMAP | SMTP | 认证方式 | IDLE | 备注 |
|---|---|---|---|---|---|
| Gmail | `imap.gmail.com:993` SSL | `smtp.gmail.com:465` SSL（或 587 STARTTLS） | 应用专用密码（需开两步验证） | 支持 | 跳过 `[Gmail]/All Mail`，否则 INBOX 重复 |
| Outlook / Microsoft 365 | `outlook.office365.com:993` SSL | `smtp.office365.com:587` STARTTLS | 应用密码；Microsoft 已停用基本认证，需 OAuth2/XOAUTH2 | 支持 | 连接数限制较严，避免频繁重连 |
| QQ 邮箱 | `imap.qq.com:993` SSL | `smtp.qq.com:465` SSL | 授权码（需先开启 IMAP/SMTP 服务） | 支持 | 文件夹名为中文，已发送为「已发送」 |
| 163 邮箱 | `imap.163.com:993` SSL | `smtp.163.com:465` SSL | 授权码（客户端授权密码） | 部分支持 | 登录后必须发送 IMAP `ID` 命令，否则报 Unsafe Login |
| 自建 Dovecot | `mail.example.com:993` SSL | `mail.example.com:587` STARTTLS | 邮箱密码或应用密码 | 支持 | 内网地址需 `ALLOW_PRIVATE_IMAP=true` |

端口说明：

- 993 = IMAP over TLS（隐式 TLS，连上即加密）
- 143 = IMAP + STARTTLS（不推荐，除非内网）
- 465 = SMTP over TLS（隐式 TLS）
- 587 = SMTP + STARTTLS（推荐）
- 25 = 明文 SMTP，几乎所有公共邮箱都拒绝客户端直连使用

## 2. Gmail

### 2.1 参数

```text
IMAP: imap.gmail.com  993  SSL/TLS
SMTP: smtp.gmail.com  465  SSL/TLS（或 587 STARTTLS）
用户名: 完整邮箱地址
密码: 16 位应用专用密码（不是 Gmail 登录密码）
```

### 2.2 申请应用专用密码

1. 打开 Google 账号 → 安全性 → 开启「两步验证」（必须）。
2. 访问 <https://myaccount.google.com/apppasswords>，选择「邮件」和「其他（自定义名称）」。
3. 生成 16 位密码，形如 `abcd efgh ijkl mnop`；zMail 里去掉空格或保留空格均可。
4. 如果看不到「应用专用密码」入口，说明账号未开启两步验证，或属于 Workspace 且管理员禁用了该功能。

### 2.3 IDLE 与限流

- 支持 IMAP IDLE，新邮件通常 1~5 秒内推送。
- 每个账号建议只保持 1 条 IDLE 连接；zMail 通过 `IDLE_REFRESH_SECONDS`（默认 1500 秒）定期重连，规避服务端空闲断开。
- Gmail 有每日发送量与带宽限制（免费版约 500 封/天、2500MB/天），超限会返回 `550 5.4.5 Daily sending quota exceeded`。
- Gmail 的「所有邮件」文件夹包含 INBOX 的副本，zMail 必须跳过 `[Gmail]/All Mail`，否则会重复入库。

### 2.4 常见错误

| 错误 | 原因 | 处理 |
|---|---|---|
| `Application-specific password required` | 用了登录密码 | 生成并使用应用专用密码 |
| `Invalid credentials` | 密码错、两步验证未开、或密码已撤销 | 重新生成应用专用密码 |
| `Too many simultaneous connections` | 多设备/多进程同时连 | 关闭其他客户端，降低并发 |

## 3. Outlook / Microsoft 365

### 3.1 参数

```text
IMAP: outlook.office365.com  993  SSL/TLS
SMTP: smtp.office365.com     587  STARTTLS
用户名: 完整邮箱地址
密码: 应用密码（若账号支持）；否则需要 OAuth2
```

### 3.2 认证现状（重要）

- Microsoft 已于 2024 年起逐步停用 Outlook.com / Microsoft 365 的基本认证（用户名+密码直连 IMAP/SMTP）。
- **仍可用的路径**：
  1. 账号开启两步验证后生成「应用密码」，部分个人账号仍可继续使用；
  2. 使用 OAuth2（XOAUTH2）获取访问令牌，zMail 若未内置 OAuth 流程，需要在 `internal/mail` 中扩展。
- 若 IMAP 登录返回 `LOGIN failed` 或 `AUTHENTICATE failed`，优先检查是否被要求 OAuth2。

### 3.3 IDLE 与限流

- 支持 IDLE；但 Exchange Online 对并发连接限制较严格（单邮箱约 20 条 IMAP 连接、发送频率也有限制）。
- 频繁重连会触发 `Server busy` / `Connection closed`，建议保持长连接并只订阅必要文件夹。
- 文件夹名随账号语言本地化（如「已发送邮件」「删除的邮件」），zMail 按 `role` 而不是名称识别。

### 3.4 常见错误

| 错误 | 原因 | 处理 |
|---|---|---|
| `LOGIN failed` | 基本认证被停用 / 密码错 | 改用应用密码或 OAuth2 |
| `User is authenticated but not connected` | 客户端协议/认证方式错误 | 检查 SSL、端口与用户名格式 |
| `Server busy` | 并发或频率超限 | 降低并发、增大重试退避 |

## 4. QQ 邮箱

### 4.1 参数

```text
IMAP: imap.qq.com  993  SSL/TLS
SMTP: smtp.qq.com  465  SSL/TLS
用户名: 完整邮箱地址（如 123456@qq.com）
密码: 16 位授权码
```

### 4.2 开启服务并获取授权码

1. 登录 QQ 邮箱网页版 → 设置 → 账号（或「账号与安全」）。
2. 找到「POP3/IMAP/SMTP/Exchange/CardDAV/CalDAV 服务」。
3. 开启「IMAP/SMTP 服务」，按提示发送短信验证。
4. 生成 16 位授权码，只显示一次，请立即保存。
5. 关闭「IMAP/SMTP 服务」会立即让授权码失效。

### 4.3 IDLE 与限流

- 支持 IDLE，新邮件推送较快。
- 单账号并发连接与每分钟请求数有限制，超限会返回 `Too many connections` 或直接断开。
- 文件夹为中文（收件箱、已发送、草稿箱、垃圾箱等）；zMail 通过 `role` 映射，不依赖名称。
- 广告邮件与通知邮件较多，注意验证码邮件的发件人白名单。

### 4.4 常见错误

| 错误 | 原因 | 处理 |
|---|---|---|
| `LOGIN failed` | 用了 QQ 密码而非授权码 | 生成并使用授权码 |
| `Too many connections` | 多端同时登录/多次重连 | 退出其他客户端，等待几分钟 |
| `Connection reset` | 触发风控或频率限制 | 降低并发，稍后重试 |

## 5. 163 邮箱

### 5.1 参数

```text
IMAP: imap.163.com  993  SSL/TLS
SMTP: smtp.163.com  465  SSL/TLS（25 端口也可但常被运营商封）
用户名: 完整邮箱地址
密码: 客户端授权密码
```

### 5.2 获取授权码

1. 登录 163 邮箱网页版 → 设置 → POP3/SMTP/IMAP。
2. 开启「IMAP/SMTP 服务」并设置「客户端授权密码」。
3. 授权码只显示一次；修改邮箱密码不会让授权码失效，但关闭服务会让它失效。

### 5.3 必须发送 IMAP ID 命令

网易要求客户端在登录后发送 IMAP `ID` 命令声明客户端信息，否则会返回：

```text
Unsafe Login. Please contact kefu@188.com for help
```

zMail 的 IMAP 客户端需要在 `LOGIN` 成功后立即发送：

```text
A001 ID ("name" "zMail" "version" "1.0" "vendor" "zMail")
```

若未实现，163 账号会一直认证失败，但 Gmail/QQ 不受影响。

### 5.4 IDLE 与限流

- 163 对 IDLE 支持依账号/地区不同，部分账号 IDLE 会被服务端断开。
- 若 IDLE 不可用或频繁断开，zMail 会回退到定时同步（受 `IDLE_REFRESH_SECONDS` 影响）。
- 发送频率与单日发信量有限制，营销/群发内容容易被拒。

### 5.5 常见错误

| 错误 | 原因 | 处理 |
|---|---|---|
| `Unsafe Login` | 未发送 IMAP ID | 补发 ID 命令 |
| `LOGIN failed` | 用了登录密码 | 使用客户端授权密码 |
| IDLE 频繁断开 | 服务端限制 | 依赖定时同步，或缩短重连间隔 |

## 6. 自建 Dovecot + Postfix

### 6.1 参数

```text
IMAP: mail.example.com  993  SSL/TLS（143 + STARTTLS 仅内网）
SMTP: mail.example.com  587  STARTTLS（465 SSL 亦可）
用户名: 完整邮箱地址（或 Dovecot 配置的 username 格式）
密码: 邮箱密码 / 应用密码
```

### 6.2 Dovecot 侧要点

- `10-ssl.conf`：`ssl = required`，证书链完整；客户端需能验证证书，自签证书会被 zMail 拒绝。
- `10-auth.conf`：`disable_plaintext_auth = yes` 时必须在 TLS 之后认证。
- `20-imap.conf`：`imap_capability = +IDLE`，`mail_max_userip_connections` 适当调大。
- 若启用两步验证，需要生成应用密码（如 `dovecot-oauth2` 或 `passdb` 应用密码）。
- 内网/私有地址默认被 zMail 的 SSRF 防护拒绝，需显式设置 `ALLOW_PRIVATE_IMAP=true`。
- 文件夹：Dovecot 默认 `INBOX`、`Sent`、`Drafts`、`Trash`、`Junk`，与 zMail `role` 映射一致。

### 6.3 IDLE 与并发

- Dovecot 完整支持 IDLE，推荐使用。
- zMail 默认 `MAX_FETCH_CONCURRENCY=4`，自建小机器可降到 2。
- 首次同步大量邮件时，注意 `IDLE_REFRESH_SECONDS` 与 Dovecot `imap_idle_notify_interval` 的配合。

### 6.4 常见错误

| 错误 | 原因 | 处理 |
|---|---|---|
| `certificate verify failed` | 自签/过期证书 | 换成受信证书，或修正系统 CA |
| `Authentication failed` | PLAIN 认证在非 TLS 下被禁 | 使用 993/STARTTLS |
| `Connection refused` | 端口/防火墙 | 检查 993、587 是否放行 |
| zMail 报私网地址被拒 | SSRF 防护 | 设置 `ALLOW_PRIVATE_IMAP=true` |

## 7. 通用排查步骤

```bash
# 1. 测 IMAP TLS 与能力（把 host 换掉）
openssl s_client -connect imap.gmail.com:993 -crlf -quiet
# 登录后输入：a1 CAPABILITY，确认返回含 IDLE

# 2. 测 SMTP 587 STARTTLS
openssl s_client -connect smtp.office365.com:587 -starttls smtp -crlf -quiet

# 3. 检查 DNS 与网络
nslookup imap.qq.com
curl -v telnet://imap.163.com:993
```

zMail 侧排查：

1. 设置页「测试连接」会返回具体错误码（`AUTH_FAILED` / `UPSTREAM_ERROR` / `TIMEOUT`）。
2. `LOG_LEVEL=debug` 后查看同步日志（`/settings/general` 或 `ListSyncLogs`），日志不会打印密码与正文。
3. 确认 `ALLOW_PRIVATE_IMAP` 与 `TRUSTED_PROXY_CIDRS` 没有误配。
4. 大量账号时降低 `MAX_FETCH_CONCURRENCY`，避免被服务商限流。

## 8. 错误码一览

| 错误码 | 含义 | 常见来源 |
|---|---|---|
| `AUTH_FAILED` | 用户名/密码/授权码错误，或认证方式不被支持 | Gmail 应用密码、Outlook 基本认证停用、163 授权码 |
| `UPSTREAM_ERROR` | 服务商返回异常或拒绝连接 | 连接数超限、风控、服务端 5xx |
| `TIMEOUT` | 连接或读取超时 | 防火墙、DNS、服务端限速 |
| `RATE_LIMITED` | 触发服务商频率限制 | 短时间大量同步/发信 |
| `UNSAFE_LOGIN` | 未发送 IMAP ID（163 特有） | 163 邮箱 |
| `UIDVALIDITY_CHANGED` | 服务端重建了邮箱 | 文件夹会被清空并重新同步 |
| `TLS_ERROR` | 证书或协议不匹配 | 自签证书、过时 TLS 版本 |

## 9. 安全与凭据

- 所有授权码/应用密码在 zMail 内以 AES-256-GCM 加密存储，密钥来自 `CREDENTIALS_KEY`，只放环境变量。
- 不要把授权码写进 `docs/`、日志、或提交到仓库。
- 建议为 zMail 单独生成应用密码，便于随时撤销而不影响其他客户端。
- 账号删除时应同时撤销服务商侧的应用密码。
