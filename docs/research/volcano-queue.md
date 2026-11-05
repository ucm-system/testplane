# 流水线里的 Volcano 队列展示，与 TestPlane 的 `GET /api/v1/queue-status` 实现方案

> 输入（只读数据）：`D:\Vibe_Workspace\Pipline\E2E-Test.jenkinsfile`（1451 行，84KB）、同目录 `Helm-Start-Service-MB.Jenkinsfile`（3240 行，163KB）、`D:\Project\TestPalne\.data\genet`（genet 仓库克隆）。
> 结论先行：**`E2E-Test.jenkinsfile` 本身没有读取任何 Volcano 队列数据**；真正的队列展示实现在它的下游 `Helm-Start-Service-MB.Jenkinsfile`，数据来源是 **Genet 的 OpenAPI（`curl` 调用）**，Genet 后端再用 client-go dynamic 读 `scheduling.volcano.sh/v1beta1` 的 `queues` / `podgroups` 并 join vc-scheduler `/metrics`。

---

## 一句话结论

E2E 流水线展示的“排队”是 **Jenkins 队列 + 下游长驻构建排队**，与 Volcano 无关；Volcano 队列水位（capability/allocated、PodGroup 计数、阻塞 reason）只出现在下游 `Helm-Start-Service-MB` 的参数页 HTML 面板与安装期 20s 轮询里，取值路径是 `curl https://genet.dev.huawei.com/api/open/queue` 与 `/api/open/queue/podgroups`。TestPlane 应当**直连 CRD**（`dynamic.Interface` + unstructured，15s 缓存）复刻这组字段，而不是绕 Genet，也绝不沿用“队列面板写进 build description”这种展示形态。

---

## 事实清单（带 文件:行号）

### A. `E2E-Test.jenkinsfile`：没有任何 Volcano 读取（“没有”是明确结论，不是没找到）

| 事实 | 证据 |
|---|---|
| 全文没有 `kubectl`、`volcano`、`podgroup`、`capability`、`allocated`、`vcctl`、`scheduler` 调用；`kubectl` 只出现在下游文件里 | 对 E2E 文件 grep `kubectl\|volcano\|podgroup\|capability\|allocated` 命中 0 条（仅命中 `pod` 字样用于注解示例 `E2E-Test.jenkinsfile:273-276`） |
| 唯一的外部数据接口是 Genet 的**镜像**接口，与队列无关 | `E2E-Test.jenkinsfile:16` `def apiBase = "https://genet.dev.huawei.com"`；`:39` `GET /api/open/registry/tree?limit=1000`；`:166` `GET /api/open/registry/tags?image=…` |
| 它说的“排队”是 **Jenkins executor / 下游构建排队** | `:400-402` “下游可能排队（最长 3 天）+ 安装（2h）”；`:404` “不包含排队等待（排队时构建对象尚未出现，不会计入）”；`:972` “重试构建还在排队（尚未出现在 getBuilds()）”；`:1077` “正在定位下游 Start-Service 构建（仍在 Jenkins 队列/未进入扫描窗口）” |
| 它甚至把 Volcano 队列面板当成**噪声**：读下游 description 时专门防御 CSS 污染 | `:1019` “下游归档的 access-info.json artifact（稳定，不被队列面板/CREATE 文本覆盖）”；`:1219-1220` “非 JSON（CREATE 文本/队列面板 CSS）解析失败即安全返回 null” |
| E2E 自己写 build description 只有两类内容：等待计时行 + 最终服务 JSON，没有队列 | `:1078` `currentBuild.description = waiting`；`:1094-1095` `启动 Helm 服务中，实时同步下游 <a …>`；`:1127` `Helm 服务已就绪<br><pre>${prettyJson}</pre>` |
| 另一处“排队/占卡”是提交前防呆提示，不是状态展示 | `:489` “用 stack 配置去拉 PD 会失败，还白白排队/占卡” |

**所以 E2E 展示的是哪一类“排队”？** 只有两类，都不来自 Volcano：
1. **Jenkins 队列**（下游 job 尚未分配 executor / 构建对象尚未出现）；
2. **下游长驻构建排队**（Helm-Start-Service-MB 里等 Volcano 调度，见 B 段）。

### B. 真正的 Volcano 队列展示：`Helm-Start-Service-MB.Jenkinsfile`

