# deploy/genet-local —— 本地 kind 上跑真 Genet（联调用，**不提交回 Genet 仓**）

> 目的：给 TestPlane 的 `internal/genet`（`TESTPLANE_DEPLOY_PROVIDER=genet`）一个**真的**对端，
> 用 Helm 把被测服务装起来，走「release 起来 → endpoint ready → precheck → test → 卸载」。
>
> 本目录的东西**只服务本地联调**：chart 在上游 Genet 仓（`.data/genet`），我们只放
> 「让它能在没有华为内网的环境里跑起来」的那些补丁与最小 values。与上游有漂移，**不回提 Genet**。

## 四个 namespace 各管什么（别搞混）

| namespace | 职责 | 谁创建 |
|---|---|---|
| `genet` | **Genet 自己**（backend / chartrepo / webhook TLS） | `helm install genet -n genet` |
| `genet-open-api` | **OpenAPI 控制态**：`genet-openapi-keys` Secret（key 元数据）。不是服务落地处 | 手工建（`openAPI.namespace`） |
| `testplane-svc` | **真 Genet 部署的被测服务**；也是 API Key 绑定的 namespace（**必须事先不存在**，Genet 带身份标签自建） | 首次创建 API Key 时由 Genet 建 |
| `testplane-mock` | **fake-genet / QA 脚本**装的被测服务；平台 `deploy/charts/mock-llm` 落这里 | `deploy/k8s/mock-namespace.yaml` |
| `testplane` | 平台自己（控制面 + Tester Job + PVC） | `deploy/k8s/namespace.yaml` |

> 真 / 假 Genet 的服务**不要混在同一个 ns**：真 Genet 的 release 由它的 webhook 独占管理
> （手工 `helm uninstall` 会被拒：`Helm release storage is managed exclusively by the Genet backend`），
> 只能通过它的 Open API DELETE。

## 在 kind 上切「真 Genet / fake-genet」（两条命令 + 预期形态）

```powershell
# 切「真 Genet」
kubectl --context kind-testplane -n testplane create secret generic testplane-genet `
  --from-literal=api-key='gk_...' --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
#   configmap: TESTPLANE_GENET_BASE_URL=http://genet-backend.genet.svc:8080  → apply -k + rollout restart

# 切「fake-genet」（kind E2E 默认走这条）
kubectl --context kind-testplane -n testplane create secret generic testplane-genet `
  --from-literal=api-key='tp-local-2026' --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
#   configmap: TESTPLANE_GENET_BASE_URL=http://host.docker.internal:18091 → apply -k + rollout restart
```

| | 真 Genet | fake-genet |
|---|---|---|
| base_url | `http://genet-backend.genet.svc:8080` | `http://host.docker.internal:18091` |
| API key | `gk_…`（管理 API 现建） | `tp-local-2026` |
| 装哪个 chart | 固定 `charts/unified-cache-stack@0.0.1`（我们改名冒充，见下节） | `deploy/charts/mock-llm` |
| 服务落地 ns | `testplane-svc` | `testplane-mock` |
| 预期 endpoint | `http://tp-<sha12>-unified-cache-stack.testplane-svc.svc.cluster.local:18080` | `http://tp-<sha12>-mock-llm.testplane-mock.svc:18080/v1` |
| 卸载 | 走它自己的 Open API DELETE（webhook 独占 Helm release storage） | 直接 `helm uninstall` |

两条命令改完都要 `kubectl apply -k deploy/overlays/with-control-plane` + `rollout restart deploy/testplaned`，
再用 `exec deploy/testplaned -- env | Select-String GENET` 确认。

## 目录

