# Argus

> 云原生、轻量化、AI Agent 友好的**模型评测流水线平台**（原名 TestPlane，v2.0 更名与术语对调见 [`GLOSSARY.md`](GLOSSARY.md)）。

这是 Argus 的**主仓库**：契约、原型、Runner，以及控制面与 Web UI 都落在这里。

当前处于**设计先行**阶段——先把契约与形态钉死，再按契约补实现。已经落地的几样东西不是"全部内容"，而是后续实现的**输入**：设计文档定裁决，原型定信息结构与状态谱，参考 Runner 定契约的落地样子。

面向评审的两篇说明性设计文档：**[总体设计](docs/design/overall-design.md)**（平台整体形态、方案选型、技术原理与部署）与 **[Runner 设计](docs/design/runner-design.md)**（Runner 的模板、规约、注册、执行与版本，原名 `tester-design.md`）。两篇都**不新增设计裁决**（均对应 `ARCHITECTURE` v2.0）；与 `design/ARCHITECTURE.md` 冲突时以它为准。

术语（Task / runner / Runner / attempt、role 四种、exports、输入文件契约、`AG_*` 资产名）以根目录 **[`GLOSSARY.md`](GLOSSARY.md)** 为准。

## 进度

| 部分 | 状态 | 位置 |
|---|---|---|
| 设计约束文档（系统边界 / 契约 / 语义） | ✅ 定稿（**v2.0**，Argus 更名与 Runner 化） | [`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) |
| 术语表（v1→v2 映射与命名消歧） | ✅ | [`GLOSSARY.md`](GLOSSARY.md) |
| 可点击原型（6 个页面 + 全状态谱） | ✅ 可交互 | [`design/prototype/`](design/prototype/) |
| 参考 Runner：endpoint-probe（Pre-Check） | ✅ 可运行（已改 v2 契约：`runner.yaml` + entry + jsonl 输入） | [`runners/endpoint-probe/`](runners/endpoint-probe/) |
| 实际 Runner：aisbench-sla（前缀缓存 SLA 搜索） | ✅ 可运行（真实 SLA 需 ais_bench 镜像 + 权重） | [`runners/aisbench-sla/`](runners/aisbench-sla/) |
| 实现技术方案（总览 + 三份分册） | 🟢 提案 v3（12 项决策已并入；v2 更名随代码改造对齐） | [`design/IMPLEMENTATION.md`](design/IMPLEMENTATION.md) |
| 4+1 设计视图（场景 / 逻辑 / 数据 / 进程 / 开发 / 部署 / API） | 🟢 可开发基线（v2 修订中） | [`design/views/`](design/views/) |
| 契约：OpenAPI 3.1 | ✅ 定稿（**v2**：`/tasks` / `/runners`，与代码一致） | [`server/api/openapi.yaml`](server/api/openapi.yaml) |
| 控制面：API / 状态机 / 编排 / 存储 | ✅ **MVP 已实现**（v1 术语，更名改造进行中） | [`server/`](server/) |
| Executor：K8s Job、超时与取消、日志采集 | ✅ **MVP 已实现**（真 Kind 集群跑通） | [`server/internal/executor/`](server/internal/executor/) |
| Genet 适配：部署 / 状态查询 | 🟡 **MVP 为 mock**；v2 起全部交互由内置 service-deploy / service-clean runner 承载 | [`server/internal/genet/mocks/`](server/internal/genet/mocks/) |
| Runner Registry：目录发现与校验 | ✅ **MVP 已实现**（启动 + 周期扫描） | [`server/internal/registry/`](server/internal/registry/) |
| Web UI：列表 / 详情 / 报告 / 日志 / 提交 | ✅ **MVP 已实现**（6 路由） | [`web/`](web/) |
| 部署与镜像（Kind / K8s 清单 / 镜像构建） | ✅ 可用 | [`deploy/`](deploy/) |
| 测试：单测 / 契约测试 / 端到端 / 失败注入 | ✅ 可用 | [`tools/`](tools/)、[`scripts/e2e/`](scripts/e2e/) |
| P1：MCP Server / CLI（`argus`）/ 跨 Task 续跑（仅外接模式）/ Task 对比 | ⬜ 之后 | 见 §14.2 |

P0 的完整验收清单是 [`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) §14.1（v2.0 共 18 项），上表只是按模块归了类；验收以那 18 项为准，不以本表为准。MVP 的逐项状态（做过 / mock / 未做）与偏差清单见 [`design/IMPLEMENTATION.md`](design/IMPLEMENTATION.md) §15 与 [`docs/runbook/acceptance.md`](docs/runbook/acceptance.md)。

