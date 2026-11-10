# Argus Helm Chart

Argus 控制面的**唯一受支持部署形态**。`deploy/k8s` 那套 kustomize 清单只服务本地 kind 调试，不要上共享环境。

- 一个镜像三种角色：`--role=api|controller|collector|all`（`K-7`）
- 产物落 RWX PVC（控制面与 Runner Job 共同读写）
- 部署阶段对接 Genet 的 Helm release 语义（一次部署 = 一个 release，`D-44`）
- 调度复用 Volcano 队列，只读队列状态，不替运维做调度决策

## 1. 三条命令（最短路径）

```bash
# 1) 先建好 4 个 Secret（口令/密钥一律不进 values）
kubectl create namespace argus
kubectl -n argus create secret generic argus-db    --from-literal=dsn='postgres://user:pass@pg.internal:5432/argus?sslmode=require'
kubectl -n argus create secret generic argus-oidc  --from-literal=client-secret='<IdP client secret>' \
                                                          --from-literal=session-secret="$(openssl rand -hex 32)"
kubectl -n argus create secret generic argus-genet --from-literal=api-key='<Genet Open API Bearer key>'
# 可选：镜像仓库凭据
kubectl -n argus create secret docker-registry ghcr-pull --docker-server=ghcr.io \
  --docker-username='<user>' --docker-password='<pass>'

# 2) 复制生产模板改一改
cp deploy/charts/argus/values-production.example.yaml values-prod.yaml

# 3) 装
helm upgrade --install argus deploy/charts/argus -n argus -f values-prod.yaml --wait --timeout 10m
```

装完 `helm status argus -n argus` 会打印 `NOTES.txt`（探活、首个登录、排障命令）。

## 2. 会创建什么

| 对象 | 说明 |
|---|---|
| `Deployment` | 控制面（`replicaCount` 副本，`--role=<role>`）；滚动更新 `maxUnavailable: 0` |
| `Service` | ClusterIP，`8080`（静态资源与 `/api/v1` 同端口，`W-7`） |
| `ConfigMap` | 非敏感配置；改动会触发滚动（`checksum/config` 注解） |
| `Job`（hook） | **pre-install / pre-upgrade** 跑 `migrate up`；多副本并发迁移是错的（`W-11`），所以只允许这个 Job 迁移 |
| `ServiceAccount` + `Role`/`RoleBinding` | 命名空间最小权限（Job/Pod/日志/Lease/Event/PVC 只读） |
| `ClusterRole`（只读） | `nodes`、Volcano `queues`/`podgroups`（队列看板数据源） |
| `PVC` | 产物存储；**生产推荐复用已有 RWX claim**（`persistence.existingClaim`） |
| `Ingress` | 可选（`ingress.enabled`）；SSE 长连接需要关 proxy buffering |
| `PodDisruptionBudget` | 可选（多副本时开；单副本开了会挡 drain） |
| `NetworkPolicy` | 可选（kind 的 CNI 不支持；生产建议开） |

**不创建**（由运维/其它系统负责）：数据库、NFS/RWX 存储、Ingress Controller、Volcano、Genet、IdP。

**刻意不授予**（`06 §4.4`，别"顺手补上"）：`secrets get/list/watch`（唯一例外是 rotate 的窄授权，见 `deploy/k8s/gen-secret-rotate-role.ps1`）、`pods/exec|attach|portforward`、`deployments/services/ingresses` 写、`nodes` 写、Volcano 写。

## 3. 必填项与校验

Chart 会在**渲染期**直接失败并给出中文原因，不会让你部署一个半残的 Pod：

| 条件 | 必填 |
|---|---|
| `config.authMode=oidc`（生产默认） | `oidc.issuer`、`oidc.clientID`、`oidc.redirectURL`、`oidc.clientSecretRef`、`session.secretRef`（HMAC ≥32 字节） |
| `config.authMode=dev`（仅本地） | `config.env` 必须是 `dev` 或 `local`（dev 模式下任何人都是 admin） |
| `config.deployProvider=genet`（生产默认） | `genet.baseURL`、`genet.apiKeySecretRef` |
| 任意 | `database.existingSecret`（DSN 含口令，只能走 Secret）、`image.tag` 或 `image.digest` 二选一 |