| 事实 | 证据 |
|---|---|
| 队列由 Genet + Volcano 实现，流水线“只查状态” | `Helm-Start-Service-MB.Jenkinsfile:22-23` “queueing 由 Genet + Volcano 实现，流水线只「查状态」”；`:1786` 同义重复 |
| 排队预算独立于安装预算：7 天 | `:26` `@Field int QUEUE_TIMEOUT_SEC = 604800   // 排队最长 7 天`；`:849` `stage('排队等待 / Queue')` |
| **数据来源 1：Genet OpenAPI `GET /api/open/queue`**（表单页面板 + 安装期轮询都调它） | `:575` `def r1 = httpGet("${apiBase}/api/open/queue")`；`:1841` `apiCall('GET', '/api/open/queue', null, false, 15)`；`:2203` `apiCall('GET', '/api/open/queue', null, false, 15)` |
| **数据来源 2：Genet OpenAPI `GET /api/open/queue/podgroups`**（本 API Key 命名空间的 PodGroup） | `:606` `def r2 = httpGet("${apiBase}/api/open/queue/podgroups")`；`:2249` `apiCall('GET', '/api/open/queue/podgroups', null, false, 15)` |
| 调用方式原样：`curl -k -sS` + Bearer 凭据 `genet-api-key`，用 `-w "###HTTP###%{http_code}"` 拆状态码 | `:553-570`（`["curl","-k","-sS","--connect-timeout","5","--max-time","12","-w", marker + "%{http_code}","-H","Authorization: Bearer ${cred.secret.plainText}","-H","accept: application/json", url]`）；凭据读取 `:554-556` |
| 没有 kubectl 查队列；kubectl 只用于 `kubectl logs -f` 跟容器日志 | `:14`、`:2352-2358`（`kubectl logs` 相关）；队列两处全是上面两个 HTTP 接口 |
| 不是 Jenkins 插件：是 `DynamicReferenceParameter` + `ET_FORMATTED_HTML` + 自定义 Groovy 脚本 | `:700-708`（`choiceType: 'ET_FORMATTED_HTML'`、`omitValueField: true`、`script: [$class:'GroovyScript', …]`） |
| 刷新触发器是内部隐藏参数 `QUEUE_REFRESH`，reactive 到 `PACKAGE_TYPE,QUEUE_REFRESH`；面板按钮写时间戳触发 change | `:698` `string(name:'QUEUE_REFRESH', defaultValue:'0', …)`；`:704` `referencedParameters: 'PACKAGE_TYPE,QUEUE_REFRESH'`；`:677-683` `window.qRefresh` |
| 兜底脚本：整块面板失败时给一句人话，不阻塞提交 | `:689-691` `queueStatusFallbackScript` |

### C. 展示字段（Jenkins 侧实际渲染的）

| 字段（面板/日志里的名字） | 来源 JSON 路径 | 证据 |
|---|---|---|
| 队列名 `Volcano 队列 <name>` | `queue.name` | `:625`；`:2279` |
| 池类型 + 队列状态 `(<poolType> · <state>)` | `queue.poolType`、`queue.state` | `:625`；`:2279` |
| 超用标记 `超用` | `queue.overused == true` | `:604`；`:2278` |
| 加速卡 已用 / 容量（多卡型用 `·` 分隔） | `queue.allocated[k]` / `queue.physicalCapacity[k]`（缺则 `queue.capability[k]`） | `:589-600`；`:2316-2332`；`:2280` |
| 队列 PodGroup 计数 `运行 / inqueue / pending` | `queue.running`、`queue.inqueue`、`queue.pending` | `:627`；`:2224`；`:2281` |
| 本命名空间排队中（PodGroup 计数） | `/api/open/queue/podgroups` 里 `phase ∈ {Inqueue, Pending}` 的条数 | `:606-621`；`:2253-2261`；`:628`、`:2282` |
| 阻塞原因 reason | 同一 PodGroup 的 `conditions[-1].message`（缺则 `.reason`） | `:615-618`；`:2262-2271`；`:624`、`:2284` |
| 已等时长 / 上限 | Jenkins 侧计时（`elapsed`/`timeoutSeconds`），**不是** Volcano 数据 | `:1836`、`:1863` |
| 本 release 未调度 Pod 数 | Jenkins 侧（`listReleasePodsBestEffort` 中 `nodeName` 为空的 Pod） | `:1818-1824`、`:1863` |
| 未展示但接口里有 | `deserved`、`request`、`realCapability`、`share`、`weight`、`reclaimable`、`guarantee`、`nodeGroups` | `…/models/volcano_views.go:13-39`（字段定义），Jenkins 面板未渲染 |

### D. 展示形态与刷新时机

| 形态 | 位置 | 刷新时机 |
|---|---|---|
| **HTML 面板**（参数页，“队列资源状态（提交前查看）” + 🔄 刷新按钮），`div.q-box` 内 4-6 行 | `:632-686`（含 `.q-*` 内联 CSS `:633-645`） | 打开参数页渲染一次；`PACKAGE_TYPE` 变更或点 🔄（写 `QUEUE_REFRESH`）→ 整个 Groovy 脚本重跑（`:676-683`、`:704`） |
| **控制台文本行**（排队期每 30s 一轮，拼接式单行） | `:1826-1827`、`:1836-1863` | 30s 轮询（`:1789` `int interval = 30`），排队期每轮都 echo |
| **构建 description 的 HTML 面板**（安装等待窗口） | `:2225`、`:2277-2287` | 20s 轮询（`:2193`），且**内容变化才写**（`:2227-2231`） |
| 安装期控制台摘要 | `:2224` `[队列 <name>] <卡 已用/容量> | 队列 运行/inqueue/pending | 本作业 运行/排队` | 同上，`summary != lastLine` 才打印 |

**两个展示前置条件（容易踩坑，值得抄进 TestPlane 的验收项）**：
- description 里的 HTML 只有 Jenkins 全局 Markup Formatter 设为 **Safe HTML** 才渲染，否则纯文本；控制台行不受影响 —— `:2189`。
- 就绪后 description 必须让位给访问方式 JSON（下游 `jenkins-helm.sh` 用 `jq` 解析它），所以队列面板**只在安装等待窗口存活**，`shared.ready` 后必须停写 —— `:2190-2191`、`:2196-2200`。

### E. 数据链路的“最底层”：Genet 后端怎么读 Volcano