### 快速开始（本机 Kind，约 10 分钟）

> 下述命令按 **v2 命名**书写（控制面 `argusd`、namespace `argus`、`POST /api/v1/tasks`）；`server/`、`deploy/` 与目录更名均已落地，命令即现网命令。

前置：Docker Desktop 在跑、`kubectl` 在 PATH、Python/Go/Node 就绪。命令都在**仓库根**执行（PowerShell）。

```powershell
# 0. 装 kind 到仓库内（.tools/ 已 gitignore）
New-Item -ItemType Directory -Force -Path .tools | Out-Null
Invoke-WebRequest -Uri 'https://kind.sigs.k8s.io/dl/v0.29.0/kind-windows-amd64' -OutFile '.tools\kind.exe' -UseBasicParsing

# 1. 基础设施：Kind 集群（含宿主 .data\storage → 节点 /storage-root 挂载）+ PostgreSQL 16 + 存储 PVC（argus-storage）+ RBAC
#    kind 配置是模板（__STORAGE_HOST_PATH__ 占位符），先渲染再用；产物在 .data\ 下、不入库
pwsh deploy\kind\render.ps1
& .\.tools\kind.exe create cluster --config .data\kind-argus.yaml --wait 180s
docker volume create argus-pg-data
docker run -d --name argus-pg --restart unless-stopped `
  -e POSTGRES_DB=argus -e POSTGRES_USER=argus -e POSTGRES_PASSWORD=argus `
  -e TZ=Asia/Shanghai -p 5432:5432 -v argus-pg-data:/var/lib/postgresql/data postgres:16-alpine
kubectl --context kind-argus apply -k deploy/k8s

# 2. 构建镜像并灌进集群（kind 节点看不到宿主镜像库，必须 kind load）
#    有 make 时用 `make build`；Windows 原生没有 make 时用下面几行等价命令
pnpm --dir web install; pnpm --dir web build
Remove-Item -Recurse -Force server\internal\webui\dist -ErrorAction SilentlyContinue
Copy-Item -Recurse web\dist server\internal\webui\dist
Push-Location server; $env:CGO_ENABLED=0; go build -trimpath -o ..\bin\argusd.exe ./cmd/argusd; Pop-Location
docker build -f deploy/docker/server.Dockerfile -t argus-server:dev .
& .\.tools\kind.exe load docker-image argus-server:dev --name argus

# 3. 起控制面（启动时自动迁移；也可单独跑 migrate Job，见 runbook）
kubectl --context kind-argus apply -k deploy/overlays/with-control-plane
kubectl --context kind-argus -n argus scale deploy/argusd --replicas=1
kubectl --context kind-argus -n argus rollout status deploy/argusd --timeout=180s

# 4. 被测服务（mock LLM 顶替）+ 提交第一个 Task
kubectl --context kind-argus apply -f scripts/e2e/kind/mock-llm.yaml
kubectl --context kind-argus -n argus port-forward svc/argus 8080:8080   # 另开一个终端跑下面的提交

#    提交体是嵌套 task: 结构：deploy（部署模式）与 service（外接模式）二选一；
#    每个 runner 一个输入框（input 为输入原文，jsonl），平台校验后落盘传入。
#    本例走外接模式：不写 deploy → 无 service-deploy / service-clean，只探活。
$manifest = @'
task:
  name: smoke
  service:
    url: http://mock-llm.default.svc:18080/v1
    model_name: mock-llm
    model_path: /models/mock-llm
  precheck: auto
  runners:
    - name: probe
      runner: endpoint-probe
      version: "1.0.0"
      execution: { profile: k8s-npu }
      input: |
        {"wait_timeout_seconds":60,"retry_interval_seconds":15,"max_tokens":1,"stream":true}
'@
curl -sS -X POST http://127.0.0.1:8080/api/v1/tasks -H 'Content-Type: application/yaml' --data-binary $manifest
```

