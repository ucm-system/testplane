# Argus 架构与设计约束

> 云原生、轻量化、AI Agent 友好的 LLM 服务评测流水线平台
>
> **文档类型：** 架构设计约束（Design Constraints）
> **文档效力：** 本文是 Argus 的**设计裁决**集合。凡与本文冲突的实现方案，以本文为准；需要变更时，按 §15 追加裁决条目并记录变更，不做静默修改。
> **适用读者：** 平台开发者、Runner 维护者、Jenkins / 流水线对接方、Agent 集成方。
> **更名说明：** 平台原名 TestPlane，现更名 **Argus**；术语全面对调（Run→Task、Task→runner、Tester→Runner），映射表见 §0.2。附录 E 中 D-01～D-65 为历史决策记录，保留 v1 术语，其中已被 v2 取代的条目以「（v2 已取代，见 D-66 系列）」标注。

---

## 0. 文档说明

### 0.1 本文回答什么

1. 系统边界在哪里：哪些职责属于 Argus，哪些属于外部系统。
2. 模块之间的契约是什么：平台控制端、Runner、Executor 之间靠什么交互。
3. 哪些事情平台**不做**：以 Non-Goals 形式显式排除，避免实现蔓延。

### 0.2 术语映射（v1 → v2）

| v1（TestPlane） | v2（Argus） | 说明 |
|---|---|---|
| Run | **Task** | 一次评测任务；重试以 Task 为单位 |
| Task（Run 内的测试任务） | **runner**（实例） | Task 内严格串行的一个执行步骤 |
| Attempt（Task 级重试执行） | **attempt**（Task 级计数） | 重试 = attempt+1，整 Task 全量重跑含重新部署 |
| Tester / `tester.yaml` / `testers/` | **Runner / `runner.yaml` / `runners/`** | 可注册的定义单元 |
| `TESTPLANE_*` 环境变量 | `AG_*` | `AG_SERVED_URL` / `AG_PARAMS_FILE` / `AG_OUTPUT_DIR` 等 |
| `tpctl` / `testplaned` | `argus` / `argusd` | CLI / 控制面二进制 |
| `on_failure: abort_run` | `on_failure: abort_task` | |
| 平台 Deploy 阶段 | **service-deploy runner**（role: deploy，内置） | 调 Genet 拉起服务 |
| 平台 Cleanup 阶段 | **service-clean runner**（role: clean，内置） | finally 语义，调 Genet 拆除 |
| argv 业务参数（D-20） | **输入文件 + `AG_PARAMS_FILE`** | 平台词法校验后落盘传入 |
| 镜像 namespace `testplane` / PVC `testplane-storage` | `argus` / `argus-storage` | |

### 0.3 本文不回答什么

- 业务背景、价值论证、用户画像、成功度量 —— 属于需求文档范畴。
- 版本排期、人力投入、里程碑。
- 具体页面交互稿、视觉规范。
- AISBench / EvalScope 等测试工具的内部实现。

> 本文曾以 `PRD.md` 命名，现已停用。需求类文档另行维护，不以设计文档代替需求基线。

### 0.4 约束强度的用词约定

| 用词 | 含义 |
|---|---|
| 必须 / 不得 | 硬约束，实现不可偏离；偏离即为设计缺陷 |
| 应该 / 推荐 | 默认方案；偏离需在实现说明中写明理由 |
| 可以 | 可选能力，不影响其他模块 |

### 0.5 前置裁决

Argus 是**对内工具**，以下事项已由团队裁决，本文按结论描述，不再重新论证：

- 服务部署统一由 **Genet** 承担，Argus 不做部署平台；平台与被测 Runner 都不直连集群操作部署资源（§9）。
- 部署与拆除以**内置 Runner**（service-deploy / service-clean）承载，与 test / pre_check 共用同一套执行契约（§4.4、§9）。
- 测试能力统一以 **Runner** 封装，平台不实现测试算法。
- **Task 内 runner 严格串行（线性管道）**，不做 DAG 与并行分支（§7.1）。
- **runner 级不做平台重试**；重试 = 整 Task 新 attempt，全量重跑含重新部署（§7.8）。
- **不引入 Gate / 门禁判定**：平台只产出事实数据，达标与否由下游或 Agent 判断（详见 §14）。
- 需要 **Web UI**：人也要用，UI 与 API / CLI / MCP 并列为一等消费方（详见 §11）。
- **不承诺数据级复现**：平台保证执行逻辑可追溯，不保证历史 Task 的测试结果可重放（详见 §5.7）。

### 0.6 变更记录

| 版本 | 日期 | 变更摘要 |
|---|---|---|
| 0.1–0.11 | 2026-11-03 ~ 11-05 | v1 全部历史变更见附录 E（D-01～D-65），术语为 v1（Run/Task/Tester） |
| **2.0** | 2026-11-10 | **Argus 更名与 Runner 化改造**：① 平台更名 Argus，术语对调（§0.2）；② 业务输入从 argv 改为输入文件（`AG_PARAMS_FILE`），entry 由 yaml 声明，推翻 D-20/D-21 的 argv-only 契约；③ 平台对 jsonl 输入做结构级校验（类型 + required）；④ 新增 `input_format: jsonl/yaml/text`；⑤ deploy/clean 以内置 runner 承载，取消平台 Deploy/Cleanup 阶段与 `cleanup.policy`，clean 为 finally 语义；⑥ **runner 级重试取消**，attempt 上移为 Task 级计数，重试 = 全量重跑含 fresh deploy；⑦ Task 内 runner **严格串行**，取代 D-32 的门控+并发模型；⑧ 新增 exports 机制（§5.12）与 `exports`/`consumes` 组合期校验；⑨ `endpoint_exclusive` 收窄为仅外接模式有效；⑩ 契约附录 A/B/C 升 schema 2.0，API 路径 `/runs→/tasks`、`/testers→/runners`；⑪ **产物契约改为三选一**（`result.json`/`result.html`/`result.csv` 至少其一，不强求 `result.json`；D-74），`RESULT_MISSING` 语义重定义为"三选一全部缺失" |

---

## 1. 核心定位与设计原则

**一句话：** 给定服务部署参数或已有服务端点，按线性管道执行部署、探活与标准化评测，产出机器可读、人可读的测试结果。

**核心职责：**

1. 需要部署时，由内置 service-deploy runner 调用 Genet 拉起被测 LLM 服务并导出服务信息。
2. 通过 pre_check runner 确认服务可用后，才允许开始正式测试。
3. 根据提交配置按序执行多个 runner 实例（严格串行）。
4. 通过 Executor 将各 runner 运行在指定执行环境中。
5. 收割 runner 导出（exports）、收集测试结果和原始产物，生成统一报告。
6. 由内置 service-clean runner 完成部署拆除（finally 语义），平台记录事实。

**核心设计原则：**

| 原则 | 约束 |
|---|---|
| API First | 所有核心能力通过 REST API 暴露。Web UI、CLI、MCP 都是 API 的消费方，**不得**存在仅某一端可用的功能 |
| Runner First | 测试能力以 Runner 为基本单位，平台不实现具体测试逻辑 |
| Single Entry Point | 每个 Runner 只有一个执行入口，由 `runner.yaml.entry` 声明（exec form，不固定文件名） |
| Executor Agnostic | Runner 不感知运行环境，同一个 Runner 可被不同 Executor 执行 |
| Runner Atomicity | runner 是最小的调度、状态和报告单元；**Task 是最小的重试与编排单元** |
| Linear Pipeline | Task 内 runner 严格串行；隔离由拓扑（Task 私有服务 + 串行）保证，不靠并发声明 |
| Contract Driven | 每一个跨边界交互都必须有可校验的 JSON 契约，见附录 A–D |
| AI Friendly | Runner 参数、状态、错误、报告均具备机器可读性 |
| Human Readable | 报告必须人可读；UI 必须覆盖主流程，但对 API 没有独占逻辑 |
| No Hidden State | 状态必须落库，控制端进程重启后可恢复，不允许仅存在于内存 |
| Fail Fast on Pre-Check | 服务不可用时尽早阻断，不浪费正式测试资源 |
| Explicit Failure Propagation | runner 失败默认按 `on_failure` 传播；`abort_task` 必须显式声明 |

---

## 2. 系统边界与分层

Argus 由五个职责层构成：

| 层次 | 核心职责 | 代表模块 |
|---|---|---|
| Presentation | 人机与机器入口 | Web UI、CLI、MCP Server |
| Orchestration | 组织评测生命周期 | Task、runner 管道、exports 收割 |
| Capability | 定义测试能力 | Runner、Registry |
| Execution | 提供执行环境 | K8s Executor（平台唯一的执行后端） |
| Integration | 对接外部系统 | Genet（经内置 deploy/clean runner）、Artifact Storage、Identity |

系统关系：

```text
                    Presentation
              Web UI / CLI / MCP
                       │
                  (REST API)
                       │
   ┌───────────────────┴────────────────────┐
   │             Argus 控制端                │
   │                                        │
   │   Orchestration            Capability  │
   │   Task ─ runners(串行)      Runner      │
   │    │      │  exports收割     Registry   │
   │    │      └──────► Executor ◄─────────┘
   │    │                   │
   │    └─ 内置 deploy/clean runner ──► Genet ──► 被测服务
   └────────────────────────┬───────────────┘
                            │  Job Spec
                            ▼
                     Kubernetes Job（每 runner 一个）
                            │  挂载：/workspace/input（只读）+ /workspace/output（读写）
                            ▼
                   runner entry（runner.yaml.entry）
                            │  产物直接写入 /workspace/output
                            │  = <storage-root>/<task_id>/attempts/<n>/runners/<seq>/
                            ▼
                       Report（JSON / MD）
```

**职责隔离：**

- Genet 负责服务部署与拆除，Argus 不重复实现 Helm 和服务部署逻辑；与 Genet 的交互由内置 service-deploy / service-clean runner 执行，平台只读取它们产出的状态与导出。
- Runner 负责测试逻辑，Argus 不实现 AISBench、EvalScope 等工具的内部功能。
- Executor 负责运行环境，Runner 不负责创建 K8s Job；反过来 Runner 也不知道自己是不是跑在 K8s 里（§5.10）。
- UI 负责展示与操作，不实现业务逻辑，也不允许持有 API 之外的私有状态。
- Argus 负责生命周期和结果归一化，**不负责判断指标是否达标**，也不替代下游报告、存储和数据分析系统。

---

## 3. 核心概念

| 概念 | 定义 | 核心职责 |
|---|---|---|
| Runner | 一种标准化能力定义 | 描述参数、运行环境和唯一执行入口；`role: pre_check / test / deploy / clean` |
| Runner Registry | Runner 注册表 | 发现、校验、管理和暴露 Runner |
| Executor | 平台唯一的执行后端 | 用 K8s Job 运行 Runner 容器 |
| Execution Profile | 执行环境配置 | 引用已登记的 K8s 集群 / 命名空间与挂载 |
| runner（实例） | Task 内的一次执行步骤 | 最小调度、状态和报告单元；严格串行 |
| attempt | Task 的第 N 次执行 | 承载该次全量重跑中所有 runner 的执行记录、日志、产物 |
| Task | 一次完整评测任务 | 管理部署、runner 管道、汇总、重试与清理 |
| Endpoint | 平台侧服务描述 | 由 deploy 导出或用户静态提供；只用于编排判定与投影成公共参数，**Runner 不可见** |
| Artifact | 测试产物 | 存储标准结果、日志和原始测试文件 |
| on_failure | 失败传播策略 | 决定一个 runner 失败后，后续 runner 继续还是终止（clean 永远执行） |

**层级关系：** `Task → attempt → runner`。Task 是稳定标识与重试单位；attempt 是 Task 的第 N 次全量执行；runner 在每次 attempt 内按序执行一次，没有自己的重试。runner 执行记录 = `(task_id, attempt_no, seq)` 三元组定位。

**pre_check / deploy / clean 不是独立概念**，它们就是 runner——只不过内置 Runner 声明了对应 role，并因此要求 `on_failure: abort_task`（§7.3）。deploy 与 clean 由平台按提交配置**自动配对注入**（提交了部署描述 → 前置 service-deploy、追加 service-clean；§10.1），用户不手动添加。pre_check 仍由 `precheck: auto` 自动注入为第一个测试类 runner（§10.1）。

---

## 4. runner 模型

### 4.1 runner 定义

一个 runner 实例 = 一个 Runner 定义 + 一段确定的输入 + 一个执行环境 + 一次执行（在某个 attempt 内）。

runner 必须具备：

- 独立执行、独立状态、独立日志、独立产物。
- 每次执行产出一个退出码；test / pre_check 角色产出**三选一**产物之一（`result.json` / `result.html` / `result.csv`，§5.3）。
- 成功时可产出 `exports.env`（§5.12），由平台收割注入下游。

不引入 Case 概念，不要求平台理解具体 Runner 内部的数据组织。

### 4.2 粒度裁决

> **裁决：** runner 粒度由**提交者**决定，平台不做强制拆分，也不对"一个 runner 内包含多少组测试参数"做任何假设。

具体规则：

1. **允许合并**：性能测试的输入长度 × 输出长度 × 并发组合，可以作为**一个** `performance-sweep` runner 提交，内部展开方式（串行、并行、分批）完全由 Runner 决定。
2. **允许拆分**：若提交者需要单独失败定位、单独报告，可以显式写成多个 runner（串行执行）。
3. **平台只保证边界事实**：一个 runner = 一次容器执行 + 一个退出码 + 三选一产物之一（test / pre_check）+ 一份日志。平台不感知 runner 内部的循环与分批。
4. **Runner 的补偿义务**：当 runner 内包含多组参数时，Runner **必须**在 `metrics[]` 中通过 `labels` 区分每一组结果（如 `{"input_len":512,"concurrency":8}`），否则报告与 UI 无法正确分组展示。

拆分的判断依据（指导，非规则）：

| 需求 | 建议 |
|---|---|
| 需要单独定位某一组参数的失败 | 拆成独立 runner |
| 各组参数需要独立的资源规格或执行环境 | 拆成独立 runner |
| 组合之间共享加载成本（模型、数据集预热） | 合并为一个 runner，内部展开 |
| 组合只是同一工具的参数扫描 | 合并为一个 runner，用 `labels` 区分 |

### 4.3 示例

```yaml
# 一个 runner 内展开 24 种组合：合法形态
runners:
  - name: performance-sweep
    runner: aisbench-perf
    version: "1.0.0"
    execution:
      profile: k8s-npu
    input: |
      {"input":512,"output":128,"concurrency":1}
      {"input":1024,"output":128,"concurrency":4}
      ...（共 24 行，行序即执行序）
```

```yaml
# 拆分形态：每个组合一个 runner，串行执行
runners:
  - name: perf-512-128-c1
    runner: aisbench-perf
    version: "1.0.0"
    execution: { profile: k8s-npu }
    input: |
      {"input":512,"output":128,"concurrency":1}
```

两种写法都被支持，平台对二者一视同仁：**只创建提交者写明的 runner 数量**。

---

## 5. Runner 契约

