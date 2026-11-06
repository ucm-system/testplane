# TestPlane 架构与设计约束

> 云原生、轻量化、AI Agent 友好的 LLM 服务测试平台
>
> **文档类型：** 架构设计约束（Design Constraints）
> **文档效力：** 本文是 TestPlane 的**设计裁决**集合。凡与本文冲突的实现方案，以本文为准；需要变更时，按 §15 追加裁决条目并记录变更，不做静默修改。
> **适用读者：** 平台开发者、Tester 维护者、Jenkins / 流水线对接方、Agent 集成方。

---

## 0. 文档说明

### 0.1 本文回答什么

1. 系统边界在哪里：哪些职责属于 TestPlane，哪些属于外部系统。
2. 模块之间的契约是什么：平台控制端、Tester、Executor 之间靠什么交互。
3. 哪些事情平台**不做**：以 Non-Goals 形式显式排除，避免实现蔓延。

### 0.2 本文不回答什么

- 业务背景、价值论证、用户画像、成功度量 —— 属于需求文档范畴。
- 版本排期、人力投入、里程碑。
- 具体页面交互稿、视觉规范。
- AISBench / EvalScope 等测试工具的内部实现。

> 本文曾以 `PRD.md` 命名，现已停用。需求类文档另行维护，不以设计文档代替需求基线。

### 0.3 约束强度的用词约定

| 用词 | 含义 |
|---|---|
| 必须 / 不得 | 硬约束，实现不可偏离；偏离即为设计缺陷 |
| 应该 / 推荐 | 默认方案；偏离需在实现说明中写明理由 |
| 可以 | 可选能力，不影响其他模块 |

### 0.4 前置裁决

TestPlane 是**对内工具**，以下事项已由团队裁决，本文按结论描述，不再重新论证：

- 服务部署统一由 **Genet** 承担，TestPlane 不做部署平台。
- 测试能力统一以 **Tester** 封装，平台不实现测试算法。
- **不引入 Gate / 门禁判定**：平台只产出事实数据，达标与否由下游或 Agent 判断（详见 §14）。
- 需要 **Web UI**：人也要用，UI 与 API / CLI / MCP 并列为一等消费方（详见 §12）。
- **不承诺数据级复现**：平台保证执行逻辑可追溯，不保证历史 Run 的测试结果可重放（详见 §5.7）。

### 0.5 变更记录

| 版本 | 日期 | 变更摘要 |
|---|---|---|
| 0.1 | — | 初版设计约束 |
| 0.2 | 2026-11-03 | 更名为 ARCHITECTURE；删除 Gate 与 `GATE_FAILED`；新增 Task 状态机、Attempt 模型与失败策略；新增 Pre-Check 阶段与 `SERVICE_UNAVAILABLE` 终态；新增 Web UI 章节；细化 `result.json` / Job Spec / `tester.yaml` 三份契约；明确不承诺数据级复现；修正 Task 粒度规则矛盾 |
| 0.3 | 2026-11-04 | 落地实践后的修订：取消 Blocked 图里的 `PENDING/QUEUED → CANCELED` 箭头（与 §7.6 对齐）；明确 Pre-Check 门控后其余 Task 可并发（不另加串行顺序）；`SERVICE_UNAVAILABLE` 仅限可重试的 Pre-Check 失败；`cleanup.policy` 与产物保留期解耦；Deploy 增加排队状态与 72h 窗口；删除平台日志脱敏义务（团队裁决日志明文） |
| 0.4 | 2026-11-05 | 可追溯性补充：镜像 tag 可变，执行层记录实际拉取的 digest；失败续跑复用优先比较 digest |
| 0.5 | 2026-11-05 | **取消提交幂等**：删除 `Idempotency-Key` 与 `idempotency_keys` 表，`POST /runs` / `POST /tasks` 每次都创建新资源；平台不判断两次提交是否等价（硬件 / 驱动 / HDK 版本等环境因素对平台透明）。内部操作（建 Job、Genet 部署）的可重试安全不变 |
| 0.6 | 2026-11-05 | **MVP 实现回写**（见 `design/IMPLEMENTATION.md §15`）：① Tester 注册只做"镜像内 `tester.yaml` + 启动/周期扫描"，不提供运行时注册 API（D-38）；② 对外错误体为**扁平** `{code,message,detail,retryable,trace_id}`（D-39）；③ 提交体以**嵌套** `run:` 为唯一形态，`03-data-view.md §4.1` 的示例已在 v0.6 统一为嵌套形态（D-40 已更正，见附录 E）；④ 产物 PVC 名统一为 `testplane-storage`（D-41）；⑤ 开发环境种子 `cluster-01` + `k8s-npu`，由进程启动时补齐（D-42）；⑥ MVP 的执行环境是宿主 Kind + `kind load docker-image`，Deploy 阶段由 mock 顶替 Genet（D-43） |
| 0.7 | 2026-11-05 | **Genet 与队列的调研回写**：按 Genet 真实实现（Helm release 为部署单元、无部署单）重写 Deploy 适配（D-44～D-48），并明确 `endpoint.status=ready` **只代表地址可解析**、必须再过 Pre-Check 服务门（D-45）；排队位次不可得（D-46）；Genet 失败终态折叠为 `blocked`（D-47）；控制面用 Open API Key、Web 用 TestPlane 自己的 OIDC client，**不把 Genet 当 IdP**（D-48）；Volcano 队列直接读 CRD、不经 Genet（D-49）；取消/卸载采用"先落意图、后置删除、对账补偿"（D-50） |
| 0.8 | 2026-11-05 | **部署形态与鉴权落定**：纯内网部署，TestPlane 与 Genet 共用**同一个企业统一身份源**，TestPlane 作为独立 OIDC client（不是 Genet 的 client），服务间用统一身份源签发的 JWT（D-51）；Genet 侧的**独占池**（exclusive pool）语义确认：进入独占池的 namespace/服务不受每日回收影响，TestPlane 通过独占自己的 namespace 获得该保证（D-52）；Genet 将开放 `openAPI`，控制面走 `/api/open/*`（D-53） |
| 0.9 | 2026-11-05 | **Deploy 收口与可观测收口**：Run 终态后必须卸载 deploy 的 release，**只走 Genet OpenAPI**（直接 `helm uninstall` 会被其 admission webhook 拒绝），并以「`GET 404` + pods 空」作为删干净的判据（D-54）；`runs.deploy` 落 §4.4 全字段（增量合并，不覆盖已冻结键）；新增 `cleanup.failed` 事件类型；OIDC 会话/CSRF/RBAC 已实现（D-51 落地），`dev` 模式仍保留给本地 |
| 0.10 | 2026-11-05 | **设计端点全量落地 + 两处窄例外**：`07 §5` 的 50 个端点全部实现（retry 续跑 3 条按「新 Run + `retry_of`」裁决，见 `02 §2.6`；deploy 日志按单 Pod 游标；admin 面 11 条 + `GET /secrets`）；新增 **D-55**（`rotate` 对**已登记 Secret** 的 `resourceNames` 只读窄例外，不得通配）；`03 §4.6.1` 明确审计动作枚举与本期范围（`submit/cancel/retry` 暂由 `state_events` 承担）；`06 §4.4` 表格口径与 D-55 对齐 |
| 0.11 | 2026-11-05 | **产物取用与部署形态收口**：新增 **D-56**（P0 唯一真源是 PVC 文件系统；新增 `GET /runs/{id}/bundle` 打包下载；S3 兼容存储列为 P1 可选镜像，不改契约）；`07 §5.1` 补该端点；`POST /tasks/{id}/retry` 明确允许 `failed|skipped`；Helm chart 成为唯一受支持部署形态（含 controller 单活与产物存储硬依赖的渲染期校验） |

---

## 1. 核心定位与设计原则

**一句话：** 给定服务部署参数或已有服务端点，执行标准化的 LLM 性能、精度等测试，产出机器可读、人可读的测试结果。

**核心职责：**

1. 调用外部部署平台 Genet，拉起被测 LLM 服务。
2. 通过 Pre-Check 确认服务可用后，才允许开始正式测试。
3. 根据测试配置创建多个独立 Task。
4. 通过 Executor 将 Tester 运行在指定执行环境中。
5. 收集测试结果和原始产物，生成统一报告。
6. 根据执行策略完成服务及测试资源清理。

**核心设计原则：**

| 原则 | 约束 |
|---|---|
| API First | 所有核心能力通过 REST API 暴露。Web UI、CLI、MCP 都是 API 的消费方，**不得**存在仅某一端可用的功能 |
| Tester First | 测试能力以 Tester 为基本单位，平台不实现具体测试逻辑 |
| Single Entry Point | 每个 Tester 只有一个标准化执行入口 |
| Executor Agnostic | Tester 不感知运行环境，同一个 Tester 可被不同 Executor 执行 |
| Task Atomicity | Task 是最小的调度、状态、重试和报告单元 |
| Contract Driven | 每一个跨边界交互都必须有可校验的 JSON 契约，见附录 A–D |
| AI Friendly | Tester 参数、状态、错误、报告均具备机器可读性 |
| Human Readable | 报告必须人可读；UI 必须覆盖主流程，但对 API 没有独占逻辑 |
| No Hidden State | 状态必须落库，控制端进程重启后可恢复，不允许仅存在于内存 |
| Fail Fast on Pre-Check | 服务不可用时尽早阻断，不浪费正式测试资源 |
| Isolation by Default | 单个 Task 失败默认不影响其他 Task；需要扩散的失败必须显式声明 |

---

## 2. 系统边界与分层

TestPlane 由五个职责层构成：

| 层次 | 核心职责 | 代表模块 |
|---|---|---|
| Presentation | 人机与机器入口 | Web UI、CLI、MCP Server |
| Orchestration | 组织测试生命周期 | Run、Task、Cleanup |
| Capability | 定义测试能力 | Tester、Registry |
| Execution | 提供执行环境 | K8s Executor（平台唯一的执行后端） |
| Integration | 对接外部系统 | Genet、Endpoint、Artifact Storage、Identity |

系统关系：

```text
                    Presentation
              Web UI / CLI / MCP
                       │
                  (REST API)
                       │
   ┌───────────────────┴────────────────────┐
   │            TestPlane 控制端             │
   │                                        │
   │   Orchestration            Capability  │
   │   Run ─ Task ─ PreCheck    Tester      │
   │    │        │              Registry    │
   │    │        └──────► Executor ◄────────┘
   │    │                    │
   │    └──► Genet ──► Endpoint
   └────────────────────────┬───────────────┘
                            │  Job Spec
                            ▼
                     Kubernetes Job
                            │  挂载 NFS：input（只读）/ output（读写）
                            ▼
                   Tester (/tester/run.sh)
                            │  产物直接写入 /workspace/output
                            │  = <storage-root>/<run_id>/tasks/<task_id>/attempts/<n>/
                            ▼
                       Report（JSON / MD）
```

**职责隔离：**

- Genet 负责服务部署，TestPlane 不重复实现 Helm 和服务部署逻辑。
- Tester 负责测试逻辑，TestPlane 不实现 AISBench、EvalScope 等工具的内部功能。
- Executor 负责运行环境，Tester 不负责创建 K8s Job；反过来 Tester 也不知道自己是不是跑在 K8s 里（§5.10）。
- UI 负责展示与操作，不实现业务逻辑，也不允许持有 API 之外的私有状态。
- TestPlane 负责生命周期和结果归一化，**不负责判断指标是否达标**，也不替代下游报告、存储和数据分析系统。

---

## 3. 核心概念

| 概念 | 定义 | 核心职责 |
|---|---|---|
| Tester | 一种标准化测试能力 | 描述测试参数、运行环境和唯一执行入口 |
| Tester Registry | Tester 注册表 | 发现、校验、管理和暴露 Tester |
| Executor | 平台唯一的执行后端 | 用 K8s Job 运行 Tester 容器 |
| Execution Profile | 执行环境配置 | 引用已登记的 K8s 集群 / 命名空间与挂载 |
| Task | 一次独立测试执行 | 最小调度、状态、重试和报告单元 |
| Attempt | Task 的一次执行尝试 | 承载单次退出码、日志、产物和时间信息 |
| Run | 一次完整测试编排 | 管理部署、Task、汇总和清理 |
| Endpoint | 平台侧服务描述 | 由 Genet 返回或用户提供；只用于编排判定与投影成公共参数，**Tester 不可见** |
| Artifact | 测试产物 | 存储标准结果、日志和原始测试文件 |
| Failure Policy | 失败传播策略 | 决定一个 Task 失败后，其余 Task 继续还是终止 |

**层级关系：** `Run → Task → Attempt`。Task 是稳定标识，Attempt 是物理执行。重试产生新的 Attempt，不改变 Task 标识。

**Pre-Check 不是独立概念**，它就是一个 Task——只不过用的 Tester 声明了 `role: pre_check`，并因此要求 `on_failure: abort_run`（§7.3）。这样它就自动继承了 Task 的全部能力：状态机、Attempt、重试、产物目录、取消、独占声明，平台不需要为它写一套特殊逻辑。它出现在 Task 列表的最前面，是**平台按 `precheck` 选项自动注入的**（§10.1）。

---

## 4. Task 模型

### 4.1 Task 定义

一个 Task = 一个 Tester + 一组确定的参数 + 一个执行环境 + 一次独立执行。

Task 必须具备：

- 独立执行、独立状态、独立日志、独立产物。
- 独立取消、独立重试。
- 每次执行产出且仅产出一份 `result.json` 和一个退出码。

不引入 Case 概念，不要求平台理解具体 Tester 内部的数据组织。

### 4.2 粒度裁决

> **裁决：** Task 粒度由**提交者**决定，平台不做强制拆分，也不对"一个 Task 内包含多少组测试参数"做任何假设。

具体规则：

1. **允许合并**：性能测试的输入长度 × 输出长度 × 并发组合，可以作为**一个** `performance-sweep` Task 提交，内部展开方式（串行、并行、分批）完全由 Tester 决定。
2. **允许拆分**：若提交者需要单独取消、单独重试、单独报告或并行执行某些组合，可以显式写成多个 Task。
3. **平台只保证边界事实**：一个 Task = 一次容器/进程执行 + 一个退出码 + 一份 `result.json` + 一份日志。平台不感知 Task 内部的循环与分批。
4. **Tester 的补偿义务**：当 Task 内包含多组参数时，Tester **必须**在 `metrics[]` 中通过 `labels` 区分每一组结果（如 `{"input_len":512,"concurrency":8}`），否则报告与 UI 无法正确分组展示。

拆分的判断依据（指导，非规则）：

| 需求 | 建议 |
|---|---|
| 需要单独失败、单独重跑某一组参数 | 拆成独立 Task |
| 需要多组参数并行跑满执行资源 | 拆成独立 Task |
| 各组参数需要独立的资源规格或执行环境 | 拆成独立 Task |
| 组合之间共享加载成本（模型、数据集预热） | 合并为一个 Task，内部展开 |
| 组合只是同一工具的参数扫描 | 合并为一个 Task，用 `labels` 区分 |

### 4.3 示例

```yaml
# 一个 Task 内展开 24 种组合：合法形态
tests:
  - name: performance-sweep
    tester: aisbench-perf
    version: "1.0.0"
    execution:
      profile: k8s-npu
    args: "--input_lens=[512,1024,4096] --output_lens=[128,512] --concurrency=[1,4,8,16]"
```

```yaml
# 拆分形态：每个组合一个 Task，可独立重跑
tests:
  - name: perf-512-128-c1
    tester: aisbench-perf
    version: "1.0.0"
    execution:
      profile: k8s-npu
    args: "--input_len=512 --output_len=128 --concurrency=1"
```

两种写法都被支持，平台对二者一视同仁：**只创建提交者写明的 Task 数量**。

---

## 5. Tester 契约

这是 TestPlane 最重要的设计部分。Tester 是平台唯一感知"测试怎么做"的边界。

### 5.1 Tester 的定位

Tester 是独立、可执行、可复用的测试单元。例如：

- `endpoint-probe`（系统内置，Pre-Check 角色）
- `aisbench-acc`、`aisbench-perf`、`aisbench-prefix`
- `evalscope-acc`

Tester 可以封装现有测试工具，也可以封装自定义测试脚本。

Tester 自己决定：

- 提供哪些测试参数，以及参数如何解释。
- 使用什么测试工具、如何拼装具体执行命令。
- 如何处理测试工具的原始输出，如何归一化为标准结果。
- 内部按什么顺序、什么并发度执行多组参数。

TestPlane 不关心 Tester 内部使用了什么工具、执行了多少步骤。

### 5.2 Single Entry Point：唯一执行入口

**每个 Tester 必须提供且只提供一个标准化执行入口：**

```bash
/tester/run.sh
```

Tester 内部可以执行任意符合自身需求的命令，例如：

```bash
aisbench --model=Qwen3-32B --dataset=/data/test.json --max-out-len=128
```

或：

```bash
evalscope eval --datasets gsm8k --limit 100
```

平台只负责调用 `/tester/run.sh`。

**平台不得：**

