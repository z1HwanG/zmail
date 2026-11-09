# zMail

[![Release](https://img.shields.io/github/v/release/z1HwanG/zmail?display_name=tag&label=release)](https://github.com/z1HwanG/zmail/releases)

[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

**把多个邮箱放进一个轻量、私有的 Web 邮箱。** zMail 使用 Go、SQLite、IMAP 和 SMTP；支持 Gmail、Outlook、QQ、163 与标准 IMAP 邮箱，提供验证码识别和多账号聚合收件箱。

> 单用户、自托管、单进程；邮件数据与密钥留在你控制的服务器。当前版本：**0.1.2**。

## 特性

- **集中收信**：多个 IMAP 账号聚合浏览，支持收件箱、已发送及其他系统文件夹。
- **及时同步**：支持 IMAP IDLE；服务器不支持时回退到短轮询，断线后自动重连。
- **保留文件夹语义**：按 IMAP SPECIAL-USE 和常见精确名称识别系统文件夹，避免将普通同名目录误判。
- **邮件操作**：SMTP 发信、回复、转发和附件；已读、星标、删除可回写服务器。
- **历史邮件**：游标分页，支持按目录以及收件箱/已发送范围回填。
- **验证码助手**：自动提取邮件验证码，可一键复制并选择浏览器通知。
- **安全与隐私**：AES-256-GCM 加密邮箱凭据、服务端 HTML 消毒、CSP、CSRF 校验和 SSRF 防护。
- **轻量部署**：纯 Go 单二进制，也支持 Docker Compose；前端资源内置，无 Node 构建链和 CDN 依赖。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/DEPLOY.md](docs/DEPLOY.md) | 1Panel、systemd、Docker Compose、备份与升级 |
| [docs/providers.md](docs/providers.md) | 邮箱服务器、授权码及文件夹识别说明 |
| [docs/reverse-proxy.md](docs/reverse-proxy.md) | HTTPS 反向代理与 SSE 配置 |
| [docs/BUILD.md](docs/BUILD.md) | 架构、数据模型和构建说明 |
| [docs/CONTRACT.md](docs/CONTRACT.md) | 内部接口契约 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更 |
| [SECURITY.md](SECURITY.md) | 安全策略和漏洞报告 |

## 本地运行

要求 Go 1.26 或更新版本。SQLite 使用纯 Go 驱动；无需 CGO、无需 Node。

~~~bash
# 获取源码并进入仓库
git clone https://github.com/z1HwanG/zmail.git
cd zmail

# 复制配置；SESSION_KEY 和 CREDENTIALS_KEY 各自生成一次
cp deploy/zmail.env.example .env
go run ./cmd/zmail -genkey

# 按命令输出分别填写密钥后校验并运行
go run ./cmd/zmail -check-config
go run ./cmd/zmail
~~~

浏览器访问 <http://127.0.0.1:7080>，用 APP_PASSWORD 登录后添加邮箱。生产环境请将 PUBLIC_URL 设为真实 HTTPS 域名，并使用独立、随机的 32 字节 SESSION_KEY 与 CREDENTIALS_KEY。配置项见 [deploy/zmail.env.example](deploy/zmail.env.example)。

## 构建与验证

~~~bash
# Linux / macOS（需要 GNU make）
make build
make check

# 静态分析与构建
go vet ./...
go build ./cmd/zmail
~~~

Windows 可用 make.cmd build、make.cmd check。`.forgejo/workflows/ci.yml` 会在 Forgejo Actions 上检查 gofmt、go vet 和构建；真实 IMAP/SMTP 连接与反向代理需在部署环境另外验收。

## Forgejo Actions 自动部署

可用 Forgejo Actions 在推送到 main 后构建 Linux 二进制，通过 SSH 安装到服务器应用目录并重启 systemd 服务。服务器应预先创建服务和运行配置，并通过 sudoers 仅允许部署账号运行固定的部署脚本。**不要将数据库、原始邮件、附件或密钥放在发布二进制目录中**；它们应位于独立的持久化数据目录。

### 配置仓库 Secrets

在 Forgejo 仓库的 Actions Secrets 中配置：

| Secret | 说明 |
|---|---|
| DEPLOY_HOST | 服务器主机名或地址 |
| DEPLOY_USER | 专用、受限的 SSH 部署用户 |
| DEPLOY_SSH_KEY | 部署专用私钥 |
| DEPLOY_KNOWN_HOSTS | 预先核验的 SSH 主机公钥记录 |
| DEPLOY_INSTALL_SCRIPT | 固定的服务器部署脚本，例如 `/usr/local/sbin/deploy-zmail` |

### 添加工作流

创建 .forgejo/workflows/deploy.yml：

~~~yaml
name: Deploy
on:
  push:
    branches: [main]

jobs:
  deploy:
    # 替换成 Forgejo Runner 注册时配置的标签。
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version: 1.26.x
      - name: Build Linux binary
        run: CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o zmail ./cmd/zmail
      - name: Install and restart
        env:
          DEPLOY_HOST: ${{ secrets.DEPLOY_HOST }}
          DEPLOY_USER: ${{ secrets.DEPLOY_USER }}
          DEPLOY_SSH_KEY: ${{ secrets.DEPLOY_SSH_KEY }}
          DEPLOY_KNOWN_HOSTS: ${{ secrets.DEPLOY_KNOWN_HOSTS }}
          DEPLOY_INSTALL_SCRIPT: ${{ secrets.DEPLOY_INSTALL_SCRIPT }}
        run: |
          install -d -m 700 ~/.ssh
          printf '%s\n' "$DEPLOY_SSH_KEY" > ~/.ssh/deploy_key
          chmod 600 ~/.ssh/deploy_key
          printf '%s\n' "$DEPLOY_KNOWN_HOSTS" > ~/.ssh/known_hosts
          scp -i ~/.ssh/deploy_key -o IdentitiesOnly=yes zmail "$DEPLOY_USER@$DEPLOY_HOST:/tmp/zmail-$GITHUB_SHA"
          ssh -i ~/.ssh/deploy_key -o IdentitiesOnly=yes "$DEPLOY_USER@$DEPLOY_HOST" "sudo -n '$DEPLOY_INSTALL_SCRIPT' '/tmp/zmail-$GITHUB_SHA'"
~~~

先确认 Runner 标签、专用部署用户以及服务器上的 /usr/local/sbin/deploy-zmail。该脚本应校验输入文件、原子替换固定路径下的二进制并重启服务；sudo 仅授权执行此固定脚本。推送到 main 后检查 Actions 日志、服务状态和 /healthz。完整手动部署步骤见 [docs/DEPLOY.md](docs/DEPLOY.md)。

> 上述流程用于说明部署模式；本仓库不包含你的服务器地址、私钥、主机指纹或 systemd 配置。请将这些值保存在 Forgejo Secrets，不要写入仓库。

## 目录结构

~~~text
cmd/zmail/                 程序入口
internal/mail/             IMAP 同步、文件夹识别、SMTP 发信
internal/store/            SQLite 与原始邮件/附件存储
internal/httpapi/          HTTP API 和 HTML 页面
internal/web/static/       内置 CSS、JavaScript、图标与 PWA 资源
deploy/                    Docker、systemd 和环境变量示例
docs/                      架构、部署及邮箱服务商指南
~~~

## 版本与许可

当前版本为 **0.1.2**。变更见 [CHANGELOG.md](CHANGELOG.md)，发布页见 [GitHub Releases](https://github.com/z1HwanG/zmail/releases)。项目采用 [MIT License](LICENSE)。
