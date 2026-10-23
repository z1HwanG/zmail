# 在 1Panel 服务器上部署 zMail

> 目标形态：**zMail 只监听 127.0.0.1:7080**，由 1Panel 的 OpenResty/Nginx 反向代理对外提供 HTTPS。TLS、证书续期、域名都由 1Panel 负责，zMail 本身不处理证书。
>
> 推荐「方式 A：二进制 + systemd」（资源占用最低、升级最简单）；想用容器可选「方式 B：Docker」。

---

## 0. 准备

| 项 | 要求 |
|---|---|
| 系统 | Linux x86_64 / arm64（1C1G 起步） |
| 1Panel | 已安装 OpenResty（1Panel 默认自带）和终端 |
| 域名 | 一个解析到服务器的域名，例如 `mail.example.com` |
| 端口 | 公网只放行 80/443（和 1Panel 面板端口），**不要**对外暴露 7080 |

zMail 需要 4 个必填配置：`PUBLIC_URL`、`APP_PASSWORD`、`SESSION_KEY`、`CREDENTIALS_KEY`。

---

## 方式 A：二进制 + systemd（推荐）

### A1. 本地构建 Linux 二进制

在项目根目录执行（Linux/macOS 使用 `make build-linux`；Windows 未安装 GNU make 时使用 `make.cmd build-linux`，纯 Go 交叉编译，无需 Docker）：

```bash
make build-linux
# 产物：
# dist/zmail-linux-amd64   （x86_64 服务器用这个）
# dist/zmail-linux-arm64   （ARM 服务器用这个）
```

等价命令：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/zmail-linux-amd64 ./cmd/zmail
```

### A2. 上传并放到 /opt/zmail

用 1Panel「主机 → 文件」把 `zmail-linux-amd64` 上传到 `/opt/zmail/`，或直接终端：

```bash
scp dist/zmail-linux-amd64 root@服务器IP:/opt/zmail/zmail
```

然后在 1Panel「终端」里：

```bash
# 创建专用系统用户（无登录权限）
useradd --system --home-dir /var/lib/zmail --shell /usr/sbin/nologin zmail || true

mkdir -p /opt/zmail /etc/zmail /var/lib/zmail
mv /opt/zmail/zmail-linux-amd64 /opt/zmail/zmail
chmod 0755 /opt/zmail/zmail
chown -R zmail:zmail /var/lib/zmail
```

### A3. 生成密钥

```bash
/opt/zmail/zmail -genkey   # 生成 SESSION_KEY，复制输出
/opt/zmail/zmail -genkey   # 再生成 CREDENTIALS_KEY
/opt/zmail/zmail -version  # 确认二进制可执行
```

> `CREDENTIALS_KEY` 决定邮箱密码的加密密钥，**一旦使用就不要更换**，否则已保存的账号密码无法解密；如需更换必须重新录入账号密码。

### A4. 写环境变量文件

把 `deploy/zmail.env.example` 上传到 `/etc/zmail/zmail.env`，或用下面内容创建（按需改域名、密钥）：

```bash
cat >/etc/zmail/zmail.env <<'EOF'
LISTEN_ADDR=127.0.0.1:7080
PUBLIC_URL=https://mail.example.com

DB_PATH=/var/lib/zmail/zmail.db
RAW_DIR=/var/lib/zmail/raw

APP_PASSWORD=改成你的强密码
SESSION_KEY=粘贴第一步生成的密钥
CREDENTIALS_KEY=粘贴第二步生成的密钥

MAX_FETCH_CONCURRENCY=4
IDLE_REFRESH_SECONDS=1500
BACKFILL_ENABLED=true
ALLOW_PRIVATE_IMAP=false

TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
LOG_LEVEL=info
EOF

chown root:zmail /etc/zmail/zmail.env
chmod 0640 /etc/zmail/zmail.env
```

注意：

- `PUBLIC_URL` 必须与最终访问的域名 + 协议完全一致（用于 Cookie 与 CSRF 校验），`https` 不能写成 `http`；
- 只有自建内网邮箱（如内网 Dovecot）才需要把 `ALLOW_PRIVATE_IMAP` 设为 `true`；
- 若反代不在本机，把代理的出口 IP 段加入 `TRUSTED_PROXY_CIDRS`（1Panel 反代一般就是本机，保持默认即可）。

### A5. 安装 systemd 服务

```bash
cat >/etc/systemd/system/zmail.service <<'EOF'
[Unit]
Description=zMail 聚合邮箱
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
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now zmail
systemctl status zmail --no-pager
```

### A6. 本机自检

```bash
curl -s http://127.0.0.1:7080/healthz
# {"status":"ok","time":"..."}

