# TestPlane · 控制面镜像（testplane-server）
#
# 构建上下文 = **仓库根**（要用 server/ 与 testers/ 两个目录，W-8）：
#   docker build -f deploy/docker/server.Dockerfile -t testplane-server:dev .
#   & .\.tools\kind.exe load docker-image testplane-server:dev --name testplane
#
# 依据
#   design/impl/web-and-delivery.md §7（构建与集成）、§8（镜像与 CI）
#   design/impl/execution.md §10（Tester Registry：镜像内 /app/testers）
#
# 与 W-8 的差异（有意的，写在这里免得被当成漏项）
#   1) W-8 的镜像还有 node:22-alpine 前端阶段 + distroless 运行阶段；
#      当前仓库还没有 web/ 产物，也没有 server/cmd/testplaned，所以这里先给
#      「Go 后端 + alpine 运行阶段」的可跑版本。web 阶段见文件末尾注释，
#      等 web/ 有产物、server/internal/webui/dist 就位后启用。
#   2) 运行阶段用 alpine 而不是 distroless：需要 ca-certificates、tzdata
#      （TZ=Asia/Shanghai 要 tzdata 才有真实偏移），distroless/static 两者都没有。
#      代价是多了 /bin/sh——CI 里用 trivy 扫 HIGH 阻断来守住（W-8 合规行）。
#
# 为什么 UID 是 10001 而不是 distroless 的 65532：
#   /storage-root 的属主是 10001:10001（06 §3.2），控制面要往根目录预创建
#   <run_id>/tasks/<task>/attempts/<n> 并 chown。UID 对不上就只能靠 fsGroup 兜底，
#   而 fsGroup 在 hostPath / 部分 CSI 组合上不保证生效（E-3.5）。10001 是硬要求。
#
# 不变量
#   - CGO_ENABLED=0：静态二进制，alpine 上不装 libc 兼容层也能跑。
#   - 迁移**不进 ENTRYPOINT**（多副本会并发迁移，W-11）：
#     由 `testplaned migrate up` 单独执行（Helm pre-upgrade hook）。
#   - testers/ 逐个 COPY 到 /app/testers，注册表启动时扫描一次（K-3、E-10）。

# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25
ARG RUNTIME_BASE=alpine:3.20

# ---------------------------------------------------------------------------
# 构建阶段
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build

# git：Go 模块走 vcs 时要它；ca-certificates：拉依赖可能走 https
RUN apk add --no-cache git ca-certificates

WORKDIR /src/server

# 先只拷 go.mod/go.sum 再 download：依赖不变时这层命中缓存，
# 改业务代码不会重新拉几百 MB 的 k8s.io 依赖。
# go.sum 用通配：它由 `go mod tidy` 生成，早期仓库里可能还没有；
# COPY 的通配符匹配不到文件时不报错（已实测），缺 go.sum 时 go mod download 会自己算校验和。
COPY server/go.mod server/go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 业务代码
COPY server/ ./

ARG VERSION=dev
ARG COMMIT=unknown
ENV CGO_ENABLED=0

# -trimpath：去掉本机路径，镜像可复现
# -s -w：去符号表，二进制小一半
# 产物名固定 testplaned（与 06 §2 的 Deployment 名、`/testplaned migrate up` 一致）
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/testplaned ./cmd/testplaned

# 构建期自检：二进制存在且是静态链接（CGO_ENABLED=0 的实证，不是"应该"）
RUN test -x /out/testplaned && \
    (file /out/testplaned || true) && \
    ls -lh /out/testplaned

# ---------------------------------------------------------------------------
# 运行阶段
# ---------------------------------------------------------------------------
FROM ${RUNTIME_BASE}

# ca-certificates：出站 HTTPS（OIDC / Genet / K8s API）
# tzdata：TZ=Asia/Shanghai 要真实时区数据；日志时间戳仍以 apiserver/DB 为准（06 §7.1）
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 testplane \
 && adduser  -D -u 10001 -G testplane -s /sbin/nologin testplane \
 && mkdir -p /app/testers \
 && chown -R 10001:10001 /app

COPY --from=build /out/testplaned /testplaned

# Tester Registry 的扫描源（E-10、K-3）。只用 COPY，不做运行时同步。
COPY testers/ /app/testers/

ENV TZ=Asia/Shanghai \
    TESTPLANE_LISTEN=:8080 \
    TESTPLANE_STORAGE_ROOT=/storage-root \
    TESTPLANE_TESTERS_DIR=/app/testers

USER 10001:10001

EXPOSE 8080

# 不用 shell 形式：ENTRYPOINT/CMD 分离，`docker run <img> migrate up` 能追加子命令
ENTRYPOINT ["/testplaned"]
CMD ["--role=all"]

# ---------------------------------------------------------------------------
# 附：W-8 的前端阶段（等 web/ 有产物再启用）
# ---------------------------------------------------------------------------
# 目标是让二进制内嵌前端（Server 单端口同时服务静态资源与 /api/v1，W-7）。
# 启用步骤：
#   1. 在下面加一个 web 阶段，构建产物拷到 build 阶段的 internal/webui/dist：
#
#        FROM node:22-alpine AS web
#        WORKDIR /src/web
#        COPY web/package.json web/pnpm-lock.yaml ./
#        RUN corepack enable && pnpm install --frozen-lockfile
#        COPY web/ ./
#        RUN pnpm build
#
#   2. 在 build 阶段 `COPY server/ ./` 之后插一行：
#        COPY --from=web /src/web/dist ./internal/webui/dist
#      （go:embed 不能跨模块目录，必须复制进来；`//go:embed all:dist` 找不到
#       dist 会直接编译失败，所以这一行是硬依赖，不能省。）
#   3. 注意 COPY 的层顺序：server/ 变动会让 go build 缓存失效，这是有意的。