| 事实 | 证据 |
|---|---|
| `GET /api/open/queue` = 当前 API Key 所绑池的那一个队列；池由 key 固定，调用方不能选池 | `backend/internal/handlers/openapi_queue.go:11-13`、`:33`（`QueueNameForPool(runtimeCtx.PoolType)`） |
| 未启用 Volcano → `enabled:false`；队列不存在 → `queue:null` | `openapi_queue.go:20-23`；`:41-46` |
| `GET /api/open/queue/podgroups` 只回本 Key 命名空间的 PodGroup（防跨租户） | `openapi_queue.go:51-53`、`:78-82` |
| 组装视图 = CRD 骨架 + vc-scheduler `/metrics` + 各池物理容量 | `backend/internal/handlers/queue.go:46-84`（三步注释）、`:88-107`（`mergeQueueMetrics` 用 metric 覆盖 `allocated`） |
| **client-go 方式：`dynamic.Interface` + unstructured，故意不引 Volcano Go 依赖** | `backend/internal/k8s/volcano.go:32-43`（注释与常量：`scheduling.volcano.sh/v1beta1`）、`:45-48`（两个 GVR）、`:184-185`（`dynamicClient == nil` 报错） |
| Queue 是 cluster-scoped，PodGroup 是 namespaced | `volcano.go:33-35`；`:255`（`Resource(volcanoQueueGVR).List(ctx, …)` 不带 namespace）；`:275`、`:295` |
| Queue 字段读取路径（逐条） | `volcano.go:317-341`：`spec.weight`、`spec.reclaimable`、`spec.guarantee.resource`、`spec.capability`、`spec.affinity.nodeGroupAffinity.…`、`status.state`、`status.allocated`、`status.running`、`status.pending`、`status.inqueue`、`status.unknown`、`status.completed`；label `genet.io/managed`、`genet.io/pool-type`（`:551-553`） |
| PodGroup 字段读取路径（逐条） | `volcano.go:356-377`：`spec.queue`、`spec.minMember`、`status.phase`、`status.conditions[].{type,status,reason,message}`（注意：**不读** `status.running`） |
| 三类数据来源的权威性说明（spec=静态、status=控制器回写有滞后、metric=调度器内存现算） | `backend/internal/models/volcano_views.go:6-12` |
| `physicalCapacity` 是 Genet **自算**的池物理量，不是 Volcano 字段；理由：队列层面未给 CPU/内存配额时会被 Volcano 兜底成 `MaxFloat64` | `volcano_views.go:24-26` |
| 失败语义：CRD list 失败 → 500 `获取 Volcano 队列失败`（**整体失败**，未降级）；仅 metrics 失败降级 | `openapi_queue.go:26-31`；`queue.go:57-69`（`metricsAvailable=false`） |
| 权限（RBAC） | `helm/genet/templates/rbac.yaml:75-78` `scheduling.volcano.sh` `queues`,`queues/status`：`get,list,watch,create,update,patch`；`:80-83` `podgroups`,`podgroups/status`：`get,list,watch,create,update,delete,patch`（含写，因为要 ensure/reconcile） |
| 队列拓扑 config 驱动：每池一个 Queue CRD，`genet-` 前缀，不用 `default` 队列 | `helm/genet/values.yaml:66-118`（`volcano.enabled: true`、`queuePrefix: "genet-"`、`defaultQueue: "genet-shared"`、`manageQueues: true`、`autoCapacity: true`、`queues: shared/exclusive/baremetal`） |

**注意一个与 TestPlane 规格的差异**：Jenkins 面板的“加速卡 已用/容量”分母取 `physicalCapacity`（池节点真实容量）优先、`capability` 兜底（`:598`、`:2328`）。而 TestPlane 的 openapi 只定义了 `capability`（`server/api/openapi.yaml:3342-3345`，注明“`spec.capability` 原文，平台不换算单位”）。直接照抄 `capability` 当分母，会在“队列没设 CPU/内存配额”的场景显示成天文数字（见 `volcano_views.go:24-26`）。这是实现时必须拍板的一点（见末节 U-4）。

### F. TestPlane 现状（实现落点）

| 事实 | 证据 |
|---|---|
| 端点与三层结构已定稿：`slots` / `waiting` / `volcano` | `server/api/openapi.yaml:1116-1151`（§ 描述三层）、`:3153-3170`（`QueueStatus`） |
| `VolcanoSnapshot` 只有 `available` 必填，`detail`、`queues[]`；明确“不可达只降级本块” | `openapi.yaml:3310-3327` |
| `VolcanoQueue` 字段：`name`(必填)/`state`/`capability`/`allocated`/`pending_jobs`/`longest_wait_seconds`/`reason` | `openapi.yaml:3329-3360` |
| 六值 `waiting_reason`，前三个属平台、后三个属 K8s/Volcano，**必须分开呈现** | `openapi.yaml:3241-3278`；`design/impl/control-plane.md:463-474` |
| 判定顺序与数据来源已定：`slot_waiting → queue_full → gang_pending → insufficient_resources`，判不了落 `detail` 不新增枚举 | `design/impl/execution.md:424-431` |
| E-11 规格：dynamic + unstructured、只读、**缓存 15s**、单查 5s 超时、不可达返回 `scheduler_unavailable` | `design/impl/execution.md:411-422` |
| 现状代码：`handleQueueStatus` 已实现 slots/waiting，Volcano 块是硬编码 `available:false / detail:"scheduler_unavailable" / queues:[]` | `server/internal/api/meta.go:156-266`（火山块 `:260-264`） |
| 已声明能力位 `queue_status: true` | `server/internal/api/meta.go:81` |
| UI 已有三分之一答案：`Volcano 队列` 卡与 `平台槽位/排队中的 Task` 卡**本来就是分开的两张卡**，不可达时只灰本卡 | `web/src/pages/ClustersPage.tsx:159-206`（Volcano 卡）、`:165-170`（降级只影响本块）、`:203-205`（footer：只读不插队） |
| 物理视图已规划只读 ClusterRole `testplane-view`：`nodes` + Volcano `queues`/`podgroups`/`jobs` | `design/views/06-physical-view.md:55` |
| 现有 `deploy/k8s/rbac-cluster-executor.yaml` **没有** Volcano 规则（只有 batch/jobs、pods、pvc、events、leases） | `deploy/k8s/rbac-cluster-executor.yaml:20-38` |
| client-go 已在依赖里（v0.31.3），无需新增模块 | `server/go.mod:11` |