- 解析 Tester 内部的命令行参数。
- 根据参数名称拼装 AISBench 或 EvalScope 命令。
- 了解不同测试工具的 CLI 规则。
- 为不同 Tester 编写特殊的命令执行逻辑。
- 根据测试工具的内部产物结构编写专属解析逻辑。

Tester 必须自行完成从平台输入到具体测试命令的映射。**平台只做词法切分与透传：不解析参数语义，不拼装工具命令。**

### 5.3 调用契约：argv + env + 输出目录

契约面只有三样东西，没有第四样：

```text
/<entry>                                    # 容器 command，默认 /tester/run.sh
   --p1=1 --p2=2 ...                        # 调用方给的参数（argv，未经 shell）
   --url=... --model_name=... --model_path=...   # 平台追加的公共参数（§5.4.1）
```

| 通道 | 内容 | 说明 |
|---|---|---|
| **argv** | 全部业务参数 | 平台按**词法**切分调用方输入（支持引号，如 `--dataset="/data/a b.json"`），得到 argv 数组，**不解释语义** |
| **环境变量** | `TESTPLANE_RUN_ID` / `TESTPLANE_TASK_ID` / `TESTPLANE_ATTEMPT_ID` | 运行元信息走变量不走文件；独立运行时缺省 `local`（§5.10） |
| **输出目录** | `/workspace/output/`，可写 | 唯一目录契约：`result.json`（必须）、`raw/`、`logs/` |

约束：

- 参数**不得**经过 `sh -c` 或任何 shell 解释：`;`、`|`、`$()`、通配符一律作为字面量传给 Tester。平台禁止把用户输入拼成一条 shell 命令。
- **没有 `/workspace/input`**。业务输入全部来自 argv；平台不生成参数文件、不生成元数据文件。
- `result.json` **必须**产出且必须是合法 JSON；缺失视为 Task 失败（`RESULT_MISSING`）。这条契约**全局唯一**，Tester 目录不需要再声明产物清单——`raw/` 与 `logs/` 是自由区，平台只做搬运与索引。
- Tester 不得依赖容器内其他路径持久化产物；`/workspace` 之外一律视为只读。
- 数据集与模型等外部资源**不由平台托管**：路径是不透明字符串，不解析、不校验存在性、不做映射（§10.4）。
- **Tester 必须能独立运行**：只给 argv + 环境变量就能跑出与平台内一致的产物（§5.10）。
- 凭据只走环境变量（§12.4），不参数化、不落盘。
- 平台**不得**向 Tester 传第四种通道（参数文件、元数据文件、平台 API 地址等）。

### 5.4 参数清单（文档语义）

> **裁决：** 平台**不渲染、不校验** Tester 参数。`tester.yaml.parameters` 只作为**说明书**存在，回答"这个 Tester 能填什么、什么含义"，主要给 Agent 与维护者看。

推论：

- Web UI 只有一个输入框（§12），**不**根据参数清单生成表单。
- 平台不校验类型、必填、枚举、范围。参数错了由 Tester 自己在运行时发现：非零退出 + `result.json.error`。
- Agent 读清单后自己拼 `--k=v`；参数值正确性由 Tester 负责。
- 因此清单**必须准确**：写错的代价从"提交被拒"变成"跑完才发现"。

清单字段**全部是文档语义**，不参与任何运行时校验：

| 字段 | 用途 |
|---|---|
| `type` | 提示取值形态：`string` / `number` / `integer` / `boolean` / `array` |
| `description` | 必填，中文；语义 + 单位 + 取值范围 |
| `default` | 不传时的实际行为，必须与入口脚本一致 |
| `enum` | 可选值，每个值写明含义 |
| `example` | 一个可直接抄的样例值 |
| `required` | 值必须存在：没有值就算配置错误（供 Agent 判断"这个必须给"）。平台不校验，Tester 自己快速失败 |
| `required_for` | 对谁必需，例如"所有调用生成接口的 Tester" |

顶层另有 `placeholder`：**一段可直接抄的示例命令行**（不含程序名），UI 拿它作输入框占位提示，Agent 拿它作最省事的模板。它同样是文档语义，平台不解析。

**清单必须完整**：Tester 实际接受的每一个 `--key`，包括公共参数，都要列出来。漏列等于让 Agent 看不见这个参数——它不会去猜。未使用的公共参数可以不列，但**必须能被容忍**（§5.4.1 第 6 条）。

**不用字段表达"谁给值"**。同一个参数在不同场景下来源不同，例如 `url`：Run 里有 `deploy` 时平台注入；跑已有服务（不拉服务）时由调用方填写；独立运行时由调用方填写。这是**条件性**事实，静态字段一定写不准，所以写在 `description` 里，不要引入 `provided_by` 之类的枚举。

**`description` 书写规范**（Agent 全靠它决策，写不好等于没有说明书）：

1. 说清"这是什么"，不能只重复参数名。
2. 带单位、取值范围、与其它参数的耦合关系。
3. `enum` 每个取值都要有含义说明。
4. 禁止"见文档"、"同上"这类无法机器消费的表述。

示例：

```yaml
parameters:
  concurrency:
    type: array
    description: "并发梯度，逐个取值串行执行；每档并发对应一组独立指标，用 metrics[].labels 区分"
    example: "[1, 4, 8]"
    default: "[1]"
  dataset:
    type: string
    description: "数据集在容器内的绝对路径；由镜像内置或 Profile 挂载提供，Tester 自己校验存在性"
    example: "/data/gsm8k.jsonl"
    required_for: "精度类 Tester"
```

#### 5.4.1 公共参数（保留字）

三个参数在几乎所有 Tester 上语义相同，**必须**使用固定名称与固定语义：

| 参数 | argv 形式 | 语义 |
|---|---|---|
| `url` | `--url=scheme://host:port/v1` | 被测服务的 **OpenAI 兼容 base URL**。Tester 从它推导 `/chat/completions`、`/models`；不带 `/v1` 由 Tester 自动补，末尾斜杠忽略 |
| `model_name` | `--model_name=<name>` | 服务端模型名，即 OpenAI 兼容接口 `model` 字段的取值 |
| `model_path` | `--model_path=<path>` | 容器内模型权重路径；可达性由挂载决定，平台不改写（§10.4） |

规则：

1. **名称固定**：禁止 `base_url`、`service_url`、`model`、`model_dir` 等变体。Agent 靠这三个固定名字做通用拼装。
2. **`url` 用业界统一形态**：OpenAI 兼容 base URL（`scheme://host:port/v1`）。**不单独暴露 `path` 参数**：具体端点（`/chat/completions`、`/models`）由 Tester 从 base 推导，缺 `/v1` 时自己补齐。跨 Tester、跨工具（AISBench / EvalScope / 自研脚本）因此指向同一语义。
3. **来源只有一处，取值点不随场景变**：

   | 场景 | 三个参数从哪来 | 平台行为 |
   |---|---|---|
   | Run 里有 `deploy` | Genet 返回的 Endpoint | 追加到 argv 末尾 |
   | 跑已有服务（不部署） | Manifest 的 `service` 块，提交时填 | 追加到 argv 末尾 |
   | 脱离平台独立运行 | 调用方自己传 | 平台不存在 |

   Manifest **必须**提供 `deploy` 或 `service` 之一，两者互斥；都填或都不填一律拒绝提交（`VALIDATION_ERROR`）。这样平台在平台侧**永远有值可注入**，不存在"这次平台没值、请你自己写进 `args`"的分支——那种分支会让同一个参数在不同 Run 里归属不同，人和 Agent 都得多想一步。

   投影仍由平台完成：`service` 填的就是这三个参数本身，平台内部把它当作 Endpoint 的来源之一（§9.1）。Tester 只看到 `--url` / `--model_name` / `--model_path`，看不到 Endpoint。

   所以参数清单里**不写**"来源"字段（`provided_by` 之类），只在 `description` 说明这三条路径。
4. **注入与冲突判定**：

   - 平台把有值的公共参数**追加到 argv 末尾**（`--key=value`）。若 `args` 里已出现同名参数，**拒绝提交**（`VALIDATION_ERROR`，按词法切分后的 token **精确匹配**（`--url=` 形式或独立 token `--url`；`--url_extra=` 不算同名），不做语义解析），避免两份值打架。
   - 因此 `tests[].args` **不应该**出现这三个参数：它们由平台统一注入，位置固定在末尾。
   - `service` 只填了部分参数（比如有 `url` 没 `model_path`）时：Tester 声明为 `required: true` 而没值的，**提交阶段拒绝**；`required: false` 的就不注入，由 Tester 用默认行为兜底。

5. **Tester 不声明对 Endpoint 的依赖**：它只认 `--url`，不关心这个值来自 Genet 部署、用户直填，还是压根拿不到。把"我需要 Endpoint"写进 `tester.yaml` 等于让 Tester 感知平台概念，禁止。
6. **值对平台不透明**：不解析、不校验存在性、不做路径映射。
7. **必须容忍**：Tester 即使不使用某个公共参数，也必须接受并忽略它（平台可能无条件下发）。把 `--model_path` 当未知参数报错是契约违规。
8. 凭据不进这三个参数（§12.4）。

### 5.5 Tester 目录结构

采用项目内子目录自动发现机制。一个 Tester 就是一个自包含目录：

```text
testers/
├── endpoint-probe/
│   ├── tester.yaml          # 唯一权威：元数据 + 参数清单（文档语义） + 资源/挂载需求
│   ├── run.sh               # 唯一执行入口（平台与独立运行都调它）
│   ├── build/
│   │   ├── Dockerfile       # 构建定义；构建方式写在注释或 README 里
│   │   └── scripts/         # 构建期脚本（可选，形态不限）
│   ├── run-local.sh         # 本地便捷脚本：docker run（不 build）
│   └── README.md
├── aisbench-acc/
├── aisbench-perf/
├── aisbench-prefix/
└── evalscope-acc/
```

| 路径 | 要求 | 职责 |
|---|---|---|
| `tester.yaml` | 必须 | 元数据、角色、镜像引用、资源、参数清单、并发声明、受限 K8s 需求 |
| `run.sh` | 必须 | 唯一执行入口，必须具备可执行权限 |
| `build/Dockerfile` | 推荐 | 定义运行环境。构建上下文是 Tester 根目录，构建命令写成 `docker build -f build/Dockerfile .`——**写在 Dockerfile 注释或 README 里即可**，平台不读 |
| `build/scripts/` | 可选 | 构建期脚本（装依赖、改配置）；**形态与命名不限**，可以没有。运行期逻辑一律进 `run.sh` 及其被它调用的文件 |
| `run-local.sh` | 可选 | 本地/独立运行的便捷封装：只 `docker run`，**不 build**（镜像来自 CI）。不是契约入口，平台不读它 |
| `README.md` | 推荐 | 使用说明、参数示例、独立运行方法 |
| `tests/` | 可选 | 维护者自测（mock 服务 + 断言），不参与注册与调度 |

**明确不在 Tester 目录里的东西：**

| 不要放 | 原因 |
|---|---|
| `deployment.yaml` / `job.yaml` / 任何完整 K8s 清单 | 部署由**平台渲染**（§6.8、附录 B）。Tester 若自带清单，就等于把集群名、命名空间、挂载点写进可复用单元，且平台无法再保证 input/output 挂载契约（subPath、只读、权限）。特殊需求改用 `tester.yaml.k8s` 的**受限声明字段**（附录 C） |
| `results/` / `output/` 等结果目录 | 容器内固定写 `/workspace/output`（§5.3），由平台挂到最终存储路径。仓库里不放测试产物；本地调试用 `OUTPUT_DIR` 指到被 gitignore 的目录 |
| 凭据、`.env`、私钥 | 一律走环境变量注入（§12.4） |

**注册扫描规则：** 只认 Tester 根目录下的 `tester.yaml` 与 `run.sh`；`build/`、`tests/`、`.local/` 等子目录不参与扫描与校验。

**构建不做规范**：平台不规定 Tester 用什么构建脚本、脚本叫什么、在哪个目录。唯一要求是构建方式能被读懂——写清构建上下文与构建命令（Dockerfile 注释或 README），镜像由 CI 产出并发布（§6.2）。

`tester.yaml` 的字段定义见附录 C。

### 5.6 注册机制

Tester 采用平台维护者管理的**白名单注册**机制。注册流程：

1. 扫描 `testers/*/` 的**根目录**（只认 `tester.yaml` + `run.sh`；`build/`、`tests/` 等子目录不递归，见 §5.5）。
2. 读取并校验 `tester.yaml`（字段完整性、Schema 合法性）。
3. 检查 `run.sh` 存在且具备执行权限。
4. 校验镜像引用非空（平台**不限制形态**：tag、digest 都行，是否固定版本由团队约定，见 §5.6 下方说明）。
5. 检查名称与版本冲突。
6. 校验失败扩散声明：`on_failure` 必须是 `continue` / `abort_run` 之一；`role: pre_check` 时必须是 `abort_run`，否则拒绝注册（§7.3）。
7. 校验通过后写入 Tester Registry，对外暴露参数清单（仅供 Agent / 人类阅读，平台不渲染不校验）。

用户和 Agent **只能使用已注册的 Tester，不能自行上传执行代码**。

> **镜像引用不作限制**：平台把 `tester.yaml.image` 原样交给 K8s，不解析 tag / digest，也不强制钉版本。版本可追溯由团队约定与 CI 保证；平台再加一道校验，只会让改一个 tag 就要走平台评审。§5.7 的"执行逻辑可追溯"因此改为记录镜像引用本身。

#### 5.6.1 版本保留与退役（票 #60 / D-60）

同一 `name` 可以有**多个版本并存**，这是正常的版本演进，不是重复入库：

| 情形 | `testers` 行 | 目录里 | `GET /testers` 默认 | `retired` |
|---|---|---|---|---|
| 新版本发布（旧版本仍在目录） | 两行都在 | `0.1.0`、`0.2.0` | 只回 `0.2.0`（`latest=true`） | 都是 `false` |
| 新版本发布后旧版本目录被删 | 旧行保留，标记 `orphaned` | 只有 `0.2.0` | 只回 `0.2.0` | 旧行 `true` |
| 整个 Tester 下架 | 所有行 `orphaned` | 无 | 默认不出现（`include_retired=true` 才可见） | 都是 `true` |

- **不删除旧行**：历史 Task/Attempt 用 `name@version` 引用（`03 §4.1`），删行会让历史报告永久失去 Tester 元数据（`§5.7` 可追溯性）。
- `latest` 的值是"该 name 的最高版本"（含 `retired` 行），比较规则见 `domain.VersionLess`（注册表扫描排序、API、`tpctl` 共用同一份）。
- `orphaned` 与 `retired` 同义（前者是 `testers` 列名，后者是对外字段名），两个字段都保留以兼容既有前端/CLI。
- **`retired` 的口径是"该 **版本** 已不在目录里"**（不是"整个 Tester 下架"）：
  - 只有旧版本从目录消失（`testers/<name>/tester.yaml` 里的 `version` 改了）→ 旧版本 `retired=true`，新版本 `retired=false`；
  - **整个 Tester 目录消失**是同一条规则的极端情形：该 name 的**所有版本**都 `retired=true`（默认列表里这个 name 整个不出现）。
  - 判定发生在启动期/重扫期的目录比对（`registry.Scan`）：目录里没有的 `name@version` → 标记；**标记会落库**
    （`registry.Store.MarkTesterOrphaned`），因此**进程重启后 `retired` 不会丢**——这是票 #60 真机回归修的持久化缺口。

### 5.7 版本与可追溯性

> **裁决：** 平台保证**执行逻辑可追溯**，不保证**结果可复现**。

具体含义：

- 已发布的 `name + version` 组合**不得**被静默覆盖；重新发布必须提升版本号（错误码 `TESTER_VERSION_PINNED`）。
- Task 记录并冻结：Tester 名称、版本、镜像引用、参数快照（即 `args` 原文）。
- 执行层把 K8s 实际拉取的镜像 `imageID` 解析为 **digest** 写入 Attempt（可得时）：引用是"要什么"，digest 是"实际跑了什么"。镜像 tag 可变，只有 digest 能回答"同一个 tag 当时背后是哪份代码"。平台仍不解析、不校验镜像引用，只记录 K8s 给出的事实。
- 历史 Run 可以确认"当时用的是哪个版本的哪个 Tester、传了什么参数"，但**不保证**重跑得到相同数字。
- 数据集与模型内容的变化由 Tester 与外部资源负责；平台不做内容指纹（hash / manifest），因此也不承诺跨时间点的结果可比性。可比性判断交给下游与 Agent。

这条裁决同时简化了**失败续跑**的复用条件：复用已成功 Task 结果时，只需校验 Tester 版本、镜像引用与**实际 digest（可得时优先比较 digest）**、参数快照、Endpoint 标识四者未变；不校验数据内容。tag 可变，若 digest 缺失则退回引用比较并在报告中标注。

**提交不做幂等**（D-37）：平台不判断两次提交是否"一样"，也不合并。硬件型号、驱动、HDK 版本等环境条件对平台透明；即使 Manifest 完全相同，也可能是不同的测试条件。`POST /runs` 每次都创建新 Run，由提交者自己承担"重复提交"的后果（可取消）。只有平台**内部操作**（同一 `run_id` 的 Genet 部署、同一 `job_id` 的 Job 创建）需要"重复执行不产生副作用"的安全语义，那是实现细节，不是对外接口承诺。

