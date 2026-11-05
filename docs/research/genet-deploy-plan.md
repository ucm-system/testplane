# Genet 本机 kind 部署可行性方案（含「用 Genet 的 OpenAPI 装自制 mock chart」判定）

> 调研时间：2026-11-05 · 目标集群：kind `testplane`（context `kind-testplane`，k8s v1.33.1，单 control-plane 节点）
> 只读调研：**未构建镜像、未部署、未修改** `D:\Project\TestPalne` 与 `.data\genet` 的任何文件（本文件除外），未执行任何 git 命令。

---

## 一句话结论

**能部署，但不能「传 chart」**：Genet 的 Helm OpenAPI 只认配置里那一个固定 chart（`helm.ucPackages.<type>.chart`），请求体里没有 `chart` 字段；**好在那个「固定 chart」本身是 values 可改的**——把它指到我们自建的 ChartMuseum 上的 `charts/mock-llm:0.1.0`，真 Genet 就会装我们的自制 chart。
**推荐路线：B 先跑通（假 Genet + 真 `helm install` 我们的 chart，~1.5–2h），A 作为保真验证（真 Genet 从源码构建，~4–6h）。** 阻塞 A 的不是 Genet 代码，而是 4 处内网基础镜像 + 内网 npm/Go proxy + `helm.enabled` 对 cert-manager 的硬依赖。

---

## 0. 本机环境实测（命令输出）

```
kind get clusters        -> claw-fleet, testplane
kind version             -> kind v0.29.0 go1.24.2 windows/amd64   (.tools\kind.exe，不在 PATH)
kubectl serverVersion    -> v1.33.1（client v1.36.1）
kubectl get nodes        -> testplane-control-plane  Ready  control-plane  v1.33.1  172.18.0.6
                            OS: Debian GNU/Linux 12 (bookworm)  containerd://2.1.1
go version               -> go1.25.4 windows/amd64
node --version           -> v24.11.1        pnpm --version -> 10.33.0
docker version (server)  -> 29.9.2
helm version             -> helm: 术语 'helm' 不会被识别为 cmdlet…（**宿主机未安装 helm**）
docker ps                -> local-registry (registry:latest, 0.0.0.0:5000->5000)、testplane-control-plane、…
kind 节点内镜像(crictl)  -> docker.io/testplane/mock-llm:dev  4f80a6a3ffc5  6.43MB   ← **mock-llm 镜像已在节点里**
```

结论：集群、Docker、Go、Node 都就绪；**唯一缺的工具是 `helm`**（安装方式见 §6.0）。`mock-llm` 镜像已 `kind load` 过，路线 B 不需要再构建任何镜像。

---

## 1. 事实与证据

### 1.1 镜像：能否 `make build-images` 从源码本地构建

| 事实 | 证据 |
|---|---|
| `build-images` = 后端 + 前端 + share-device-plugin 三个镜像 | `Makefile:79` |
| tag 由 `REGISTRY`/`IMAGE_TAG` 决定，默认 `your-registry.io/genet-backend:latest`、`…/genet-frontend:latest` | `Makefile:6-12` |
| 后端镜像：`docker build -t $(BACKEND_IMAGE) -f backend/Dockerfile .`，**build context 必须是仓根**（依赖 `shared/` module） | `Makefile:33`、`backend/Dockerfile:1`、`backend/go.mod:26`（`replace …/shared => ../shared`） |
| 前端镜像：`docker build -t $(FRONTEND_IMAGE) -f frontend/Dockerfile frontend/` | `Makefile:37` |
| **后端 Dockerfile 的 2 个 FROM 全在内网 registry**：`registry.dev.huawei.com/flash_stor/golang:1.21-alpine`、`…/flash_stor/alpine:latest` | `backend/Dockerfile:3`、`backend/Dockerfile:48` |
| 后端 build args：无 `ARG`；靠 `ENV GOPROXY=http://mirrors.tools.huawei.com/goproxy/`、`GONOSUMDB=*`，并把 alpine 源换成 `mirrors.tools.huawei.com` | `backend/Dockerfile:7`、`13-14`、`49` |
| 后端产出 6 个二进制：`api-server`、`genet-helm-post-renderer`、`webshell-gateway`、`volcano-diagnoser`、`cleanup`、`image-cleanup`；运行时 `apk add … helm`（**镜像里自带 helm CLI，Helm 功能靠它**） | `backend/Dockerfile:29-44`、`51` |
| **前端 Dockerfile 的 2 个 FROM 也在内网**：`node:18-alpine`、`nginx:alpine`；并且 `npm set registry http://mirrors.tools.huawei.com/npm/` | `frontend/Dockerfile:2`、`8`、`18` |
| 但**前端依赖全是公网包**（无 `@huawei` 等私有 scope）；`build` = `react-scripts build` | `frontend/package.json:1-40`（deps 均 antd/axios/monaco/react…）、`package.json:31` |
| **Go 依赖无私有模块**：`go.mod` 里只有一条本地 `replace`，其余为 gin/client-go/zap 等公开模块 | `backend/go.mod:5-26` |

**判定**：`make build-images` 在本机**开箱即失败**（4 处 FROM 拉不到），但**只差「换基础镜像」这一层**——Go 依赖与 npm 依赖本身都能从公网拿到。做法（下一轮落文件，本轮不写）：

- 把 `backend/Dockerfile`、`frontend/Dockerfile` 复制到我们仓 `deploy/genet-local/`，把 4 处 `FROM` 换成 `golang:1.21-alpine` / `alpine:3.20` / `node:18-alpine` / `nginx:alpine`，删掉 3 行内网 mirror 替换（`backend/Dockerfile:7,13,49`、`frontend/Dockerfile:8`），保留 `apk add … helm`（alpine 官方仓库有 `helm` 包）。
- 构建 tag 用自有前缀：`local/genet-backend:local`、`local/genet-frontend:local`（绕开 `Makefile:6` 的 `your-registry.io`）。
- **只部署后端、前端用占位 nginx 是不行的**：前端 Deployment 的存活/就绪探针打 `/health`（`helm/genet/templates/deployment-frontend.yaml:39-48`），裸 `nginx:alpine` 对 `/health` 返回 404 → 容器会被反复重启。要么构建真前端，要么做一个带 `/health` 返回 200 的 10 行 nginx 镜像（`nginx.conf` 里 `/health` 的现成写法见 `frontend/nginx.conf:40-44`）。
- **最小方案（不构建前端）**：`frontend.image` 指到一个自建 `local/frontend-stub:local`（`FROM nginx:alpine` + 一个 `location /health { return 200; }` 的 conf）。前端只是 UI，OpenAPI 走 `genet-backend` Service，完全不经过前端。

### 1.2 values.yaml 里的内网耦合点