journalctl -u zmail -n 50 --no-pager    # 查看启动日志
```

---

## 1. 在 1Panel 配置域名 + HTTPS + 反向代理

### 1.1 建站

1Panel →「网站」→「创建网站」→ 选择 **反向代理**：

| 字段 | 值 |
|---|---|
| 域名 | `mail.example.com` |
| 代理地址 | `http://127.0.0.1:7080` |
| 发送域名 | `$host`（默认即可） |

创建后到「网站 → 域名」确认域名已绑定。

### 1.2 申请证书

「网站 → 详情 → HTTPS」→ 开启 → 选择 Let's Encrypt 申请证书 → 保存并开启「强制 HTTPS」。

### 1.3 处理 SSE（关键）

zMail 的实时收信走 `/api/events`（SSE）。如果反代缓冲响应，新邮件会延迟到连接关闭才出现，必须关闭该路径的缓冲。

打开「网站 → 配置 → 配置文件」（OpenResty 配置），在 `server { ... }` 内加入：

```nginx
# SSE 实时推送：关闭缓冲与压缩
location /api/events {
    proxy_pass http://127.0.0.1:7080;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_cache off;
    gzip off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    add_header X-Accel-Buffering no;
}

# 发信允许最大 25MB 请求体（附件）
client_max_body_size 25m;
```

如果是 1Panel 的「反向代理 → 源文/自定义配置」入口，把上面这段放到对应站点的代理配置里，效果相同。

保存后重载 OpenResty（1Panel 会自动重载；也可「应用商店 → OpenResty → 重载」）。

验证 SSE 是否生效：

```bash
curl -N -H "Cookie: 登录后的zmail_session=..." https://mail.example.com/api/events
# 应立刻看到 "retry: 3000" 和 ": connected"，并每 20 秒收到 ": ping"
```

### 1.4 防火墙

1Panel →「主机 → 防火墙」：

- 放行 80、443（以及 1Panel 面板端口）；
- **不要**放行 7080；
- 云厂商安全组同样只开放 80/443。

---

## 2. 首次使用

1. 浏览器打开 `https://mail.example.com`，用 `APP_PASSWORD` 登录；
2. 「账号设置 → 添加账号」：填邮箱、授权码/应用密码；常用服务商（Gmail/Outlook/QQ/163）可只选服务商，服务器地址自动填充；
3. 添加后会后台连接 IMAP 并发现文件夹，稍等即可在「全部收件箱」看到邮件；
4. 邮件多的账号：列表底部点「**加载更早邮件（从服务器拉取）**」，后台也会持续回填历史邮件（首次同步只取最近 500 封）。

---

## 3. 备份（1Panel 计划任务）

zMail 的数据只有两处：SQLite 数据库 + 原始邮件/附件目录。

1Panel →「计划任务」→「创建任务」→ 类型 **Shell 脚本**，每天一次，示例：

```bash
#!/bin/bash
set -e
BACKUP_DIR=/opt/backup/zmail
DATE=$(date +%F)
mkdir -p "$BACKUP_DIR"

# SQLite 在线备份（需要 sqlite3；没有可 apt install -y sqlite3）
sqlite3 /var/lib/zmail/zmail.db ".backup '$BACKUP_DIR/zmail-$DATE.db'"

# 原始邮件与附件（体积较大，可按需降低频率）
tar -czf "$BACKUP_DIR/raw-$DATE.tar.gz" -C /var/lib/zmail raw

# 只保留 14 天
find "$BACKUP_DIR" -type f -mtime +14 -delete
```

把备份目录再通过 1Panel「计划任务 → 备份到云存储」或 rclone 同步到对象存储更稳妥。恢复时停服务 → 替换 db/raw → 启动。

> 不装 sqlite3 也可以临时用 `systemctl stop zmail && cp zmail.db* ...` 冷备，但要先停服务避免 WAL 不一致。

