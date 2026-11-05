# TestPlane · 部署与本地环境（`deploy/`）

面向 **dev 环境**：kind 单节点集群 + docker 里的 PostgreSQL 16 + 本地构建的镜像。
从零到「集群 ready + PG 起来 + 镜像已灌入」全部可复制粘贴，Windows / PowerShell。

> 规格来源是 `design/`，不是本文件。本文件只回答"怎么把它跑起来"；
> 拓扑、RBAC、存储树、容量见 [`design/views/06-physical-view.md`](../design/views/06-physical-view.md)，
> Job 渲染见 [`design/impl/execution.md`](../design/impl/execution.md) §2–§3。

**已核验环境（2026-11-05）**

| 项 | 实际值 |
|---|---|
| kind | `v0.29.0 go1.24.2 windows/amd64`（装在 `.tools\kind.exe`） |
| 集群 | `testplane`，1 control-plane，k8s `v1.33.1`，context `kind-testplane` |
| PostgreSQL | `16.13`，容器 `testplane-pg`，宿主端口 **5432** |
| Docker | `29.9.2`（Desktop，WSL2 backend） |

---

## 0 前置条件

- Docker Desktop 已启动（`docker version` 能打出 Server 段）。
- `kubectl` 在 PATH 上（本仓库只用它连本集群，改 context 不改别人的集群）。
- PowerShell 5.1+ 或 pwsh 7+。

> **不要碰别人的集群**：`claw-fleet`、`unitest-k3s`、`docker-desktop` 是既有环境。
> 本文所有 `kubectl` 命令都显式带 `--context kind-testplane`，别省略。

---

## 1 装 kind.exe（仓库内，不入库）

`kind.exe` 落在 `.tools\`，由 `.gitignore` 的 `*.exe` 规则挡住（不需要额外改忽略清单）。

```powershell
New-Item -ItemType Directory -Force -Path .tools | Out-Null
$ProgressPreference = 'SilentlyContinue'
Invoke-WebRequest -Uri 'https://kind.sigs.k8s.io/dl/v0.29.0/kind-windows-amd64' `
                  -OutFile '.tools\kind.exe' -UseBasicParsing
& .\.tools\kind.exe version
# kind v0.29.0 go1.24.2 windows/amd64
```

> WSL 里那个 kind 用不了（WSL 的 docker socket 没通），别在 WSL 里折腾。

---

## 2 建集群 `testplane`

```powershell
New-Item -ItemType Directory -Force -Path .data\storage | Out-Null
& .\.tools\kind.exe create cluster --config deploy\kind\testplane.yaml --wait 180s
kubectl --context kind-testplane get nodes
```

期望：

```text
NAME                      STATUS   ROLES           AGE   VERSION
testplane-control-plane   Ready    control-plane   ...   v1.33.1
```

关键点（配置写在 [`deploy/kind/testplane.yaml`](kind/testplane.yaml)）：

- 集群名 `testplane`，1 个 control-plane 就够。
- `extraMounts` 把宿主 `D:\Project\TestPalne\.data\storage` 挂成节点里的 `/storage-root`。
  这就是 dev 的「RWX 存储」（见第 4 步）。
- **`extraMounts` 只在建集群时生效**。改了 yaml 要 `kind delete cluster --name testplane` 再建。
- 想验证挂载真的通了：

  ```powershell
  docker exec testplane-control-plane sh -c 'echo hi > /storage-root/.node-write && cat /storage-root/.node-write && rm -f /storage-root/.node-write'
  ```

---

## 3 起 PostgreSQL 16

库名 / 用户 / 口令都是 `testplane`。端口优先 **5432**；被占用就换 5433 并同步改
[`deploy/.env.example`](.env.example)（那份文件记录的就是实际值）。

```powershell
# 先看 5432 有没有被占：有输出就是被占用，改用 -p 5433:5432 并改 .env.example
Get-NetTCPConnection -State Listen -LocalPort 5432 -ErrorAction SilentlyContinue

docker volume create testplane-pg-data
docker run -d --name testplane-pg --restart unless-stopped `
  -e POSTGRES_DB=testplane -e POSTGRES_USER=testplane -e POSTGRES_PASSWORD=testplane `
  -e TZ=Asia/Shanghai -p 5432:5432 `
  -v testplane-pg-data:/var/lib/postgresql/data `
  --health-cmd "pg_isready -U testplane -d testplane" `
  --health-interval 5s --health-timeout 3s --health-retries 10 `
  postgres:16-alpine

# 验证
docker exec testplane-pg pg_isready -U testplane -d testplane
# /var/run/postgresql:5432 - accepting connections
docker exec testplane-pg psql -U testplane -d testplane -tAc "select version();"
# PostgreSQL 16.13 on x86_64-pc-linux-musl, ...
```

