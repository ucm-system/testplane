# TestPlane · 本地 kind 环境运行手册（草稿，随用随补）

> 状态：**草稿**。正式版是 #11「运行手册与验收记录」，这里先攒材料——
> 每条都是真踩过的，不凭记忆补。踩到新坑直接加一节，别等最后回忆。
> 环境事实与一次性搭建步骤见 [`deploy/README.md`](../../deploy/README.md)，
> 本文只记**操作动作 + 坑 + 判据**。

---

## 0 日常起停（一台机器上反复做的事）

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

## 4 坑：镜像灌进集群（crictl 看不到 / 拉不到）

| 现象 | 原因 | 处理 |
|---|---|---|
| `crictl images` 里没有刚 build 的镜像 | `kind load` 只把镜像导进**节点**的 containerd，不是宿主 docker | 先 `kind load docker-image <img>:<tag> --name testplane`，再 `docker exec testplane-control-plane crictl images` 确认 |
| `kind load` 报 `content digest sha256:... not found` | buildx 默认给镜像加 provenance/SBOM attestation，导出的 manifest list 里有节点侧没有的 digest | `docker build --provenance=false --sbom=false ...` 重建；或兜底 `docker save <img> \| docker exec -i testplane-control-plane ctr -n k8s.io images import -` |
| Pod `ErrImagePull` 去拉 `registry.dev.huawei.com/...` | 节点上没有那个引用 | 本地构建时**多打一个 tag**，名字等于 `tester.yaml` 里的引用（见 `deploy/README.md` 第 5.1 节）；`testers/` 不改 |
| 改了镜像重新 load，Pod 还是跑旧的 | `imagePullPolicy: IfNotPresent` + 同名同 tag，节点缓存命中 | 用带唯一 tag 的镜像（`<img>:dev-<shortsha>`），别复用 tag；或 `kind delete cluster` 重来 |
| `docker build` 找不到 `server/go.sum` 之类 | 构建上下文是**仓库根**，路径要写全 | 用 `-f deploy/docker/server.Dockerfile .`（末尾那个 `.` 是仓库根） |

### 4.1 重打镜像的标准动线（tag 不变时最容易踩）

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

## 8 待补（写正式手册时补）

- 一次完整 Run 的验收动线（提交 → PREPARING → RUNNING → 终态 → 产物下载）。
- 事件与数据库对账（`state_events` vs Pod 状态）。
- 保留期 sweep 的验证方法。
- 日志分段（200MiB）的验证方法。
- 已知偏差表：PVC 名三处不一致（`testplane-storage` 实现值 / 06 §2 `testplane-storage-root` / execution §2.2 `testplane-artifacts`），等文档回写统一。
