# zMail 反向代理配置指南

zMail 只监听本机回环地址（默认 `LISTEN_ADDR=127.0.0.1:7080`），TLS 与公网入口交给外部反向代理。
本文给出 Nginx / Caddy / Traefik 的可用配置，以及 SSE、超时、请求体、真实 IP、TLS 与 Cloudflare 的注意事项。

> 关键前提：zMail 是单用户应用，页面、API、静态资源和 SSE 全部同源，不需要 CORS，也不允许跨站写请求。
> 写操作会校验 `Origin` 与 `PUBLIC_URL` 一致，反代必须保持 `Host` 与 `Origin` 原样传递。

## 1. 最小要求清单

| 项目 | 要求 |
|---|---|
| TLS | 在反代终止，`PUBLIC_URL` 写 `https://` 完整地址（含子路径时也必须一致） |
| Host | 保留原始 `Host`，不要改写成 `127.0.0.1:7080` |
| SSE | `/api/events` 必须关闭响应缓冲，读超时 ≥ 1 小时 |
| 超时 | 普通 API 建议 60s；附件上传/下载 300s；SSE 3600s |
| 请求体 | `client_max_body_size` 至少 25m（按常用附件上限，可调到 50m） |
| 真实 IP | 只信任自己这一层代理，`TRUSTED_PROXY_CIDRS` 配成反代所在网段 |
| 压缩 | `/api/events` 禁用 gzip；静态资源开启 gzip/brotli |
| 缓存 | `/api/*` 一律 `no-store`；`/static/*` 可长期缓存（建议内容哈希或短 TTL） |
| HTTP/2 | 建议开启；SSE 在 HTTP/1.1 下也可用，但每域名 6 连接上限更易被占满 |

## 2. Nginx

```nginx
# /etc/nginx/conf.d/zmail.conf
upstream zmail_backend {
    server 127.0.0.1:7080;
    keepalive 16;
}

server {
    listen 80;
    server_name mail.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    http2 on;
    server_name mail.example.com;

    ssl_certificate     /etc/letsencrypt/live/mail.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mail.example.com/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:SSL:10m;

    # 附件上传上限；按需调整
    client_max_body_size 25m;
    client_body_timeout 60s;

    # 安全响应头（页面本身也会设置 CSP，此处兜底）
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header X-Content-Type-Options "nosniff" always;
    # 不要用 no-referrer：它会让 Chrome 把表单 POST 的 Origin 降级为 "null"，触发同源校验误判
    add_header Referrer-Policy "same-origin" always;
    add_header X-Frame-Options "DENY" always;

    # 静态资源：由 Go 内嵌提供，可缓存
    location /static/ {
        proxy_pass http://zmail_backend;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header Connection "";
        expires 7d;
        add_header Cache-Control "public, max-age=604800";
        gzip on;
        gzip_types text/css application/javascript application/manifest+json image/svg+xml;
    }

    # SSE：关闭一切缓冲与压缩，长读超时
    location = /api/events {
        proxy_pass http://zmail_backend;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Connection "";
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;

        proxy_buffering off;
        proxy_cache off;
        proxy_request_buffering off;
        gzip off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        chunked_transfer_encoding on;
        add_header X-Accel-Buffering "no" always;
        add_header Cache-Control "no-cache, no-transform" always;
    }

    # 附件下载：大文件、长超时
    location ^~ /api/attachments/ {
        proxy_pass http://zmail_backend;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;
        proxy_buffering on;
    }

    # 其余请求
    location / {
        proxy_pass http://zmail_backend;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header Connection "";
        proxy_read_timeout 60s;
        proxy_send_timeout 60s;
    }
}
```

要点：

- `proxy_buffering off` 是 SSE 能否实时到达的关键；只加 `X-Accel-Buffering: no` 也有效，但两者都加最稳。
- `gzip off` 避免压缩缓冲把事件攒成一批。
- 不要把 `/api/events` 放进带 `proxy_cache` 的 location，也不要在 CDN 侧缓存它。
- 若使用 `proxy_pass http://127.0.0.1:7080;` 直连（无 upstream），同样需要上述头部。

