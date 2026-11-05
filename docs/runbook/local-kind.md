# TestPlane · 本地 kind 环境运行手册

> 从裸机到一次 Run 跑完的操作手册 + 真踩过的坑。每条都实跑验证过，不凭记忆写。
> 环境事实与一次性搭建（kind / PG / 存储 / RBAC）见 [`deploy/README.md`](../../deploy/README.md)，
> 17 项验收结论见 [`acceptance.md`](acceptance.md)，本文只记**操作动作 + 坑 + 判据**。

---

## 0 标准重灌动线（改了 server/ 代码后唯一正确的顺序）

五步，顺序不能换：**先建镜像 → 再灌进节点 → 再 apply 清单 → 再拉起副本 → 最后看健康**。
`kind` 的节点看不到宿主的 docker 镜像库，所以"灌"这一步永远不能省。

```powershell
# 1 构建（上下文 = 仓库根，别的路径都不对）
docker build -f deploy/docker/server.Dockerfile -t testplane-server:dev .

# 2 灌进 kind 节点（只覆盖节点 containerd，不影响宿主镜像）
& .\.tools\kind.exe load docker-image testplane-server:dev --name testplane

# 3 应用清单（基础设施 + 控制面；只想要基础设施用 -k deploy/k8s）
kubectl --context kind-testplane apply -k deploy/overlays/with-control-plane

# 4 确保副本是 1（有人排障时会 scale 到 0，避免双 controller）
kubectl --context kind-testplane -n testplane scale deploy/testplaned --replicas=1

# 5 看健康（三条都要过）
kubectl --context kind-testplane -n testplane rollout status deploy/testplaned --timeout=180s
kubectl --context kind-testplane -n testplane exec deploy/testplaned -- wget -qO- http://127.0.0.1:8080/healthz
kubectl --context kind-testplane -n testplane exec deploy/testplaned -- wget -qO- http://127.0.0.1:8080/readyz
```

**判据**（四条全中才算重灌成功）：

1. `get pods` → `testplaned-xxxx 1/1 Running`；
2. `logs deploy/testplaned` 里有 `orchestrator: controller started (ns=testplane, concurrency=20, storage=testplane-storage)`；
3. `/healthz` 与 `/readyz` 都返回 `ok`（`readyz` 含 DB ping + 存储可写 + 迁移版本）；
4. 节点里镜像是最新的：`docker exec testplane-control-plane crictl images | Select-String testplane-server`，
   且**运行中的 Pod 用的就是它**：

   ```powershell
   kubectl --context kind-testplane -n testplane get pod -l app.kubernetes.io/name=testplaned `
     -o jsonpath='{.items[0].status.containerStatuses[0].imageID}{"\n"}'
   docker images testplane-server:dev --format '{{.ID}}{{"\n"}}'
   # 两个 digest 要一致；不一致 = Pod 还跑着旧镜像（这就是"改了代码但功能没变"的头号原因）
   ```

> 第 4 步不能漏。`kubectl apply`/`rollout` 只保证"新 Pod 用新镜像"，但若之前有人把副本缩到 0，
> apply 一个 `replicas:1` 的 Deployment 也会拉起来——两条路径都要确认**只有一个** controller，
> 否则两个 controller 同时 dispatch，Job 会被建两遍。

跑迁移（表结构变了才需要；正常启动不迁移）：

```powershell
kubectl --context kind-testplane -n testplane apply -f deploy/components/control-plane/migrate-job.yaml
kubectl --context kind-testplane -n testplane wait --for=condition=complete job/testplaned-migrate --timeout=180s
kubectl --context kind-testplane -n testplane logs job/testplaned-migrate    # 期望 schema_version=N
kubectl --context kind-testplane -n testplane delete job testplaned-migrate
```

---

## 0.1 日常起停（一台机器上反复做的事）

```powershell
# 起（Docker Desktop 先起来）
docker start testplane-pg                        # PG 容器；--restart unless-stopped，通常不用管
& .\.tools\kind.exe get clusters                 # testplane 在不在
kubectl --context kind-testplane get nodes       # Ready 才算起

