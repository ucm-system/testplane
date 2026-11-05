# TestPlane · 前端独立镜像（testplane-web）
#
# 用途：**只给"前端单独发版 / 单独托管"的场景**。默认部署仍用 server 内嵌前端
# （server.Dockerfile 的 web 阶段 → go:embed → 单端口同时服务静态资源与 /api/v1），
# 不要因为有了本文件就把默认部署改成双镜像。
#
# 构建上下文 = **仓库根**（要 web/ 整个目录）：
#   docker build -f deploy/docker/web.Dockerfile -t testplane-web:dev .
#   或：pwsh deploy/docker/build-push.ps1 -Component web
#
# 运行时约定：
#   - 只读根可跑：nginx 的临时目录/缓存/pid 全部落在可写的空挂点上；
#   - 非 root（10001:10001）；
#   - `/api/` **可选**反代：设了 `TESTPLANE_API_UPSTREAM` 才启用（envsubst 渲染模板）；
#     没设就纯静态（只服务 SPA，API 由别处托管）；
#   - SSE 友好：`proxy_buffering off`、长连接超时放宽；
#   - `/healthz` 固定 200，供 k8s 探针。

# ---------------------------------------------------------------------------
# 前端构建阶段（与 server.Dockerfile 的 web 阶段保持同一套 pnpm 修正）
# ---------------------------------------------------------------------------
FROM node:22-alpine AS web

RUN corepack enable

# pnpm 12 默认开 supply-chain 策略（minimumReleaseAge）：锁文件里"太新"的包会被判红，
# 且 `pnpm run` 会在怀疑依赖陈旧时**自动重装**——构建要离线/可复现，所以显式关掉。
ENV NPM_CONFIG_MINIMUM_RELEASE_AGE=0

WORKDIR /src/web

COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml* ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile --config.minimumReleaseAge=0 --allow-build=esbuild

COPY web/ ./
# `pnpm build` = `tsc -b && vite build`：类型错误会让镜像构建失败（前端也有门禁）。
RUN pnpm --config.verifyDepsBeforeRun=false --config.minimumReleaseAge=0 run build

# ---------------------------------------------------------------------------
# 运行阶段：nginx（非 root + 只读根）
# ---------------------------------------------------------------------------
FROM nginx:1.27-alpine

# 非 root 的 nginx 需要：
#   - 自己的 pid 文件位置（不能写 /run）
#   - 临时目录指向可写空挂
#   - 监听 8080（<1024 需要额外 capability，我们不用）
# 这里不用 nginx 官方的 101 用户，而是固定 10001（与本平台其他镜像一致，
# 便于 fsGroup 与 PVC 属主统一）。
RUN set -eux; \
    addgroup -g 10001 testplane; \
    adduser -D -u 10001 -G testplane -H -s /sbin/nologin testplane; \
    mkdir -p /etc/nginx/templates /var/cache/nginx /var/run /tmp; \
    chown -R 10001:10001 /var/cache/nginx /var/run /tmp /etc/nginx; \
    # 官方镜像默认监听 80 并在 /etc/nginx/conf.d/default.conf；我们用自己的模板，
    # 保留默认 conf.d 目录但不放 default.conf（模板渲染到 default.conf）。
    rm -f /etc/nginx/conf.d/default.conf

# SPA 静态资源 + 可选 /api 反代模板（由入口脚本 envsubst 渲染）
COPY web/nginx.conf /etc/nginx/templates/default.conf.template
COPY deploy/docker/web-entrypoint.sh /usr/local/bin/web-entrypoint.sh
RUN chmod 0755 /usr/local/bin/web-entrypoint.sh

# 产物放在 nginx 默认 root（read-only 根不影响：静态资源是镜像层，只读可读）
COPY --from=web /src/web/dist /usr/share/nginx/html

USER 10001:10001

ENV TZ=Asia/Shanghai \
    TESTPLANE_API_UPSTREAM=""

EXPOSE 8080

# 健康检查（k8s 探针另有 liveness/readiness，这里给 docker run 用）
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/web-entrypoint.sh"]