---

## 字段与映射表（CRD → `QueueStatus.volcano.queues[]`）

### 1. 队列级（`scheduling.volcano.sh/v1beta1, resource=queues`，cluster-scoped）

| 目标字段 | CRD 路径 | 读取方式 | 说明 / 坑 |
|---|---|---|---|
| `name` | `metadata.name` | `u.GetName()` | 本项目里形如 `genet-shared` / `genet-exclusive` / `genet-baremetal`（`helm/genet/values.yaml:106-114`） |
| `state` | `.status.state` | `unstructured.NestedString` | `Open` / `Closed`（genet 注释还列了 `Closing`，见 `volcano_views.go:15`）；`Closed` ⇒ 队列内排队应判 `queue_full`（`execution.md:427`） |
| `capability` | `.spec.capability` | `NestedMap` → `map[string]string` | 资源名 → 数量**字符串**（`nvidia.com/gpu`、`huawei.com/Ascend910`、`cpu`、`memory`），平台不换算单位（openapi `:3342-3345`）。⚠ CPU/内存未设配额时 Volcano 会兜底成极大值（`volcano_views.go:24-26`），**不能直接当进度条分母** |
| `allocated` | `.status.allocated` | `NestedMap` → `map[string]string` | 控制器回写，**有滞后**；Genet 会优先用 vc-scheduler `/metrics` 覆盖（`handlers/queue.go:98-100`）。TestPlane 无 Prometheus 时就只能用它，需在 `detail` 注明来源 |
| `pending_jobs` | 两条等价路：① 该 queue 下 `podgroups[].status.phase == "Pending"` 的计数；② 快捷值 `.status.pending` | ① 需一次全量 PodGroup List（反正要为 `longest_wait_seconds` 做）；② 单次 Queue List 即可 | 规格定义为①（`openapi.yaml:3350-3353`、`execution.md:422`）。`status.pending` 是 Volcano 自己维护的计数，语义可能含 `Inqueue`，**不要混用**——若要分列，另外加 `inqueue`（`.status.inqueue`） |
| `longest_wait_seconds` | `now - min(podgroup.metadata.creationTimestamp)`，限定同 queue 且 `phase ∈ {Pending, Inqueue}` | 需要 `creationTimestamp`（unstructured `GetCreationTimestamp()`） | `openapi.yaml:3354-3357`。没有 Pending/Inqueue ⇒ 0（**不是**负数，也不要回退成队列创建时间） |
| `reason` | 该 queue 下 `phase ∈ {Pending, Inqueue}` 的 PodGroup 里，取**最新 condition** 的 `message`（空则 `reason`）→ 归一写成 `WaitingReason` 六值之一 | `status.conditions[-1].{type,status,reason,message}` | Jenkins 的取法就是 `conds[-1].message ?: conds[-1].reason`（`Helm-Start-Service-MB.Jenkinsfile:617`、`:2266`）。⚠ 这是**自由文本**，必须映射回枚举：命中“queue … is not open/closed” ⇒ `queue_full`；“insufficient (cpu|memory|…)” ⇒ `insufficient_resources`；“gated by gang / minMember” ⇒ `gang_pending`；都判不了 ⇒ `null` + 原文进 `detail`（`execution.md:431`） |

### 2. PodGroup 级（`…, resource=podgroups`，namespaced）

| 字段 | CRD 路径 | 用途 |
|---|---|---|
| `spec.queue` | `.spec.queue` | 归组到哪个 Queue（与 Queue 名关联的唯一键） |
| `spec.minMember` | `.spec.minMember` | gang 是否满足：`minMember > status.running` ⇒ `gang_pending`（`execution.md:422`、`:429`） |
| `status.phase` | `.status.phase` | `Pending` / `Inqueue` / `Running` / `Unknown` / `Completed`（`volcano_views.go:54`）；`pending_jobs` 与 `longest_wait_seconds` 的过滤条件 |
| `status.running` | `.status.running` | 规格用它判 gang（`execution.md:417`、`:422`）。⚠ **Genet 没读这个字段**（`k8s/volcano.go:356-377` 只读 phase/conditions），实际是否存在需在目标集群验证（见 U-7） |
| `status.conditions[]` | `.type` / `.status` / `.reason` / `.message` | `reason` 的唯一来源（Volcano 说的“为什么没调度”） |
| `metadata.creationTimestamp` | — | `longest_wait_seconds` 的 `min()` 输入；Jenkins 侧没展示，TestPlane 规格要求它 |

