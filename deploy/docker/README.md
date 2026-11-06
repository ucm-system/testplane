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
# 会自动多打一个 :kind-local —— values-kind.yaml 默认就钉它，装的时候不用再 --set image.tag
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
| `-KindCluster` | 空 | 构建完成后导入该 kind 集群（仅单架构）；**同时额外打 `:kind-local`**（`values-kind.yaml` 默认值，装的时候一条命令即可） |

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

`-Platforms linux/amd64,linux/arm64` 会推 **manifest list**（digest 取 index 的）。

`server.Dockerfile` 用 **交叉编译**：web 阶段与 golang 构建阶段都钉在 `--platform=$BUILDPLATFORM`，
按 `GOARCH=$TARGETARCH` 产出目标架构二进制（`CGO_ENABLED=0`，静态二进制）。实测双架构从 361s 降到 **~18s**。

> ⚠️ 两条踩过的坑，改 Dockerfile 时别重犯：
> 1. **不要给 `ARG TARGETARCH` 写默认值**（`ARG TARGETARCH=amd64`）。默认值会盖掉 buildx 注入的值，
>    结果 arm64 镜像里塞的是 amd64 二进制。要兜底就在 RUN 里写 `${TARGETARCH:-amd64}`。
> 2. 光看"构建成功"不够。构建末尾有 ELF 自检（读 `e_machine`：`3e00`=x86-64、`b700`=AArch64），
>    它会拿实际二进制与 `TARGETARCH` 比对，不一致直接让构建失败。
>    验证产物时**必须真跑**：`docker run --platform linux/arm64 --entrypoint /bin/sh <ref> -c 'uname -m; dd if=/testplaned bs=1 count=20 | od -An -tx1'`。

集群里没有 arm 节点时按需在 chart 里用 `nodeSelector` 限定，或只发 amd64。

## CI 产出与本地产出的对应关系

CI（`.github/workflows/**`，仓库维护者负责）与本脚本用**同一套组件名**，对不上时照这张表查：

| CI matrix 值 | 本脚本命令 | 镜像 | Dockerfile | 构建上下文 |
|---|---|---|---|---|
| `server` | `-Component server` | `ghcr.io/ucm-system/testplane-server` | `deploy/docker/server.Dockerfile` | 仓库根 |
| `web` | `-Component web` | `ghcr.io/ucm-system/testplane-web` | `deploy/docker/web.Dockerfile` | 仓库根 |
| `tester:endpoint-probe` | `-Component tester:endpoint-probe` | `ghcr.io/ucm-system/testplane-tester-endpoint-probe` | `testers/endpoint-probe/build/Dockerfile` | `testers/endpoint-probe/` |

每个镜像都打两个 tag（`:<tag>`、`:sha-<12>`）；CI 解析脚本输出的 `DIGEST=` 写 job summary，
Helm chart 的 `image.digest`、Tester 的 `tester.yaml` 都用它。

## 排障表

| 症状 | 先看什么 |
|---|---|
| `web` 容器**起来就退出** | 入口脚本的 `nginx -t` 自检输出（渲染坏/变量写错都会在这里明确报错，而不是留个 502 的容器） |
| `web` 起来但 `/api/` 502 | 有没有设 `TESTPLANE_API_UPSTREAM`、上游是否可达；纯静态模式下 `/api/` 会返回 `index.html`（200），不是错误 |
| `web` 只读根下崩溃 | `/tmp` 是否挂了可写空挂（`--tmpfs /tmp` / emptyDir）；`securityContext.fsGroup: 10001` |
| `server` 构建报 `BUILD_CHECK_FAIL: TARGETARCH=…, elf e_machine=…` | 交叉编译的 `GOARCH` 没生效（见上文坑 1） |
| `kind load docker-image` 报找不到镜像 | 这不是 "load 失败"，是 Docker Desktop + containerd + manifest list；用 `-KindCluster`（走 save→cp→ctr import） |
| 推送 403/denied | GHCR：token 缺 `write:packages`；CCR：没 `docker login` |

## Tester 发版流程约束（重要）

`testers/<name>/tester.yaml` 是 **Tester 契约**，`image:` 是契约内容的一部分（K-3：版本钉死、注册表会校验）。
镜像换了但 `version` 没换 = 同一版本号指向不同内容，注册表校验/复现都会出问题。

正确流程（对应设计里的 G-15「版本升级流程」）：

```text
1. CI 产出新镜像：ghcr.io/ucm-system/testplane-tester-<name>@sha256:<digest>（+ :sha-<12> tag）
2. Tester 维护者改 tester.yaml：
     image:   ghcr.io/ucm-system/testplane-tester-<name>@sha256:<digest>   # 建议用 digest 而不是 tag
     version: <新版本号>                                                    # 必须一起改
3. 走新版本发布（版本号 → 注册表重新入库 → 平台按新版本调度）
```

**严禁**只改 `image:` 不改 `version:`——那会让"同一个版本号"对应两份不同内容，历史 Run 无法复现。
（本仓库当前的 `testers/endpoint-probe/tester.yaml` 仍指向旧的 `registry.dev.huawei.com/...:1.0.0`，
等 G-15 落地后由 Tester 维护者按上面流程一次性升级。）

## kind 的坑（本机实测）

`kind load docker-image` 在这台机器上**失效**：Docker Desktop 用 containerd image store，
且 buildx 产出的是 manifest list，kind 看不到"本地镜像"。所以 `-KindCluster` 时改走：

```text
docker save → docker cp → docker exec <node> ctr -n k8s.io images import
```

`-KindCluster` 只支持**单架构**（kind 节点跑宿主架构），多架构会直接报错，不会悄悄产出不可用镜像。

## CI

`.github/workflows/**` 由仓库维护者负责（GHCR、多架构 matrix 覆盖 server / web / tester）。