### 5.8 退出语义

| 执行结果 | Exit Code | Task 状态 | 含义 |
|---|---|---|---|
| 测试正常完成 | 0 | `SUCCEEDED` | 执行成功。指标是否达标由外部判断，平台不做门禁 |
| 参数错误或资源缺失 | 非 0 | `FAILED` | 执行失败，错误码由平台按退出码与 stderr 归类 |
| 测试工具异常 | 非 0 | `FAILED` | 执行失败 |
| 执行超时 | 平台终止 | `FAILED` | 错误码 `TASK_TIMEOUT` |
| 被取消 | 平台终止 | `CANCELED` | 用户或策略取消 |

**测试执行成功与测试结果达标是两件事。** 例如精度测试完整执行但准确率偏低，Tester 应正常返回结果（exit 0），由下游消费指标；**不得**把"指标不理想"映射为执行失败——平台已不设门禁，若 Tester 又把不达标表达为失败，会让"执行可靠性"指标失真。

### 5.9 结果归一化义务

Tester 负责把工具输出映射到标准结果 Schema（附录 A），具体要求：

- `metrics[].name` 使用 `snake_case`，同一 Task 内同名指标的区分靠 `labels`，不得用名字编码维度（禁止 `ttft_concurrency_8` 这类命名）。
- 每个指标必须带 `unit` 与 `statistic`。
- `statistic` 只允许：`mean` / `p50` / `p90` / `p95` / `p99` / `min` / `max` / `sum` / `count`。
- 多组参数的结果必须能在 `labels` 上区分，否则视为契约不合规。
- 失败时也必须写 `result.json`（`status: failed` 且带 `error`），保证每个 Attempt 都有可读结果。

### 5.10 独立运行（脱离平台）

> **裁决：** Tester 必须能脱离 TestPlane 单独运行。平台只是它的一种运行方式。

任何 Tester 在只有参数和环境变量的情况下，都必须跑出与平台内一致的产物。

| 要求 | 说明 |
|---|---|
| 输入只靠 argv | 业务输入全部走命令行参数（含公共参数 `--url` / `--model_name` / `--model_path`），不读任何平台专有文件 |
| 元信息可缺省 | 平台不注入 `TESTPLANE_*` 变量时按 `local` 处理；与平台相关的只有 `run_id` / `task_id` / `attempt_id` |
| 目录可覆盖 | 输出目录必须能被 `OUTPUT_DIR` 覆盖（默认 `/workspace/output`） |
| 凭据走环境变量 | 认证信息一律从环境变量读，不依赖平台注入机制 |
| 产物自足 | 独立运行时同样产出符合附录 A 的 `result.json`，日志写 `logs/`，不要求平台收集 |
| 不依赖 K8s | 不得调用 K8s API、不得要求 ServiceAccount、不得假设挂载来源 |

运行方式（都由使用者负责，不属于平台能力，§6.4）：

```bash
# 容器内直接跑：与平台里是同一条命令形态
docker run --rm \
  -e ENDPOINT_API_KEY=... \
  -e TESTPLANE_RUN_ID=local \
  -v /tmp/out:/workspace/output \
  registry.dev.huawei.com/testplane/endpoint-probe:1.0.0 \
  --wait_timeout_seconds=600 --url=10.0.0.10:8000 --model_name=Qwen3-32B

# 或 SSH 登录一台主机，在已有镜像/环境里跑
ssh host 'bash /tester/run.sh --wait_timeout_seconds=600 --url=... --model_name=...'
```

没有 build 步骤：镜像由 CI 产出并发布，平台、SSH、本地一律只 pull + run（§6.4）。

**验收方法：** 在没装 TestPlane 的机器上，`docker run <image> --p1=1 --p2=2` 能跑出 `result.json` 即为合规。

### 5.11 Pre-Check 类 Tester 的附加约束

`role: pre_check` 的 Tester 除了通用契约，还要满足四条。它们都只跟"它是前置门"有关，不是另一种概念：

1. **探测必须发生在实际测试能访问服务的网络里**。在控制端进程内探测没有意义：K8s ClusterIP 之类地址从控制端不可达，探测环境必须与 Tester 执行环境的网络可达性一致。
2. **等待窗口由 Tester 自己实现**（`wait_timeout_seconds` / `retry_interval_seconds`）。平台不提供 `wait_for_ready` 这类包装字段——等多久、多久探一次是 Tester 的内部策略。
3. **必须区分"未就绪"与"不可重试"**：5xx、404（PD 注册中）、429、连接被拒、超时可以继续等；4xx 请求错误、认证失败、DNS / TLS 错误、2xx 里包着 `error` 对象应当立即失败并置 `retryable: false`。它独占着该 Endpoint，空等等于把后面的正式测试全挡住（§1 Fail Fast on Pre-Check）。
4. **两种"重试"不可混用**：

| 概念 | 归属 | 语义 |
|---|---|---|
| 等待服务就绪 | Tester 参数 `wait_timeout_seconds` / `retry_interval_seconds` | 一次执行内反复探测，属正常等待；平台不加壳、不产生新 Attempt |
| Task 执行重试 | `tests[].retry` | 判定失败后创建新 Attempt 重跑整个 Task（§7.3） |

---

## 6. Executor 与 Job Spec

### 6.1 Executor 的定位

Executor 是平台提供的通用任务执行适配层。它**只负责**：

- 准备执行环境。
- 获取 Tester 镜像。
- 挂载输入和输出目录。
- 注入环境变量与凭据。
- 启动 Tester 并跟踪运行状态。
- 收集日志和产物。
- 取消和清理执行资源。

它**不负责**：

- 理解 Tester 参数语义。
- 拼接具体测试工具命令。
- 解析原始测试结果。
- 判断性能或精度是否达标（平台不设门禁，见 §14）。

### 6.2 Executor 与 Tester 正交

平台只有**一种** Executor：**K8s Job**。所谓"正交"，指的是 Tester 不感知执行环境——它不知道自己跑在 K8s Job 里，还是被人手工在主机上执行。

| 运行方式 | 由谁负责 | 是否平台能力 |
|---|---|---|
| K8s Job（`kubernetes`） | 平台 Executor | 是 |
| 容器 / 主机上直接执行 `run.sh`（含 SSH 登录 Docker 主机后执行） | 使用者本人 | **否**（§5.10） |

推论：

- **不得**为适配某个执行环境去修改 Tester 的测试逻辑；若必须修改，说明契约设计有问题。
- 平台**不提供** SSH / Docker 执行后端，也不登记远端主机、不代管远端凭据。要在这类环境跑，就走独立运行方式（§5.10）。
- `tester.yaml` 不声明 `executors` 字段——只有一种平台 Executor，这个字段没有信息量。
- **任何运行环境都不 build**：镜像引用由 CI 产出并发布，平台、SSH 主机、本地一律只 pull + run（§6.4）。

### 6.3 Kubernetes Executor

在已有 K8s 集群创建 Job 执行 Tester 容器。

负责：选择已有 Cluster 与 Namespace、创建 Job、配置资源请求与限制、配置 GPU/NPU 资源、挂载数据集与输入输出目录、跟踪 Job/Pod 状态、收集日志与产物、结束后清理 Job。

约束：

- 使用 **Job** 执行一次性测试任务。
- 不把 Deployment 作为常规 Tester 的执行资源。
- 不负责集群创建、集群扩缩容。
- 不负责底层 GPU/NPU 调度策略（交给 K8s / Volcano）。

### 6.4 平台外运行（不是 Executor）

在 Docker 主机上跑 Tester——包括 SSH 登录后执行——是**使用者的运行方式，不是平台能力**：

- **没有 build 行为**：所有人都用 CI 产出的镜像；SSH 主机与本地只做 `docker pull` + `docker run`（§6.2）。在目标机器上 build 会让"跑的是哪个版本"不可追溯。
- 平台不登记主机、不建 SSH 连接、不管理远端密钥、不把 Task 调度到 Docker 主机。
- 使用者自行拉镜像、挂输入输出目录、执行 `/tester/run.sh`；产出的 `result.json` 与平台里跑出来的是同一份（附录 A）。
- 现网最常见的场景是人在一台机器上跑一个 Tester，而不是平台跑测试——这也是 §5.10 的来源。

约束：这条路径不产生平台的 Run / Task 记录。需要进入平台生命周期（状态机、重试、报告归集），就走 K8s Executor。

### 6.5 Execution Profile

Execution Profile 描述一个可供 Task 使用的 K8s 执行环境。Profile 通过 `cluster` 字段引用**已登记的集群对象**（`clusters`：`name` / `api_server` / `kubeconfig_key` / `last_check_at` / `last_check_result`）；kubeconfig 由 Secret 挂载到 `CLUSTERS_DIR`，**不入库、不进日志**（G-10、W-10）。

```text
Execution Profiles
├── k8s-npu
│   ├── executor: kubernetes
│   ├── cluster: cluster-01
│   ├── namespace: testplane
│   └── mounts:
│       ├── dataset-nfs: /data
│       └── model-nfs: /models
└── k8s-gpu
    ├── executor: kubernetes
    ├── cluster: cluster-02
    └── mounts:
        └── dataset-nfs: /data
```

约束：

- Tester 不绑定具体集群、不绑定命名空间。
- Execution Profile 不绑定具体 Tester。
- Task 选择一个满足 Tester 运行需求的 Execution Profile。
- Profile 负责提供环境能力与挂载供给；Tester 负责声明运行需求。

### 6.6 超时与取消语义

| 项目 | 约束 |
|---|---|
| Task 超时 | 由**平台配置 + Manifest 覆盖**决定（`tests[].timeout_seconds`），Tester 不声明时限——跑多久是编排策略，不是 Tester 属性。上限见 §12.7 |
| 超时处理 | Executor 强制终止进程 / 删除 Job；Task 状态 `FAILED`，错误码 `TASK_TIMEOUT` |
| 取消 | 必须能终止底层资源（删除 K8s Job 并清理对应 Pod） |
| 取消竞态 | 取消与自然结束同时发生，以**先落库的状态**为准，后到者不覆盖 |
| 清理失败 | 清理失败不改变 Task 结果，但必须记录 `CLEANUP_FAILED` 事件并告警 |

### 6.9 调度可见性（Volcano）

> **裁决：** 调度归调度器，**可见性归平台**。平台不参与调度决策，但必须能回答"为什么还在排队"。

平台自己的并发控制（§6.7）只决定"谁先拿到 Endpoint 槽位"；真正决定 Pod 什么时候起来的，是 K8s 与 Volcano。这两层等待必须分开呈现，否则用户看到 `QUEUED` 只会去查错地方。

平台必须暴露（**只读**，不写回调度器）：

| 信息 | 来源 | 用在哪 |
|---|---|---|
| 队列名、队列状态（Open / Closed）、`deserved` / `allocated` | Volcano `Queue` CRD | 看板与 Task 详情的等待原因 |
| 排在该队列前的 Job 数、gang 是否满足 | Volcano `Job` / PodGroup | 同上 |
| 节点资源总量与已分配（NPU / GPU / CPU / 内存） | K8s Node 与 Pod 统计 | 看板 |
| 具体 Task 当前卡在谁身上 | Pod 的 `Pending` 事件（`insufficient nvidia.com/gpu`、`gated by gang` 等） | Task 详情的 `waiting_reason` |

约束：

- **只读**：平台不改队列权重、不抢占、不缩短别人（没这个权限，也不该有这个权力）。
- **`QUEUED` 必须带原因**：`queue_full` / `insufficient_resources` / `gang_pending` / `slot_waiting`（平台自身的 Endpoint 槽位）。只给状态不给原因，等于让人去猜。
- **不引入第二套监控**：数据从 K8s / Volcano API 拉，不部署 Prometheus 之类新组件；指标转发给已有监控系统是另一件事（§12.6）。

### 6.7 并发控制

平台必须实现 Endpoint 级互斥：**同一服务上同一时刻只允许存在一个"独占型" Task，其他 Task 一律排队**。机制（唯一实现方式，不允许各模块自行加锁）：

- 每个 Task 在执行前按 `endpoint.key` 申请执行槽位。没有 Genet 部署时，`endpoint.key` 取公共参数 `url` 的规范化值（协议 + host + port），保证同一服务的不同写法仍互斥。
- `tester.yaml` 的 `concurrency.endpoint_exclusive: true` 表示该 Tester 的 Task **独占**该 Endpoint：执行期间，同 Endpoint 上的**任何**其他 Task（无论什么类型）都不得运行，必须排队。
- `endpoint_exclusive: false` 表示允许与其他非独占 Task 并行；但它**永远不能与任何独占型 Task 并行**——独占是单向压制。
- 队列与槽位状态必须落库，控制端重启后恢复；不允许只在内存中维护。
- 全局并发上限由平台配置项控制，超出时 Task 停在 `QUEUED`。

**谁必须声明独占：**

| 类型 | 独占 | 理由 |
|---|---|---|
| 性能类（`aisbench-perf` 等） | 必须 `true` | 并发会污染吞吐 / 延迟测量 |
| **Pre-Check（探活）** | **必须 `true`** | ① 探活本身就是一次真实生成请求，会干扰同时进行的性能测量；② 它是正式测试的前置门，语义上不能与其他 Task 并行（§5.11） |
| 精度类 | 默认 `false` | 不测时延，可并行摊满资源 |

**实际形态**：虽然规则上允许并行，但一个 Run 里常见的是"部署 → 探活 → 精度 A → 精度 B → 性能"，**大部分时间只有一个 Task 在跑**——Pre-Check 和性能类都是独占的，精度类又要排队等同一个 Endpoint。并行是例外而非常态，所以运行详情默认按顺序呈现执行链路，不要假设成一批同时开跑的任务。

平台不负责决定底层 GPU/NPU 的分配策略，交给 Executor 与底层调度系统。

---

### 6.8 输入输出挂载约定

Executor 渲染任务时，必须把两个目录挂到固定容器路径，且**输出目录直接指向最终存储路径**——不允许"先写容器本地、跑完再拷贝"作为默认实现：

| 容器路径 | 挂载源 | 权限 | 说明 |
|---|---|---|---|
| `/workspace/output` | `<storage-root>/<run_id>/tasks/<task_id>/attempts/<attempt>/` | 读写 | **就是最终落盘位置**，平台不搬运任何字节 |
| Profile 挂载点 | Execution Profile 提供的共享存储（数据集、模型） | 只读 | 由 `tester.yaml.mounts` 声明（§10.4） |

> 没有 `/workspace/input`：业务输入全走 argv（§5.3）。容器里除 `/workspace/output` 与 Profile 挂载点外，其余路径一律只读或不存在。

约束：

1. **路径由平台生成**：`run_id` / `task_id` / `attempt` 全部由平台生成并做路径安全校验（禁止 `/`、`..`、空白、控制字符），不得把用户输入直接拼进路径。
2. **必须用 subPath 挂载**：K8s 用 PVC `subPath`。**不得**把 `<storage-root>` 整体挂进容器——否则 Tester 可以读写其他 Run 的产物。
3. **目录由平台预创建**（属主 = 运行 UID，权限 `0770`）；同时 Tester 仍应容忍目录缺失并自行 `mkdir -p`，两边都要成立。
4. **Attempt 之间互不可见**：重试写入新的 `attempts/<n>/`，历史 Attempt 的产物不得被覆盖。
5. **失败保留现场**：Task 失败不得删除该目录；归档或清理由 Run 的 `cleanup.policy` 决定。
6. **"收集"退化为校验**：产物已直接落在最终位置，收集环节只剩三件事：校验 `result.json`、补齐 Task / Run 报告、生成索引。平台不得重新打包或改写 Tester 写出的内容。
7. **存储前提**：K8s 所有节点都能访问同一份 NFS（现网已具备）。这属于环境准入条件，因此不提供"先写 emptyDir、跑完再打包上传"的退化模式：那会在 Pod 被抢占时丢产物，也让"产物直落最终路径"这个简化失效。
8. **渲染权归平台**：Tester **不得**提供任何完整 K8s 清单（`deployment.yaml` / `job.yaml`）。特殊需求（节点亲和、容忍、非 root UID 等）只能在 `tester.yaml.k8s` 用白名单字段声明（附录 C），由平台合并渲染；平台字段（镜像、命令、input/output 挂载、超时）优先且不可被覆盖。

---

## 7. 状态机与失败策略

### 7.1 Task 状态机

```text
              PENDING
                 │
        ┌────────┴────────┐
        ▼                 ▼
     SKIPPED           QUEUED
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
           SKIPPED   PREPARING     CANCELED
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
           FAILED      RUNNING      CANCELED
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
          SUCCEEDED     FAILED      CANCELED
                          │
                          └──► QUEUED（重试策略允许时，新建 Attempt）
```