这是 Argus 最重要的设计部分。Runner 是平台唯一感知"这一步怎么做"的边界。

### 5.1 Runner 的定位

Runner 是独立、可执行、可复用的定义单元。例如：

- `endpoint-probe`（系统内置，pre_check 角色）
- `service-deploy`（系统内置，deploy 角色）
- `service-clean`（系统内置，clean 角色）
- `aisbench-acc`、`aisbench-perf`、`aisbench-prefix`、`aisbench-sla`
- `evalscope-acc`

role 四种取值与注册边界：

| role | 职责 | 注册 |
|---|---|---|
| `pre_check` | 前置门：确认服务可用，失败必须终止 Task | 可注册 |
| `test` | 正式测试：产出指标与报告 | 可注册 |
| `deploy` | 部署被测服务：消费部署 YAML，调 Genet，导出服务信息 | **仅内置**，用户注册拒绝 |
| `clean` | 拆除部署：消费 deploy 导出，幂等拆除 | **仅内置**，用户注册拒绝 |

Runner 可以封装现有测试工具，也可以封装自定义脚本。Runner 自己决定：提供哪些业务参数及如何解释；使用什么工具、如何拼装命令；如何处理工具原始输出并归一化；内部按什么顺序、什么并发度执行多组参数。平台不关心 Runner 内部使用了什么工具、执行了多少步骤。

### 5.2 唯一执行入口：entry 由 yaml 声明

**每个 Runner 必须提供且只提供一个执行入口**，由 `runner.yaml.entry` 以 **exec form 数组**声明（如 `["/runner/run.sh"]`），文件名不限。平台执行 `exec <entry[0]> <entry[1]> ...`，**不经 shell、不追加任何动态 argv**（§5.3）。

平台不得：

- 解析 Runner 内部的命令行参数。
- 根据参数名称拼装 AISBench 或 EvalScope 命令。
- 了解不同测试工具的 CLI 规则。
- 为不同 Runner 编写特殊的命令执行逻辑。
- 根据测试工具的内部产物结构编写专属解析逻辑。
- 依赖镜像的 `ENTRYPOINT` / `CMD`（yaml 的 `entry` 是唯一权威）。

Runner 必须自行完成从平台输入到具体命令的映射。**平台只做词法校验、落盘与透传：不解析参数语义，不拼装工具命令。**

### 5.3 调用契约：输入文件 + env + 输出目录

契约面只有三样东西，没有第四样：

```text
/workspace/input/params.jsonl（或 params.yaml，按 input_format；只读）
   ← 平台把提交的输入原文写入，并注入 AG_PARAMS_FILE 告知路径
AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH / AG_TASK_ID / AG_RUNNER_SEQ / AG_ATTEMPT
   ← 平台注入的环境变量（公共参数 + 元信息）
/workspace/output/（可写，= AG_OUTPUT_DIR）
   ← 唯一目录契约：产物三选一（test/pre_check，result.json/html/csv 至少其一）、exports.env（平台收割）、raw/、logs/
```

| 通道 | 内容 | 说明 |
|---|---|---|
| **输入文件** | 全部业务输入 | 平台做**词法/结构校验**后原样落盘（§5.4）；entry 按 `--params-file` > `AG_PARAMS_FILE` > 约定路径的优先级读取 |
| **环境变量** | `AG_TASK_ID` / `AG_RUNNER_SEQ` / `AG_ATTEMPT`（元信息）；`AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH`（公共参数）；`AG_DEPLOY_REF`（deploy 导出，仅 clean 消费） | 公共参数走环境变量，不再走 argv |
| **输出目录** | `/workspace/output/`，可写 | 产物**三选一**（`result.json` / `result.html` / `result.csv` 至少其一，test/pre_check）、`exports.env`、`raw/`、`logs/` |

约束：

- **业务输入不经过 argv**：平台执行 entry 时 argv 为空（仅 entry 数组本身）。这取代 v1 的 D-20：`/workspace/input` 恢复存在，成为唯一业务输入通道；参数文件随 attempt 落盘，可审计可复现。
- entry 读取输入的优先级（契约）：`--params-file <path>` > 环境变量 `AG_PARAMS_FILE` > 约定路径 `/workspace/input/params.jsonl`（yaml 格式为 `params.yaml`）> 三通道皆无。不支持 stdin。**三通道皆无时**：`parameters` 含 `required: true` 字段（或无 `parameters` 声明但需要输入）的 Runner 必须报错退出（退出码 2）；无 required 字段的 Runner 可按空输入处理（全部取默认值）——这保证"只传 `--url` 跑一个全可选 Runner"的独立运行形态可用（§5.10）。
- **产物三选一**（test / pre_check）：`result.json` / `result.html` / `result.csv` **至少产出其一**，由 Runner 决定——**不强求 `result.json`**。产出 `result.json` 时必须是合法 JSON（否则 `RESULT_INVALID`）；三选一全部缺失视为 runner 失败（`RESULT_MISSING`）。这条契约**全局唯一**，Runner 目录不需要再声明产物清单——`raw/` 与 `logs/` 是自由区，平台只做搬运与索引。
- Runner 不得依赖容器内其他路径持久化产物；`/workspace` 之外一律视为只读。
- 数据集与模型等外部资源**不由平台托管**：路径是不透明字符串，不解析、不校验存在性、不做映射（§10.4）。
- **Runner 必须能独立运行**：只给输入文件 + 环境变量就能跑出与平台内一致的产物（§5.10）。
- 凭据只走环境变量（§12.4），不参数化、不落盘。
- 平台**不得**向 Runner 传第四种通道（元数据文件、平台 API 地址等）。`exports.env` 是 Runner 写、平台读的输出，不是输入通道。

### 5.4 输入校验（词法 + 结构）

> **裁决（v2，取代 D-21 的"不校验"）：** 平台对 `jsonl` 输入做**语法级 + 结构级**校验：逐行 `JSON.parse`、校验"是对象"、按 `runner.yaml.parameters` 校验**字段类型**与 **`required: true` 字段存在性**。枚举值、范围、正则、跨字段约束仍归 Runner。`yaml` / `text` 输入只做语法级校验（可解析），语义完全归 Runner。

理由：输入形态从 argv 字符串变为结构化 JSONL，校验左移到提交期可以把"跑完才发现参数错"提前为"提交被拒"，报错带行号；而跨字段语义校验平台依然不做，守住"不解析业务语义"的边界。

- 校验失败直接拒绝提交（`VALIDATION_ERROR`），错误带**行号 + 字段名 + 原因**。
- `parameters` 中未声明的 JSONL 字段：平台**放行**；是否允许由 Runner 自行决定。
- 无业务参数时 `parameters: []`（不得省略），平台只做"是对象"校验。
- `input_format: yaml / text` 时 `parameters` 必须为 `[]`。

`runner.yaml.parameters` 的完整 schema 见附录 C。`placeholder` 必须与 `input_format` 同格式（JSONL 片段 / YAML 片段），UI 用作输入框占位。

#### 5.4.1 公共参数（保留字）

三个参数在几乎所有 test / pre_check Runner 上语义相同，**必须**使用固定名称与固定语义，以**环境变量**注入：

| 环境变量 | 语义 |
|---|---|
| `AG_SERVED_URL` | 被测服务的 **OpenAI 兼容 base URL**（`scheme://host:port/v1`）。Runner 从它推导 `/chat/completions`、`/models`；不带 `/v1` 由 Runner 自动补，末尾斜杠忽略 |
| `AG_SERVED_MODEL_NAME` | 服务端模型名，即 OpenAI 兼容接口 `model` 字段的取值 |
| `AG_MODEL_PATH` | 容器内模型权重路径；可达性由挂载决定，平台不改写（§10.4） |

规则：

1. **名称固定**：禁止 `AG_BASE_URL`、`AG_SERVICE_URL` 等变体。Agent 靠这三个固定名字做通用拼装。
2. **来源只有一处，取值点不随场景变（deploy XOR service）**：

   | 场景 | 三个变量从哪来 | 平台行为 |
   |---|---|---|
   | Task 里有 `deploy`（部署模式） | service-deploy runner 的 exports | 注入后续 runner |
   | 外接模式（不部署） | Manifest 的 `service` 块，提交时静态填写 | 作为"序号 0 的导出"注入全部 runner |
   | 脱离平台独立运行 | 调用方自己传 | 平台不存在 |

   Manifest **必须**提供 `deploy` 或 `service` 之一，两者互斥；都填或都不填一律拒绝提交（`VALIDATION_ERROR`）。部署描述与静态 service 同时出现的分支不存在。

3. **Runner 不声明对 Endpoint 的依赖**：它只认 `AG_SERVED_URL`，不关心这个值来自 deploy 导出、用户直填，还是压根拿不到。把"我需要 Endpoint"写进 `runner.yaml` 等于让 Runner 感知平台概念，禁止（依赖表达用 `consumes`，§5.12）。
4. **值对平台不透明**：不解析、不校验存在性、不做路径映射、不校验可达性。
5. **必须容忍**：Runner 即使不使用某个公共变量，也必须接受并忽略它（平台可能无条件下发）。把 `AG_MODEL_PATH` 当未知变量报错不违规（env 本就可忽略），但**不得**因它的存在改变行为。
6. 凭据不进这三个变量（§12.4）。

### 5.5 Runner 目录结构

采用项目内子目录自动发现机制。一个 Runner 就是一个自包含目录：

```text
runners/
├── endpoint-probe/
│   ├── runner.yaml          # 唯一权威：元数据 + entry + input_format + 参数 schema + 资源/挂载
│   ├── run.sh               # 本示例声明的 entry（文件名由 yaml 决定，不限）
│   ├── build/
│   │   ├── Dockerfile       # 构建定义；构建方式写在注释或 README 里
│   │   └── scripts/         # 构建期脚本（可选，形态不限）
│   ├── run-local.sh         # 本地便捷脚本：docker run（不 build）
│   └── README.md
├── aisbench-perf/
├── aisbench-prefix/
└── evalscope-acc/
```

| 路径 | 要求 | 职责 |
|---|---|---|
| `runner.yaml` | 必须 | 元数据、角色、`entry`、`input_format`、镜像引用、资源、参数 schema、exports/consumes、并发声明、受限 K8s 需求 |
| entry（文件） | 必须 | 由 `runner.yaml.entry` 指向；必须具备可执行权限 |
| `build/Dockerfile` | 推荐 | 定义运行环境。构建上下文是 Runner 根目录，构建命令写成 `docker build -f build/Dockerfile .`——写在 Dockerfile 注释或 README 里即可，平台不读 |
| `build/scripts/` | 可选 | 构建期脚本；形态与命名不限。运行期逻辑一律进 entry 及其调用的文件 |
| `run-local.sh` | 可选 | 本地/独立运行的便捷封装：只 `docker run`，**不 build**。不是契约入口，平台不读它 |
| `README.md` | 推荐 | 使用说明、输入示例、独立运行方法 |
| `tests/` | 可选 | 维护者自测，不参与注册与调度 |

**明确不在 Runner 目录里的东西：**

| 不要放 | 原因 |
|---|---|
| `deployment.yaml` / `job.yaml` / 任何完整 K8s 清单 | 部署由**内置 service-deploy + Genet** 负责（§9）。特殊运行时需求改用 `runner.yaml.k8s` 的**受限声明字段**（附录 C） |
| `results/` / `output/` 等结果目录 | 容器内固定写 `/workspace/output`（§5.3）。仓库里不放测试产物；本地调试用 `AG_OUTPUT_DIR` 指到被 gitignore 的目录 |
| 凭据、`.env`、私钥 | 一律走环境变量注入（§12.4） |

**注册扫描规则：** 只认 Runner 根目录下的 `runner.yaml`；entry 文件的存在性由 CI 保证（§5.6）。`build/`、`tests/`、`.local/` 等子目录不参与扫描与校验。

**构建不做规范**：平台不规定 Runner 用什么构建脚本。唯一要求是构建方式能被读懂，镜像由 CI 产出并发布到 `registry.dev.huawei.com/argus/...`（§6.2）。

`runner.yaml` 的字段定义见附录 C。

### 5.6 注册机制

Runner 采用平台维护者管理的**白名单注册**机制。注册流程：

1. 扫描 `runners/*/` 的**根目录**（只认 `runner.yaml`；`build/`、`tests/` 等子目录不递归）。
2. 读取并校验 `runner.yaml`：字段完整性、Schema 合法性、`entry` 非空且为 exec form 数组、`input_format` 合法、`parameters` / `exports` / `consumes` 结构合法。
3. 校验 entry 文件存在且具备执行权限（CI 亦应自检 `docker run --rm --entrypoint test <image> -x <entry[0]>`；平台不做镜像内检查）。
4. 校验镜像引用非空（平台**不限制形态**：tag、digest 都行）。
5. 检查名称与版本冲突。
6. 校验角色与失败扩散声明：`on_failure` 必须是 `continue` / `abort_task` 之一；`role: pre_check` 与内置 `deploy` / `clean` 必须是 `abort_task`，否则拒绝注册；**用户目录声明 `role: deploy` / `role: clean` 一律拒绝**（§5.1）。
7. 校验通过后写入 Runner Registry，对外暴露元数据（`entry` / `input_format` / `parameters` / `exports` / `consumes`，供 Agent / 前端 / 人类阅读；平台不据此渲染命令）。

用户和 Agent **只能使用已注册的 Runner，不能自行上传执行代码**。

> **镜像引用不作限制**：平台把 `runner.yaml.image` 原样交给 K8s，不解析 tag / digest，也不强制钉版本。

#### 5.6.1 版本保留与退役（票 #60 / D-60）

同一 `name` 可以**多个版本并存**，这是正常的版本演进，不是重复入库：

| 情形 | `runners` 表 | 目录里 | `GET /runners` 默认 | `retired` |
|---|---|---|---|---|
| 新版本发布（旧版本仍在目录） | 两行都在 | `0.1.0`、`0.2.0` | 只回 `0.2.0`（`latest=true`） | 都是 `false` |
| 新版本发布后旧版本目录被删 | 旧行保留，标记 `orphaned` | 只有 `0.2.0` | 只回 `0.2.0` | 旧行 `true` |
| 整个 Runner 下架 | 所有行 `orphaned` | 无 | 默认不出现（`include_retired=true` 才可见） | 都是 `true` |

- **不删除旧行**：历史 runner 执行记录用 `name@version` 引用，删行会让历史报告永久失去 Runner 元数据（§5.7 可追溯性）。
- `latest` 的值是"该 name 的最高版本"（含 `retired` 行），比较规则见 `domain.VersionLess`（注册表扫描排序、API、`argus` CLI 共用同一份）。
- `orphaned` 与 `retired` 同义（前者是 `runners` 列名，后者是对外字段名），两个字段都保留以兼容既有前端/CLI。
- **`retired` 的口径是"该**版本**已不在目录里"**（不是"整个 Runner 下架"）：只有旧版本从目录消失 → 旧版本 `retired=true`；整个目录消失是极端情形（该 name 所有版本 `retired=true`）。
- 判定发生在启动期/重扫期的目录比对（`registry.Scan`）；标记**落库**（`registry.Store.MarkRunnerOrphaned`），进程重启后 `retired` 不会丢。