### 3. 反向映射：`WaitingReason` 六值从哪来（供 `waiting.by_reason` 与 `volcano.queues[].reason` 共用）

| 值 | 归属 | 判定输入 | 证据 |
|---|---|---|---|
| `precheck_gate` | 平台 | Task `PENDING` 等 Pre-Check | `control-plane.md:469` |
| `slot_waiting` | 平台 | `endpoint_leases` 有他人（独占/被独占） | `control-plane.md:470`、`:417-432` |
| `global_limit` | 平台 | 端点空闲但 `attempts(status in preparing,running) >= TESTPLANE_CONCURRENCY_MAX` | `control-plane.md:471`、`:436-443` |
| `queue_full` | K8s/Volcano | `queue.status.state = Closed`，或 `allocated + 本次请求 > deserved` 且队列内 Pending > 0 | `execution.md:427` |
| `insufficient_resources` | K8s | Pod `Unschedulable` 且 message 命中 `insufficient (cpu\|memory\|nvidia.com/gpu\|huawei.com/Ascend910)` | `execution.md:428` |
| `gang_pending` | K8s/Volcano | PodGroup `Pending` 且 `minMember > running`，或事件含 `gated by gang` | `execution.md:429` |

> 平台槽位（前三）**全部可从 DB 得出**（`meta.go:158-245` 已实现）；后三个必须来自上面两张 CRD 表（外加 Pod 的 `Unschedulable` condition）。这是“同一端点、两套数据源”的根因。

---

## client-go 读取要点（TestPlane 实现）

### 1. 客户端与 GVK

```go
// 只读，cluster-scoped queues + namespaced podgroups；不引 volcano.sh/apis（与 genet 同策略：
// 避免被 Volcano 版本绑死；参考 .data/genet/backend/internal/k8s/volcano.go:32-48）
const volcanoGroup = "scheduling.volcano.sh"

var (
    volcanoQueueGVR    = schema.GroupVersionResource{Group: volcanoGroup, Version: "v1beta1", Resource: "queues"}
    volcanoPodGroupGVR = schema.GroupVersionResource{Group: volcanoGroup, Version: "v1beta1", Resource: "podgroups"}
)

dyn, err := dynamic.NewForConfig(cfg)          // cfg 来自 in-cluster 或 kubeconfig（多集群）
```

- **不要**用 `volcano.sh/apis` typed client：它会拖 `k8s.io/*` 版本（genet 明确写下了这条理由），而 TestPlane 锁的是 `client-go v0.31.3`（`server/go.mod:11`）。
- 每个集群一个 `dynamic.Interface`，与现有 `server/internal/k8s/client.go` 的 `kubernetes.Clientset` 并列复用同一份 `rest.Config`（避免重复认证/限流器）。

### 2. informer 还是轮询？—— **轮询 + 进程内 TTL 缓存**，不要 informer

理由（按重要性）：

1. 规格已经钉死：“缓存 15s、单查 5s 超时”（`execution.md:412`，`openapi.yaml:3164`）。informer 的语义是 watch 实时，与 15s 新鲜度上限不匹配，反而要额外加一层限流。
2. 规模小且**不需要 watch 事件**：Queue 是 3 个；PodGroup 是本集群所有测试命名空间的量级（数十~数百），一次 `List` 足够。
3. 多集群时 informer 要为每个集群常驻一组 reflector + 内存 store，且不可达集群会不断重连（降级语义要求“不可达就给 `scheduler_unavailable`”，轮询天然满足）。
4. RBAC 更小：轮询只需 `get,list`，不必给 `watch`（见下节）。genet 给 `watch` 是因为它要 reconcile，我们只读。

实现形状：

```go
type volcanoCache struct {
    mu       sync.Mutex
    at       time.Time
    snapshot VolcanoSnapshot   // available/detail/queues
    ttl      time.Duration     // 15s
    inflight chan struct{}     // 单飞：并发请求最多一个在打 API
}

func (c *volcanoCache) Get(ctx context.Context) VolcanoSnapshot {
    // 1) 命中 TTL → 直接返回（含 last-known，可带 stale 标记）
    // 2) 未命中 → 单飞刷新；ctx 上加 5s 超时（execution §11）
    // 3) 刷新失败 → 返回 available:false + detail，绝不让 /queue-status 500
}
```

- **单飞（single-flight）**：`/queue-status` 是 30s 轮询的公共端点（`openapi.yaml:1122`），看板一开就有多客户端同时打；不单飞会出现 N 倍 `List podgroups`。
- **超时必须短于端点超时**：单查 5s（规格），整体 `/queue-status` 建议 3s 内返回（火山块失败即降级，不等）。
- **两个 List 的顺序**：先 `queues`（小、稳定），再 `podgroups`（大）；后者失败**不应**把前者作废——但为了简单，`available:false` 时 `queues:[]` 更符合 openapi 的“不可达时为 `[]`”（`:3325`）。若要“半可用”，把 `detail` 写明 `podgroups_unavailable` 并保留 queues（`additionalProperties: true` 允许，但需先在规格里补一句，见 U-6）。

### 3. 解析（unstructured，逐字段容错）