| 状态 | 语义 | 终态 |
|---|---|---|
| `PENDING` | 已创建并通过参数校验，等待调度（依赖或编排未推进） | 否 |
| `QUEUED` | 依赖已满足，等待执行槽位 | 否 |
| `PREPARING` | 已获得槽位，正在准备环境（拉镜像、挂载、写入输入目录） | 否 |
| `RUNNING` | Tester 容器 / 进程已启动 | 否 |
| `SUCCEEDED` | 退出码 0，且 `result.json` 合法存在 | 是 |
| `FAILED` | 非零退出、超时、环境错误、结果缺失 | 是（无剩余重试时） |
| `CANCELED` | 用户或策略主动取消 | 是 |
| `SKIPPED` | 未执行：前置失败、上游 abort、或被显式跳过。**必须带 `reason`** | 是 |

**状态规则：**

- 状态转移只能沿上图进行，任何模块不得直写状态，必须通过状态机服务（`No Hidden State`）。
- `FAILED` 在重试策略允许时不是终态：立即转回 `QUEUED` 并创建新的 Attempt。
- 终态判定 = 最后一次 Attempt 已结束且无剩余重试。
- `SKIPPED` 与 `FAILED` 严格区分：`SKIPPED` 表示"没跑"，`FAILED` 表示"跑了但失败"。报告与 UI 不得把二者混为一谈。
- **大小写约定**：本文状态名用大写表示概念；库 / API / 报告中的 token 统一小写（与 §8.4 示例一致），前端展示为大写徽标。

### 7.2 Attempt 模型

Task 是稳定标识，Attempt 是物理执行。一个 Task 至少有一个 Attempt。

```text
Task (task_id)
 └── attempt#1  exit_code / 超时 / 错误码 / 日志 / 产物 / result.json
 └── attempt#2  （重试产生）
 └── attempt#3  ...
```

约束：

- `attempt_id` 全局唯一，`index` 从 1 递增。
- 每次 Attempt 独立产出日志与 `result.json`，产物路径按 `attempt` 分层（§8.1）。
- 重试**不覆盖**历史 Attempt 的记录，只追加。
- Task 级报告默认取**最后一个 Attempt**；UI 可切换查看历史 Attempt。

### 7.3 失败策略

> **裁决：** 默认隔离。要影响其他 Task，必须**显式声明**——Tester 与 Manifest 都能声明，取更严格者。

失败扩散有两处声明，方向不同：

| 位置 | 谁声明 | 表达什么 |
|---|---|---|
| `tester.yaml.on_failure` | Tester 维护者 | 这个 Tester 失败时**必须**怎样，例如"探活没过，后面测什么都没意义" |
| `tests[].on_failure` / `precheck.on_failure` | 提交者 | 这次执行希望怎样 |

**合并规则：取更严格者。** Tester 声明 `abort_run` 而 Manifest 写 `continue` 属配置冲突，**提交阶段拒绝**（`VALIDATION_ERROR`），不静默降级。

```yaml
# tester.yaml
on_failure: abort_run        # continue | abort_run
```

```yaml
# Manifest
on_failure: continue         # 只能在 Tester 未要求 abort_run 时生效
retry:
  max_attempts: 0            # 默认 0，即不自动重试
  backoff_seconds: 30
  retry_on:                  # 错误码白名单，仅在列出的错误上重试
    - EXECUTOR_UNAVAILABLE
    - IMAGE_PULL_FAILED
```

| 策略 | 行为 |
|---|---|
| `continue`（默认） | 该 Task 失败后，其余 Task 继续执行；Run 终态 `COMPLETED_WITH_ERRORS` |
| `abort_run` | 该 Task 失败后**终止同 Run 内所有未完成的 Task**：`PENDING` / `QUEUED` 标 `SKIPPED`（`reason: upstream_abort`）；`PREPARING` / `RUNNING` 调 Executor 取消并标 `CANCELED`。随后 Run 进入 `CLEANUP`，终态 `COMPLETED_WITH_ERRORS` |
| 重试 | 仅当错误码命中 `retry_on` 且 `attempt < max_attempts` 时自动重试；重试沿用同一参数与环境 |

`abort_run` 不是"跳过还没开始的"，而是**连正在跑的也一起终止**：一个前置门失败后，继续消耗 GPU 跑下去只是浪费。

**强制规则：**

- `role: pre_check` 的 Tester **必须**在 `tester.yaml` 声明 `on_failure: abort_run`；注册校验直接拒绝没声明的（§5.6）。这条取代了"平台按 role 隐含 abort_run"——隐含规则看不见也校验不了，声明才可以。
- `retry_on` 必须使用附录 D 的错误码枚举，不接受自由文本。
- 不配置 `retry_on` 时不得自动重试——避免把确定性错误（参数错误）重试成噪声。

### 7.4 Run 状态机

```text
SUBMITTED
    │
    ▼
QUEUED
    │
    ▼
DEPLOYING（存在 Deploy 时）
    │
    ▼
TESTING（执行全部 Task；平台注入的 pre_check Task 排在第一个）
    │
    ▼
CLEANUP
    │
    ▼
SUCCEEDED / COMPLETED_WITH_ERRORS / SERVICE_UNAVAILABLE / FAILED
```

任意非终态可取消：

```text
CANCELLING ──► CANCELED
```

**没有独立的 `PRECHECK` 阶段**：注入的 pre_check 就是 Task 链上的第一个 Task（§10.1），它跑在 `TESTING` 里，失败时按 `abort_run` 终止其余 Task。

**TESTING 内的顺序：** Pre-Check（若有）先于其他 Task；Pre-Check 通过后，其余 Task 按端点互斥规则并发调度（§6.7），平台**不额外施加串行顺序**，是否并行由 Tester 的 `concurrency.endpoint_exclusive` 声明决定。

**`DEPLOYING` 可能较久**：Genet 侧可能排队数小时到数天，平台必须能表示 `queued` 状态、展示已等待时长并允许取消；部署总窗口由平台配置（默认 72 小时）。

**注意：不存在 `GATING` 阶段，也不存在 `GATE_FAILED` 终态。** 指标是否达标由下游系统或 Agent 判断，平台只保证把事实完整、准确地交付出去。

### 7.5 Run 终态语义

| 终态 | 语义 | 触发条件 |
|---|---|---|
| `SUCCEEDED` | 全部 Task 成功 | 所有非 `SKIPPED` 的 Task 均为 `SUCCEEDED` |
| `COMPLETED_WITH_ERRORS` | 流程走完但存在失败 | 至少一个 Task `FAILED`，且失败策略允许继续 |
| `SERVICE_UNAVAILABLE` | 被测服务不可用 | 注入的 pre_check Task 失败且错误可重试（即窗口内一直没就绪），或部署后服务未就绪 |

Pre-Check 以**不可重试**错误失败（4xx、认证失败）时，说明"服务在但配置/权限不对"，Run 归 `COMPLETED_WITH_ERRORS`，由具体 Task 的错误码定位；不把它记成服务不可用。
| `FAILED` | 平台侧无法完成流程 | Genet 请求失败、Executor 不可用、平台内部错误、清理之外的编排异常 |
| `CANCELED` | 用户主动取消 | 收到取消请求并完成资源回收 |

用户排查方向由此明确：`FAILED` 找平台，`SERVICE_UNAVAILABLE` 找服务部署方，`COMPLETED_WITH_ERRORS` 看具体失败 Task。

### 7.6 取消语义

取消 Run 时：

1. `PENDING` / `QUEUED` 的 Task 立即标记 `SKIPPED`，`reason: canceled`。
2. `PREPARING` / `RUNNING` 的 Task 调用 Executor 终止底层资源，状态置 `CANCELED`。
3. 取消必须尽力而为但不阻塞：终止失败时记录 `CLEANUP_FAILED` 并告警，不悬挂在 `CANCELLING`。
4. 按 Cleanup 策略处理被测服务（是否卸载由 Run 的 `cleanup.policy` 决定）。

取消单个 Task 只影响该 Task；若该 Task 的策略为 `abort_run`，其余 Task 按 §7.3 处理。

### 7.7 失败重跑与续跑

不引入 Case，续跑基于 Task：

- `SUCCEEDED` 的 Task：直接复用原结果，**不重新执行**。
- `FAILED` 的 Task：创建新 Attempt。
- `SKIPPED` 的 Task：按需补齐。
- 复用必须校验：Tester 版本、镜像引用、参数快照、Endpoint 标识四者未变。**不校验数据内容**（见 §5.7），也**不因为 Task 名字相同就认定是同一个测试**。

续跑支持两种形态：

- **同 Run 内重试**：追加 Attempt，Task 标识不变。
- **跨 Run 续跑**：新 Run 通过 `retry_of: <run_id>` 关联原 Run，未失败的 Task 直接引用原结果，报告中标注来源。

---

## 8. 报告与产物

### 8.1 产物组织

```text
<storage-root>/
└── <run_id>/
    ├── report.json              # Run 级汇总（平台生成）
    ├── report.md                # 人可读摘要（平台生成）
    ├── metadata.json            # Run 元信息与裁决快照
    ├── events.jsonl             # 状态与错误事件流
    ├── deploy/                  # 有 Deploy 时存在（§9.1）
    │   ├── manifest.yaml        # 实际提交给 Genet 的参数快照
    │   ├── pods.json            # Pod 列表与状态快照（含重启次数）
    │   └── logs/
    │       └── <pod_name>.log   # 按 Pod 分别落盘，测试期间持续追加
    └── tasks/
        └── <task_id>/
            ├── report.json      # Task 级报告（平台生成）
            └── attempts/
                └── <attempt_index>/
                    ├── result.json   # Tester 产出
                    ├── logs/
                    └── raw/
```

独立提交的 Task 使用相同结构，`run_id` 为占位的独立标识。

**Deploy 日志不是"部署完就冻结"的一次性产物。** 两个事实决定了它必须单独设计：

1. **一个 Deploy 可能对应多个 Pod**：多节点部署、PD 分离（router / prefill / decode）、主从实例，都各有各的日志流。
2. **日志在测试期间仍在产生**：服务起来了不等于日志停了——Tester 压测时 Pod 还在打请求日志。

所以：

- **按 Pod 一份文件**（`deploy/logs/<pod_name>.log`），不合并成一个流。合并会丢掉"哪台机器出的问题"，而排查恰恰需要这个。
- **边跑边写**：Run 结束才去 `kubectl logs` 拿不到——Pod 可能已被清理，或日志已被轮转。平台在 Run 存续期间持续追加写入。
- **记录区间**：每份日志标注起止时间，UI 要能看出"这份日志覆盖到什么时候"。
- **不受 `cleanup.policy` 删除影响**：Deploy 日志随 Run 产物保留，不随 Pod 一起回收。

**`attempts/<n>/` 就是 Executor 挂给容器的 `/workspace/output`**（§6.8）：Tester 直接写最终位置，平台不做搬运。因此 `<storage-root>` 必须对所有执行节点可见；"收集产物"退化为校验 `result.json` 并补齐上层的 `report.json`。

`events.jsonl` 是**派生文件**：库里的 `state_events` 才是权威，由单 writer 追加，重启后按 `seq` 幂等补齐，可整份重建（G-8）。

### 8.2 报告分工

| 文件 | 生成方 | 职责 |
|---|---|---|
| `result.json` | Tester | 符合附录 A 的标准化测试结果 |
| Task `report.json` | TestPlane | Task 状态、参数快照、Attempt 列表、结果与产物引用 |
| Run `report.json` | TestPlane | Task 汇总、状态统计、失败清单，**不做指标数学聚合** |
| `report.md` | TestPlane | 人类可读摘要 |
| `raw/` | Tester | 原始测试工具产物 |
| `logs/` | Executor / TestPlane | 执行日志 |
| `deploy/manifest.yaml` | TestPlane | 实际提交给 Genet 的参数快照，用于回答"当时到底部署了什么" |
| `deploy/pods.json` | TestPlane | Pod 列表、角色、节点与重启次数；多 Pod 场景下这是唯一能还原拓扑的地方 |
| `deploy/logs/<pod>.log` | TestPlane | 按 Pod 分别采集，测试期间持续追加（§8.1） |

平台**不解析** Tester 的原始日志或私有产物格式，只做搬运与索引。

### 8.3 指标规范

详见附录 A。除 Schema 约束外，还需满足：

- 每个指标必须带 `unit` 与 `statistic`，禁止只给裸数值。
- 同一 Task 内多组参数的结果靠 `labels` 区分，`name` 不允许编码维度（`ttft_c8` 这类命名不合规）。
- 性能测试的 `input_len` / `output_len` / `concurrency` 必须进入 `workload` 或 `labels`，不得丢失。
- **平台不判断两组指标是否可比**，只保证把工作负载与统计口径如实交付；是否可比由下游与 Agent 判断（与"不设门禁"一致）。

示例：

```json
{
  "metrics": [
    { "name": "ttft",  "value": 125.6, "unit": "ms",  "statistic": "mean", "sample_size": 400,
      "labels": { "input_len": 1024, "concurrency": 1 } },
    { "name": "ttft",  "value": 892.4, "unit": "ms",  "statistic": "p99",  "sample_size": 400,
      "labels": { "input_len": 1024, "concurrency": 1 } }
  ]
}
```

### 8.4 Run 汇总结构

Run 报告只做**组织与索引**，不对不同 Task 的指标做加减乘除：

```json
{
  "run_id": "...",
  "status": "completed_with_errors",
  "counts": { "total": 4, "succeeded": 2, "failed": 1, "skipped": 1, "canceled": 0 },
  "tasks": [
    { "task_id": "...", "name": "accuracy-dataset-a", "status": "succeeded",
      "attempts": 1, "result_ref": "tasks/.../result.json", "duration_ms": 812000 }
  ],
  "failures": [
    { "task_id": "...", "reason": "TESTER_NONZERO_EXIT", "error_code": "TESTER_NONZERO_EXIT" }
  ]
}
```

首次指标筛选、分组、图表化由 UI 与下游系统完成。

### 8.5 结果完整性

- 每个 `SUCCEEDED` 的 Task 必须有合法 `result.json`。
- `FAILED` 的 Task 必须有状态、错误码与 stderr 摘要。
- `SKIPPED` 的 Task 必须带 `reason`。
- 不允许缺少结果的 Task 被当作成功。
- 部分结果必须可以单独获取（不要求整 Run 完成才可读）。

### 8.6 日志采集与持久化

> **裁决：** 日志是**持续采集并直接落盘**的产物，不是"Run 结束后再去拉一次"的快照。Deploy 的 Pod 会重启，Tester 的 Pod 会被删除——只在结束时拉，这两类日志都会丢。

**两类日志，两个位置：**

| 类别 | 落盘位置 | 写的人 |
|---|---|---|
| Tester 执行日志（stdout / stderr） | `tasks/<task_id>/attempts/<n>/logs/stdout.log` | 平台采集容器输出 |
| Tester 自己写的结构化日志 | `tasks/<task_id>/attempts/<n>/logs/`（Tester 自定） | Tester |
| Deploy 的 Pod 日志 | `deploy/logs/<pod_name>.log` | 平台采集（§9.1） |

**采集方式：平台侧 collector，不是 sidecar。**

Deployment 由 Genet 渲染，平台没有权限往里加 sidecar，也不该为了采日志去改别人的部署参数。所以 collector 是平台自己的常驻组件：按 Genet 返回的 label selector 发现 Pod，用 `follow` 流式读取并追加写入共享存储。

**Pod 重启怎么处理：**

容器重启时日志流会断。这不是异常，是常态——OOM、探针失败、节点驱逐都会导致重启。处理规则：

1. **不覆盖，续写同一个文件**，并在断点插入一行标记：

   ```text
   --- [container restarted #3 at 06:44:11, previous exit: OOMKilled] ---
   ```

2. 若重启次数很多（或文件超过配额），**分段落盘**：`<pod>.1.log` / `<pod>.2.log`，`pods.json` 里记录每段的起止时间。分段比无限追加更好查。
3. **Pod 被删除**（扩容缩容、Run 清理）时写 `--- [pod terminated at ...] ---` 后关闭该文件。
4. **文件永不因 Pod 消失而删除**：它已经在共享存储上，与 Pod 生命周期解耦。

**collector 自身故障：**

允许丢日志，**不允许伪造连续**。恢复后写入：

```text
--- [log gap: 06:52:10 – 06:55:40, collector restarted] ---
```

宁可承认有缺口，也不要让读日志的人误以为"这段时间服务没输出"。

**配额与截断：**

单文件超过阈值（如 200 MB）就滚动到下一段并写标记；日志体积**不影响 Task 结果**——日志满了不是测试失败。

**停止条件：**

Run 进入 `CLEANUP` 后停止采集；**`cleanup.policy` 只决定被测服务是否卸载，不决定产物保留期**——产物与日志的保留期由平台配置（默认 30 天）统一决定，日志不单独提前删除。

**为什么不用集群日志系统：**

团队已有的 Loki / ES 可以作为二级备份，但**平台不能依赖它**：那意味着"看日志要先有另一套系统的权限"，而日志恰恰是排查时最先要的东西。平台自己落一份，是成本最低的确定性。

---

## 9. Genet 与外部系统

### 9.1 Genet

Genet 是服务部署的唯一职责方。TestPlane 只负责：