### 5.7 版本与可追溯性

> **裁决：** 平台保证**执行逻辑可追溯**，不保证**结果可复现**。

具体含义：

- 已发布的 `name + version` 组合**不得**被静默覆盖；重新发布必须提升版本号（错误码 `RUNNER_VERSION_PINNED`）。
- runner 执行记录冻结：Runner 名称、版本、镜像引用、**输入快照（提交的输入文件原文）**。
- 执行层把 K8s 实际拉取的镜像 `imageID` 解析为 **digest** 写入执行记录（可得时）：引用是"要什么"，digest 是"实际跑了什么"。
- 导出（exports）随 attempt **落库落盘**：事后可完整还原该 attempt 的每次注入（§5.12）。这是 v2 新增的可追溯面：deploy 动态产出的服务信息不再是黑盒。
- 历史 Task 可以确认"当时用的是哪个版本的哪个 Runner、传了什么输入、注入了什么导出"，但**不保证**重跑得到相同数字。
- 数据集与模型内容的变化由 Runner 与外部资源负责；平台不做内容指纹。可比性判断交给下游与 Agent。

**提交不做幂等**（D-37）：平台不判断两次提交是否"一样"，也不合并。`POST /tasks` 每次都创建新 Task。只有平台**内部操作**（同一 `(task_id, attempt, seq)` 的 Job 创建）需要"重复执行不产生副作用"的安全语义，那是实现细节，不是对外接口承诺。

### 5.8 退出语义

| 执行结果 | Exit Code | runner 状态 | 含义 |
|---|---|---|---|
| 执行正常完成 | 0 | `SUCCEEDED` | 执行成功。指标是否达标由外部判断，平台不做门禁 |
| 参数错误或资源缺失 | 2 | `FAILED` | 输入/环境错误（输入非法、参数预检失败、依赖缺失、连接不通） |
| 工具执行失败 | 1 | `FAILED` | 用例不通过、指标未达标（自报失败）、部署未就绪、拆除失败 |
| 工具异常退出 | >2 或信号 | `FAILED` | 异常终止 |
| 执行超时 | 平台终止 | `FAILED` | 错误码 `RUNNER_TIMEOUT` |
| 被取消 | 平台终止 | `CANCELED` | 用户或策略取消 |

约定：

- **必须通过退出码表达结果**，不得"进程 0 退出但产物里写失败"。
- 参数预检失败应在任何执行动作前退出，不得先跑后判。
- 平台不解析 stdout 判断成败，只认退出码 + 产物。
- **抖动自愈在 entry 内部完成**（连接重试、等待就绪）；平台不做 runner 级重试（§7.8）。
- 测试执行成功与测试结果达标是两件事：精度测试完整执行但准确率偏低，Runner 应正常返回结果（exit 0），由下游消费指标；**不得**把"指标不理想"映射为执行失败。

### 5.9 结果归一化义务

Runner 负责把工具输出映射到标准结果 Schema（附录 A），具体要求：

- `metrics[].name` 使用 `snake_case`，同一 runner 内同名指标的区分靠 `labels`，不得用名字编码维度（禁止 `ttft_concurrency_8` 这类命名）。
- 每个指标必须带 `unit` 与 `statistic`。
- `statistic` 只允许：`mean` / `p50` / `p90` / `p95` / `p99` / `min` / `max` / `sum` / `count`。
- 多组参数的结果必须能在 `labels` 上区分，否则视为契约不合规。
- 失败时**应尽可能**产出三选一之一（推荐 `result.json`：`status: failed` 且带 `error`），保证每次执行都有可读结果；什么都没产出时由平台按退出码与 stderr 记录失败事实。

### 5.10 独立运行（脱离平台）

> **裁决：** `role: test` / `pre_check` 的 Runner 必须能脱离 Argus 单独运行。平台只是它的一种运行方式。内置 `service-deploy` / `service-clean` 依赖 Genet 凭据，属**豁免项**，但也必须在有凭据的机器上可独立运行（同一套 entry 逻辑）。

任何 test / pre_check Runner 在只有输入文件和环境变量的情况下，都必须跑出与平台内一致的产物。

| 要求 | 说明 |
|---|---|
| 业务输入只走声明格式 | 从 `--params-file` / `AG_PARAMS_FILE` / 约定路径读取（§5.3）；不读任何平台专有文件，不支持 stdin |
| 公共参数走环境变量或 CLI | 独立运行时 `--url` / `--model_name` / `--model_path` CLI 优先，回落 `AG_SERVED_URL` 等 |
| 元信息可缺省 | 平台不注入 `AG_TASK_ID` / `AG_RUNNER_SEQ` / `AG_ATTEMPT` 时按 `local` 处理 |
| 目录可覆盖 | 输出目录必须能被 `AG_OUTPUT_DIR` 覆盖（默认 `/workspace/output`） |
| 凭据走环境变量 | 认证信息一律从环境变量读，不依赖平台注入机制 |
| 产物自足 | 独立运行时同样产出与平台一致的三选一产物（产出 `result.json` 时符合附录 A），日志写 `logs/`，不要求平台收集 |
| 不依赖 K8s | test / pre_check 不得调用 K8s API、不得要求 ServiceAccount、不得假设挂载来源；deploy / clean 走 Genet API，同样不直连 K8s |

运行方式（都由使用者负责，不属于平台能力，§6.4）：

```bash
# 容器内直接跑：与平台里是同一套 entry 逻辑
docker run --rm \
  -e ENDPOINT_API_KEY=... \
  -e AG_OUTPUT_DIR=/workspace/output \
  -v /tmp/out:/workspace/output \
  -v /tmp/params.jsonl:/workspace/input/params.jsonl:ro \
  registry.dev.huawei.com/argus/endpoint-probe:1.0.0 \
  /runner/run.sh --url http://10.0.0.10:8000/v1 --model_name Qwen3-32B

# 或 SSH 登录一台主机，在已有镜像/环境里跑
ssh host 'bash /runner/run.sh --params-file /tmp/params.jsonl --url=... --model_name=...'
```

没有 build 步骤：镜像由 CI 产出并发布，平台、SSH、本地一律只 pull + run（§6.4）。

**验收方法：** 在没装 Argus 的机器上，`docker run <image> <entry> --params-file <path> --url ... --model_name ...` 能跑出期望产物即为合规。

### 5.11 pre_check 类 Runner 的附加约束

`role: pre_check` 的 Runner 除了通用契约，还要满足四条：

1. **探测必须发生在实际测试能访问服务的网络里**。在控制端进程内探测没有意义：K8s ClusterIP 之类地址从控制端不可达。
2. **等待窗口由 Runner 自己实现**（`wait_timeout_seconds` / `retry_interval_seconds`）。平台不提供 `wait_for_ready` 这类包装字段。
3. **必须区分"未就绪"与"不可重试"**：5xx、404（PD 注册中）、429、连接被拒、超时可以继续等；4xx 请求错误、认证失败、DNS / TLS 错误、2xx 里包着 `error` 对象应当立即失败并置 `retryable: false`。
4. **探活的"内部等待"与 Task 级重试不可混用**：前者是一次执行内反复探测（正常等待）；后者是整 Task 新 attempt 全量重跑含重新部署（§7.8）。

### 5.12 导出契约（exports）

> **裁决（v2 新增）：** Task 内的信息传递统一走"**exports 文件 + 平台收割 + 环境变量注入**"。平台只做词法级 KV 透传，不理解键或值的含义。

**产出契约：**

- runner 向 `$AG_OUTPUT_DIR/exports.env` 写 KV 行：每行 `KEY=VALUE`，`#` 开头为注释；值不含换行。
- 只传**小字符串**（URL、模型名、路径、部署 ID）。大数据走 `AG_OUTPUT_DIR` 产物文件 + 路径入 exports。
- `exports.env` 中**不得**出现凭据明文（凭据走 §12.4 的 secrets 注入，反向不成立）。

**收割与注入规则（平台职责，全部词法级）：**

1. runner 以**退出码 0** 结束时，平台读取其 `exports.env`；解析失败的行整体拒绝收割，runner 判失败（`EXPORT_INVALID`）；
2. 收割结果随 attempt 记录**落库落盘**——事后可完整还原每次注入的内容与来源；
3. 按**管道线性顺序合并**注入后续 runner；组合期校验**禁止重复 key**（两个 runner 声明导出同名 key → 拒绝提交）；
4. 失败（退出码 ≠ 0）或 SKIPPED 的 runner **不收割**；
5. Task 级静态值（外接模式的 service 信息、secrets）与 exports 走同一套注入通道，静态值视为"序号 0 的导出"。

**声明与组合期校验：**

- `runner.yaml.exports`（可选）：声明本 Runner 产出的 key 列表；
- `runner.yaml.consumes`（可选）：声明本 Runner 需要的上游 key；组合期校验每个 key 必须有上游导出、静态值或 Task 级 secret 来源，否则拒绝提交；
- 内置契约：service-deploy **必须**导出 `AG_SERVED_URL`、`AG_SERVED_MODEL_NAME`、`AG_MODEL_PATH`、`AG_DEPLOY_REF`；test / pre_check Runner **建议**声明 `consumes: [AG_SERVED_URL, AG_SERVED_MODEL_NAME]`。

---

## 6. Executor 与 Job Spec

### 6.1 Executor 的定位

Executor 是平台提供的通用任务执行适配层。它**只负责**：

- 准备执行环境。
- 获取 Runner 镜像。
- 挂载输入和输出目录。
- 注入环境变量与凭据。
- 启动 runner 并跟踪运行状态。
- 收集日志和产物。
- 取消和清理执行资源。

它**不负责**：

- 理解输入语义。
- 拼接具体工具命令。
- 解析原始测试结果。
- 判断性能或精度是否达标（平台不设门禁，见 §14）。

### 6.2 Executor 与 Runner 正交

平台只有**一种** Executor：**K8s Job**。"正交"指 Runner 不感知执行环境——它不知道自己跑在 K8s Job 里，还是被人手工在主机上执行。

| 运行方式 | 由谁负责 | 是否平台能力 |
|---|---|---|
| K8s Job（`kubernetes`） | 平台 Executor | 是 |
| 容器 / 主机上直接执行 entry（含 SSH） | 使用者本人 | **否**（§5.10） |

推论：

- **不得**为适配某个执行环境去修改 Runner 的逻辑；若必须修改，说明契约设计有问题。
- 平台**不提供** SSH / Docker 执行后端，也不登记远端主机、不代管远端凭据。
- `runner.yaml` 不声明 `executors` 字段——只有一种平台 Executor。
- **任何运行环境都不 build**：镜像引用由 CI 产出并发布，平台、SSH 主机、本地一律只 pull + run（§6.4）。

### 6.3 Kubernetes Executor

在已有 K8s 集群创建 Job 执行 Runner 容器。

负责：选择已有 Cluster 与 Namespace、创建 Job、配置资源请求与限制、配置 GPU/NPU 资源、挂载输入输出与数据集目录、跟踪 Job/Pod 状态、收集日志与产物、结束后清理 Job。

约束：

- 使用 **Job** 执行一次性 runner；每个 runner 一个 Job，Job 的幂等键为 `(task_id, attempt, seq)`。
- 不把 Deployment 作为常规 runner 的执行资源。
- 不负责集群创建、集群扩缩容、底层 GPU/NPU 调度策略（交给 K8s / Volcano）。

### 6.4 平台外运行（不是 Executor）

在 Docker 主机上跑 Runner——包括 SSH 登录后执行——是**使用者的运行方式，不是平台能力**：

- **没有 build 行为**：所有人都用 CI 产出的镜像；SSH 主机与本地只做 `docker pull` + `docker run`。
- 平台不登记主机、不建 SSH 连接、不管理远端密钥。
- 使用者自行拉镜像、挂输入输出目录、执行 entry；产出的产物（三选一）与平台里跑出来的是同一份（附录 A）。

约束：这条路径不产生平台的 Task / runner 记录。需要进入平台生命周期，就走 K8s Executor。

### 6.5 Execution Profile

Execution Profile 描述一个可供 runner 使用的 K8s 执行环境。Profile 通过 `cluster` 字段引用**已登记的集群对象**；kubeconfig 由 Secret 挂载到 `CLUSTERS_DIR`，**不入库、不进日志**（G-10、W-10）。