```go
v, _, _ := unstructured.NestedString(u.Object, "status", "state")
alloc, _, _ := unstructured.NestedMap(u.Object, "status", "allocated")   // map[string]any → fmt.Sprintf("%v")
capb, _, _  := unstructured.NestedMap(u.Object, "spec", "capability")
pend, _, _  := unstructured.NestedInt64(u.Object, "status", "pending")   // 快捷值，仅作交叉校验
ph, _, _    := unstructured.NestedString(pg.Object, "status", "phase")
mm, _, _    := unstructured.NestedInt64(pg.Object, "spec", "minMember")
q, _, _     := unstructured.NestedString(pg.Object, "spec", "queue")
```

- 数字一律当**字符串**透传进 `capability`/`allocated`，不要在平台侧做单位换算或 `resource.Quantity` 归一（`openapi.yaml:3345` 明确“原文”）。
- 缺字段不是错误：`NestedXxx` 的 `found=false` 时留 `null`/`0`；**不要**用 `0` 冒充“未知”（`pending_jobs` 用 0 是对的，`state` 缺就应是 `null`）。
- `conditions` 要遍历 slice 再取最后一条（`[]interface{}` → `map[string]interface{}`），参考 `k8s/volcano.go:362-375`。

### 4. 失败降级（四条路径，全部只影响 `volcano` 块）

| 情形 | 检测 | 输出 |
|---|---|---|
| CRD 未安装 | `discovery.ServerResourcesForGroupVersion("scheduling.volcano.sh/v1beta1")` 返回 404（结果可缓存 5 分钟，别每次都 discovery） | `available:false, detail:"crd_missing", queues:[]` |
| 无权限 | List 返回 `apierrors.IsForbidden` | `available:false, detail:"rbac_forbidden"`（**不要**在 detail 里吐原始 RBAC 报错全文，避免泄露 SA 名/角色名） |
| 超时/不可达 | `context.DeadlineExceeded` / 连接错误 | `available:false, detail:"scheduler_unavailable"`，可保留上次快照并加 `stale:true`（`design/views/04-process-view.md:32` 要求“保留上次快照并标 stale”） |
| 部分字段判不了（如 reason 映射不上、没有 metrics 无法算 deserved） | 值为空 | 字段给 `null`，`detail` 里写 `reason_unclassified` / `metrics_unavailable`；**不新增枚举值**（`execution.md:431`） |

硬规则：`volcano` 块任何失败都**不得**影响 `slots` / `waiting`，也不得让 HTTP 非 200（openapi 只约定 200/401/403/500，500 留给真正的平台故障）。

### 5. 测试

- 用 `k8s.io/client-go/dynamic/fake` 构造假 CRD 列表（genet 的做法可参考 `handlers/openapi_queue_test.go:45-60`、`:188-260`），断言：正常映射、`queue=null`、`enabled=false`、PodGroup 为空、条件缺失、`reason` 无法归类。
- 加一条“CRD 不存在”用例：fake discovery 返回空列表 → `crd_missing`。
- 加一条竞态用例：并发 50 个 `Get` 只触发 1 次 List（单飞）。

---

## RBAC（最小集）

```yaml
# ClusterRole：testplane-view（与 design/views/06 §2 的规划同名）
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: testplane-view
rules:
  # Volcano Queue 是 cluster-scoped，只能 ClusterRole；只读
  - apiGroups: ["scheduling.volcano.sh"]
    resources: ["queues"]
    verbs: ["get", "list"]
  # PodGroup 是 namespaced；要跨命名空间出总览就得 cluster 级 list
  - apiGroups: ["scheduling.volcano.sh"]
    resources: ["podgroups"]
    verbs: ["get", "list"]
  # 仅当实现 insufficient_resources（Pod Unschedulable）与 deserved 分母时才要
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list"]
```

要点：

1. **不要**授予 `queues/status` / `podgroups/status` 的写权限；读 `status` 随 `get`/`list` 主资源一起返回，不需要额外 subresource 规则。genet 给写是因为它 `ensure`/`reconcile`（`rbac.yaml:75-83`），TestPlane 只读（`design/views/06-physical-view.md:163` 明确“Volcano 写”是禁止项）。
2. **不要**给 `watch`——轮询实现不需要；给了等于允许无限长连接，反而扩大权限面。
3. 现有 `deploy/k8s/rbac.yaml` 的 namespace `Role` **绑不到** cluster-scoped `queues`，必须新增 ClusterRole + ClusterRoleBinding（参考 `rbac-cluster-executor.yaml:39-54` 的绑定写法）。
4. 多集群：每个被管集群的 SA 都要这组权限；TestPlane 侧以 `clusters` 表为单位做可用性判定（现在 `server/internal/api/meta.go` 的 cluster DTO `:269-279` 已有 `last_check_result`，可复用为“Volcano 可见性”探针）。
5. discovery 本身不需要 RBAC（`system:discovery` 默认绑定），但若集群收紧了 `system:discovery`，`crd_missing` 会误报成 `rbac_forbidden`——用 error 的 `IsForbidden` / `IsNotFound` 分开判。

---

## 降级语义（多集群 / Volcano 未安装）