1. 根据 Manifest 调用 Genet 部署接口。
2. 跟踪部署状态。
3. 获取部署结果与**权威 Endpoint**。
4. 根据清理策略调用卸载接口。

TestPlane 不负责 Helm 渲染、服务安装、镜像部署和集群资源管理。

**Endpoint 是什么**：服务的描述（地址、协议、端口、模型名、就绪状态）。来源只有两种：Genet 部署成功后返回（权威来源），或提交时在 Manifest 的 `service` 块里直接给出（§10.1）。两种来源产出的 Endpoint 对下游完全等价——这正是"部署 / 不部署"两条路能共用同一套注入逻辑的原因。它只有两个消费方——Deploy 阶段判 `ready` / `blocked`，以及参数投影生成 `url` / `model_name` / `model_path`（§5.4.1）。除此之外不落文件、不跨 Task 传递、不下发给 Tester：`url` 不限制目标地址，平台不解析、不校验可达性。

`blocked` 是终态：直接失败并给出后端原因，既不进入后续 Task，也不下发给 Tester。

**Deploy 与 Pod 不是一对一。** 多节点部署、PD 分离（router / prefill / decode 各成一组）都可能让一次 Deploy 拉起多个 Pod。平台不解释这个拓扑——拓扑是 Genet 参数决定的——但必须：

- 把 Pod 清单（名称、角色、节点、重启次数）落进 `deploy/pods.json`，否则事后无法还原"当时部署成了什么样"；
- 按 Pod 分别采集日志到 `deploy/logs/<pod>.log`，并在 Run 存续期间持续追加（§8.1）；
- 不在 UI 层假设"只有一个服务实例"：日志面板按 Pod 分页签，详情页按 Pod 展示重启与状态。

部署失败的归属：`FAILED`（平台侧调用 Genet 失败）与 `SERVICE_UNAVAILABLE`（Genet 受理但服务未就绪）必须区分，见 §7.5。

**Deploy 会排队。** 配额紧张时 Genet 可能把部署排到队列里，时长从数小时到数天不等。平台必须：
- 表示 `queued` 状态并展示**水位计数 + 原因原文 + 已等待时长**；**不展示位次**（D-46，Genet 不提供）；
- 使用足够长的部署窗口（平台默认 72 小时，Profile 可覆盖），不按分钟级超时误判失败；
- 排队期间允许取消（先向 Genet 取消排队/卸载，失败只记 `CLEANUP_FAILED`）。

### 9.2 Jenkins

Jenkins 是 TestPlane 的普通调用方，不享有特权路径。

迁移原则：

- 现有 Jenkins 流水线可以继续存在。
- 测试执行逻辑逐步迁移到 Tester。
- Jenkins 通过 REST API 提交 Run 或 Task、查询状态、下载报告。
- Jenkins 不再维护独立的测试命令拼装和结果解析逻辑。
- **提交不做幂等**（D-37）：流水线重试提交会产生新的 Run；Jenkins 侧必须自己避免"盲目重试提交"，或提交前先查询、提交后记录 Run ID。
- 最终 Jenkins 退化为上层流水线客户端。

### 9.3 下游消费系统

下游只需要三样东西：

- Run ID（或 Task ID）。
- 标准报告接口。
- 标准产物下载接口。

下游不需要关心具体使用了 AISBench、EvalScope 还是其他工具，也不需要平台替它判断达标。

**关于历史兼容：** 若迁移期确有消费方依赖旧格式（如旧的 `summary.json`），由**外部适配层**生成，不进入平台核心契约，也不得因此改变附录 A 的标准结果结构。

---

## 10. Manifest

Manifest 只描述测试意图、参数和执行环境。**不允许在 Manifest 中直接定义任意 Shell 命令**——`args` 只承载 `--k=v` 形式的参数，不是命令。

### 10.1 字段语义

| 字段 | 说明 |
|---|---|
| `run.name` | Run 名称，用于展示，不参与复用判定 |
| `deploy` | 与 `service` 二选一；`deploy.parameters` 原样转交 Genet。多 Pod / PD 分离由这些参数决定，平台不解析拓扑，只记录返回的 Pod 清单（§9.1） |
| `service` | 与 `deploy` 二选一；不部署时直接给出已有服务的 `url` / `model_name` / `model_path`。平台把它当作 Endpoint 的来源，投影成三个公共参数注入（§5.4.1） |
| `precheck` | `auto`（默认）/ `off` / 显式对象。`auto`：平台取注册表里**唯一**一个 `role: pre_check` 的 Tester（0 个或多个都拒绝提交），用注入的 `url` / `model_name` 生成 args，插到 `tests[]` 最前面；execution profile 复用 `tests[0].execution.profile`，没有 tests 时用 `precheck.execution.profile`。显式对象则由提交者全权指定 |
| `env` | Run 级环境变量，明文，注入该 Run 的全部 Task（§12.4） |
| `secrets` | Run 级敏感值，**只填 Secret 引用**，由平台取值注入，不回显（§12.4） |
| `tests[]` | Task 列表；每项 = 一个 Task |
| `tests[].name` | 必填；同一 Run 内唯一，用于报告、日志定位与 UI 展示（§8.4 的 `tasks[].name` 即来自这里） |
| `tests[].tester` / `version` | 必填；必须命中已注册的白名单组合 |
| `tests[].execution.profile` | 必填；引用已登记的 Execution Profile |
| `tests[].args` | **一段字符串**，即 UI 那个输入框的内容（如 `--dataset=/data/a --concurrency=[1,4]`）。平台只做词法切分，不校验、不解释 |
| `tests[].on_failure` | `continue` / `abort_run`，默认 `continue`；**不能弱化** `tester.yaml.on_failure`（§7.3） |
| `tests[].timeout_seconds` | 可选；覆盖平台默认 Task 超时。Tester 不声明时限（§6.6） |
| `tests[].retry` | 重试策略，默认不重试 |
| `cleanup.policy` | `always` / `on_failure` / `never` |

`args` 里**不应该**出现公共参数（`--url` / `--model_name` / `--model_path`）：它们由平台从 `deploy` 或 `service` 取值后追加到 argv 末尾，写重复了会被拒绝（`VALIDATION_ERROR`，§5.4.1 第 4 条）。

### 10.2 完整 Run 示例

```yaml
run:
  name: qwen3-32b-daily

  deploy:
    provider: genet
    parameters:
      model: Qwen3-32B

  precheck: auto            # 平台注入 Task#1（role: pre_check）；要自定义时写显式对象

  env:                      # Run 级，注入所有 Task（§12.4）
    LOG_LEVEL: info
  secrets:                  # Run 级敏感值，只填引用
    ENDPOINT_API_KEY: endpoint/prod-qwen

  tests:
    - name: accuracy-dataset-a
      tester: aisbench-acc
      version: "1.0.0"
      execution: { profile: k8s-npu }
      args: "--dataset=/data/dataset-a"
      on_failure: continue

    - name: accuracy-dataset-b
      tester: aisbench-acc
      version: "1.0.0"
      execution: { profile: k8s-npu }
      args: "--dataset=/data/dataset-b"
      on_failure: continue

    - name: performance-sweep
      tester: aisbench-perf
      version: "1.0.0"
      execution: { profile: k8s-npu }
      on_failure: abort_run
      retry:
        max_attempts: 1
        backoff_seconds: 60
        retry_on: [IMAGE_PULL_FAILED, TASK_TIMEOUT]
      args: "--input_lens=[512,1024,4096] --output_lens=[128,512] --concurrency=[1,4,8,16]"

  cleanup:
    policy: always
```

### 10.3 单独 Task 示例

```yaml
task:
  name: existing-service-accuracy

  service:                        # 不部署：直接给出已有服务
    url: http://10.0.0.10:8000/v1
    model_name: Qwen3-32B
    model_path: /models/Qwen3-32B

  precheck: auto                  # 先插一个 Pre-Check Task，通过后再跑本 Task

  tester: aisbench-acc
  version: "1.0.0"

  execution:
    profile: k8s-npu

  args: "--dataset=/data/dataset-a"

  on_failure: continue
```

这个 Task **不拉服务**（没有 `deploy`），三个公共参数写在 `service` 里，由平台注入，`args` 里不写。换成"部署"的情况，把 `service` 整个换成 `deploy`，`args` 依然不用动——这正是把取值点统一到一处换来的好处。

### 10.4 资源路径语义

> **裁决：** 参数中的任何路径对平台都是**不透明字符串（opaque string）**。平台不解析、不校验存在性、不做路径替换、不猜测容器内外映射。

- 同一个 `--dataset=/data/dataset-a`，在 `k8s-npu` 与 `k8s-gpu` 下可能来自不同挂载，这**不是**平台需要抹平的差异。
- 路径如何可达由 Tester 决定：镜像内置、Profile 提供的挂载点、或 Tester 自行下载。
- Tester 若需要额外挂载点，在 `tester.yaml` 的 `mounts` 中声明：`name`（volume 名，可省略自动生成）/ `mount_path`（容器内绝对路径）/`read_only` / `source`（承载该挂载点的 PVC 名，缺省用平台 storage PVC）。平台按声明渲染 volume；**没有 `required` 字段**，也不做"Profile 是否供给"的提交期校验——挂载失败是**运行期**事实（Pod `FailedMount` / `FailedAttachVolume`），由 executor 观测后报 `MOUNT_UNAVAILABLE`（§附录 D）。
- 平台不得为"帮你找数据集"而引入资源登记与解析逻辑——那会把平台变成数据管理系统的重复实现。

### 10.5 引用与安全

- `deploy` / `precheck` / `tests[].execution.profile` 都只能**引用**已登记对象，不能在 Manifest 中内联凭据、主机器密信息或任意命令。
- `--url` 的值不做任何地址限制（§9.1）；凭据相关的约束见 §12.4。
- `args` 只接受参数，**不得**含 shell 元字符生效：平台按词法切分后直接 exec，不经过 shell（§5.3）。

---

## 11. Web UI

### 11.1 定位

> **裁决：** TestPlane 必须提供 Web UI。人也要用，UI 与 API / CLI / MCP 并列为一等消费方。

同时坚持 API First：**UI 的每一个操作都必须有对应的 REST API**，不允许存在"只有 UI 能做"的能力，也不允许 UI 持有 API 之外的状态。

### 11.2 P0 能力

| 能力 | 说明 |
|---|---|
| Run / Task 列表 | 按状态、时间、Tester 筛选，展示耗时与错误码 |
| Run / Task 详情 | 状态流转时间线、Attempt 列表、参数快照 |
| 报告查看 | 渲染 `result.json` 的指标表格（按 `labels` 分组）、`report.md`、产物下载 |
| 日志查看 | 只读，支持 Attempt 级定位；长日志分页/流式 |
| 提交 Run / Task | **一个输入框**：直接填 `--p1=1 --p2=2`。平台不渲染表单、不预填、不校验；参数说明放 Tester 详情页只读展示 |
| 环境变量与凭据 | **Run 级填一次**，注入该 Run 全部 Task；明文变量可回显，敏感值只显示"已设置"（§12.4） |
| Deploy 与 Pod 日志 | 详情页把执行链路画成 Deploy → Pre-Check → Task 的流程图，点节点展开；Deploy 节点下按 Pod 分页签看日志，标注"测试期间仍在追加"（§8.1、§9.1） |
| 提交 · 服务来源 | 一张卡片两个形态：**部署**填 Genet 的 YAML；**不部署**就在同一张卡片里填 `url` / `model_name` / `model_path`。两种情况平台下游行为一致——都要投影出这三个参数 |
| Tester 目录 | 浏览已注册 Tester、角色、版本、参数清单与示例 |
| 取消 / 重试 | 单个 Task 与整个 Run；重试需展示将复用的 Task 与将重跑的 Task |
| 环境信息 | Execution Profile 列表与其挂载能力（只读） |
| 看板 | 集群与队列（Volcano）快照、运行概况、最近 Run，带"新建 Run"入口（§11.5） |

### 11.3 明确不做

- 拖拽式流程编排器（编排通过 Manifest 表达）。
- 自定义报表 / BI（下游系统负责）。
- 用户注册与自助开户（使用统一身份源）。
- 指标达标判定与门禁展示（平台不设门禁，见 §14）。
- 在 UI 内直接编辑 Tester 代码或上传镜像。

### 11.4 技术约束

- 前端为 SPA，直连同一套 REST API，**不引入独立 BFF**。
- 状态推送使用 SSE 订阅 `/runs/{id}/events`，避免轮询。事件类型、信封与回放语义以 `impl/control-plane.md §C-9` 与 `07-api.md §7` 为准：`seq` 单调、`Last-Event-ID` 续传、**不是日志通道**（G-14）。
- 新 Tester 上线无需改前端：提交框永远是一个文本输入框，参数说明由 `tester.yaml.parameters` 只读展示，指标展示由 `result.json` 结构驱动。若新增 Tester 需要改前端，视为契约设计缺陷。
- 鉴权与 API 完全一致，UI 不自行判断权限，以后端返回为准。

### 11.5 看板

> **裁决：** 要一个看板，但它只回答三个问题：**现在忙不忙、谁在排队、有没有东西挂着**。

**不是 BI，不是监控大盘。** §11.3 明确不做自定义报表；看板是固定的、只读的一屏，给"我今天要不要提测试、提了要等多久"这个判断用。

三块内容：

| 区块 | 内容 | 为什么要 |
|---|---|---|
| 集群与队列 | 每个 Volcano 队列：状态、`allocated` / `deserved`、排队 Job 数、最长等待时长；节点 NPU / GPU 已用 / 总量 | 排队慢的时候，人第一个想知道的是"卡在队列还是卡在机器"，而不是去翻 K8s |
| 运行概况 | 进行中 Run 数、排队中 Task 数、近 24h 失败数、Pre-Check 失败数 | 一眼看出是不是集体性问题（全在排队 / 全在 Pre-Check 失败） |
| 最近 Run | 复用列表页的紧凑版，点进去看详情 | 常用入口，省一次跳转 |

**快捷创建**：看板右上角一个「新建 Run」按钮，直接进提交页；并支持"照上次的配置再来一次"——把最近一次 Run 的 Tester 组合与参数**预填**进表单（仍可改）。这是唯一一处允许预填的地方：预填的是**上一次的输入**，不是平台推导出来的语义。

**边界：**

- 只显示当下与近 24h 的计数，**不做趋势曲线、不做同比**（那是监控系统的事）。
- 不做告警配置、不发通知（§12.6 的告警走已有通道）。
- 排队原因只展示，**不提供"优先插队"这类操作**——平台没有这个权力（§6.9）。

归属：**P1**。P0 阶段至少要能在 Run 详情里看到 `waiting_reason`（§6.9），否则 `QUEUED` 是个黑盒。

---

## 12. 安全、可靠性与运维

TestPlane 是对内工具，但**对内部网络与生产主机拥有执行能力**，因此安全边界必须显式定义。

### 12.1 认证与授权

> **裁决：** 复用团队统一身份源（OIDC / SSO），不自建账号体系。

| 角色 | 权限 |
|---|---|
| `viewer` | 查看 Run / Task / 报告 / 日志 / Tester 目录 |
| `operator` | viewer + 提交 Run 与 Task、取消、重试 |
| `admin` | operator + 管理 Tester 注册、Execution Profile、清理策略 |

约束：

- 每个写操作都要记录操作者（写入 `events.jsonl` 与审计表），**禁止匿名提交**。
- 取消他人提交的 Run 需要 `operator` 及以上，且必须留痕。
- 服务间调用（Jenkins、Agent）使用服务令牌，RBAC 与用户相同，不设后门。
- Tester 注册（发布新能力）仅 `admin`，且走代码评审流程，不接受运行时上传。

### 12.2 网络与执行边界

- 平台不限制 `--url` 指向哪里（§9.1）：内网、宿主机、公网目标都不做地址校验，实际可达性由下面的出站策略决定。
- 平台不管理任何远端执行主机：SSH / Docker 主机上的运行属平台外行为（§6.4），其网络与凭据由使用者负责。
- Executor 侧网络策略默认拒绝出站到互联网，仅允许镜像仓库与目标服务网段。
- 容器默认非 root、只读根文件系统（除 `/workspace`），不允许 `privileged`。

> 平台不承担"在一台远端主机上执行命令"的能力（§6.4）。这类场景由使用者自行运维；平台的攻击面收敛到 K8s API 与 Secret 两处。

### 12.3 安全约束清单

| 项目 | 约束 |
|---|---|
| Tester 注册 | 仅 `admin` 可新增或发布，走评审 |
| 镜像 | 引用形态不限（tag / digest 均可），由团队约定与 CI 保证版本可追溯 |
| Endpoint | 地址不限，平台不做白名单校验（§9.1） |
| 凭据 | 由平台注入，不写入输入目录、不落日志（§12.4） |
| Manifest | 禁止提交任意 Shell 命令 |
| 参数 | 以 argv 数组传递，禁止经过 Shell 解释 |
| 超时 | 每个 Task 必须有最大执行时间 |
| 取消 | 必须能终止底层执行资源 |
| 产物 | 必须持久化，不依赖临时 Pod / 容器 |
| 续跑 | 必须校验复用结果的有效性（§7.7） |
| 版本 | Tester 版本可追溯、不可静默覆盖 |

