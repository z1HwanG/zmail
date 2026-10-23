# zMail

[![Release](https://img.shields.io/github/v/release/z1HwanG/zmail?display_name=tag&label=release)](https://github.com/z1HwanG/zmail/releases)
[![CI](https://github.com/z1HwanG/zmail/actions/workflows/ci.yml/badge.svg)](https://github.com/z1HwanG/zmail/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](https://github.com/z1HwanG/zmail/blob/main/LICENSE)

> 单用户、Web 端、多账号聚合邮箱客户端。Go + SQLite(WAL) + IMAP IDLE + SSE，单二进制、单进程、低资源占用。

zMail 把 Gmail、Outlook、QQ、163 和自建 IMAP 邮箱聚合到一个三栏 Web 界面里，新邮件通过 IMAP IDLE 在 1~3 秒内推送到浏览器，验证码自动识别并一键复制。完整设计见 [docs/BUILD.md](docs/BUILD.md)。

> 环境要求：Go 1.26+（`modernc.org/sqlite` / `go-mail` 的最低要求）、SQLite 由纯 Go 驱动内置，无需 CGO、无需 Node。

## 文档导航

| 文档 | 内容 |
|---|---|
| [docs/BUILD.md](docs/BUILD.md) | 架构、数据模型、模块设计、API 与完整构建步骤 |
| [docs/CONTRACT.md](docs/CONTRACT.md) | 内部包接口与前端 DOM 契约 |
| [docs/DEPLOY.md](docs/DEPLOY.md) | 二进制/systemd、Docker、反向代理与运维 |
| [docs/reverse-proxy.md](docs/reverse-proxy.md) | Nginx / Caddy / Traefik 反代与 SSE 配置 |
| [docs/providers.md](docs/providers.md) | Gmail、Outlook、QQ、163、自建邮箱接入 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更记录 |
| [SECURITY.md](SECURITY.md) | 安全策略与漏洞报告 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 构建、验证与贡献说明 |

## 特性

- **多账号聚合**：任意数量账号统一收件箱，按账号分组浏览；已添加账号可随时编辑连接参数/密码、停用或启用
- **秒级收信**：每账号一条 IDLE 专用连接；不支持 IDLE 的服务商自动降级为短轮询
- **稳定不漏信**：断线指数退避重连、UIDVALIDITY 变更检测、按 UID 增量补齐
- **可翻到很旧的邮件**：游标分页 + 后台历史回填（向更早 UID 方向滚动补齐），列表底部还有「加载更早邮件（从服务器拉取）」入口
- **账号可折叠**：侧栏每个账号可单独折叠/展开，状态记在浏览器本地，账号多也好管理
- **连续浏览邮件**：阅读器内「上一封 / 下一封」＋位置指示（如 3 / 14），移动端有「返回列表」，不必反复回列表找下一封
- **升级即时生效**：静态资源带构建指纹（`?v=`）并长缓存，服务端升级后浏览器与 Service Worker 会自动拉取新资源，不会继续用旧 app.js
- **验证码识别**：中文/英文模板提取，右下角大号 Toast + 一键复制 + 浏览器通知
- **发信/回复/转发**：SMTP（SSL/STARTTLS）发送 + APPEND 到 Sent，失败自动入队重试；Composer 支持多附件
- **已读/星标/删除回写**：本地即时生效，删除异步回写 IMAP（STORE \Deleted + EXPUNGE），失败写同步日志并重试
- **安全**：凭据 AES-256-GCM 加密、HMAC 签名会话、CSRF Origin 校验、邮件 HTML 服务端白名单消毒、CSP、SSRF 防护（含远程图片代理）
- **轻量前端**：html/template + 原生 JS（htmx 本地 vendored），无构建链、无 CDN、深色模式、键盘快捷键、命令面板、PWA
- **单二进制部署**：CGO_ENABLED=0 静态编译，systemd 托管，TLS 交给外部反代

## 快速开始

```bash
# 1. 准备配置
cp deploy/zmail.env.example .env   # 或直接 export

# 2. 生成密钥（SESSION_KEY / CREDENTIALS_KEY 各执行一次）
go run ./cmd/zmail -genkey

# 3. 本地运行
export LISTEN_ADDR=127.0.0.1:7080
export PUBLIC_URL=http://localhost:7080
export DB_PATH=./data/zmail.db
export RAW_DIR=./data/raw
export APP_PASSWORD=change-me
export SESSION_KEY=<上一步输出>
export CREDENTIALS_KEY=<上一步输出>
export ALLOW_PRIVATE_IMAP=true   # 仅自建内网 IMAP 需要

go run ./cmd/zmail
```

浏览器打开 <http://localhost:7080>，用 `APP_PASSWORD` 登录，然后在「账号管理」里添加邮箱。

> **关于 `.env`**：程序启动时会自动读取**当前工作目录**下的 `.env`（支持 `#` 注释、`export KEY=VALUE`、引号值），已存在的进程环境变量优先，不会被覆盖。用 `ENV_FILE=/path/to/x.env` 可指定其他文件，`ENV_FILE=-` 可禁用。systemd 部署时由 `EnvironmentFile=` 注入，无需 `.env`。配置是否生效可用 `go run ./cmd/zmail -check-config` 检查。

## 构建与部署

Linux / macOS（需要 GNU make）：

```bash
make build          # CGO_ENABLED=0 静态二进制
make check          # build + vet
```

Windows（系统默认没有 GNU make，用自带的等价脚本）：

```bat
make.cmd build       # 生成 zmail.exe（CGO_ENABLED=0 静态编译），会自动绕过脚本执行策略
make.cmd check       # build + vet
make.cmd run         # 本地运行
make.cmd genkey      # 生成 SESSION_KEY / CREDENTIALS_KEY
make.cmd build-linux # 交叉编译 Linux amd64/arm64（服务器部署用）
```

或在 PowerShell 7 里直接运行 ` .\make.ps1 build `（脚本策略受限时用上面的 `make.cmd`）。

不想用脚本时，直接执行等价命令即可：

```powershell
$env:CGO_ENABLED=0; go build -trimpath -ldflags "-s -w" -o zmail.exe ./cmd/zmail
go vet ./...; go run ./cmd/zmail
```

> 想继续用 `make` 的话可以安装 GNU make：`winget install ezwinports.make`、`scoop install make`、`choco install make` 或 MSYS2 的 `pacman -S make`，然后在 Git Bash/MSYS2 里执行 Makefile。

## 质量检查

```bash
go vet ./...
```

仓库不包含自动化测试代码与冒烟脚本；发布前请按 [docs/BUILD.md](docs/BUILD.md) 第 13 章的验收清单手工验证关键流程。

部署方式：

- **通用部署（推荐）**：见 [docs/DEPLOY.md](docs/DEPLOY.md)——二进制 + systemd、Docker Compose、反向代理与 HTTPS、SSE 关缓冲、备份升级和排错清单
- 通用 Linux 部署：见 [docs/BUILD.md](docs/BUILD.md) 第 11 章
- 反代细节：[docs/reverse-proxy.md](docs/reverse-proxy.md)；服务商参数与授权码申请：[docs/providers.md](docs/providers.md)

```bash
make build-linux   # 交叉编译 dist/zmail-linux-amd64 与 -arm64，上传服务器即可
```

Windows 未安装 GNU make 时使用 `make.cmd build-linux`（等价于 `.\make.ps1 build-linux`）。

## 目录结构

```
cmd/zmail/           入口：配置、DB、HTTP、同步引擎生命周期
internal/config/     环境变量加载与校验
internal/secret/     AES-256-GCM 凭据加解密
internal/store/      SQLite(WAL) 持久层 + blob 存储（原始 .eml / 附件）
internal/mail/       IMAP IDLE、增量同步、MIME 解析、SMTP 发信、历史回填
internal/realtime/   SSE 事件广播 Hub
internal/verify/     验证码提取
internal/imgproxy/   远程图片代理（SSRF 防护、大小/超时/重定向限制）
internal/httpapi/    HTTP/API、鉴权、SSE 路由
internal/httpapi/templates/  HTML 页面模板
internal/web/        静态资源（embed：CSS/JS/图标/PWA）
scripts/             本地开发环境脚本
deploy/              systemd unit、Dockerfile、Compose、环境变量示例
docs/                架构设计、接口契约、部署、反代与服务商说明
Makefile             Linux/macOS 构建入口
make.ps1 / make.cmd  Windows 构建入口
```

## 环境变量

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `LISTEN_ADDR` | 否 | `127.0.0.1:7080` | 只监听本机，TLS 由反代负责 |
| `PUBLIC_URL` | 是 | - | 外部访问地址，用于 Cookie/CSRF 校验 |
| `DB_PATH` | 否 | `./data/zmail.db` | SQLite 文件路径 |
| `RAW_DIR` | 否 | `./data/raw` | 原始邮件与附件 blob 目录 |
| `APP_PASSWORD` | 是 | - | 单用户登录密码 |
| `SESSION_KEY` | 是 | - | base64 32 字节，会话签名 |
| `CREDENTIALS_KEY` | 是 | - | base64 32 字节，凭据加密 |
| `MAX_FETCH_CONCURRENCY` | 否 | `4` | 全局拉取并发 |
| `IDLE_REFRESH_SECONDS` | 否 | `1500` | IDLE 重发间隔 |
| `BACKFILL_ENABLED` | 否 | `true` | 是否启用历史回填 |
| `ALLOW_PRIVATE_IMAP` | 否 | `false` | 是否允许连接内网 IMAP |
| `TRUSTED_PROXY_CIDRS` | 否 | `127.0.0.1/32,::1/128` | 受信反代网段 |
| `LOG_LEVEL` | 否 | `info` | debug/info/warn/error |
| `ENV_FILE` | 否 | `./.env` | `.env` 路径；`-` 表示禁用自动加载；进程环境变量优先级更高 |

## 健康检查

`GET /healthz` 返回 `{"status":"ok","time":"..."}`（数据库不可用时为 503 + `status=error`），可直接接 systemd/负载均衡探针。

## 常见问题

### 登录时返回 `{"error":{"code":"FORBIDDEN","message":"跨站请求被拒绝"}}`

两种常见原因：

1. **浏览器地址与 `PUBLIC_URL` 不一致**，例如 `.env` 里配的是 `http://localhost:7080`，却用 `http://127.0.0.1:7080` 打开；
2. **页面响应头带 `Referrer-Policy: no-referrer`**（旧版本或反代自行添加）会让 Chrome 把表单 POST 的 `Origin` 头降级为 `null`。项目页面现已改为 `same-origin`，并把 `Origin: null` 当作“来源不可知”处理（登录仍受密码校验与限速保护）；若反代额外加了 `no-referrer`，请改成 `same-origin`。

- 现已兼容 `localhost` / `127.0.0.1` / `[::1]` 回环等价，以及“Origin 与当前请求 Host 一致”的同源判定；
- 仍建议把 `PUBLIC_URL` 改成你实际访问的地址（域名、协议、端口都要一致），否则页面里的绝对链接、Cookie `Secure` 判定等仍可能对不上；
- 修改 `.env` 后需要重启进程；可用 `go run ./cmd/zmail -check-config` 确认生效的配置；
- 反代部署时 `PUBLIC_URL` 填外部域名（https），并确保 `TRUSTED_PROXY_CIDRS` 覆盖反代地址，程序会读取 `X-Forwarded-Proto`。

### 启动报 `config: 缺少必填环境变量: ...`

程序只自动读取**当前工作目录**下的 `.env`（或其路径由 `ENV_FILE` 指定）；用 `go run` 时请确保在项目根目录执行，或显式 `export` 变量。运行 `go run ./cmd/zmail -check-config` 可看到实际加载的 `envFile` 与配置。

## 版本

当前版本：`0.1.1`。变更记录见 [CHANGELOG.md](CHANGELOG.md)，发布包见 [GitHub Releases](https://github.com/z1HwanG/zmail/releases)。

## 许可

本项目采用 [MIT License](LICENSE) 开源许可。
