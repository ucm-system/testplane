# Genet 服务生命周期调研笔记（部署 / 就绪 / 删除）

> 证据来源：只读克隆 `D:\Project\TestPalne\.data\genet`（下称 `<G>`）+ 真实调用方 `D:\Vibe_Workspace\Pipline\E2E-Test.jenkinsfile` + 本仓 `design/`、`server/`。
> 用途：直接支撑《TestPlane Genet 适配》实现设计。
> 约定：`<G>/path:line` 表示 Genet 仓库内的文件与行号；`<G>` = `D:\Project\TestPalne\.data\genet`。

---

## 一句话结论

Genet 没有"部署单"这个概念，**一次部署 = 一次 Helm release**（幂等键只能自己造，定位符只有 `namespace + releaseName`）；就绪判定被拆成两根互不相干的轴——**helm `status`（pending-install/deployed/failed/uninstalling…）** 和 **`callInfo.endpoint.status`（pending/ready/blocked）**，而 `ready` 只代表"地址解析出来了"，既不代表 Pod Ready，也不代表能推理（`<G>/docs/HELM_RELEASE_CALL_ENDPOINT.md:44`）；**排队不是 Genet 的状态**，它发生在 Volcano/PodGroup 层，Genet 只能给出队列水位计数而没有位次；**删除 = 同步 `helm uninstall`（可 force）**，唯一"部署中取消"的手段就是 DELETE，且异步 install 未落 release 时 DELETE 只会 404。因此 TestPlane 的 `queued/accepted/deploying/ready/blocked` 五态**无法一一对应**，适配层必须自己做状态合成与幂等键。

---

## 事实清单（带行号）

### A. 拉起（deploy / start service）

#### A1. 拉起接口：方法 / 路径 / 请求体 / 响应体

| 项 | 值 | 证据 |
|---|---|---|
| 推荐路径（按包类型） | `POST /api/open/helm/uc-packages/{stack\|pd}/releases` | `<G>/backend/cmd/api/main.go:721`（`openHelm.POST("/uc-packages/:type/releases", …InstallUcPackageRelease)`） |
| 兼容路径 | `POST /api/open/helm/uc-stack/releases`、通用 `POST /api/open/helm/releases` | `<G>/backend/cmd/api/main.go:708`、`:734` |
| Base URL（现网） | `https://genet.dev.huawei.com` | `<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:785`；`E2E-Test.jenkinsfile:16` |
| 请求体 schema | `UcStackInstallRequest`，**仅 `releaseName` 必填** | `<G>/backend/internal/models/helm.go:170-184`；`<G>/backend/api/openapi.yaml:3824-3865` |
| 响应 | **HTTP 202** + 精简 `HelmReleaseDTO`（`name/requestedName/namespace/packageType/chart/chartVersion/status="pending-install"`） | `<G>/backend/internal/handlers/helm.go:2992-3000`；`<G>/backend/api/openapi.yaml:1586-1605` |
| `request_id` / `job_id` / `release_id` | **未找到**（全后端只有 CLI 登录有 `request_id`，与部署无关） | `grep request_id\|job_id\|release_id` 仅命中 `<G>/backend/internal/handlers/cli_auth.go:39-58` |

请求体字段（`<G>/backend/internal/models/helm.go:170-184`）：

| 字段 | 必填 | 说明 | 证据 |
|---|---|---|---|
| `releaseName` | ✅ | 正则 `^[a-z][a-z0-9-]{0,52}$` | `<G>/backend/internal/handlers/helm.go:36`、`:2730`、`:3041` |
| `values` | ✳ 与 `templatePath`/`templateId` 三选一 | 最终 values.yaml 文本 | `helm.go:2152-2177` |
| `templatePath` | ✳ | chart 内 `models/` 下的相对 YAML 路径（"部署模板"） | `helm.go:2168-2175`；`openapi.yaml:3830-3833` |
| `templateId` | ✳ | 已保存的 values 模板（ConfigMap） | `helm.go:2156-2167` |
| `modelPath` | ❌ | 高优先级覆盖，必须 `/mnt/model` 前缀 | `helm.go:613-626` |
| `image` | ❌ | 高优先级镜像覆盖 | `helm.go:2189-2195`；`openapi.yaml:3844-3847` |
| `set[]` | ❌ | `{name,value,type}` → helm `--set/--set-string/--set-json` | `helm.go:2213`；`models/helm.go:286-290` |
| `wait` | ❌ | 默认由后端固定为 `!noWait` | `helm.go:2760-2764` |
| `noWait` | ❌ | 显式关 `helm --wait`，apply 即返回（Jenkins 排队-安装双超时用） | `models/helm.go:164-166`；`helm.go:2764` |
| `timeoutSec` | ❌ | helm `--wait` deadline；默认 `config.Helm.TimeoutSec`（600s） | `helm.go:3532-3540`；`<G>/helm/genet/values.yaml:762` |
| `dryRun` | ❌ | 只渲染，同步 201 返回 | `helm.go:2939-2958` |

最小示例（与 Jenkins 实际报文一致，`<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:1279-1308`）：

```json
POST /api/open/helm/uc-packages/stack/releases
Authorization: Bearer <genet-api-key>
{
  "releaseName": "e2etest128-345",
  "values": "servingEngineSpec:\n  enableEngine: true\n  ...",
  "templatePath": "ascend/values-qwq-32b-single.yaml",
  "modelPath": "/mnt/model/Qwen2.5-1.5B-Instruct",
  "image": "registry.dev.huawei.com/xxx:v1",
  "set": [{"name": "servingEngineSpec.containerPort", "value": "23345", "type": "set"}],
  "wait": true, "atomic": true, "timeoutSec": 7200, "noWait": true
}
```

响应示例（`helm.go:2992-3000`，注意 `status` 是常量字符串，不含进度信息）：

```json
{ "name": "e2etest128-345", "requestedName": "e2etest128-345", "namespace": "user-alice",
  "packageType": "stack", "chart": "charts/unified-cache-stack", "chartVersion": "0.0.1",
  "status": "pending-install" }
```

> 关键：202 之后真正的 `helm upgrade --install` 在**后台 goroutine** 跑（`helm.go:2981-2990`），文件头注释也写明"请求立即返回，release 状态由列表轮询如实呈现"（`helm.go:2981-2982`）。**调用方拿不到任何"作业号"**，只能凭自己起的 `releaseName` 去轮询。