| 键 | 内网值 | 证据 |
|---|---|---|
| `backend.image.repository` | `registry.dev.huawei.com/flash_stor/genet-backend` | `helm/genet/values.yaml:24` |
| `frontend.image.repository` | `…/genet-frontend` | `values.yaml:918` |
| `webshellGateway.image` / `cleanup.image` / `imageCleanup.image` | 复用 `genet-backend` 内网镜像 | `values.yaml:878`、`970`、`1024` |
| `backend.config.oauth.enabled` | `true`，且端点是 `uniportal.huawei.com` | `values.yaml:627`、`646-648` |
| `backend.config.cluster.server` / `caData` | 内网 apiserver `https://141.111.32.128:8443` + 内网 CA | `values.yaml:704-705` |
| `backend.config.openAPI.enabled` | **`true`**（注意：`genet-issue.md` 里写的 `false` 已过时） | `values.yaml:740-741` |
| `backend.config.helm.chartRepo` | `harbor-oci` + `http://registry.dev.huawei.com` + `projects: [charts]` | `values.yaml:749-759` |
| `backend.config.helm.ucPackages.stack/pd.chart` | `charts/unified-cache-stack` / `charts/unified-cache-pd`，均 `0.0.1` | `values.yaml:775-783` |
| `backend.config.storage.volumes` | 4 个 Dorado NFS 挂载（`{doradoIP}`） | `values.yaml:369-396` |
| `backend.config.userWorkspaceInit` / `modelBrowser` | NFS `192.168.3.6:/private`、`:/public_model`，默认 `enabled: true` | `values.yaml:830-841` |
| `backend.config.pod.*` | `hostNetwork: true`、`hostIPC: true`、`privileged: true` + **只调度到 GPU 节点**的 affinity | `values.yaml:400-476` |
| `backend.config.proxy` | `http://p_atlas:…@172.19.152.237:8080` | `values.yaml:800-802` |
| `backend.config.prometheusURL` | `http://prometheus-kube-prometheus-prometheus.monitoring:9090` | `values.yaml:855` |
| `backend.affinity` / `frontend.affinity` | 要求 `nvidia.com/gpu.present`/`accelerator` **不存在**（kind 满足） | `values.yaml:38-54`、`928-944` |

### 1.3 「不装也能跑」的开关在哪

- `backend.config.oauth.enabled: false` → 走默认用户 `dev-user`，且审批门自动关闭：`models/config.go:1390-1398`（`IsApprovalEnforced` 在 OAuth 关时恒 false，注释 `values.yaml:622-624`）。
- Volcano 只在 `enabled && manageQueues` 时才 ensure Queue CRD，且**失败只 Warn 不退出**：`cmd/api/main.go:192-198`；`validateVolcanoConfig` 在 `enabled=false` 时直接 return：`models/config.go:1490-1493`。
- `imageCleanup.enabled` 有显式开关：`values.yaml:1001`。
- 每日 GC CronJob **没有 enabled 开关**（`helm/genet/templates/cronjob-gc.yaml:1-5`，永远渲染），只能改 `cleanup.schedule`（`values.yaml:954`）。

### 1.4 Helm release API 的实现与能力边界（关键）

**两个 install 端点，行为不同：**

| 端点 | 请求体里有 `chart` 吗 | 证据 |
|---|---|---|
| `POST /api/open/helm/uc-packages/:type/releases`（`stack`/`pd`） | **没有**。`UcStackInstallRequest` 只有 `releaseName/templatePath/templateId/values/modelPath/image/set/wait/timeoutSec/atomic/dryRun/noWait` | 路由 `cmd/api/main.go:721`；handler `handlers/openapi_helm.go:423-428`；结构体 `models/helm.go:169-184`；chart 由配置注入 `handlers/helm.go:2208-2219` |
| `POST /api/open/helm/releases`（通用） | **有** `chart: project/name` + `chartVersion`，但会被钉死在同一个配置值上 | 路由 `cmd/api/main.go:734`；`handlers/helm.go:3443-3448` 调 `validateFixedUcStackChart` |

- 「固定 chart」的来源：`h.ucPackageChart(type)` 读 `config.Helm.UcPackages[type].Chart`（`handlers/helm.go:159-177`），默认值 `charts/unified-cache-stack` / `charts/unified-cache-pd`（`handlers/helm.go:138-153`）。
- 校验函数写明只允许固定值：
  ```go
  // handlers/helm.go:237-250
  if strings.TrimSpace(chart) != fixedChart {
      return fmt.Errorf("仅支持部署固定 uc package %s chart %s", packageType, fixedChart)
  }
  ```
- **能不能传自定义 chart？**
  - 上传 tgz：**不能**。`backend/internal` 下 `multipart|FormFile|Upload` **0 命中**（全仓 grep 结论），没有任何 chart 上传端点。
  - 指定 repo+chart+version：**不能选 repo**（只有一个全局 `helm.chartRepo`，`values.yaml:752-759`）；**chart 名只能等于配置值**，版本也必须等于配置 `version`（`handlers/helm.go:245-248`）。
  - **但配置值本身可以改**：把 `helm.ucPackages.stack.chart` 改成 `charts/mock-llm`、`version: 0.1.0`，则 `charts/mock-llm@0.1.0` 就是「固定 chart」——**这是绕过「只能装内置模板」的唯一正路**。
- **chart 从哪来**：永远是「从 `chartRepo` 拉」——
  - `harbor-oci`：先打 Harbor artifacts API 取 digest（`internal/registry/harbor_chart.go:405-430`），再 `helm pull oci://`（`handlers/helm.go:111-135`）。→ 需要**真 Harbor**，普通 `registry:2` 不行。
  - `harbor-chartmuseum`：`GET /api/chartrepo/{project}/charts/{name}/{version}` 拿 `urls[]`（失败则回落 `charts/{name}-{version}.tgz`，`harbor_chart.go:618-629`），再下载 `{base}/chartrepo/{project}/{urls[0]}`（`harbor_chart.go:641-662`）。→ **一个静态 nginx 就能假冒**。
  - 未配置 repo 时直接 424：`handlers/helm.go:94-103`（错误串 `Helm Chart 仓库未配置`）。
