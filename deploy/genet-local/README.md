# deploy/genet-local —— 本地 kind 上跑真 Genet（联调用，**不提交回 Genet 仓**）

> 目的：给 TestPlane 的 `internal/genet`（`TESTPLANE_DEPLOY_PROVIDER=genet`）一个**真的**对端，
> 用 Helm 把被测服务装进 `testplane-mock`，走「release 起来 → endpoint ready → precheck → test → 卸载」。
>
> 本目录的东西**只服务本地联调**：chart 在上游 Genet 仓（`.data/genet`），我们只放
> 「让它能在没有华为内网的环境里跑起来」的那些补丁与最小 values。与上游有漂移，**不回提 Genet**。

## 目录

| 文件 | 作用 |
|---|---|
| `backend.Dockerfile` | 上游 `.data/genet/backend/Dockerfile` 的本地补丁版（4 处 FROM/源替换，见文件头注释） |
| `values-min.yaml` | 最小 values：关 OAuth / 用户 RBAC / Volcano / 前端 / webshell / NFS 挂载 / 清理 CronJob |
| `chartrepo.yaml` | 假冒 ChartMuseum（静态 nginx）：Genet 只认 harbor-oci / harbor-chartmuseum，前者要真 Harbor artifacts API |
| `build-chartrepo.ps1` | `helm package deploy/charts/mock-llm` → 塞进 ConfigMap → apply chartrepo.yaml |

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

## 创建 Open API Key（有坑，按下面做）

**上游 bug**：直接 `POST /api/admin/apikeys` 会 500，日志
`Failed to create api key record {"error":"resource name may not be empty"}`；
用 `openAPI.apiKeys` 静态 key 更糟——启动即 FATAL（迁移路径同一个 bug）。
**绕过**：先手工种一个空 records Secret，再调管理 API。

```powershell
# 1) 种空记录（Genet 会在这个 Secret 上做 CAS）
kubectl --context kind-testplane -n genet-open-api create secret generic genet-openapi-keys `
  --from-literal=records.json='[]' --dry-run=client -o yaml | kubectl --context kind-testplane apply -f -
kubectl --context kind-testplane -n genet-open-api label secret genet-openapi-keys `
  "genet.io/managed=true" "genet.io/type=openapi-keys" --overwrite

# 2) 起隧道并创建 key（oauth 关掉后 dev 模式用户是 dev-user，已加进 values 的 adminUsers）
kubectl --context kind-testplane -n genet port-forward svc/genet-backend 18083:8080
# 另一个终端：
$body = @{ name='testplane-e2e'; namespace='testplane-svc'; scope='write'; poolType='exclusive' } | ConvertTo-Json
Invoke-RestMethod -Uri 'http://127.0.0.1:18083/api/admin/apikeys?user=dev-user' -Method Post `
  -ContentType 'application/json' -Body $body
# → 返回 { item:{...}, plaintextKey:"gk_..." }   ← key 只出现这一次
```

**namespace 语义（容易误解）**：`openAPI.namespace` 是**控制态**（放 keys Secret），
不是被测服务落地的 ns；服务落到 **API Key 绑定**的 namespace，且该 ns 必须
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

```powershell
# 1) 复制 chart 并改名（生成物放 .data，**不进仓库**）
$dst = '.data\packages\unified-cache-stack'
New-Item -ItemType Directory -Force -Path (Split-Path $dst) | Out-Null
Copy-Item -Recurse deploy\charts\mock-llm $dst   # 再改 Chart.yaml：name=unified-cache-stack, version=0.0.1

# 2) 打包并推进假 ChartMuseum（脚本会同时装 mock-llm 与本 chart）
pwsh deploy/genet-local/build-chartrepo.ps1
#    Genet 的 chart 缓存 TTL 300s；想立刻生效：
kubectl --context kind-testplane -n genet rollout restart deploy/genet-backend
#    日志应出现 Chart cache refreshed {"totalChartsInCache":2}
```

装一把（**values 不能为空**，否则 400 `templatePath 或 values 不能为空`）：

```powershell
$key = '<gk_...>'
$body = @{ releaseName='tp-ucs-0001'; noWait=$true; timeoutSec=300
           values="replicaCount: 1`nimage:`n  repository: testplane/mock-llm`n  tag: dev`n" } | ConvertTo-Json
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

## 已知阻塞 / 未完成

1. **TestPlane 侧 deploy 冻结缺失**（当前阻塞，非 Genet 问题）：
   提交 `deploy:{provider:genet}` 的 Run，`runs.deploy` 只落了
   `{state,queued_at,deployment_id}`，没有 `manifest` 字段，于是 Genet 客户端报
   `genet: deploy manifest is empty (runs.deploy must be frozen before Start)`，Run 立刻 FAILED。
   需要 controller 在 Start 之前把 `DeployManifest` 冻进 `runs.deploy`。
2. 前端镜像未构建（`frontend.replicas=0`，用不到）。
3. 真实集群的 `vc-scheduler /metrics`（`deserved`/`realCapability`）未接，`queue_full` 只能靠
   `queue.state=Closed`。

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