为什么数据用 **named volume** 而不是宿主目录：Windows 绑定挂载到 `postgres` 容器的
`PGDATA` 会因为 drvfs 的属主/权限语义让 `initdb` 报错（`could not change permissions`）。
named volume 在 Docker VM 里是原生 ext4，没这个问题。

**从集群里连 PG**（in-cluster 跑控制面时）：走 `host.docker.internal`，已核验可解析可连：

```powershell
docker exec testplane-control-plane bash -c 'timeout 5 bash -c "exec 3<>/dev/tcp/host.docker.internal/5432 && echo TCP_OK"'
```

---

## 4 应用 K8s 清单

```powershell
kubectl --context kind-testplane apply -k deploy/k8s
kubectl --context kind-testplane -n testplane get sa,pvc,cm
kubectl --context kind-testplane get pv testplane-storage
```

会创建：

| 对象 | 文件 | 说明 |
|---|---|---|
| Namespace `testplane`（PSA `restricted`） | [`k8s/namespace.yaml`](k8s/namespace.yaml) | 控制面 + Tester Job + PVC + Lease 同处一室 |
| ServiceAccount `testplane` | [`k8s/serviceaccount.yaml`](k8s/serviceaccount.yaml) | 只有控制面需要 token |
| Role / RoleBinding `testplane` | [`k8s/rbac.yaml`](k8s/rbac.yaml) | namespace 级：jobs 全套、pods get/list/watch/delete、pods/log、pvc get、events list、leases get/create/update |
| ClusterRole / Binding `testplane-view` | [`k8s/rbac.yaml`](k8s/rbac.yaml) | 集群级**只读**：nodes + Volcano queues/podgroups/jobs |
| PV/PVC `testplane-storage` | [`k8s/storage.yaml`](k8s/storage.yaml) | 10Gi、`ReadWriteMany`、hostPath `/storage-root`、`Retain` |
| ConfigMap `testplane-config` | [`k8s/configmap.yaml`](k8s/configmap.yaml) | 非敏感配置；键名对齐 `server/internal/config/config.go` |

**刻意不在 kustomization 里**的五份清单（各自有启用条件）：

| 文件 | 什么时候加 |
|---|---|
| [`k8s/deployment.yaml`](k8s/deployment.yaml) | 等 `server/cmd/testplaned` 存在、镜像构建并灌进集群后（第 6 步） |
| [`k8s/migrate-job.yaml`](k8s/migrate-job.yaml) | 同上；迁移**必须**先于控制面启动跑完（`testplaned migrate up`，W-11） |
| [`k8s/rbac-cluster-executor.yaml`](k8s/rbac-cluster-executor.yaml) | Job 要落到 `testplane` 以外的 namespace 时（扩权，需评审） |
| [`k8s/networkpolicy.yaml`](k8s/networkpolicy.yaml) | 有真 CNI 时。**kind 的默认 CNI（kindnet）不实现 NetworkPolicy**，apply 了也不生效 |
| [`k8s/pdb.yaml`](k8s/pdb.yaml) | 单副本 + `minAvailable: 1` 会永久阻塞 drain（06 §7.1），dev 不需要 |

### 4.1 存储冒烟（每建一次集群跑一次）

```powershell
kubectl --context kind-testplane apply -f deploy/k8s/storage-smoke-job.yaml
kubectl --context kind-testplane -n testplane wait --for=condition=complete job/testplane-storage-smoke --timeout=180s
kubectl --context kind-testplane -n testplane logs job/testplane-storage-smoke
Get-ChildItem -Recurse .data\storage\smoke
kubectl --context kind-testplane -n testplane delete job testplane-storage-smoke
```

期望日志里有 `uid=10001` 与 `STORAGE_SMOKE_OK`，且宿主 `.data\storage\smoke\...` 下能看到
同一个 `stdout.log`——这说明「宿主目录 ↔ 节点 `/storage-root` ↔ PVC ↔ Pod」是同一份数据，
而且 **UID 10001 能写**（06 §3.2 的属主要求）。

---

## 5 构建镜像并灌进集群

`kind` 的节点看不到宿主的 docker 镜像库，所以要 `kind load docker-image` 灌进去。
两个镜像都**不打 `:latest`**（W-8），并按 dev 约定在本地构建。

### 5.1 Tester 镜像 `endpoint-probe:1.0.0`

构建上下文 = **Tester 目录**（不是 `build/`），用现成的 `build/Dockerfile`：

```powershell
Push-Location testers\endpoint-probe
docker build -f build/Dockerfile `
  -t testplane/endpoint-probe:1.0.0 `
  -t registry.dev.huawei.com/testplane/endpoint-probe:1.0.0 .
Pop-Location

# 两个 tag 都灌（同一镜像 ID，只多一层 tag，不占两份空间）
& .\.tools\kind.exe load docker-image `
  testplane/endpoint-probe:1.0.0 `
  registry.dev.huawei.com/testplane/endpoint-probe:1.0.0 `
  --name testplane