### 12.4 环境变量与凭据

> **裁决：** 环境变量是 **Run 粒度**的：提交时填一次，平台注入该 Run 下的**所有** Task。Tester 不各自绑定，只声明自己读哪些变量。
>
> **明文变量不做 Secret 化**：这是内部工具，`env` 里的值按明文处理——可见、可回显、可直接改。不搞"全部进 Secret、只显示已设置"那一套：它对内部单团队没有收益，只让查看和修改变麻烦。只有 `secrets` 里的 token 走 Secret Store，因为凭据会随 Manifest 落到代码库和日志里。

**① Tester 声明读哪些变量**（`tester.yaml.env`）

这份声明是**说明书 + 必填检查依据**，不是绑定入口：

```yaml
env:
  ENDPOINT_API_KEY:
    required: false          # 探活不强制鉴权
    secret: true             # 敏感值：注入后脱敏、不记日志、不回显
    description: "Bearer token，用于 Authorization 头"
```

**② 提交时填一次，Run 内共用**

Manifest 里分两块，把敏感值和普通变量隔开：

```yaml
run:
  name: qwen3-32b-daily

  env:                       # 明文，注入所有 Task，UI 可回显
    LOG_LEVEL: info
    HTTP_TIMEOUT: "300"

  secrets:                   # 敏感，只填引用；平台不回显、不落明文
    ENDPOINT_API_KEY: endpoint/prod-qwen
```

- 该 Run 下每个 Task（含平台注入的 Pre-Check）拿到的环境变量完全相同；Tester 按自己声明的名字去读，读不到就用默认值。
- **填一次，全链生效**：探活的 key 与精度测试的 key 是同一个，不必在多个 Task 上重复维护。
- 必填检查：某 Task 的 Tester 声明了 `required: true` 的 env，而 Run 的 `env` / `secrets` 都没提供 → **提交阶段拒绝**（`VALIDATION_ERROR`）。

**③ 值只存在 Secret Store**

K8s Secret，由 `admin` 登记与轮换；Manifest 里只有**引用**（如 `endpoint/prod-qwen`）。平台只保存引用，不让明文落关系库、不经日志、不回显给 `viewer`。

> 命名约定**不做强制**。示例里的 `endpoint/prod-qwen` 只是可读性习惯，平台不解析这个名字，也就不需要"按 Endpoint 自动推荐凭据"——选择权在提交人手上。

**④ 注入与脱敏**

平台把 Run 级 `env` 与 `secrets` 展开到每个 Task 的 Job Spec（`env` / `secret_env`，附录 B），Executor 从 Secret Store 取值注入。此外：

- 凭据不得出现在 `args` 与产物目录；Tester 不得主动打印凭据。
- **平台不做日志脱敏**（团队裁决 2026-11-04）：Tester 与 Deploy 日志按明文落盘，平台不读取 Secret 值做替换；凭据一旦被 Tester 打印即会进入日志，该风险由团队显式接受。
- 轮换只改 Secret，不改 Manifest，也不必重建 Tester 版本。

**UI 上的位置**：提交表单是「一个参数输入框 + 一组 Run 级环境变量 + 一组凭据引用」。凭据是下拉，只列已登记 Secret 的名称与说明，不渲染明文；没有可用凭据时给登记入口。凭据不属于参数输入框——这正是 `args` 里不能出现 token 的原因。

**独立运行（§5.10）**：没有平台，变量由使用者自己 `docker run -e ...` 给。平台不代管任何平台外主机的密钥。

**权限**：登记 / 轮换 Secret 仅 `admin`；提交时填 `env` / `secrets` 需要 `operator`；`viewer` 只看到变量名，看不到敏感值。

### 12.5 数据与状态存储

| 数据 | 存储 | 说明 |
|---|---|---|
| Run / Task / Attempt / 状态事件 | 关系库（PostgreSQL） | 唯一的权威状态源；控制端无内存状态 |
| Tester 注册信息 | 关系库 | 与 `testers/` 目录扫描结果同步，以库为准 |
| Execution Profile | 关系库 | 由 `admin` 维护 |
| 报告与产物 | NFS（所有 K8s 节点可见） | 按 `<run_id>/tasks/<task_id>/attempts/<n>/` 组织；Tester 直写，见 §6.8 |
| 事件流 | 文件 + 库 | `events.jsonl` 供流式消费，库供查询 |

约束：

- 状态机写入必须与业务操作在同一事务内（`No Hidden State`）。
- 控制端重启后，处于 `RUNNING` 的 Task 必须通过 Executor 对账恢复真实状态，不得直接假定失败。
- 状态与产物分离：库挂掉不影响已有产物可下载。

### 12.6 可观测性

平台自身必须具备可观测性（不是被测服务的可观测性）：

- `/metrics` 暴露 Prometheus 指标：`task_duration_seconds`、`queue_depth`、`task_failures_total{code}`、`precheck_failures_total`、`executor_errors_total`、`cleanup_failures_total`。
- 结构化日志（JSON），每条含 `run_id` / `task_id` / `attempt_id`。
- 事件流：`submitted` / `state_changed` / `attempt_started` / `attempt_finished` / `cleanup_started` / `run_finished`。
- 告警：清理失败、Executor 不可达、队列积压超过阈值、Pre-Check 连续失败。

### 12.7 容量与非功能约束（初始值，可调整）

| 项目 | 约束 |
|---|---|
| 单 Run 最大 Task 数 | 200（超出拒绝，`RUN_TOO_LARGE`） |
| 全局并发执行 Task 数 | 可配置，默认 20 |
| 单 Task 最大执行时长 | 默认 4 小时，配置上限 24 小时 |
| 报告写入 | 单 Run 报告 ≤ 20 MB，超出则产物外置 |
| 单次日志读取 | 分页返回，避免整文件加载 |
| 单次 Attempt 产物上限 | 默认 10 GiB；超出判失败（`ARTIFACT_QUOTA_EXCEEDED`），避免单个 Task 打满共享存储。**执行方式**：collector 每 60s 对 `<run>/tasks/<task>/attempts/<n>` 做 `du` 扫描，超限即删 Job 并判该 Attempt 失败（G-7） |
| 状态查询延迟 | P95 < 300 ms（非日志类接口） |

---

## 13. Non-Goals

分两类：**永久非目标**（属于系统定位，不应重新讨论）与**当期不做**（可在后续版本重新评估）。

### 13.1 永久非目标

| 不做 | 理由 |
|---|---|
| 不自研测试算法 | 复用 AISBench、EvalScope 等现有工具 |
| 不自研底层调度 | 复用 K8s、Volcano 和已有环境能力 |
| 不做服务部署平台 | Genet 已经承担该职责 |
| 不引入 Case | Task 已经是最小独立执行单元 |
| 不解析 Tester CLI | 测试命令属于 Tester 内部实现 |
| **不渲染、不校验 Tester 参数** | 平台只做词法切分与公共参数追加（§5.3、§5.4）；UI 只有一个输入框，参数清单仅供 Agent 与维护者阅读 |
| 不解析 Tester 原始产物 | Tester 负责生成标准化结果 |
| **不做门禁判定（Gate）** | 平台只产出事实数据；是否达标由下游系统或 Agent 判断，避免把业务阈值固化进平台 |
| **不做 SSH / Docker 执行后端** | 平台只有 K8s 一种 Executor；在 Docker 主机上跑 Tester 属平台外运行方式（§6.4），不登记主机、不代管凭据、不进入平台生命周期 |
| 不替 Agent 做结论 | 平台提供事实，Agent 负责解读 |
| 不允许用户上传任意 Tester | 保证测试执行能力可控 |
| 不托管数据集与模型 | 沿用现有资源管理方式，平台只透传路径 |
| 不承诺结果可复现 | 平台保证执行逻辑可追溯（§5.7），数据内容由 Tester 与外部管理 |

### 13.2 当期不做（可重新评估）

| 不做 | 理由 |
|---|---|
| 不做模型仓库 | 沿用现有模型存储 |
| 不做数据集仓库 | 沿用 NFS、镜像等现有资源管理方式 |
| 不做镜像仓库管理 | 复用现有镜像管理能力 |
| 不做拖拽式流程编排 | Manifest 已足够表达依赖，编排器的复杂度不划算 |
| 不做自定义报表 / BI | 下游分析系统负责 |
| 不做多租户隔离 | 对内单团队使用，RBAC 已够用 |

---

## 14. P0 / P1 范围

### 14.1 P0：最小可用闭环

| 功能 | 验收要求 |
|---|---|
| Run 提交 | Manifest 提交后，Run 按 §7.4 走完整状态链，终态正确 |
| Task 提交 | 已有服务可独立提交 Task（`url` / `model_name` 作为公共参数给出），产物结构与非独立 Task 一致 |
| Genet 适配 | 部署、状态查询、Endpoint 获取三项可用；部署失败与未就绪可区分 |
| Pre-Check | `precheck: auto` 时平台注入 Task#1（`role: pre_check`）；它独占该 Endpoint，未通过时按 `abort_run` 终止其余 Task，Run 终态 `SERVICE_UNAVAILABLE`。注入时该 Task 的 `args` 为空，`url`/`model_name`/`model_path` 按 §5.4.1 在末尾注入（G-11）；判别某个 Task 是不是 Pre-Check 的唯一依据是 `runs.resolved.precheck.injected`（`03-data-view.md §4.2`） |
| Tester Registry | 目录自动发现并通过校验；非法 `tester.yaml` 被拒绝并给出字段级错误 |
| Tester 契约 | 唯一入口、argv 参数传递、输出目录、`result.json` 全部可校验 |
| **Tester 独立运行** | 不依赖平台：`docker run <image> --p1=1 --p2=2` 即可跑通并产出符合附录 A 的 `result.json`（§5.10） |
| K8s Executor | 使用 Job 执行，超时可终止，取消可删除 Job |
| Task 状态机 | 8 个状态、状态落库、重启后可恢复；`SKIPPED` 与 `FAILED` 严格区分 |
| Attempt 与重试 | 每次执行独立 Attempt；按 `retry_on` 白名单重试；历史不被覆盖 |
| 失败策略 | `continue` / `abort_run` 生效；`abort_run` 终止同 Run 内所有未完成 Task；Pre-Check 由其 `tester.yaml` 声明强制 `abort_run` |
| Run 编排 | Deploy → Task 链（含平台注入的 Pre-Check）→ Cleanup 全链路贯通 |
| 结果报告 | Task 与 Run 级 `report.json` 符合 §8.2 分工与 §8.4 汇总结构；`result.json` 符合附录 A |
| 产物持久化 | 产物直接落在共享存储的最终路径（§6.8），容器销毁后仍可获取报告与原始产物 |
| 失败隔离 | 单个 Task 失败不影响其他 Task（策略为 `continue` 时） |
| **Web UI** | 列表、详情、报告、日志、单输入框提交、取消/重试可用；全部操作有对应 REST API |
| API 契约 | OpenAPI 描述完备，错误码使用附录 D 枚举 |

### 14.2 P1：AI 友好与效率

| 功能 | 验收要求 |
|---|---|
| MCP Server | Tester 查询、Run/Task 提交与查询、取消、报告获取 |
| CLI | API 能力的命令行映射，无独有能力 |
| 结构化事件 | `events.jsonl` + SSE 推送状态与错误码 |
| 失败续跑 | 仅重跑失败或缺失的 Task，复用结果按 §7.7 校验 |
| Run 对比 | 同名指标 + `labels` + 工作负载对齐展示；平台不判断可比性 |
| Matrix 展开 | 提交时将参数矩阵自动展开为独立 Task（可选能力，不替代 §4.2） |
| Tester 校验工具 | 维护者可在本地校验 `tester.yaml`、`run.sh` 与样例执行结果 |
| UI 增强 | 指标图表、Attempt 间对比、批量操作 |
| 看板 | 集群 / Volcano 队列快照、运行概况、快捷新建 Run（§11.5） |

---

## 15. 文档维护与变更流程

1. 修改本文中带"裁决"的条目，必须：提升版本号、在 §0.5 记录、并在附录 E 追加决策条目。
2. 修改附录 A / B / C 的契约字段属于**破坏性变更**，必须提升 `schema_version`，并给出至少一个版本的兼容期。
3. Non-Goals 的调整：§13.1 需要重新论证系统定位；§13.2 只需在变更记录中说明即可。
4. 实现与本文冲突时以本文为准；若认为本文有误，先改本文再改代码，不允许"代码先行、文档滞后"。

---

## 附录 A：`result.json` 结构

Tester 必须产出，平台校验。缺失 `schema_version`、`task_id`、`tester` 或 `metrics` 视为不合格（`RESULT_INVALID`）。

```json
{
  "schema_version": "1.0",
  "run_id": "r-20261103-0001",
  "task_id": "t-0007",
  "attempt_id": "t-0007-a2",
  "tester": {
    "name": "aisbench-perf",
    "version": "1.0.0",
    "image_ref": "registry.internal/testplane/aisbench-perf:1.0.0"
  },
  "status": "succeeded",
  "started_at": "2026-11-03T10:00:00+08:00",
  "finished_at": "2026-11-03T10:14:20+08:00",
  "duration_ms": 860000,
  "workload": {
    "model_name": "Qwen3-32B",
    "dataset": "/data/dataset-a",
    "input_len": 1024,
    "output_len": 128,
    "concurrency": 8
  },
  "metrics": [
    {
      "name": "ttft",
      "value": 125.6,
      "unit": "ms",
      "statistic": "mean",
      "sample_size": 400,
      "labels": { "input_len": 1024, "output_len": 128, "concurrency": 8 }
    },
    {
      "name": "throughput",
      "value": 42.7,
      "unit": "req_per_s",
      "statistic": "mean",
      "sample_size": 400,
      "labels": { "input_len": 1024, "output_len": 128, "concurrency": 8 }
    }
  ],
  "error": null,
  "artifacts": [
    { "name": "raw_results", "path": "raw/perf.xlsx", "size_bytes": 20480, "sha256": "9ab3..." }
  ],
  "notes": "并发 16 档位触发服务端 429，已跳过，见 labels=concurrency:16 的缺失记录",
  "metadata": { "tool": "aisbench", "tool_version": "0.9.1" }
}
```

| 字段 | 类型 | 必填 | 约束 |
|---|---|---|---|
| `schema_version` | string | 是 | 当前为 `"1.0"`；不兼容变更才升版本 |
| `run_id` / `task_id` / `attempt_id` | string | 是 | 平台运行时取 `TESTPLANE_RUN_ID` / `TESTPLANE_TASK_ID` / `TESTPLANE_ATTEMPT_ID` 环境变量；**独立运行**（§5.10）时填 `local` 即可 |
| `tester.name` / `tester.version` | string | 是 | 必须与注册表一致（含 Pre-Check Tester） |
| `tester.image_ref` | string | 否 | 实际运行的镜像引用；平台可注入，Tester 不知道时可缺省 |
| `status` | enum | 是 | `succeeded` / `failed`（Tester 视角，仅描述执行是否完成） |
| `started_at` / `finished_at` | RFC3339 | 是 | 允许与平台记录有毫秒级差异 |
| `duration_ms` | integer | 否 | 缺失时由平台按时间戳计算 |
| `workload` | object | 否 | 工作负载摘要；性能类 Tester 必须提供 |
| `metrics` | array | 是 | 允许空数组（纯探测类），但需在 `notes` 说明 |
| `metrics[].name` | string | 是 | `snake_case`，不得编码维度 |
| `metrics[].value` | number \| null | 是 | `null` 表示无法计算，必须在 `notes` 说明原因 |
| `metrics[].unit` | string | 是 | 推荐：`ms` / `s` / `req_per_s` / `tokens_per_s` / `tokens` / `percent` / `score` / `count` |
| `metrics[].statistic` | enum | 是 | `mean` / `p50` / `p90` / `p95` / `p99` / `min` / `max` / `sum` / `count` |
| `metrics[].labels` | object | 否 | 标量值；多组参数必须靠它区分 |
| `metrics[].sample_size` | integer | 否 | 样本量；性能与精度指标强烈建议提供 |
| `error` | object \| null | 是 | 失败时必填，结构 `{code, message, detail, retryable}` |
| `artifacts[]` | array | 否 | `path` 为相对 `/workspace/output` 的路径 |
| `notes` | string | 否 | 自由文本，仅用于人类排查，平台不解析 |
| `metadata` | object | 否 | 工具名与版本等溯源信息 |

**唯一性约束：** 同一 `labels` 组合下，`name + statistic` 组合必须唯一。

---

## 附录 B：Job Spec 结构

平台 → K8s Executor 的唯一定义。它只被平台内部使用；Tester 不知道 Job Spec 的存在（独立运行时根本没有 Job Spec，§5.10）。

