# zMail 架构设计与构建文档

> 状态：首版功能已实现并进入维护阶段；本文保留架构、数据模型、构建与运维设计，接口签名以代码和 CONTRACT.md 为准。
> 文档版本：v1.1
> 最近更新：2026-10-24
> 当前发布：0.1.1
> 项目定位：个人聚合邮箱 Web 端
> 技术栈：Go + SQLite + IMAP IDLE + SSE
> 部署形态：单二进制、单进程、反代外置

---

## 目录

1. 项目概述
2. 需求规格
3. 架构设计
4. 技术选型
5. 目录结构
6. 数据模型
7. 核心模块设计
8. API 规范
9. 前端设计与优化
10. 安全设计
11. 部署与运维
12. 从零构建步骤
13. 手工验收
14. 开发里程碑
15. 风险与对策
16. 附录

---

## 1. 项目概述

### 1.1 一句话定义

zMail 是一个**单用户、Web 端、多账号聚合邮箱客户端**，支持 Gmail、Outlook、QQ、163 和自建 IMAP 邮箱，通过 IMAP IDLE 实现秒级收信，低资源占用，单机可部署。

### 1.2 核心目标

| 目标 | 说明 |
|---|---|
| 多账号聚合 | 10 个不同服务商邮箱统一管理 |
| 秒级收信 | 新邮件到达后 1~3 秒内推送到浏览器 |
| 低资源 | 1C1G VPS 可运行，常驻内存 ≤ 150MB |
| 稳定不漏信 | 断线自动重连、增量同步、UIDVALIDITY 检测 |
| 简单部署 | 单个静态二进制 + systemd，无 Redis/ES/Node |
| 安全 | 凭据加密、邮件 HTML 沙箱渲染、单用户访问控制 |

### 1.3 非目标

- 不做多租户 / 团队共享邮箱
- 不做移动端 / 桌面端 / 浏览器插件
- 不做邮件服务器（只做客户端聚合）
- 不在本服务内实现反向代理、TLS 证书、ACME
- 首版不做复杂规则引擎、AI 摘要

### 1.4 成功指标

| 指标 | 目标值 |
|---|---|
| 收信延迟（Gmail/Outlook/自建） | P95 ≤ 3 秒 |
| 收信延迟（QQ/163 支持 IDLE 时） | P95 ≤ 3 秒 |
| 收信延迟（不支持 IDLE 兜底） | ≤ 15 秒 |
| 常驻内存 | ≤ 150MB |
| 首次同步 | 最近 500 封，≤ 60 秒 |
| 漏信率 | 0（断线后能补齐） |
| 重复邮件 | 0（幂等去重） |

---

## 2. 需求规格

### 2.1 功能需求

| 编号 | 功能 | 优先级 | 说明 |
|---|---|---|---|
| FR-01 | 添加邮箱账号 | P0 | 支持 IMAP/SMTP 参数配置，凭据加密存储 |
| FR-02 | 账号连接测试 | P0 | 登录 IMAP/SMTP 验证配置 |
| FR-03 | 收件箱同步 | P0 | 首次最近 500 封，之后增量 |
| FR-04 | 邮件列表 | P0 | 游标分页、未读标识 |
| FR-05 | 邮件详情 | P0 | HTML/纯文本正文、发件人、时间 |
| FR-06 | 实时新邮件推送 | P0 | SSE + 浏览器通知 |
| FR-07 | 验证码识别 | P0 | 自动提取并一键复制 |
| FR-08 | 已读/星标 | P1 | 本地 + 回写 IMAP |
| FR-09 | 删除/移动 | P1 | 回写 IMAP |
| FR-10 | 发信/回复/转发 | P1 | SMTP 发送 + APPEND 到已发送 |
| FR-11 | 附件下载 | P1 | 流式下载，哈希去重 |
| FR-12 | 全文搜索 | P1 | 主题/正文/发件人，支持中文 |
| FR-13 | 多账号统一收件箱 | P1 | 聚合所有账号 INBOX |
| FR-14 | 文件夹管理 | P2 | 映射 INBOX/Sent/Drafts/Trash |
| FR-15 | 历史回填 | P2 | 后台低优先级拉取旧邮件 |
| FR-16 | 签名/模板 | P2 | 发信默认签名 |

### 2.2 非功能需求

| 编号 | 类别 | 要求 |
|---|---|---|
| NFR-01 | 性能 | 收信端到端 P95 ≤ 3s；列表接口 P95 ≤ 200ms |
| NFR-02 | 资源 | 单进程 ≤ 150MB；SQLite 文件 ≤ 20GB 可接受 |
| NFR-03 | 稳定性 | 单账号故障不影响其他账号；进程崩溃自动重启 |
| NFR-04 | 安全 | 凭据 AES-256-GCM 加密；邮件 HTML 沙箱渲染 |
| NFR-05 | 可维护 | 模块边界清晰；核心同步逻辑可独立验证 |
| NFR-06 | 可观测 | 结构化日志；`/healthz`；同步状态可查 |

---

## 3. 架构设计

### 3.1 总体架构

```
┌──────────────────── Browser ────────────────────┐
│  html/template + htmx + vanilla JS + EventSource │
└───────────────────────┬─────────────────────────┘
                        │ HTTP / SSE
┌───────────────────────▼─────────────────────────┐
│              zMail 单进程 (127.0.0.1:7080)       │
│                                                 │
│  ┌─────────────┐        ┌──────────────────┐    │
│  │ HTTP/API    │        │ SSE Hub          │    │
│  │ 路由/鉴权   │        │ 事件广播/心跳     │    │
│  └──────┬──────┘        └────────▲─────────┘    │
│         │                        │              │
│  ┌──────▼────────────────────────┴─────────┐    │
│  │ 同步引擎                                  │    │
│  │  ├ IDLE watcher × 10（每账号一条专用连接） │    │
│  │  ├ fetch worker pool × 4（短连接拉取）     │    │
│  │  └ backfill scheduler × 1（低优先级回填）  │    │
│  └──────────────────┬───────────────────────┘    │
│                     │                            │
│  ┌──────────────────▼───────────────────────┐    │
│  │ Store：SQLite WAL + 单写者                 │    │
│  │  accounts / folders / messages / fts      │    │
│  └──────────────────────────────────────────┘    │
└─────────────────────────────────────────────────┘
```

### 3.2 进程模型

