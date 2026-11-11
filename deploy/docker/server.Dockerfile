# Argus · 控制面镜像（argus-server）
#
# 构建上下文 = **仓库根**（要用 server/ 与 runners/ 两个目录，W-8）：
#   docker build -f deploy/docker/server.Dockerfile -t argus-server:dev .
#   & .\.tools\kind.exe load docker-image argus-server:dev --name argus
#
# 依据
#   design/impl/web-and-delivery.md §7（构建与集成）、§8（镜像与 CI）
#   design/impl/execution.md §10（Runner Registry：镜像内 /app/runners）
#
# 与 W-8 的差异（有意的，写在这里免得被当成漏项）
#   1) W-8 的镜像还有 node:22-alpine 前端阶段 + distroless 运行阶段；
#      当前仓库还没有 web/ 产物，也没有 server/cmd/argusd，所以这里先给
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
#     由 `argusd migrate up` 单独执行（Helm pre-upgrade hook）。
#   - runners/ 逐个 COPY 到 /app/runners，注册表启动时扫描一次（K-3、E-10）。

# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25
ARG RUNTIME_BASE=alpine:3.20

# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# 前端阶段（W-8 / W-7）：构建 web/，把 dist 拷进 server 的 go:embed 目录。
#   - 二进制内嵌前端 → Server 单端口同时服务静态资源与 /api/v1；
#   - `pnpm build` = `tsc -b && vite build`，类型错误会让构建失败（前端也有门禁）；
#   - 只把 package.json/lock 先拷进来装依赖，改源码不重装（层缓存）。
# ---------------------------------------------------------------------------
# 这个阶段产出的是 **与 CPU 架构无关** 的静态资源，所以钉在构建机平台（$BUILDPLATFORM）上跑：
# 多架构构建时不必为 arm64 再 qemu 慢跑一遍 node/vite（实测省掉 1~2 分钟）。
FROM --platform=$BUILDPLATFORM node:22-alpine AS web

RUN corepack enable

# pnpm 12 默认开 supply-chain 策略（minimumReleaseAge）：锁文件里"太新"的包会被判红，
# 且 `pnpm run` 会在怀疑依赖陈旧时**自动重装**——构建要离线/可复现，所以显式关掉。
ENV NPM_CONFIG_MINIMUM_RELEASE_AGE=0

WORKDIR /src/web

# 前端构建前要跑 scripts/sync-spec.mjs 把契约投影到 src/generated/。
# 构建上下文是**仓库根**（下面 COPY web/ 就是证据），所以这里能把契约拷进 web 的工作目录；
# 只 COPY web/ 的话容器里没有 ../server/api/，prebuild 会直接失败（踩过）。
COPY server/api/openapi.yaml ./server-api/openapi.yaml

COPY web/package.json web/pnpm-lock.yaml* ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile --config.minimumReleaseAge=0 --allow-build=esbuild

COPY web/ ./
RUN pnpm --config.verifyDepsBeforeRun=false --config.minimumReleaseAge=0 run build


# 构建阶段
# ---------------------------------------------------------------------------
# 交叉编译：构建阶段钉在构建机平台（$BUILDPLATFORM），用 GOARCH=$TARGETARCH 产出目标架构二进制。
# 好处：多架构时 amd64/arm64 **共用同一个 build stage**，不必在 qemu 里跑 arm64 的 Go 工具链
# （实测 361s → ~60s）。CGO_ENABLED=0，产物是静态二进制，交叉编译没有 libc 兼容问题
# （构建末尾的 `! ldd` 与 ELF e_machine 自检会守住这一点）。
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

# buildx 提供的目标平台参数。
#
# ⚠️ **不要给它们写默认值**（`ARG TARGETARCH=amd64` 这种）：默认值会**盖掉** buildx 注入的值，
# 结果 arm64 那一份也会编出 amd64 二进制（实测踩过：镜像层 digest 不同、ELF 却都是 `3e 00`）。
# 普通 `docker build`（非 buildx）时这两个变量为空，所以下面 RUN 里用 `${TARGETARCH:-amd64}` 兜底。
ARG TARGETOS
ARG TARGETARCH

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

# 前端产物：go:embed 不能跨模块目录，必须复制进 server 模块内（缺 dist 会编译失败）
COPY --from=web /src/web/dist ./internal/webui/dist

ARG VERSION=dev
ARG COMMIT=unknown
ENV CGO_ENABLED=0

