# TestPlane

> 云原生、轻量化、AI Agent 友好的 **LLM 服务测试编排平台**。

这是 TestPlane 的**主仓库**：契约、原型、Tester，以及后续的控制面与 Web UI 都落在这里。

当前处于**设计先行**阶段——先把契约与形态钉死，再按契约补实现。已经落地的三样东西不是"全部内容"，而是后续实现的**输入**：设计文档定裁决，原型定信息结构与状态谱，参考 Tester 定契约的落地样子。

## 进度

| 部分 | 状态 | 位置 |
|---|---|---|
| 设计约束文档（系统边界 / 契约 / 语义） | ✅ 定稿（v0.6） | [`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) |
| 可点击原型（6 个页面 + 全状态谱） | ✅ 可交互 | [`design/prototype/`](design/prototype/) |
| 参考 Tester：endpoint-probe（Pre-Check） | ✅ 可运行 | [`testers/endpoint-probe/`](testers/endpoint-probe/) |
| 实际 Tester：aisbench-sla（前缀缓存 SLA 搜索） | ✅ 可运行（离线 stub 已验证；真实 SLA 需 ais_bench 镜像 + 权重） | [`testers/aisbench-sla/`](testers/aisbench-sla/) |
| 实现技术方案（总览 + 三份分册） | 🟢 提案 v3（12 项决策已并入） | [`design/IMPLEMENTATION.md`](design/IMPLEMENTATION.md) |
| 4+1 设计视图（场景 / 逻辑 / 数据 / 进程 / 开发 / 部署 / API） | 🟢 可开发基线 | [`design/views/`](design/views/) |
| 契约：OpenAPI 3.1（25 operations） | ✅ 定稿 | [`server/api/openapi.yaml`](server/api/openapi.yaml) |
| 控制面：API / 状态机 / 编排 / 存储 | ✅ **MVP 已实现** | [`server/`](server/) |
| Executor：K8s Job、超时与取消、日志采集 | ✅ **MVP 已实现**（真 Kind 集群跑通） | [`server/internal/executor/`](server/internal/executor/) |
| Genet 适配：部署 / 状态查询 / Endpoint | 🟡 **MVP 为 mock**，接口已留 | [`server/internal/genet/mocks/`](server/internal/genet/mocks/) |
| Tester Registry：目录发现与校验 | ✅ **MVP 已实现**（启动 + 周期扫描） | [`server/internal/registry/`](server/internal/registry/) |
| Web UI：列表 / 详情 / 报告 / 日志 / 提交 | ✅ **MVP 已实现**（6 路由） | [`web/`](web/) |
| 部署与镜像（Kind / K8s 清单 / 镜像构建） | ✅ 可用 | [`deploy/`](deploy/) |
| 测试：单测 / 契约测试 / 端到端 / 失败注入 | ✅ 可用 | [`tools/`](tools/)、[`scripts/e2e/`](scripts/e2e/) |
| P1：MCP Server / CLI / 失败续跑 / Run 对比 | ⬜ 之后 | 见 §14.2 |

P0 的完整验收清单是 [`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) §14.1 的 17 项，上表只是按模块归了类；验收以那 17 项为准，不以本表为准。MVP 的逐项状态（做过 / mock / 未做）与偏差清单见 [`design/IMPLEMENTATION.md`](design/IMPLEMENTATION.md) §15 与 [`docs/runbook/acceptance.md`](docs/runbook/acceptance.md)。

### 快速开始（本机 Kind，约 10 分钟）

前置：Docker Desktop 在跑、`kubectl` 在 PATH、Python/Go/Node 就绪。命令都在**仓库根**执行（PowerShell）。