```json
{
  "spec_version": "1.0",
  "job_id": "j-9f3a1c",
  "run_id": "r-20261103-0001",
  "task_id": "t-0007",
  "attempt": 1,
  "executor": "kubernetes",
  "image": "registry.internal/testplane/aisbench-perf:1.0.0",
  "command": ["/tester/run.sh"],
  "args": ["--input_lens=[512,1024,4096]", "--concurrency=[1,4,8]", "--url=http://10.0.0.10:8000", "--model_name=Qwen3-32B"],
  "env": {
    "TESTPLANE_RUN_ID": "r-20261103-0001",
    "TESTPLANE_TASK_ID": "t-0007",
    "TESTPLANE_ATTEMPT_ID": "t-0007-a1",
    "TZ": "Asia/Shanghai"
  },
  "secret_env": [
    { "name": "ENDPOINT_API_KEY", "secret_ref": "endpoint/qwen3-prod/api_key" }
  ],
  "resources": {
    "cpu": "4",
    "memory": "16Gi",
    "gpu": { "resource": "nvidia.com/gpu", "count": 1 }
  },
  "timeout_seconds": 7200,
  "mounts": [
    { "name": "output", "type": "output", "mount_path": "/workspace/output", "read_only": false,
      "storage_path": "<storage-root>/r-20261103-0001/tasks/t-0007/attempts/1",
      "sub_path": true },
    { "name": "dataset","type": "profile_mount", "profile_mount": "dataset-nfs", "mount_path": "/data", "read_only": true }
  ],
  "network": { "egress_allow": ["registry.internal", "10.0.0.0/8"] },
  "node_selector": {},
  "tolerations": [],
  "security_context": { "run_as_non_root": true, "read_only_root_filesystem": true },
  "labels": { "testplane.io/run": "r-20261103-0001", "testplane.io/task": "t-0007" },
  "annotations": {}
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `spec_version` | 是 | 当前 `1.0`；Executor 遇到未知版本必须拒绝（退出码 2） |
| `job_id` | 是 | 幂等键；同一 `job_id` 重复提交返回既有作业 |
| `run_id` / `task_id` / `attempt` | 是 | 用于回填与目录组织 |
| `image` | 是 | 镜像引用，原样来自 `tester.yaml`；平台不解析、不限制 tag / digest |
| `executor` | 是 | 目前唯一合法值 `kubernetes`；保留字段用于未来扩展，但**不得**在此之前引入第二个执行后端 |
| `command` | 是 | 固定为 Tester 的 `entry`（`["/tester/run.sh"]`），平台不拼装业务命令 |
| `args` | 是 | 参数数组：调用方输入经词法切分后的结果 + 末尾追加的 `--url/--model_name/--model_path`。**不经过 shell** |
| `env` | 否 | 非敏感环境变量；必须含 `TESTPLANE_RUN_ID` / `TESTPLANE_TASK_ID` / `TESTPLANE_ATTEMPT_ID` |
| `secret_env` | 否 | 敏感变量，引用 Secret Store 键，**不传明文** |
| `resources` | 是 | `gpu.count: 0` 表示不需要加速卡 |
| `timeout_seconds` | 是 | Executor 必须据此强制终止 |
| `mounts` | 是 | 只有 `output`（读写，必须给 `storage_path`，由 Executor 以 subPath 挂载）与 `profile_mount`（只读）两种；**没有 input 挂载**（§5.3、§6.8） |
| `network.egress_allow` | 否 | 缺省时使用 Executor 默认策略 |
| `security_context` | 否 | 缺省为 `run_as_non_root: true` |

Executor 返回结果：

```json
{
  "job_id": "j-9f3a1c",
  "phase": "succeeded",
  "exit_code": 0,
  "started_at": "2026-11-03T10:00:00+08:00",
  "finished_at": "2026-11-03T10:14:20+08:00",
  "reason": null,
  "log_ref": "task-output/t-0007/1/logs",
  "artifact_ref": "task-output/t-0007/1",
  "executor_ref": { "namespace": "testplane", "job": "tp-t-0007-a1" }
}
```

`phase` 枚举：`pending` / `running` / `succeeded` / `failed` / `canceled` / `timeout` / `unknown`。

**映射规则：** `succeeded` → 读取 `result.json` 决定 Task 状态（存在即 `SUCCEEDED`，缺失为 `RESULT_MISSING`）；`failed` / `timeout` / `canceled` 分别映射 `FAILED` / `FAILED(TASK_TIMEOUT)` / `CANCELED`；`unknown` 必须由对账任务复核，**不得**直接判失败。

---

## 附录 C：`tester.yaml` 结构

```yaml
name: aisbench-perf
version: 1.0.0
display_name: AISBench 性能测试
description: 对已部署 LLM 服务执行性能压测，输出 TTFT / TPOT / 吞吐等指标
role: test                    # test | pre_check
on_failure: continue          # continue | abort_run；pre_check 必须 abort_run（§7.3）
entry: /tester/run.sh

image: registry.internal/testplane/aisbench-perf:1.0.0

resources:
  cpu: "4"
  memory: "16Gi"
  gpu: { resource: nvidia.com/gpu, count: 0 }

concurrency:
  endpoint_exclusive: true    # 独占该 Endpoint；性能类与 Pre-Check 必须为 true（§6.7）

mounts:
  - { name: dataset, mount_path: /data, read_only: true, source: dataset-nfs }

# 受限 K8s 需求（可选，白名单字段）。平台渲染 Job 时合并；平台字段优先且不可覆盖（§6.8）。
# 不允许写完整清单，也不允许 privileged / hostPath / hostNetwork。
k8s:
  node_selector: { accelerator: npu }
  tolerations:
    - { key: accelerator, operator: Exists, effect: NoSchedule }
  security_context: { run_as_user: 10001, fs_group: 10001 }

parameters:
  dataset:
    type: string
    description: "数据集在容器内的绝对路径；由挂载提供，Tester 自己校验存在性"
    example: "/data/gsm8k.jsonl"
    required_for: "精度类 Tester"