- **`releaseName`**：正则 `^[a-z][a-z0-9-]{0,52}$`（`handlers/helm.go:36`、校验点 `2730-2732`）。落库时不加前缀，原样使用（`resolveUcStackReleaseName` 只 trim：`handlers/helm.go:2696-2698`）。
- **`values`**：是 **YAML 字符串**，语义是「在 chart 默认 values 之上做深合并」（`handlers/helm.go:2148-2178`：`ChartShowValues` → `mergeYAMLDocuments`），再叠加 chart 内的 `values-genet.yaml` overlay（`2182-2188`）、再叠加 `image/modelPath` 覆盖（`2189-2192`）；**不是全量替换**。并且会被安全策略扫描：`namespace`/`serviceaccountName`/`privileged`/`hostNetwork|hostPID|hostIPC` 受限（`handlers/helm.go:3601-3672`）。
- **`set`**：`[]HelmSetParameter{name,value,type:"set|set-string|set-json"}`（`models/helm.go:286-290`），直接转 helm `--set`（`handlers/helm.go:284-298`）。
- **`namespace`（install 落在哪）**：**不是** API Key 的「池」决定的，而是 **API Key 记录里绑定的 `Namespace` 字段**：
  ```go
  // handlers/openapi_helm.go:39-45  没有 namespace 直接 403
  namespace := strings.TrimSpace(c.GetString("openapiNamespace"))
  if namespace == "" { c.JSON(403, gin.H{"error": "API Key 未绑定命名空间"}) }
  ```
  `openapiNamespace` 来自记录：`auth/apikey.go:104`；`HelmHandler.userContext` 优先用它（`handlers/helm.go:76-80`）。`PoolType` 只进调度策略（`auth/apikey.go:105-108`），**不影响 release 的 namespace**。
- 安装还强制两件事：① 写 release owner 元数据（`handlers/helm.go:2971`、`3506`），② manifest 预渲染安全扫描（`safetyCheckUcStack` `handlers/helm.go:3577-3594`），并且真实 apply 走 post-render 过滤器 `PostRenderPolicyTrustedUC`（`handlers/helm.go:2768`）。

### 1.5 鉴权：Open API Key 怎么造

- 头格式：`Authorization: Bearer <key>`（`auth/apikey.go:37-49`）；`openAPI.enabled=false` 时一律 403（`auth/apikey.go:31-35`）。
- 存储：Secret **`genet-openapi-keys`**（在 `openAPI.namespace`，默认 `genet-open-api`）里的 **`records.json`** key（`internal/k8s/apikeys.go:26-28`）。
- 记录结构 `APIKeyRecord`：`id/name/namespace/poolType/scope/enabled/keyHash/keyPreview/encryptedKey/expiresAt/createdBy`（`models/apikey.go:18-36`）；**只存 `keyHash = sha256(明文) 的 hex`**（`k8s/apikeys.go:454-467`，常量时间比较）。
- 有效 = `enabled==true` 且未过期（`k8s/apikeys.go:475-486`）；`namespace` 为空视为 legacy 非法（`models/apikey.go:42-44` → `auth/apikey.go:60-64` 返回 401）。
- **管理页造的 key 带 `gk_` 前缀 + hex**（`handlers/admin.go:1771`）；但它要求先能登录（OAuth）。
- **本地最省事的路：用 values 里的 legacy 静态 key，让后端启动时自动迁移成 managed 记录**：
  ```go
  // cmd/api/main.go:153-163：OpenAPI 开启时，启动阶段把 config.openAPI.apiKeys 迁移为受管主体，失败即 Fatal
  // internal/k8s/legacy_apikey_migration.go:147-151
  func legacyStaticAPIKeyIdentity(plaintext string) (keyID, namespace string) {
      hash := HashOpenAPIKey(plaintext)                 // sha256 hex
      prefix := hash[:48]
      return "legacy-" + prefix, "genet-legacy-" + prefix
  }
  ```
  迁移出的记录：`Scope=write`、`Enabled=true`、`PoolType=shared`（`legacy_apikey_migration.go:59-66`），并自动建同名 namespace + 配额 + label（`…:168-171`、`196`）。
- 换算示例（已本地算过）：`tp-local-2026` → `sha256=def2a0fa4a7c010e76a765f35f2bfa6df271d481d293d849a439a2fbc79afb93`
  ⇒ keyID = `legacy-def2a0fa4a7c010e76a765f35f2bfa6df271d481d293d849`，namespace = `genet-legacy-def2a0fa…d849`（13+48=61 字符，合法）。
- ⚠️ **升级时会挡住**：只要 `openAPI.apiKeys` 非空，`helm upgrade` 直接 fail（`templates/job-managed-namespace-backfill.yaml:1-3`）。所以**装完第一次后必须把 `apiKeys` 从 values 里清空**再 upgrade。

### 1.6 依赖：CRD / 外部组件

| 组件 | 性质 | 证据 |
|---|---|---|
| **cert-manager** | **硬依赖（一旦 `helm.enabled=true`）**：chart 渲染期就 fail，除非 `webhook.enabled=true` **且** `webhook.certManager.enabled=true` | `helm/genet/templates/configmap-config.yaml:5-7`；后端同样校验 `models/config.go:1457-1467` |
| webhook TLS Secret `genet-webhook-tls` | 由 cert-manager 的 `Certificate` 签发；后端 Pod **挂载该 Secret**，没有就起不来 | `templates/webhook-certificate.yaml:1-4`；`templates/deployment-backend.yaml:113-117`；caBundle 靠注解注入 `templates/webhook-configuration.yaml:8` |
| ClusterIssuer `genet-ca-issuer` | 只有 `ingress.enabled=true` 且 `ingress.certManager.enabled=true` 才渲染（`templates/cert-manager.yaml:1-2`）；而 webhook 证书 `issuerRef` 指向它（`values.yaml:1124-1129`）⇒ **必须留 `ingress.enabled=true`**（不需要真 ingress controller，也可以把 `ingress.tls.enabled=false` 免得要 `genet-tls`） | `values.yaml:1055-1077` |
| Volcano（`Queue`/`PodGroup` CRD、vc-scheduler） | **可关**。`volcano.enabled=false` 跳过 ensure；即便开着失败也只 Warn | `values.yaml:71-78`；`cmd/api/main.go:192-198`；`models/config.go:1490-1493` |
| `genet-share-device-plugin` | **可关**（只有共享池要申请 `genet.io/share-*` 资源时才需要）。但默认 `schedulingMode: nativeProxySharing` 指向它 | `values.yaml:163`；独立 chart `helm/genet-share-device-plugin/` |
| Prometheus | **可关**：`prometheusURL` 留空即不从 Prometheus 取指标（注释 `values.yaml:853-855`） | `values.yaml:855` |
| `genet-accelerator-exporter` | 可关（独立 chart；`hostDockerMetric.enabled` 默认 true 但指标缺了只影响展示） | `values.yaml:860-874` |
| Redis | 可选（WebShell 会话），不配则退化 | `models/config.go:1325-1330` |
| NFS（Dorado / 192.168.3.6） | **必须关**（否则 `required` 校验+挂载失败） | `values.yaml:830-841`；`deployment-backend.yaml:101-112` |
| Harbor（chart 仓库） | **硬依赖（Helm 功能）** | `handlers/helm.go:94-103` |
| `helm` 二进制 | 后端镜像内自带 | `backend/Dockerfile:51` |

