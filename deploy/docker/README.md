# 镜像：构建与发布

一条命令完成「构建 → 打两个 tag → 推送 → 报 digest」。构建入口固定，参数统一：

| 组件 | `-Component` | Dockerfile | 构建上下文 | 默认镜像名 |
|---|---|---|---|---|
| 控制面（**内嵌前端**） | `server`（默认） | `deploy/docker/server.Dockerfile` | 仓库根 | `testplane-server` |
| 前端（独立托管时用） | `web` | `deploy/docker/web.Dockerfile` | 仓库根 | `testplane-web` |
| Tester | `tester:<name>` | `testers/<name>/build/Dockerfile` | **`testers/<name>/`**（不是仓库根） | `testplane-tester-<name>` |

> 默认部署用 **server 内嵌前端**（`go:embed`，单端口同时服务静态资源与 `/api/v1`）；
> `web` 镜像只给"前端单独发版 / 单独托管"的场景，两者互不影响。

## 前置

- Docker 29+（含 buildx）；多架构需要 buildx 的 qemu 支持（本机实测 `linux/arm64` 可构建）
- 默认 registry 是 **GHCR**：`ghcr.io/ucm-system`

```powershell
# 本地登录（gh 的 token 需要有 write:packages）
gh auth token | docker login ghcr.io -u <你的 GitHub 用户名> --password-stdin

# CI 里（repo 为 public 时用内置 token，不需要额外 secret）
docker login ghcr.io -u ${{ github.actor }} -p ${{ secrets.GITHUB_TOKEN }}
```

## 最常用

```powershell
# 控制面：本地构建（不推送）
pwsh deploy/docker/build-push.ps1

# 控制面：构建 + 推送（同时打 :<tag> 与 :sha-<12>）
pwsh deploy/docker/build-push.ps1 -Push

# 前端独立镜像
pwsh deploy/docker/build-push.ps1 -Component web -Push

# Tester（endpoint-probe）
pwsh deploy/docker/build-push.ps1 -Component tester:endpoint-probe -Push

# 多架构（两个平台都真的构建；arm64 走 qemu，较慢）
pwsh deploy/docker/build-push.ps1 -Platforms linux/amd64,linux/arm64 -Push

# 指定版本号
pwsh deploy/docker/build-push.ps1 -Tag v0.2.0 -Platforms linux/amd64,linux/arm64 -Push

# 本地 kind 验证：构建后导入集群（见下方"kind 的坑"）
pwsh deploy/docker/build-push.ps1 -KindCluster testplane
```

## 打出来的两个 tag（为什么）

| tag | 用途 |
|---|---|
| `:<Tag>` | 默认 `git describe --tags --always --dirty`（CI 的 tag push 时就是 `v0.2.0`），人读方便 |
| `:sha-<12 位 git sha>` | **回滚按 sha 拉**（W-8）。`<Tag>` 会被重打，sha tag 不会 |

推送后脚本打印 digest，Helm chart 的 `image.digest` 填它：

```text
digest: sha256:…
引用（digest）: <registry>/<image>@sha256:…
```

最后还会打印机器可读行（CI 直接解析）：

```text
COMPONENT=server  DOCKERFILE=…  CONTEXT=…
TAG=…  SHA_TAG=…  REF_TAG=…  REF_SHA=…
DIGEST=sha256:…          # 多架构时是 manifest list 的 digest
PLATFORMS=linux/amd64,linux/arm64  PUSHED=True
```

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-Component` | `server` | `server` \| `web` \| `tester:<name>` |
| `-Registry` | `ghcr.io/ucm-system` | 仓库前缀（CCR 等其它仓库见"其它 registry"） |
| `-Image` | 按 Component 推导 | 覆盖默认镜像名 |
| `-Tag` | `git describe --tags --always --dirty`，取不到 `dev-<yyyymmdd>` | 主 tag |
| `-Platforms` | `linux/amd64` | 逗号分隔多架构；多架构自动走 `buildx --push` |
| `-Push` | 关 | 不传 = 只构建（PR 校验用） |
| `-NoCache` | 关 | `--no-cache` |
| `-KindCluster` | 空 | 构建完成后导入该 kind 集群（仅单架构） |

## web 镜像的运行时行为

- 非 root（`10001:10001`）、**只读根可跑**：nginx 主配置里 pid / 日志 / 临时目录全部指向 `/tmp`，
  站点配置渲染到 `/tmp/nginx/conf.d/`。k8s 里给 `/tmp` 挂 `emptyDir` 即可
  （`readOnlyRootFilesystem: true`、`securityContext.fsGroup: 10001`）。
- `/api/` **可选反代**：设 `TESTPLANE_API_UPSTREAM=http://testplane-api:8080` 才启用
  （envsubst 渲染模板，SSE 已 `proxy_buffering off`）；不设 = 纯静态，`/api/` 落到 SPA fallback。