# 验证（在节点里看 containerd 的镜像库）
docker exec testplane-control-plane crictl images | Select-String endpoint-probe
```

> **为什么打两个 tag —— 这是 `tester.yaml` 的 image 引用问题，别再想别的招**
>
> [`testers/endpoint-probe/tester.yaml`](../testers/endpoint-probe/tester.yaml) 里写的是
> `image: registry.dev.huawei.com/testplane/endpoint-probe:1.0.0`（CI 推内部仓库的形态）。
> 平台把 `image` **原样透传**给 K8s（E-2.1、§5.6 不解析 tag/digest），所以本地要么能推到那个
> 仓库，要么让节点上"已经有这个引用"。
>
> 做法：本地构建后**多打一个 tag**，名字正好等于 `tester.yaml` 里的引用，再 `kind load`。
> 因为 tag 不是 `:latest`，`imagePullPolicy` 是 `IfNotPresent`（E-2.4），kubelet 发现本地已有
> 该引用就直接用，不会去 `registry.dev.huawei.com` 拉。
>
> **收益：`testers/` 里的文件一个字都不用改**（改了就是动契约 + 可能被注册表校验拦住）。
> 代价：这个 tag 是本地约定，换个 registry 域名要重打——写在本文第 5 步，别散落在聊天里。
>
> 备选（不推荐）：把 `testers/` 换成别的镜像名 → 属于改契约，需 Lead 裁决；
> 或者用 `TESTPLANE_TESTERS_DIR` 挂一个改过的 `tester.yaml` → 多一份漂移源，dev 不值得。

已核验：用 `registry.dev.huawei.com/testplane/endpoint-probe:1.0.0` 起一个
`restricted` 的 Job，运行用户 `uid=10001`、`/tester/run.sh` 可执行、无外网拉取。

### 5.2 控制面镜像 `testplane-server:dev`

构建上下文 = **仓库根**（要 `server/` 与 `testers/`，W-8）：

```powershell
docker build -f deploy/docker/server.Dockerfile -t testplane-server:dev .
& .\.tools\kind.exe load docker-image testplane-server:dev --name testplane
docker exec testplane-control-plane crictl images | Select-String testplane-server
```

- 依赖版本（go.mod）已就位后这条才可能成功；`server/cmd/testplaned` 还没写时**必然失败**，
  这是预期的——Dockerfile 本身可以用 `docker build --check -f deploy/docker/server.Dockerfile .` 静态校验。
- 前端阶段（`node:22-alpine` → `web/dist` → `go:embed`）写在 Dockerfile 末尾的注释里，
  等 `web/` 有产物再启用。
- 运行阶段是 `alpine` + `ca-certificates` + `tzdata`，UID **10001**（`/storage-root` 的属主就是 10001）。

---

## 6 把控制面放进集群（可选，等镜像就绪）

```powershell
# 1) DB DSN 进 Secret（不落盘、不入库）
kubectl --context kind-testplane -n testplane create secret generic testplane-db `
  --from-literal=dsn='postgres://testplane:testplane@host.docker.internal:5432/testplane?sslmode=disable' `
  --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -

# 2) 先跑迁移，再起控制面（迁移是唯一入口，多副本不得并发迁移 → W-11）
kubectl --context kind-testplane apply -f deploy/k8s/migrate-job.yaml
kubectl --context kind-testplane -n testplane wait --for=condition=complete job/testplaned-migrate --timeout=300s
kubectl --context kind-testplane -n testplane logs job/testplaned-migrate
kubectl --context kind-testplane -n testplane delete job testplaned-migrate

# 3) 启用 Deployment：把 deployment.yaml 加进 kustomization 的 resources
#    （edit deploy/k8s/kustomization.yaml，取消 `# - deployment.yaml` 的注释）
kubectl --context kind-testplane apply -k deploy/k8s

# 4) 看状态
kubectl --context kind-testplane -n testplane rollout status deploy/testplaned --timeout=180s
kubectl --context kind-testplane -n testplane get pods -o wide
kubectl --context kind-testplane -n testplane port-forward svc/testplane 8080:8080
```

`psql` 直连（可选，验证 Secret 里的 DSN 拼对了）：

```powershell
docker run --rm -e PGPASSWORD=testplane postgres:16-alpine `
  psql -h host.docker.internal -U testplane -d testplane -c "select 1"
```

---

## 7 验收速查（贴进 issue 用这几条）

