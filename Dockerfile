# AetherLink 以太链接 - x86_64 Docker 镜像
# 前端与后端在同一次构建中完成，产物是单个静态二进制。

FROM node:22-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.24-alpine AS backend
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# go:embed 需要在编译前就看到编译好的前端产物。
COPY --from=frontend /src/internal/web/dist ./internal/web/dist
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/aetherlink ./cmd/aetherlink

FROM alpine:3.21
# su-exec 用于入口脚本修正 /config 属主后降权，比 gosu 小得多。
RUN apk add --no-cache ca-certificates tzdata wget su-exec && \
    addgroup -g 10001 aetherlink && \
    adduser -D -H -u 10001 -G aetherlink aetherlink && \
    mkdir -p /config && chown 10001:10001 /config
COPY --from=backend /out/aetherlink /aetherlink
COPY deploy/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# 仅作参考文档；运行时的配置由程序自己在 /config 下创建和维护。
COPY deploy/config.example.yaml /defaults/config.example.yaml
ENV AETHERLINK_CONFIG=/config/config.yaml \
    TZ=Asia/Shanghai \
    PUID=10001 \
    PGID=10001
EXPOSE 5151
VOLUME ["/config"]
# 健康检查用免鉴权的存活探针，不会暴露任何配置。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:5151/aetherlink/api/health >/dev/null || exit 1
# 以 root 进入 entrypoint，脚本会把 /config 的属主改成 PUID:PGID 后再降权。
# 需要全程非 root 时在 compose 里加 user:，脚本会自动跳过 chown。
ENTRYPOINT ["/entrypoint.sh"]