| 文件 | 作用 |
|---|---|
| `backend.Dockerfile` | 上游 `.data/genet/backend/Dockerfile` 的本地补丁版（4 处 FROM/源替换，见文件头注释） |
| `values-min.yaml` | 最小 values：关 OAuth / 用户 RBAC / Volcano / 前端 / webshell / NFS 挂载 / 清理 CronJob |
| `build-chartrepo.ps1` | `helm package` 各 chart → 生成并 apply 假冒 ChartMuseum（ConfigMap + Deployment + Service） |
| `make-chartrepo.ps1` | **一条命令冒充**：复制 `deploy/charts/mock-llm` → 改 name/version → 打包 → 重建 ChartMuseum → 重启 genet 并等 `Chart cache refreshed`（参数 `-Name/-Version`） |

> 假冒 ChartMuseum 存在的理由：Genet 的 `chartRepo` 只认 `harbor-oci`（要真 Harbor artifacts API）
> 或 `harbor-chartmuseum`（`GET /api/chartrepo/<project>/charts[/name[/version]]` +
> `GET /chartrepo/<project>/charts/<name>-<version>.tgz`）。后者用静态 nginx 就能假冒。
> 早期版本用的是手写 `chartrepo.yaml` 模板，已删——现在清单由 `build-chartrepo.ps1` 按实际 tgz 现生成。

## 一次性准备（约 10 分钟）

```powershell
# 0. 前置：kind 集群 + cert-manager（webhook TLS 与 Helm 受保护存储依赖它）
kubectl --context kind-testplane apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.3/cert-manager.yaml

# 1. 构建 Genet backend（上下文 = .data/genet 仓根，不能用 backend/）
docker build -f deploy/genet-local/backend.Dockerfile -t genet-backend:dev .data/genet
& .\.tools\kind.exe load docker-image genet-backend:dev --name testplane

# 2. 两个 namespace：Genet 自己一个，OpenAPI 控制态一个
kubectl --context kind-testplane create namespace genet
kubectl --context kind-testplane create namespace genet-open-api

# 3. 装 Genet（装在 genet，控制态在 genet-open-api；被测服务落在 API Key 绑定的 ns）
& .\.tools\helm.exe install genet .data\genet\helm\genet -n genet `
  -f deploy/genet-local/values-min.yaml --set cronJob.useTimeZoneField=true

# 4. 假 ChartMuseum + 打包我们的 mock-llm chart
pwsh deploy/genet-local/build-chartrepo.ps1
```

`helm upgrade` 之后 Genet 会刷新 chart 缓存，日志里应出现
`Chart cache refreshed {"refreshedProjects":1,...,"totalChartsInCache":1}`。

## 创建 Open API Key（有坑，按下面 5 行做）

**上游 bug**：直接 `POST /api/admin/apikeys` 会 500，日志
`Failed to create api key record {"error":"resource name may not be empty"}`；
用 `openAPI.apiKeys` 静态 key 更糟——启动即 FATAL（迁移路径同一个 bug）。
**绕过**：先手工种一个空 records Secret（Genet 在这个 Secret 上做 CAS），再调管理 API。

复制这 5 行即可（先在一个终端里 `kubectl -n genet port-forward svc/genet-backend 18083:8080` 不要关）：

```powershell
# 1) 种空 records（只需一次；重建集群后要重来）
kubectl --context kind-testplane -n genet-open-api create secret generic genet-openapi-keys --from-literal=records.json='[]' --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
kubectl --context kind-testplane -n genet-open-api label secret genet-openapi-keys "genet.io/managed=true" "genet.io/type=openapi-keys" --overwrite
# 2) 建 key（oauth 关掉后 dev 模式用户是 dev-user，已在 values-min 的 adminUsers 里）
$body = @{ name='testplane-e2e'; namespace='testplane-svc'; scope='write'; poolType='exclusive' } | ConvertTo-Json
$resp = Invoke-RestMethod -Uri 'http://127.0.0.1:18083/api/admin/apikeys?user=dev-user' -Method Post -ContentType 'application/json' -Body $body
$resp.plaintextKey   # ← 明文只出现这一次，立刻存进平台 Secret
# 3) 落到平台侧 Secret（别写进任何仓库文件/ConfigMap/日志）
kubectl --context kind-testplane -n testplane create secret generic testplane-genet --from-literal=api-key=$resp.plaintextKey --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
```

要点：

- **key 明文只在创建响应里出现一次**（`plaintextKey`），之后管理 API 只能拿到 `keyPreview`；
  丢了就删了重建（`DELETE /api/admin/apikeys/{id}`）。
- 明文**只落** `testplane-genet` Secret（key 名 `api-key`），不写进 ConfigMap / 仓库 / 日志。
- 当前环境：key 已建好并写进 `testplane-genet`；**真 Genet 用 `gk_…`，fake-genet 用 `tp-local-2026`**，
  切换时改这个 Secret 即可（见上面「切真/fake」一节）。
- **namespace 语义（容易误解）**：`openAPI.namespace`（`genet-open-api`）是**控制态**（放 keys Secret），
  不是被测服务落地的 ns；服务落到 **API Key 绑定**的 namespace（`testplane-svc`），且该 ns 必须
  **先不存在**（Genet 会带身份标签自己创建，已存在的会 409）。

## 接进 TestPlane（本集群当前状态）

```powershell
kubectl --context kind-testplane -n testplane create secret generic testplane-genet `
  --from-literal=api-key='<上一步的 gk_...>' --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
# ConfigMap: TESTPLANE_DEPLOY_PROVIDER=genet + TESTPLANE_GENET_BASE_URL=http://genet-backend.genet.svc:8080
kubectl --context kind-testplane apply -k deploy/overlays/with-control-plane
kubectl --context kind-testplane -n testplane rollout restart deploy/testplaned
```