```powershell
# 0. 装 kind 到仓库内（.tools/ 已 gitignore）
New-Item -ItemType Directory -Force -Path .tools | Out-Null
Invoke-WebRequest -Uri 'https://kind.sigs.k8s.io/dl/v0.29.0/kind-windows-amd64' -OutFile '.tools\kind.exe' -UseBasicParsing

# 1. 基础设施：Kind 集群（含宿主 .data\storage → 节点 /storage-root 挂载）+ PostgreSQL 16 + 存储 + RBAC
#    kind 配置是模板（__STORAGE_HOST_PATH__ 占位符），先渲染再用；产物在 .data\ 下、不入库
pwsh deploy\kind\render.ps1
& .\.tools\kind.exe create cluster --config .data\kind-testplane.yaml --wait 180s
docker volume create testplane-pg-data
docker run -d --name testplane-pg --restart unless-stopped `
  -e POSTGRES_DB=testplane -e POSTGRES_USER=testplane -e POSTGRES_PASSWORD=testplane `
  -e TZ=Asia/Shanghai -p 5432:5432 -v testplane-pg-data:/var/lib/postgresql/data postgres:16-alpine
kubectl --context kind-testplane apply -k deploy/k8s

# 2. 构建镜像并灌进集群（kind 节点看不到宿主镜像库，必须 kind load）
#    有 make 时用 `make build`；Windows 原生没有 make 时用下面两行等价命令
pnpm --dir web install; pnpm --dir web build
Remove-Item -Recurse -Force server\internal\webui\dist -ErrorAction SilentlyContinue
Copy-Item -Recurse web\dist server\internal\webui\dist
Push-Location server; $env:CGO_ENABLED=0; go build -trimpath -o ..\bin\testplaned.exe ./cmd/testplaned; Pop-Location
docker build -f deploy/docker/server.Dockerfile -t testplane-server:dev .
& .\.tools\kind.exe load docker-image testplane-server:dev --name testplane

# 3. 起控制面（启动时自动迁移；也可单独跑 migrate Job，见 runbook）
kubectl --context kind-testplane apply -k deploy/overlays/with-control-plane
kubectl --context kind-testplane -n testplane scale deploy/testplaned --replicas=1
kubectl --context kind-testplane -n testplane rollout status deploy/testplaned --timeout=180s

# 4. 被测服务（mock LLM 顶替）+ 提交第一个 Run
kubectl --context kind-testplane apply -f scripts/e2e/kind/mock-llm.yaml
kubectl --context kind-testplane -n testplane port-forward svc/testplane 8080:8080   # 另开一个终端跑下面的 curl
curl -sS -X POST http://127.0.0.1:8080/api/v1/runs -H 'Content-Type: application/json' -d '{"run":{"name":"smoke","service":{"url":"http://mock-llm.default.svc:18080/v1","model_name":"mock-llm"},"precheck":"auto","tests":[{"name":"probe","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"}}]}}'
```

**判据**：`kubectl -n testplane get pods` 里 `testplaned-*` 是 `1/1 Running`；
`/healthz` 与 `/readyz` 都返回 `ok`；Run 从 `queued` 走到 `succeeded`，两个 Task（precheck + probe）都成功。

改了代码之后重灌集群是**五步**（构建 → load → apply → scale → 验健康），顺序不能换，见
[`docs/runbook/local-kind.md`](docs/runbook/local-kind.md) §0；排障、ConfigMap 必填项、常见坑也在那里。
17 项 P0 验收的逐项状态（做过 / mock / 未做 + 证据）见 [`docs/runbook/acceptance.md`](docs/runbook/acceptance.md)。

## 目录

```text
design/               设计文档与可点击原型（已有）
  ARCHITECTURE.md     设计裁决集合（1800+ 行）
  IMPLEMENTATION.md   实现技术方案：选型 / 模块 / 交付路线 / 待拍板项
  impl/               实现分册：控制面、执行层、Web 与交付
  views/              4+1 设计视图：用例、逻辑、数据、进程、开发、部署、API
  prototype/          单文件原型 + 渲染截图 + 自托管字体

testers/              Tester 实现，一个目录一个 Tester（已有）
  endpoint-probe/     Pre-Check；契约的参考实现，也是新 Tester 的模板
  aisbench-sla/       实际 Tester：前缀缓存 SLA 搜索（最大并发 / 最高吞吐）；
                      构建上下文 = Tester 目录（与发版矩阵一致）

server/               规划：控制面（API / 状态机 / 编排 / 调度 / 存储）
  cmd/tpctl/          P1：Agent 亲和 CLI（submit / wait / report / logs / cancel）
  internal/mcp/       P1：MCP Server（stdio + JSON-RPC 2.0）
web/                  规划：Web UI，按 design/prototype 落地
deploy/               规划：镜像构建、K8s 清单、配置
```

