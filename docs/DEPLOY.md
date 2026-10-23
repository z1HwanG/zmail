# zMail 部署文档

> 目标形态：zMail 只监听 `127.0.0.1:7080`，TLS、证书和域名交给外部反向代理。支持 Linux 二进制、systemd 与 Docker Compose 三种部署方式。
>
> 推荐：单机低资源使用「二进制 + systemd」；希望统一管理容器时使用「Docker Compose」。

## 目录

1. [部署形态](#1-部署形态)
2. [准备](#2-准备)
3. [方式一：二进制 + systemd](#3-方式一二进制--systemd)
4. [方式二：Docker Compose](#4-方式二docker-compose)
5. [反向代理与 HTTPS](#5-反向代理与-https)
6. [首次使用](#6-首次使用)
7. [备份](#7-备份)
8. [升级](#8-升级)
9. [健康检查与排障](#9-健康检查与排障)
10. [安全清单](#10-安全清单)

---

## 1. 部署形态

```text
浏览器 / 邮件客户端
        │ HTTPS
        ▼
Nginx / Caddy / 1Panel OpenResty
        │ http://127.0.0.1:7080
        ▼
zMail 单进程
        ├─ SQLite (WAL)
        ├─ 原始邮件 / 附件 blob
        └─ IMAP IDLE + SMTP
```

zMail 不处理 TLS，也不建议直接把 7080 暴露到公网。

---

## 2. 准备

| 项 | 要求 |
|---|---|
| 系统 | Linux x86_64 / arm64，1C1G 起步 |
| 域名 | 已解析到服务器，例如 `mail.example.com` |
| 端口 | 公网仅放行 80/443；7080 只允许本机或反代访问 |
| 反向代理 | Nginx、Caddy、Traefik 或 1Panel 自带 OpenResty |
| 必填配置 | `PUBLIC_URL`、`APP_PASSWORD`、`SESSION_KEY`、`CREDENTIALS_KEY` |

生成密钥：

```bash
go run ./cmd/zmail -genkey   # 或使用发布二进制 ./zmail -genkey
```

执行两次，分别保存 `SESSION_KEY` 与 `CREDENTIALS_KEY`。

> `CREDENTIALS_KEY` 用于加密已保存的邮箱密码，使用后不要更换；否则旧账号密码无法解密，需要重新录入。

---

## 3. 方式一：二进制 + systemd

### 3.1 构建或下载二进制

本地构建（Linux/macOS）：

```bash
make build-linux
# dist/zmail-linux-amd64
# dist/zmail-linux-arm64
```

Windows：

```bat
make.cmd build-linux
```

也可以直接从 [GitHub Releases](https://github.com/z1HwanG/zmail/releases) 下载对应架构的 `zmail-linux-amd64` 或 `zmail-linux-arm64`。

### 3.2 上传并创建用户

```bash
scp dist/zmail-linux-amd64 root@服务器IP:/opt/zmail/zmail
```

服务器上：

```bash
useradd --system --home-dir /var/lib/zmail --shell /usr/sbin/nologin zmail || true
mkdir -p /opt/zmail /etc/zmail /var/lib/zmail
chmod 0755 /opt/zmail/zmail
chown -R zmail:zmail /var/lib/zmail
```

### 3.3 写环境变量

```bash
cat >/etc/zmail/zmail.env <<'EOF'
LISTEN_ADDR=127.0.0.1:7080
PUBLIC_URL=https://mail.example.com

DB_PATH=/var/lib/zmail/zmail.db
RAW_DIR=/var/lib/zmail/raw

APP_PASSWORD=改成你的强密码
SESSION_KEY=粘贴生成的 SESSION_KEY
CREDENTIALS_KEY=粘贴生成的 CREDENTIALS_KEY

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

### 3.4 安装 systemd 服务

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

### 3.5 自检

```bash
curl -s http://127.0.0.1:7080/healthz
journalctl -u zmail -n 50 --no-pager
```

---

## 4. 方式二：Docker Compose

当前 Dockerfile 采用「静态二进制 + Alpine 运行镜像」方式，仓库提供 `deploy/Dockerfile` 与 `deploy/docker-compose.yml`。

### 4.1 准备目录与文件

```bash
mkdir -p /opt/zmail/docker
cd /opt/zmail/docker
```

下载部署文件：

```bash
curl -fL -o Dockerfile \
  https://raw.githubusercontent.com/z1HwanG/zmail/main/deploy/Dockerfile
curl -fL -o docker-compose.yml \
  https://raw.githubusercontent.com/z1HwanG/zmail/main/deploy/docker-compose.yml
```

下载 0.1.1 发布二进制（按架构选择）：

```bash
# x86_64
curl -fL -o zmail-linux-amd64 \
  https://github.com/z1HwanG/zmail/releases/download/0.1.1/zmail-linux-amd64

# ARM64 服务器改用：
# curl -fL -o zmail-linux-amd64 \
#   https://github.com/z1HwanG/zmail/releases/download/0.1.1/zmail-linux-arm64

chmod 0755 zmail-linux-amd64
```

也可以本地构建后上传：

```bash
make build-linux
scp dist/zmail-linux-amd64 deploy/Dockerfile deploy/docker-compose.yml \
    root@服务器IP:/opt/zmail/docker/
```

> Dockerfile 固定读取 `zmail-linux-amd64`。ARM 服务器上传时可直接用该文件名保存 arm64 二进制。

### 4.2 构建镜像

```bash
cd /opt/zmail/docker
docker build -f Dockerfile -t zmail:latest .
```

### 4.3 创建 `zmail.env`

```bash
cat > zmail.env <<'EOF'
LISTEN_ADDR=0.0.0.0:7080
PUBLIC_URL=https://mail.example.com

DB_PATH=/data/zmail.db
RAW_DIR=/data/raw

APP_PASSWORD=改成你的强密码
SESSION_KEY=粘贴生成的 SESSION_KEY
CREDENTIALS_KEY=粘贴生成的 CREDENTIALS_KEY

MAX_FETCH_CONCURRENCY=4
IDLE_REFRESH_SECONDS=1500
BACKFILL_ENABLED=true
ALLOW_PRIVATE_IMAP=false
TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
LOG_LEVEL=info
EOF

chmod 600 zmail.env
```

注意：

- `DB_PATH=/data/zmail.db`、`RAW_DIR=/data/raw` 不要改，否则数据不会落在挂载卷中
- `PUBLIC_URL` 必须与浏览器实际访问的 HTTPS 地址完全一致
- `ALLOW_PRIVATE_IMAP=true` 仅用于自建内网 IMAP

### 4.4 准备数据目录权限

镜像内以非 root 的 `zmail` 用户运行：

```bash
mkdir -p data
docker run --rm zmail:latest id   # 查看 uid/gid，一般为 100:101
chown -R 100:101 data             # 按上一步结果修改
```

### 4.5 启动与检查

```bash
docker compose up -d
docker compose ps
docker compose logs -f zmail
curl -s http://127.0.0.1:7080/healthz
```

Compose 默认只映射 `127.0.0.1:7080:7080`，由反向代理对外提供 HTTPS。

### 4.6 Docker 常用命令

```bash
cd /opt/zmail/docker
docker compose logs -f zmail
docker compose restart zmail
docker compose down
```

升级：

```bash
# 替换 zmail-linux-amd64 后重新构建
docker build -f Dockerfile -t zmail:latest .
docker compose up -d --force-recreate
```

数据保存在 `/opt/zmail/docker/data`，重建容器不会丢失。备份时直接备份该目录。

---

## 5. 反向代理与 HTTPS

### 5.1 Nginx 示例

```nginx
server {
    listen 80;
    server_name mail.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name mail.example.com;

    ssl_certificate     /etc/letsencrypt/live/mail.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mail.example.com/privkey.pem;

    client_max_body_size 25m;

    location / {
        proxy_pass http://127.0.0.1:7080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # SSE 实时推送：必须关闭缓冲与压缩
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
}
```

Caddy / Traefik 配置见 [reverse-proxy.md](reverse-proxy.md)。

### 5.2 1Panel 反向代理

1. 「网站 → 创建网站 → 反向代理」
2. 域名填 `mail.example.com`，代理地址填 `http://127.0.0.1:7080`
3. 在「HTTPS」中申请 Let's Encrypt 证书并开启强制 HTTPS
4. 在站点配置文件中加入上面的 SSE `location /api/events` 配置和 `client_max_body_size 25m`
5. 防火墙只放行 80/443，不要放行 7080

### 5.3 验证 SSE

```bash
curl -N -H "Cookie: 登录后的zmail_session=..." \
  https://mail.example.com/api/events
```

应立刻看到 `retry: 3000` 和 `: connected`，并每 20 秒收到 `: ping`。

---

## 6. 首次使用

1. 浏览器打开 `https://mail.example.com`，用 `APP_PASSWORD` 登录
2. 「账号设置 → 添加账号」填写邮箱和授权码/应用密码
3. 常用服务商（Gmail / Outlook / QQ / 163）可选择服务商自动填充服务器地址
4. 首次同步只拉最近 500 封；列表底部可点「加载更早邮件（从服务器拉取）」，后台也会继续回填

服务商参数与授权码申请见 [providers.md](providers.md)。

---

## 7. 备份

zMail 的运行数据只有两处：

- SQLite 数据库：`DB_PATH`
- 原始邮件与附件：`RAW_DIR`

### 7.1 systemd 部署

```bash
#!/bin/bash
set -e
BACKUP_DIR=/opt/backup/zmail
DATE=$(date +%F)
mkdir -p "$BACKUP_DIR"

# SQLite 在线备份（需要 sqlite3）
sqlite3 /var/lib/zmail/zmail.db ".backup '$BACKUP_DIR/zmail-$DATE.db'"

# 原始邮件与附件
tar -czf "$BACKUP_DIR/raw-$DATE.tar.gz" -C /var/lib/zmail raw

# 只保留 14 天
find "$BACKUP_DIR" -type f -mtime +14 -delete
```

### 7.2 Docker 部署

```bash
#!/bin/bash
set -e
BACKUP_DIR=/opt/backup/zmail
DATE=$(date +%F)
mkdir -p "$BACKUP_DIR"

docker compose -f /opt/zmail/docker/docker-compose.yml stop zmail
tar -czf "$BACKUP_DIR/zmail-data-$DATE.tar.gz" -C /opt/zmail/docker data
docker compose -f /opt/zmail/docker/docker-compose.yml start zmail

find "$BACKUP_DIR" -type f -mtime +14 -delete
```

> SQLite 使用 WAL，冷备前应停止服务；在线备份请使用 `sqlite3 .backup`。恢复时停止服务，替换 `data/` 或数据库与 raw 目录后重启。

---

## 8. 升级

### 8.1 systemd 升级

```bash
make build-linux
# 上传 dist/zmail-linux-amd64 到 /opt/zmail/zmail.new

chmod 0755 /opt/zmail/zmail.new
mv /opt/zmail/zmail /opt/zmail/zmail.bak
mv /opt/zmail/zmail.new /opt/zmail/zmail
systemctl restart zmail
curl -s http://127.0.0.1:7080/healthz
```

### 8.2 Docker 升级

```bash
cd /opt/zmail/docker
# 替换 zmail-linux-amd64 或修改 Dockerfile 使用的二进制
docker build -f Dockerfile -t zmail:latest .
docker compose up -d --force-recreate
```

页面静态资源带构建指纹，正常刷新浏览器即可获取新版本。

---

## 9. 健康检查与排障

健康检查：

```bash
curl -s http://127.0.0.1:7080/healthz
# {"status":"ok","time":"..."}
```

| 现象 | 排查 |
|---|---|
| 502 / Bad Gateway | `systemctl status zmail` 或 `docker compose ps`；确认反代目标是 `127.0.0.1:7080` |
| 登录成功但立刻退出 | `PUBLIC_URL` 与实际访问的协议+域名不一致；检查反代是否传 `X-Forwarded-Proto` |
| 新邮件不实时 | 反代缓冲 SSE；确认 `/api/events` 已关闭 `proxy_buffering` 和 `gzip` |
| 发信/附件 413 | Nginx `client_max_body_size` 调到 `25m` |
| 添加账号连接失败 | 自建内网 IMAP 需 `ALLOW_PRIVATE_IMAP=true`；QQ/163 需使用授权码；检查出站 993/465 |
| `database is locked` | 确认只有一个实例；数据目录本地可写；不要把 SQLite 放 NFS |
| Docker 写入失败 | `chown -R 100:101 /opt/zmail/docker/data` |
| 升级后样式未更新 | 正常刷新；必要时 Ctrl+F5 |
| 内存占用高 | 降低 `MAX_FETCH_CONCURRENCY`；确认没有重复启动 |

日志：

```bash
journalctl -u zmail -f          # systemd
docker compose logs -f zmail    # Docker
```

---

## 10. 安全清单

- [ ] 公网只开放 80/443，不直接暴露 7080
- [ ] `PUBLIC_URL` 使用实际 HTTPS 地址
- [ ] `APP_PASSWORD` 使用强密码
- [ ] `SESSION_KEY` 与 `CREDENTIALS_KEY` 独立随机生成并妥善备份
- [ ] `.env` 权限设为 600 或 640，不提交到 Git
- [ ] `TRUSTED_PROXY_CIDRS` 仅包含可信反代网段，禁止 `0.0.0.0/0`
- [ ] 定期备份数据库与 raw 目录
- [ ] 服务器出站仅按需开放 993/465/587