# zMail 运行镜像（配合 1Panel「容器」/「编排」使用）
# 先把 Linux 二进制与 Dockerfile 放到同一目录，再执行：
#   docker build -t zmail:latest .
# 二进制来自项目根目录的 make build-linux 产物。
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S zmail && adduser -S -G zmail -h /data zmail

WORKDIR /app
COPY zmail-linux-amd64 /app/zmail
RUN chmod 0755 /app/zmail

# 数据目录：SQLite 与原始邮件/附件都放在这里，务必挂载持久化卷
RUN mkdir -p /data && chown -R zmail:zmail /data /app
USER zmail

ENV LISTEN_ADDR=0.0.0.0:7080 \
    DB_PATH=/data/zmail.db \
    RAW_DIR=/data/raw \
    LOG_LEVEL=info

EXPOSE 7080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O - http://127.0.0.1:7080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/zmail"]
