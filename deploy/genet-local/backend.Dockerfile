# build context 必须是仓根（不能是 backend/），因为 backend 依赖 shared/ module。
#FROM golang:1.21-alpine AS go-deps

# ============================================================================
# ⚠️ 本地联调 patch（TestPlane infra，2026-11-05）——**仅联调用，不提交回 Genet**
#
# 上游 Dockerfile 依赖华为内网：基础镜像来自 registry.dev.huawei.com、
# apk 源指向 mirrors.tools.huawei.com、GOPROXY 也走内网。本地 kind 拉不到这些，
# 因此做最小替换（4 处）：
#   1) 构建基础镜像 registry.dev.huawei.com/flash_stor/golang:1.21-alpine → golang:1.21-alpine
#   2) 删掉 apk 源改写（用默认 dl-cdn.alpinelinux.org）
#   3) GOPROXY → https://goproxy.cn,direct（可达）
#   4) 运行基础镜像 registry.dev.huawei.com/flash_stor/alpine:latest → alpine:3.20
# 其余（构建命令、二进制清单、CMD）与上游逐字一致。
# ============================================================================
FROM golang:1.21-alpine AS go-deps

WORKDIR /src

# 本地：不改 apk 源

# 安装依赖
RUN apk add --no-cache git

ENV GO111MODULE=on
ENV GOPROXY=https://goproxy.cn,direct
ENV GONOSUMDB=*

# 复制 go.mod / go.sum 以及 shared module（供 replace 引用），先下载依赖
COPY backend/go.mod backend/go.sum ./backend/
COPY shared/ ./shared/
WORKDIR /src/backend
RUN go mod download

FROM go-deps AS backend-builder

# 复制后端完整源代码
COPY backend/ /src/backend/
WORKDIR /src/backend

# 构建 API 服务器
RUN CGO_ENABLED=0 GOOS=linux go build -o api-server ./cmd/api

# 构建 Helm 固定 post-render 安全过滤器（Helm 实际 apply 的字节必须经过它）
RUN CGO_ENABLED=0 GOOS=linux go build -o genet-helm-post-renderer ./cmd/helm-post-renderer

# 构建持久终端网关
RUN CGO_ENABLED=0 GOOS=linux go build -o webshell-gateway ./cmd/webshell-gateway

# 构建 Volcano scheduler 日志诊断进程
RUN CGO_ENABLED=0 GOOS=linux go build -o volcano-diagnoser ./cmd/volcano-diagnoser

# 构建清理任务
RUN CGO_ENABLED=0 GOOS=linux go build -o cleanup ./cmd/cleanup

# 构建节点镜像清理任务
RUN CGO_ENABLED=0 GOOS=linux go build -o image-cleanup ./cmd/image-cleanup

# 运行时镜像
#FROM alpine:latest
# 本地 patch：registry.dev.huawei.com/flash_stor/alpine:latest → alpine:3.20
FROM alpine:3.20
# 本地：不改 apk 源
# ca-certificates / tzdata 支撑 TLS 与时区；helm 供 Helm 管理功能调用（若关闭该特性可以移除）
RUN apk --no-cache add ca-certificates tzdata helm
ENV PATH="/usr/local/bin:${PATH}"

WORKDIR /app

# 复制二进制文件
COPY --from=backend-builder /src/backend/api-server /app/
COPY --from=backend-builder /src/backend/genet-helm-post-renderer /usr/local/bin/
COPY --from=backend-builder /src/backend/webshell-gateway /app/
COPY --from=backend-builder /src/backend/volcano-diagnoser /app/
COPY --from=backend-builder /src/backend/cleanup /app/
COPY --from=backend-builder /src/backend/image-cleanup /app/

EXPOSE 8080

# 默认运行 API 服务器
CMD ["/app/api-server"]