```text
Execution Profiles
├── k8s-npu
│   ├── executor: kubernetes
│   ├── cluster: cluster-01
│   ├── namespace: argus
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

- Runner 不绑定具体集群、不绑定命名空间。
- Execution Profile 不绑定具体 Runner。
- runner 选择一个满足 Runner 运行需求的 Execution Profile。
- Profile 负责提供环境能力与挂载供给；Runner 负责声明运行需求。

### 6.6 超时与取消语义

| 项目 | 约束 |
|---|---|
| runner 超时 | 由**平台配置 + Manifest 覆盖**决定（`runners[].timeout_seconds`），Runner 不声明时限。上限见 §12.7 |
| 超时处理 | Executor 强制终止进程 / 删除 Job；runner 状态 `FAILED`，错误码 `RUNNER_TIMEOUT` |
| 取消 | 必须能终止底层资源（删除 K8s Job 并清理对应 Pod） |
| 取消竞态 | 取消与自然结束同时发生，以**先落库的状态**为准，后到者不覆盖 |
| 清理失败 | 不改变 runner 结果，但必须记录 `CLEANUP_FAILED` 事件并告警 |

### 6.7 并发控制（v2 收窄）

> **裁决（v2，取代 D-32）：** Task 内 runner **严格串行**，平台不再提供"门控后并发"模型。Endpoint 级互斥只在外接模式（多 Task 共享同一外部服务）下保留。

- **部署模式**：服务由本 Task 的 service-deploy 拉起，是 Task 私有的；runner 串行执行，隔离由拓扑保证。`endpoint_exclusive` 声明为冗余但无害。
- **外接模式**：多个 Task 可能指向同一外部服务。`runner.yaml.concurrency.endpoint_exclusive: true` 的 runner 执行期间，同 Endpoint（按 `AG_SERVED_URL` 规范化 host:port 识别）上的任何其他 Task 的 runner 必须排队。
- 队列与槽位状态必须落库，控制端重启后恢复；全局并发上限由平台配置项控制。
- pre_check 与性能类 Runner 在外接模式下仍应声明 `endpoint_exclusive: true`（探活与测量不得共享服务）。

### 6.8 输入输出挂载约定

Executor 渲染任务时，必须把输入与输出目录挂到固定容器路径，且**输出目录直接指向最终存储路径**——不允许"先写容器本地、跑完再拷贝"作为默认实现：

| 容器路径 | 挂载源 | 权限 | 说明 |
|---|---|---|---|
| `/workspace/input` | `<storage-root>/<task_id>/attempts/<n>/input/<seq>/` | **只读** | 平台把**该 runner** 的输入原文写入 `params.jsonl` / `params.yaml`；每 runner 一个子目录，互不可见 |
| `/workspace/output` | `<storage-root>/<task_id>/attempts/<n>/runners/<seq>/` | 读写 | **就是最终落盘位置**，平台不搬运任何字节（`exports.env` 由平台在成功后读取） |
| Profile 挂载点 | Execution Profile 提供的共享存储（数据集、模型） | 只读 | 由 `runner.yaml.mounts` 声明（§10.4） |

约束：

1. **路径由平台生成**：`task_id` / `attempt` / `seq` 全部由平台生成并做路径安全校验（禁止 `/`、`..`、空白、控制字符）。
2. **必须用 subPath 挂载**：K8s 用 PVC `subPath`。**不得**把 `<storage-root>` 整体挂进容器——否则 Runner 可以读写其他 Task 的产物。
3. **目录由平台预创建**（属主 = 运行 UID，权限 `0770`）；Runner 仍应容忍目录缺失并自行 `mkdir -p`。
4. **attempt 之间互不可见**：重试写入新的 `attempts/<n>/`，历史 attempt 的产物不得被覆盖。
5. **失败保留现场**：runner 失败不得删除该目录；归档或清理由平台配置的保留期决定（§8.6）。
6. **"收集"退化为校验**：产物已直接落在最终位置，收集环节只剩四件事：校验产物契约（三选一，`result.json` 存在时须合法）、收割 `exports.env`、补齐 runner / Task 报告、生成索引。
7. **存储前提**：K8s 所有节点都能访问同一份 NFS（现网已具备）。不提供"先写 emptyDir、跑完再打包上传"的退化模式。
8. **渲染权归平台**：Runner **不得**提供任何完整 K8s 清单。特殊需求只能在 `runner.yaml.k8s` 用白名单字段声明（附录 C），由平台合并渲染；平台字段（镜像、命令、input/output 挂载、超时）优先且不可被覆盖。

### 6.9 调度可见性（Volcano）

> **裁决：** 调度归调度器，**可见性归平台**。平台不参与调度决策，但必须能回答"为什么还在排队"。

平台自己的并发控制（§6.7）只决定"谁先拿到 Endpoint 槽位"；真正决定 Pod 什么时候起来的，是 K8s 与 Volcano。这两层等待必须分开呈现。

平台必须暴露（**只读**，不写回调度器）：

| 信息 | 来源 | 用在哪 |
|---|---|---|
| 队列名、队列状态、`deserved` / `allocated` | Volcano `Queue` CRD | 看板与 runner 详情的等待原因 |
| 排在该队列前的 Job 数、gang 是否满足 | Volcano `Job` / PodGroup | 同上 |
| 节点资源总量与已分配 | K8s Node 与 Pod 统计 | 看板 |
| 具体 runner 当前卡在谁身上 | Pod 的 `Pending` 事件 | runner 详情的 `waiting_reason` |

约束：

- **只读**：平台不改队列权重、不抢占。
- **`QUEUED` 必须带原因**：`queue_full` / `insufficient_resources` / `gang_pending` / `slot_waiting`。
- **不引入第二套监控**：数据从 K8s / Volcano API 拉（D-49：轮询 + 15s TTL + 单飞，降级四态只影响 `volcano` 块）。
- **部署排队（Genet 侧）的可见性**由 service-deploy runner 周期改写 `deploy-state.json`（§9.1）承载，不再有平台 `DEPLOYING` 阶段。

---

## 7. 状态机与失败策略

### 7.1 runner 状态机

```text
   PENDING ──► SKIPPED（上游 abort / 取消，reason 必填）
      │
      ▼
   QUEUED ──► CANCELED
      │
      ▼
  PREPARING ──► CANCELED
      │
      ▼
   RUNNING ──► SUCCEEDED / FAILED / CANCELED
```

| 状态 | 语义 | 终态 |
|---|---|---|
| `PENDING` | 已创建，等待前序 runner（依赖未推进） | 否 |
| `QUEUED` | 依赖已满足，等待执行槽位（外接模式 Endpoint 互斥 / 全局并发） | 否 |
| `PREPARING` | 已获得槽位，正在准备环境（拉镜像、挂载、写入输入文件） | 否 |
| `RUNNING` | Runner 容器 / 进程已启动 | 否 |
| `SUCCEEDED` | 退出码 0，且产物契约满足（三选一至少其一存在；`result.json` 存在时必须合法） | 是 |
| `FAILED` | 非零退出、超时、环境错误、结果缺失、导出解析失败（`EXPORT_INVALID`） | 是 |
| `CANCELED` | 用户或策略主动取消 | 是 |
| `SKIPPED` | 未执行：上游 abort 或取消。**必须带 `reason`** | 是 |

**状态规则：**

- 状态转移只能沿上图进行，任何模块不得直写状态，必须通过状态机服务（`No Hidden State`）。
- **没有重试回路**：`FAILED` 是终态（v2 取消了 v1 的 FAILED→QUEUED 自动重试，§7.8）。
- **大小写约定**：本文状态名用大写表示概念；库 / API / 报告中的 token 统一小写，前端展示为大写徽标（D-34 沿用）。
- `SKIPPED` 与 `FAILED` 严格区分：`SKIPPED` 表示"没跑"，`FAILED` 表示"跑了但失败"（D-06 沿用）。

### 7.2 attempt 模型（Task 级）

Task 是稳定标识与**重试单位**；attempt 是 Task 的第 N 次全量执行；runner 在 attempt 内按序执行**恰好一次**。

```text
Task (task_id)
 └── attempt#1
      ├── runner seq=1  service-deploy   exit_code / 日志 / exports / deploy-state.json
      ├── runner seq=2  endpoint-probe   exit_code / 日志 / result.json
      ├── runner seq=3  aisbench-acc     ...
      └── runner seq=4  service-clean    exit_code / 日志
 └── attempt#2（整 Task 重试产生，全量重跑）
```

约束：

- `attempt_no` 从 1 递增；runner 执行记录由 `(task_id, attempt_no, seq)` 唯一定位，`seq` 从 1 递增（含注入的 deploy/precheck/clean）。
- 每次 attempt 独立产出全部 runner 的日志与产物，产物路径按 `attempts/<n>/` 分层（§6.8）。
- 重试**不覆盖**历史 attempt 的记录，只追加。
- Task 级报告默认取**最后一个 attempt**；UI 可切换查看历史 attempt。

### 7.3 失败策略

> **裁决：** 失败传播必须**显式声明**——Runner 与 Manifest 都能声明，取更严格者。`role: clean` 不参与传播（finally 语义，§7.6）。

失败扩散有两处声明，方向不同：

| 位置 | 谁声明 | 表达什么 |
|---|---|---|
| `runner.yaml.on_failure` | Runner 维护者 | 这个 Runner 失败时**必须**怎样，例如"探活没过，后面测什么都没意义" |
| `runners[].on_failure` | 提交者 | 这次执行希望怎样 |

**合并规则：取更严格者。** Runner 声明 `abort_task` 而 Manifest 写 `continue` 属配置冲突，**提交阶段拒绝**（`VALIDATION_ERROR`），不静默降级。

```yaml
# runner.yaml
on_failure: abort_task        # continue | abort_task
```

```yaml
# Manifest（runners[] 内）
on_failure: continue         # 只能在 Runner 未要求 abort_task 时生效
```

| 策略 | 行为 |
|---|---|
| `continue`（默认） | 该 runner 失败后，后续 runner 继续执行；Task 终态按 §7.5 聚合（存在失败 → `failed`，即 v1 的 `COMPLETED_WITH_ERRORS` 语义并入 `failed`） |
| `abort_task` | 该 runner 失败后**终止本 attempt 内所有未执行的 runner**：`PENDING` / `QUEUED` 标 `SKIPPED`（`reason: upstream_abort`，传播到 clean 为止）；正在 `PREPARING` / `RUNNING` 的只可能是该 runner 自己（串行）。随后执行 service-clean，Task 落终态 |

**强制规则：**

- `role: pre_check` 与内置 `deploy` / `clean` **必须**声明 `on_failure: abort_task`；注册校验直接拒绝没声明的。
- `continue` 仅适用于失败不影响后续 runner 正确性的 test Runner。

### 7.4 Task 状态机

```text
SUBMITTED → QUEUED → RUNNING ──► SUCCEEDED / FAILED / SERVICE_UNAVAILABLE
                        │
                        └─► CANCELLING ──► CANCELED
```

- `RUNNING` 的内部推进 = runner 管道按序执行：`[service-deploy] → [pre_check…] → [test…] → [service-clean]`。**没有独立的 `DEPLOYING` / `CLEANUP` / `PRECHECK` 平台阶段**（v2 取消）：部署就是管道第一个 runner 在跑，拆除就是最后一个 runner 在跑，探活就是注入的第一个测试类 runner。前端在 Task 详情按 runner 时间线呈现执行链路。
- **部署排队与长窗口**：Genet 可能排队数小时到数天（D-31 沿用）。service-deploy runner 必须按 §9.1 周期改写 `deploy-state.json`（`queued/accepted/deploying/ready/blocked` + 水位 + 已等待时长），UI 读它呈现；部署总窗口由平台配置（默认 72 小时），超窗判该 runner 失败（`DEPLOY_BLOCKED`，`detail.reason=timeout`）。
- **取消**：任意非终态可取消；取消 = 终止当前 runner → 执行 service-clean → Task 落 `CANCELED`（§7.6）。
- **不存在 `GATING` 阶段与 `GATE_FAILED` 终态**（D-02 沿用）：指标是否达标由下游系统或 Agent 判断。

### 7.5 Task 终态语义

| 终态 | 语义 | 触发条件 |
|---|---|---|
| `succeeded` | 全部 test / pre_check runner 成功 | 所有非 SKIPPED 的测试类 runner 均 SUCCEEDED（clean 失败不影响，只打 `cleanup_failed`） |
| `failed` | 流程走完但存在失败 | 至少一个测试类 runner FAILED，且失败策略允许继续（v1 的 `COMPLETED_WITH_ERRORS` 并入此态） |
| `service_unavailable` | 被测服务不可用 | pre_check 失败且错误可重试（窗口内一直没就绪），或 service-deploy 失败且 `deploy-state.json` 归类为 Genet 受理但未就绪（`DEPLOY_BLOCKED`） |
| `failed`（平台侧） | 平台无法完成流程 | Executor 不可用、平台内部错误、service-deploy 调 Genet 失败（`DEPLOY_FAILED`）等编排异常 |
| `canceled` | 用户主动取消 | 收到取消请求并完成 clean 与资源回收 |

排查方向由此明确：平台侧失败找平台，`service_unavailable` 找服务部署方，`failed` 看具体失败 runner。pre_check 以**不可重试**错误失败（4xx、认证失败）时归 `failed`，由具体错误码定位；不记成服务不可用（D-35 沿用）。

### 7.6 取消语义与 clean 的 finally 执行

取消 Task 时：

1. 正在 `PREPARING` / `RUNNING` 的 runner 调用 Executor 终止底层资源，状态置 `CANCELED`；`PENDING` / `QUEUED` 的 runner 标 `SKIPPED`（`reason: canceled`，传播到 clean 为止）。
2. **追加执行 service-clean**（若本 Task 含部署）：调用 Genet 卸载（幂等，资源不存在 = 成功）。clean 自身失败记 `cleanup_failed` + `CLEANUP_FAILED` 事件，**不改变 Task 终态**。
3. clean 结束后 Task 才落 `canceled`。取消必须尽力而为但不阻塞：clean 重试若干次仍失败则落终态并进入可重放清单（§9.3），不悬挂在 `CANCELLING`。

**clean 是 finally 语义**：成功、失败、取消、超时四条路径全部执行；唯一不执行的路径是平台自身崩溃 / 节点丢失（clean Job 未被调度）→ 走 §9.3 重放兜底。

### 7.7 失败重跑与续跑

- **runner 级没有重跑**：runner 失败就是本次 attempt 的失败事实。
- **整 Task 重试**：见 §7.8——新 attempt 全量重跑。
- **跨 Task 续跑（`retry_of` 关联、复用未失败 runner 的结果）是 P1 能力**：仅限外接模式（无部署）；部署模式因 fresh-deploy 裁决（§7.8）不提供部分复用。P1 落地时沿用 v1 的复用校验（Runner 版本、镜像引用、digest 优先、输入快照、Endpoint 标识五者未变）。

### 7.8 重试语义（v2 新增）

> **裁决：** runner 级不做平台重试；**重试 = 整 Task 新 attempt，全量重跑，含重新部署（fresh deploy）**。

- 抖动自愈（连接重试、等待就绪）由 entry 内部完成（§5.3、§5.11）；平台的 v1 `retry.max_attempts` / `retry_on` 白名单机制**取消**。
- **不得复用上一次 attempt 的已部署服务**：跑过的服务带着 warm cache、残留连接与 KV cache 状态，性能数字与全新部署不可比（对 `aisbench-prefix` / `aisbench-perf` 是直接的正确性问题）。重试结果的可信度优先于省下的部署时间。
- attempt 只是计数与历史归组：`attempt_no` 上限由平台配置（默认 3）；重试入口为 `POST /tasks/{id}/retry`（外接模式同样全量重跑）。

---

## 8. 报告与产物

### 8.1 产物组织

```text
<storage-root>/
└── <task_id>/
    ├── report.json              # Task 级汇总（平台生成）
    ├── report.md                # 人可读摘要（平台生成）
    ├── metadata.json            # Task 元信息与提交快照
    ├── events.jsonl             # 状态与错误事件流
    └── attempts/
        └── <n>/
            ├── input/                  # 每 runner 一个子目录（input/<seq>/params.jsonl|yaml）
            ├── exports/                  # 平台收割落档：runners/<seq>.exports.env + 注入清单
            ├── deploy-state.json         # service-deploy 产出的最新部署状态副本（平台同步归档）
            └── runners/
                └── <seq>/
                    ├── result.json       # Runner 产出（test / pre_check）
                    ├── exports.env       # Runner 产出的导出（平台收割源）
                    ├── deploy-state.json # service-deploy 产出（其余 runner 无此文件）
                    ├── logs/
                    │   ├── stdout.log    # 平台采集容器输出
                    │   └── <pod_name>.log # service-deploy 部署的 Pod 日志（按 Pod，§8.6）
                    └── raw/              # 原始测试工具产物（自由区）