**已验证**（2026-11-05）：

```
GET /health                                      → 200
GET /api/open/helm/releases?light=true + Bearer  → 200 {"items":[]}
同上不带 Bearer                                  → 401
GET /api/open/queue                              → 200 {"enabled":false,...}
chartRepo 缓存                                   → totalChartsInCache=1（mock-llm）
```

## Chart「改名冒充」：让真 Genet 装我们的 mock 服务 ✅ 已验证可行

Genet 的 Open API **不能**指定任意 chart——`POST /api/open/helm/uc-packages/stack/releases`
用的永远是配置里的 `backend.config.helm.ucPackages.stack.chart`，本地这份配置是：

```yaml
ucPackages:
  stack: { chart: charts/unified-cache-stack, version: 0.0.1, modelTemplatesPath: models }
```

所以反过来做：把我们的 chart 改成那份 chart 的名字/版本，让 Genet 以为装的是它自己的 stack。
**一条命令**（脚本参数化 chart 名/版本；冒充出来的 chart 是 Genet 侧资产，落 `.data/packages/`、**不入库**）：

```powershell
pwsh deploy/genet-local/make-chartrepo.ps1                                  # 默认 unified-cache-stack 0.0.1
pwsh deploy/genet-local/make-chartrepo.ps1 -Name unified-cache-stack -Version 0.0.1
pwsh deploy/genet-local/make-chartrepo.ps1 -Name other-chart -Version 1.2.3 # 换别的冒充目标
```

它做四件事：复制 `deploy/charts/mock-llm` → 改 `Chart.yaml` 的 name/version → `helm package` +
重建假 ChartMuseum（内部调用 `build-chartrepo.ps1`）→ 重启 `genet-backend` 并**轮询日志等**
`Chart cache refreshed {"totalChartsInCache":≥2}`（缓存 TTL 300s，重启是唯一立刻生效的办法）。

> `build-chartrepo.ps1` 会打包 `deploy/charts/mock-llm` + `.data/packages/` 下**所有**目录，
> 所以**换冒充目标前先删掉旧的**（`Remove-Item -Recurse .data\packages\<旧名>`），
> 否则仓库里会同时挂着两个冒充件。

<details>
<summary>手工版（想理解每一步时看）</summary>