# 停
& .\.tools\kind.exe stop cluster  --name testplane   # 保留数据，可 start 回来
& .\.tools\kind.exe start cluster --name testplane

# 彻底重建（存储挂载变更后必须重建）
& .\.tools\kind.exe delete cluster --name testplane
& .\.tools\kind.exe create cluster --config deploy\kind\testplane.yaml --wait 180s
kubectl --context kind-testplane apply -k deploy/k8s
# 重建后镜像要重新灌（节点没了，containerd 里的镜像也一起没了）
```

**判据**：`get nodes` 的 `STATUS=Ready`；`docker exec testplane-pg pg_isready -U testplane -d testplane` 打出 `accepting connections`。

---

## 1 坑：kind 节点里的时区 / apt / yum

| 现象 | 原因 | 处理 |
|---|---|---|
| 节点里 `date` 是 UTC，日志时间戳差 8 小时 | `kindest/node` 是 Debian，默认容器时区 UTC | **不要**靠节点时区：平台的时间戳取 apiserver / DB（06 §7.1），Pod 里要本地时区就 `TZ=Asia/Shanghai` + 镜像自带 tzdata（我们的两个镜像都装了） |
| 想在节点里 `apt-get install` 调试工具 | `kindest/node` 里没有 apt（被裁掉），`kubectl exec` 进节点更不该装东西 | 用 `kubectl run --rm -it --image=alpine:3.20 --restart=Never ...` 起临时 Pod；节点侧只剩 `crictl`、`ctr` |
| `docker exec testplane-control-plane apt-get ...` 报 command not found | 同上 | 同上；真要动节点就换镜像重建，别在节点里打补丁（不可复现） |

---

## 2 坑：hostPath / PVC 权限

| 现象 | 原因 | 处理 |
|---|---|---|
| PVC `Pending`，`describe pvc` 看不到 provision | PV 与 PVC 的 `storageClassName` 不一致 | 两侧都写 `storageClassName: ""`，PV 再用 `claimRef` 预绑到 `testplane/testplane-storage` |
| Pod `Permission denied` 写 `/storage-root` | Pod UID 与宿主目录属主不一致 | Pod 必须 `runAsUser: 10001` + `fsGroup: 10001`（`/storage-root` 属主 10001:10001，06 §3.2）；`storage-smoke-job.yaml` 是可照抄的模板 |
| `chown` 在容器里失败 / 静默无效 | Docker Desktop 的 Windows 绑定挂载是 drvfs，属主/权限语义不完整 | chown 只当优化（E-3.5 同款结论）；靠 `fsGroup` + 目录预创建 + Tester 自己 `mkdir -p` 兜底。实际观测：drvfs 上目录是 `0777 root:root`，任何 UID 都能写 |
| 节点里 `/storage-root` 是空的 | `extraMounts` 只在 `kind create` 时生效 | 改 `deploy/kind/testplane.yaml` 后必须 `kind delete cluster` + 重建 |
| 重建集群后产物不见了 | 宿主目录其实还在，是 PVC/PV 没了 | 数据在 `D:\Project\TestPalne\.data\storage`；`apply -k` 重建 PVC（PV `Retain`，重绑同一路径即可） |
| Windows 路径写成反斜杠 | kind 的 `hostPath` 走 docker `-v`，反斜杠会被当转义 | 用正斜杠：`D:/Project/TestPalne/.data/storage` |

---

## 3 坑：PSA restricted 拦 Pod

**现象**：`Error creating: pods "..." is forbidden: violates PodSecurity "restricted:latest": ...`

namespace `testplane` 打了 `pod-security.kubernetes.io/enforce: restricted`（06 §2），任何 Pod 都要满足：

```yaml
spec:
  securityContext:            # Pod 级
    runAsNonRoot: true
    runAsUser: 10001          # 控制面与 Tester 都是 10001
    runAsGroup: 10001
    fsGroup: 10001
    seccompProfile: { type: RuntimeDefault }
  containers:
    - securityContext:        # 容器级
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: { drop: ["ALL"] }
      volumeMounts:
        - { name: tmp, mountPath: /tmp }   # 只读根必须有可写的 /tmp + HOME=/tmp