#### A2. 参数怎么传（values / release / namespace / 镜像 / 模型 / 资源 / 队列）

- **release 名**：调用方自己给，必须是唯一名（Genet 不做任何名字加工——`resolveUcStackReleaseName` 直接 `TrimSpace` 返回，`<G>/backend/internal/handlers/helm.go:2696-2698`；`openapi.yaml:4175` README 里"Genet adds a namespace prefix"是**过时描述**）。
- **namespace**：**不允许**在请求体里指定，取 API Key 绑定的 namespace（`<G>/backend/internal/handlers/openapi_helm.go:39-56` → `helm.go:67-83`）；Key 未绑 namespace → 403（`openapi_helm.go:41-45`）。用户 values 里出现 `namespace` / `serviceAccountName` 会被策略直接拒（`helm.go:3655-3662`）。
- **values 合并顺序**（这是"部署模板"概念的全部）：chart 默认 values → `templateId` 或 `templatePath` 或 `values`（三选一） → `image`/`modelPath` 覆盖 → `set` 覆盖 → chart 内 `values-genet.yaml` overlay（调度字段）→ **再**覆盖一次 image/modelPath（防止 overlay 遮蔽）。
  - 证据：`<G>/backend/internal/handlers/helm.go:2143-2196`；顺序声明 `<G>/backend/api/openapi.yaml:1584-1586`。