**「没有 Volcano 也能跑」= 能。** 只需 `backend.config.volcano.enabled=false`（`values.yaml:72`），Queue CRD 不会被创建也不会阻塞启动；`GET /api/open/queue` 之类会退化，但 Helm install 生命周期不依赖它。

---

## 2. 最小 values 清单（逐项）

> 文件名建议：`D:\Project\TestPalne\.data\research\values-local.yaml`（**本轮未落盘**）。

| # | 键 | 必须值 | 理由 |
|---|---|---|---|
| 1 | `backend.image.repository` / `.tag` | `local/genet-backend` / `local` | 默认内网 registry（`values.yaml:24`） |
| 2 | `frontend.image.repository` / `.tag` | `local/genet-frontend` / `local`（或 stub） | 默认内网 registry（`values.yaml:918`） |
| 3 | `backend.replicas` / `frontend.replicas` / `webshellGateway.replicas` / `volcanoDiagnoser.replicas` | 全 `1` | 单节点 kind，默认 2 副本会互相抢 `Recreate` 窗口（`values.yaml:28,922,881`） |
| 4 | `webshellGateway.image.repository/tag`、`cleanup.image.*`、`imageCleanup.image.*` | 同 #1 | 三处都复用 backend 内网镜像（`values.yaml:878,970,1024`） |
| 5 | **`backend.config.oauth.enabled`** | **`false`** | 关掉 `uniportal.huawei.com` OAuth，走 `dev-user`，审批门自动失效（`values.yaml:627`、`config.go:1390-1398`） |
| 6 | **`backend.config.openAPI.enabled`** | **`true`** | 默认已是 true，显式写上防回归（`values.yaml:741`） |
| 7 | **`backend.config.openAPI.apiKeys`** | **`["tp-local-2026"]`** | 启动即迁移成 write key（`main.go:153-163`）。**第二次 `helm upgrade` 前必须清空** |
| 8 | `backend.config.openAPI.namespace` | `genet-open-api` | 控制命名空间，默认值即可（`values.yaml:742`） |
| 9 | **`backend.config.helm.enabled`** | **`true`** | 否则 Helm API 一律 403（`handlers/helm.go:94-98`） |
| 10 | **`backend.config.helm.chartRepo.type`** | **`harbor-chartmuseum`** | 只有这个模式能用静态文件服务假冒（`harbor_chart.go:611-645`）；`harbor-oci` 要真 Harbor API |
| 11 | **`backend.config.helm.chartRepo.url`** | `http://chartmuseum.genet-charts.svc.cluster.local` | `IsConfigured` 只看 URL 非空（`harbor_chart.go:90-92`） |
| 12 | `backend.config.helm.chartRepo.projects` | `["charts"]` | project 白名单校验（`harbor_chart.go:109-117`） |
| 13 | **`backend.config.helm.ucPackages.stack.chart` / `.version`** | **`charts/mock-llm` / `0.1.0`** | 这就是「固定 chart」，改成我们的 chart 名（`values.yaml:776-778`、`helm.go:237-250`） |
| 14 | `backend.config.helm.ucPackages.stack.modelTemplatesPath` | `models`（可留默认） | 仅 uc-packages 模板浏览用 |
| 15 | `backend.config.helm.timeoutSec` | `300` | 本地无需 600s |
| 16 | `backend.config.helm.valuesGuard.*` | 保持 `allowHostPath: ["/"]`、`allowHostNetwork/Privileged: true` | 预扫 manifest（`values.yaml:769-772`、`helm.go:3567-3575`） |
| 17 | **`backend.config.accessPolicy.enabled`** | **`false`** | 观察模式也会起 controller + 打 Warning 头；关掉最干净（`values.yaml:141`、`runtime_policy.go:26-29`）。注意 `controlNamespace` 由模板自动填 `Release.Namespace`（`_helpers.tpl:155`），校验恒过 |
| 18 | **`backend.config.userWorkspaceInit.enabled`** | **`false`** | 否则要求内网 NFS server 且挂载失败（`values.yaml:831`、`deployment-backend.yaml:104`） |
| 19 | **`backend.config.modelBrowser.enabled`** | **`false`** | 同上（`values.yaml:838`） |
| 20 | `backend.config.storage.volumes` | `[]` | 4 个 Dorado NFS 挂载（`values.yaml:369-396`） |
| 21 | `backend.config.storage.allowUserMounts` | `false` | 减少误用 |
| 22 | `backend.config.proxy.httpProxy/httpsProxy` | `""`（`noProxy` 留 `localhost,127.0.0.1,.cluster.local`） | 内网代理会污染集群内调用（`values.yaml:800-804`） |
| 23 | `backend.config.prometheusURL` | `""` | 内网 Prometheus 不存在（`values.yaml:855`） |
| 24 | `backend.config.hostDockerMetric.enabled` | `false` | 无 exporter（`values.yaml:864`） |
| 25 | **`backend.config.volcano.enabled`** | **`false`** | 无 Volcano CRD（`values.yaml:72`） |
| 26 | `backend.config.volcano.manageQueues` / `autoCapacity` | `false` | 双保险，避免 ensure Queue |
| 27 | `backend.config.volcano.diagnostics.enabled` | `false` | 默认已关（`values.yaml:94`） |
| 28 | **`backend.config.gpu.availableTypes`** | **只留一项 CPU**：`[{name: "CPU AMD64", platform: "amd64", resourceName: "", nodeSelector: {"kubernetes.io/arch": "amd64"}}]` | 非空是硬校验（`config.go:1404-1406`）；NVIDIA/Ascend 项的 nodeSelector 在 kind 永不匹配 |
| 29 | `backend.config.gpu.nodePool.enabled` | `false` | 不需要打 non-shared 污点（`values.yaml:182`） |
| 30 | `backend.config.gpu.poolPolicies.*` | 保持默认（`shared: nativeProxySharing`） | `schedulingMode` 是必填且只能取固定值（`config.go` `validateGPUPoolPolicy`）；不建 Pod 就不用装 device plugin |
| 31 | `backend.config.pod.hostNetwork` / `hostIPC` / `securityContext.privileged` | `false` / `false` / 去掉 privileged | 用户 Pod 默认值；kind 上不需要（`values.yaml:400-440`） |
| 32 | `backend.config.pod.affinity` | `{}` | 默认 affinity 要求 GPU/Ascend 标签（`values.yaml:456-476`） |
| 33 | `backend.config.kubeconfig.mode` | `cert`（保持） | 默认即可（`values.yaml:715`） |
| 34 | `backend.config.cluster.server` / `caData` | 可用假值（`https://kubernetes.default.svc` / 任意 base64） | 只用于**给用户下发**的 kubeconfig，后端自己用 in-cluster config；但字段存在更好读 |
| 35 | `backend.config.adminUsers` | `["dev-user"]` | OAuth 关闭后用户名固定 `dev-user`，管理页才进得去 |
| 36 | `backend.config.ui.*` | `cpuOptions:["1","2"]`、`memoryOptions:["1Gi","2Gi"]`、`defaultCPU:"1"`、`defaultMemory:"1Gi"`、`shmSizeOptions:["64Mi"]`、`imageTransfer.url:""` | 默认 16C/64Gi 在单节点无意义（`values.yaml:287-308`） |
| 37 | `backend.config.registry.url/username/password` | 留空或 `local-registry:5000` | 只影响镜像保存/搜索（`values.yaml:811-821`） |
| 38 | `backend.config.images.nerdctl` | `""` | 只用于 commit（`values.yaml:825`） |
| 39 | **`ingress.enabled`** | **`true`（必须保留）** | 只有它为 true 才渲染 ClusterIssuer `genet-ca-issuer`（`templates/cert-manager.yaml:1-2`），而 webhook 证书依赖它 |
| 40 | `ingress.tls.enabled` | `false` | 免掉 `genet-tls` 证书申请（`templates/ingress.yaml:25-33`） |
| 41 | `ingress.className` | `nginx`（保持；集群里没有 ingress controller 也不影响） | 只是资源闲置 |
| 42 | `certManager.createClusterIssuer` | `true`（保持） | 见 #39 |
| 43 | `webhook.enabled` / `webhook.certManager.enabled` | `true` / `true`（**不能关**） | `configmap-config.yaml:5-7` 渲染期 fail |
| 44 | **`cleanup.schedule`** | **`"0 3 1 1 *"`**（或注释掉风险自评） | 默认每晚 23:00 删用户 Pod（`values.yaml:954`）；CronJob 没有 enabled 开关（`cronjob-gc.yaml:1-5`） |
| 45 | `imageCleanup.enabled` | `false` | 默认 true（`values.yaml:1001`） |
| 46 | `cronJob.useTimeZoneField` | `true` | k8s 1.33 ≥ 1.25，直接交给 `spec.timeZone`（`values.yaml:19`、`cronjob-gc.yaml:8-11`） |
| 47 | `timezone` | `Asia/Shanghai`（保持） | — |
| 48 | `cliDownloads.routeToIndependentService` | `false`（保持） | 不装独立 chart |
| 49 | `frontend.nodeSelector/affinity`、`backend.affinity` | 保持默认即可 | kind 节点无 GPU 标签，`DoesNotExist` 匹配通过（`values.yaml:38-54`） |