```

**别做的事**：为了迁就 dev 去把 namespace 的 PSA 降成 `baseline`/`privileged`。降了以后 Tester Job 在 prod 会被同一档拦下，问题只是延后暴露。临时调试可以用 `kubectl run --overrides` 或另起一个 namespace。

---

## 3.1 ConfigMap 必填项（缺了集群内跑不起来）

`deploy/k8s/configmap.yaml` 里这几行**不是可选优化**，删了就会有功能直接不可用。
键名以代码实际读取的为准（`server/internal/config/config.go`、`server/internal/genet/mocks/genet.go`）。

| 键 | 值（dev） | 缺了会怎样 |
|---|---|---|
| `TESTPLANE_STORAGE_ROOT` | `/storage-root` | 控制面写不了产物（readyz 会报"存储不可写"） |
| `TESTPLANE_STORAGE_PVC` | `testplane-storage` | Tester Job 渲染失败：`invalid job spec: storage PVC is required for /workspace/output` |
| `TESTPLANE_TESTERS_DIR` | `/app/testers` | 注册表扫空 → `GET /testers` 空列表，提交直接 `TESTER_NOT_FOUND` |
| `TESTPLANE_NAMESPACE` | `testplane` | Job 落到错误的 namespace（默认值恰好也是 testplane，但别赌默认） |
| `TESTPLANE_CLUSTER_NAME` | `cluster-01` | 与启动 seed 的 clusters 行对不上 |
| `TESTPLANE_K8S_IN_CLUSTER` | `true` | Pod 内去找 kubeconfig，controller 降级（`k8s: no in-cluster config …`，Run 卡 queued） |
| `TESTPLANE_MOCK_ENDPOINT_URL` | `http://mock-llm.default.svc:18080/v1` | **deploy 形态不可用**：DeployReady 注入的 Endpoint 指向不存在的地址（默认 `mock-llm.testplane.svc:8080`），pre-check 直接 `ENDPOINT_UNREACHABLE` |
| `MODEL_NAME` / `DEPLOY_MOCK_MODEL_NAME` | `mock-llm` | 注入的 Endpoint 模型名是默认 `mock-model`，与被测服务不匹配 |
| `DEPLOY_MOCK_DURATION_SECONDS` | `3` | 部署 mock 默认 5s（能用，只是 E2E 慢一点） |

在 kind 里 deploy 形态要跑通，最少就是这三行（mock Genet，`internal/genet/mocks`，D-14）：

```yaml
TESTPLANE_MOCK_ENDPOINT_URL: "http://mock-llm.default.svc:18080/v1"
MODEL_NAME: "mock-llm"
DEPLOY_MOCK_DURATION_SECONDS: "3"
```

改完 ConfigMap 必须 `rollout restart`（原生清单没有 Helm 的 `checksum/config` 注解，
env 不会热更新）：

```powershell
kubectl --context kind-testplane apply -k deploy/overlays/with-control-plane
kubectl --context kind-testplane -n testplane rollout restart deploy/testplaned
kubectl --context kind-testplane -n testplane exec deploy/testplaned -- env | Select-String 'MOCK|MODEL|STORAGE'
```

另有两个 mock 开关，**只用来验证失败/阻塞路径**，平时不要设：
`DEPLOY_MOCK_FAIL=true`（部署失败 → Run FAILED）、`DEPLOY_MOCK_BLOCKED=true`（卡 blocked）。

---

## 4 坑：镜像灌进集群（crictl 看不到 / 拉不到）