```powershell
Copy-Item -Recurse deploy\charts\mock-llm .data\packages\unified-cache-stack   # 改 Chart.yaml: name/version
pwsh deploy/genet-local/build-chartrepo.ps1                                   # 打包 + 重建 ChartMuseum
kubectl --context kind-testplane -n genet rollout restart deploy/genet-backend # 立刻刷新缓存
kubectl --context kind-testplane -n genet logs -l app=genet-backend --tail=80 | Select-String 'Chart cache refreshed'
```
</details>

装一把（**values 不能为空**，否则 400 `templatePath 或 values 不能为空`；最小给一行就够）：

```powershell
$key = '<gk_...>'
$body = @{ releaseName='tp-ucs-0001'; noWait=$true; timeoutSec=300; values="replicaCount: 1`n" } | ConvertTo-Json
Invoke-RestMethod -Uri "$BASE/api/open/helm/uc-packages/stack/releases" -Method Post `
  -Headers @{ Authorization = "Bearer $key" } -ContentType 'application/json' -Body $body
Invoke-RestMethod -Uri "$BASE/api/open/helm/releases/tp-ucs-0001" -Headers @{ Authorization = "Bearer $key" }
```

**实测结果（2026-11-05，`tp-ucs-0001`）**：

```json
"status": "deployed",
"callInfo": {
  "endpoint": {
    "status": "ready",
    "source": "cluster",
    "url": "http://tp-ucs-0001-unified-cache-stack.testplane-svc.svc.cluster.local:18080",
    "port": 18080
  },
  "serviceName": "tp-ucs-0001-unified-cache-stack",
  "namespace": "testplane-svc",
  "model": "tp-ucs-0001"
},
"pods": [{ "phase": "Running", "ready": true }]
```

**Endpoint 计算规则**（`internal/handlers/helm.go` 的 `pickHelmReleaseCallService` / `helmReleaseServiceScore`）：

| 优先级 | Service 名形态 | 得分 |
|---|---|---|
| 1 | `<release>-router-service` | 0 |
| 2 | `<release>-…-pod0-service` | 1 |
| 3 | `<release>-…-engine-service` | 2 |
| 4 | `<release>-…-ray-head-svc` | 3 |
| 5 | **任意属于该 release 的 Service**（靠 `app.kubernetes.io/instance=<release>` 等 Helm 标签判定） | 4 |

命中后 URL = `http://<svc>.<ns>.svc.cluster.local:<svc 的第一个端口>`。
**结论：不要求特定的 Service 名后缀，也不要求 8000 端口** —— 我们的 chart 用
`<release>-<chartname>` + 18080 就落到了规则 5，`endpoint.status=ready`。
（`model` 字段在 values 里没给 model 时回退成 **release 名**，TestPlane 侧用自己的 `--model_name` 注入覆盖，不影响探活。）

### `values` 最小集合 & `package_type` 的差别（2026-11-05 实测）

**`values` 最小集合 = 任意一行非空 YAML。** 实测只给 `replicaCount: 1` 就能装起来并 `endpoint=ready`
（镜像沿用 chart 默认 `testplane/mock-llm:dev`）。规则来自 `UcStackInstallRequest` 的校验：

| 传入 | 结果 |
|---|---|
| `values: ""`（且 `templatePath` 也空） | **400** `templatePath 或 values 不能为空` |
| `values: "replicaCount: 1"` | 202 → `deployed` → `endpoint.status=ready` |
| `values` 里再给 `image:` | 覆盖 chart 默认镜像（我们联调时用过，非必需） |

**`package_type` 有区别，区别在「装哪个固定 chart」：**

| package_type | 用的 chart（来自 `backend.config.helm.ucPackages`） | 我们仓库里 | 实测 |
|---|---|---|---|
| `stack`（客户端默认） | `charts/unified-cache-stack@0.0.1` | 有（冒充件） | ✅ 202 → deployed → ready |
| `pd` | `charts/unified-cache-pd`（配置里另带 `routerDiscovery: {enabled:true, namespace: kthena-system}`） | **没有** | ❌ 400 `chart download failed: status=404`（假 ChartMuseum 的 404 页） |