- `/healthz` 固定 200（探针用）；SPA 深链 `try_files $uri $uri/ /index.html`；
  `assets/` 长缓存、`index.html` `no-cache`。
- 配置模板在 `web/nginx.conf`，入口脚本 `deploy/docker/web-entrypoint.sh`。

本地验收（已实测）：

```powershell
pwsh deploy/docker/build-push.ps1 -Component web -Tag dev-web
docker run -d --rm -p 18096:8080 `
  -e TESTPLANE_API_UPSTREAM=http://host.docker.internal:8160 `
  ghcr.io/ucm-system/testplane-web:dev-web
curl -s -o /dev/null -w "%{http_code}`n" http://127.0.0.1:18096/         # 200（真 index.html）
curl -s -o /dev/null -w "%{http_code}`n" http://127.0.0.1:18096/runs     # 200（SPA fallback）
curl -s -o /dev/null -w "%{http_code}`n" http://127.0.0.1:18096/healthz  # 200
curl -s -o /dev/null -w "%{http_code}`n" http://127.0.0.1:18096/assets/<file>.js  # 200
```

只读根冒烟（两种模式都验过）：

```powershell
docker run --rm --read-only --tmpfs /tmp -p 18097:8080 ghcr.io/ucm-system/testplane-web:dev-web
```

## Tester 镜像

构建上下文是 **`testers/<name>/`**（不是仓库根），脚本已处理；入口固定 `/tester/run.sh`
（ARCHITECTURE §5.2），非 root `10001`，参数只接受 `--key=value`。

```powershell
pwsh deploy/docker/build-push.ps1 -Component tester:endpoint-probe -Push
docker run --rm ghcr.io/ucm-system/testplane-tester-endpoint-probe:<tag>   # 无参 → status=failed 缺少 --url
```

发布后把 `testers/<name>/tester.yaml` 的 `image:` 换成新产物（tag 或 digest 都行，§5.6）。

## 其它 registry

默认是 GHCR；要推别的仓库用 `-Registry`：

```powershell
docker login ccr.ccs.tencentyun.com
pwsh deploy/docker/build-push.ps1 -Registry ccr.ccs.tencentyun.com/keriko -Push
```

## 多架构注意

`-Platforms linux/amd64,linux/arm64` 会推 **manifest list**（digest 取 index 的），
并真的在 arm64 上构建（qemu 模拟：server 约 6 分钟、web 约 2 分钟、tester 秒级）。
集群里没有 arm 节点时按需在 chart 里用 `nodeSelector` 限定，或只发 amd64。

## kind 的坑（本机实测）

`kind load docker-image` 在这台机器上**失效**：Docker Desktop 用 containerd image store，
且 buildx 产出的是 manifest list，kind 看不到"本地镜像"。所以 `-KindCluster` 时改走：

```text
docker save → docker cp → docker exec <node> ctr -n k8s.io images import
```

`-KindCluster` 只支持**单架构**（kind 节点跑宿主架构），多架构会直接报错，不会悄悄产出不可用镜像。

## CI

`.github/workflows/**` 由仓库维护者负责（GHCR、多架构 matrix 覆盖 server / web / tester）。