| 现象 | 原因 | 处理 |
|---|---|---|
| `crictl images` 里没有刚 build 的镜像 | `kind load` 只把镜像导进**节点**的 containerd，不是宿主 docker | 先 `kind load docker-image <img>:<tag> --name testplane`，再 `docker exec testplane-control-plane crictl images` 确认 |
| `kind load` 报 `content digest sha256:... not found` | buildx 默认给镜像加 provenance/SBOM attestation，导出的 manifest list 里有节点侧没有的 digest | `docker build --provenance=false --sbom=false ...` 重建；或兜底 `docker save <img> \| docker exec -i testplane-control-plane ctr -n k8s.io images import -` |
| Pod `ErrImagePull` 去拉 `registry.dev.huawei.com/...` | 节点上没有那个引用 | 本地构建时**多打一个 tag**，名字等于 `tester.yaml` 里的引用（见 `deploy/README.md` 第 5.1 节）；`testers/` 不改 |
| 改了镜像重新 load，Pod 还是跑旧的 | `imagePullPolicy: IfNotPresent` + 同名同 tag，节点缓存命中 | 用带唯一 tag 的镜像（`<img>:dev-<shortsha>`），别复用 tag；或 `kind delete cluster` 重来 |
| `docker build` 找不到 `server/go.sum` 之类 | 构建上下文是**仓库根**，路径要写全 | 用 `-f deploy/docker/server.Dockerfile .`（末尾那个 `.` 是仓库根） |

### 4.1 重打镜像的标准动线（tag 不变时最容易踩）