**前 5 项（最关键，缺一必挂）**：`backend/frontend.image.*` → `local/*`；`oauth.enabled=false`；`openAPI.enabled=true` + `openAPI.apiKeys=[...]`；`helm.chartRepo.{type,url,projects}` + `helm.ucPackages.stack.chart=charts/mock-llm@0.1.0`；`userWorkspaceInit.enabled=false` + `modelBrowser.enabled=false`。

---

## 3. 依赖矩阵

| 组件 | 类别 | 本地 kind 可用？ | 关闭方式 / 影响 |
|---|---|---|---|
| **cert-manager**（CRD `Certificate`/`ClusterIssuer`） | **硬（仅当 helm 功能开）** | 需联网装（`quay.io/jetstack` 镜像） | **不能关**：`configmap-config.yaml:5-7` 渲染期 fail；不装 cert-manager 则 `Certificate` apply 失败 → helm install 失败 |
| **webhook + TLS Secret** | **硬（chart 设计）** | 随 cert-manager 就绪 | 关不掉（同上）；`failurePolicy: Fail` 只作用于带 `genet.io/managed=true` 的 namespace（`webhook-configuration.yaml:21-24`），我们的 `genet-legacy-*` 不带该 label ⇒ **不受 webhook 拦截** |
| **Harbor / ChartMuseum** | **硬（Helm 功能）** | 用 `harbor-chartmuseum` + 静态 nginx 假冒 | 不配 → 424 `Helm Chart 仓库未配置`（`helm.go:99-102`） |
| `helm` CLI | 硬（后端镜像内） | 官方 `apk add helm` 可装 | 无 |
| **Volcano**（`scheduling.volcano.sh` Queue/PodGroup、vc-scheduler） | **软** | 不装 | `volcano.enabled=false`；ensure 失败只 Warn（`main.go:192-198`） |
| `genet-share-device-plugin`（`genet.io/share-*`） | 软 | 不装 | 只在共享池建 GPU Pod 时需要；默认 `nativeProxySharing`（`values.yaml:163`） |
| Prometheus / Grafana | 软 | 无 | `prometheusURL=""`；热力图只剩 K8s 视角（`values.yaml:853-855`） |
| `genet-accelerator-exporter`（ServiceMonitor CRD） | 软 | 不装 | `hostDockerMetric.enabled=false` |
| NFS（Dorado、192.168.3.6） | **硬（默认开启时）** | 不可达 | `userWorkspaceInit.enabled=false` + `modelBrowser.enabled=false` |
| Redis | 软 | 无 | 只影响 WebShell 会话 |
| Ingress controller（nginx） | 软 | 无需 | Ingress 资源闲置；但要保留 `ingress.enabled=true` 以出 ClusterIssuer |
| `ray.io` / `monitoring.coreos.com` / `workload.serving.volcano.sh` | 软 | 无 | 只是 RBAC/webhook 规则里列出的 apiGroup，缺失不报错 |

---

## 4. Helm release API 能力判定（结论前置）

### 4.1 「只能装内置模板」还是「能从 repo 读」

**两者都不是「内置模板/embed」**：chart 来自 `chartRepo` 的网络拉取（`handlers/helm.go:111-135` → 或 `harbor_chart.go:611` 的 HTTP 下载），**但 chart 的 `project/name` 与 `version` 由服务端配置钉死**，不是请求方给。

### 4.2 调用方能不能传自定义 chart

| 能力 | 结论 | 证据 |
|---|---|---|
| 上传 tgz | ❌ 不能 | `backend/internal` 下 `multipart/FormFile/Upload` 0 命中 |
| 请求里指定 repo + chart + version | ❌ 不能 | `uc-packages` 请求体无 `chart`（`models/helm.go:169-184`）；通用端点有 `chart` 但被 `validateFixedUcStackChart` 拒绝非配置值（`handlers/helm.go:3446`、`237-250`）；repo 是全局唯一配置（`models/helm.go:104-112`） |
| `values` 全量覆盖 | ⚠️ 部分：**深合并到 chart 默认值之上**，并过安全策略 | `handlers/helm.go:2148-2178`、`2746`、`3601-3672` |
| `set` 覆盖 | ✅ 可以（`set/set-string/set-json`） | `models/helm.go:286-290`、`handlers/helm.go:284-298` |
| 选 namespace | ❌ 不能：由 API Key 绑定的 namespace 决定 | `openapi_helm.go:39-56`、`auth/apikey.go:104`、`helm.go:76-80` |
| 自选 releaseName | ✅ 可以，正则 `^[a-z][a-z0-9-]{0,52}$` | `handlers/helm.go:36`、`2730-2732` |