## 4. 关键 values

完整列表见 [`values.yaml`](values.yaml)。常改的：

| 键 | 默认 | 说明 |
|---|---|---|
| `image.repository` | `ghcr.io/ucm-system/argus-server` | 镜像仓库 |
| `image.digest` / `image.tag` | `""` / `0.1.0` | **生产用 digest**（`deploy/docker/build-push.ps1` 会打印）；tag 与 digest 二选一 |
| `replicaCount` / `role` | `1` / `all` | **controller 必须单活**（无 leader election）：`role=all\|controller` 且副本 >1 渲染期直接失败；要水平扩只能拆 `role=api`（多副本）+ 独立 `controller`（1 副本） |
| `config.env` / `config.authMode` | `prod` / `oidc` | 生产别用 dev |
| `config.deployProvider` | `genet` | `mock` 只用于本地（进程内假 Genet） |
| `config.deployTimeout` | `72h` | 部署总窗口，取值 `1h..7d`（`K-6`） |
| `config.maxGlobalConcurrency` | `20` | 全局并发 Task 上限（超出排队，不拒绝） |
| `persistence.existingClaim` | `""` | 生产建议给（NFS/CSI 的 RWX claim） |
| `persistence.size` | `100Gi` | 动态供给时的容量估算见 `06 §5.4` |
| `migration.enabled` | `true` | 关掉它就意味着你要自己跑迁移（不推荐） |
| `ingress.enabled` | `false` | 开时记得配 `proxy-buffering: off`（SSE） |
| `networkPolicy.enabled` | `false` | 生产建议开 |
| `podDisruptionBudget.enabled` | `false` | 多副本时开 |

## 5. 升级 / 回滚

```bash
# 升级（pre-upgrade hook 先跑迁移，再滚镜像）
helm upgrade argus deploy/charts/argus -n argus -f values-prod.yaml --wait

# 回滚镜像（钉 digest 才有意义：W-8）
helm upgrade argus deploy/charts/argus -n argus -f values-prod.yaml \
  --set image.digest=sha256:<上一个 digest>

# 回滚整个 release
helm rollback argus -n argus
```

迁移是**只前进**的（`03 §7`）：镜像回滚不需要回滚库；如果需要改 schema，先发"扩展"版本，再发"收口"版本。

## 6. 本地验证（kind）

```bash
# 1) 构建镜像并灌进 kind（本机 Docker Desktop + containerd 下 `kind load` 会失效）
pwsh deploy/docker/build-push.ps1 -KindCluster argus
# 2) 造库与 Secret
docker exec argus-pg psql -U argus -d postgres -c "CREATE DATABASE argus_helm OWNER argus;"
kubectl create namespace argus-helm
kubectl -n argus-helm create secret generic argus-db \
  --from-literal=dsn='postgres://argus:argus@host.docker.internal:5432/argus_helm?sslmode=disable'
# 3) 用 kind 专用 values 装（dev 鉴权 + mock 部署方，仅本地）
helm install tp deploy/charts/argus -n argus-helm \
  -f deploy/charts/argus/values-kind.yaml --wait
# 4) 探活
kubectl -n argus-helm port-forward svc/argus 8080:8080
curl -sS localhost:8080/healthz && curl -sS localhost:8080/readyz
```

实测（2026-11-05，kind v1.33.1）：迁移 hook `schema_version: 1` 通过、Pod `1/1 Running`、
`/healthz` `/readyz` `/api/v1/meta` 均 200、`/` 返回内嵌前端（`index.html` + `/assets/*.js|css` 200）、
在该实例上提交的 Run（`r-20261105-D1JX`）正常建 Job 并执行。

## 7. 与 kustomize 清单的关系

