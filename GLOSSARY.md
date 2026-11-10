# Argus 术语表（GLOSSARY）

> 本文件是全仓库**唯一**的术语表（single-context 约定，见 [`AGENTS.md`](AGENTS.md)）。v2.0（Argus 更名与 Runner 化改造）的术语以本文与 [`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) §0.2 为准；两者冲突时以 ARCHITECTURE 为准并回写本文。本文只做定义与消歧，**不新增裁决**。
> 文中出现 v1 术语（Run、Task 指执行实例、Tester、`TP_*`）即为历史语境：ARCHITECTURE 附录 E 的 D-01～D-65 决策记录保留 v1 术语。

## 1. 核心术语

| 术语 | 定义 |
|---|---|
| **Argus** | 平台名（原 TestPlane）。云原生、轻量化、AI Agent 友好的**模型评测流水线平台**；角色是 Runner 编排器——按序拉起 runner Job、收割导出、注入下游、收集产物、记录事实 |
| **Task** | 一次评测任务（v1 的 Run）。一次完整管道执行；**最小的重试与编排单位**，稳定标识为 `task_id`；终态四种：`succeeded` / `failed` / `service_unavailable` / `canceled` |
| **runner（实例）** | Task 内严格串行的一个执行步骤（v1 的 Task）。最小的调度、状态和报告单元；没有自己的重试。执行记录由 **`(task_id, attempt_no, seq)`** 三元组定位 |
| **Runner（定义）** | 独立、可执行、可复用的**定义单元**（v1 的 Tester）：`runner.yaml` + 镜像 + entry。平台唯一感知"这一步怎么做"的边界 |
| **attempt** | Task 的第 N 次执行（**Task 级计数**）。v2 将 v1 的 per-runner Attempt 上移：runner 没有自己的 attempt；重试 = attempt+1，**整 Task 全量重跑含 fresh deploy**（不得复用上一次已部署服务，warm cache 污染测量） |
| **role** | Runner 的角色，四种取值：`pre_check`（前置门，可注册）/ `test`（正式测试，可注册）/ `deploy`（部署被测服务，**仅内置**）/ `clean`（拆除部署，**仅内置**）。`role: deploy` / `role: clean` 的用户注册一律拒绝 |
| **Entry** | runner 的唯一执行入口，由 `runner.yaml.entry` 以 **exec form 数组**声明（如 `["/runner/run.sh"]`），文件名不限；平台 exec 该数组，不经 shell、不追加任何动态 argv（argv 恒空） |
| **线性管道** | Task 内 runner **严格串行**：`[service-deploy] → [pre_check…] → [test…] → [service-clean]`。**没有** `DEPLOYING` / `CLEANUP` / `PRECHECK` 平台阶段——部署就是管道第一个 runner 在跑，拆除就是最后一个；隔离由拓扑（Task 私有服务 + 串行）保证 |
| **service-deploy** | 内置 Runner（role: deploy），平台按提交配置自动前置（用户不手动添加）。调 Genet 拉起被测服务、周期改写 `deploy-state.json`，ready 后导出 `AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH` / `AG_DEPLOY_REF` |
| **service-clean** | 内置 Runner（role: clean），与 deploy 自动配对追加。消费 `AG_DEPLOY_REF` 调 Genet **幂等**拆除（资源已不存在 = 成功） |
| **clean 的 finally 语义** | service-clean 在**成功、失败、取消、超时**四条路径全部执行；失败只打 `cleanup_failed` 不改 Task 终态。唯一不执行的路径是平台崩溃 / 节点丢失 → 用落档导出**重放**兜底：`argus clean --task <id> [--attempt <n>]`（不做平台 GC） |
| **deploy-state.json** | service-deploy 周期改写的部署状态文件（`phase ∈ queued/accepted/deploying/ready/blocked`、水位、已等待时长、Pod 清单、导出值副本）。**部署排队可见性**由它承载（平台无 `DEPLOYING` 阶段）；平台归档，是泄漏重放的输入 |
| **Endpoint** | 平台侧被测服务描述（地址、协议、端口、模型名）。只来自两处：service-deploy 的 exports，或提交时静态 `service` 块；只用于编排判定与公共参数投影，**Runner 不可见**（Runner 只认 `AG_SERVED_*`） |
| **Genet** | 外部服务部署平台（部署单元是 Helm release）。service-deploy / service-clean 的后端：集群资源的创建、就绪判定与拆除都由 Genet 执行；平台不直连 Genet（交互由内置 runner 执行），runner 不直连集群 |
| **exports** | Task 内的信息传递机制（v2 新增）：runner 向 `$AG_OUTPUT_DIR/exports.env` 写 KV 行（`KEY=VALUE`，`#` 注释），平台**词法收割**（失败 → `EXPORT_INVALID`）、随 attempt 落库落档、按管道序合并注入下游；组合期校验 `exports` / `consumes` 声明闭合、禁止重复 key；凭据禁止入 exports |
| **产物契约（三选一）** | test / pre_check 的 runner 产出 `result.json` / `result.html` / `result.csv` **至少其一**（D-74，由 Runner 决定，**不强求 `result.json`**）；`result.json` 为推荐的机器可读形态（附录 A，schema 2.0），`result.html` / `result.csv` 由前端通用展示（iframe / 通用表格）、平台不解析内容；`RESULT_MISSING` = 三选一全部缺失，`RESULT_INVALID` 仅在 `result.json` 产出但不合法时适用；失败时推荐仍产出 `result.json`（带 error）但为软要求 |
| **输入文件** | 业务输入的唯一通道（v2 取代 v1 的 argv）：提交的输入原文经词法 / 结构校验后落盘 `/workspace/input/params.jsonl`（或 `params.yaml`，只读），注入 `AG_PARAMS_FILE` 告知路径；entry 读取优先级 `--params-file` > `AG_PARAMS_FILE` > 约定路径 > 报错退出（码 2）；不支持 stdin |
| **input_format** | `runner.yaml` 声明的输入语法：`jsonl`（默认，平台做语法 + 结构校验：类型 + required，报错带行号 + 字段名）/ `yaml`（语法校验，语义归 Runner，如部署描述）/ `text`（不校验）。`yaml` / `text` 时 `parameters` 必须为 `[]` |
| **公共参数** | 三个**保留字环境变量**：`AG_SERVED_URL`（OpenAI 兼容 base URL）/ `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH`。来源唯一（deploy 导出 XOR 静态 `service` 块，两者互斥，都填或都不填拒绝提交）；Runner 不声明对 Endpoint 的依赖（依赖表达用 `consumes`） |
| **Manifest** | 提交体：嵌套 `task:` 结构 + `runners[]`（`input` 为输入原文）；`deploy` 与 `service` 二选一；`precheck: auto` 注入第一个测试类 runner；**没有** `cleanup.policy`（已取消）与 `runners[].retry`（runner 级重试已取消） |
| **Runner Registry** | Runner 注册表：`runners/*/` 白名单目录发现 + 注册校验 + `content_sha` 指纹 + `name@version` 不可静默覆盖；`deploy` / `clean` 角色拒绝注册；对外只读目录 API |
| **Executor** | 平台唯一执行后端：K8s Job（每 runner 一个，幂等键 `(task_id, attempt, seq)`）。SSH / Docker 主机运行是平台外运行方式，不是 Executor |
| **Execution Profile** | 执行环境配置：引用已登记的 K8s 集群 / 命名空间与挂载；Runner 不绑定具体集群 |
| **独立运行** | `role: test` / `pre_check` 的 Runner 必须能脱离 Argus 运行：只给输入文件 + 环境变量就产出与平台内一致的产物；验收 = `docker run <image> <entry> --params-file … --url …` 跑通 |

## 2. v1 → v2 术语映射表

抄自 `design/ARCHITECTURE.md` §0.2：

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

补充映射（出自 ARCHITECTURE D-66 / D-73 与附录 D）：

| 项 | v1 | v2 |
|---|---|---|
| API 路径 | `/runs`、`/testers`、`/runs/{id}/tasks/{tid}` | `/tasks`、`/runners`、`/tasks/{id}/runners/{seq}` |
| 错误码 | `TESTER_*`、`TASK_TIMEOUT`、`RUN_TOO_LARGE` | `RUNNER_*`、`RUNNER_TIMEOUT`、`TASK_TOO_LARGE`；新增 `EXPORT_INVALID` / `ATTEMPT_EXHAUSTED` |
| Task 终态 | 含 `completed_with_errors` | `completed_with_errors` 并入 `failed`；无 `DEPLOYING` / `CLEANUP` / `PRECHECK` 平台阶段 |
| runner 级重试 | `retry.max_attempts` / `retry_on` 白名单 | **取消**；重试 = 整 Task 新 attempt（`POST /tasks/{id}/retry`，上限 `ATTEMPT_EXHAUSTED`） |
| 镜像仓库 / K8s 标签 / Genet release 前缀 | `…/testplane/`、`testplane.io/*`、`tp-` | `registry.dev.huawei.com/argus/…`、`argus.io/*`、`arg-` |

## 3. 命名消歧约定

- **定义层书面用大写 Runner**：Runner Registry、"注册 Runner"、"Runner 契约"——指可注册、可复用的定义单元。
- **实例层用小写 runner**：Task 内 runner、"runner 实例"——指管道中的一次执行步骤；易混处写"runner 实例"或"Task 内 runner"。attempt、endpoint 同理用小写。
- **Task 恒大写**：一次评测任务的概念在书面语中始终写 Task，与小写 runner 相区分。
- **代码与库表对应**（实现命名以本条为准）：
  - 定义：类型 / 结构 `Runner`，表 `runners`，目录 `runners/`，文件 `runner.yaml`；
  - 实例：类型 / 结构 `TaskRunner`，表 `task_runners`；
  - 一次评测：类型 / 结构 `Task`，表 `tasks`，API `/tasks`。
- **token 口径**：环境变量、错误码、标签用大写（`AG_*`、`RUNNER_TIMEOUT`、`argus.io/*`）；库与 API 中的状态 token 统一小写（`succeeded` / `failed` / `skipped`），正文讲概念可用大写（ARCHITECTURE §7.1，D-34 沿用）。

## 4. 使用规约

- 新文档、新代码一律用 v2 术语；引用 v1 历史决策（ARCHITECTURE 附录 E D-01～D-65）时保留 v1 术语并注明"（v1）"或"（v2 已取代）"。
- 评审用说明文（`docs/design/overall-design.md`、`docs/design/runner-design.md`）文首注明"对应 ARCHITECTURE v2.0"。
- 发现术语与 `design/ARCHITECTURE.md` 冲突时，以 ARCHITECTURE 为准并回写本文与 ARCHITECTURE §0.6 变更记录，不做静默修改。
