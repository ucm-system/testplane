# 控制面镜像：构建与发布

一条命令完成「构建 → 打两个 tag → 推送 → 报 digest」，运维与 CI 共用。
构建入口**只有** `deploy/docker/server.Dockerfile`（W-8 / `web-and-delivery §7、§8`）。

## 前置

- Docker 29+（含 buildx）；多架构需要 buildx 的 `docker-container`/`docker` driver 支持对应平台
- 推送前先登录 registry（CI 里由 `secrets.CCR_USERNAME` / `CCR_PASSWORD` 完成）：
  ```powershell
  docker login ccr.ccs.tencentyun.com
  ```

## 最常用

```powershell
# 本地构建（不推送）
pwsh deploy/docker/build-push.ps1

# 构建 + 推送（同时打 :<tag> 与 :sha-<12>）
pwsh deploy/docker/build-push.ps1 -Push

# 指定版本号
pwsh deploy/docker/build-push.ps1 -Tag v0.2.0 -Push

# 多架构（必须带 -Push：buildx 不能把 manifest list 载入本地 docker）
pwsh deploy/docker/build-push.ps1 -Platforms linux/amd64,linux/arm64 -Push

# 本地 kind 验证：构建后导入集群（见下方"kind 的坑"）
pwsh deploy/docker/build-push.ps1 -KindCluster testplane
```

## 打出来的两个 tag（为什么）

| tag | 用途 |
|---|---|
| `:<Tag>` | 默认 `git describe --tags --always --dirty`（CI 上 tag push 时就是 `v0.2.0`），人读方便 |
| `:sha-<12 位 git sha>` | **回滚按 sha 拉**（W-8）。`<Tag>` 会被重打，sha tag 不会 |

推送后脚本打印 digest，Helm chart 的 `image.digest` 填它：

```text
digest: sha256:…
引用（digest）: <registry>/<image>@sha256:…
```

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-Registry` | `ccr.ccs.tencentyun.com/keriko` | 仓库前缀 |
| `-Image` | `testplane-server` | 镜像名 |
| `-Tag` | `git describe --tags --always --dirty`，取不到用 `dev-<yyyymmdd>` | 主 tag |
| `-Platforms` | `linux/amd64` | 逗号分隔多架构；多架构时自动走 `buildx --push` |
| `-Push` | 关 | 不传 = 只构建（PR 校验用） |
| `-NoCache` | 关 | `docker build --no-cache` |
| `-KindCluster` | 空 | 构建完成后导入该 kind 集群 |

## kind 的坑（本机实测）

`kind load docker-image` 在这台机器上**失效**：Docker Desktop 用 containerd image store，
且 buildx 产出的是 manifest list，kind 看不到"本地镜像"。所以脚本在 `-KindCluster` 时改走：

```text
docker save → docker cp → docker exec <node> ctr -n k8s.io images import
```

`-KindCluster` 只支持**单架构**（kind 节点跑宿主架构），多架构会直接报错而不是悄悄产出不可用镜像。

## 多架构注意

`-Platforms linux/amd64,linux/arm64` 会推 manifest list；集群里没有 arm 节点时，
arm 那一份拉取会失败——按需在 chart 里用 `nodeSelector` 限定，或只发 amd64。

## CI

`.github/workflows/release-image.yml`：

- `push` tag `v*` 或手工 `workflow_dispatch` → 构建 + 推送 + digest 写 job summary；
- 任何 PR → **只构建不推送**（防止 Dockerfile 腐烂）。

需要的仓库 Secret：`CCR_USERNAME` / `CCR_PASSWORD`；可选 Variable `CCR_REGISTRY`（默认上面的 registry）。