```

独立提交的 runner（无 Task 上下文的调试形态）不进入此结构；平台内一切执行都有 Task 归属。

**deploy 的 Pod 日志不是"部署完就冻结"的一次性产物**（D-26/D-59 沿用）：一个部署可能对应多个 Pod（多节点、PD 分离），日志在测试期间持续产生。按 Pod 一份文件、边跑边写、记录区间、不随 Pod 回收——落盘位置从 v1 的 `deploy/logs/` 移到 deploy runner 的产物目录 `runners/<deploy_seq>/logs/`，语义不变。

`events.jsonl` 是**派生文件**：库里的 `state_events` 才是权威，由单 writer 追加，重启后按 `seq` 幂等补齐，可整份重建（G-8）。

### 8.2 报告分工

| 文件 | 生成方 | 职责 |
|---|---|---|
| `result.json` | Runner | 符合附录 A 的标准化测试结果（test / pre_check，**三选一之一**，不强求） |
| `exports.env` | Runner（产出）/ 平台（收割落档） | 下游可见 KV 与注入清单（§5.12） |
| `deploy-state.json` | service-deploy（产出）/ 平台（归档） | Genet 部署状态与导出值副本，泄漏重放的输入（§9.3） |
| runner `report.json` | Argus | runner 状态、输入快照、退出码、导出、产物引用 |
| Task `report.json` | Argus | runner 汇总、状态统计、失败清单，**不做指标数学聚合** |
| `report.md` | Argus | 人类可读摘要 |
| `logs/` | Executor / Argus | 执行日志与部署 Pod 日志 |

平台**不解析** Runner 的原始日志或私有产物格式，只做搬运、收割与索引。

### 8.3 指标规范

详见附录 A。除 Schema 约束外：

- 每个指标必须带 `unit` 与 `statistic`，禁止只给裸数值。
- 同一 runner 内多组参数的结果靠 `labels` 区分，`name` 不允许编码维度。
- 性能测试的 `input_len` / `output_len` / `concurrency` 必须进入 `workload` 或 `labels`，不得丢失。
- **平台不判断两组指标是否可比**，只保证把工作负载与统计口径如实交付。

### 8.4 Task 汇总结构

Task 报告只做**组织与索引**，不对不同 runner 的指标做加减乘除：

```json
{
  "task_id": "...",
  "attempt_no": 1,
  "status": "failed",
  "counts": { "total": 4, "succeeded": 2, "failed": 1, "skipped": 1, "canceled": 0 },
  "runners": [
    { "seq": 1, "name": "service-deploy", "role": "deploy", "status": "succeeded",
      "exports_ref": "attempts/1/exports/1.exports.env", "duration_ms": 5400000 },
    { "seq": 2, "name": "endpoint-probe", "role": "pre_check", "status": "succeeded", "duration_ms": 4200 },
    { "seq": 3, "name": "accuracy-dataset-a", "role": "test", "status": "failed",
      "result_ref": "attempts/1/runners/3/result.json", "duration_ms": 812000 }
  ],
  "failures": [
    { "seq": 3, "name": "accuracy-dataset-a", "error_code": "RUNNER_NONZERO_EXIT" }
  ],
  "cleanup_failed": false
}
```

首次指标筛选、分组、图表化由 UI 与下游系统完成。

### 8.5 结果完整性

- 每个 `SUCCEEDED` 的 test / pre_check runner 至少有**三选一**产物之一；产出 `result.json` 时必须合法（附录 A）。
- `FAILED` 的 runner 必须有状态、错误码与 stderr 摘要。
- `SKIPPED` 的 runner 必须带 `reason`。
- 不允许缺少结果的 runner 被当作成功。
- 部分结果必须可以单独获取（不要求整 Task 完成才可读）。

### 8.6 日志采集与持久化

> **裁决：** 日志是**持续采集并直接落盘**的产物，不是"Task 结束后再去拉一次"的快照。

**三类日志，三个位置：**

| 类别 | 落盘位置 | 写的人 |
|---|---|---|
| runner 执行日志（stdout / stderr） | `attempts/<n>/runners/<seq>/logs/stdout.log` | 平台采集容器输出 |
| Runner 自己写的结构化日志 | `attempts/<n>/runners/<seq>/logs/`（Runner 自定） | Runner |
| 部署 Pod 日志（Genet 渲染的服务 Pod） | `attempts/<n>/runners/<deploy_seq>/logs/<pod_name>.log` | 平台 collector（§9.2） |

**采集方式：平台侧 collector，不是 sidecar**（D-29 沿用）。服务 Pod 由 Genet 渲染，平台没有权限往里加 sidecar。collector 按 deploy 导出的 label selector 发现 Pod，`follow` 流式读取并追加写入共享存储；Pod 重启续写并插入标记行、分段落盘、允许 gap 但不伪造连续、日志体积不影响 runner 结果——规则全部沿用 v1 §8.6。**采集范围**：从 deploy runner 成功（服务 ready）起，到 clean 完成或 Task 终态为止；clean 完成后 collector 停止。

---

## 9. Genet 与内置 deploy / clean Runner

### 9.1 职责重划（v2）

> **裁决（v2，取代"平台 Deploy/Cleanup 阶段"）：** 与 Genet 的全部交互由**内置 service-deploy / service-clean runner** 执行；平台不再直接调用 Genet，只读取 runner 产出的 `deploy-state.json` 与 exports。

Genet 是服务部署与拆除的唯一职责方，其部署单元是 **Helm release**（D-44 沿用：定位符 = namespace + releaseName；`releaseName = "arg-" + sha256(task_id:attempt_no)[:12]`，必须 `noWait=true`）。内置 runner 的行为契约：

**service-deploy（role: deploy）**

1. 读取 `/workspace/input/params.yaml`（Genet 部署描述，`input_format: yaml`，平台不解析语义）；
2. 调 Genet OpenAPI 创建 release（`POST /api/open/helm/uc-packages/{stack|pd}/releases`，D-53）；
3. **轮询部署状态并周期改写 `AG_OUTPUT_DIR/deploy-state.json`**：`{genet_status, phase ∈ queued|accepted|deploying|ready|blocked, reason, waited_seconds, pod_count, label_selector, exports}`——这是部署排队可见性（D-46：水位有、位次无）与 `blocked` 折叠（D-47）的承载者；
4. 服务 ready → 导出 `AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH` / `AG_DEPLOY_REF`（releaseName 等），退出 0；
5. 失败 → 在 `deploy-state.json` 写明归类（`genet_call_failed` → `DEPLOY_FAILED`；`blocked/timeout` → `DEPLOY_BLOCKED`），退出 1 或 2。平台据此映射 Task 终态（§7.5）。

**service-clean（role: clean）**

1. 消费 `AG_DEPLOY_REF` 与 `deploy-state.json` 副本；
2. 调 `DELETE /api/open/helm/releases/{name}`（**只能**走 Genet OpenAPI，直接 `helm uninstall` 会被 admission webhook 拒绝，D-54）；
3. 判干净 = `GET 404` + pods 空（D-50/D-54）；幂等：已不存在 = 成功；
4. finally 语义执行（§7.6）；失败记 `cleanup_failed` + `CLEANUP_FAILED`，不改变 Task 终态，进入 §9.3 重放清单。

**Endpoint 语义**（v1 §9.1 沿用，载体变化）：服务的描述（地址、协议、端口、模型名）只来自两处——service-deploy 的 exports，或提交时静态 `service` 块。它只有两个消费方：pre_check 判服务可用、公共参数投影。不落文件、不跨 Task 传递、不下发给 Runner（Runner 只见 `AG_SERVED_*`）。

### 9.2 部署 Pod 日志与状态采集

- collector 按 deploy 导出的 `label_selector` 发现服务 Pod，按 Pod 落盘日志（§8.6）；
- `deploy-state.json` 的 Pod 清单快照（名称、角色、节点、重启次数）由 runner 周期写入，平台归档——多 Pod 拓扑还原的依据（D-26 沿用）。

### 9.3 泄漏兜底：重放内置 clean

> **裁决（v2 新增）：** 平台崩溃 / 节点丢失导致 clean 未被调度时，靠**重放**兜底，不引入平台侧 GC 机制。

- exports 与 `deploy-state.json` 按 attempt 落库落盘，信息不丢；
- 提供 `argus clean --task <id> [--attempt <n>]`：用落档的导出**重新拉起一次内置 service-clean Job**。平台仍是哑平台，只是重放一个 Job。REST 形态：`POST /api/v1/tasks/{id}/clean`（body 可选 `attempt`/`force`，缺省最后一个 attempt；重放执行记录名 `service-clean-replay` 追加在该 attempt 尾部；失败不改 Task 终态，只发 `cleanup_failed` 事件）；
- v1.5：周期扫描"已终态、含部署、无 clean 成功记录"的 attempt，自动重放并告警；
- **硬规则**：任何组件不得对 Genet 管理的资源设置指向 runner Job 的 K8s ownerReferences——Job 回收的级联删除会杀掉活着的被测服务。**runner 进程生命周期 ≠ 资源使用生命周期**（§8.1 原则）：deploy runner 在服务 ready 时退出，服务必须存活到 clean 完成或重放完成。

### 9.4 Jenkins 与下游

- Jenkins 是 Argus 的普通调用方，不享有特权路径：通过 REST API 提交 Task、查询状态、下载报告（D-37：不做提交幂等，Jenkins 侧自行避免盲目重试）。
- 下游只需要：Task ID、标准报告接口、标准产物下载接口。历史兼容由外部适配层负责，不进平台契约。

---

## 10. Manifest

Manifest 只描述评测意图、输入和执行环境。**不允许在 Manifest 中直接定义任意 Shell 命令**——`input` 只承载业务输入原文，不是命令。

### 10.1 字段语义

| 字段 | 说明 |
|---|---|
| `task.name` | Task 名称，用于展示，不参与复用判定；上限 200 字符（D-58） |
| `deploy` | 与 `service` 二选一；内容为 Genet 部署描述（YAML 语义，平台不解析），作为 service-deploy 的输入传入 |
| `service` | 与 `deploy` 二选一；外接模式直接给出 `url` / `model_name` / `model_path`，平台作为静态导出注入全部 runner |
| `precheck` | `auto`（默认）/ `off` / 显式对象。`auto`：平台取注册表里**唯一**一个 `role: pre_check` 的 Runner（0 个或多个都拒绝提交），插入测试类 runner 最前面；execution profile 复用首个 test runner 的 profile |
| `env` | Task 级环境变量，明文，注入该 Task 全部 runner（§12.4） |
| `secrets` | Task 级敏感值，**只填 Secret 引用**，由平台取值注入，不回显（§12.4） |
| `runners[]` | runner 列表（仅 test / 可选显式 pre_check）；严格串行执行 |
| `runners[].name` | 必填；同一 Task 内唯一，用于报告、日志定位与 UI 展示 |
| `runners[].runner` / `version` | 必填；必须命中已注册的白名单组合（`role: deploy/clean` 不可引用——它们由平台自动注入） |
| `runners[].input` | **输入原文**（该 runner 输入框的内容，JSONL / YAML 按 `input_format`）。平台校验后落盘为输入文件；平台运行时 argv 为空 |
| `runners[].execution.profile` | 必填；引用已登记的 Execution Profile |
| `runners[].on_failure` | `continue` / `abort_task`，默认 `continue`；**不能弱化** `runner.yaml.on_failure`（§7.3） |
| `runners[].timeout_seconds` | 可选；覆盖平台默认 runner 超时（§6.6） |
| ~~`tests[].retry`~~ | **已取消**（§7.8）：runner 级重试不存在 |
| ~~`cleanup.policy`~~ | **已取消**（§9.1）：有部署必有 clean（finally）；产物保留期由平台配置统一决定（D-33 保留） |

组合期校验（提交阶段，全部 `VALIDATION_ERROR`）：

1. `deploy` XOR `service`（都填或都不填拒绝）；
2. `runners[]` 至少 1 条（D-57 沿用；注入的 deploy / precheck / clean 不计入用户列表）；
3. exports/consumes 闭合：每个 runner 声明的 `consumes` key 必须有上游导出、静态值或 secret 来源；任何两个 runner 的 `exports` 不允许同名 key；
4. `input` 通过该 Runner 的 `input_format` 校验（jsonl 结构校验见 §5.4）；
5. 必填 env 检查（§12.4）。

### 10.2 完整 Task 示例（部署模式）

```yaml
task:
  name: qwen3-32b-daily

  deploy:                       # 部署模式：平台自动前置 service-deploy、追加 service-clean
    provider: genet
    spec: |
      model: Qwen3-32B
      replicas: 1
      resources: { gpu: 8 }

  precheck: auto                # 平台注入 service-deploy 之后的第一个测试类 runner

  env:
    LOG_LEVEL: info
  secrets:
    ENDPOINT_API_KEY: endpoint/prod-qwen

  runners:
    - name: accuracy-dataset-a
      runner: aisbench-acc
      version: "1.0.0"
      execution: { profile: k8s-npu }
      on_failure: continue
      input: |
        {"dataset":"/data/dataset-a","limit":100}

    - name: performance-sweep
      runner: aisbench-perf
      version: "1.0.0"
      execution: { profile: k8s-npu }
      on_failure: abort_task
      timeout_seconds: 14400
      input: |
        {"input":512,"output":128,"concurrency":1}
        {"input":512,"output":128,"concurrency":4}
        {"input":1024,"output":128,"concurrency":8}
```

### 10.3 外接模式示例（单 runner）

```yaml
task:
  name: existing-service-accuracy

  service:                        # 不部署：直接给出已有服务
    url: http://10.0.0.10:8000/v1
    model_name: Qwen3-32B
    model_path: /models/Qwen3-32B

  precheck: auto                  # 先探活，通过后再跑

  runners:
    - name: accuracy-dataset-a
      runner: aisbench-acc
      version: "1.0.0"
      execution: { profile: k8s-npu }
      input: |
        {"dataset":"/data/dataset-a"}