**单进程**，内部并发由 goroutine 承担：

- 1 个 HTTP server goroutine
- 10 个 IDLE watcher goroutine（每账号一个）
- 4 个 fetch worker（全局并发上限）
- 1 个 backfill scheduler
- 1 个 SSE Hub

这样避免多进程写 SQLite 的锁竞争，内存也最省。

### 3.3 关键数据流

**实时收信链路（目标 < 1 秒服务端处理）：**

```
邮件到达服务商
  → IMAP IDLE 连接收到 EXISTS
  → watcher 投递内存事件（不阻塞）
  → fetch worker 用独立连接 UID SEARCH lastUID+1:*
  → FETCH 新邮件 → MIME 解析
  → SQLite 短事务插入
  → SSE Hub 广播 mail:new
  → 浏览器通知 / 列表刷新 / 验证码弹窗
```

**发信链路：**

```
用户提交 → 参数校验 → SMTP 发送 → APPEND 到 Sent 文件夹
         → 本地插入已发送记录 → SSE 刷新
```

**历史回填链路：**

```
backfill scheduler → 选择低优先级文件夹 → 每次拉 50 封
→ 写入 → 更新游标 → 限速休眠
（检测到实时任务时暂停或降速）
```

### 3.4 关键设计决策（ADR）

| 决策 | 结论 | 理由 |
|---|---|---|
| ADR-01 | 用 Go 而非 Node | 低内存、单二进制、goroutine 适合 10 条长连接 |
| ADR-02 | SQLite 而非 PostgreSQL | 单用户写入量小，本地写延迟低，零运维 |
| ADR-03 | 不用 Redis / 消息队列 | 实时链路走内存 channel；任务表即可满足重试 |
| ADR-04 | 每账号 IDLE 专用连接 + fetch 连接池 | IDLE 期间不能执行 FETCH，混用会阻塞推送 |
| ADR-05 | 反代外置 | 服务只监听 127.0.0.1:7080，TLS 由外部设施负责 |
| ADR-06 | 首版 IMAP 通用适配 | 统一覆盖 5 类服务商，OAuth API 后续再加 |
| ADR-07 | 前端不用 SPA 框架 | 个人应用，html/template + htmx 足够，零 Node 工具链 |

---

## 4. 技术选型

| 组件 | 选型 | 版本 | 说明 |
|---|---|---|---|
| 语言 | Go | 1.23+ | 单二进制、低占用 |
| HTTP | 标准库 `net/http` | 1.22+ 路由 | 无框架依赖 |
| 模板 | `html/template` | 标准库 | 服务端渲染 |
| 前端增强 | htmx + vanilla JS | 本地 vendored | 局部刷新、快捷键/Toast/主题 |
| 数据库 | SQLite (WAL) | 3.40+ | 单文件、零运维 |
| SQLite 驱动 | `modernc.org/sqlite` | latest | 纯 Go，无需 CGO |
| IMAP | `github.com/emersion/go-imap` | v1.2.x | 成熟稳定，支持 IDLE |
| MIME | `github.com/emersion/go-message` | latest | 解析头部/正文/编码 |
| SMTP | `github.com/wneessen/go-mail` | latest | 支持 SSL/STARTTLS/OAuth2 |
| HTML 消毒 | `github.com/microcosm-cc/bluemonday` | latest | 服务端清洗 |
| 加密 | 标准库 `crypto/aes` + `cipher` | - | AES-256-GCM |
| 日志 | 标准库 `log/slog` | 1.21+ | 结构化日志 |
| 部署 | systemd + 外部反代 | - | 无容器依赖 |

### 4.1 依赖清单

```
github.com/emersion/go-imap
github.com/emersion/go-message
github.com/wneessen/go-mail
github.com/microcosm-cc/bluemonday
modernc.org/sqlite
golang.org/x/text        // 字符集解码
```

---

## 5. 目录结构

```
zMail/
├─ go.mod / go.sum
├─ Makefile                      # Linux/macOS 构建入口
├─ make.ps1 / make.cmd           # Windows 构建入口
├─ cmd/
│  └─ zmail/
│     └─ main.go                 # 入口：配置、DB、启动 HTTP/同步
├─ internal/
│  ├─ config/                    # 环境变量与 .env 加载、校验
│  ├─ secret/                    # AES-256-GCM 加解密
│  ├─ store/                     # SQLite schema、持久层、blob
│  ├─ mail/                      # IDLE、增量同步、解析、发信、回填
│  ├─ realtime/                  # SSE Hub
│  ├─ verify/                    # 验证码提取
│  ├─ imgproxy/                  # 远程图片代理与 SSRF 防护
│  ├─ httpapi/                   # 路由、鉴权、SSE、HTML 模板
│  │  └─ templates/              # login/mail/settings 页面模板
│  └─ web/
│     ├─ embed.go
│     └─ static/                 # CSS/JS/htmx/图标/PWA
├─ scripts/                      # 本地开发环境脚本
├─ deploy/                       # systemd、Dockerfile、Compose、env 示例
├─ docs/                         # 本文、接口契约、部署、反代、服务商
└─ data/                         # 运行数据（gitignored）
```

---

## 6. 数据模型

### 6.1 ER 关系

```
accounts 1 ── N folders
accounts 1 ── N messages
folders  1 ── N messages
messages 1 ── N attachments
accounts 1 ── N threads
messages_fts  (全文索引，关联 messages.rowid)
```

### 6.2 完整 DDL

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
  id             TEXT PRIMARY KEY,
  provider       TEXT NOT NULL DEFAULT 'imap',   -- gmail/outlook/qq/163/imap
  email          TEXT NOT NULL UNIQUE,
  display_name   TEXT NOT NULL DEFAULT '',
  username       TEXT NOT NULL,
  password_enc   BLOB NOT NULL,                  -- AES-256-GCM
  imap_host      TEXT NOT NULL,
  imap_port      INTEGER NOT NULL DEFAULT 993,
  imap_secure    INTEGER NOT NULL DEFAULT 1,
  smtp_host      TEXT NOT NULL,
  smtp_port      INTEGER NOT NULL DEFAULT 465,
  smtp_secure    INTEGER NOT NULL DEFAULT 1,
  status         TEXT NOT NULL DEFAULT 'active', -- active/auth_failed/disabled
  last_error     TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT,
  created_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS folders (
  id             TEXT PRIMARY KEY,
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  remote_path    TEXT NOT NULL,                  -- IMAP 原始路径
  name           TEXT NOT NULL,
  role           TEXT NOT NULL DEFAULT 'custom', -- inbox/sent/drafts/trash/spam/archive/custom
  uid_validity   INTEGER NOT NULL DEFAULT 0,
  last_uid       INTEGER NOT NULL DEFAULT 0,
  unread_count   INTEGER NOT NULL DEFAULT 0,
  total_count    INTEGER NOT NULL DEFAULT 0,
  enabled        INTEGER NOT NULL DEFAULT 1,
  priority       INTEGER NOT NULL DEFAULT 100,
  last_synced_at TEXT,
  UNIQUE(account_id, remote_path)
);