**判据**：`kubectl -n argus get pods` 里 `argusd-*` 是 `1/1 Running`；
`/healthz` 与 `/readyz` 都返回 `ok`；Task 从 `queued` 走到 `succeeded`，两个 runner（pre_check 注入的 endpoint-probe + probe）都成功。

改了代码之后重灌集群是**五步**（构建 → load → apply → scale → 验健康），顺序不能换，见
[`docs/runbook/local-kind.md`](docs/runbook/local-kind.md) §0；排障、ConfigMap 必填项、常见坑也在那里。
P0 验收的逐项状态（做过 / mock / 未做 + 证据）见 [`docs/runbook/acceptance.md`](docs/runbook/acceptance.md)。

## 目录

```text
design/               设计文档与可点击原型（已有）
  ARCHITECTURE.md     设计裁决集合（v2.0，唯一裁决来源）
  IMPLEMENTATION.md   实现技术方案：选型 / 模块 / 交付路线 / 待拍板项
  impl/               实现分册：控制面、执行层、Web 与交付
  views/              4+1 设计视图：用例、逻辑、数据、进程、开发、部署、API
  prototype/          单文件原型 + 渲染截图 + 自托管字体

docs/design/          评审用说明文（不新增裁决，对应 ARCHITECTURE v2.0）
  overall-design.md   总体设计
  runner-design.md    Runner 设计（原 tester-design.md）

GLOSSARY.md           术语表：v2 术语定义、v1→v2 映射与命名消歧

runners/              Runner 实现，一个目录一个 Runner
  endpoint-probe/     Pre-Check；契约的参考实现，也是新 Runner 的模板
  aisbench-sla/       实际 Runner：前缀缓存 SLA 搜索（最大并发 / 最高吞吐）；
                      构建上下文 = Runner 目录（与发版矩阵一致）

server/               控制面（API / 状态机 / 编排 / 存储；MVP 已实现）
  cmd/argus/          P1：Agent 亲和 CLI（submit / status / logs / clean，子命令式）
  internal/mcp/       P1：MCP Server（stdio + JSON-RPC 2.0）
web/                  Web UI，按 design/prototype 落地（MVP 已实现）
deploy/               镜像构建、K8s 清单、配置（helm chart 已更名 deploy/charts/argus/）
```

> 目录与二进制更名（`testers/` → `runners/`、`cmd/testplaned` → `cmd/argusd`、`cmd/tpctl` → `cmd/argus`）已落地；本表与上面的进度表已同步为实际路径。

## 接口

- API 前缀 `/api/v1`（Web 控制台调用的全部资源都在这下面；v2 资源路径为 `/tasks` / `/runners`，见 ARCHITECTURE D-73）
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
- 覆盖 6 个页面与完整状态谱：成功 / 失败（含部分失败） / 服务不可用 / 执行中 / 已取消 / 外接模式跑已有服务；
- 截图在 [`design/prototype/screenshots/`](design/prototype/screenshots/)，设计说明在 [`design/prototype/README.md`](design/prototype/README.md)；
- 字体自托管（`assets/fonts/`），离线也不回落系统字体。

原型是**实现 Web UI 时的参照物**，不是可交付的产品——它没有后端、不做校验、所有操作只改内存。

## Runner：契约的参考实现

[`runners/endpoint-probe/`](runners/endpoint-probe/) 既是可用的 Pre-Check Runner，也是新 Runner 的模板：