```

这个 Task **不拉服务**（没有 `deploy`），三个公共参数写在 `service` 里，由平台注入环境变量。换成部署模式，把 `service` 换成 `deploy`，`runners[].input` 一字不改——这正是把取值点统一到一处换来的好处（D-27 沿用）。

### 10.4 资源路径语义

> **裁决：** 输入与公共参数中的任何路径对平台都是**不透明字符串（opaque string）**。平台不解析、不校验存在性、不做路径替换、不猜测容器内外映射。

- 路径如何可达由 Runner 决定：镜像内置、Profile 提供的挂载点、或 Runner 自行下载。
- **统一共享挂载是平台级配置（D-63/D-65 沿用）**：`shared_mounts` 表是运行时真源，executor 渲染所有 runner Job 时读"启用中的行"；管理面 4 个端点 + viewer 只读端点；平台不创建 PV/PVC；与 `runner.yaml.mounts` 或平台保留路径（`/workspace/output`、`/workspace/input`、`/tmp`、`/runner`）同路径 → 提交期 400。
- Runner 额外挂载在 `runner.yaml.mounts` 声明：`name` / `mount_path` / `read_only` / `source`（PVC 名，缺省用平台 storage PVC）；挂载失败是运行期事实（`MOUNT_UNAVAILABLE`）。
- 平台不得为"帮你找数据集"而引入资源登记与解析逻辑。

### 10.5 引用与安全

- `deploy` / `precheck` / `runners[].execution.profile` 都只能**引用**已登记对象，不能在 Manifest 中内联凭据、主机器密信息或任意命令。
- `service.url` 的值不做任何地址限制（§9.1）；凭据约束见 §12.4。
- 平台不经 shell 执行任何提交内容；`input` 原样落盘，语义归 Runner。

---

## 11. Web UI

### 11.1 定位

> **裁决：** Argus 必须提供 Web UI。UI 与 API / CLI / MCP 并列为一等消费方；UI 的每一个操作都必须有对应的 REST API。

### 11.2 P0 能力

| 能力 | 说明 |
|---|---|
| Task 列表 | 按状态、时间、Runner 筛选，展示耗时与错误码；概览页承载全量 Task 表（D-62 沿用） |
| Task 详情 | 状态流转时间线 + **runner 管道时间线**（串行链路：deploy → pre_check → tests → clean，逐节点展开）、attempt 切换、输入快照 |
| 报告查看 | 渲染 `result.json` 的指标表格（按 `labels` 分组）、`report.md`、产物下载；产物清单只在结果页（D-64 沿用） |
| 日志查看 | 只读，按 runner 与 attempt 定位；部署 Pod 日志在 deploy 节点下按 Pod 分页签，标注"测试期间仍在追加" |
| 提交 Task | **一张卡片两个形态**：部署（填 Genet YAML）XOR 外接（填 `url` / `model_name` / `model_path`）；每个 runner 一个输入框（按 `input_format` 占位）；平台不渲染表单、不预填（照上次配置再来一次除外，D-62） |
| 环境变量与凭据 | **Task 级填一次**，注入该 Task 全部 runner（§12.4） |
| 部署状态展示 | 读最新 `deploy-state.json`：`phase` / 水位 / 已等待时长 / Pod 清单；不展示位次（D-46） |
| Runner 目录 | 浏览已注册 Runner、角色、版本、`input_format`、参数说明、exports/consumes |
| 取消 / 重试 | 取消 Task（先 clean 后终态）；重试 = 新 attempt 全量重跑，需二次确认"将重新部署" |
| 看板 | 集群与队列（Volcano）快照、运行概况、最近 Task（P1，§11.4） |

### 11.3 明确不做

- 拖拽式流程编排器（编排通过 Manifest 表达；管道拓扑是固定的线性形态）。
- 自定义报表 / BI（下游系统负责）。
- 用户注册与自助开户（使用统一身份源）。
- 指标达标判定与门禁展示（平台不设门禁）。
- 在 UI 内直接编辑 Runner 代码或上传镜像。
- DAG / 并行分支的可视化编排（v2 管道是线性的）。

### 11.4 技术约束

- 前端为 SPA，直连同一套 REST API，**不引入独立 BFF**。
- 状态推送使用 SSE 订阅 `/tasks/{id}/events`：`seq` 单调、`Last-Event-ID` 续传、不是日志通道（G-14）。
- 新 Runner 上线无需改前端：提交框是文本输入框，参数说明由 `runner.yaml` 只读展示，指标展示由 `result.json` 结构驱动。
- 鉴权与 API 完全一致；Web 用户走团队统一身份源 + Argus 自建 OIDC client（D-48/D-51 沿用）。

---

## 12. 安全、可靠性与运维

Argus 是对内工具，但**对内部网络与生产主机拥有执行能力**，因此安全边界必须显式定义。

### 12.1 认证与授权

> **裁决：** 复用团队统一身份源（OIDC / SSO），不自建账号体系。

| 角色 | 权限 |
|---|---|
| `viewer` | 查看 Task / 报告 / 日志 / Runner 目录 |
| `operator` | viewer + 提交、取消、重试 |
| `admin` | operator + 管理 Runner 注册、Execution Profile、Secret、共享挂载 |

约束：

- 每个写操作都要记录操作者（写入 `events.jsonl` 与审计表），**禁止匿名提交**。
- 服务间调用（Jenkins、Agent）使用统一身份源签发的 JWT + JWKS 校验；控制面 / CLI 调 Genet 的 Open API Key 由 admin 管理（D-48/D-51/D-53 沿用）。
- 内置 Runner 的变更（service-deploy / service-clean / endpoint-probe）随平台发版走代码评审，不接受运行时上传。

### 12.2 网络与执行边界

- 平台不限制 `service.url` 指向哪里；实际可达性由出站策略决定。
- Executor 侧网络策略默认拒绝出站到互联网，仅允许镜像仓库、目标服务网段与 Genet API（service-deploy / service-clean 需要）。
- 容器默认非 root、只读根文件系统（除 `/workspace`），不允许 `privileged`。
- **runner 一律不绑定 K8s ServiceAccount**：deploy / clean 对集群的操作全部经 Genet API，凭据（Open API Key）以 Task 级 secret 注入。
- 平台不承担"在一台远端主机上执行命令"的能力（§6.4）。

### 12.3 安全约束清单

| 项目 | 约束 |
|---|---|
| Runner 注册 | 仅 `admin` 可新增或发布，走评审；`deploy` / `clean` 角色仅内置 |
| 镜像 | 引用形态不限，由团队约定与 CI 保证版本可追溯 |
| Endpoint | 地址不限，平台不做白名单校验 |
| 凭据 | 由平台注入，不写入输入目录、不落 exports、不落日志 |
| Manifest | 禁止提交任意 Shell 命令 |
| 输入 | 词法/结构校验后原样落盘，禁止经 shell |
| 超时 | 每个 runner 必须有最大执行时间 |
| 取消 | 必须能终止底层执行资源，且 clean finally 执行 |
| 产物 | 必须持久化，不依赖临时 Pod / 容器 |
| 续跑 | 复用结果必须校验有效性（§7.7，P1） |
| 版本 | Runner 版本可追溯、不可静默覆盖 |
| ownerReferences | 禁止指向 runner Job（§9.3） |

### 12.4 环境变量与凭据

> **裁决：** 环境变量是 **Task 级**的：提交时填一次，平台注入该 Task 下的**所有** runner。明文变量不做 Secret 化（D-25 沿用）。

- Runner 在 `runner.yaml.env` 声明读取哪些变量（`required` / `secret` / `description`）；声明 `required: true` 而未提供 → 提交阶段拒绝。
- 平台把 Task 级 `env` 与 `secrets` 展开到每个 runner 的 Job Spec（`env` / `secret_env`，附录 B）。
- **凭据不得进入 exports**（§5.12）； Tester/Runner 不得主动打印凭据；平台不做日志脱敏（D-30 沿用）。
- Genet Open API Key 以 secret 引用配置，注入 service-deploy / service-clean。

### 12.5 数据与状态存储

| 数据 | 存储 | 说明 |
|---|---|---|
| Task / runner 执行记录 / attempt / 状态事件 | 关系库（PostgreSQL） | 唯一的权威状态源；控制端无内存状态 |
| Runner 注册信息 | 关系库 | 与 `runners/` 目录扫描结果同步，以库为准 |
| exports 落档 | 库 + 文件 | 每次收割的 KV 与来源（§5.12） |
| Execution Profile / Secret / 共享挂载 | 关系库 | 由 `admin` 维护 |
| 报告与产物 | NFS（所有 K8s 节点可见） | 按 `<task_id>/attempts/<n>/runners/<seq>/` 组织（§6.8） |

约束：

- 状态机写入必须与业务操作在同一事务内（`No Hidden State`）。
- 控制端重启后，处于 `RUNNING` 的 runner 必须通过 Executor 对账恢复真实状态，不得直接假定失败。
- 状态与产物分离：库挂掉不影响已有产物可下载。

### 12.6 可观测性

- `/metrics` 暴露 Prometheus 指标：`runner_duration_seconds`、`queue_depth`、`runner_failures_total{code}`、`precheck_failures_total`、`executor_errors_total`、`cleanup_failures_total`、`deploy_state_blocked_total`。
- 结构化日志（JSON），每条含 `task_id` / `attempt_no` / `runner_seq`。
- 事件流：`submitted` / `state_changed` / `runner_started` / `runner_finished` / `exports_harvested` / `clean_finished` / `task_finished`。
- 告警：清理失败（含 `cleanup_failed` 与泄漏重放失败）、Executor 不可达、队列积压超阈值、Pre-Check 连续失败。

### 12.7 容量与非功能约束（初始值，可调整）

| 项目 | 约束 |
|---|---|
| 单 Task 最大 runner 数 | 200（超出拒绝，`TASK_TOO_LARGE`） |
| 单 Task 最大 attempt 数 | 默认 3（`POST /tasks/{id}/retry` 拒绝超出） |
| 全局并发执行 runner 数 | 可配置，默认 20 |
| 单 runner 最大执行时长 | 默认 4 小时，配置上限 24 小时（service-deploy 的部署窗口另行配置，默认 72 小时） |
| 报告写入 | 单 Task 报告 ≤ 20 MB，超出则产物外置 |
| 单次日志读取 | 分页返回 |
| 单 attempt 产物上限 | 默认 10 GiB；超出判失败（`ARTIFACT_QUOTA_EXCEEDED`），collector 每 60s 扫描（G-7 沿用） |
| 状态查询延迟 | P95 < 300 ms（非日志类接口） |

---

## 13. Non-Goals

分两类：**永久非目标**（属于系统定位，不应重新讨论）与**当期不做**（可在后续版本重新评估）。

### 13.1 永久非目标

| 不做 | 理由 |
|---|---|
| 不自研测试算法 | 复用 AISBench、EvalScope 等现有工具 |
| 不自研底层调度 | 复用 K8s、Volcano 和已有环境能力 |
| 不做服务部署平台 | Genet 已经承担该职责；平台连 Genet 调用都交给内置 runner（§9.1） |
| 不引入 Case | runner 已经是最小独立执行单元 |
| 不解析业务输入语义 | 输入属于 Runner 内部实现；平台只做 §5.4 的词法/结构校验 |
| 不解析导出变量的含义 | exports 只做 KV 透传与注入（§5.12） |
| **不做门禁判定（Gate）** | 平台只产出事实数据；是否达标由下游系统或 Agent 判断 |
| **不做 SSH / Docker 执行后端** | 平台只有 K8s 一种 Executor；主机直跑属平台外运行方式（§6.4） |
| 不替 Agent 做结论 | 平台提供事实，Agent 负责解读 |
| 不允许用户上传任意 Runner | 保证执行能力可控；`deploy` / `clean` 角色仅内置 |
| 不托管数据集与模型 | 沿用现有资源管理方式，平台只透传路径 |
| 不承诺结果可复现 | 平台保证执行逻辑可追溯（§5.7） |
| 不做平台侧 GC 机制 | 部署拆除由 clean runner（finally + 重放）承载（§9.3） |

### 13.2 当期不做（可重新评估）

| 不做 | 理由 |
|---|---|
| **不做 DAG / 并行分支管道** | 线性串行已满足评测场景；并行需要显式依赖声明与测量污染警告，复杂度不划算（§7.1） |
| 不做 runner 级平台重试 | 抖动自愈归 entry；整 Task 重试已覆盖（§7.8） |
| 不做跨 Task 部分续跑（P1 再评估） | 部署模式 fresh-deploy 裁决使部分复用仅在外接模式有意义（§7.7） |
| 不做模型 / 数据集 / 镜像仓库 | 沿用现有资源管理 |
| 不做自定义报表 / BI | 下游分析系统负责 |
| 不做多租户隔离 | 对内单团队使用，RBAC 已够用 |

---

## 14. P0 / P1 范围

### 14.1 P0：最小可用闭环

| 功能 | 验收要求 |
|---|---|
| Task 提交 | Manifest 提交后，Task 按 §7.4 走完整状态链，终态正确；`deploy` XOR `service`、exports/consumes、输入校验等组合期规则全部生效 |
| 部署模式 | service-deploy 自动前置：调 Genet 部署、`deploy-state.json` 周期更新、导出四件套、失败归类 `DEPLOY_FAILED` / `DEPLOY_BLOCKED` |
| 拆除与 finally | service-clean 自动追加：成功/失败/取消/超时四路径全部执行；幂等；失败只打 `cleanup_failed` 不改终态 |
| 泄漏重放 | `argus clean --task <id> [--attempt <n>]` 用落档导出重放 clean |
| Pre-Check | `precheck: auto` 注入第一个测试类 runner；失败按 `abort_task` 终止后续（clean 除外），Task 终态 `service_unavailable` |
| Runner Registry | 目录自动发现并通过校验；非法 `runner.yaml` 拒绝并给出字段级错误；`deploy`/`clean` 角色拒绝注册 |
| Runner 契约 | entry 声明、输入文件 + `AG_PARAMS_FILE`、`AG_*` 注入、输出目录、产物三选一、`exports.env` 全部可校验 |
| Runner 独立运行 | 不依赖平台：`docker run <image> <entry> --params-file ... --url ...` 跑通并产出三选一产物（§5.10） |
| K8s Executor | 每 runner 一个 Job，输入/输出 subPath 挂载，超时可终止，取消可删除 Job |
| runner 状态机 | 8 个状态、状态落库、重启后可恢复；`SKIPPED` 与 `FAILED` 严格区分；无重试回路 |
| attempt 模型 | `(task_id, attempt_no, seq)` 定位执行；重试产生新 attempt 全量重跑；历史不被覆盖 |
| 失败策略 | `continue` / `abort_task` 生效；`abort_task` 传播到 clean 为止；合并规则取更严格者 |
| exports | 收割、落库、线性合并注入、`EXPORT_INVALID` 处理、组合期重复 key 拒绝 |
| 结果报告 | runner 与 Task 级 `report.json` 符合 §8.2 分工与 §8.4 汇总结构；产物三选一可校验（产出 `result.json` 时符合附录 A） |
| 产物持久化 | 产物直接落共享存储最终路径（§6.8）；输入原文与导出落档可审计 |
| 部署 Pod 日志 | 按 Pod 落盘在 deploy runner 产物目录，持续追加（§8.6、§9.2） |
| **Web UI** | 列表、详情（runner 时间线）、报告、日志、提交（部署 XOR 外接卡片）、取消/重试可用；全部操作有对应 REST API |
| API 契约 | OpenAPI 描述完备（`/tasks`、`/runners` 路径），错误码使用附录 D 枚举 |

### 14.2 P1：AI 友好与效率

| 功能 | 验收要求 |
|---|---|
| MCP Server | Runner 查询、Task 提交与查询、取消、报告获取 |
| CLI（`argus`） | API 能力的命令行映射（submit / status / logs / clean），无独有能力 |
| 结构化事件 | `events.jsonl` + SSE 推送状态与错误码 |
| 跨 Task 续跑 | 仅外接模式；`retry_of` 关联 + 复用校验（§7.7） |
| Task 对比 | 同名指标 + `labels` + 工作负载对齐展示；平台不判断可比性 |
| Matrix 展开 | 提交时将参数矩阵自动展开为独立 runner（可选，不替代 §4.2） |
| Runner 校验工具 | 维护者本地校验 `runner.yaml`、entry 与样例执行结果 |
| UI 增强 | 指标图表、attempt 间对比、批量操作 |
| 看板 | 集群 / Volcano 队列快照、运行概况、快捷新建 Task（§11.2） |
| 泄漏自动重放 | 周期扫描无 clean 成功记录的终态 attempt 自动重放（§9.3） |

---

## 15. 文档维护与变更流程

1. 修改本文中带"裁决"的条目，必须：提升版本号、在 §0.6 记录、并在附录 E 追加决策条目。
2. 修改附录 A / B / C 的契约字段属于**破坏性变更**，必须提升 `schema_version`（v2 已升至 `2.0`），并给出至少一个版本的兼容期。
3. Non-Goals 的调整：§13.1 需要重新论证系统定位；§13.2 只需在变更记录中说明即可。
4. 实现与本文冲突时以本文为准；若认为本文有误，先改本文再改代码，不允许"代码先行、文档滞后"。
5. **状态机、数据表、API 三处必须同步改**：`design/views/02`、`design/views/03`、`design/views/07` + `server/api/openapi.yaml`。

---

## 附录 A：`result.json` 结构（schema 2.0）

**三选一之一（§5.3）：** `result.json` / `result.html` / `result.csv` 至少产出其一，由 Runner 决定，**不强求 `result.json`**。本附录约束**产出 `result.json` 时**的结构；Runner 产出时平台校验。缺失 `schema_version`、`task_id`、`runner` 或 `metrics` 视为不合格（`RESULT_INVALID`）。

```json
{
  "schema_version": "2.0",
  "task_id": "t-20261110-0001",
  "attempt_no": 1,
  "runner_seq": 3,
  "runner": {
    "name": "aisbench-perf",
    "version": "1.0.0",
    "image_ref": "registry.dev.huawei.com/argus/aisbench-perf:1.0.0"
  },
  "status": "succeeded",
  "started_at": "2026-11-10T10:00:00+08:00",
  "finished_at": "2026-11-10T10:14:20+08:00",
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
| `schema_version` | string | 是 | 当前 `"2.0"`；不兼容变更才升版本 |
| `task_id` / `attempt_no` / `runner_seq` | string / int / int | 是 | 平台运行时取 `AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ`；**独立运行**（§5.10）时分别填 `local` / `1` / `1` |
| `runner.name` / `runner.version` | string | 是 | 必须与注册表一致 |
| `runner.image_ref` | string | 否 | 实际运行的镜像引用；平台可注入，Runner 不知道时可缺省 |
| `status` | enum | 是 | `succeeded` / `failed`（Runner 视角，仅描述执行是否完成） |
| `started_at` / `finished_at` | RFC3339 | 是 | 允许与平台记录有毫秒级差异 |
| `duration_ms` | integer | 否 | 缺失时由平台按时间戳计算 |
| `workload` | object | 否 | 工作负载摘要；性能类 Runner 必须提供 |
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

## 附录 B：Job Spec 结构（spec 2.0）

平台 → K8s Executor 的唯一定义。它只被平台内部使用；Runner 不知道 Job Spec 的存在（独立运行时根本没有 Job Spec，§5.10）。

```json
{
  "spec_version": "2.0",
  "job_id": "j-9f3a1c",
  "task_id": "t-20261110-0001",
  "attempt": 1,
  "seq": 3,
  "executor": "kubernetes",
  "image": "registry.dev.huawei.com/argus/aisbench-perf:1.0.0",
  "command": ["/runner/run.sh"],
  "args": [],
  "env": {
    "AG_TASK_ID": "t-20261110-0001",
    "AG_ATTEMPT": "1",
    "AG_RUNNER_SEQ": "3",
    "AG_PARAMS_FILE": "/workspace/input/params.jsonl",
    "AG_OUTPUT_DIR": "/workspace/output",
    "AG_SERVED_URL": "http://10.0.0.10:8000/v1",
    "AG_SERVED_MODEL_NAME": "Qwen3-32B",
    "AG_MODEL_PATH": "/models/Qwen3-32B",
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
    { "name": "input", "type": "input", "mount_path": "/workspace/input", "read_only": true,
      "storage_path": "<storage-root>/t-20261110-0001/attempts/1/input/3", "sub_path": true },
    { "name": "output", "type": "output", "mount_path": "/workspace/output", "read_only": false,
      "storage_path": "<storage-root>/t-20261110-0001/attempts/1/runners/3", "sub_path": true },
    { "name": "dataset", "type": "profile_mount", "profile_mount": "dataset-nfs", "mount_path": "/data", "read_only": true }
  ],
  "network": { "egress_allow": ["registry.dev.huawei.com", "10.0.0.0/8"] },
  "node_selector": {},
  "tolerations": [],
  "security_context": { "run_as_non_root": true, "read_only_root_filesystem": true },
  "labels": {
    "argus.io/task": "t-20261110-0001",
    "argus.io/attempt": "1",
    "argus.io/runner-seq": "3"
  },
  "annotations": {}
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `spec_version` | 是 | 当前 `2.0`；Executor 遇到未知版本必须拒绝 |
| `job_id` | 是 | 幂等键；同一 `job_id` 重复提交返回既有作业（幂等键 = `(task_id, attempt, seq)` 的派生） |
| `task_id` / `attempt` / `seq` | 是 | 用于回填与目录组织 |
| `image` | 是 | 镜像引用，原样来自 `runner.yaml`；平台不解析 |
| `executor` | 是 | 目前唯一合法值 `kubernetes` |
| `command` | 是 | Runner 的 `entry`（exec form 数组），平台不追加 argv |
| `args` | 是 | **恒为空数组**（v2：业务输入走输入文件，§5.3）。保留字段以维持结构稳定 |
| `env` | 否 | 非敏感环境变量；必须含 `AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ` / `AG_PARAMS_FILE` / `AG_OUTPUT_DIR`；公共参数 `AG_SERVED_*` 由平台按来源注入（deploy 导出或静态 service） |
| `secret_env` | 否 | 敏感变量，引用 Secret Store 键，**不传明文** |
| `resources` | 是 | `gpu.count: 0` 表示不需要加速卡 |
| `timeout_seconds` | 是 | Executor 必须据此强制终止 |
| `mounts` | 是 | `input`（只读，该 runner 的输入子目录 `attempts/<n>/input/<seq>/`）+ `output`（读写，必须给 `storage_path`，subPath 挂载）+ `profile_mount`（只读）三种 |
| `network.egress_allow` | 否 | 缺省时使用 Executor 默认策略（须含镜像仓库与服务网段；deploy/clean 另含 Genet API） |
| `security_context` | 否 | 缺省为 `run_as_non_root: true` |

Executor 返回结果：

```json
{
  "job_id": "j-9f3a1c",
  "phase": "succeeded",
  "exit_code": 0,
  "started_at": "2026-11-10T10:00:00+08:00",
  "finished_at": "2026-11-10T10:14:20+08:00",
  "reason": null,
  "log_ref": "task-output/t-20261110-0001/1/3/logs",
  "artifact_ref": "task-output/t-20261110-0001/1/3",
  "executor_ref": { "namespace": "argus", "job": "argus-t-0001-1-3" }
}
```

`phase` 枚举：`pending` / `running` / `succeeded` / `failed` / `canceled` / `timeout` / `unknown`。

**映射规则：** `succeeded` → 校验产物契约（三选一至少其一；`result.json` 存在时须符合附录 A）并收割 `exports.env`（§5.12），两者决定 runner 状态；`failed` / `timeout` / `canceled` 分别映射 `FAILED` / `FAILED(RUNNER_TIMEOUT)` / `CANCELED`；`unknown` 必须由对账任务复核，**不得**直接判失败。

---

## 附录 C：`runner.yaml` 结构（v2）

```yaml
name: aisbench-perf
version: 1.0.0
display_name: AISBench 性能测试
description: 对已部署 LLM 服务执行性能压测，输出 TTFT / TPOT / 吞吐等指标
role: test                    # test | pre_check | deploy | clean（deploy/clean 仅内置）
on_failure: continue          # continue | abort_task；pre_check 与内置 deploy/clean 必须 abort_task（§7.3）

entry: ["/runner/run.sh"]     # exec form 数组；文件名不限（§5.2）
input_format: jsonl           # jsonl（默认）| yaml | text（§2.1 / §5.4）

image: registry.dev.huawei.com/argus/aisbench-perf:1.0.0

resources:
  cpu: "4"
  memory: "16Gi"
  gpu: { resource: nvidia.com/gpu, count: 0 }

concurrency:
  endpoint_exclusive: true    # 仅外接模式有效（§6.7）；性能类与 pre_check 应为 true

mounts:
  - { name: dataset, mount_path: /data, read_only: true, source: dataset-nfs }

exports: []                   # 本 Runner 产出的下游 key（§5.12）；测试类 Runner 通常为空
consumes: [AG_SERVED_URL, AG_SERVED_MODEL_NAME]

# 受限 K8s 需求（可选，白名单字段）。平台渲染 Job 时合并；平台字段优先且不可覆盖（§6.8）。
k8s:
  node_selector: { accelerator: npu }
  tolerations:
    - { key: accelerator, operator: Exists, effect: NoSchedule }
  security_context: { run_as_user: 10001, fs_group: 10001 }

# 业务参数 schema（仅 input_format: jsonl；yaml/text 时必须为 []）。
# 平台校验：类型匹配 + required 存在性（§5.4）；其余（范围/枚举/跨字段）归 Runner。
parameters:
  - name: dataset
    type: string
    required: true
    description: "数据集在容器内的绝对路径；由挂载提供，Runner 自己校验存在性"
  - name: input
    type: integer
    required: false
    default: 512
    minimum: 1
    description: "输入长度（tokens）"

env:                          # 运行期环境变量声明（§12.4）
  ENDPOINT_API_KEY:
    required: false
    secret: true
    description: "Bearer token；由平台从 Secret Store 注入，Runner 只读不打印"

placeholder: |
  {"dataset":"/data/dataset-a","input":512,"output":128,"concurrency":8}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` / `version` | 是 | `name` 全局唯一；`version` 语义化版本，发布后不可覆盖（§5.6.1） |
| `display_name` / `description` | 是 | 中文，UI 展示用 |
| `role` | 是 | `test` / `pre_check` / `deploy` / `clean`；`deploy` / `clean` 用户注册直接拒绝（§5.1） |
| `on_failure` | 是 | `continue` / `abort_task`；Manifest 只能取更严格者（§7.3） |
| `entry` | 是 | **exec form 数组**（如 `["/runner/run.sh"]`，允许含解释器），平台 exec 该数组、不经 shell、不追加 argv；文件名不限（§5.2） |
| `input_format` | 是 | `jsonl`（默认）/ `yaml` / `text`；决定平台校验深度与输入文件名（§5.4） |
| `image` | 是 | 镜像引用（tag 或 digest 均可）；平台不解析 |
| `resources` | 是 | 默认资源需求，可被 Manifest 提高不可降低 |
| `timeout_seconds` | 禁止 | **不设此字段**。跑多久是编排策略；runner 超时由平台配置 + `runners[].timeout_seconds` 覆盖（§6.6） |
| `concurrency.endpoint_exclusive` | 是 | 仅外接模式有效（§6.7） |
| `mounts` | 否 | 额外挂载声明：`name` / `mount_path` / `read_only` / `source`（PVC 名）；`MOUNT_UNAVAILABLE` 是运行期观测（D-60） |
| `exports` / `consumes` | 否 | 导出/消费声明（§5.12）；组合期校验闭合与唯一性 |
| `k8s` | 否 | 受限 K8s 需求，白名单字段。**禁止**完整清单、`privileged`、`hostPath`、`hostNetwork`、自定义 `image` / `command`（§6.8） |
| `parameters` | 是 | 业务参数 schema（仅 jsonl）：平台校验**类型**与 **required 存在性**，其余归 Runner（§5.4）；`yaml` / `text` 时必须 `[]` |
| `env` | 否 | 声明本 Runner **读取**哪些环境变量（§12.4） |
| `placeholder` | 推荐 | 与 `input_format` 同格式的合法片段，UI 作输入框占位（§5.4） |
| `outputs` | 禁止 | **不设此字段**。产物契约全局统一：三选一（`result.json` / `result.html` / `result.csv` 至少其一，test/pre_check，不强求 result.json）+ `exports.env` + `raw/`、`logs/` 自由区（§5.3、§5.9） |
| `requires` | 禁止 | **不设此字段**。Runner 不声明对 Endpoint 的依赖；依赖表达用 `consumes`（§5.4.1、§5.12） |

---

## 附录 D：错误码（v2）

错误对象统一结构（扁平，D-39 沿用）：

```json
{ "code": "RUNNER_TIMEOUT", "message": "runner 执行超过 7200 秒被终止", "detail": { "task_id": "t-0001", "seq": 3, "timeout_seconds": 7200 }, "retryable": false, "trace_id": "..." }
```

| code | HTTP | 阶段 | 可重试 | 说明 |
|---|---|---|---|---|
| `VALIDATION_ERROR` | 400 | 提交 | 否 | Manifest 结构不合规；`deploy`/`service` 非互斥；inputs/consumes 校验失败；exports 重复 key；输入词法/结构校验失败（detail.issues[] 带行号+字段名） |
| `UNAUTHENTICATED` | 401 | 全阶段 | 否 | 缺少或无效令牌 |
| `FORBIDDEN` | 403 | 全阶段 | 否 | 角色权限不足 |
| `NOT_FOUND` | 404 | 全阶段 | 否 | 对象不存在 |
| `CONFLICT` | 409 | 提交 | 否 | 名称 / 版本冲突 |
| `RUNNER_NOT_FOUND` | 400 | 提交 | 否 | 未注册的 Runner |
| `RUNNER_VERSION_PINNED` | 409 | 注册 | 否 | 试图覆盖已发布版本 |
| `RUNNER_INVALID` | 400 | 注册 | 否 | `runner.yaml` / entry 不合规；`deploy`/`clean` 角色非法注册 |
| `EXECUTION_PROFILE_NOT_FOUND` | 400 | 提交 | 否 | 未登记的 Execution Profile |
| `TASK_TOO_LARGE` | 400 | 提交 | 否 | runner 数超过上限（§12.7） |
| `ATTEMPT_EXHAUSTED` | 409 | 重试 | 否 | attempt 数达到上限（§12.7） |
| `RESOURCE_EXHAUSTED` | 429 | 调度 | 是 | 并发槽位或配额不足 |
| `ARTIFACT_QUOTA_EXCEEDED` | 507 | 收集 | 否 | 单 attempt 产物超配额（§12.7） |
| `EXECUTOR_UNAVAILABLE` | 503 | 执行 | 是 | K8s API 不可达 |
| `IMAGE_PULL_FAILED` | 502 | 执行 | 是 | 镜像拉取失败 |
| `MOUNT_UNAVAILABLE` | 502 | 执行 | 否 | Pod 挂载失败（运行期观测，D-60） |
| `EXECUTOR_POD_ERROR` | 502 | 执行 | 是 | Pod 异常终止；`Unschedulable` 不算失败，按 §6.9 留在排队态 |
| `EXPORT_INVALID` | 500 | 收割 | 否 | 退出码 0 但 `exports.env` 解析失败（§5.12），runner 判失败 |
| `DEPLOY_FAILED` | 502 | deploy runner | 否 | Genet 调用失败（连接/5xx/同名冲突/安装失败）；`detail.reason` 保留 Genet 原文（D-47） |
| `DEPLOY_BLOCKED` | 502 | deploy runner | 是 | Genet `blocked` 或超部署窗口；`detail.reason ∈ {blocked,timeout,superseded,uninstalling,uninstalled}` |
| `ENDPOINT_UNREACHABLE` | 502 | pre_check | 是 | 网络不可达或连接被拒 |
| `PRECHECK_FAILED` | 502 | pre_check | 是 | 探测返回非预期状态 |
| `RUNNER_TIMEOUT` | 504 | 执行 | 按策略 | 超过 `timeout_seconds` |
| `RUNNER_NONZERO_EXIT` | 500 | 执行 | 否 | Runner 非零退出 |
| `SLA_PRECHECK_FAILED` | 500 | 执行 | 否 | Runner 自报：SLA 预检不达标 |
| `SLA_NO_FEASIBLE_CONCURRENCY` | 500 | 执行 | 否 | Runner 自报：搜索完并发区间仍无满足 SLA 的点 |
| `RESULT_MISSING` | 500 | 收集 | 否 | 退出码 0 但三选一产物全部缺失（test/pre_check） |
| `RESULT_INVALID` | 500 | 收集 | 否 | 产出的 `result.json` 不符合附录 A |
| `CLEANUP_FAILED` | 500 | 清理 | 是 | 拆除失败；不改变 Task 终态，进重放清单（§9.3） |
| `CANCELED` | 409 | 全阶段 | 否 | 操作对象已取消 |
| `INTERNAL_ERROR` | 500 | 全阶段 | 视情况 | 未归类错误，必须记录 trace |

**规则：** 只有 `retryable: true` 的错误才允许进入重试相关的自动化判断（v2 中仅用于 §9.3 重放与告警分级）；新增错误码必须同步更新本表。

---

## 附录 E：决策记录摘要

完整决策记录（背景、替代方案、后果）由团队另行维护，本表为索引。**D-01～D-65 为 v1 历史记录，保留 v1 术语；被 v2 取代的条目以 🗙 标注。**

| ID | 决策 | 影响段落 |
|---|---|---|
| D-01 | 文档从 PRD 更名为 ARCHITECTURE，仅承载设计约束 | §0 |
| D-02 | 删除 Gate：平台不判定指标是否达标 | §1、§7.4、§13.1 |
| D-03 | runner 粒度由提交者决定，多组合可合并 | §4.2 |
| D-04 | 不承诺数据级复现，只保证执行逻辑可追溯 | §5.7、§7.7 |
| D-05 | 必须提供 Web UI | §11 |
| D-06 | runner 状态机显式定义，SKIPPED 与 FAILED 严格分离 | §7.1 |
| D-07 | v1：Attempt 为 Task 级重试执行。**v2 修订：attempt 上移为 Task 级计数，runner 无独立 attempt（见 D-68）** | §7.2 |
| D-08 | pre_check 失败使用独立终态 `SERVICE_UNAVAILABLE` | §5.11、§7.5 |
| D-09 | 失败默认隔离，`abort_run` 必须显式声明。**v2 修订：枚举值更名 `abort_task`，传播到 clean 为止（见 D-69）** | §7.3 |
| D-10 | 资源路径对平台是不透明字符串 | §10.4 |
| D-11 | Executor 通过 Job Spec 解耦；平台只有 K8s 一种 Executor | §6.2、附录 B |
| D-12 | 错误码统一枚举 | 附录 D |
| D-13 | 状态必须落库，重启后对账恢复 | §12.5 |
| D-14 | 公共参数定义为保留字。**v2 修订：通道从 argv 改为环境变量 `AG_SERVED_*`（见 D-66）** | §5.3、§5.4.1、§9.1、附录 C |
| D-15 | Endpoint 收敛为平台内部对象，Runner 不可见 | §3、§9.1 |
| D-16 | 产物直接写入最终存储路径（subPath） | §6.8、§8.1、附录 B |
| D-17 | Runner 必须能脱离平台独立运行 | §5.3、§5.10 |
| D-18 | SSH / Docker 主机运行是平台外运行方式 | §6.2、§6.4、§13.1 |
| D-19 | K8s 全节点可访问同一 NFS，产物直落最终路径 | §6.8 |
| 🗙 D-20 | v1：参数以 argv 传递、无 `/workspace/input`。**v2 已取代（见 D-66）：业务输入走输入文件 + `AG_PARAMS_FILE`，argv 恒空** | §5.3、§6.8、附录 B |
| 🗙 D-21 | v1：平台不渲染、不校验参数（文档语义）。**v2 部分取代（见 D-67）：平台对 jsonl 做类型 + required 结构校验；范围/枚举/跨字段仍归 Runner；UI 仍不渲染表单** | §5.4、附录 C |
| D-22 | 任何运行环境都不 build | §5.5、§6.2、附录 C |
| D-23 | Pre-Check 不是独立概念，是 `role: pre_check` 的普通 runner | §3、§5.11、§7.4、§10.1 |
| D-24 | 环境变量是提交粒度（v1 Run 级 → v2 Task 级），填一次全链生效 | §10.1、§12.4、附录 C |
| D-25 | 明文变量不强制入 Secret | §12.4 |
| D-26 | Deploy 是一等展示对象，日志按 Pod 采。**v2 修订：落盘位置移至 deploy runner 产物目录（见 D-71）** | §8.1、§8.6、§9.1 |
| D-27 | 公共参数取值点统一：deploy XOR service，互斥 | §5.4.1、§10.1 |
| D-28 | 调度可见性与看板：QUEUED 必须带原因 | §6.9、§11 |
| D-29 | 日志持续采集并直接落盘；允许 gap 不伪造连续 | §8.6、§9.2 |
| D-30 | 日志明文、平台不脱敏 | §8.6、§12.4 |
| D-31 | 部署排队是一等状态，窗口默认 72 小时。**v2 修订：可见性由 service-deploy 的 deploy-state.json 承载，无平台 DEPLOYING 阶段（见 D-70）** | §7.4、§9.1 |
| 🗙 D-32 | v1：Pre-Check 门控 + 其余 Task 按 endpoint_exclusive 并发。**v2 已取代（见 D-69）：Task 内 runner 严格串行；endpoint_exclusive 收窄为仅外接模式** | §6.7、§7.1 |
| D-33 | cleanup.policy 与产物保留期解耦。**v2 修订：cleanup.policy 取消（有部署必有 clean，finally）；保留期仍由平台配置统一决定** | §8.6、§10.1 |
| D-34 | 状态 token 小写 | §7.1、§8.4 |
| D-35 | pre_check 不可重试失败归 failed（v1 COMPLETED_WITH_ERRORS 语义并入 v2 failed） | §7.5 |
| D-36 | 可追溯性以"引用 + 实际 digest"为准 | §5.7、附录 B |
| D-37 | 不做提交幂等 | §9.4、附录 D |
| D-38 | Runner 只做镜像内发现，不做运行时注册 | §5.6 |
| D-39 | 对外错误体是扁平的 | 附录 D |
| D-40 | 提交 Manifest 的唯一形态是嵌套 `task:`（v1 为 `run:`） | §10.2、views/07 |
| D-41 | 产物 PVC 名统一（v1 `testplane-storage` → v2 `argus-storage`） | §6.8、附录 B |
| D-42 | 开发/验收环境种子由进程启动补齐（`cluster-01` + `k8s-npu`） | views/03 |
| D-43 | MVP 执行环境 = 宿主 Kind + `kind load docker-image`；Genet 由本地假 Genet（genet-local）顶替 | §9.1、impl/execution |
| D-44 | Genet 部署单元是 Helm release；`releaseName = "arg-" + sha256(task_id:attempt)[:12]`（v1 前缀 `tp-`），必须 `noWait=true` | §9.1、impl/execution |
| D-45 | `endpoint.status=ready` 只代表地址可解析，必须再过 pre_check 服务门 | §7.4、§9.1 |
| D-46 | 部署排队不承诺位次，只有水位 | §7.4、§9.1 |
| D-47 | Genet 失败终态折叠为 `blocked` | §7.5、§9.1 |
| D-48 | Genet 身份两类：控制面用 Open API Key，Web 用统一身份源 OIDC | §12.1、§12.2 |
| D-49 | Volcano 队列直接读 CRD，不经 Genet；轮询 + 15s TTL | §6.9 |
| D-50 | Genet 取消/卸载：先落意图、后置删除、对账补偿 | §7.6、§9.1 |
| D-51 | 纯内网部署 + 共用企业统一身份源（Argus 独立 OIDC client） | §12.1 |
| D-52 | Genet 独占池语义是部署前提（Argus 独占自己的 namespace） | §9.1、views/06 |
| D-53 | Genet openAPI 统一走 `/api/open/*`（Bearer Open API Key） | §9.1 |
| D-54 | 终态后的卸载只走 Genet OpenAPI DELETE；判干净 = GET 404 + pods 空。**v2 修订：卸载动作由 service-clean runner 执行（见 D-70）** | §7.6、§9.1 |
| D-55 | secret rotate 的 resourceNames 只读窄例外 | views/06、views/07 |
| D-56 | 产物取用唯一真源是 PVC 文件系统；`GET /tasks/{id}/bundle` 打包下载；S3 兼容存储为 P1 可选 | §8.2、views/07 |
| D-57 | 提交下限：`runners[]` 至少 1 条 | §10.1 |
| D-58 | `task.name` 上限 200 字符 | §10.1 |
| D-59 | Pod 日志全部落盘，落盘副本是唯一真源 | §8.6、§9.2 |
| D-60 | Runner 同名多版本与挂载声明口径（`GET /runners` 默认回最新版本等） | §5.6.1、§10.4 |
| D-61 | 列表分页：UI offset、agent/CLI cursor，`/tasks/facets` 列筛选 | views/07 |
| D-62 | 概览与 Task 列表合并；`/tasks` 退化为兼容重定向 | §11.2 |
| D-63 | Runner 统一共享挂载：全局默认 + Profile 可关；载体只用 PVC | §10.4 |
| D-64 | 结果/报告页信息分工；产物按来源分组默认折叠 | §11.2 |
| D-65 | 平台级统一共享挂载升级为运行时可管理资源（`shared_mounts` 表 + 管理面端点） | §10.4、§12.5 |

**v2 新增裁决（2026-11-10，Argus 更名与 Runner 化改造）：**

| ID | 决策 | 影响段落 |
|---|---|---|
| **D-66** | **平台更名 Argus，术语全面对调**（§0.2 映射表）：Run→Task、Task→runner、Tester→Runner、`TP_*`→`AG_*`、`tpctl`→`argus`、`testplaned`→`argusd`、`testplane-storage`→`argus-storage`、registry namespace→`argus`、K8s 标签 `argus.io/*`、Genet releaseName 前缀 `arg-`。取代 D-20 / D-14 的 argv 通道：**业务输入走输入文件**（`/workspace/input/params.jsonl|yaml`，只读挂载）+ `AG_PARAMS_FILE`，entry argv 恒空；公共参数以 `AG_SERVED_*` 环境变量注入 | 全文 |
| **D-67** | **平台对 jsonl 输入做结构级校验**（类型匹配 + required 存在性，报错带行号+字段名），部分取代 D-21；`input_format: jsonl|yaml|text` 声明校验深度，yaml/text 仅语法校验、语义归 Runner；`placeholder` 必须与 `input_format` 同格式 | §5.4、附录 C |
| **D-68** | **attempt 上移为 Task 级计数，runner 无独立 attempt**：runner 执行记录由 `(task_id, attempt_no, seq)` 定位；产物路径 `attempts/<n>/runners/<seq>/`；取代 D-07 的 per-runner Attempt 模型 | §7.2、§8.1、附录 A/B |
| **D-69** | **runner 级平台重试取消；Task 内 runner 严格串行（线性管道）**：v1 `retry.max_attempts`/`retry_on` 取消；重试 = 整 Task 新 attempt 全量重跑**含 fresh deploy**（不得复用上次服务，warm cache 污染测量）；取代 D-32 的门控+并发模型；`endpoint_exclusive` 收窄为仅外接模式有效；`on_failure` 枚举 `abort_run`→`abort_task`，传播到 clean 为止 | §6.7、§7.1、§7.3、§7.8 |
| **D-70** | **deploy/clean 以内置 runner 承载**：service-deploy（role: deploy）与 service-clean（role: clean）由平台按 `deploy` 字段自动配对注入（用户不手动添加，不可注册）；平台 Deploy/Cleanup 阶段与 `cleanup.policy` 取消；clean 为 finally 语义（成功/失败/取消/超时全路径执行，幂等，失败只打 `cleanup_failed`）；平台不再直连 Genet，只读 runner 产出的 `deploy-state.json` 与 exports；部署排队可见性由 deploy-state.json 承载（D-31/D-46/D-47 的语义保留、载体变更）；卸载动作移入 service-clean（D-54 语义保留） | §7.4、§7.6、§9.1、§10.1 |
| **D-71** | **exports 机制**：runner 向 `$AG_OUTPUT_DIR/exports.env` 写 KV，平台词法收割（失败→`EXPORT_INVALID`）、随 attempt 落库落档、按管道序合并注入下游、组合期禁止重复 key 并校验 `consumes` 闭合；静态 service 值视为"序号 0 的导出"；凭据禁止入 exports | §5.12、§10.1 |
| **D-72** | **泄漏兜底 = 重放内置 clean**：`argus clean --task <id> [--attempt <n>]` 用落档导出重拉 service-clean Job；不做平台 GC；**硬规则：禁止对 Genet 资源设置指向 runner Job 的 ownerReferences**（进程生命周期 ≠ 资源生命周期）；runner 一律不绑 K8s ServiceAccount，Genet 凭据走 Task 级 secret | §9.3、§12.2 |
| **D-73** | **API 与契约同步更名**：路径 `/runs`→`/tasks`、`/testers`→`/runners`、`/runs/{id}/tasks/{tid}`→`/tasks/{id}/runners/{seq}`；`POST /tasks/{id}/retry` 语义改为整 Task 新 attempt（上限 `ATTEMPT_EXHAUSTED`）；附录 A/B 升 schema `2.0`（`result.json` 字段 `task_id/attempt_no/runner_seq/runner{}`；Job Spec `args` 恒空、env 全 `AG_*`、mounts 增加 input）；附录 D 错误码 `TESTER_*`→`RUNNER_*`、`TASK_TIMEOUT`→`RUNNER_TIMEOUT`、`RUN_TOO_LARGE`→`TASK_TOO_LARGE`，新增 `EXPORT_INVALID`/`ATTEMPT_EXHAUSTED`；Task 终态取消 `COMPLETED_WITH_ERRORS`（并入 `failed`） | views/07、openapi.yaml、附录 A/D |
| **D-74** | **产物契约改为三选一**：test / pre_check 的 runner 产出 `result.json` / `result.html` / `result.csv` **至少其一**，由 Runner 决定，**不强求 `result.json`**（取代 v1 §5.3 与本版初稿的"result.json 必产"）；`RESULT_MISSING` 重定义为"三选一全部缺失"，`RESULT_INVALID` 仅在 `result.json` 产出但不合法时适用；`result.html` / `result.csv` 为前端通用展示（iframe / 通用表格），平台不解析内容；失败时推荐仍产出 `result.json`（带 error）但为软要求 | §4.1、§5.3、§5.9、§7.1、§8.2、§8.5、附录 A/D |