| | Helm（本目录） | `deploy/k8s`（kustomize） |
|---|---|---|
| 用途 | 生产/共享环境 | 本地 kind 调试、演示 |
| 认证默认 | `oidc` + `prod`（安全默认） | `dev`（`overlays/local-dev`） |
| 产物存储 | 生产给 `existingClaim`（RWX） | hostPath 静态 PV |
| 迁移 | chart hook 自动跑 | 人手 `kubectl apply -f migrate-job.yaml` |
| 镜像 | digest 钉死 | `argus-server:dev` + `kind load` |

两者**不要混用**：同一个 namespace 里既有 Helm release 又有 kustomize 对象会导致 ownership 冲突（`helm upgrade` 会报 "exists and cannot be imported"）。

## 8. 排障

```bash
kubectl -n argus get pods,svc,ingress
kubectl -n argus logs deploy/argus -f
kubectl -n argus get events --sort-by=.lastTimestamp | tail -20
helm get values argus -n argus          # 实际生效的 values
helm template argus deploy/charts/argus -n argus -f values-prod.yaml  # 渲染出来看
```

常见问题：

| 现象 | 原因 |
|---|---|
| `helm install` 卡在 pre-install | 迁移 Job 失败：看 `kubectl logs job/<release>-argus-migrate`；多半是 DB DSN/网络 |
| Pod `CrashLoopBackOff` + `缺少 AG_DATABASE_URL` | Secret 名字/键名不对（默认键 `dsn`） |
| `readyz` 持续 503 | 迁移版本不匹配（`jobs` 没跑成）或 DB 不可达 |
| 登录跳转 500 | `oidc.redirectURL` 与 IdP 登记的 callback 不一致（必须逐字节相同） |
| 页面上没有静态资源 | 镜像没打前端（`server.Dockerfile` 的 web 阶段被关掉）；重新 `build-push.ps1` |
| `ClusterRole ... exists and cannot be imported` | 同 namespace 里已有 kustomize 装的同名对象，先 `helm uninstall` 或换 release 名 |

## 9. 镜像从哪来（GHCR）

发版走 tag，镜像自动打到 GitHub Container Registry（公开仓库用 `GITHUB_TOKEN`，**不需要额外 Secret**）：

```bash
git tag -a v0.1.0 -m "Argus v0.1.0" && git push origin v0.1.0
```

| 镜像 | 说明 |
|---|---|
| `ghcr.io/ucm-system/argus-server` | 控制面（前端已内嵌，单端口） |
| `ghcr.io/ucm-system/argus-web` | 可选：纯静态前端镜像（需要单独托管前端时用） |
| `ghcr.io/ucm-system/argus-runner-<name>` | 各 Runner（`runners/<name>/build/Dockerfile` 自动发现） |

每个镜像都有 `:<version>` / `:sha-<12>` / `:latest` 三个 tag，**双架构** `linux/amd64` + `linux/arm64`；
digest 在 Actions 的 job summary 里，直接填 `image.digest`。

> 首次发版后去 **Packages → Package settings → Change visibility → Public**，否则匿名拉取 401。
> CI 用 `GHCR_TOKEN`（PAT，`write:packages`）：本组织把默认 workflow 权限锁成 `read`，`GITHUB_TOKEN` 拿不到 `packages: write`。
> 需要私有包时用 `image.pullSecrets`（`kubectl create secret docker-registry ghcr-pull ...`）。

### 迁移 hook 的两条边界

- **失败会留证据，但下次升级会被覆盖**：hook 用 `before-hook-creation`，失败的迁移 Job 会保留到下一次 `helm upgrade`
  才被删掉——出事要**先看日志再升级**：`kubectl -n <ns> logs job/<release>-argus-migrate`。
- **Helm 只锁同一个 release**：两个不同 release 指向同一个数据库时仍会各迁移一次；`store` 层**没有** advisory lock，
  所以同一个库只允许一个 Argus 安装（多环境共库是禁止的）。