| 场景 | `volcano` 块 | 其他两块 | UI |
|---|---|---|---|
| Volcano 未安装（常见：纯 K8s 执行） | `{available:false, detail:"crd_missing", queues:[]}` | 照常 | 只灰“Volcano 队列”卡，文案“该集群未安装 Volcano，平台槽位与排队原因不受影响”（对齐 `ClustersPage.tsx:165-170` 现有语气） |
| 有 CRD 但 `volcano.enabled=false`（Genet 语义） | `{available:false, detail:"disabled", queues:[]}` | 照常 | 同上，文案换成“平台未启用队列调度”（对齐 Jenkins `:584`） |
| 队列存在但 PodGroup 为空 | `available:true`，`queues[]` 里 `pending_jobs:0`、`longest_wait_seconds:0`、`reason:null` | 照常 | 显示 `—`，**不要**显示“0 个排队”这种绝对断言 |
| 有队列名但 CRD 对象没被 ensure | `available:true`，`queues:[{name:"genet-shared", state:null, …}]` 还是干脆不列？ | — | **建议：不列不存在的队列**（Jenkins 会显示“暂未创建”，但那是因为它只知道配置；TestPlane 列事实即可），`detail` 里说明期望队列缺失 |
| 多集群，部分不可达 | 单对象装不下多集群 ⇒ 现在会互相覆盖 | 照常 | 见 U-6：要么 `queues[].name = "<cluster>/<queue>"`，要么加 `cluster` 字段（`additionalProperties:true` 允许，但要写进 openapi 才算契约） |
| 首次加载 / TTL 过期且刷新失败 | 返回 `stale` 快照 + `detail:"scheduler_unavailable"`，`available:false` | 照常 | 灰卡 + “上次更新 HH:MM” |
| `waiting_reason` 判不了 | Task 保持上一个已知 reason，`detail` 记原因 | — | 绝不把 `unknown` 当第七个枚举值（`execution.md:431`） |

**降级的红线**：队列不可达 **不等于** 没有排队。任何“不可达 → 显示 0 排队 / 显示空闲”的默认值都是误导（这正是 Jenkins 在 `:586`、`:2215` 都要显式区分“暂未创建”的原因）。

---

## 为什么“平台槽位”和“Volcano 队列”必须在 UI 上分开

1. **归责对象不同，处理动作完全相反**。`slot_waiting` / `global_limit` / `precheck_gate` 的解法在 TestPlane 内（等前一个独占 Task 结束、调大 `TESTPLANE_CONCURRENCY_MAX`、看 Pre-Check）；`queue_full` / `gang_pending` / `insufficient_resources` 的解法在集群侧（找管理员扩卡、开队列、调 `minMember`）。合成一个数字，用户只能猜该找谁——这恰是 `control-plane.md:474` 与 `openapi.yaml:1129-1130` 反复强调“分开呈现”的原因。
2. **时间尺度差 1~2 个数量级**。平台槽位是秒~分钟级（Task 结束即释放，`control-plane.md:432`）；Volcano 队列是小时~天级（Jenkins 直接把排队上限设成 **7 天**，`Helm-Start-Service-MB.Jenkinsfile:26`）。同一个 ETA 口径套两者必错。
3. **单位与分母不同**。平台是“并发 permit 计数”（默认 20，`control-plane.md:440`）；Volcano 是“资源配额”（卡/CPU/内存的 `capability` 与 `allocated`）。把 20 与 “96 卡”放同一个进度条或做加法，是纯粹的数值错误。
4. **可用性不同**。平台槽位永远可读（DB）；Volcano 会 `scheduler_unavailable`（`execution.md:412`）。合并后，一次 CRD 读失败会让整块“排队”变成不可知，或者更糟——被渲染成“没有排队”。
5. **典型误导场景（必须能一眼看出）**：
   - 队列 `Open` 且有 Pending，但本 Task 其实是卡在端点独占 → 合并后显示“在等队列”，用户去扩队列，实际只要等前一个 Task 结束；
   - 平台 permit 已满但 Volcano 队列空闲 → 合并后显示“集群资源不足”，用户去申请机器，实际是自己把并发打满了；
   - `precheck_gate` 是“自己的流水线没走完”，最不该和“集群没卡”混为一谈。
6. **最小可行 UI**（现成实现基本满足，只需把数据填上）：
   - 两张卡并列、标题带归属：`平台槽位（TestPlane）` / `Volcano 队列（只读快照，15s）`——`ClustersPage.tsx:159-206`；
   - 计数**不可相加**，页面上不要出现“总排队 N”；
   - `waiting` 明细逐条带 `waiting_reason`（`openapi.yaml:3280-3308`），并在 tag 上区分“平台侧/集群侧”两组颜色；
   - Volcano 卡 footer 固定一句“平台只读调度器状态，不插队、不改配额”（`:203-205` 已有）。

---

## 未确认项