### 4.3 那怎么装我们的 mock chart（唯一正路）

**改「固定 chart」= 改配置**：`helm.ucPackages.stack.chart: charts/mock-llm`、`version: 0.1.0`，并把 `helm.chartRepo` 指向一个自建的 ChartMuseum 假服务，里面放 `mock-llm-0.1.0.tgz`。之后：

- `POST /api/open/helm/uc-packages/stack/releases`（TestPlane 现有客户端用的就是这条）能用；
- `POST /api/open/helm/releases` 带 `"chart":"charts/mock-llm","chartVersion":"0.1.0"` 也能用（因为配置值就等于它）。

**默认配置（`charts/unified-cache-stack@0.0.1`）下，答案明确是「不能装自定义 chart」**——请求里任何别的 chart 名都会被 `仅支持部署固定 uc package stack chart …` 拒掉。

### 4.4 依赖与副作用

安装前会 `helm template` 预渲染并扫 manifest（`helm.go:3577-3594`，`valuesGuard` 见 `values.yaml:769-772`），真实 apply 经 post-render 过滤器（`helm.go:2768`），并写 owner 元数据（`helm.go:2971`）。`--wait` 默认开启（`helm.go:2762-2764`），`noWait=true` 可关。

---

## 5. mock chart 设计（**已存在，无需新建**）

位置：`D:\Project\TestPalne\deploy\charts\mock-llm`（`Chart.yaml` / `values.yaml` / `templates/{_helpers.tpl,deployment.yaml,service.yaml}` / `.helmignore`）。

```
deploy/charts/mock-llm/
├── Chart.yaml        # name: mock-llm, version: 0.1.0, appVersion: dev
├── values.yaml       # image testplane/mock-llm:dev, service 18080, 探针 /healthz, 故障注入
├── .helmignore
└── templates/
    ├── _helpers.tpl
    ├── deployment.yaml   # 1 副本，runAsNonRoot 10001，readOnlyRootFilesystem，/healthz 探针
    └── service.yaml      # ClusterIP:18080 → targetPort http
```

关键信息：

| 项 | 值 | 来源 |
|---|---|---|
| chart 名/版本 | `mock-llm` / `0.1.0`（对应 Genet 侧 `charts/mock-llm` + `0.1.0`） | `deploy/charts/mock-llm/Chart.yaml` |
| 镜像 | `testplane/mock-llm:dev`，`pullPolicy: IfNotPresent`，**已在 kind 节点内** | `values.yaml`（image 段）；`docker exec testplane-control-plane crictl images` 输出 |
| 端口 | 容器/Service 均 `18080`（对应 `tools/mock-llm/main.go` 的 `-addr=:18080`） | `tools/mock-llm/main.go` flag 定义 |
| 就绪探针 | `GET /healthz`（永远 200，不受 `ready_after` 影响 ⇒ Pod Ready 与业务就绪解耦） | `tools/mock-llm/main.go` 注释 + `values.yaml` 探针段 |
| 故障注入 | `faultInjection.{failMode,readyAfter,slowMs,errorIn200}` → `MOCK_LLM_*` env | `values.yaml` 注释段 |
| 渲染结果（可对照） | `.data/mock-llm.yaml`（已渲染好的 Service+Deployment，namespace `testplane-mock`） | 本仓既有产物 |

**怎么把 ready URL 回填**：mock chart **不需要**回填——`callInfo.endpoint` 由 Genet 自己算：它列同 namespace 的 Service，`pickHelmReleaseCallService` 选中 release 的 Service，拼 `http://<svc>.<ns>.svc.cluster.local:<port>`（`handlers/helm.go:791-802`、`1451-1495`），`endpoint.status=ready`（`resolveHelmReleaseCallEndpoint`，`helm.go:855+`）。TestPlane 侧判据正是 `endpoint.status=="ready" && url!=""`（`server/internal/genet/genet.go:293-336`）。

⚠️ **Service 名带 release 前缀**：`mock-llm.fullname` 在 release 名不含 `mock-llm` 时拼 `<release>-mock-llm`（`deploy/charts/mock-llm/templates/_helpers.tpl:10-17`）。所以 release 名为 `tp-mock-1` 时 Service 是 **`tp-mock-1-mock-llm`**，URL 为 `http://tp-mock-1-mock-llm.<ns>.svc.cluster.local:18080`（只有 release 名里已含 `mock-llm`，才退化成 `mock-llm`）。假 Genet 必须按同一规则拼，或直接从 K8s 读 Service 名再拼。

> 注意：mock-llm 的 base URL 带 `/v1` 前缀（`http://…:18080/v1`）。`callInfo.endpoint.url` 不带 `/v1`，由 TestPlane 侧拼（`dto.go:152-160` 只接 `url` 原值，拼接在 probe 侧）；不要指望 Genet 返回 `/v1`。

---

## 6. 可复制命令序列

### 6.0 先装 helm（宿主机，缺这个一切免谈）

```powershell
# 方式一（推荐，WinGet）
winget install --id Helm.Helm -e --accept-source-agreements --accept-package-agreements

# 方式二（离线 zip）
Set-Location D:\Project\TestPalne\.tools
curl.exe -LO https://get.helm.sh/helm-v3.16.4-windows-amd64.zip
Expand-Archive .\helm-v3.16.4-windows-amd64.zip -DestinationPath . -Force
.\windows-amd64\helm.exe version --short

# 本会话把 kind 和 helm 放进 PATH（kind 只在 .tools 里）
$env:PATH = "D:\Project\TestPalne\.tools;D:\Project\TestPalne\.tools\windows-amd64;$env:PATH"
kind get clusters          # -> claw-fleet / testplane
helm version --short
```

### 6.1 路线 A：部署真 Genet（~4–6h）