```powershell
& .\.tools\kind.exe version
kubectl --context kind-testplane get nodes
docker exec testplane-pg pg_isready -U testplane -d testplane
kubectl --context kind-testplane -n testplane get pvc testplane-storage
kubectl --context kind-testplane get pv testplane-storage
kubectl --context kind-testplane auth can-i create jobs -n testplane --as=system:serviceaccount:testplane:testplane
kubectl --context kind-testplane auth can-i get secrets -n testplane --as=system:serviceaccount:testplane:testplane   # 期望 no
kubectl --context kind-testplane auth can-i list nodes --as=system:serviceaccount:testplane:testplane
docker exec testplane-control-plane crictl images
```

**清理**（只清 TestPlane 自己的东西，别碰别人的容器/集群）：

```powershell
& .\.tools\kind.exe delete cluster --name testplane
docker rm -f testplane-pg
docker volume rm testplane-pg-data        # 连数据一起删；只想清集群就跳过这两条
```

---

## 8 坑与约定（踩过才写下来）

| 坑 | 现象 | 处理 |
|---|---|---|
| `extraMounts` 不生效 | 节点里 `/storage-root` 是空的/不存在 | 只有 `kind create` 时生效，改了配置必须 `kind delete` 再建 |
| kind load 报 `content digest … not found` | 节点导入镜像失败 | 多架构/带 attestation 的 manifest list 会触发；`docker build --provenance=false` 重建，或 `docker save <img> \| docker exec -i testplane-control-plane ctr -n k8s.io images import -` |
| PVC 一直 `Pending` | 没绑上 PV | 两侧 `storageClassName: ""` + PV 的 `claimRef` 必须同时存在；kind 自带 `standard`（local-path）会抢动态供给 |
| PSA 拒绝 Pod | `violates PodSecurity "restricted"` | Pod 必须 `runAsNonRoot` + drop `ALL` + `seccompProfile: RuntimeDefault`；`storage-smoke-job.yaml` 是可直接照抄的模板 |
| busybox 没有 brace expansion | `mkdir -p a/{b,c}` 建出名字叫 `{b,c}` 的目录 | 一行一个 `mkdir -p`（冒烟 Job 里踩过一次，已修） |
| 冒烟 Job 写不进 `/storage-root` | `Permission denied` | 确认 Pod 的 `runAsUser: 10001` + `fsGroup: 10001`；宿主目录是 drvfs（`0777`），UID 只是约定不是硬约束 |
| dockerd 的 5432 被占 | `testplane-pg` 起不来 | 换 `-p 5433:5432`，并同步 `deploy/.env.example` 的值 |
| 端口 8080 | 控制面在集群内监听 `:8080`，宿主没映射 | 用 `kubectl port-forward`，不要给 kind 加 `extraPortMappings`（会和宿主 dev server 抢） |

---

## 9 目录结构

```text
deploy/
├── README.md                    # 本文件
├── .env.example                 # 环境变量样例（deploy/.env 被 .gitignore 忽略）
├── kind/
│   └── testplane.yaml           # kind 集群定义（含 extraMounts → /storage-root）
├── k8s/                         # 原生清单 + kustomization
│   ├── kustomization.yaml       #   kubectl apply -k deploy/k8s
│   ├── namespace.yaml           #   Namespace + PSA restricted
│   ├── serviceaccount.yaml      #   SA testplane
│   ├── rbac.yaml                #   Role/RoleBinding + ClusterRole/CRB testplane-view
│   ├── rbac-cluster-executor.yaml  # （opt-in）跨 namespace 执行层
│   ├── storage.yaml             #   PV + PVC testplane-storage（10Gi RWX）
│   ├── configmap.yaml           #   testplane-config
│   ├── deployment.yaml          # （opt-in）Deployment testplaned + Service
│   ├── migrate-job.yaml         # （opt-in）迁移 Job（testplaned migrate up）
│   ├── networkpolicy.yaml       # （opt-in）默认拒绝 + 放行条目
│   ├── pdb.yaml                 # （opt-in）PDB
│   └── storage-smoke-job.yaml   #   存储冒烟 Job（一次性）
└── docker/
    ├── server.Dockerfile        # 控制面镜像（上下文 = 仓库根）
    └── server.Dockerfile.dockerignore  # 该 Dockerfile 专用的上下文过滤
```

---

## 10 待办 / 未做

- Helm chart（`deploy/helm/testplane/`）：P0 用原生清单 + kustomize 就够，chart 等要发版时再拆（W-9）。
- 真 RWX：多节点必须换 NFS CSI（06 §5.5）；单节点 hostPath 只是等价物，`accessModes: ReadWriteMany` 在单节点下事实成立、跨节点无保证。
- 控制面镜像的实际构建：等 `server/cmd/testplaned` 落地（第 5.2 步）。
- `deploy/versions.lock`（镜像 digest 记录，W-8）：等有了真镜像再记。