| 编号 | 未确认的事 | 影响 | 建议默认 |
|---|---|---|---|
| **U-1** | 是否存在第三个流水线（如 Create-Release / Daily-Build）也往 description 写队列面板；本次只读了 E2E 与 Helm-Start-Service-MB 两个文件 | 只影响“还有没有别的展示形态”这一判断，不影响数据源结论 | 以 Helm-Start-Service-MB 为唯一队列读取点（`/api/open/queue` 全库仅此两处调用，:575/:606/:1841/:2203/:2249） |
| **U-2** | 生产集群 Volcano 具体版本（只确认 API 组为 `v1beta1`） | `status.pending/inqueue/unknown/completed`、`queues/status` 子资源在各版本是否一致 | 实现按 `v1beta1` 字段全容错读取；发布前在目标集群 `kubectl get crd queues.scheduling.volcano.sh -o yaml` 核对 |
| **U-3** | `Queue.status.state` 是否真会出现 `Closing`（genet 注释列了，`volcano_views.go:15`） | 状态徽章只有 Open/Closed 两态（`ClustersPage.tsx:190`） | 未知值原样透传为字符串，UI 用 `muted` 徽章兜底，不硬映射成 Closed |
| **U-4** | `capability` 当分母是否可用：队列未设 CPU/内存配额时会被兜底成 `MaxFloat64`（`volcano_views.go:24-26`）；Jenkins 因此改用 `physicalCapacity`（`:598`、`:2328`） | 直接影响 UI 进度条是否会显示天文数字 | **建议**：`capability` 照抄进 openapi 字段，但 UI/判定另取“加速卡分母” = `capability[accKey]` 与 `sum(node.status.allocatable[accKey])` 的较小值；后者需要 `nodes` 权限。需先拍板 |
| **U-5** | 是否引入 vc-scheduler `/metrics`（`deserved` / `realCapability` / `overused` / `share`）。`queue_full` 的规格判定含 `allocated + 请求 > deserved`，而 `deserved` **只存在于调度器内存指标**，CRD 里没有（`volcano_views.go:6-12`） | 不接 Prometheus 就只能在 `state=Closed` 时判 `queue_full`，判定覆盖率下降 | **建议 MVP 不接**：`queue_full` 仅由 `state=Closed`（+ `pending>0`）触发，`detail` 写 `deserved_unavailable`；P1 再评估拉 Prometheus。当前 `server/go.mod` 无 prometheus 依赖（`:1-60` 未见） |
| **U-6** | 多集群如何塞进单个 `volcano` 对象（openapi 无 `cluster` 字段，`additionalProperties:true` 可加但未成契约） | 多集群下队列会互相覆盖，或只能表达一个集群 | **建议**：`queues[].name` 用 `"<cluster>/<queue>"` 前缀（零契约变更，UI 已按字符串渲染 `ClustersPage.tsx:188`）；若要多集群并列展示再补 `cluster` 字段并同步改 openapi + `web/src/api/types.ts:432-437` |
| **U-7** | `PodGroup.status.running` 是否可靠存在：规格用它判 gang（`execution.md:417`、`:422`），但 genet 的实现**没读**这个字段（`k8s/volcano.go:356-377` 只读 `phase`/`conditions`） | `gang_pending` 判定可用性 | 实现时**双路判**：`minMember > status.running`（若字段存在），否则退回 `phase==Pending && conditions 含 gang 关键词`；字段缺失时不要判成“满足” |
| **U-8** | `pending_jobs` 口径：openapi 定义为“`PodGroup.phase=Pending` 的数量”（`:3353`），而 Volcano 的 `status.pending` 是它自己的计数，可能含 `Inqueue` | 数字对不上会被当 bug | 严格按 openapi 用 PodGroup 计数；额外把 `status.pending`/`status.inqueue` 放进 `additionalProperties` 便于对账（不显示在默认列） |
| **U-9** | `waiting_reason` 由谁写：`queue_full`/`gang_pending` 需要“Task ↔ PodGroup ↔ Queue”的关联（Task → Job → Pod → PodGroup）。现有代码里没有这条链路（`server/internal/orchestrator` 只处理 Attempt/Job） | 决定 Volcano 块能不能真正反哺 `waiting.by_reason`（否则只是只读展示） | 若短期内做不通关联：`volcano` 块照做（展示层），`waiting.by_reason` 的 K8s 三项先保持 0 并在 `detail` 说明，不要造数 |

---

## 附：证据文件索引

- `D:\Vibe_Workspace\Pipline\E2E-Test.jenkinsfile`（1451 行）—— 无 Volcano 读取；`:16/:39/:166` Genet 镜像 API；`:400-407/:972/:1077` Jenkins 队列语义；`:1019/:1219-1220` 队列面板=噪声
- `D:\Vibe_Workspace\Pipline\Helm-Start-Service-MB.Jenkinsfile`（3240 行）—— `:21-26` 排队 7 天；`:539-708` 参数页面板；`:1782-1863` 排队期日志；`:2184-2332` 安装期轮询 + HTML 面板
- `D:\Project\TestPalne\.data\genet\backend\internal\handlers\openapi_queue.go`（85 行）、`handlers\queue.go`（199 行）
- `D:\Project\TestPalne\.data\genet\backend\internal\k8s\volcano.go`（570 行，`:32-48` GVR、`:317-341` Queue 字段、`:356-377` PodGroup 字段）
- `D:\Project\TestPalne\.data\genet\backend\internal\models\volcano_views.go`（100 行，`:6-39` 三类数据源权威性）
- `D:\Project\TestPalne\.data\genet\helm\genet\templates\rbac.yaml:75-83`、`helm\genet\values.yaml:66-118`
- `D:\Project\TestPalne\server\api\openapi.yaml:1116-1151`、`:3153-3360`
- `D:\Project\TestPalne\design\impl\execution.md:411-431`、`design\impl\control-plane.md:417-474`
- `D:\Project\TestPalne\server\internal\api\meta.go:156-266`（现状 stub）、`server\go.mod:11`
- `D:\Project\TestPalne\web\src\pages\ClustersPage.tsx:159-206`、`web\src\api\types.ts:432-437`
- `D:\Project\TestPalne\deploy\k8s\rbac-cluster-executor.yaml:20-38`、`design\views\06-physical-view.md:55`