## 3. Caddy

Caddy 2 默认对响应做流式转发，SSE 需要显式设置 `flush_interval -1`。

```caddyfile
mail.example.com {
    encode zstd gzip

    # SSE 单独处理：不压缩、不缓冲
    @events path /api/events
    handle @events {
        reverse_proxy 127.0.0.1:7080 {
            flush_interval -1
            transport http {
                read_timeout 1h
                write_timeout 1h
            }
            header_up Host {host}
            header_up X-Forwarded-Proto {scheme}
            header_up X-Forwarded-Host {host}
            header_up X-Forwarded-For {remote_host}
        }
    }

    @static path /static/*
    header @static Cache-Control "public, max-age=604800"
    header @static -Server

    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options "nosniff"
        # 不要用 no-referrer（会破坏表单 POST 的 Origin 头，导致登录 403）
        Referrer-Policy "same-origin"
        X-Frame-Options "DENY"
        -Server
    }

    # 请求体上限 25MB
    request_body {
        max_size 25MB
    }

    reverse_proxy 127.0.0.1:7080 {
        header_up Host {host}
        header_up X-Forwarded-Proto {scheme}
        header_up X-Forwarded-Host {host}
        header_up X-Forwarded-For {remote_host}
        transport http {
            read_timeout 60s
            write_timeout 60s
            dial_timeout 5s
        }
    }
}
```

要点：

- 没有 `flush_interval -1` 时 Caddy 会缓冲响应，SSE 事件会延迟或成批到达。
- Caddy 自动申请证书；`PUBLIC_URL=https://mail.example.com` 即可。
- `request_body max_size` 对发信附件生效，超限返回 413。

## 4. Traefik

静态配置（traefik.yml）中为 SSE 服务设置 `flushInterval: -1`：

```yaml
entryPoints:
  websecure:
    address: ":443"

providers:
  file:
    filename: /etc/traefik/dynamic.yml
```

动态配置（dynamic.yml）：

```yaml
http:
  routers:
    zmail:
      rule: "Host(`mail.example.com`)"
      entryPoints: [websecure]
      tls:
        certResolver: le
      service: zmail
    zmail-events:
      rule: "Host(`mail.example.com`) && Path(`/api/events`)"
      entryPoints: [websecure]
      tls:
        certResolver: le
      service: zmail-sse
      middlewares: [security-headers]

  services:
    zmail:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:7080"
        passHostHeader: true
        responseForwarding:
          flushInterval: 100ms
    zmail-sse:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:7080"
        passHostHeader: true
        responseForwarding:
          flushInterval: -1

  middlewares:
    security-headers:
      headers:
        stsSeconds: 31536000
        contentTypeNosniff: true
        referrerPolicy: no-referrer
        frameDeny: true
    body-limit:
      buffering:
        maxRequestBodyBytes: 26214400
    compress:
      compress: {}
```

Docker 标签方式时，SSE 的 flushInterval 仍需在文件 provider 里配置（标签目前无法直接表达）。

要点：

- 普通服务 `flushInterval` 可用 100ms，SSE 必须 `-1`。
- 为普通路由加上 `body-limit`、`compress` 中间件；不要给 SSE 路由加 compress。
- `passHostHeader: true` 保证 `PUBLIC_URL` 的 Origin 校验通过。

## 5. Cloudflare 注意事项