```powershell
$ctx = "kind-testplane"
$genet = "D:\Project\TestPalne\.data\genet"
$repo  = "D:\Project\TestPalne"

# A1. cert-manager（helm.enabled 的硬前置）
kubectl --context $ctx apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.3/cert-manager.yaml
kubectl --context $ctx -n cert-manager wait --for=condition=Available deploy/cert-manager-webhook --timeout=300s
kubectl --context $ctx -n cert-manager wait --for=condition=Available deploy/cert-manager-cainjector --timeout=300s

# A2. 镜像（下一轮先写 deploy/genet-local/Dockerfile.backend 与 Dockerfile.frontend：
#     4 处 FROM 换成公网基础镜像、删掉内网 mirror/npm/goproxy 行）
#     注意 backend 的 build context 必须是 genet 仓根（依赖 shared/）
docker build -t local/genet-backend:local  -f $repo\deploy\genet-local\Dockerfile.backend  $genet
docker build -t local/genet-frontend:local -f $repo\deploy\genet-local\Dockerfile.frontend $genet\frontend

# A3. 灌进 kind（kind 节点不共享宿主 docker 镜像库）
kind load docker-image local/genet-backend:local  --name testplane
kind load docker-image local/genet-frontend:local --name testplane

# A4. 假 ChartMuseum：把 mock-llm-0.1.0.tgz 放到 /chartrepo/charts/charts/ 下
helm package $repo\deploy\charts\mock-llm -d $repo\.data\charts-out
@"
FROM nginx:alpine
COPY mock-llm-0.1.0.tgz /usr/share/nginx/html/chartrepo/charts/charts/mock-llm-0.1.0.tgz
"@ | Set-Content $repo\.data\charts-out\Dockerfile.chartmuseum
docker build -t local/chartmuseum:local -f $repo\.data\charts-out\Dockerfile.chartmuseum $repo\.data\charts-out
kind load docker-image local/chartmuseum:local --name testplane
kubectl --context $ctx create namespace genet-charts --dry-run=client -o yaml | kubectl --context $ctx apply -f -
kubectl --context $ctx -n genet-charts create deployment chartmuseum --image=local/chartmuseum:local
kubectl --context $ctx -n genet-charts expose deployment chartmuseum --port=80 --name=chartmuseum
kubectl --context $ctx -n genet-charts rollout status deploy/chartmuseum --timeout=120s

# A5. 最小 values（内容见 §2；本轮不落盘）
#     $repo\.data\research\values-local.yaml

# A6. 安装（第一次带上 openAPI.apiKeys）
helm --kube-context $ctx upgrade --install genet $genet\helm\genet `
  --namespace genet-system --create-namespace `
  -f $repo\.data\research\values-local.yaml --wait --timeout 10m

# A7. 观察
kubectl --context $ctx -n genet-system get pods -w
kubectl --context $ctx -n genet-system logs deploy/genet-backend --tail=100
```

**A8. 验证（两条 install 路径）**

```powershell
# 端口转发（前端 8080 在容器内也占，宿主用 18080）
kubectl --context $ctx -n genet-system port-forward svc/genet-backend 18080:8080

# ① key 是否有效（应 200 + chart/version）
curl.exe -sS -H "Authorization: Bearer tp-local-2026" `
  http://127.0.0.1:18080/api/open/helm/uc-packages/stack/config

# ② 用 TestPlane 现有客户端会走的那条路（uc-packages）装我们的 chart
#    注意：values 必须给（templatePath 和 values 不能同时为空，helm.go:2169-2171）
curl.exe -sS -X POST http://127.0.0.1:18080/api/open/helm/uc-packages/stack/releases `
  -H "Authorization: Bearer tp-local-2026" -H "Content-Type: application/json" `
  -d '{\"releaseName\":\"tp-mock-1\",\"values\":\"replicaCount: 1\n\",\"noWait\":true}'

# ③ 轮询到 ready，并取出 URL
curl.exe -sS -H "Authorization: Bearer tp-local-2026" `
  http://127.0.0.1:18080/api/open/helm/releases/tp-mock-1
# 期望：status=deployed，callInfo.endpoint.status=ready，
#       callInfo.endpoint.url=http://tp-mock-1-mock-llm.genet-legacy-<48hex>.svc.cluster.local:18080
#       （Service 名 = <release>-mock-llm，见 templates/_helpers.tpl:10-17）

# ④ 业务探活（ClusterIP 只能在集群内，走 port-forward 验）
kubectl --context $ctx -n genet-legacy-<48hex> port-forward svc/tp-mock-1-mock-llm 18081:18080
curl.exe -sS http://127.0.0.1:18081/healthz         # 200
curl.exe -sS http://127.0.0.1:18081/v1/models       # {"data":[{"id":"mock-llm"}]}

# ⑤ 卸载
curl.exe -sS -X DELETE http://127.0.0.1:18080/api/open/helm/releases/tp-mock-1 `
  -H "Authorization: Bearer tp-local-2026"
```

**A9. 收尾**：把 `openAPI.apiKeys` 从 `values-local.yaml` 删掉（否则 `helm upgrade` 会被 `job-managed-namespace-backfill.yaml:1-3` 直接 fail）。

### 6.2 路线 B：假 Genet + 真 `helm install`（~1.5–2h）—— **本机首选**

不需要构建任何 Genet 镜像、不需要 cert-manager、不需要 Harbor。

**B1. 契约（照抄 Genet 的真实响应形状，TestPlane 侧 0 改动）**

| 端点 | 语义 |
|---|---|
| `POST /api/open/helm/uc-packages/{stack\|pd}/releases` | 202 + `{name,requestedName,namespace,packageType,chart,chartVersion,status:"pending-install"}`；后台 `helm upgrade --install <releaseName> deploy/charts/mock-llm -n <ns> --create-namespace`（`noWait=true` 则不 `--wait`） |
| `GET /api/open/helm/releases/{name}` | 200 + `{name,namespace,chart,chartVersion,revision,status,callInfo:{endpoint:{status:"ready",url:"http://<release>-mock-llm.<ns>.svc.cluster.local:18080"},serviceName:"<release>-mock-llm",namespace,port:18080,clusterURL,model:"mock-llm"},pods:[…]}` |
| `GET /api/open/helm/releases` | 200 + `{items:[…]}` |
| `DELETE /api/open/helm/releases/{name}?keepHistory=false&force=false` | 200/204，`helm uninstall` |
| `GET /api/open/helm/releases/{name}/pods` | 200 + `{items:[…]}`（可选） |

状态映射必须与 TestPlane 判据对齐：`endpoint.status=="ready"` 且 `url!=""` ⇒ 就绪（`server/internal/genet/genet.go:327-337`）。

**B2. 命令**

```powershell
$ctx = "kind-testplane"; $repo = "D:\Project\TestPalne"
$env:PATH = "D:\Project\TestPalne\.tools;D:\Project\TestPalne\.tools\windows-amd64;$env:PATH"

# B2.1 造一个 release 专用 namespace（等价于 Genet 的 key 绑定 namespace）
kubectl --context $ctx create namespace testplane-mock --dry-run=client -o yaml | kubectl --context $ctx apply -f -