也就是说：`pd` 不只是换个 chart 名，它还期待 kthena 的 router 来做 endpoint 发现；本地没有 router，
即使把 chart 补上，endpoint 解析也会走另一条分支。**联调统一用 `stack`**（我们客户端默认就是它）。

## 已知阻塞 / 未完成

1. **真 Genet 路径的「终态卸载」没接上**（backend 修，已派单）：
   Run `r-20261105-Z2K9`（真 Genet）跑到 `succeeded`、`cleanup.started` 也发了，但 release
   `tp-5095cf6caa26` 仍 `deployed`，Genet 日志里**没有**来自 `testplane-controller` 的 DELETE。
   两个连带事实：
   - 手工 `helm uninstall` 会被 inadmission 拒绝：`Helm release storage is managed exclusively by the Genet backend`；
     只能走 `DELETE /api/open/helm/releases/{name}`（实测 200，可清干净）。
   - 所以修复判据应是：终态后必须发 DELETE 并校验 `GET → 404` + `pods == []`，失败落
     `uninstall.state=failed` 交给对账补偿。
   （fake-genet 路线的卸载是好的：Lead 的 Run `r-20261105-N7YP` 卸载后 `helm list` 里已无该 release。）
2. `runs.deploy` 的 §4.4 字段不全：当前只有 `{state, queue_reason, queue_position}`，
   没有 `genet_release_status` / `endpoint_status`（fake 路线同样如此）；backend 正在补齐。
3. 前端镜像未构建（`frontend.replicas=0`，用不到）。
4. 真实集群的 `vc-scheduler /metrics`（`deserved`/`realCapability`）未接，`queue_full` 只能靠
   `queue.state=Closed`。

> 已修复的历史阻塞（留档，别重复排查）：controller 在 `Start` 前没把 `DeployManifest` 冻进
> `runs.deploy`，报 `genet: deploy manifest is empty`。现在已能取到冻结的 manifest 并成功部署。

## namespace 语义（容易踩，务必按这个来）

| 概念 | 值 | 说明 |
|---|---|---|
| Genet 自身 | `genet` | chart 装在它自己的 ns；**不要**装进 `testplane-mock` |
| OpenAPI **控制态** | `genet-open-api` | `openAPI.namespace`，放 `genet-openapi-keys` Secret；**不是**服务落地 ns |
| **API Key 绑定 ns** | `testplane-svc` | 被测服务实际落地在这里；**必须是事先不存在的 ns** —— Genet 会带身份标签自己创建，已存在的会 409 `already exists without a Genet API key identity` |
| 平台自己的 ns | `testplane` | 控制面 + Tester Job；与上面三个都分开 |

创建 key 时 `namespace` 传 `testplane-svc`（当前环境已是这个）。换一个全新的名字即可重新开一个
"服务空间"，例如 `testplane-svc2`；同一个 ns 不能被两个 **write** key 同时占用。

## 与 fake-genet 的分工

- **真 Genet**：验证 `internal/genet` 客户端与真对端的契约（本目录）。当前被上面第 1、2 条挡住。
- **fake-genet**（`tools/fake-genet`，backend-exec 维护）：实现 TestPlane 真正调的那几条端点、
  内部用真 `helm install` 装 `deploy/charts/mock-llm`。功能联调优先用它；
  真 Genet 留作 P1 的契约对齐环境。

启动 fake-genet（真 helm 驱动，装我们的 chart）：

```powershell
kubectl config view --context kind-testplane --minify --flatten | Out-File -Encoding utf8 .data\kubeconfig-kind
$env:KUBECONFIG = (Resolve-Path .data\kubeconfig-kind).Path
.\tools\fake-genet\fake-genet.exe -addr :18091 -helm -kubeconfig $env:KUBECONFIG `
  -chart deploy/charts/mock-llm -namespace testplane-mock -helm-bin .tools/helm.exe -api-key tp-local-2026
```

控制面在集群内 → `TESTPLANE_GENET_BASE_URL=http://host.docker.internal:18091`（已实测 Pod 内可达）。