CREATE TABLE IF NOT EXISTS threads (
  id              TEXT PRIMARY KEY,
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_key      TEXT NOT NULL,                 -- 根 Message-ID 或归一化 Subject hash
  subject         TEXT NOT NULL DEFAULT '',
  last_message_at TEXT NOT NULL,
  message_count   INTEGER NOT NULL DEFAULT 1,
  UNIQUE(account_id, thread_key)
);

CREATE TABLE IF NOT EXISTS messages (
  id              TEXT PRIMARY KEY,
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder_id       TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  thread_id       TEXT REFERENCES threads(id) ON DELETE SET NULL,
  remote_uid      INTEGER NOT NULL,
  message_id      TEXT,
  subject         TEXT NOT NULL DEFAULT '',
  from_name       TEXT NOT NULL DEFAULT '',
  from_email      TEXT NOT NULL DEFAULT '',
  to_json         TEXT NOT NULL DEFAULT '[]',
  cc_json         TEXT NOT NULL DEFAULT '[]',
  bcc_json        TEXT NOT NULL DEFAULT '[]',
  sent_at         TEXT,
  received_at     TEXT NOT NULL,
  snippet         TEXT NOT NULL DEFAULT '',
  body_text       TEXT NOT NULL DEFAULT '',
  body_html       TEXT NOT NULL DEFAULT '',
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_read         INTEGER NOT NULL DEFAULT 0,
  is_starred      INTEGER NOT NULL DEFAULT 0,
  is_deleted      INTEGER NOT NULL DEFAULT 0,
  is_draft        INTEGER NOT NULL DEFAULT 0,
  size_bytes      INTEGER NOT NULL DEFAULT 0,
  raw_key         TEXT,
  created_at      TEXT NOT NULL,
  UNIQUE(account_id, folder_id, remote_uid)
);