env:                          # 运行期环境变量声明（§12.4）
  ENDPOINT_API_KEY:
    required: false
    secret: true
    description: "Bearer token；由平台从 Secret Store 注入，Tester 只读不打印"
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` / `version` | 是 | `name` 全局唯一；`version` 语义化版本，发布后不可覆盖 |
| `display_name` / `description` | 是 | 中文，UI 展示用 |
| `role` | 是 | `test` 普通测试；`pre_check` 参与服务就绪检查，且**必须**同时声明 `on_failure: abort_run`（§7.3） |
| `on_failure` | 是 | `continue` / `abort_run`。Tester 声明自己的失败是否必须终止整个 Run；Manifest 只能取更严格者，不能弱化（§7.3） |
| `entry` | 是 | 必须为 `/tester/run.sh`（Single Entry Point） |
| `image` | 是 | 镜像引用（tag 或 digest 均可）；平台不解析、不限制形态，由团队约定版本策略（§5.6） |
| `requires` | 禁止 | **不设此字段**。Tester 不声明对 Endpoint / 模型的依赖——它只读 `--url` / `--model_name` / `--model_path`；注入与否是平台的事（§5.4.1）。资源类的需求（GPU、CPU）仍用 `resources` 表达 |
| `resources` | 是 | 默认资源需求，可被 Manifest 提高不可降低 |
| `timeout_seconds` | 禁止 | **不设此字段**。跑多久是编排策略，不是 Tester 属性；Task 超时由平台配置 + `tests[].timeout_seconds` 覆盖（§6.6） |
| `concurrency.endpoint_exclusive` | 是 | 是否独占该 Endpoint：性能类与 **Pre-Check 必须 `true`**，精度类默认 `false`（§6.7） |
| `mounts` | 否 | 额外挂载声明：`name` / `mount_path` / `read_only` / `source`（PVC 名，缺省用 storage PVC）。平台按声明渲染 volume；挂载失败由**运行期**观测报 `MOUNT_UNAVAILABLE`，不做提交期校验（D-60） |
| `k8s` | 否 | 受限 K8s 需求，白名单字段：`node_selector` / `tolerations` / `annotations` / `security_context`（仅 `run_as_user`、`fs_group`）。**禁止**完整清单、`privileged`、`hostPath`、`hostNetwork`、自定义 `image` / `command`（§6.8） |
| `parameters` | 是 | **参数清单（文档语义）**，见 §5.4：只给 Agent 与人类看，平台不渲染、不校验、不据此生成表单 |
| `env` | 否 | 声明本 Tester **读取**哪些环境变量：`required` / `secret` / `description`。用于文档与必填检查；值由 Run 级 `env` / `secrets` 提供（§12.4） |
| `placeholder` | 否 | 一段示例命令行（不含程序名），UI 作输入框占位提示；文档语义，平台不解析 |
| `outputs` | 禁止 | **不设此字段**。产物契约全局统一：`result.json` 必须有、`raw/` 与 `logs/` 自由（§5.3、§5.9）。每个 Tester 再声明一遍既冗余也没有消费方 |

---

## 附录 D：错误码

错误对象统一结构：

```json
{ "code": "TASK_TIMEOUT", "message": "Task 执行超过 7200 秒被终止", "detail": { "task_id": "t-0007", "timeout_seconds": 7200 }, "retryable": false }
```

| code | HTTP | 阶段 | 可重试 | 说明 |
|---|---|---|---|---|
| `VALIDATION_ERROR` | 400 | 提交 | 否 | Manifest 结构不合规，或平台已注入某公共参数时 `args` 里又出现同名参数（§5.4.1） |
| `UNAUTHENTICATED` | 401 | 全阶段 | 否 | 缺少或无效令牌 |
| `FORBIDDEN` | 403 | 全阶段 | 否 | 角色权限不足 |
| `NOT_FOUND` | 404 | 全阶段 | 否 | 对象不存在 |
| `CONFLICT` | 409 | 提交 | 否 | 名称 / 版本冲突 |
| `TESTER_NOT_FOUND` | 400 | 提交 | 否 | 未注册的 Tester |
| `TESTER_VERSION_PINNED` | 409 | 注册 | 否 | 试图覆盖已发布版本 |
| `TESTER_INVALID` | 400 | 注册 | 否 | `tester.yaml` / `run.sh` 不合规 |
| `EXECUTION_PROFILE_NOT_FOUND` | 400 | 提交 | 否 | 未登记的 Execution Profile |
| `RUN_TOO_LARGE` | 400 | 提交 | 否 | Task 数超过上限（§12.7） |
| `RESOURCE_EXHAUSTED` | 429 | 调度 | 是 | 并发槽位或配额不足，稍后重试 |
| `ARTIFACT_QUOTA_EXCEEDED` | 507 | 收集 | 否 | 单次 Attempt 产物超过配额（§12.7），Task 判失败 |
| `EXECUTOR_UNAVAILABLE` | 503 | 执行 | 是 | K8s API 不可达 |
| `IMAGE_PULL_FAILED` | 502 | 执行 | 是 | 镜像拉取失败 |
| `MOUNT_UNAVAILABLE` | 502 | 执行 | 否 | Pod 挂载失败（`FailedMount` / `FailedAttachVolume`），由 executor 观测到 Pod 状态后报（运行期，不是提交期校验） |
| `EXECUTOR_POD_ERROR` | 502 | 执行 | 是 | Pod 异常终止（`detail.k8s_reason` 保留原文，如 `Evicted`）；`Unschedulable` 不算失败，按 §6.9 留在排队态 |
| `DEPLOY_FAILED` | 502 | Deploy | 否 | **平台侧**调用 Genet 失败（连接/5xx/同名冲突/安装失败）→ Run `failed`；`detail.reason` 保留 Genet 原文 |
| `DEPLOY_BLOCKED` | 502 | Deploy | 是 | Genet 报 `blocked`（配额/资源/调度阻塞）或超部署窗口 → Run `service_unavailable`；`detail.reason ∈ {blocked,timeout,superseded,uninstalling,uninstalled}`（D-47） |
| `ENDPOINT_UNREACHABLE` | 502 | Pre-Check | 是 | 网络不可达或连接被拒 |
| `PRECHECK_FAILED` | 502 | Pre-Check | 是 | 探测返回非预期状态 |
| `TASK_TIMEOUT` | 504 | 执行 | 按策略 | 超过 `timeout_seconds` |
| `TESTER_NONZERO_EXIT` | 500 | 执行 | 否 | Tester 非零退出 |
| `RESULT_MISSING` | 500 | 收集 | 否 | 退出码 0 但未产出 `result.json` |
| `RESULT_INVALID` | 500 | 收集 | 否 | `result.json` 不符合附录 A |
| `CLEANUP_FAILED` | 500 | 清理 | 是 | 资源回收失败，不改变 Task 结果 |
| `CANCELED` | 409 | 全阶段 | 否 | 操作对象已取消 |
| `INTERNAL_ERROR` | 500 | 全阶段 | 视情况 | 未归类错误，必须记录 trace |

**规则：** 只有 `retryable: true` 的错误才允许进入 `retry_on` 白名单；新增错误码必须同步更新本表。

---

## 附录 E：决策记录摘要

完整决策记录（背景、替代方案、后果）由团队另行维护，本表为索引。

| ID | 决策 | 影响段落 |
|---|---|---|
| D-01 | 文档从 PRD 更名为 ARCHITECTURE，仅承载设计约束 | §0 |
| D-02 | **删除 Gate**：平台不判定指标是否达标，删除 `GATING` 阶段与 `GATE_FAILED` 终态 | §1、§7.4、§13.1 |
| D-03 | Task 粒度由提交者决定，多组合可合并为单 Task，平台不做强制拆分 | §4.2 |
| D-04 | 不承诺数据级复现，只保证执行逻辑可追溯；续跑不校验数据内容 | §5.7、§7.7 |
| D-05 | 必须提供 Web UI，与 API / CLI / MCP 并列为一等消费方 | §12 |
| D-06 | Task 状态机显式定义，`SKIPPED`（未执行）与 `FAILED`（执行失败）严格分离 | §7.1 |
| D-07 | 新增 Attempt 模型，重试不覆盖历史执行记录 | §7.2 |
| D-08 | Pre-Check 失败使用独立终态 `SERVICE_UNAVAILABLE`，不混入平台故障 | §5.11、§7.5 |
| D-09 | 失败默认隔离，`abort_run` 必须显式声明；Pre-Check 的声明由 `tester.yaml.on_failure` 承载，注册校验强制；`abort_run` 终止同 Run 内所有未完成 Task | §7.3 |
| D-10 | 资源路径对平台是不透明字符串，平台不解析、不校验、不托管 | §10.4 |
| D-11 | Executor 通过 Job Spec 解耦；**平台只有 K8s 一种 Executor** | §6.2、附录 B |
| D-12 | 错误码统一枚举，重试白名单只能引用枚举值 | 附录 D |
| D-13 | 状态必须落库，控制端无内存状态，重启后对账恢复 | §12.5 |
| D-14 | `url` / `model_name` / `model_path` 定义为**公共参数保留字**；取消 `endpoint.json` 与 `needs_endpoint`，Endpoint 由平台投影成参数后再校验；依赖 Endpoint 的 Tester 用 `url.required: true` 表达 | §5.3、§5.4.1、§9.1、附录 C |
| D-15 | Endpoint 收敛为平台内部对象：只在 Deploy 判定与参数投影两处出现，不落文件、不跨 Task 传递、Tester 不可见 | §3、§9.1 |
| D-16 | 产物**直接写入最终存储路径**：`<storage-root>/<run_id>/tasks/<task_id>/attempts/<n>/` 以 subPath 挂到 `/workspace/output`；"收集"退化为校验与补齐报告 | §6.8、§8.1、附录 B |
| D-17 | **Tester 必须能脱离平台独立运行**（一等约束）：输入只靠参数、元信息可缺省、目录可用环境变量覆盖、产物自足；`endpoint.json` 等平台专有输入全部取消 | §5.3、§5.10 |
| D-18 | SSH / Docker 主机上的运行是**平台外运行方式**，不是 Executor：不登记主机、不代管凭据、不进入平台生命周期 | §6.2、§6.4、§13.1 |
| D-19 | K8s 全节点可访问同一 NFS，产物直落最终路径；取消"打包上传"退化模式 | §6.8 |
| D-20 | **参数以 argv 传递**：调用方只给一段 `--k=v` 字符串，平台做词法切分后直接 exec，不经 shell；不再有 `params.json` / `metadata.json` / `/workspace/input` | §5.3、§6.8、附录 B |
| D-21 | **平台不渲染、不校验参数**：UI 只有一个输入框，`tester.yaml.parameters` 降级为文档语义；平台不设任何"参数校验/能力自检"字段（`requires` 已明确禁止） | §5.4、§5.4.1、§12、附录 C |
| D-22 | **任何运行环境都不 build**：镜像由 CI 产出并发布（形态不限，平台不校验）；`build/` 是 CI 的上下文，不是平台或 SSH 主机的动作 | §5.5、§6.2、§6.4、附录 C |
| D-23 | **Pre-Check 不是独立概念**：它就是一个 `role: pre_check` 的普通 Task，继承状态机、Attempt、产物、取消与独占；由 `precheck: auto` 让平台注入到 Task 链最前面，Run 状态机取消 `PRECHECK` 阶段 | §3、§5.11、§7.4、§10.1 |
| D-24 | **环境变量是 Run 粒度**：提交时填一次（`env` 明文 / `secrets` 引用），注入该 Run 全部 Task；Tester 只在 `tester.yaml.env` 声明读取哪些变量，不各自绑定；Secret 命名不做约定，因此不做"按 Endpoint 自动推荐凭据" | §10.1、§11.2、§12.4、附录 C |
| D-25 | **明文变量不强制入 Secret**：内部工具定位下 `env` 按明文处理（可回显、可编辑）；只有 `secrets` 里的 token 走 Secret Store | §12.4 |
| D-26 | **Deploy 是一等展示对象，日志按 Pod 采**：一个 Deploy 可能对应多个 Pod（多节点 / PD 分离）；日志在测试期间持续产生，必须按 Pod 分别落盘并在 Run 存续期间追加，不能等 Run 结束再取 | §8.1、§8.2、§9.1、§10.1、§11.2 |
| D-27 | **公共参数的取值点统一**：`deploy` 或 `service` 二选一（互斥），平台总是从这一处取 `url` / `model_name` / `model_path` 注入；取消"平台没值就由调用方写进 `args`"的分支，`args` 里出现公共参数一律拒绝 | §5.4.1、§9.1、§10.1、§10.3、§11.2 |
| D-28 | **调度可见性与看板**：调度归 Volcano，平台只读暴露队列 / 节点 / 等待原因；`QUEUED` 必须带 `waiting_reason`；看板为 P1 的固定一屏，不做趋势与告警 | §6.9、§11.2、§11.5 |
| D-29 | **日志持续采集并直接落盘**：平台侧 collector 流式写入共享存储；Pod 重启续写并插入 restart 标记，允许 gap 但不伪造连续；日志体积不影响 Task 结果 | §8.6、§9.1 |
| D-30 | **日志明文、平台不脱敏**：Tester 与 Deploy 日志按明文落盘，平台不读取 Secret 做替换；Tester 仍不得主动打印凭据 | §8.6、§12.4 |
| D-31 | **Deploy 排队是一等状态**：Genet 可能排队数小时到数天，平台表示 `queued`、展示已等待时长、允许取消，部署窗口默认 72 小时可配 | §7.4、§9.1 |
| D-32 | **Pre-Check 门控 + 其余并发**：Pre-Check 先于其他 Task；通过后其余 Task 按端点互斥规则并发，平台不额外施加串行顺序，是否并行由 Tester 的 `endpoint_exclusive` 声明决定 | §6.7、§7.4、§10.1 |
| D-33 | **`cleanup.policy` 与产物保留期解耦**：policy 只决定被测服务是否卸载；产物/日志保留由平台配置统一决定 | §8.6、§10.1 |
| D-34 | **状态 token 小写**：库 / API / 报告统一小写（与 §8.4 示例一致），文档与 UI 展示用大写 | §7.1、§8.4 |
| D-35 | **Pre-Check 不可重试失败归 `COMPLETED_WITH_ERRORS`**：只有"窗口内一直没就绪"这类可重试失败才记 `SERVICE_UNAVAILABLE` | §7.5 |
| D-36 | **可追溯性以"引用 + 实际 digest"为准**：执行层记录 K8s 实际拉取的镜像 digest；续跑复用优先比较 digest；引用只表示"要什么"，digest 表示"实际跑了什么" | §5.7、§7.7、§12.4 |
| D-37 | **不做提交幂等**：`POST /runs` / `POST /tasks` 每次都创建新资源，重复提交产生多个 Run/Task；平台不判断两次提交是否等价——硬件、驱动、HDK 版本等环境因素对平台透明，相同 Manifest 也可能不是同一测试条件。内部操作（Job 创建、Genet 部署）仍须可重试安全，但不属于接口幂等 | §9.2、§11.4、附录 D |
| D-38 | **Tester 只做镜像内发现，不做运行时注册**：注册表来自镜像内的 `tester.yaml`，启动扫描 + 周期重扫入库；不提供 `POST /testers` 之类的写入端点（与 K-3 一致）。「自发现注册」指的是平台发现，不是提交方上传 | §5.6、`IMPLEMENTATION.md §15` |
| D-39 | **对外错误体是扁平的**：`{code,message,detail,retryable,trace_id}`，不套 `{error:{…}}` 外壳；`code` 取附录 D 枚举，校验类错误用 `detail.issues[]` | `07-api.md §3` |
| D-40 | **提交 Manifest 的唯一形态是嵌套 `run:`**：`run:` 内承载 `name`/`deploy`/`service`/`precheck`/`env`/`secrets`/`tests`/`cleanup`；`runs.manifest` 原样保存该形态。`03-data-view.md §4.1` 原示例是平级写法，与 `07-api.md §4.1` 冲突；**v0.6 已把 §4.1 示例改为嵌套形态并注明唯一性**（文档已一致，不再是待修缺陷） | `07-api.md §4.1`、`03-data-view.md §4.1` |
| D-41 | **产物 PVC 名统一 `testplane-storage`**：`06-physical-view.md §2`（`testplane-storage-root`）与 `impl/execution.md §2.2`（`testplane-artifacts`）属文档不一致，以实现与清单为准回写 | §6.8、附录 B |
| D-42 | **开发/验收环境的种子由进程启动补齐**：`cluster-01` 与 `k8s-npu`（namespace `testplane`）在表为空时插入；生产仍走 Helm values 首次 seed。种子名沿用全文档示例（`k8s-npu`），不另起名字 | §6.11、`03-data-view.md §9` |
| D-43 | **MVP 执行环境 = 宿主 Kind + `kind load docker-image`**：不引入镜像仓库，镜像以本地 tag 灌入节点；Deploy 阶段由 mock 顶替 Genet（状态与字段真实，副作用为空），P1 换真实实现时接口不变 | §6.8、§7.4、`impl/execution.md §9` |
| D-44 | **Genet 的部署单元是 Helm release，没有"部署单"**：定位符 = `namespace`（由 API Key 绑定）+ `releaseName`（调用方自造）；拉起 `POST /api/open/helm/uc-packages/{stack\|pd}/releases`（仅 `releaseName` 必填，202 + `pending-install`；同名再 POST → 400），轮询 `GET /api/open/helm/releases/{name}`，卸载 `DELETE .../releases/{name}?keepHistory=&force=`。内部幂等键 `releaseName = "tp-" + sha256(run_id:deploy_id)[:12]`，且**必须 `noWait=true`**——`timeoutSec` 是 helm `--wait` 的 deadline，排队超过它就会被判 failed（排队可能长达数天） | `impl/execution.md §9`、§9.1 |
| D-45 | **`endpoint.status=ready` 只代表地址可解析，不等于可服务**：Genet 的就绪拆成两根轴（helm `status` 与 `callInfo.endpoint.status`），ready 既不保证 Pod Ready 也不保证能推理。因此 Deploy 进 `testing` 后仍必须由 Pre-Check 探活通过才放行其余 Task——平台是"地址门 + 服务门"两道门，不把 Genet 的 ready 当作测试开始的充分条件 | §7.4、§9.1、`impl/execution.md §9` |
| D-46 | **Deploy 阶段不承诺"排队第 N 位"**：排队发生在 Volcano 层，Genet 只给水位计数（无位次）。UI 展示 PodGroup `phase` / 原因 / 已等待时长；`runs.deploy.queue.position` 一律 `null`，不得用本地序号伪造成位次 | §7.4、`impl/control-plane.md §C-5.3`、`03-data-view.md §4.4` |
| D-47 | **Genet 的失败终态折叠为 `blocked`**：Genet 有 `failed/superseded/uninstalling/uninstalled` 等终态，而 Deploy 子状态只有 `queued/accepted/deploying/ready/blocked`（不新增 `failed`）。这些终态一律落 `blocked` + `Failure`，Run 转 `service_unavailable`（服务没起来，不是平台调不通），错误码按附录 D | §7.5、§9.1 |
| D-48 | **Genet 身份分两类，不把 Genet 当 IdP**：控制面/CLI 用 `Authorization: Bearer <Open API Key>` 打 `/api/open/*`（key 由管理员创建、服务端存 SHA256、绑定唯一 namespace）；Web 用户走团队统一身份源 + TestPlane 自建 OIDC client 与会话。Genet 无 client-credentials / PAT，其 OIDC Provider 默认关闭且只有硬编码 client；代理头 `X-Auth-Request-*` 可伪造，**禁止**作为身份来源 | `07-api.md §2.1`、`impl/control-plane.md §C-10` |
| D-49 | **Volcano 队列直接读 CRD，不经 Genet**：数据源是 `scheduling.volcano.sh/v1beta1` 的 `queues`（`.spec.capability`、`.status.state`、`.status.allocated`）与 `podgroups`（`.status.phase`、`.status.conditions[-1]`）；采样用**轮询 + 15s TTL + 单飞**（不用 informer，RBAC 只需 `get,list`，cluster-scoped 需 ClusterRole）。降级四态 `crd_missing/rbac_forbidden/scheduler_unavailable/disabled` 只影响 `volcano` 块——**不可达 ≠ 0 排队**。平台槽位与 Volcano 队列是两类排队，UI 必须分开 | §11、`07-api.md §5.3`、`06-physical-view.md §2` |
| D-50 | **Genet 部署的取消与卸载：先落意图、后置删除、对账补偿**：取消先写 `runs.deploy.cancel_requested_at` 再发 `DELETE`；release 尚未创建时 DELETE 只会 404，不能当作"取消成功"，必须由对账循环重试直到 `GET 404` 且 `pods.items` 为空（卸载校验），失败记 `uninstall.state=failed` + `CLEANUP_FAILED`，**不改 Run 终态** | §9.1、`impl/execution.md §9` |
| D-51 | **纯内网部署 + 与 Genet 共用同一个企业统一身份源（不同 client）**：TestPlane 是该 IdP 的独立 OIDC client（自己的 client_id / 回调 / scope），**不把 Genet 当 IdP、不共享 Genet 会话**；Web 用 authorization code + PKCE + `tp_session`/CSRF，服务间（Jenkins / Agent / CLI / MCP）用同一身份源签发的 JWT + JWKS 校验；本地开发仍可 `TESTPLANE_AUTH_MODE=dev`。这样不需要 Genet 提供 client-credentials，也不新增账号体系 | §12.1、`07-api.md §2.1`、`impl/control-plane.md §C-10` |
| D-52 | **Genet 的独占池（exclusive pool）语义是部署前提**：进入独占池的 namespace/服务不受每日 23:00 回收影响。TestPlane **独占自己的 namespace**（`testplane`）与被测服务的 namespace（如 `testplane-mock`），在 Genet 侧以「独占池 + 专用 API Key」登记；因此 §9.1 里"每日 GC 可能挂起长测"的风险对本部署不成立，但**必须**在部署文档里写明这条前提，否则换到共享池会踩坑 | §9.1、`06-physical-view.md §2`、`impl/execution.md §9` |
| D-53 | **Genet 将开放 `openAPI`**：控制面统一走 `/api/open/*`（Bearer Open API Key），不再依赖管理页/浏览器会话；Genet 侧 `openAPI.enabled` 的开关与 key 的 scope/命名空间绑定由对接 issue（`ucm-system/genet#1`）确认后再固化。在此之前本地联调可用假 Genet 或源码构建的实例 | `impl/execution.md §9` |
| D-54 | **Run 终态后的卸载：先落终态、再事务外卸载、失败只记不回溯**：`cleanupDeployIfTerminal` 在 Run 进入终态**之后**执行；是否卸载读 `cleanup_policy`（`never` → `uninstall.state=skipped`；`on_failure` → 仅在「存在非 succeeded 的 Task」时卸载；`always` → 总是）。卸载**只能**走 `DELETE /api/open/helm/releases/{name}`（直接 `helm uninstall` 会被 Genet admission webhook 拒绝）；判干净 = `GET 404` + pods 空；失败写 `uninstall.state=failed` + 一条 `cleanup.failed` 事件并由对账重试，**不改 Run 终态** | §9.1、`impl/control-plane.md §C-5.7`、`03-data-view.md §4.6`、`07-api.md §7` |
| D-55 | **`POST /admin/secrets/{name}/rotate` 是 `06 §4.4`（不授予 secrets 读写）的一条窄例外**：轮换需要确认目标 Secret 存在且该 key 非空，因此允许**对已登记 `secret_refs` 的这一条**做 `get`——RBAC 必须用 `resourceNames` 限定到具体 Secret（不是通配 `secrets get/list/watch`）；值**不进日志、不落库、不回显**，只记 `verified` + `checked_at`；无权限/不可达返回 `503 EXECUTOR_UNAVAILABLE`（可重试，`detail.reason ∈ forbidden|k8s_unavailable`），**不得降级成"看起来成功"**；`resourceNames` 的 Role/RoleBinding 由 `deploy/k8s/gen-secret-rotate-role.ps1` 按登记项生成（`deploy/README.md §6.1`），**不得**把 `secrets get` 并进通配 RBAC。审计 action = `secret_rotated`（含失败的 `rejected`） | `06-physical-view.md §4.4`、`07-api.md §5.4`、`03-data-view.md §4.6.1` |
| D-56 | **产物与日志的对外取用**：P0 的**唯一真源是 PVC 上的文件系统**（`<storage-root>/<run>/…`），平台不引入对象存储；对外提供两类入口：① 细粒度端点（`/artifacts`、`/files?path=`、`/report`、`/report.md`、`/events/history`、`/deploy/logs`、`/tasks/{id}/attempts/{n}/logs`）；② **一次性打包** `GET /runs/{id}/bundle`（`application/zip`，含 `report.json` / `report.md` / `metadata.json` / `events.jsonl` / `artifacts/index.json` + 产物 / `logs/deploy/<pod>.log` / `logs/tasks/...`，缺失项写 `MANIFEST.json.missing`，超限项记 `truncated`）。打包是**流的**，不在内存里攒整包。**S3 兼容存储（MinIO/rustFS 等）列为 P1 可选镜像**：只做"把 PVC 内容镜像出去"的旁路能力，不改任何端点契约、也不做 P0 依赖 | §8.2、`07-api.md §5.1`、`impl/web-and-delivery.md §12` |
| D-57 | **提交下限**：`POST /runs` 的 `tests` **至少 1 条**（与 `precheck` 无关）。Pre-Check 是平台注入的 `seq=0` Task，不计入用户测试；0 条 → 400 `VALIDATION_ERROR`（`path="tests"`），不落库。理由：否则会产出"成功地什么都没测"的 Run（绿色成功 + 报告"全部 1 个 Task 通过"），误导性极强 | `07-api.md §4.1`、`impl/control-plane.md` |
| D-58 | **`run.name` 上限 200 字符**（当前 DB 是无限长 text、服务端也没校验，220 字符能存进去）。超长名称会撑坏列表/面包屑/工单标题；校验在服务端（`VALIDATION_ERROR` + `path="run.name"`）与前端（`maxlength` + 实时计数）各做一遍 | `07-api.md §4.1`、`openapi.yaml` `RunSubmit.name.maxLength` |
| D-59 | **Pod 日志全部落盘，落盘副本是唯一真源**：Tester（attempt）与 **Deploy 的每个 Pod、每个容器**都落盘（`tasks/<t>/attempts/<n>/logs/stdout.log`、`deploy/logs/<pod>[.<container>].log`），**含容器重启前的 `previous` 日志**（重启写标记行并续写同一文件，`execution.md §7.2`）；`deploy/pods.json` 每 30s 原子写（Pod/容器/重启次数/日志文件与区间）。`GET /runs/{id}/deploy/logs`、`GET /runs/{id}/artifacts`、`GET /runs/{id}/bundle` 一律**优先读盘**；只有盘上缺失时才降级实时读 `pods/log`，且必须在响应里显式标注"这是实时窗口、Pod 回收后不可得"。理由：Pod 被 GC 或重启后实时读就永久丢失证据，而"事后复盘一次失败的部署"正是平台的核心价值 | `impl/execution.md §7`、`03-data-view.md §4.4`、`07-api.md §5.1` |
|| D-60 | **Tester 目录的"同名多版本"与挂载声明口径**（票 #60）：① `GET /testers` **默认每个 name 只回最新版本**，`include_retired=true` 时回全部版本（`orphaned=true` 保留为旧别名）；条目新增 `latest`（该 name 的最高版本，比较规则 = `domain.VersionLess`，全仓唯一真源）与 `retired`（目录里已不存在、只剩库里的历史版本 = `testers.orphaned`）。② **历史版本永不删除**：`testers` 行不删，`orphaned` 只标记；历史 Run/Task 继续按 `name@version` 解析，报告与 Task 详情里的 `tester.version` 保持提交时冻结的值（可追溯性，§5.7）。③ `tester.yaml.mounts` 的字段以**实现为准**收敛为 `name / mount_path / read_only / source`（`source` = 承载该挂载点的 PVC 名，缺省用平台 storage PVC），**没有 `required`**；`MOUNT_UNAVAILABLE` 是**运行期**观测（Pod `FailedMount`/`FailedAttachVolume`），不是提交期校验——提交期无法可靠判断 Profile/StorageClass 能否供给。 | `07-api.md §5.1`、`ARCHITECTURE §5.6/§10.4`、`openapi.yaml` `Tester` |