- 平台只认 `runner.yaml` + entry（`runner.yaml.entry` 以 exec form 声明，文件名不限），通过**输入文件 + 环境变量**调它，不关心内部怎么实现；argv 恒空，业务参数在 `input`（jsonl）里，公共参数走 `AG_SERVED_*` 注入；
- 输出统一落 `/workspace/output`：产物**三选一**（`result.json` / `result.html` / `result.csv` 至少其一，由 Runner 决定，不强求 `result.json`，D-74）+ 可选 `exports.env`（平台收割注入下游）+ `raw/`、`logs/`；
- 不依赖平台也能跑：`docker run <image> <entry> --params-file ... --url ...`，或本地 `./run-local.sh`（只 run 不 build）。

新 Runner 从复制这个目录开始，改 `runner.yaml` 与 entry 即可；`role: deploy` / `role: clean` 仅内置（service-deploy / service-clean），用户注册会被拒绝。

细节见[该目录的 README](runners/endpoint-probe/README.md)。

## 实现阶段不能"顺手优化"的几条

这些在文档里已经钉死，实现时最容易想当然地违反：

| 约束 | 出处 |
|---|---|
| 公共参数（`AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH`）只从 `deploy` XOR `service` 一处来，恒由平台以环境变量注入；Runner 的输入文件与 `parameters` 里不得出现它们 | §5.4.1 |
| 业务输入走**输入文件**（`AG_PARAMS_FILE`），平台对 jsonl 做**词法 + 结构校验**（类型 + required，报错带行号）后原样落盘；entry 由 yaml 声明、**argv 恒空**，语义归 Runner | §5.2–§5.4 |
| Task 内 runner **严格串行**（线性管道）；**没有** `DEPLOYING` / `CLEANUP` / `PRECHECK` 平台阶段，部署排队可见性由 `deploy-state.json` 承载 | §7.4、§9.1 |
| **runner 级不做平台重试**；重试 = 整 Task 新 attempt 全量重跑，**含 fresh deploy**（不得复用上次已部署服务） | §7.8 |
| deploy / clean 是**内置 runner**（自动配对注入，用户不可注册不可手动添加）；clean 是 **finally 语义**，失败只打 `cleanup_failed`，泄漏用 `argus clean --task` 重放 | §9.1、§7.6、§9.3 |
| `SKIPPED`（没跑）与 `FAILED`（跑了但失败）严格区分，不混用 | §7.1 |
| test / pre_check 产物是**三选一**（`result.json` / `result.html` / `result.csv` 至少其一，不强求 `result.json`）；`RESULT_MISSING` = 三者全缺，`RESULT_INVALID` 仅在 `result.json` 产出但不合法时适用 | §5.3、D-74 |
| 日志按 Pod 采集、与 Pod 生命周期解耦；测试期间日志还在长 | §8.6 |
| 只有 K8s 一种 Executor，不做 SSH / Docker 执行后端 | §6.4、§13.1 |
| 平台只产出事实，**不做 Gate 判定**、不替 Agent 下结论 | §13.1 |
| **禁止**对 Genet 管理的资源设置指向 runner Job 的 K8s ownerReferences——进程生命周期 ≠ 资源生命周期 | §9.3 |
| 契约字段（附录 A/B/C）变更属破坏性变更，必须升 `schema_version` 并给兼容期（v2 已升 2.0） | §15 |

## 不属于 Argus 的部分

部署归 Genet（Argus 连 Genet 调用都交给内置 service-deploy / service-clean runner），调度与执行归 K8s / Volcano，测试算法归 AISBench / EvalScope——平台只编排、只记录事实。完整 Non-Goals 见 §13。

## 协作

> ✅ `main` 已开分支保护：只能走 PR、线性历史、管理员也不能直推；另有 agent 自动评审（见 `docs/agents/pr-review.md`）。

流程见 [`CONTRIBUTING.md`](CONTRIBUTING.md)：所有改动走 PR，`main` 只接受 **squash / rebase** 合入（merge commit 已关闭），合并后源分支自动删除。

- 文档改动请在 PR 描述里指出对应章节号（如 §6.7）；涉及术语先对 [`GLOSSARY.md`](GLOSSARY.md)；
- 实现 PR 请在描述里对应到 §14.1 的验收项；
- 不要提交运行产物、日志与任何凭据（见 `.gitignore`）；
- 脚本与 Dockerfile 强制 LF（见 `.gitattributes`）。