CREATE INDEX IF NOT EXISTS idx_messages_folder
  ON messages(folder_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_account
  ON messages(account_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_message_id
  ON messages(account_id, message_id);

CREATE TABLE IF NOT EXISTS attachments (
  id          TEXT PRIMARY KEY,
  message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename    TEXT NOT NULL,
  mime_type   TEXT NOT NULL,
  size        INTEGER NOT NULL DEFAULT 0,
  content_id  TEXT,
  sha256      TEXT NOT NULL DEFAULT '',
  storage_key TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_sha
  ON attachments(sha256);

CREATE TABLE IF NOT EXISTS sync_logs (
  id         TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  level      TEXT NOT NULL,       -- info/warn/error
  message    TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sync_logs_account
  ON sync_logs(account_id, created_at DESC);

CREATE TABLE IF NOT EXISTS jobs (
  id         TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,       -- backfill/retry_send/retry_sync
  account_id TEXT,
  payload    TEXT NOT NULL DEFAULT '{}',
  run_at     TEXT NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_due
  ON jobs(status, run_at);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- 全文检索：trigram 对中文子串友好
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  subject,
  body_text,
  from_name,
  from_email,
  content='messages',
  content_rowid='rowid',
  tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, body_text, from_name, from_email)
  VALUES (new.rowid, new.subject, new.body_text, new.from_name, new.from_email);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body_text, from_name, from_email)
  VALUES ('delete', old.rowid, old.subject, old.body_text, old.from_name, old.from_email);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body_text, from_name, from_email)
  VALUES ('delete', old.rowid, old.subject, old.body_text, old.from_name, old.from_email);
  INSERT INTO messages_fts(rowid, subject, body_text, from_name, from_email)
  VALUES (new.rowid, new.subject, new.body_text, new.from_name, new.from_email);
END;
```

### 6.3 数据保留策略

| 数据 | 保留策略 |
|---|---|
| messages | 长期保留；超过 2 年可归档到冷表 |
| body_html | 保留，但渲染前消毒 |
| raw .eml | 压缩存储；可配置只保留最近 1 年 |
| attachments | 按 sha256 去重；大附件可外置 |
| sync_logs | 保留 30 天，定时清理 |
| jobs | 完成 7 天后清理 |

---

## 7. 核心模块设计

### 7.1 配置模块

环境变量：

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `LISTEN_ADDR` | 否 | `127.0.0.1:7080` | 只监听本机 |
| `PUBLIC_URL` | 是 | - | 外部访问地址，用于 Cookie/链接 |
| `DB_PATH` | 否 | `./data/zmail.db` | SQLite 路径 |
| `RAW_DIR` | 否 | `./data/raw` | 原始邮件目录 |
| `APP_PASSWORD` | 是 | - | 单用户访问密码 |
| `SESSION_KEY` | 是 | - | base64 32 字节，签名 Cookie |
| `CREDENTIALS_KEY` | 是 | - | base64 32 字节，加密邮箱凭据 |
| `MAX_FETCH_CONCURRENCY` | 否 | `4` | 全局拉取并发 |
| `IDLE_REFRESH_SECONDS` | 否 | `1500` | IDLE 重发间隔（25 分钟） |
| `BACKFILL_ENABLED` | 否 | `true` | 是否启用历史回填 |
| `ALLOW_PRIVATE_IMAP` | 否 | `false` | 是否允许自建内网 IMAP |
| `LOG_LEVEL` | 否 | `info` | debug/info/warn/error |

### 7.2 凭据加密

- 算法：AES-256-GCM
- 密钥：`CREDENTIALS_KEY`，32 字节，base64 编码
- 存储格式：`nonce(12B) || ciphertext || tag`
- 只在建立 IMAP/SMTP 连接时解密，不落日志
- 更换密钥需提供迁移命令（先解密旧数据再用新密钥加密）

### 7.3 存储层

- `sql.Open("sqlite", "file:...?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")`
- `db.SetMaxOpenConns(1)`：所有写串行化，避免 `database is locked`
- 批量插入使用单事务，每 50 封提交一次
- 列表查询必须走索引：`(folder_id, received_at DESC)`
- 游标分页：`WHERE received_at < ? ORDER BY received_at DESC LIMIT ?`

### 7.4 IMAP 连接管理

#### 连接模型

| 连接 | 数量 | 用途 | 生命周期 |
|---|---|---|---|
| IDLE 连接 | 每账号 1 条 | 只接收新邮件通知 | 长期，断线重连 |
| FETCH 连接 | 全局池 4 条 | 拉取正文/附件 | 短连接，用完即关 |
| 管理连接 | 按需 | 文件夹列表、标记已读 | 短连接 |

#### 状态机

```
DISCONNECTED
    │ connect
    ▼
CONNECTING ──fail──► BACKOFF ──timer──► CONNECTING
    │ ok
    ▼
AUTH ──fail──► AUTH_FAILED（UI 提示重新授权）
    │ ok
    ▼
SELECT INBOX
    │ ok
    ▼
CATCHUP（UID SEARCH lastUID+1:*）
    │
    ▼
IDLE ◄──────────────┐
    │ EXISTS         │ 25 分钟重发 IDLE
    ├────────────────┘
    │ BYE / error / ctx
    ▼
BACKOFF（1s→2s→4s…max 60s + jitter）
```

#### 能力检测

连接后检查 `CAPABILITY`：

- 含 `IDLE`：进入 IDLE 模式
- 不含 `IDLE`：降级为每 12~15 秒轮询 INBOX（仅 `UID SEARCH`，非常轻）
- 含 `MOVE`：优先用 MOVE 移动邮件，否则 `COPY + STORE \Deleted + EXPUNGE`

### 7.5 同步引擎

#### 首次同步

```
1. LOGIN
2. SELECT INBOX
3. 读取 UIDVALIDITY、UIDNEXT
4. 本地 lastUID = max(0, UIDNEXT - 500)   // 只拉最近 500 封
5. UID SEARCH UID lastUID+1:*
6. 分批 UID FETCH envelope + flags + body
7. 解析并 INSERT OR IGNORE
8. 更新 folder.last_uid、uid_validity
```

#### 增量同步

```
1. UID SEARCH UID lastUID+1:*
2. 若结果为空：只更新计数
3. 分批 FETCH
4. 解析、去重、插入
5. 事务提交后更新 last_uid
```

#### UIDVALIDITY 处理

```
if server.UIDVALIDITY != local.uid_validity {
    DELETE FROM messages WHERE folder_id = ?
    UPDATE folders SET uid_validity = ?, last_uid = 0
    // 重新走首次同步
}
```

#### 去重规则

- 主键：`UNIQUE(account_id, folder_id, remote_uid)`
- 跨文件夹去重：`account_id + message_id` 查询已存在则跳过或合并
- Gmail 必须跳过 `[Gmail]/All Mail`，否则 INBOX 邮件重复

#### 错误分类

| 错误 | 处理 |
|---|---|
| 网络超时 | 指数退避重连 |
| 认证失败 | 标记 `auth_failed`，停止该账号重试，UI 提示 |
| 服务商限流 | 退避 5~30 分钟 |
| 单封解析失败 | 记录日志，跳过该封，继续 |
| DB 写入失败 | 事务回滚，游标不推进，稍后重试 |

### 7.6 实时推送（SSE Hub）

事件协议：

```
event: mail:new
data: {"id":"...","accountId":"...","folderId":"...","subject":"...","fromName":"...","snippet":"...","receivedAt":"..."}

event: mail:code
data: {"messageId":"...","code":"123456","from":"...","subject":"..."}

event: sync:status
data: {"accountId":"...","status":"idle|syncing|error","message":"..."}
```

服务器实现要点：

- `Content-Type: text/event-stream`
- `Cache-Control: no-cache, no-transform`
- `X-Accel-Buffering: no`
- 每 20 秒发送 `: ping` 心跳
- 慢客户端缓冲区满则丢弃，前端重连后重新拉取
- 前端 `EventSource` 自动重连，重连成功后调用一次增量列表刷新

### 7.7 验证码识别

只对最近 24 小时、发件箱的邮件执行：

```go
var codePatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|安全码|verification code|verify code|otp|passcode)[^\d]{0,20}(\d{4,8})`),
    regexp.MustCompile(`(?i)(\d{4,8})[^\d]{0,10}(?:验证码|verification code|otp)`),
}

var negativePatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)(订单号|order|invoice|金额|price|\$|￥)`),
}
```

规则：

1. 先匹配关键词 + 数字组合
2. 无结果时，若主题含验证码关键词，取正文中第一个 4~8 位数字
3. 排除订单号、金额、年份
4. 提取成功 → 推送 `mail:code` + 浏览器通知 + 一键复制按钮

### 7.8 发信

流程：

```
1. 校验 from/to/subject/body
2. 用账号 SMTP 配置建立连接（465 SSL 或 587 STARTTLS）
3. 构造 MIME 邮件（HTML + 纯文本 fallback）
4. 发送
5. 成功 → APPEND 到 Sent 文件夹（如果服务商支持）
6. 本地插入 messages 记录（is_draft=0）
7. 失败 → 写 jobs 表重试（最多 3 次）
```

注意：

- 发件人必须与账号邮箱一致，否则会被服务商拒绝
- 回复时设置 `In-Reply-To` 和 `References`
- 附件使用 MIME base64
- SMTP 密码同样从加密凭据解密

### 7.9 搜索

- 使用 FTS5 `trigram`，支持中文子串
- 查询：`SELECT ... FROM messages_fts JOIN messages ON messages.rowid = messages_fts.rowid WHERE messages_fts MATCH ? ORDER BY rank LIMIT 50`
- 查询词长度 < 2 时走 `LIKE` 兜底
- 搜索范围可按账号过滤
- 索引由触发器自动维护

### 7.10 前端设计

页面：

| 路径 | 页面 |
|---|---|
| `/login` | 单用户密码登录 |
| `/mail` | 三栏主界面 |
| `/mail/compose` | 写信/回复弹层 |
| `/settings/accounts` | 账号管理 |
| `/settings/general` | 签名、通知设置 |

主界面组件：

- **AccountRail**：账号列表 + 未读数 + 同步状态
- **FolderTree**：按账号分组显示文件夹
- **MessageList**：虚拟滚动、未读加粗、时间分组
- **MessageReader**：服务端消毒后内联渲染 HTML 正文
- **Composer**：contenteditable + 简单格式工具栏
- **CodeToast**：验证码大号显示 + 复制按钮
- **SyncIndicator**：旋转图标 + 错误提示

状态管理：

- 页面状态放 URL（`?folder=...&message=...`）
- htmx 负责局部刷新
- SSE 负责增量更新
- 少量 vanilla helper 处理快捷键、Toast、主题切换

---

## 8. API 规范

### 8.1 认证

- `POST /login` 表单登录，服务端设置 `zmail_session` HttpOnly Cookie
- 所有 `/api/*` 需要有效会话
- 写操作校验 `Origin` 与 `PUBLIC_URL` 一致

### 8.2 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查 |
| GET | `/login` | 登录页 |
| POST | `/login` | 登录 |
| POST | `/logout` | 退出 |
| GET | `/mail` | 三栏主界面 |
| GET | `/settings/accounts` | 账号管理页 |
| GET | `/settings/general` | 通用设置页 |
| GET | `/api/accounts` | 账号列表 |
| POST | `/api/accounts` | 添加账号 |
| GET | `/api/accounts/{id}` | 账号详情 |
| PUT / PATCH | `/api/accounts/{id}` | 修改账号/启停 |
| DELETE | `/api/accounts/{id}` | 删除账号 |
| POST | `/api/accounts/{id}/test` | 测试连接 |
| POST | `/api/accounts/{id}/sync` | 立即同步 |
| GET | `/api/folders?accountId=` | 文件夹列表 |
| GET | `/api/messages?scope=&accountId=&folderId=&before=&limit=` | 邮件列表 |
| GET | `/api/messages/{id}` | 邮件详情 |
| PATCH | `/api/messages/{id}` | 更新已读/星标 |
| DELETE | `/api/messages/{id}` | 删除邮件 |
| POST | `/api/messages/send` | 发信 |
| POST | `/api/messages/backfill` | 历史回填 |
| GET | `/api/attachments/{id}` | 下载附件 |
| GET | `/api/search?q=` | 全文搜索 |
| GET | `/api/proxy/image?url=` | 远程图片代理 |
| GET | `/api/events` | SSE 事件流 |
### 8.3 添加账号示例

```http
POST /api/accounts
Content-Type: application/json

{
  "provider": "gmail",
  "email": "me@gmail.com",
  "displayName": "个人 Gmail",
  "username": "me@gmail.com",
  "password": "app-password",
  "imapHost": "imap.gmail.com",
  "imapPort": 993,
  "smtpHost": "smtp.gmail.com",
  "smtpPort": 465
}
```

### 8.4 错误格式

```json
{
  "error": {
    "code": "AUTH_FAILED",
    "message": "IMAP 登录失败，请检查授权码"
  }
}
```

错误码：`BAD_REQUEST` / `UNAUTHORIZED` / `FORBIDDEN` / `NOT_FOUND` / `CONFLICT` / `RATE_LIMITED` / `UPSTREAM_ERROR` / `INTERNAL`。

---

## 9. 前端设计与优化

> 基线：html/template + htmx + SSE
> 目标：在不引入 Node/SPA 的前提下，把体验做到接近原生邮件客户端

### 9.1 结论

当前方向是对的：服务端渲染 + htmx 局部刷新，首屏快、内存低、无需前端构建链。
真正值得优化的是**交互细节、列表性能、阅读器渲染和实时体验**，而不是换框架。

### 9.2 优化优先级

| 优先级 | 优化项 | 收益 | 工作量 |
|---|---|---|---|
| P0 | 列表增量追加与滚动位置保持 | 消除翻页闪烁 | 小 |
| P0 | 已读/星标乐观更新 | 操作零等待 | 小 |
| P0 | 阅读器改为服务端消毒后内联渲染 | 高度自适应、可搜索 | 中 |
| P0 | SSE 增量插入而不是整列表重载 | 新邮件不打断阅读 | 小 |
| P1 | 键盘快捷键与命令面板 | 效率大幅提升 | 中 |
| P1 | 图片代理与懒加载 | 防追踪、加载快 | 中 |
| P1 | 深色模式与响应式三栏 | 体验完整 | 小 |
| P1 | PWA + Web Push | 关掉标签页也能收验证码 | 中 |
| P2 | 虚拟滚动 | 万封邮件仍流畅 | 中 |
| P2 | 附件预览、批量操作 | 完整客户端能力 | 大 |

### 9.3 邮件列表

- 游标分页加追加：接口返回 nextCursor，新一批 append 到列表尾部，不整块替换
- 只传列表字段：id、from、subject、snippet、receivedAt、isRead、isStarred
- 骨架屏：首次加载显示 8 条占位
- 滚动位置保持：htmx swap 前记录 scrollTop，swap 后恢复
- 未读视觉：左侧 3px 强调条加粗，不只靠颜色
- 时间分组：今天、昨天、本周、更早
- SSE 增量插入：收到 mail:new 时 prepend 一条 DOM，不重拉整个列表
- 虚拟滚动（P2）：超过 500 条时启用

### 9.4 阅读器

推荐：服务端用 bluemonday 消毒后内联渲染，而不是 iframe。

| 方案 | 优点 | 缺点 |
|---|---|---|
| iframe sandbox | 隔离强 | 高度难自适应、无法页内搜索、样式割裂 |
| 内联加消毒 | 自适应、可搜索、可折叠引用 | 必须严格消毒加 CSP |

内联渲染要点：

- 白名单清洗，移除 script、事件属性、表单
- CSP 设置 default-src none，img-src 只允许 self 和 data
- 外链加 noopener noreferrer，拦截 javascript 协议
- 引用内容默认折叠，提供显示引用按钮
- 远程图片默认不加载，点击后走服务端图片代理
- 正文宽度 72ch，行高 1.6，代码块横向滚动
- 纯文本邮件自动识别链接并保留换行

### 9.5 交互优化

- 键盘快捷键：j/k 上下移动，Enter 打开，r 回复，c 写信，e 归档，s 星标，/ 搜索
- 命令面板：Ctrl+K 搜索邮件、跳转文件夹、执行操作
- 乐观更新：已读、星标先改 DOM，请求失败再回滚
- 悬停预取：鼠标停在邮件上 150ms 后预取详情
- 批量选择：Shift 连选，支持批量已读、归档、删除
- 无障碍：aria-live 播报新邮件，焦点在删除后自动落到下一条
- 快捷键帮助：按 ? 显示快捷键面板

### 9.6 实时与通知

- SSE 只用来发事件，前端按事件类型做最小 DOM 更新
- 新邮件：更新对应文件夹未读数、列表顶部插入、标题显示未读总数
- 验证码：右下角 Toast 大号显示，点击即复制，并触发浏览器通知
- 浏览器通知：标题用发件人，正文用验证码或主题
- PWA：manifest 加 service worker，支持添加到桌面
- Web Push：使用 VAPID，标签页关闭也能收到验证码通知
- 断线重连：EventSource 自动重连后调用一次增量刷新

### 9.7 资源与加载性能

- 去掉 Alpine，改用 3KB 左右的 vanilla helper，htmx 已够用
- JS 总量控制在 30KB gzip 以内，全部本地 embed，不用 CDN
- 图标使用内联 SVG sprite，不引入图标库
- 字体使用系统字体栈，避免下载 Web Font
- CSS 内联关键部分，其余按页面加载
- 静态资源内容哈希命名，设置长期缓存
- 首屏只渲染可见区域，图片一律 loading=lazy
- 预加载：preconnect 无意义，同源即可；preload 关键 CSS

### 9.8 响应式与主题

- 三栏布局按宽度降级：大于 1200px 三栏，800 到 1200px 两栏，小于 800px 单栏
- 移动端：列表和详情用页面切换，返回时保留滚动位置
- 深色模式：CSS 变量加 prefers-color-scheme，设置项可手动切换
- 主题色：每个账号一个色点，便于快速识别
- 高对比度：不依赖颜色传达状态，未读加粗加圆点
- 触屏：按钮最小 44px，列表项整行可点

### 9.9 性能预算与指标

| 指标 | 目标 |
|---|---|
| JS 体积 | 小于 30KB gzip |
| CSS 体积 | 小于 20KB gzip |
| 首屏 LCP | 小于 1 秒（局域网/同城） |
| 交互 INP | 小于 100ms |
| CLS | 小于 0.1 |
| 列表翻页 | 小于 150ms |
| 邮件详情渲染 | 小于 200ms |

- 用 Lighthouse 和 Chrome Performance 面板验证
- 后端接口加 Server-Timing 头，定位慢在 DB 还是模板
- 大邮件正文做截断和懒加载，超过 1MB 的 HTML 只渲染前 200KB

### 9.10 要不要换前端框架

| 方案 | JS 体积 | 开发效率 | 适合度 |
|---|---|---|---|
| htmx + vanilla | 最小 | 中 | 推荐 |
| Alpine + htmx | 小 | 中 | 可接受，按需引入 |
| Svelte | 中 | 高 | 需要构建链，收益有限 |
| React/Vue SPA | 大 | 高 | 对个人项目过重 |

结论：继续 htmx，去掉 Alpine，用少量 vanilla 处理快捷键、Toast、主题切换即可。

### 9.11 实施顺序

1. 列表追加与滚动保持（P0）
2. 已读/星标乐观更新（P0）
3. SSE 增量插入（P0）
4. 阅读器改内联消毒渲染（P0）
5. 键盘快捷键（P1）
6. 深色模式与响应式（P1）
7. 图片代理与懒加载（P1）
8. PWA 与 Web Push（P1）
9. 虚拟滚动与批量操作（P2）

---

### 9.12 前端实现与资源组织

首版完成后，临时可运行原型 `web/prototype/` 与设计迭代截图 `web/design-demos/` 已移除，避免与生产实现重复。生产代码即最终设计：

| 文件 | 作用 |
|---|---|
| internal/httpapi/templates/login.html | 登录页 |
| internal/httpapi/templates/mail.html | 三栏主界面 |
| internal/httpapi/templates/settings_accounts.html | 账号管理页 |
| internal/httpapi/templates/settings_general.html | 通用设置页 |
| internal/httpapi/templates/partials.html | 公共 head、顶栏、脚本入口 |
| internal/web/static/app.css | 主题变量、响应式、列表与阅读器样式 |
| internal/web/static/app.js | 列表/阅读器、快捷键、命令面板、Toast、SSE |
| internal/web/static/sw.js | Service Worker 与静态资源缓存 |
| internal/web/static/htmx.min.js | 本地 vendored htmx |
| internal/web/static/icons/ | favicon 与 PWA 图标 |

#### 9.12.1 已实现的能力

- 三栏布局，窄屏下侧栏抽屉化、阅读时隐藏列表
- 深色/浅色主题切换并记忆到 localStorage
- 邮件分组、未读强调、星标、骨架屏
- 阅读器内联渲染与引用折叠，服务端消毒 + CSP 兜底
- 验证码 Toast、浏览器通知与一键复制
- 已读/星标/删除乐观更新，失败回滚并写同步日志
- 账户折叠、连续浏览、游标分页与历史回填
- j/k/Enter/r/c/e/s///? 快捷键与 Ctrl+K 命令面板
- SSE 新邮件/验证码/同步状态事件
- PWA manifest、图标与 Service Worker

#### 9.12.2 前后端对接

| 前端行为 | 生产接口 |
|---|---|
| 加载邮件列表 | GET /api/messages?scope=&accountId=&folderId=&before=&limit= |
| 打开邮件 | GET /api/messages/{id} |
| 已读/星标更新 | PATCH /api/messages/{id}，失败回滚 |
| 新邮件推送 | EventSource('/api/events') 监听 mail:new |
| 验证码通知 | 监听 mail:code，展示 code 字段 |
| 静态资源 | Go embed `internal/web/static`，带构建指纹 |

---

### 9.13 如何判断邮件属于哪个账户

核心原则：账户归属由数据层决定，UI 只做展示，不靠邮箱地址字符串猜。

#### 9.13.1 数据链路

```
accounts.id
  └─ folders.account_id      每个账户有自己的 INBOX
       └─ messages.account_id
       └─ messages.folder_id
```

- 每个账户一个 INBOX：folders 表用 `UNIQUE(account_id, remote_path)` 约束
- 建议再加部分唯一索引，保证每个账户只有一个 role='inbox' 的文件夹
- messages 同时存 account_id 和 folder_id，查询时双条件过滤

#### 9.13.2 查询示例

```sql
-- 找到某账户的收件箱
SELECT id FROM folders
WHERE account_id = ? AND role = 'inbox';

-- 某账户收件箱列表
SELECT * FROM messages
WHERE account_id = ? AND folder_id = ?
ORDER BY received_at DESC
LIMIT 30;
```

#### 9.13.3 API 设计

| 场景 | 请求 |
|---|---|
| 某账户文件夹 | GET /api/folders?accountId=a1 |
| 某账户收件箱 | GET /api/messages?folderId=f1&limit=30 |
| 全部收件箱 | GET /api/messages?scope=inbox&limit=30 |
| 邮件详情 | GET /api/messages/{id}，返回含 accountId 和 accountEmail |
| 发信 | POST /api/messages/send，body 必须带 accountId |

关键：列表和详情响应都必须包含 `accountId` 与 `accountEmail`，前端不需要再查账户表。

#### 9.13.4 UI 设计

- 左侧按账户分组：账户邮箱 + 该账户的收件箱/已发送
- 顶部提供「全部收件箱」，聚合所有账户 INBOX
- 聚合列表每条邮件显示账户色点和邮箱
- 阅读器顶部显示当前邮件所属账户
- 回复/转发时用该邮件的 accountId 选择 SMTP 配置
- 删除/标记已读必须带 accountId 和 folderId 校验归属

#### 9.13.5 原型已同步修改

- 侧边栏改为按账户分组渲染：me@gmail.com、work@outlook.com
- 新增「全部收件箱」入口
- 模拟邮件增加 accountId 字段
- 列表和阅读器显示账户色点与邮箱
- 点击不同账户的收件箱会按 accountId 过滤

---

## 10. 安全设计

| 面 | 措施 |
|---|---|
| 认证 | 单用户密码 + HMAC 签名 Cookie，30 天有效 |
| 会话 | HttpOnly、Secure（HTTPS 时）、SameSite=Lax |
| CSRF | 写操作校验 Origin；关键操作可加一次性 token |
| 凭据 | AES-256-GCM 加密；密钥仅存环境变量；日志脱敏 |
| 邮件 HTML | bluemonday 白名单消毒 + CSP + 默认禁图 |
| SSRF | 自定义 IMAP 主机解析后校验，默认拒绝内网 IP |
| 附件 | 流式下载；文件名净化；可选 ClamAV 扫描 |
| 日志 | 不打印密码、token、完整邮件正文 |
| 代理信任 | 只信任 TRUSTED_PROXY_CIDRS 的 X-Forwarded-* |
| 依赖 | 锁定版本，定期 govulncheck |

---

## 11. 部署与运维

### 11.1 环境要求

- Linux x86_64 或 arm64
- 1C1G 起步，2C2G 推荐
- 外部反向代理（Nginx/Traefik/Cloudflare Tunnel 等）
- 无需 Node、Redis、PostgreSQL

### 11.2 构建

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o zmail ./cmd/zmail
```

产物为单个静态二进制。

### 11.3 systemd

```ini
[Unit]
Description=zMail
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=zmail
Group=zmail
WorkingDirectory=/var/lib/zmail
EnvironmentFile=/etc/zmail/zmail.env
ExecStart=/opt/zmail/zmail
Restart=always
RestartSec=2
TimeoutStopSec=20
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/zmail

[Install]
WantedBy=multi-user.target
```

### 11.4 外部反代要求（不属于本服务）

- 终止 TLS
- SSE 路由关闭缓冲：

```nginx
location /api/events {
    proxy_pass http://127.0.0.1:7080;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_cache off;
    gzip off;
    proxy_read_timeout 3600s;
}
```

- 设置请求体上限（如 25MB）
- 传递 X-Forwarded-Proto 与 X-Forwarded-For

### 11.5 备份

```bash
sqlite3 /var/lib/zmail/zmail.db ".backup '/backup/zmail-2026-01-01.db'"
tar -czf /backup/raw-2026-01-01.tar.gz /var/lib/zmail/raw
```

建议每日备份，保留 14 天，并定期做恢复演练。

### 11.6 监控

- /healthz 返回 200
- 日志输出到 stdout，由 journald 收集
- 关键日志：账号连接失败、同步异常、SMTP 发送失败、DB 错误
- 可选：暴露 /metrics 给 Prometheus

---

## 12. 从零构建步骤

### 12.1 初始化

```bash
mkdir -p zMail && cd zMail
go mod init zmail
mkdir -p cmd/zmail internal/config internal/secret internal/store internal/mail internal/realtime internal/verify internal/httpapi web/static deploy docs data/raw
```

### 12.2 拉依赖

```bash
go get github.com/emersion/go-imap@v1.2.1
go get github.com/emersion/go-message@latest
go get github.com/wneessen/go-mail@latest
go get github.com/microcosm-cc/bluemonday@latest
go get modernc.org/sqlite@latest
go get golang.org/x/text@latest
```

### 12.3 实现顺序

| 顺序 | 文件 | 验收 |
|---|---|---|
| 1 | internal/config/config.go | 环境变量缺失时启动报错清晰 |
| 2 | internal/secret/secret.go | 加解密往返验证通过 |
| 3 | internal/store/schema.sql 与 store.go | 建库成功，WAL 生效 |
| 4 | internal/realtime/hub.go | 多订阅者广播正常 |
| 5 | internal/httpapi/server.go | /healthz 和登录可用 |
| 6 | internal/mail/parse.go | 能解析中文主题、HTML、附件 |
| 7 | internal/mail/idle.go 与 fetch.go | 真实账号能秒级收到测试邮件 |
| 8 | internal/verify/code.go | 验证码邮件能提取 6 位码 |
| 9 | internal/mail/send.go | 能发信并出现在 Sent |
| 10 | 模板与前端 | 三栏界面可用 |
| 11 | deploy/ | systemd 启动、重启自恢复 |

### 12.4 本地运行

```bash
export LISTEN_ADDR=127.0.0.1:7080
export PUBLIC_URL=http://localhost:7080
export DB_PATH=./data/zmail.db
export APP_PASSWORD=change-me
export SESSION_KEY=$(openssl rand -base64 32)
export CREDENTIALS_KEY=$(openssl rand -base64 32)
export ALLOW_PRIVATE_IMAP=true

go run ./cmd/zmail
```

---

## 13. 手工验收

仓库不包含自动化测试代码与冒烟脚本；发布前按以下清单手工验证关键流程。

### 13.1 核心链路

- [ ] `-check-config` 能正确报告配置缺失与生效路径
- [ ] 登录、退出、CSRF 校验正常
- [ ] 添加账号、编辑连接参数、测试连接、停用/启用正常
- [ ] 能收到新邮件，IDLE 账号延迟不超过 3 秒
- [ ] 邮件列表分页、搜索、详情、附件下载正常
- [ ] 已读/星标/删除能回写 IMAP
- [ ] 发信、回复、转发、多附件正常，Sent 可见
- [ ] 验证码邮件能识别并一键复制
- [ ] 服务端 HTML 消毒、图片代理与 CSP 生效

### 13.2 故障注入

| 场景 | 预期 |
|---|---|
| 杀掉 IDLE 连接 | 指数退避重连，恢复后补齐 |
| UIDVALIDITY 变化 | 清空该文件夹并重新同步 |
| SQLite 锁 | 请求重试，不返回 500 |
| 单个账号授权码错误 | 其他账号不受影响，UI 显示异常 |
| 反代缓冲 SSE | 关闭缓冲后恢复秒级推送 |

### 13.3 部署验收

- [ ] systemd 或 Docker 启动后 `/healthz` 返回 200
- [ ] 重启进程后自动恢复账号与同步
- [ ] 备份 `/data` 后能恢复数据库与原始邮件
- [ ] 反代 SSE 不缓冲，静态资源缓存与升级指纹正常

---

## 14. 开发里程碑

> 下表为首版开发阶段的记录；当前功能已实现，后续以维护和小步迭代为主。

| 阶段 | 内容 | 估时 |
|---|---|---|
| M0 | 仓库、配置、加密、SQLite schema | 1 天 |
| M1 | 账号 CRUD、连接测试、登录 | 2 天 |
| M2 | IMAP IDLE + 增量同步 + SSE | 3 天 |
| M3 | 三栏 UI、列表、详情、已读 | 3 天 |
| M4 | 发信、回复、草稿 | 2 天 |
| M5 | 附件、搜索、验证码、通知 | 3 天 |
| M6 | 稳定性、部署、备份、验收 | 2 天 |
| 合计 | 约 16 个工作日 | |

---

## 15. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| QQ、163 不支持 IDLE | 收信延迟 10 到 15 秒 | 能力检测 + 短轮询兜底 |
| Gmail 应用密码政策变化 | 无法登录 | 预留 Gmail OAuth 适配器 |
| 163 IMAP 要求 ID 命令 | 登录被拒 | 实现 IMAP ID 扩展 |
| 邮件 HTML XSS | 安全风险 | bluemonday 白名单消毒 + CSP + 禁止远程图片 |
| SQLite 写锁 | 请求失败 | 单写者 + WAL + busy_timeout |
| 附件占满磁盘 | 服务不可用 | 去重、压缩、保留策略、磁盘告警 |
| IDLE 连接数过多 | 服务商限流 | 每账号 1 条，全局监控 |
| 反代缓冲 SSE | 收信变慢 | 明确反代配置要求 + 心跳检测 |
| go-imap API 变更 | 编译失败 | 锁定版本，适配层隔离 |

---

## 16. 附录

### 16.1 服务商配置参考

| 服务商 | IMAP | SMTP | 认证 | 备注 |
|---|---|---|---|---|
| Gmail | imap.gmail.com:993 SSL | smtp.gmail.com:465 SSL | 2FA + 应用专用密码 | 跳过 All Mail |
| Outlook 个人 | outlook.office365.com:993 SSL | smtp-mail.outlook.com:587 STARTTLS | OAuth2 或应用密码 | 企业版需管理员同意 |
| Microsoft 365 | outlook.office365.com:993 SSL | smtp.office365.com:587 STARTTLS | OAuth2 | 需 App 注册 |
| QQ 邮箱 | imap.qq.com:993 SSL | smtp.qq.com:465 SSL | 授权码 | 先在设置中开启 IMAP |
| 163 邮箱 | imap.163.com:993 SSL | smtp.163.com:465 SSL | 授权码 | 可能需要 IMAP ID 命令 |
| 自建 Dovecot/Postfix | 自填 | 自填 | 密码或 OAuth | 注意 max_user_connections |

### 16.2 常用命令

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o zmail ./cmd/zmail
go vet ./...
govulncheck ./...
sqlite3 data/zmail.db ".tables"
sqlite3 data/zmail.db "select count(*) from messages;"
sqlite3 data/zmail.db ".backup 'data/backup.db'"
```

### 16.3 环境变量完整表

| 变量 | 必填 | 默认 |
|---|---|---|
| LISTEN_ADDR | 否 | 127.0.0.1:7080 |
| PUBLIC_URL | 是 | - |
| DB_PATH | 否 | ./data/zmail.db |
| RAW_DIR | 否 | ./data/raw |
| APP_PASSWORD | 是 | - |
| SESSION_KEY | 是 | - |
| CREDENTIALS_KEY | 是 | - |
| MAX_FETCH_CONCURRENCY | 否 | 4 |
| IDLE_REFRESH_SECONDS | 否 | 1500 |
| BACKFILL_ENABLED | 否 | true |
| ALLOW_PRIVATE_IMAP | 否 | false |
| TRUSTED_PROXY_CIDRS | 否 | 127.0.0.1/32,::1/128 |
| LOG_LEVEL | 否 | info |
| ENV_FILE | 否 | ./.env |

### 16.4 术语

| 术语 | 说明 |
|---|---|
| IDLE | IMAP 协议的推送扩展，服务器有新邮件主动通知 |
| UID | IMAP 邮件在当前文件夹内的唯一递增编号 |
| UIDVALIDITY | 文件夹 UID 有效性标识，变化代表 UID 全部重置 |
| EXISTS | IMAP 通知邮件数量变化的响应 |
| SSE | Server-Sent Events，服务器单向推送 |
| FTS5 | SQLite 全文检索扩展 |
| WAL | SQLite 预写日志模式，提升读写并发 |

---

文档结束。