完整五步见本文 [§0 标准重灌动线](#0-标准重灌动线改了-server-代码后唯一正确的顺序)。这里只强调最容易漏的两点：

```powershell
docker build -f deploy/docker/server.Dockerfile -t testplane-server:dev .
& .\.tools\kind.exe load docker-image testplane-server:dev --name testplane
kubectl --context kind-testplane -n testplane rollout restart deploy/testplaned
kubectl --context kind-testplane -n testplane get pods      # 看新的 ReplicaSet 起来
```

**不 restart 就等于没换**：`kind load` 只覆盖节点 containerd 里的镜像，旧 Pod 仍握着旧层。

### 4.2 `testplaned-migrate` Job 卡 Running（真实踩过）

**现象**：Job 5 分钟还没 `Completed`，`logs` 里打的却是 `testplaned 启动 … API 监听`，
说明容器跑的是**服务**而不是迁移。

**原因**：当时的镜像里 `migrate` 子命令还没实现，`/testplaned migrate up` 被当成
"无子命令 + 默认 CMD `--role=all`" 走了服务分支；服务常驻 → Job 永远不结束。

**处理**：重打镜像 → 删掉旧 Job（`kubectl -n testplane delete job testplaned-migrate`）→
重新 `apply -k deploy/overlays/with-control-plane`。正常输出：

```json
{"level":"INFO","msg":"数据库迁移完成","schema_version":1}
```

**判据**：`kubectl -n testplane get job testplaned-migrate` 的 `COMPLETIONS=1/1`，
耗时 5s 量级。跑完记得删 Job（同名 Job 不会重跑，留着只会让下次 apply 以为"已完成"）。

排查时先看**入口到底传了什么**，别看栈：

```powershell
kubectl -n testplane get pod -l app.kubernetes.io/name=testplaned-migrate `
  -o jsonpath='{.items[0].spec.containers[0].command}{"\n"}{.items[0].spec.containers[0].args}{"\n"}'
# 期望 ["/testplaned"] / ["migrate","up"]
```


---

## 5 坑：与别人共用这台机器

| 现象 | 处理 |
|---|---|
| 不知道动了谁的集群 | 所有 kubectl 命令显式 `--context kind-testplane`；别人的：`claw-fleet`、`unitest-k3s`、`docker-desktop` |
| 5432 起不来 | 别人可能占了端口；`Get-NetTCPConnection -State Listen -LocalPort 5432` 确认，换 5433 并同步 `.env.example` |
| 误删别人的容器 | 别用 `docker system prune` / `docker rm -f $(docker ps -aq)`；只按名字删 `testplane-pg`、`testplane-control-plane` |
| 8080 冲突 | 控制面在集群内监听 8080，宿主不映射；用 `kubectl port-forward`，换个本地端口（`8080:8080` 里左边那个） |

---

## 6 排障动线（从外到内）

```powershell
# 1 集群在不在
kubectl --context kind-testplane get nodes
# 2 对象状态
kubectl --context kind-testplane -n testplane get all,pvc,pv
kubectl --context kind-testplane -n testplane get events --sort-by=.lastTimestamp | Select-Object -Last 20
# 3 Pod 为什么起不来
kubectl --context kind-testplane -n testplane describe pod <pod>
kubectl --context kind-testplane -n testplane get pod <pod> -o jsonpath='{.status.containerStatuses[*].state}{"\n"}'
# 4 镜像到底在不在节点
docker exec testplane-control-plane crictl images
docker exec testplane-control-plane crictl ps -a
# 5 存储链路
kubectl --context kind-testplane apply -f deploy/k8s/storage-smoke-job.yaml   # 换 namespace 的副本也行
# 6 DB
docker exec testplane-pg pg_isready -U testplane -d testplane
docker exec testplane-pg psql -U testplane -d testplane -c '\dt'
```

---

## 7 E2E 时最常见的两个提交错误（都不在集群侧）

Run 提交成功但 `t-0001`（pre-check）秒失败、后续 Task 全 `skipped/upstream_abort` 时，
先查库里的错误，不要在 kind 上转圈：

```powershell
docker exec testplane-pg psql -U testplane -d testplane `
  -c "select id,seq,status,status_reason,error from tasks where run_id='<run_id>' order by seq;"
```

### 7.1 `executor: invalid job spec: storage PVC is required for /workspace/output`

`orchestrator` 的 `StoragePVC` 没接线。dev 里由 ConfigMap 兜：
`testplane-config` 的 `TESTPLANE_STORAGE_PVC=testplane-storage`（代码默认值相同，
但显式写出来才看得见"部署值"）。改完 ConfigMap 要 `rollout restart` 才生效。

### 7.2 `executor: invalid job spec: secret env "X" must be ENV=secretName/key or ENV=secretName:key`

提交的 manifest 里 `secret_env` **只给了变量名**，没给 Secret 映射。
执行层不猜 Secret 名与 key（E-2.1），必须写成二选一：

```yaml
secret_env:
  - "ENDPOINT_API_KEY=tp-secret-endpoint-mock/api_key"   # 或 ...=tp-secret-endpoint-mock:api_key
```

且该 Secret 要在 **Job 所在 namespace**（`testplane`）里存在：

```powershell
kubectl --context kind-testplane -n testplane create secret generic tp-secret-endpoint-mock `
  --from-literal=api_key=dummy-dev-key `
  --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
```

（dev 已建好这一份 `tp-secret-endpoint-mock`，E2E 直接用。）

---

## 8 已知偏差与待补

### 8.1 已知偏差（实现 vs 设计，等文档回写统一）

| # | 偏差 | 处置 |
|---|---|---|
| 1 | 产物 PVC 名：实现是 `testplane-storage`；06 §2 写 `testplane-storage-root`，execution §2.2 示例写 `testplane-artifacts` | 以实现为准（D-41），文档待回写；**不要**照设计文档改清单 |
| 2 | 提交体只认**嵌套** `run:` 形态；03 §4.1 的平级写法是文档缺陷 | 以 07 §4.1 / D-40 为准 |
| 3 | 开发种子 `cluster-01` + `k8s-npu` profile 由进程启动时插入（不是 Helm values） | D-42；`TESTPLANE_CLUSTER_NAME` 要和种子对得上 |
| 4 | 执行环境是宿主 kind + `kind load docker-image`，**没有镜像仓库** | D-43；部署用 digest 的事等有 registry 再说 |
| 5 | 真 RWX 用单节点 hostPath 模拟（`accessModes: ReadWriteMany` 在单节点下事实成立、跨节点无保证） | 多节点必须换 NFS CSI（06 §5.5） |

### 8.2 待补（做 P1 或写正式运维手册时补）

- Run 级报告与产物索引的验收动线（当前 `/report` 404，见 `acceptance.md` §3）。
- 取消语义（`cancelling` → Job/Pod 30s 内消失）的验证方法与判据。
- 失败注入（precheck 必失败 / 一败一成）的标准动作。
- 事件与数据库对账（`state_events` vs Pod 状态）在杀 controller 场景下的演练。
- 保留期 sweep（30 天）与日志分段（200MiB）的验证方法。
- 多节点 / 真 NFS 的切换步骤（StorageClass 替换 + PVC 重绑）。