- **chart / version 不可指定**：由后端配置 `helm.ucPackages.{stack|pd}` 固定（`helm.go:171-177`、`:2210-2211`；`<G>/helm/genet/values.yaml:775-783`）。这就是"部署模板"的两层含义：**包（chart）固定 + 包内 models/*.yaml 模板可选**。
- **模型路径**：`modelPath` → `servingEngineSpec.modelSpec.modelURL`（stack）/ `modelPath`（pd）；必须以 `/mnt/model` 开头（`helm.go:613-626`）。
- **镜像**：`image` → `servingEngineSpec.modelSpec.image`（`helm.go:2189-2195`）。
- **资源（CPU/内存/GPU/shm）**：**没有独立字段**，只能写进 `values`（例如 `requestCPU`/`requestMemory`/`requestGPU`/`shmSize`，见 `E2E-Test.jenkinsfile:282-285` 的 values 示例）。
- **队列**：**没有入参**。队列由 chart 的 `values-genet.yaml` overlay + API Key 绑定的池决定（`helm.go:2182-2185`）；调用方只能**读**队列状态（见 A4）。

#### A3. 幂等与重试

- **重复调用同一次拉起 = 报错，不是幂等成功**：`ensureFreshHelmInstallTarget` 在 release 已存在（有 protected metadata 或 helm 里存在）时直接返回 `release %q already has protected metadata; install cannot replace an existing release`（`<G>/backend/internal/handlers/helm.go:2700-2718`）。
- **HTTP 状态**：该错误不是 conflict 类，经 `protectedHelmMutationErrorStatus(err, 400)` 归一化为 **400**（`helm.go:2933-2936` + `:2454-2459`）；仅有"操作进行中/元数据丢失/需修复"三类返回 409（同处 `:2455-2456`）。
- **没有 Idempotency-Key / 幂等头**：中间件只认 `Authorization: Bearer`（`<G>/backend/internal/auth/apikey.go:37-49`），未见任何幂等 header 解析。
- **并发 install 同一 release**：第二次会因 metadata 操作 token 在飞而 409 `protected Helm operation is in progress`（`helm.go:39`、`:2438`、`:2554`）。
- **调用方规避手段（现网做法）**：
  - release 名 = 用户基名 + `-` + 构建号（`<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:1055-1066`）；
  - 或空名自动生成 `ucm-<build>-<uuid8>`（同文件 `:1153-1155`）；
  - E2E 侧用 `HELM_RELEASE_PREFIX + BUILD_NUMBER` 作基名，并**以下游返回的权威 release 为准**（`E2E-Test.jenkinsfile:512-513`、`:843-847`）。
- 结论：**TestPlane 必须自己造幂等键并落到库**，推荐 `releaseName = "tp-" + sha(run_id)[:8] + "-" + sha(deploy_id)[:6]`，并在 `runs.deploy` 冻结；**恢复/重启后先 GET 再决定 POST**。

#### A4. 排队：会不会被排队、排队时返回什么

- **Genet 侧没有 "queued" 状态**。排队由 Volcano + chart overlay 实现，Genet 只是"照单 apply"，因此 release 可能在 `status=pending-install` 或（`noWait=true` 时）**直接 `deployed`**，而 Pod 仍在 `Inqueue/Pending`（`helm.go:2760-2764`、`:2981-2982`；Jenkins 的 SPLIT 逻辑 `Jenkinsfile:21-26`、`:1924-1928`）。
- **队列可见性接口**（只能"看水位"，没有"位次"）：

| 端点 | 返回 | 证据 |
|---|---|---|
| `GET /api/open/queue` | `{enabled, schedulerName, metricsAvailable, poolType, queueName, queue:{name,state,weight,capability,allocated,physicalCapacity,deserved,request,realCapability,overused,share,running,pending,inqueue,unknown,completed}}` | `<G>/backend/internal/handlers/openapi_queue.go:14-49`；`<G>/backend/internal/models/volcano_views.go:13-39`；`<G>/backend/internal/models/openapi_queue.go:5-22` |
| `GET /api/open/queue/podgroups` | `{enabled, poolType, queueName, podGroups:[{name,namespace,queue,phase,minMember,conditions[]}]}`，仅本 Key 命名空间；`phase ∈ Pending/Inqueue/Running/Unknown/Completed`，**阻塞原因取 `conditions[last].message\|reason`** | `<G>/backend/internal/handlers/openapi_queue.go:54-85`；`volcano_views.go:41-57` |

- **没有 `queue_position`（第几位）字段**——只有计数（`running/pending/inqueue`）。Jenkins 自己也只能打印"本命名空间排队 N 个 + 队列水位 + 阻塞原因"（`Jenkinsfile:1836-1864`、`:2246-2274`）。
- **拉起本身可能被拒/被卡**（这些才是 Genet 侧的"排队"，但都以错误码或长时间无 Pod 表达）：

| 场景 | 表现 | 证据 |
|---|---|---|
| release 数达上限 | 400 `release 数量已达上限 N`（默认 `maxReleasesPerUser: 0` 不限） | `helm.go:2737-2745`；`<G>/helm/genet/values.yaml:764` |
| 不在资源访问时间窗 / 调度策略无可用节点 | **403** `当前不在资源访问时间窗口内`（带 `nextTransitionAt`）/ `调度策略当前没有有效可用节点` | `<G>/backend/internal/handlers/runtime_policy.go:24-55` |
| chart 仓库未配置 | **424** | `helm.go:94-104` |
| API Key 未绑 namespace | **403** | `openapi_helm.go:39-45` |
| 资源不足（Volcano 排不上） | 请求成功、release `pending-install`/`deployed`，Pod 长时间 `Pending/Inqueue`，**无任何超时** | `Jenkinsfile:1782-1832`（自建 7 天排队窗口）、`:26` |

---

### B. 就绪判定

#### B5. 状态机（Genet 有两根轴）

**轴 1：`release.status`（直接来自 helm，不是枚举常量定义）**

| 取值 | 含义 | 终态 | 证据 |
|---|---|---|---|
| `pending-install` / `pending-upgrade` / `pending-rollback` | helm 操作进行中 | 否 | Jenkins 用 `startsWith('pending')` 判定（`<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:2406-2408`）、202 返回体硬编码 `pending-install`（`helm.go:2999`） |
| `deployed` | helm 认为成功（`--wait` 关闭时不代表 Pod 就绪） | 是 | `helm.go:2849-2850`；`Jenkinsfile:1924-1928` |
| `failed` | helm 失败（含 `--wait` 超时） | 是 | Jenkins 统一判失败（`Jenkinsfile:1929-1931`、`:2121-2123`、`:2841-2845`） |
| `uninstalling` / `uninstalled` | 删除中 / 已删 | 是 | `Jenkinsfile:2910-2913`；`countActiveReleases` 把 uninstalled/superseded/uninstalling 排除出配额（`helm.go:713-725`） |
| `superseded` | 被更高 revision 取代 | 是 | 同上 |

> 后端未定义 status 常量，值原样透传 helm CLI `status/list` 输出（`<G>/backend/internal/helm/client.go:30-58`、`:82-142`）。

**轴 2：`callInfo.endpoint.status`（后端自己算的三值）**

| 取值 | 含义 | 证据 |
|---|---|---|
| `pending` | 还没有可用地址（既无 external、也无 Service） | `helm.go:924-927` |
| `ready` | 地址已解析（router / master hostIP / ClusterIP 三选一），**不代表能推理** | `helm.go:855-928`；`docs/HELM_RELEASE_CALL_ENDPOINT.md:44` |
| `blocked` | 需要 router 但 router 不可用/配置无效（**终态**） | `helm.go:859-876` |

`blocked` 的三种文案（都是自由文本，没有错误码枚举）：`该实例调用入口不可用：未配置 …routerEndpoint，且 routerDiscovery 未启用`、`…routerEndpoint 配置无效`、以及 `RouterMessage`（通常是 k8s 读取失败原文，例如 `服务 "kthena-router" not found`，见 `docs/HELM_RELEASE_CALL_ENDPOINT.md:169-181`）。

#### B6. 查询接口 / 权威服务信息 JSON 的形状

| 端点 | 用途 | 证据 |
|---|---|---|
| `GET /api/open/helm/releases/{name}` | **主轮询端点**：`status + callInfo.endpoint + pods[] + revision + description` | `<G>/backend/cmd/api/main.go:727`；`helm.go:3726-3754` |
| `GET /api/open/helm/releases/{name}/pods` | 只取 Pod 列表 `{items:[…]}`（不含 callInfo） | `main.go:728`；`helm.go:3756-3777`；模型 `models/helm.go:392-411` |
| `GET /api/open/helm/releases?light=true` | 只回 name/status/chart…，跳过 callInfo/pods 富化（release 多时必须用，否则易超时） | `helm.go:3353-3355`、`:3373-3377` |
| `GET /api/open/helm/releases/{name}/history` | helm revision 序列（`status`/`description` 是失败取证主力） | `main.go:729`；`helm.go:4160-4190` |
| `GET /api/open/helm/releases/{name}/manifest` | 渲染后的 K8s YAML | `main.go:730`；`helm.go:4192-4209` |
| `GET /api/open/helm/operation-history?workloadName=&status=failed&pageSize=` | **异步 install 失败原因（helm stderr）在这里** | `main.go:733`；`Jenkinsfile:1386-1397`；模型 `models/operation_history.go:24-57` |

`GET /releases/{name}` 的关键字段（`models/helm.go:292-311`）：`name/requestedName/namespace/packageType/chart/chartVersion/revision/status/updatedAt/description/values/manifest/callInfo/pods`。

`callInfo`（`models/helm.go:313-343`）：
`endpoint{status,source,url,host,port,curlExample,portForwardCommand,message}`（`models/helm.go:366-378`）、`serviceName/namespace/port/clusterURL/portForwardCommand/curlExample/model/hostNetwork`、`external[]{podName,nodeName,hostIP,port,url,ready,curlExample}`、`routerRequired/routerAddress/routerURL/routerCurlExample/routerMessage`、`routerLogTarget{status,namespace,serviceName,message}`。

**"权威服务信息 JSON"不是 Genet 的响应**，而是 `Helm-Start-Service-MB` 自己组装并写进 `currentBuild.description` + 归档 `access-info.json` 的产物（`<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:1619-1633`）：

```json
{ "release": "...", "build": "128", "model": "Qwen3-0.6B",
  "endpoint": {"status":"ready","source":"external","url":"http://ip:port",
               "host":"ip","port":23128,"curlExample":"…","portForwardCommand":"","message":""},
  "router": null, "cluster": null,
  "external": [{"podName":"…","nodeName":"…","hostIP":"…","port":23128,"url":"…","ready":true,"curlExample":"…"}] }
```

Jenkinsfile（真实调用方）解析路径（`E2E-Test.jenkinsfile`）：
1. 优先 `copyArtifacts` 拉下游 `access-info.json`（`:1189-1216`，带"每次 E2E 构建号"目录隔离，`:1194`）；
2. 回退从 build description 里抓首个 `{` 到末个 `}`（`:1222-1233`）；
3. 就绪只看 `endpoint.status`：`ready` 才算就绪、`blocked` 立刻终止等待（`:1114-1117`、`:1240-1250`）；
4. 消费的字段仅 `release/build/model` + `endpoint.{status,url,host,port,curlExample,message}`（`:533-557`），`router/cluster/external` 明确为诊断用（`:538-539`）；
5. **另做业务探活**：先 `GET /v1/models` 诊断（`:587`、`:1343-1360`），再循环 `POST /v1/chat/completions`（`max_tokens=1`、默认 `stream=true`）直到 2xx（`:594-608`、`:1263-1328`）。

#### B7. 就绪的判定依据 / 失败如何表达

- `endpoint.ready` 的判据是"**地址存在**"，优先级：router 可用 → 有 hostIP 的 master Pod（external）→ ClusterIP Service（`helm.go:855-928`）。其中 external 只在 values 的 `hostNetwork=true` 时生成（`helm.go:804-832`），并**优先挑 Ready 的 Pod，但不是必须**（`:887-899`：任何有 URL 的都能被选中，`Ready` 只是加权）。
- **Pod Ready 只体现在 `external[].ready` / `pods[].ready` 两个展示字段**（`helm.go:1541-1557` + `podReady()`），**不参与** `endpoint.status` 决策。
- **Service 选择靠命名约定**（不是 label 精确匹配）：`<release>-router-service`(0) > `<release>-*-pod0-service`(1) > `<release>-*-engine-service`(2) > `<release>-*-ray-head-svc`(3) > label 命中 `helm-release-name|app.kubernetes.io/instance|app.kubernetes.io/name`(4)（`helm.go:1472-1494`、`:1528-1539`）。
- Pod 归属判定同上三条 label（`helm.go:1520-1539`），列表按 `creationTimestamp` 升序（`:1511-1516`）。
- **失败/阻塞的表达**（无错误码枚举，全是文本）：
  - `endpoint.status=blocked` + `endpoint.message`（`helm.go:859-876`）→ Jenkins 立即失败（`Jenkinsfile:2761-2763`）；
  - `release.status=failed` + `release.description`（helm 原文）+ `history[].description`（`Jenkinsfile:1490-1502`）；
  - 异步 install 的 helm stderr → 操作历史 `status=failed` 的 `message`（`helm.go:2875-2889`；`Jenkinsfile:1386-1411`）；
  - Pod 级原因：`pods[].status`（`podDisplayStatus`：`Terminating` / 容器 `Waiting.Reason` / `Terminated.Reason` / `Phase`，`helm.go:1561+`）。

#### B8. 超时与取消

| 项 | 事实 | 证据 |
|---|---|---|
| helm `--wait` 超时 | `timeoutSec`（请求可传）；默认 `config.Helm.TimeoutSec`，出厂 600s | `helm.go:3532-3540`；`<G>/helm/genet/values.yaml:762` |
| 后台 goroutine 生命周期 | `timeoutSec + 60s` | `helm.go:2819-2820` |
| 无 `--wait` 模式 | `noWait=true` → apply 即返回，释放 helm 进程；就绪完全由调用方按 Pod 判 | `helm.go:2760-2764`；`models/helm.go:164-166` |
| Genet 侧"部署窗口" | **没有**独立于 helm 的窗口；排队期不消耗任何 Genet 计时器（noWait 时更是完全没有计时器） | 同上 + `Jenkinsfile:21-26` |
| 取消部署接口 | **未找到**。唯一手段是 `DELETE /api/open/helm/releases/{name}`（本质是卸载） | `main.go:736`；全文件搜索无 cancel/abort 端点（`helm.go` 仅有 `context.Cancel`） |
| 取消的竞态 | 异步 install 未落 release 时 DELETE → **404**；install 进行中 DELETE → metadata 操作在飞，可能 409/500，Jenkins 因此先 `waitReleaseNotPending` 等状态脱离 `pending*` | `Jenkinsfile:2397-2414`；`helm.go:2438`、`:2454-2459` |

---

### C. 删除（卸载）

#### C9. 卸载接口

| 项 | 值 | 证据 |
|---|---|---|
| 方法与路径 | `DELETE /api/open/helm/releases/{name}?keepHistory=<bool>&force=<bool>` | `<G>/backend/internal/handlers/helm.go:3930-3931`；`main.go:736` |
| 请求体 | 无（全部走 query） | 同上 |
| 同步/异步 | **同步阻塞**，直到 `helm uninstall` 完成（`force` 时 `--wait` 关闭并强删 Pod） | `helm.go:4000-4003`；`:3950-3955` |
| 超时 | 常规 **10 分钟**；`force=true` **2 分钟** | `helm.go:3952-3955` |
| 响应 | `{message, forcedPods[], secretsDeleted, cleanupErrors[]}` | `helm.go:4122-4130` |
| 幂等性 | release 不存在 → **404** `helm release not found`（先验 owner metadata）；调用方把 404 当"已删" | `openapi_helm.go:58-78`、`:147-155`；`Jenkinsfile:3063-3072`；`<G>/jenkins/Helm-Stop-Service-MB.Jenkinsfile:533-542` |
| 归属校验 | 需 protected metadata 的 `owner == API Key ID`，否则 404 遮蔽 | `openapi_helm.go:58-78` |
| 前置完整性校验 | 会校验当前 revision 的 manifest/hooks digest，不一致 → 403/400 | `helm.go:3970-3978` |

**"已卸载干净"的确认方式**（现网唯一可信方式，两个条件同时满足）：
1. `GET /releases/{name}` → 404，或 `status ∈ {uninstalled, uninstalling, ""}`；
2. `GET /releases/{name}/pods` → `items == []`（且该请求本身成功，而非报错）。
证据：`<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:3027-3061`、`<G>/jenkins/Helm-Stop-Service-MB.Jenkinsfile:501-531`。

#### C10. 失败卸载 / 自动回收

- **force 语义**：不等 Pod 优雅退出，先放掉 release，再 `ForceDeletePod`（去 finalizer、grace=0），helm 报错时进一步删 `sh.helm.release.v1.*` Secret；部分失败会 `MarkHelmReleaseMetadataOperationRepairRequired` 永久 fail-closed 等人工修复（`helm.go:4027-4065`）。
- **重试策略（现网）**：常规删除 → 5 分钟没确认 → `force=true` 兜底（`CLEANUP_VERIFY_TIMEOUT_SEC=300`，`Jenkinsfile:8-10`、`:3197-3213`；Stop-Service 同构）。
- **TTL / 自动回收**：
  - **没有 release TTL**。GC 只针对 Pod / Deployment / StatefulSet：CronJob 每天 **23:00**（`<G>/helm/genet/values.yaml:954`；`<G>/helm/genet/templates/cronjob-gc.yaml:7`）扫描 `genet.io/managed=true` 且名字以 `user-` 开头的 namespace（`<G>/backend/internal/cleanup/lifecycle.go:126-145`），对 Deployment/StatefulSet 做"挂起"（单副本、`local` 模式），对无主 Pod 做"挂起"（commit 镜像 → 写挂起记录 → 删 Pod）。
  - 豁免：Pod 带未过期的 `genet.io/protected-until`（`lifecycle.go:41-52`、`:182-190`、`:360-367`）、独占/裸金属池命名空间整仓跳过（`:149-156`）。
  - **不删 Helm release 记录、不 uninstall**（`lifecycle.go:294-334` 只动 Deployment/StatefulSet）。
  - ⚠️ **对 TestPlane 的直接影响**：`helm-release-name` 之类的 label **不在豁免条件里**（`grep helm` 于 `lifecycle.go` 无命中），即"长驻服务在半夜被挂起"在代码上没有被排除 → 必须实测（见 §未确认 R4）。

---

### D. 鉴权与调用约束

| 项 | 事实 | 证据 |
|---|---|---|
| 认证方式 | `Authorization: Bearer <API Key>`（**必须 header**，不支持 query/服务账号/基本认证） | `<G>/backend/internal/auth/apikey.go:37-49` |
| 未带/错 key | 401 `Missing or invalid Authorization header` / `Invalid API key` | 同处 `:38-49`、`:71-73` |
| 权限 scope | `read` / `write`；写操作要求 `write`，不足 → 403 `forbidden: insufficient scope` | `<G>/backend/internal/models/apikey.go:10-15`；`auth/apikey.go:76-143` |
| 命名空间 | 由 Key 绑定（`record.Namespace`），缺省 403 `API Key 未绑定命名空间` | `openapi_helm.go:39-45`；`auth/apikey.go:104` |
| 池限制 | 共享池用户被拒（`该功能仅对独占池 / 裸金属池用户开放`，`code=not_non_shared_pool`）；判断依据 `openapiPoolType` | `<G>/backend/internal/auth/exclusive_pool.go:18-80` ⚠️ 但 OpenAPI `/helm` 路由注册处**未见**挂该中间件（`main.go:691-695`），与 `openapi_helm.go:17` 的注释不一致 → 未确认 R6 |
| 访问时间窗 | 生效时 mutation 可能被 403 拒绝，返回 `nextTransitionAt` | `handlers/runtime_policy.go:44-55` |
| 请求体大小限制 | **未找到**（无 `MaxBytesReader`；只有 chart 包大小上限 `maxChartSize: 100Mi`） | `grep MaxBytesReader` 无命中；`<G>/helm/genet/values.yaml:761` |
| 限流 | **未找到**（无 rate limiter 中间件） | `grep -i rate.?limit` 于 `backend/` 无命中 |
| 超时建议 | 轮询用 30s（现网 `maxTimeSec=30`）、force 删除用 360s、dry-run 60s；PowerShell/Go 客户端建议 connect 5s / 总 10–30s | `Jenkinsfile:3005`、`:3016`、`:1340`；`docs` 未给 |
| 响应体积 | 单 release 的 `GetRelease` 带 `values + manifest + pods`，长 values 可达数十 KB；列表务必 `light=true` | `helm.go:3345-3384`、`:3726-3754` |
| 客户端实现细节 | 后端用 helm CLI 子进程（`<G>/backend/internal/helm/runner.go`），故响应延迟受集群影响，轮询不要并发打爆 | `helm/client.go:439-486` |

---

## 接口清单表

| 用途 | 方法 | 路径 | 请求 | 响应 | 幂等 | 证据 |
|---|---|---|---|---|---|---|
| **拉起（按包）** | POST | `/api/open/helm/uc-packages/{stack\|pd}/releases` | `UcStackInstallRequest`（`releaseName` 必填） | **202** `HelmReleaseDTO{status:"pending-install"}` | ❌ 同名 → 400 | `main.go:721`；`helm.go:2906-3001` |
| 拉起（legacy） | POST | `/api/open/helm/uc-stack/releases` / `/api/open/helm/releases` | 同上 / `HelmInstallRequest` | 同上 / 201 | ❌ | `main.go:708`、`:734` |
| 预检 | POST | `/api/open/helm/uc-packages/{type}/dry-run` | 同上（`dryRun` 无效） | `{manifest,safetyProblems,values,releaseName,packageType}` | ✅（只读） | `main.go:725`；`helm.go:3017-3068` |
| 升级 | PUT | `/api/open/helm/uc-packages/{type}/releases/{name}` | `UcStackUpgradeRequest` | 200 `HelmReleaseDTO` | 语义幂等（同 values 仍会加 revision） | `main.go:722`；`helm.go:3070-3197` |
| **查状态（主轮询）** | GET | `/api/open/helm/releases/{name}` | — | `HelmReleaseDTO`（含 `status`+`callInfo.endpoint`+`pods`） | ✅ | `main.go:727`；`helm.go:3726-3754` |
| 查 Pod | GET | `/api/open/helm/releases/{name}/pods` | — | `{items:[HelmReleasePodDTO]}` | ✅（已删后仍可查，返回空） | `main.go:728`；`openapi_helm.go:85-122` |
| 列 release | GET | `/api/open/helm/releases?light=true` | — | `{items:[…]}` | ✅ | `helm.go:3345-3384` |
| 历史 | GET | `/api/open/helm/releases/{name}/history` | — | `{items:[{revision,status,description,…}]}` | ✅ | `helm.go:4160-4190` |
| 清单 | GET | `/api/open/helm/releases/{name}/manifest` | — | `{manifest}` | ✅ | `helm.go:4192-4209` |
| **卸载** | DELETE | `/api/open/helm/releases/{name}?keepHistory=&force=` | — | `{message,forcedPods,secretsDeleted,cleanupErrors}` | ⚠️ 404=已删可当成功；`keepHistory=true` 会留下 `uninstalled` 记录 | `main.go:736`；`helm.go:3931-4131` |
| 队列 | GET | `/api/open/queue` | — | `OpenAPIQueueResponse` | ✅ | `main.go:641` |
| 排队/阻塞 | GET | `/api/open/queue/podgroups` | — | `{podGroups:[{phase,conditions}]}` | ✅ | `main.go:642` |
| 失败原因 | GET | `/api/open/helm/operation-history?workloadName=&status=failed` | — | `{items:[{action,status,message,details}],total,page,pageSize}` | ✅ | `main.go:733`；`models/operation_history.go:24-57` |
| 模板 | GET | `/api/open/helm/uc-packages/{type}/model-templates`、`…/model-template?path=` | — | 树 / `{path,values}` | ✅ | `main.go:714-715`；`helm.go:1824-1839` |

---

## 状态映射表（Genet → TestPlane）

TestPlane 侧定义：`DeployState.Phase ∈ queued|accepted|deploying|ready|blocked`（`server/internal/ports/ports.go:118-129`），失败只走 Run 的 `failed`（`design/impl/control-plane.md:519-532`）。

**合成规则**（适配层 Poll 每次按序判定，命中即返回）：

| # | Genet 观测（`GET /releases/{name}` + 可选 `/queue/podgroups`） | TestPlane Phase | 附加动作 |
|---|---|---|---|
| 1 | GET 404，且**距 Start < N 次轮询/180s**（release 尚未 apply 出来） | `accepted` | 记 `state_detail="release not yet visible"` |
| 2 | 404 超过宽限（现网 36×5s=180s） | `blocked` | Run → `service_unavailable`（`deploy_failed`） |
| 3 | `status` 以 `pending-` 开头，且 `pods == []` 或所有 Pod `nodeName == ""` | `queued` | 取 `/queue/podgroups` 的 `phase/reason` 填 `reason`（**无位次**） |
| 4 | `status` 以 `pending-` 开头，Pod 已调度（`nodeName != ""`）或已有 `ready=false` 的 Pod | `deploying` | 记 `pods[]` 快照 |
| 5 | `endpoint.status == "ready"`（且 `url` 非空） | `ready` | 写 Endpoint；**建议再做一次业务探活**才真正放行 Task |
| 6 | `endpoint.status == "blocked"` | `blocked` | 终态；`Failure.message = endpoint.message` |
| 7 | `status ∈ {failed, superseded, uninstalled, uninstalling}` | `blocked` | 终态；`Failure.message = release.description` + `operation-history.message` |
| 8 | `status == "deployed"` 但 `endpoint.status == "pending"` | `deploying` | **不可**当 ready（noWait 下 deployed 早于地址出现） |
| 9 | 超 `deploy_timeout_seconds` 仍非终态 | `blocked` | 映射 Run → `service_unavailable`（`design/impl/execution.md:370`） |

**对不上的地方（必须在设计里显式处理）**：

1. **`queued` 没有权威来源**。Genet 无队列状态、无位次；只能由"release 存在但 Pod 未调度"反推，且 Pod 也可能因为镜像拉取慢而非排队。→ `runs.deploy.queue_position` **必须允许为 null**，UI 用"已等待时长 + 队列水位计数"替代位次。
2. **`accepted` vs `deploying` 没有官方分界**。Genet 202 之后没有"受理"事件，`pending-install` 既可能是排队也可能是正在拉镜像。→ 用 Pod 是否被调度作代理判据，属**平台侧推断**，需在文档中标注为启发式。
3. **`ready` 语义窄一档**。TestPlane 的 `Ready=true` 被当作"可以注入 endpoint 跑 Tester"，而 Genet 的 `endpoint.ready` 只保证"地址解析完成"（`docs/HELM_RELEASE_CALL_ENDPOINT.md:44`）。→ 适配层要么额外探活（推荐，与现网一致），要么把 `Ready` 的语义降级并让 Tester 自己重试——不能假装两者等价。
4. **Genet 有 `failed` 终态，TestPlane 的 Deploy 子状态没有**。→ 统一折叠成 `blocked` + `Failure{code: DEPLOY_FAILED, message}`，Run 转 `service_unavailable`，**不要**把 Run 打成 `failed`（否则与"平台调不通"混淆，违反 `control-plane.md:530` 的区分）。
5. **取消不可靠**。`Cancel` 在 release 尚未出现时只能 404；"取消意图"必须落库，等 release 出现后再删（否则会留下无人认领的长驻服务，正是现网 `Jenkins` post 里反复强调的泄漏点，`E2E-Test.jenkinsfile:838-847`）。
6. **排队窗口不在 Genet**：Genet 的 `timeoutSec` 是 helm `--wait` 的 deadline。若 TestPlane 传 `wait=true` + `timeoutSec=7200`，排队超过 2h 就会得到 `status=failed`（而 Pod 还在排队）→ **必须 `noWait=true`**（或把 `timeoutSec` 拉到与 `deploy_timeout_seconds` 同级），就绪由我们轮询判。

---

## 适配层设计要点

### 能否直接用 `ports.Deployer` 实现

**能，但接口不用改、语义要补。** 三个方法的落点：

| 方法 | Genet 映射 | 补齐的语义 |
|---|---|---|
| `Start(ctx, DeployRequest{RunID, DeployID, Manifest, Timeout})` | `POST /uc-packages/{type}/releases`（`noWait=true`） | 由 `DeployID` 派生确定性 `releaseName`；**先 GET 再 POST**（幂等）；把 `values/packageType/templatePath/image/modelPath/set` 从 `Manifest` 解出并冻结 |
| `Poll(ctx, runID)` | `GET /releases/{name}`（+ 低频 `/queue/podgroups`） | 按上表合成 Phase；维护 `pods` 快照、`endpoint`、`reason`、`lastPolledAt` |
| `Cancel(ctx, runID)` | `DELETE /releases/{name}?keepHistory=false&force=true` | 404 视为成功；release 未出现则写"待取消"标记，后续 Poll 见到即删 |

不需要的是：Helm 渲染、chart 选择、拓扑判断、Endpoint 自行推导——这些 `execution.md:13`（"不做 Helm 渲染、不改 Genet 参数、不解释拓扑"）与 `:378`（"URL 只取 Genet 返回值"）已经明确禁止。

### 幂等键的选择

- **`releaseName` 是唯一可用幂等键**（Genet 无 Idempotency-Header、无 request_id）。规则：`tp-` + 12 位（`sha256(run_id + ":" + deploy_id)` 前 12 位，小写 hex），满足 `^[a-z][a-z0-9-]{0,52}$`（`helm.go:36`），且**同 run 同 deploy 恒等、不同 deploy 必不同**（避免重试撞上"already exists"）。
- 落库到 `runs.deploy.release_name`，**先落库再 POST**（POST 超时不代表没创建——Genet 是 202 后后台跑，网络断也可能已 apply）。
- 恢复流程：`GET /releases/{name}` → 200 则继续 Poll；404 且已 POST 过 → 重新 POST 前先确认不是"还没 apply 出来"（用 `deploy.accepted_at` 的宽限期 180s 判）。

### 并发轮询与退避

- **单 run 单飞**：一个 poller goroutine，`sync.Map[runID]` 去重；`Poll` 幂等无副作用，但要避免多协程同时打 Genet（后端是 CLI 子进程，重了会拖垮）。
- 退避：与设计稿一致——**前 30s 每 1s，之后 5s 起指数退避，上限 15s**（`design/impl/execution.md:376`，`control-plane.md:521`）；单请求超时 **10s**（同 `execution.md:376`）。
- 队列信息降频：`/queue/podgroups` 每 5 次 Poll 打一次（15s×5=75s 量级），且失败**不判 run 失败**（它只是展示信息，现网也是 best-effort，`Jenkinsfile:1856-1860`）。
- 404 宽限与"连续 N 次"：现网 `RELEASE_NOT_FOUND_LIMIT=36`（180s，`Jenkinsfile:27-30`）。**建议改为时间窗**（`accepted_at + 180s`），不要用固定次数，因为退避后次数与时间不再线性。
- 总窗口：`deploy_timeout_seconds`（默认 72h，`control-plane.md:214`/`execution.md:377`）；排队计入窗口，取消不受限。

### 卸载（Cleanup）与"删干净"

- `DELETE` 是同步的，但**不要相信 DELETE 的 200**：以"`GET` 404 + `pods == []`"为成功判据（`Jenkinsfile:3027-3061`）。建议 5min 常规确认 → `force=true` 重试 → 再确认 2min（对齐现网 `CLEANUP_VERIFY_TIMEOUT_SEC=300`）。
- 卸载前置：若 `status` 仍 `pending-*`，先等其脱离（最多 120s），否则 409/500（`Jenkinsfile:2397-2414`、`helm.go:2438`）。
- 卸载失败**不改 Run 终态**，只记 `CLEANUP_FAILED` 事件 + 告警（`execution.md:371`、`control-plane.md:576`）。

### 需要变更的设计资产

- `design/impl/execution.md` §9 的"假定 Genet 提供 REST 接口 `POST /deployments`"这一句已被证伪，要改为 **Helm-release 语义**（`GET 404 宽限`、`ready ≠ 可用`、`无位次`）。
- `ports.DeployRequest.Manifest` 的 schema 必须包含 `packageType + values(冻结) + templatePath + image + modelPath + set`，否则 Genet 侧必 400（`helm.go:2168-2171`）。

---

## 需要落库的字段（`runs.deploy` JSON）

现状：`{state, queue_position, queue_reason, queued_at}`（`server/internal/store/deploy.go:17-35`）；设计稿 `{provider, parameters, deployment_id, state, queue_position, queued_at}`（`design/impl/control-plane.md:69`）。

建议最终形状（`queued_at`/`queue_reason`/`queue_position` 保留，但明确**位次可为 null**）：

```jsonc
{
  // 定位与幂等（复现的核心）
  "provider": "genet",
  "provider_api": "/api/open/helm/uc-packages/stack/releases",
  "namespace": "user-alice",
  "release_name": "tp-3f9c1a2b7d40",        // 幂等键：由 run_id+deploy_id 派生
  "requested_name": "tp-3f9c1a2b7d40",
  "deploy_id": "d-20261105-0001",
  "chart": "charts/unified-cache-stack",
  "chart_version": "0.0.1",
  "package_type": "stack",

  // 复现所需的冻结输入（Genet 侧不接受二次渲染）
  "values_digest": "sha256:…",
  "values": "<完整 values.yaml 文本>",
  "template_path": "ascend/values-qwq-32b-single.yaml",
  "image": "registry.dev.huawei.com/…:tag",
  "model_path": "/mnt/model/Qwen2.5-1.5B-Instruct",
  "model_name": "Qwen2.5-1.5B",             // 取自 callInfo.model，Tester 公共参数要用
  "set": [{"name":"servingEngineSpec.containerPort","value":"23345","type":"set"}],
  "no_wait": true,
  "helm_timeout_sec": 7200,

  // 状态机
  "state": "queued",                        // queued|accepted|deploying|ready|blocked
  "genet_release_status": "pending-install",// 原始 status，排障用
  "endpoint_status": "pending",             // 原始 endpoint.status
  "state_detail": "Pod pending: 0/1 scheduled",
  "queue_position": null,                   // Genet 不提供，保留字段但明确可为 null
  "queue_reason": "Queue shared-gpu: Insufficient huawei.com/Ascend910",
  "queue": { "phase": "Inqueue", "inqueue": 3, "pending": 1, "running": 7, "overused": false },

  // Endpoint（ready 后回填，Task 的 endpoint_key 由它算）
  "endpoint": { "status":"ready", "source":"external", "url":"http://141.111.32.68:23345",
                "host":"141.111.32.68", "port":23345, "message":"" },

  // Pods 快照（用于日志跟随、pods.json、拓扑漂移）
  "pods": [{"name":"…-engine-0","phase":"Running","status":"Running","ready":true,
            "nodeName":"gpu-node-1","podIP":"…","hostIP":"141.111.32.68","image":"…"}],

  // 时间线（排队时长展示 + 超时判定）
  "accepted_at": "…", "queued_at": "…", "ready_at": null,
  "last_polled_at": "…", "poll_count": 42, "not_found_streak": 0,

  // 失败/取消/卸载
  "failure": { "code": "DEPLOY_BLOCKED", "message": "该实例调用入口不可用：…" },
  "cancel_requested_at": null,
  "uninstall": { "state":"pending", "requested_at":null, "verified_at":null,
                 "force":false, "keep_history":false, "cleanup_errors":[] }
}
```

必须落库的理由（逐条对应需求）：`release_name`（唯一幂等键，重启后还能 Poll/删）、`values + values_digest`（复现与审计）、`endpoint`（`endpoint_key` 回填 + 注入 Tester）、`queue + 时间线`（位次展示 + 排队时长）、`cancel_requested_at`（取消意图）、`uninstall`（回收确认）、`genet_release_status/endpoint_status`（两层状态都留证，避免"Genet 说 deployed、我们说 deploying"时无从解释）。

---

## 未确认与风险

| # | 项 | 说明 | 影响 |
|---|---|---|---|
| R1 | **`ready` 不等于可服务** | `endpoint.ready` 只看地址是否解析出来（`helm.go:855-928`），PD/router 场景尤其明显：router 端点先可达，但 model server 注册要额外时间（`E2E-Test.jenkinsfile:408-411`） | TestPlane 若不额外探活，会把 404/5xx 派给 Tester，表现为"测试全挂"而非"部署失败" |
| R2 | **无排队位次** | 只有 `running/pending/inqueue` 计数（`volcano_views.go:13-39`） | `queue_position` 只能为 null；UI 若承诺"第 N 位"会做不出来 |
| R3 | **`queued` / `accepted` / `deploying` 三者靠推断** | Genet 无"受理"事件、无队列状态（见 §B5/A4） | 状态机可能出现"卡在 accepted"或"跳过 deploying"的抖动；需在 UI/日志里说明是启发式 |
| R4 | **每日 23:00 GC 可能挂起长驻服务** | `cleanup` 扫 `genet.io/managed=true` 的 `user-*` namespace，挂起单副本 Deployment/StatefulSet 与无主 Pod；**未见对 Helm release 的豁免**（`cleanup/lifecycle.go:119-334`；`cronjob-gc.yaml:7`；`values.yaml:954`） | 若 UC chart 的工作负载落在 `user-*` 且未设 `genet.io/protected-until`，长测在跨夜时服务可能消失 → **必须实测**（用一个 release 跨 23:00 观察） |
| R5 | **取消只在 DELETE 一条路上** | 无 cancel 端点；release 未出现时 DELETE=404；install 在飞时 DELETE 可能 409/500（`helm.go:2438`） | `Cancel` 必须"落意图 + 后置删除"，且要兜住"删了但 Pod 还在"（force + 二次确认） |
| R6 | **共享池限制在 OpenAPI 路由上是否生效存疑** | `openapi_helm.go:17` 注释称由 `auth.RequireNonSharedPool` 完成，但 `main.go:691-738` 的 `/open/helm` 路由**未挂**该中间件（仅 `RequireOpenAPIScope`） | 权限模型与注释不一致：可能是"靠 Key 的 namespace/pool 绑定 + 访问窗"间接限制，需向 Genet 团队确认，不要据此设计鉴权 |
| R7 | **`releaseName` 无命名空间前缀** | `resolveUcStackReleaseName` 直返（`helm.go:2696-2698`），与 `openapi.yaml:4175` 的"Genet adds a namespace prefix"描述矛盾 | 同名 release 在不同 namespace 下可共存（`namespace+name` 才是主键），我们的键要含 namespace |
| R8 | **Helm release 的 Service 靠命名约定** | `-router-service` / `-pod0-service` / `-engine-service` / `-ray-head-svc`（`helm.go:1472-1494`） | 换 chart 版本若改名，`endpoint` 会退化成 `pending` 或选错 Service；TestPlane 只能被动接受（禁止自行推导，`execution.md:378`） |
| R9 | **`deleted` 的确认依赖两次查询的时序** | "GET 404 + pods 空"才判干净；若 GET 先 404、pods 查询失败则不能判成功（`Jenkinsfile:3045`） | 适配层要把"pods 查询失败"与"pods 为空"区分开，否则会误报清理成功 |
| R10 | **未被验证的"长驻"假设** | Jenkins 的"部署即长驻"完全靠它自己长轮询维持（`Jenkinsfile:2352-2395`），Genet 侧没有任何"服务租期"概念 | TestPlane 不能指望 Genet 保活；Run 结束后不删就永久占卡（`E2E-Test.jenkinsfile:838-847` 的教训） |
| R11 | **请求体大小/限流未知** | 无 `MaxBytesReader`、无限流中间件（grep 无命中） | 大 values（含证书）可能触发反向代理层限制（Ingress 未查）→ 建议 values 控在 100KB 内并做 digest 校验 |
| R12 | **`GET /releases` 富化开销** | 每个 release 会 `helm get values` + `list pods`（`helm.go:767-769` 注释自认会超时） | 任何"列表页/对账任务"必须 `light=true`，否则拖垮后端 |
| R13 | **文档与实现漂移** | `docs/HELM_RELEASE_CALL_ENDPOINT.md` 未提 `noWait`、未提 `release.status`、未提 400/409/424 语义；`openapi.yaml:4175` 的 namespace 前缀描述过时 | 设计文档要以代码为准，勿以 doc 为准 |
| R14 | **Genet 是"个人 Pod 平台"还是"服务托管平台"？** | 结论：**两者都有**——既有 `/api/open/pods`（个人 Pod，`main.go:645-649`），也有 Helm release + Service 的服务语义。但"服务"的保障等级很弱：无 SLA、无租期、无健康检查、GC 可能挂起 | 不要把 Genet 当"服务编排器"用；TestPlane 必须自己承担生命周期收敛与清理兜底 |

---

## 附：与现网调用方（E2E-Test.jenkinsfile）的行为差异提示

| 现网行为 | 证据 | TestPlane 适配时应避免 |
|---|---|---|
| 靠"下游 Jenkins 构建"存在性做超时分层（总 3 天 / 单次 3h） | `E2E-Test.jenkinsfile:400-412`、`:1023-1030` | 直接对着 Genet 接口，只有 `deploy_timeout_seconds` 一层；不要把"release 不存在"当"下游没启动" |
| 重试 = 重新触发整条下游 + 新 release 名 | `E2E-Test.jenkinsfile:1139-1146`、`:512-513` | 重试要复用 `deploy_id` 派生的同名 release（先 GET），否则每次重试都泄漏一个长驻 release |
| 清理靠 post + `stopDownstreamBuildSafe` 双保险 | `E2E-Test.jenkinsfile:834-861`、`:1402-1423` | 适配层需"卸载 + 二次确认 + force 兜底"三件套，缺一必留残卡 |
| 就绪后仍持续探活 `/v1/chat/completions`（窗口 18000s） | `E2E-Test.jenkinsfile:594-608`、`:412` | 把"等待就绪"和"跑测试"分开；`ready` 之后给 Tester 一段 warmup 重试预算 |