> `server/`、`web/`、`deploy/` **尚未创建**，是给 P0 实现预留的位置。目录一落地就同步更新本表与上面的进度表。

## 接口

- API 前缀 `/api/v1`（Web 控制台调用的全部资源都在这下面）
- 元信息 `/api/v1/meta`
- 健康检查 `/healthz`（不带版本前缀，供探针使用）

页面上不再有全局页脚展示这些入口：技术前缀属于部署与联调信息，放进本文档即可。

## 设计文档：约束，不是需求

[`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) 是**设计裁决**的集合：

- 写的是裁决（必须 / 不得 / 应该 / 可以），不是背景论证与用户画像；
- 与实现冲突时**以文档为准**；确需变更时按 §15 追加裁决条目、升版本号并记录，不做静默修改；
- 显式列出 Non-Goals（§13），防止实现蔓延。

它曾以 `PRD.md` 命名，现已停用——需求类文档另行维护，不用设计文档代替需求基线。

## 原型：双击就能看

无需构建、无需服务器、无需联网：

```text
双击 design/prototype/index.html
```

- 单文件 HTML + vanilla JS，数据是 mock，刷新即还原；
- 覆盖 6 个页面与完整状态谱：成功 / 部分失败 / 服务不可用 / 执行中 / 已取消 / 不部署跑已有服务；
- 截图在 [`design/prototype/screenshots/`](design/prototype/screenshots/)，设计说明在 [`design/prototype/README.md`](design/prototype/README.md)；
- 字体自托管（`assets/fonts/`），离线也不回落系统字体。

原型是**实现 Web UI 时的参照物**，不是可交付的产品——它没有后端、不做校验、所有操作只改内存。

## Tester：契约的参考实现

[`testers/endpoint-probe/`](testers/endpoint-probe/) 既是可用的 Pre-Check Tester，也是新 Tester 的模板：

- 平台只认 `tester.yaml` + `run.sh`，用 argv 调它，不关心内部怎么实现；
- 输出统一落 `/workspace/output`，`result.json` 是唯一必产出文件；
- 不依赖平台也能跑：`docker run <image> --p1=1 --p2=2`，或本地 `./run-local.sh --url ... --model_name ...`。

新 Tester 从复制这个目录开始，改 `tester.yaml` 与 `run.sh` 即可。

细节见[该目录的 README](testers/endpoint-probe/README.md)。

## 实现阶段不能"顺手优化"的几条

这些在文档里已经钉死，实现时最容易想当然地违反：

| 约束 | 出处 |
|---|---|
| 公共参数（`url` / `model_name` / `model_path`）只从 `deploy` XOR `service` 一处来，且**恒由平台注入**，Tester 参数里不得重复 | §5.4.1 |
| 平台**不渲染、不校验** Tester 参数，只做词法切分后原样传 argv | §5.3、§13.1 |
| `SKIPPED`（没跑）与 `FAILED`（跑了但失败）严格区分，不混用 | §7 |
| 日志按 Pod 采集、与 Pod 生命周期解耦；测试期间日志还在长 | §8.6 |
| 只有 K8s 一种 Executor，不做 SSH / Docker 执行后端 | §6.4、§13.1 |
| 平台只产出事实，**不做 Gate 判定**、不替 Agent 下结论 | §13.1 |
| 契约字段（附录 A/B/C）变更属破坏性变更，必须升 `schema_version` 并给兼容期 | §15 |

## 不属于 TestPlane 的部分

部署归 Genet，调度与执行归 K8s / Volcano，测试算法归 AISBench / EvalScope——平台只编排、只记录事实。完整 Non-Goals 见 §13。

## 协作

> ✅ `main` 已开分支保护：只能走 PR、线性历史、管理员也不能直推；另有 agent 自动评审（见 `docs/agents/pr-review.md`）。

流程见 [`CONTRIBUTING.md`](CONTRIBUTING.md)：所有改动走 PR，`main` 只接受 **squash / rebase** 合入（merge commit 已关闭），合并后源分支自动删除。

- 文档改动请在 PR 描述里指出对应章节号（如 §6.7）；
- 实现 PR 请在描述里对应到 §14.1 的验收项；
- 不要提交运行产物、日志与任何凭据（见 `.gitignore`）；
- 脚本与 Dockerfile 强制 LF（见 `.gitattributes`）。