# B2.2 起假 Genet（下一轮写 deploy/fake-genet/main.go，单文件，stdlib + 调 helm CLI）
go run .\deploy\fake-genet -addr :18081 -kubeconfig "$env:USERPROFILE\.kube\config" `
  -context kind-testplane -namespace testplane-mock `
  -chart $repo\deploy\charts\mock-llm -api-key tp-local-2026

# B2.3 验证（形状与 A8 完全一致，便于将来切真 Genet）
curl.exe -sS -X POST http://127.0.0.1:18081/api/open/helm/uc-packages/stack/releases `
  -H "Authorization: Bearer tp-local-2026" -H "Content-Type: application/json" `
  -d '{\"releaseName\":\"tp-mock-1\",\"noWait\":true}'
curl.exe -sS -H "Authorization: Bearer tp-local-2026" http://127.0.0.1:18081/api/open/helm/releases/tp-mock-1
helm --kube-context $ctx -n testplane-mock list
kubectl --context $ctx -n testplane-mock get pods,svc
curl.exe -sS -X DELETE -H "Authorization: Bearer tp-local-2026" `
  http://127.0.0.1:18081/api/open/helm/releases/tp-mock-1
```

**B3. 保真要点（决定「联调算不算真的」）**

1. `namespace` 固定为 key 绑定值，且**不接受请求方传入的 namespace**（与 `helm.go:76-80` 一致）；
2. 同名 release 已存在时 **POST 返回 400**（真实语义：`ensureFreshHelmInstallTarget`，`helm.go:2700-2723`）；
3. `releaseName` 用同一正则拒绝非法名（`^[a-z][a-z0-9-]{0,52}$`）；
4. 卸载后 `GET` 返回 **404**（TestPlane 的删除确认依赖这个）；
5. 就绪只看 `endpoint.status/url`，**不要**把 `status=deployed` 当就绪（`helm.go:282` 注释与 `genet.go:373` 的 pending 分支）。

---

## 7. 时间估算与风险

| 路线 | 阶段 | 估时 | 主要耗时点 |
|---|---|---|---|
| **B（假 Genet）** | 写 fake 服务 | 0.8h | 单文件 ~300 行 + 4 个端点 + helm 子进程 |
| | 装 helm、建 ns、装 chart 验证 | 0.3h | 镜像已在节点，秒级 |
| | 与 TestPlane 客户端联调（换 base URL） | 0.5h | 现有 `server/internal/genet` 已按真实 DTO 实现 |
| | **合计** | **~1.6h** | 无构建、无外部依赖 |
| **A（真 Genet）** | 改 2 个 Dockerfile + 构建后端（Go 6 个二进制） | 1.0–1.5h | 首次拉 Go modules ~300MB；`golang:1.21-alpine` 交叉编译 |
| | 构建前端 | 0.3–0.5h | `react-scripts build`（公网 npm） |
| | 装 cert-manager + 镜像灌入 | 0.3h | quay.io 镜像拉取 |
| | 假 ChartMuseum（nginx + tgz） | 0.3h | — |
| | values 调参 + 首次 rollout 排障 | 0.8–1.5h | webhook 证书/Secret 挂载、NFS 忘关、affinity 不匹配是高频坑 |
| | 打通 OpenAPI 装 chart + 回填 URL | 0.5–1.0h | 首次会遇到 values 策略/安全预扫拒绝 |
| | **合计** | **~4–6h** | 阻塞点见下 |

**风险 / 阻塞点（按严重度）**

1. **内网基础镜像（4 处 FROM）** → `make build-images` 直接失败。必须复制 Dockerfile 换公网基础镜像；**不能改 `.data\genet`**，所以只能放在我们仓 `deploy/genet-local/`。
2. **`helm.enabled` ⇒ cert-manager + webhook 硬依赖**（`configmap-config.yaml:5-7`）。cert-manager 装不上（无外网）则 **A 路线彻底不可行**，只能走 B。
3. **chart 来源只能是 Harbor/ChartMuseum**：`harbor-oci` 要真 Harbor artifacts API（`harbor_chart.go:405-430`），普通 `registry:2` 不行；**必须用 `harbor-chartmuseum` + 静态 nginx** 才有戏。
4. **前端内网 npm registry**：`frontend/Dockerfile:8` 指向 `mirrors.tools.huawei.com/npm/`；依赖本身是公网的，删掉那行即可，但 4 处 FROM 仍要换。
5. **默认 `cleanup.schedule` 每晚 23:00 删用户 Pod**（`values.yaml:954`），CronJob 无 enabled 开关 ⇒ 长测跨夜会被清掉（`genet-issue.md` 已就此向 Genet 提疑）。
6. **`openAPI.apiKeys` 非空会挡住后续 `helm upgrade`**（`job-managed-namespace-backfill.yaml:1-3`）⇒ 一次性引导、之后靠已迁移记录。
7. **`replicas=2` + `strategy: Recreate`**（`values.yaml:28`、`deployment-backend.yaml:14-15`）：单节点上两个副本必然一个 Pending，`--wait` 会超时 ⇒ 必须 `replicas: 1`。
8. **webhook `failurePolicy: Fail`**：只匹配 `genet.io/managed=true` 的 namespace（`webhook-configuration.yaml:21-24`），我们的 `genet-legacy-*` 不受影响；但 `genet-system` 内的 Helm 元数据 ConfigMap 是 fail-closed（`webhook-configuration.yaml` 第二段），backend Pod 挂掉时手工改 CM 会被拒 —— 排障要先删 webhook 或把 backend 拉起来。
9. **`gpu.availableTypes` 非空是硬校验**（`config.go:1404-1406`），删空会 Fatal；必须留一个 CPU 项。
10. **`values` 不是全量替换**（`helm.go:2148-2178`）：想「完全用我们的 values」是做不到的，只能是「在我们的 chart 默认值上覆盖」。

---

## 8. 结论与推荐

| 问题 | 答案 |
|---|---|
| 能否本机部署 Genet？ | **能**，前提是换基础镜像 + 装 cert-manager + 自建 ChartMuseum 假服务。约 4–6h。 |
| 能否用它的 OpenAPI 装自制 chart？ | **默认不能**（只认 `charts/unified-cache-stack@0.0.1`）；**改 `helm.ucPackages.stack.chart` 后能** —— 这是把「固定 chart」换成我们 chart 的唯一办法。请求体里永远没有 chart 字段、没有 tgz 上传、没有 repo 选择。 |
| 值不值得为联调先做 A？ | **不值得**。TestPlane 侧只依赖 4 个端点 + `callInfo.endpoint` 契约（`server/internal/genet/genet.go`）。**先用 B 联调，把 A 留到需要验证 Genet 真实行为（如 `values` 深合并、`safetyCheck`、元数据归属）时再做。** |

**推荐路线：B 立刻做，A 作为第二阶段保真验证。**