- **橙色云代理会缓冲与改写响应**：SSE 可以工作，但必须让源站每 20 秒内至少发送一次心跳（zMail 默认 `: ping`），否则 Cloudflare 会因空闲超时断开（通常 100 秒左右）。
- 确保 `/api/events` 不命中任何 Cache Rule；建议为 `/api/*` 建规则：Cache Eligibility = Bypass。
- 关闭 Rocket Loader、Email Address Obfuscation、Automatic HTTPS Rewrites 对 `/static/app.js` 的改写，避免脚本被注入或延迟执行。
- 免费版对单请求有 100 秒无数据超时；如需长时间空闲连接，改用 Cloudflare Tunnel 并让隧道直连源站，或降低心跳间隔（zMail 心跳 20s 已满足）。
- 开启 `Under Attack` 模式时 SSE 可能被质询页打断，建议对 `/api/events` 放行。
- **真实 IP**：若前面是 Cloudflare，`X-Forwarded-For` 由 Cloudflare 注入且可被伪造。zMail 只在 `TRUSTED_PROXY_CIDRS` 命中时才信任 `X-Forwarded-*`，所以要么把 Cloudflare 网段加入该变量，要么在 Nginx 用 `ngx_http_realip_module` 配合可信 IP 段还原后再转发。
- 不要把 `SESSION_KEY`、`CREDENTIALS_KEY` 或登录页放进 Cloudflare Access 之外的公开缓存。

## 6. 真实 IP 与 TRUSTED_PROXY_CIDRS

zMail 默认只信任 `127.0.0.1/32,::1/128` 发来的 `X-Forwarded-For` / `X-Forwarded-Proto`。
反代与 zMail 同机时保持默认即可；反代在 Docker 网络或另一台机器时，必须改成对应网段：

```bash
# 例：Docker bridge 网段
TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128,172.17.0.0/16
```

否则 `Origin` 校验、Secure Cookie 判断、登录审计日志里的 IP 都会出错。
**永远不要把 `0.0.0.0/0` 写进这个变量**，那等于允许任何人伪造来源。

## 7. 环境变量与反代的关系

| 变量 | 与反代相关的作用 |
|---|---|
| `PUBLIC_URL` | 书写完整外部地址（含协议、域名、可选子路径），用于 Origin/CSRF 校验与 Cookie Secure |
| `LISTEN_ADDR` | 建议保持 `127.0.0.1:7080`，只让反代访问 |
| `TRUSTED_PROXY_CIDRS` | 决定是否信任 `X-Forwarded-*` |
| `APP_PASSWORD` | 单用户登录密码；反代层不要再加 Basic Auth，避免双重认证 |
| `LOG_LEVEL` | 排查 SSE/反代问题时设为 `debug` |

子路径部署示例：`PUBLIC_URL=https://example.com/zmail` 时，反代需把 `/zmail/` 原样转发给 zMail（不要剥掉前缀），zMail 会按 `PublicPath()` 生成链接与静态资源地址。

## 8. 验证清单

```bash
# 1. SSE 是否不缓冲：10 秒内应看到至少一条 ": ping" 或事件
curl -N -H "Cookie: zmail_session=..." https://mail.example.com/api/events

# 2. 响应头是否带 no-transform 与 no-cache
curl -sI https://mail.example.com/api/events | grep -iE "cache-control|x-accel"

# 3. 大附件上传是否被 413
curl -s -o /dev/null -w "%{http_code}\n" -F "file=@big.bin" https://mail.example.com/api/attachments

# 4. 静态资源是否可缓存且无 CDN 依赖
curl -sI https://mail.example.com/static/app.css | grep -i cache-control
```

排查顺序：浏览器 Network 里 `/api/events` 是否 pending 且持续收到数据 → 反代是否关闭 buffering → 是否命中 CDN 缓存 → 防火墙空闲超时。

## 9. 常见坑

| 现象 | 原因 | 处理 |
|---|---|---|
| 新邮件要等很久才出现 | SSE 被 Nginx/Caddy 缓冲 | 关闭 proxy_buffering / flush_interval -1 |
| 每 60 秒断一次 SSE | 反代读超时过短 | `proxy_read_timeout 3600s` |
| 登录后立刻掉线 | Origin 校验失败或 Cookie 非 Secure | 检查 `PUBLIC_URL` 协议与 `Host` 透传 |
| 发信附件 413 | 请求体上限太小 | 调大 `client_max_body_size` / `max_size` |
| gzip 后 SSE 乱码 | SSE 被压缩 | `/api/events` 关闭 gzip/compress |
| 日志里 IP 全是 127.0.0.1 | 未配置 `TRUSTED_PROXY_CIDRS` | 加入反代网段 |
