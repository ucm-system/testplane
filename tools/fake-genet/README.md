# fake-genet

可独立运行的**假 Genet**：只实现 TestPlane 的 deploy 适配层真正会调的那几条 Helm OpenAPI，
用于在本地/kind 上验证 `internal/genet` 的**真实客户端**（`TESTPLANE_DEPLOY_PROVIDER=genet`），
以及在没有 Genet 环境时做端到端联调。

语义来源：`design/impl/execution.md §9`（Helm release 语义）+ `.data/research/genet-deploy-plan.md §6.2`。

## 端点

| 方法 | 路径 | 行为 |
|---|---|---|
| `POST` | `/api/open/helm/uc-packages/{stack\|pd}/releases` | 校验 `releaseName` 正则 `^[a-z][a-z0-9-]{0,52}$`；同名已存在 → **400**（`...already has protected metadata...`）；执行 `helm upgrade --install <release> <chart> -n <ns>`（**不 `--wait`**，对应 `noWait=true`）→ **202** `{status:"pending-install"}` |
| `GET` | `/api/open/helm/releases/{name}` | **200** `{status, description, callInfo:{model, endpoint:{status,url,message}}, pods:[...]}`；未安装 → **404**；`?light=true` 去掉 `pods`/`callInfo` |
| `DELETE` | `/api/open/helm/releases/{name}?force=&keepHistory=` | `helm uninstall` → **200**；不存在 → **404** |
| `GET` | `/api/open/helm/releases?light=true` | 列表（`/api/open/queue*` 也返回最小结构，避免客户端 best-effort 查询报错） |
| `GET` | `/healthz` | 健康检查（**不需要鉴权**） |

**鉴权**：除 `/healthz` 外都要求 `Authorization: Bearer <API_KEY>`，否则 401。
Key 取值优先级：`-api-key` > 环境变量 `FAKE_GENET_API_KEY` > 默认 `tp-local-2026`。

**namespace**：由"API Key 绑定"（真实 Genet 的语义），本实现用 `-namespace` 固定，**请求体不能指定**。

**endpoint 判定**：`helm status == deployed` **且** 该 release 的所有 Pod `Ready` →
`callInfo.endpoint.status=ready`，`url = http://<release>-mock-llm.<ns>.svc:18080/v1`；
否则 `pending`；`helm status == failed` → `blocked`（message 取 helm 的 description）。

## 两种驱动

| 模式 | 参数 | 说明 |
|---|---|---|
| 内存模拟（默认） | 无 | 不碰集群；安装后 `-sim-ready-after`（默认 3s）变 ready。适合只验证客户端状态机 |
| 真 helm | `-helm` | 在**宿主**上执行 `helm`/`kubectl`，`KUBECONFIG` 指向 kind |

## 起假 Genet

### 1) 内存模拟（最快，验证客户端逻辑）

```powershell
cd D:\Project\TestPalne
go run ./tools/fake-genet -addr :18090
```

### 2) 真 helm（kind 上真装 chart）

```powershell
# 1. 把 kind 的 kubeconfig 摊平到一个文件（宿主上的假 Genet 用它调集群）
kubectl config view --context kind-testplane --minify --flatten | Out-File -Encoding utf8 .data\kubeconfig-kind

# 2. 起假 Genet（注意 KUBECONFIG 用绝对路径）
$env:KUBECONFIG = (Resolve-Path .data\kubeconfig-kind).Path
go run ./tools/fake-genet -addr :18090 -helm `
  -kubeconfig $env:KUBECONFIG `
  -chart deploy/charts/mock-llm `
  -namespace testplane-mock `
  -helm-bin .tools/helm.exe
```

> chart 目录是相对路径，**在仓库根目录启动**；`-helm` 也可以给绝对路径。

### 3) 让 TestPlane 指过来

```powershell
$env:TESTPLANE_DEPLOY_PROVIDER = "genet"
$env:TESTPLANE_GENET_BASE_URL   = "http://127.0.0.1:18090"
$env:TESTPLANE_GENET_API_KEY    = "tp-local-2026"
# 自签/本机 HTTP 不需要 CA；若目标是 https 自签，再加：
# $env:TESTPLANE_GENET_CA_FILE  = "C:\path\to\ca.pem"
```

然后照常起 `testplaned`；`runs.deploy` 里的 `genet_release_status` / `endpoint_status`
会反映假 Genet 的两根轴。

## 手工冒烟（curl）

```bash
BASE=http://127.0.0.1:18090
AUTH='Authorization: Bearer tp-local-2026'

# 安装（202 pending-install）
curl -sS -i -X POST "$BASE/api/open/helm/uc-packages/stack/releases" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"releaseName":"tp-demo0001","noWait":true}'

# 查询（pending → ready 需要等 Pod Ready）
curl -sS "$BASE/api/open/helm/releases/tp-demo0001" -H "$AUTH"

# 卸载（200）；再查应为 404
curl -sS -i -X DELETE "$BASE/api/open/helm/releases/tp-demo0001?force=true" -H "$AUTH"
```

## 测试

```powershell
# 只跑内存逻辑（CI 默认）
go test ./tools/fake-genet

# 真集群端到端（默认跳过）
$env:FAKE_GENET_LIVE = "1"
$env:HELM_BIN = ".tools/helm.exe"
$env:KUBECONFIG = (Resolve-Path .data\kubeconfig-kind).Path
go test -run TestLiveWithHelm ./tools/fake-genet
```

## 与真实 Genet 的差异（**只**用于联调，不是行为契约）

- 没有 chart 校验（真实 Genet 只认配置里的固定 chart；本实现直接装 `-chart` 指定的 chart）。
- 没有 Helm 历史/回滚/superseded 语义；`status` 只有 `pending-install`/`deployed`/`failed`。
- 没有 Volcano 队列数据（`/api/open/queue*` 返回 `enabled:false`）。
- 没有限流、审计、多租户（namespace 固定）。
- 卸载不做"删干净"验证（真实 Genet 要 `GET 404 + pods 为空`；本实现直接删 release）。