# -trimpath：去掉本机路径，镜像可复现
# -s -w：去符号表，二进制小一半
# 产物名固定 argusd（与 06 §2 的 Deployment 名、`/argusd migrate up` 一致）
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/argusd ./cmd/argusd

# 构建期自检：
#   1) 二进制存在，且**不是**动态链接（CGO_ENABLED=0 的实证，不是"应该"）；
#      用 ldd 而不是 file：golang:*-alpine 里没有 file，ldd 是 musl 自带的。
#      静态二进制 `ldd` 会失败 → `! ldd` 成立；若哪天有人加了 CGO 依赖，这一步会让构建失败。
#   2) **架构 == TARGETARCH**：读 ELF 头 e_machine（偏移 18-19，0x3e=x86-64，0xb7=AArch64）。
#      交叉编译最大的坑就是"忘了传 GOARCH"，结果 arm64 镜像里塞了 amd64 二进制；
#      这一步把那种错误变成**构建失败**，而不是等运行期 exec format error。
RUN test -x /out/argusd && \
    ls -lh /out/argusd && \
    ! ldd /out/argusd 2>/dev/null && \
    echo "BUILD_CHECK_OK: static binary" && \
    elf_machine=$(od -An -tx1 -j18 -N2 /out/argusd | tr -d ' \n') && \
    case "${TARGETARCH:-amd64}:${elf_machine}" in \
      amd64:3e00) echo "BUILD_CHECK_OK: elf x86-64 (amd64)" ;; \
      arm64:b700) echo "BUILD_CHECK_OK: elf aarch64 (arm64)" ;; \
      arm:2800)   echo "BUILD_CHECK_OK: elf arm32 (arm)" ;; \
      *)          echo "BUILD_CHECK_FAIL: TARGETARCH=${TARGETARCH:-amd64}, elf e_machine=${elf_machine}" >&2; exit 1 ;; \
    esac

# ---------------------------------------------------------------------------
# 运行阶段
# ---------------------------------------------------------------------------
FROM ${RUNTIME_BASE}

# ca-certificates：出站 HTTPS（OIDC / Genet / K8s API）
# tzdata：TZ=Asia/Shanghai 要真实时区数据；日志时间戳仍以 apiserver/DB 为准（06 §7.1）
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 argus \
 && adduser  -D -u 10001 -G argus -s /sbin/nologin argus \
 && mkdir -p /app/runners \
 && chown -R 10001:10001 /app

COPY --from=build /out/argusd /argusd

# Runner Registry 的扫描源（E-10、K-3）。只用 COPY，不做运行时同步。
COPY runners/ /app/runners/
# chown 必须放在 COPY 之后：COPY 以 root 落盘，前面那次 chown 覆盖不到它。
# 注册表只读这些文件，但属主统一到 10001，避免以后有人加写操作踩权限。
RUN chown -R 10001:10001 /app/runners

ENV TZ=Asia/Shanghai \
    AG_LISTEN=:8080 \
    AG_STORAGE_ROOT=/storage-root \
    AG_TESTERS_DIR=/app/runners

USER 10001:10001

EXPOSE 8080

# 不用 shell 形式：ENTRYPOINT/CMD 分离，`docker run <img> migrate up` 能追加子命令
ENTRYPOINT ["/argusd"]
CMD ["--role=all"]

# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# 附：前端内嵌（已启用）
# ---------------------------------------------------------------------------
# 上面的 `web` 阶段即 W-8 的前端阶段：`pnpm build`（= tsc -b && vite build）→ 产物拷进
# build 阶段的 `internal/webui/dist`，由 `//go:embed all:dist` 内嵌进二进制，
# 于是 Server 单端口同时服务静态资源与 /api/v1（W-7）。
#
# 两个已知坑（都已在上面处理）：
#   1) go:embed 不能跨模块目录 —— 必须 `COPY --from=web /src/web/dist ./internal/webui/dist`，
#      缺 dist 会**编译失败**（不是运行期 404）。
#   2) pnpm 12 的 supply-chain 策略（minimumReleaseAge）会让"较新"的依赖判红，且 `pnpm run`
#      在怀疑依赖陈旧时会自动重装 —— 已用 `NPM_CONFIG_MINIMUM_RELEASE_AGE=0` 与
#      `--config.verifyDepsBeforeRun=false` 关掉，保证镜像构建可复现（锁文件就是构建输入）。