---

## 4. 升级

```bash
# 1) 本地重新构建并上传
make build-linux
# 上传 dist/zmail-linux-amd64 到 /opt/zmail/zmail.new

# 2) 服务器上替换并重启
chmod 0755 /opt/zmail/zmail.new
mv /opt/zmail/zmail /opt/zmail/zmail.bak
mv /opt/zmail/zmail.new /opt/zmail/zmail
systemctl restart zmail
curl -s http://127.0.0.1:7080/healthz
```

页面静态资源带构建指纹，刷新浏览器即是新版本，无需手动清缓存。

---

## 方式 B：Docker / 1Panel 编排

适合希望在 1Panel「容器」里统一管理的场景，**注意数据卷必须持久化**。

### B1. 构建镜像

```bash
make build-linux                       # 得到 dist/zmail-linux-amd64
# 把 deploy/Dockerfile 和 dist/zmail-linux-amd64 上传到服务器同一目录：
#   /opt/zmail/docker/Dockerfile
#   /opt/zmail/docker/zmail-linux-amd64
cd /opt/zmail/docker
docker build -f Dockerfile -t zmail:latest .
```

### B2. 用 1Panel 编排启动

把仓库里的 [deploy/docker-compose.yml](../deploy/docker-compose.yml) 复制到 `/opt/zmail/docker/docker-compose.yml`，同目录创建 `zmail.env`（内容同方式 A 的 zmail.env，但 `DB_PATH=/data/zmail.db`、`RAW_DIR=/data/raw` 不要改），然后：

- 1Panel →「容器 → 编排」→ 创建编排 → 粘贴 compose 内容 → 启动；
- 或终端：`docker compose up -d`。

容器只映射 `127.0.0.1:7080`，反代配置与方式 A 完全相同（指向 `http://127.0.0.1:7080`）。

数据目录权限：镜像内以非 root 的 `zmail` 用户运行，首次启动前要保证宿主机 `./data` 可写：

```bash
mkdir -p data
docker run --rm zmail:latest id          # 查看容器内 uid/gid，一般为 100:101
chown -R 100:101 data                    # 用上一步看到的 uid:gid
```

若日志出现 `permission denied` 或 SQLite 无法写入，基本都是这一步没做。

常用命令：

```bash
docker compose logs -f zmail
docker compose restart zmail
docker compose pull   # 仅镜像方式升级时使用
```

> 升级方式 B：重建镜像后 `docker compose up -d --force-recreate`，数据在 `./data` 卷里不受影响。

---

## 5. 排错清单

| 现象 | 排查 |
|---|---|
| 浏览器 502 / 502 Bad Gateway | `systemctl status zmail`、`curl 127.0.0.1:7080/healthz`；确认反代目标地址和端口 |
| 登录成功但立刻退出/一直跳登录页 | `PUBLIC_URL` 与实际访问的协议+域名不一致；清 Cookie 重试；检查反代是否传 `X-Forwarded-Proto` |
| 新邮件不实时、要刷新才出现 | SSE 被缓冲：按 1.3 关闭 `/api/events` 的 `proxy_buffering` / `gzip`；确认 `PUBLIC_URL` 是 https |
| 发信/附件 413 | nginx `client_max_body_size` 调大到 25m |
| 添加账号提示连接失败 | 自建内网 IMAP 需 `ALLOW_PRIVATE_IMAP=true`；QQ/163 需先在邮箱设置里开启 IMAP 并使用授权码；服务器出站 993/465 是否放行 |
| 数据库 `database is locked` | 确认只有一个 zMail 进程；数据目录在本地盘且 `zmail` 用户可写；不要把 SQLite 放 NFS |
| 升级后页面还是旧样式 | 正常刷新即可（资源带 ?v= 指纹）；若浏览器强缓存，Ctrl+F5 |
| 内存占用高 | 降低 `MAX_FETCH_CONCURRENCY`；确认没重复启动多个实例 |

---

## 6. 健康检查

- 应用：`GET /healthz` 返回 200（数据库异常时 503）
- 1Panel「网站 → 监控」可观察请求量；也可在 1Panel「计划任务」加一个每分钟 curl 自检并告警
- 日志：`journalctl -u zmail -f`（systemd）或 `docker compose logs -f`（容器）
