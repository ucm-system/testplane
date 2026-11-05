# TestPlane 实现技术方案（P0）

> **文档类型：** 实现方案（Implementation Plan）
> **文档效力：** 本文不新增设计裁决。凡与 [`ARCHITECTURE.md`](ARCHITECTURE.md) 冲突，以 ARCHITECTURE 为准；实现中发现文档有缺口或自相矛盾，记入本文 §12，按 ARCHITECTURE §15 先改文档再改代码，不做"代码先行"。
> **变更规则：** 本文随实现演进而改，不涉及契约字段，不需要升 `schema_version`。
> **适用范围：** P0（ARCHITECTURE §14.1 的 17 项验收）。P1 只在 §13 预留扩展点，不提前实现。

---

## 1. 读法与边界

### 1.1 三份文档的分工

| 层次 | 文档 | 回答什么 | 效力 |
|---|---|---|---|
| 设计裁决 | [`ARCHITECTURE.md`](ARCHITECTURE.md) | 系统边界、契约、语义：必须 / 不得 / 应该 | 冲突时以此为准 |
| 实现方案 | 本文 + 三份分册 | 用什么技术、怎么分层、数据怎么存、流程怎么跑、如何验收 | 实现层取舍，可改 |
| 现场事实 | 代码、`schemas/`、`api/openapi.yaml`、测试 | 唯一可执行的真相 | 与文档不一致即为缺陷 |

本文只写"怎么落 ARCHITECTURE 的裁决"，不复述裁决本身。

### 1.2 分册索引

| 文档 | 覆盖 |
|---|---|
| [`impl/control-plane.md`](impl/control-plane.md) | 数据模型、状态机、调度与并发、编排流程、API 契约、SSE、认证授权、配置 |
| [`impl/execution.md`](impl/execution.md) | Job 渲染、挂载、生命周期观测、超时取消、清理、日志采集、产物校验、Genet 适配、Registry、Volcano 可见性 |
| [`impl/web-and-delivery.md`](impl/web-and-delivery.md) | Web UI 落地、样式复用、构建集成、镜像与 CI、Helm、可观测性、测试策略、环境与灰度、P1 扩展点 |
| [`views/`](views/) | **4+1 设计视图**：用例 / 逻辑（状态机与接口）/ 数据表 / 进程时序 / 开发视图 / 部署视图 / API 契约（开发基线） |

### 1.3 本文不含

- 需求论证、用户画像、排期与人力（ARCHITECTURE §0.2 已排除）。
- Tester 内部如何调 AISBench / EvalScope（§5.2）。
- Genet 内部如何渲染 Helm（§9.1）。
- AISBench / EvalScope 等工具自身的能力边界。

### 1.4 编号约定

`§x.y` = ARCHITECTURE 章节（附录 E 的 `D-01…D-29` 是**设计裁决编号**）；`I-x` = 本文；`C-x` / `E-x` / `W-x` / `D-x` = 三份分册的章节（分册的 `D-x` 是"交付"章节，与附录 E 的 `D-29` 不同源，引用时会写全文档名）；`K-x` = 本文 §11 的待拍板决策；`G-x` = 本文 §12 的文档缺口。

---

## 2. 一句话方案

**一个 Go 二进制（api / controller / collector 三种角色）+ PostgreSQL 唯一权威状态 + NFS 产物直落 + K8s Job 唯一执行后端 + React SPA。控制面不留内存状态，所有推进都由"数据库事务 + 事件 + 对账"驱动。**

```text
                      ┌──────────────────────────────┐
  Web UI / CLI(P1)    │  api：REST + SSE，无状态      │
  Jenkins / Agent ───►│  只做鉴权、校验、读写投影      │
                      └──────────────┬───────────────┘
                                     │
                 ┌───────────────────┼────────────────────┐
                 ▼                   ▼                    ▼
        ┌────────────────┐   ┌──────────────┐    ┌─────────────────┐
        │  PostgreSQL    │   │  controller  │    │  collector      │
        │  状态/事件/注册 │◄─►│  调度+编排+对账│    │  Pod 日志跟随    │
        └────────────────┘   └──────┬───────┘    └────────┬────────┘
                                     │ K8s API              │ K8s logs API
                                     ▼                     ▼
                              ┌─────────────┐      ┌──────────────┐
                              │ K8s Job     │      │ deploy Pods  │
                              │ (Tester)    │      │ (Genet 部署) │
                              └──────┬──────┘      └──────┬───────┘
                                     │ subPath 挂载          │
                                     ▼                     ▼
                              ┌──────────────────────────────────┐
                              │ NFS <storage-root>（RWX，最终位置）│
                              └──────────────────────────────────┘
```

一次 Run 的主干（详见 C-5）：

```text
提交 → 校验 → SUBMITTED → QUEUED
   → [有 deploy] DEPLOYING（Genet 排队/部署，最长 72h 可配）→ 拿权威 Endpoint
   → TESTING：pre_check（平台注入）通过后，其余 Task 一起入池（非独占可并行，独占排队）
        每个 Task：QUEUED（端点槽位）→ PREPARING（创建 Job）→ RUNNING（Pod）
                  → 终态（校验 result.json / 收集日志 / 写 Task report）
   → CLEANUP（按 cleanup.policy 卸载服务、收尾日志）
   → 终态 SUCCEEDED / COMPLETED_WITH_ERRORS / SERVICE_UNAVAILABLE / FAILED
```

三条贯穿全局的约束，后面每个模块都受它约束：

1. **状态只能经状态机写**：只有 `store.Transition*` 能改状态，且与业务写在同一事务（§12.5 No Hidden State）。
2. **产物不搬运**：容器 `/workspace/output` 直接是最终存储路径（§6.8），平台只校验与索引。
3. **平台不做判断**：不校验 Tester 参数、不聚合指标、不判定达标（§13.1）。

---

## 3. 技术选型

### 3.1 总表

| 层 | 选型 | 关键理由 |
|---|---|---|
| 控制面语言 | **Go 1.23+** | client-go informer / workqueue、静态二进制、并发模型；K8s 是第一公民 |
| HTTP 框架 | `chi` + 中间件 | 轻、贴近 net/http；不引重量级框架 |
| 契约 | **OpenAPI 3.1 spec-first**，`oapi-codegen` 生成 server 接口与类型 | 与 "Contract Driven"、API First 一致；错误码与附录 D 同源 |
| 存储 | **PostgreSQL 16**，`pgx/v5` + `sqlc` + `golang-migrate` | §12.5 已裁决；显式 SQL 便于控制事务与锁 |
| K8s | `client-go`（typed + dynamic） | Job/Pod/Lease 用 typed；Volcano CRD 用 dynamic |
| 日志 | `log/slog`（JSON） | 无额外依赖；字段含 run/task/attempt |
| 指标 | `prometheus/client_golang` | §12.6 要求 `/metrics` |
| 认证 | `coreos/go-oidc` + 服务令牌（JWT/JWKS） | §12.1 复用统一身份源 |
| 前端 | **React 19 + TypeScript + Vite 7 + TanStack Router + TanStack Query** | SPA 直连 REST（§11.4）；路由与数据缓存清晰 |
| 前端样式 | **Tailwind CSS v4 + shadcn/ui**（CSS 变量主题） | 决策 K-2：原型只作风格参考；主题变量映射原型色板，组件用 shadcn |
| 测试 | Go `test` + testcontainers(Postgres) + kind + k6 + vitest + Playwright | 分层覆盖见 web-and-delivery §13 |
| 打包 | 多阶段构建 → distroless；前端 `go:embed` 进同一镜像 | 单镜像单进程，轻量 |

### 3.2 为什么是 Go（备选与反例）

| 维度 | Go | Python / FastAPI | 结论 |
|---|---|---|---|
| K8s 集成 | client-go informer/reflector、Lease、pod log follow 原生 | 官方 client 可用，informer/断线重连需自建 | Go 明显占优 |
| 并发模型 | 每 Job 一 goroutine，数千并发无压力 | asyncio 可用，但 CPU 密集与多进程部署更复杂 | Go |
| 部署体积 | 静态二进制 ~30–50 MB，distroless | 需解释器 + 依赖层，镜像 300 MB+ | Go |
| 契约生成 | OpenAPI 双向生成成熟 | 同样成熟 | 平 |
| 团队栈 | 若团队以 Python 为主，学习成本存在 | 若团队已有测试工具链是 Python，复用便利 | **唯一需要拍板的点（§11 K-1）** |

**结论：** 控制面用 Go；Tester 继续各自选语言，两者不共享运行时。若团队明确要求 Python，替换成本集中在 `store/executor/informer`，数据模型、API、契约、状态机设计全部不变。

### 3.3 明确不引入的东西

| 不引入 | 理由 |
|---|---|
| Redis / Kafka / 消息队列 | 状态必须落库（§12.5）；任务量级（每日百级 Run）用 DB 队列足够；引入即第二个状态源与新的运维面 |
| 独立 BFF / 后端聚合层 | §11.4 明确禁止；UI 直连 REST |
| sidecar 采集日志 | 平台无权改 Genet 的 Deployment（§8.6）；且 sidecar 随 Pod 消失，正是要避免的 |
| 通用 ORM | 状态机的 CAS、行锁、`SKIP LOCKED` 需要显式 SQL；`sqlc` 生成静态查询，类型安全且可读 |
| K8s Operator / CRD 作为权威状态 | 会让状态出现两个权威源：CRD 与 PG。P0 用普通 Job + 内部状态机，CRD 只读 |
| 自建 Prometheus / Loki | §6.9、§8.6、§12.6 均明确不新增监控组件；指标转发给既有系统是另一件事 |
| 运行时上传 Tester / 运行时 build | §5.6、§6.2、§6.4 明确禁止；只认 CI 产出的镜像 |

### 3.4 选型风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| Go 团队不熟 | 交付变慢 | M0 先钉骨架与接口；复杂点集中在 executor，可单独结对 |
| 前端自研组件工作量 | UI 比预期慢 | 用 shadcn/ui 现成组件 + Tailwind 主题映射原型色板；只对日志/链路图这类专用视图自研 |
| OpenAPI 生成产物臃肿 | 编译与审查成本 | 按 tag 分文件生成；生成物不进 code review |
| PG 作为队列的争用 | 大 Run 抢锁 | 短事务 + `SKIP LOCKED`；并发由端点槽位与全局 permit 限流；压测在 M6 |

---

## 4. 运行时形态

### 4.1 三种角色，一个镜像

| 角色 | 职责 | 状态 |
|---|---|---|
| `api` | REST、SSE、鉴权、校验、读投影 | 无状态，可水平扩 |
| `controller` | 调度、编排、状态机推进、对账、报告生成、日志文件写入 | 单实例（Lease 选主） |
| `collector` | Tester 与 Deploy Pod 的日志跟随 | 可多副本：全局 Lease 选主 + 每个 Job 一个 Lease 分片（E-7.3） |

启动：`testplaned --role=api|controller|collector|all`。P0 默认 `all`（单 Pod 跑三种角色），部署时可用同一镜像拆成三个 Deployment 独立扩缩。角色拆分不需要改代码，只改启动参数与副本数。

**P0 部署形态：** 1 副本 `all`（或 controller+collector 1 副本 + api 2 副本）。HA 不追求多主：controller 与 collector 通过 K8s `Lease` 选主，租约 15s、续约 5s；leader 失效后新 leader 从 DB 重建状态并进入对账（C-3.5）。

### 4.2 启动与关闭顺序

```text
启动：加载配置 → 校验（DB/存储可写/K8s 可达）→ 迁移版本检查
     → controller：Lease 抢主 → 加载注册表（扫描由 admin 触发，启动不自动改库）→ 启动 informer → 启动调度/对账/超时循环
     → collector：Lease 抢主 → 恢复未完成 Run 的采集任务
     → api：监听（readyz = DB ping + 存储可写 + 迁移版本匹配；**不含 K8s API**——K8s 抖动不应摘掉仍能读历史报告的 api，§12.5 状态与产物分离）
关闭：摘流（/readyz 先失败）→ 停止接受新任务 → 等在途事务结束（≤10s）
     → 关闭事件写入器 → 退出。运行中的 Job 不删（由对账接管）
```

### 4.3 故障域与恢复

| 故障 | 表现 | 平台行为 |
|---|---|---|
| controller 崩溃 | 无推进 | Lease 过期后新 leader 接管；RUNNING/PREPARING 的 Attempt 全部进对账（C-3.5），不假定失败 |
| api 崩溃 | 请求 5xx/探针摘除 | 多副本自动摘流；SSE 客户端自动重连并按 `Last-Event-ID` 回放 |
| DB 不可用 | 读写失败 | `/readyz` 失败；controller 暂停推进；Job 继续跑，恢复后按状态对账 |
| K8s API 不可达 | 无法建/查 Job | Task 停在 PREPARING 或按 `retry_on` 判 `EXECUTOR_UNAVAILABLE`；不误判 Tester 失败 |
| NFS 不可写 | 产物写不进 | 创建 Attempt 前做可写探测；失败即 `EXECUTOR_UNAVAILABLE`（可重试）；Profile 挂载缺失才是 `MOUNT_UNAVAILABLE`（不可重试，E-3.4） |
| Genet 不可用 | 部署失败 | 归 `FAILED`（平台侧），与 `SERVICE_UNAVAILABLE`（受理但未就绪）严格区分 |
| collector 崩溃 | 日志断档 | 恢复后写 `log gap` 标记，允许丢日志、不允许伪造连续（§8.6） |

### 4.4 部署形态

Helm chart（`deploy/helm/testplane/`）：Deployment + Service + Ingress + ConfigMap/Secret + ServiceAccount/RBAC + PVC（RWX NFS）+ NetworkPolicy + PDB。PostgreSQL 作为外部依赖只给连接参数，chart 不内置数据库（K-9）。资源清单见 web-and-delivery §9。

---

## 5. 代码结构

```text
server/
├── cmd/testplaned/            # 唯一入口：角色由 --role 决定
├── cmd/tpctl/                 # P1：CLI（P0 不建）
├── api/openapi.yaml           # 契约源头（OpenAPI 3.1）
├── internal/
│   ├── api/                   # 路由、中间件、handler、SSE、错误映射
│   ├── domain/                # 纯类型与规则：状态枚举、转移表、校验规则（零 IO）
│   ├── store/                 # pgx + sqlc 生成代码、事务与锁、事件写入
│   ├── orchestrator/          # 提交校验、precheck 注入、Run 推进、abort、cleanup
│   ├── scheduler/             # 候选选取、端点槽位、全局并发、超时 watchdog
│   ├── executor/k8s/          # Job 渲染、informer、对账、取消与清理
│   ├── executor/storage/      # 目录预创建、subPath、配额统计
│   ├── executor/artifacts/    # result.json 校验、产物候选清单
│   ├── registry/              # testers/ 扫描、Schema 校验、版本不可覆盖
│   ├── genet/                 # Genet client + fake（开发与测试用）
│   ├── logcollect/            # Pod 日志跟随、分段、重启/gap 标记
│   ├── schedview/             # Volcano / Node 只读视图（waiting_reason 输入）
│   ├── report/                # Task/Run report、report.md 模板
│   ├── events/                # state_events → events.jsonl / SSE 广播
│   ├── auth/                  # OIDC、会话、RBAC、服务令牌
│   ├── copy/                  # 用户可见中文文案（状态/错误码），前端字体子集采集源
│   ├── webui/                 # go:embed 前端产物
│   ├── config/                # 配置结构与校验
│   └── metrics/               # Prometheus collector
├── migrations/                # golang-migrate，只加不删
└── schemas/                   # tester.schema.json / result.schema.json / manifest.schema.json
```

**依赖方向（单向，禁止反向）：**

```text
api → orchestrator / scheduler → domain / store
                        ↘ executor / genet / registry / report / events / logcollect
domain 不依赖任何包；api 不得直接调 executor；executor 不写 tasks 状态（只回写 attempts 的物理事实，由 controller 在事务内推进状态机）。
```

**判断归属的口诀：** 定义"测试怎么做"→ Tester；定义"在哪跑、怎么起停"→ executor；定义"谁在什么情况下变成什么"→ orchestrator + store；定义"能看到什么"→ api + web。

---

## 6. 模块与实现文档对应

| 模块 | 关键实现点 | 文档 |
|---|---|---|
| 状态机 | 8 态转移表、CAS、事件、重启恢复 | C-3 |
| 调度 | 端点槽位互斥、全局并发、Pre-Check 门控与并行、`waiting_reason` | C-4 |
| 编排 | 提交校验、precheck 注入、abort_run、cleanup | C-5 |
| 取消/超时/重试 | API 到 Job 删除的闭环、竞态 | C-6、E-5 |
| 报告与产物 | result.json 校验、report 生成、下载与日志读取 | C-7、E-8 |
| API | 端点表、词法切分、公共参数注入、错误码 | C-8 |
| SSE | 事件模型、回放、多副本广播 | C-9 |
| 认证授权 | OIDC、RBAC、审计 | C-10 |
| Executor | Job 渲染、informer、挂载、清理 | E-2~E-6 |
| 日志采集 | Tester 与 Deploy 两类日志、重启与缺口 | E-7 |
| Genet | 部署、状态、Endpoint、失败归类 | E-9 |
| Registry | 扫描、校验、不可覆盖、同步 | E-10 |
| 队列可见性 | Volcano 只读、排队原因 | E-11 |
| Web UI | 页面映射、SSE 合并、日志查看、提交页 | W-1~W-7 |
| 交付 | 镜像/CI/Helm/可观测性/测试/灰度 | web-and-delivery §8–§15 |

---

## 7. 数据与一致性总览

| 数据 | 存放 | 权威性 | 备注 |
|---|---|---|---|
| Run / Task / Attempt / 状态事件 | PostgreSQL | 唯一权威 | 状态机单写者，事件 append-only |
| Tester 注册信息 | PostgreSQL | 以库为准，来源是 `testers/` 目录 + admin 同步 | 版本不可覆盖（§5.7） |
| Execution Profile / Cluster | PostgreSQL | admin 维护 | 集群凭据只存 Secret 引用 |
| 报告与产物 | NFS `<storage-root>/` | 文件为准，DB 只存索引 | 容器直写最终路径（§6.8） |
| `events.jsonl` | NFS，Run 目录 | **派生**：库为准，文件可重建 | 单个 writer 追加 |
| Secret 明文 | 只在 Secret Store | 平台只存引用 | **平台不读明文**（K-5：日志不脱敏，无需反查替换） |
| 指标（result.json metrics） | 文件 | P0 不落库 | P1 若需跨 Run 查询再加索引表（§13） |

**一致性边界：** 一切都以 PG 事务为原子单位。事务提交后允许"文件还没写完"（report、jsonl），由 writer 幂等补齐并写 `state_events` 标记；反之不允许"文件有了、库里没有"。

---

## 8. P0 交付路线

按依赖排序，每个里程碑结束时都能演示，且不回退前面的验收。

| 里程碑 | 内容 | 演示 / DoD | 对应 §14.1 |
|---|---|---|---|
| **M0 骨架与契约** | 仓库布局、`api/openapi.yaml` 骨架、迁移、配置、`/healthz`、`/readyz`、CI（lint/test/build） | `make dev` 起服务 + 迁移成功；OpenAPI 可 lint | API 契约（部分） |
| **M1 提交与状态机** | Manifest 校验、Run/Task 落库、状态机与事件、SSE、Registry 同步、Tester 列表 | 提交 `service` 模式 Run；非法 Manifest 返回字段级错误；Task 停在 QUEUED（尚无 Executor）；SSE 能收到事件 | Run 提交、Task 状态机、Registry、API |
| **M2 Executor** | Job 渲染、informer、Attempt 生命周期、产物校验、超时与取消 | kind 里跑 `endpoint-probe` 对 mock OpenAI 服务：成功/失败/超时/取消四态可复现；删除 Job 后 Pod 消失 | K8s Executor、Tester 契约、产物持久化、超时取消 |
| **M3 Genet 与编排闭环** | Deploy/状态/Endpoint、precheck 自动注入、abort_run、cleanup、Run 终态、Task/Run report | mock Genet 全链路：`SUCCEEDED` / `COMPLETED_WITH_ERRORS` / `SERVICE_UNAVAILABLE` / `FAILED` 四终态各一例 | Genet、Pre-Check、失败策略、报告、失败隔离 |
| **M4 日志** | Tester stdout/stderr 采集、Deploy 多 Pod 采集、重启/gap 标记、200MB 分段、pods.json | 人为重启 Pod 与服务，日志中可见重启标记与分段；Run 结束后日志仍可读 | 产物与日志、Deploy 日志 |
| **M5 Web UI** | 全部 P0 页面：列表、详情（执行链路 + 事件）、报告、日志、提交（两形态）、Tester 目录、环境、取消/重试 | 对照 `design/prototype/` 逐页走查；所有操作均有对应 API（无 UI 独有逻辑） | Web UI、API First |
| **M6 加固与验收** | 对账与 HA 选主、配额、审计、指标、压测、故障注入、文档收尾 | §14.1 17 项逐条给出证据（§9）；压测 P95 < 300ms；杀 controller 后自动恢复 | 全部 |

**排期建议：** M0–M1 约 2 周，M2–M3 约 3 周，M4–M5 约 3 周，M6 约 1.5 周（按 2–3 人估算，仅用于讨论，不是承诺）。

---

## 9. 验收矩阵（§14.1 → 证据）

| # | 验收项 | 证据形态 | 里程碑 |
|---|---|---|---|
| 1 | Run 提交与完整状态链 | e2e 用例 `TestRunLifecycle_*`（成功 / 部分失败 / 服务不可用 / 取消） | M3 |
| 2 | Task 独立提交 | e2e：`POST /tasks`（`service` 模式）产物结构与 Run 内一致 | M3 |
| 3 | Genet 适配三项 | mock Genet 单测 + 契约测试；`blocked` 与调用失败分支各一例 | M3 |
| 4 | Pre-Check 注入与阻断 | e2e：`precheck: auto` 注入 Task#1；可重试失败 → 其余 SKIPPED + 终态 `SERVICE_UNAVAILABLE`；不可重试（401）→ `COMPLETED_WITH_ERRORS` | M3 |
| 5 | Registry 发现与校验 | 单测：非法 `tester.yaml` 字段级报错；`pre_check` 未声明 `abort_run` 被拒 | M1 |
| 6 | Tester 契约（入口/argv/输出/result.json） | golden 用例 + 真实镜像跑通 | M2 |
| 7 | Tester 独立运行 | 在无平台机器上 `docker run <image> --url=...` 产出合法 `result.json` | M2 |
| 8 | K8s Executor（超时/取消） | kind e2e：超时判 `TASK_TIMEOUT`；取消 30s 内 Job/Pod 消失 | M2 |
| 9 | 状态机与重启恢复 | 单测转移表全覆盖；e2e 杀 controller 后状态最终一致 | M2/M6 |
| 10 | Attempt 与重试 | 单测 + e2e：`retry_on` 命中才重试，历史 Attempt 不被覆盖 | M2 |
| 11 | 失败策略 | e2e：`abort_run` 在**多个 Task 并行**时终止其余未完成 Task（`RUNNING→CANCELED`、`PENDING/QUEUED→SKIPPED`） | M3 |
| 12 | Run 编排全链路 | M3 的四终态用例 | M3 |
| 13 | 报告结构 | JSON Schema 校验 report / result；`SKIPPED` 带 reason、`FAILED` 带错误码 | M3 |
| 14 | 产物持久化 | 删除 Pod 后仍可下载 `result.json` 与 `raw/` | M2 |
| 15 | 失败隔离 | 两 Task 的 Run，一个失败一个成功，终态 `COMPLETED_WITH_ERRORS` | M3 |
| 16 | Web UI | Playwright 冒烟：提交→详情→日志→取消；每操作对应 API | M5 |
| 17 | API 契约与错误码 | OpenAPI lint + 生成客户端编译 + 附录 D 全码有测试 | M1/M6 |

---

## 10. 风险清单

| 风险 | 概率/影响 | 处置 |
|---|---|---|
| **Genet 契约未定**（请求/响应/状态枚举/幂等/Pod 选择器） | 高/高 | 先出 adapter 接口 + Fake Genet，把契约谈判作为 M3 前置；不猜接口 |
| NFS UID 与目录预创建冲突（容器 UID 10001 vs NFS root squash） | 中/高 | M0 在真实环境验证；chart 提供 `runAsUser`/`fsGroup` 配置；文档化准入条件 |
| 日志量超预期（性能测试 Pod 日志大） | 中/中 | 200MB 分段 + 配额；采集背压时丢日志并标记 gap，绝不阻塞状态推进 |
| 单 controller 成为吞吐瓶颈 | 低/中 | 状态推进是毫秒级事务；M6 压测 20 并发 Task；必要时按 Run 分片（预留字段） |
| Web UI 范围蔓延（做成了 BI） | 中/中 | 严格按 §11.3 与非目标；看板 P1；不做图表库 |
| 文档与实现漂移 | 中/中 | PR 模板要求对应 §14.1 验收项；§12 的缺口先改文档 |
| OpenAPI 与实现不一致 | 中/低 | 生成类型 + 契约测试（web-and-delivery §13） |
| 多集群凭据管理成为安全弱点 | 低/高 | 每集群一个最小权限 kubeconfig Secret，admin 登记，轮换有审计 |

---

## 11. 已拍板决策（2026-11-04）

12 项实现层选择已由团队拍板。下表只记结论与落地影响；备选方案与代价见 git 历史。

| ID | 事项 | 决定 | 落地影响 |
|---|---|---|---|
| **K-1** | 控制面语言 | **Go** | 按本文 §3/§5 落地；Python 方案不再维护 |
| **K-2** | 前端样式 | **Tailwind CSS + shadcn/ui**；原型只作风格参考 | W 分册改写：组件树加 `components/ui`，主题变量映射原型色板；不再"原样引入原型 CSS" |
| **K-3** | Tester 发布 | **镜像内构建，不做运行时更新** | 删 `POST /admin/testers/sync`；启动时扫描镜像内 `/app/testers` 并校验；新 Tester = 新平台镜像，CI 先于运行时拦住坏 `tester.yaml` |
| **K-4** | 多集群凭据 | 同意：kubeconfig 由 Secret 挂载，DB 只存文件名/连通性 | 维持 C-2.3/E-10 的落法 |
| **K-5** | 日志脱敏 | **不脱敏，日志明文** | 移除 collector 读 Secret、RBAC `secrets get`、`redactions_total`；与 §12.4 冲突，见 G-9 |
| **K-6** | Deploy 超时 | **默认 72h（可配）**，必须支持 Genet 排队 | `TESTPLANE_DEPLOY_TIMEOUT=72h`；新增 `deploy.queued` 状态/排队时长展示与取消语义；见 G-4 |
| **K-7** | P0 副本数 | 单副本起步 | 维持 |
| **K-8** | 指标存储 | P0 不落库；**P1 可选 MongoDB** | 加 P1 扩展行；P0 仍读 `result.json` |
| **K-9** | PostgreSQL | 外部实例 | 维持 |
| **K-10** | `result.json` | **字段不限制，Tester 自维护**；技术上限只防 OOM | 允许未知字段；体积上限改为可配的 `TESTPLANE_RESULT_MAX_BYTES`（默认 100 MiB），超限仍 `RESULT_INVALID`；P1 可把指标写 MongoDB |
| **K-11** | Run 名称 | 不唯一；**对外一律用 id** | 维持；API/UI/报告引用全用 id |
| **K-12** | Run 内并行 | **平台提供能力，是否并行由 Tester 维护者决定**（`endpoint_exclusive` 即开关） | 调度器不做 Run 级串行/并行开关；独占声明决定排队，共享声明允许并行 |

### 11.1 决策带来的验收变化

- **Deploy 排队**（K-6）：Run 在 `DEPLOYING` 可能持续数小时到数天。P0 必须展示"排队中/部署中 + 已等待时长 + 可取消"（新增验收点，落 E-9/C-5.3）。
- **日志明文**（K-5）：接受"凭据可能出现在日志"的风险；Tester 契约里"不得主动打印凭据"保留，但平台不再兜底替换。
- **Tester 不动态更新**（K-3）：注册表只随平台镜像发布；`TESTPLANE_TESTERS_DIR` 只用于开发/灰度覆盖，不提供 API 触发同步。

---

## 12. 实现中发现的文档缺口与不一致

按 ARCHITECTURE §15，"认为文档有误，先改文档再改代码"。以下缺口已在 **ARCHITECTURE v0.3–v0.5** 回写一部分，保留本表作追溯与实现对照；标"待回写"的仍需下一版修订。

| ID | 位置 | 问题 | 实现默认 | 建议修订 |
|---|---|---|---|---|
| **G-1** | §7.1 图 vs §7.6 | 图中 `PENDING/QUEUED → CANCELED`，§7.6 正文写"标记 `SKIPPED`，reason: canceled" | 按正文：未开始的 Task 取消 = `SKIPPED(reason=canceled)`；`CANCELED` 只用于已拿到槽位/已启动的 | **已回写 v0.3**：图已删两条箭头 |
| **G-2** | §7.5 | 只写了"pre_check 失败且错误可重试"归 `SERVICE_UNAVAILABLE`，未写不可重试（4xx/认证）时归哪类终态 | 严格按文义：可重试失败 → `SERVICE_UNAVAILABLE`；不可重试 → `COMPLETED_WITH_ERRORS`（失败 Task 带错误码与 `retryable`） | **已回写 v0.3**：§7.5 补不可重试归属 |
| **G-3** | §7.4 / §6.7 | 未写明"Pre-Check 在前"与"非独占可并行"如何共存（谁先进、是否要串行） | **Pre-Check 门控 + 并行**：Pre-Check 通过后其余 Task 批量入池，不新增串行规则（K-12） | **已回写 v0.3**：§7.4 补顺序与并发说明 |
| **G-4** | §9.1 / §10.1 | Deploy 没有超时、排队状态与取消语义的定义；实际 Genet 部署要排队（数小时到数天） | `TESTPLANE_DEPLOY_TIMEOUT` 默认 **72h**、Profile 可覆盖；Genet 返回 `queued` 时 Run 保持 `DEPLOYING` 并展示排队时长，可取消；调用失败不自动重试（避免重复部署） | **部分回写 v0.3**：§7.4/§9.1 已补排队、窗口与取消；Manifest 仍不加入 `deploy.timeout_seconds`（走平台配置） |
| **G-5** | 附录 D | Pod 级失败（OOMKilled / Evicted / Unschedulable）没有专属错误码 | OOM/Evicted → `INTERNAL_ERROR`（`detail.k8s_reason` 保留原文）；**`Unschedulable` 不判失败**，保持 `pending` + `waiting_reason`，超时后按 `TASK_TIMEOUT` 收口（§6.9） | 视需要补 `EXECUTOR_POD_ERROR` / `CONTAINER_OOM_KILLED`；`RESOURCE_EXHAUSTED` 只用于提交/配额拒绝 |
| **G-6** | §5.4.1 第 4 条 | "子串粗筛"会把 `--url_extra=` 误判为 `--url` | 按**词法切分后的 token 精确匹配**（`--url=` 前缀或独立 token `--url`），错误码不变 | 把"子串粗筛"改为"token 精确匹配" |
| **G-7** | §12.7 | 单 Attempt 10 GiB 配额的执行方式未定义 | collector 每 60s `du` 扫描 + 超限即删 Job、判 `ARTIFACT_QUOTA_EXCEEDED` | 补充执行方式与判定时机 |
| **G-8** | §8.1 | `events.jsonl` 与库中事件的关系未定义 | 库为权威；`events.jsonl` 由单 writer 追加，重启后按 `seq` 幂等补齐 | 明确"派生文件、可重建" |
| **G-9** | §12.4 | §12.4 要求"平台对已知 secret 值做脱敏替换"，但团队已拍板 **日志明文、不脱敏**（K-5） | 按 K-5：平台不读 Secret 做替换，日志原样落盘；Tester 契约里的"不得主动打印凭据"保留 | **已回写 v0.3**：§12.4 改为"平台不做日志脱敏、Tester 不得主动打印"；安全风险由团队显式接受 |
| **G-10** | §6.5 | Profile 引用 `cluster`，但没有集群凭据的登记模型 | 新增 `clusters` 对象（api_server、kubeconfig 文件名、连通性状态），kubeconfig 由 Secret 挂载到 `CLUSTERS_DIR`（W-10），**不入库** | 属新增内部对象，不是契约字段，可在 ARCHITECTURE 补一段 |
| **G-11** | §10.1 `precheck: auto` | "用注入的 url/model_name 生成 args"与"公共参数统一末尾注入"存在歧义 | auto 时 pre_check Task 的 `args` 为空，公共参数按 §5.4.1 注入 | 明确写"args 为空，公共参数照常注入" |
| **G-12** | §8.4 | 状态字符串示例为小写，§7 全大写 | 库/API/报告统一**小写 token**（`pending`…），前端展示大写徽标 | **已回写 v0.3**：§7.1 加大写/小写约定 |
| **G-13** | §8.6 vs §10.1 | §8.6 写"`cleanup.policy` 决定产物保留多久"，但 §10.1 的取值 `always/on_failure/never` 是**卸载语义**，无法表达"保留多久" | `cleanup.policy` 只管被测服务是否卸载；产物/日志保留由平台保留期配置（默认 30 天）统一决定 | **已回写 v0.3**：§8.6 改为"policy 只决定卸载" |
| **G-14** | §11.4 | 只说"状态推送使用 SSE"，未定义事件类型与回放语义 | 事件类型与信封在 C-9 定义；SSE 支持 `Last-Event-ID` 回放，非日志通道 | 在 §11.4 补一句事件契约的位置 |
| **G-15** | §5.4 / 附录 C vs `testers/endpoint-probe/tester.yaml` | 该文件的 `placeholder` 是多行说明+注释，`parameters.url.description` 里还写"退出码 2"；而 §5.4 定义 `placeholder` 为"一段可直接抄的示例命令行"，§5.8 只有 0 / 非 0 两种退出语义 | 平台取 `placeholder` 首行作 UI 占位；退出码一律按 0 / 非 0 归类，忽略注释里的"2" | 修 `tester.yaml`：`placeholder` 收敛为一行命令，删掉退出码 2 的说法 |
| **G-16** | §5.7、§7.7 | 镜像引用可以是可变 tag（同一 `image:1.0` 背后可能是新代码），"镜像引用未变"不足以判定两次执行等价；ARCHITECTURE 未要求记录实际 digest | 执行层把 K8s `imageID` 解析为 `attempts.image_digest` 落库并写入报告；P1 续跑复用以 **digest** 为准（缺失时退回引用并给出警告） | 回写 §5.7/§7.7：可追溯性以"引用 + 实际 digest"为准；续跑复用优先比较 digest |

---

## 13. P1 扩展点（只预留，不实现）

| P1 能力 | 预留方式 |
|---|---|
| MCP Server | 复用同一 OpenAPI 生成的客户端；MCP 只是另一层 adapter，无独有逻辑 |
| CLI `tpctl` | 同上；命令与 API 一一对应 |
| 失败续跑（跨 Run） | `runs.retry_of` 字段已在 DDL 预留；复用校验条件在 C-5.6 定义 |
| Run 对比 | 读 `result.json` 的 UI 层能力（按指标名 + labels + workload 对齐）；P1 可依赖 MongoDB 指标集合 |
| 指标索引（跨 Run 查询） | P0 只存 `result.json` 文件；P1 可写 **MongoDB**（`metrics` 集合：run/task/attempt/name/statistic/unit/value/labels），查询接口不变；也可先加 PostgreSQL `metrics_index` 表过渡 |
| Matrix 展开 | 提交前的纯函数变换（`args` 展开为多个 Task），不改变调度与契约 |
| 看板 | `GET /queue-status` + Volcano 只读快照（E-11），前端独立路由 |

---

## 14. 文档维护

- 本文与三份分册随实现更新；契约字段变化一律先改 ARCHITECTURE（§15）。
- 每个实现 PR 在描述里写：对应 §14.1 的验收项、是否触及 §12 的缺口项、是否改 `api/openapi.yaml`。
- `schemas/` 与 `api/openapi.yaml` 是机器可校验的真相；文档与它们不一致时，先修文档再修代码。

---

## 15. MVP 实现状态与偏差（2026-11-05）

本节记录"按本文实现出来的 P0 MVP 到底做到了哪一步"，是**实现回写**，不是新需求。逐项验收与证据见 `docs/runbook/acceptance.md`，运行步骤见 `docs/runbook/local-kind.md`。

### 15.1 状态总览

| 能力 | MVP 状态 | 代码位置 |
|---|---|---|
| 三角色单二进制（`--role=api\|controller\|collector\|all`） | ✅ `api`/`controller`/`all` 可用；`collector` 未拆（日志由 executor 采、报告由终态 hook 写） | `server/cmd/testplaned` |
| 状态机内核（Run/Task/Attempt/Tester + Deploy 子状态） | ✅ 转移表 R1–R10 / T1–T11，`Kernel.Apply` 8 步、CAS、事件 seq | `server/internal/domain`、`server/internal/store` |
| 存储与迁移 | ✅ 11 张表 + 15 索引，启动自动迁移（`migrate up` 子命令亦可） | `server/migrations`、`server/internal/store` |
| REST + SSE（25 operations） | ✅ `/api/v1` 全量；SSE 断点续传 + `events/history` | `server/internal/api`、`server/api/openapi.yaml` |
| 调度（门控 / 独占 / 全局并发 / 公平性） | ✅ 门控与两类并发上限、`waiting_reason` 六值；公平性仅"按提交时间" | `server/internal/orchestrator` |
| K8s Job 执行器 | ✅ 真集群跑通（渲染/提交/观测/取消/日志/digest） | `server/internal/executor` |
| Deploy 阶段（Genet） | 🟡 **mock**；**适配设计已按 Genet 真实实现定稿**（Helm release 为部署单元、`noWait=true`、ready≠可用、位次不可得，见 D-44～D-48 与 `impl/execution.md §9`），实现待 P1 | `server/internal/genet/mocks` |
| Tester 注册（自发现） | ✅ 启动扫描 + 周期重扫 + 15 条校验 + 版本钉死 + orphan | `server/internal/registry` |
| 报告与产物 | ✅ `report.json` / `report.md` / `events.jsonl` / 产物索引，终态后异步写 | `server/internal/report` |
| Web UI | ✅ 6 路由（概览/列表/详情/提交/Tester/集群），SSE 实时 | `web/` |
| 部署与镜像 | ✅ Kind + kustomize（Component + overlay）+ 两镜像 | `deploy/` |
| Volcano / gang / 队列看板 | ⬜ 未做（`/queue-status` 报 `volcano_unavailable`） | — |
| OIDC / RBAC / CSRF | ⬜ 未做（仅 `TESTPLANE_AUTH_MODE=dev`） | — |
| 独立 Task（`POST /tasks`） | ⬜ 未做 | — |
| 失败续跑（`POST /runs/{id}/retry`）、`/report` 富指标 | ⬜ 未做（P1） | — |

### 15.2 P0 延后项（`07-api.md §5` 有、MVP 未实现）

| 端点 | 状态 | 理由 | 归属 |
|---|---|---|---|
| `GET /runs/{id}/artifacts` | 未实现 | `artifacts` 表有行，但无路由；产物只能按 Task/Attempt 取（`/tasks/{id}/attempts/{idx}/result` + `/logs`） | §15.4 第 1 项（与 `/files` 一起补，验收 §14.1 #14「原始产物可获取」才完整） |
| `GET /runs/{id}/files?path=` | 未实现 | 需要 Range 下载与路径白名单（`impl/execution.md §8.5`），MVP 只做了日志直读 | 同 `/artifacts` |
| `GET /tasks/{id}/attempts/{idx}/artifacts` | 未实现 | 同上，产物索引未对外 | 同 `/artifacts` |
| `GET /profiles` | 未实现 | 提交期用「表为空则放行任意 profile 名」的引导期例外兜住；前端 profile 是自由文本框 | P1 首批（去掉例外需要它） |
| `GET /testers/{name}/versions/{version}` | 未实现 | 只做了 `GET /testers/{name}`（取最新版本） | P1，与 Tester 版本对比一起做 |
| `GET /metrics` | 未实现 | Prometheus 指标未接入（`05-development-view` 的观测项） | P1 |

> 这张表是评审 [PR #13](https://github.com/ucm-system/testplane/pull/13) 指出的「API 面比设计少、且没有延后说明」的补记。**未列入本表的端点都按 `openapi.yaml` 实现完毕**（25 operation）。

### 15.3 与设计文档的偏差（已由 ARCHITECTURE v0.6 / D-38…D-43 裁决）

| # | 偏差 | 处置 |
|---|---|---|
| 1 | 不提供 `POST /testers` 运行时注册 | 符合 K-3；自发现=启动/周期扫描（D-38），`07-api.md` 端点表本就无此端点 |
| 2 | 错误体为**扁平** `{code,message,detail,retryable,trace_id}` | 以 `07-api.md §3` 为准（D-39）；`api/openapi.yaml` 同步 |
| 3 | 提交体以**嵌套** `run:` 为唯一形态 | 以 `07-api.md §4.1` 为准（D-40）；`03-data-view.md §4.1` 的平级示例是文档缺陷，待回写 |
| 4 | 产物 PVC 名统一 `testplane-storage` | 06 §2（`-storage-root`）与 execution §2.2（`-artifacts`）待回写（D-41） |
| 5 | 开发种子 `cluster-01` + `k8s-npu` 由进程启动时插入 | 03 §9 原为"Helm values 首次 seed"（D-42）；MVP 简化为进程内 seed |
| 6 | 执行环境为宿主 Kind + `kind load docker-image`，无镜像仓库 | D-43 |
| 7 | 未采用 `oapi-codegen` / `sqlc` 生成物，handler 与 DTO 手写 | 05 §5.2 的"重生成 diff 为空"门禁暂不适用；P1 再引入 |
| 8 | `task_counts` 为 MVP 新增的 Run 投影字段 | 已写入 `server/api/openapi.yaml`；`03/07` 待回写 |
| 9 | 架构表实为 **11 张**（文档曾按 12 表述） | 以 `03-data-view.md §3.2` 实际 DDL 为准 |
| 10 | Pre-Check 判别曾有 4 处实现，其中门控（`orchestrator/controller.go`）、放行 SQL（`store/control.go`）、终态判定（`orchestrator/status.go`）误用 `TesterRole` / 单看 `seq=0` | 唯一判据 = `runs.resolved->'precheck'->>'injected' = true` **且** `tasks.seq = 0`（`03-data-view.md §4.2`）；`endpoint-probe` 本身 `role=pre_check`，用户可在 `precheck:"off"` 下把它当普通测试提交，故角色不可作判据。评审 #14 已修：`store.PrecheckTaskIDs`/`PrecheckFlags` + `orchestrator.TaskView.IsPrecheck` |
| 11 | Job/Pod 正常结束后**没有回收路径**（只有取消才删 Job），也没有孤儿 GC goroutine；评审时集群已积压 53 个已终态 Job | 已按 `execution.md §6` 补齐（PR #14/#15）：`ports.Executor` 增 `Delete(ctx, h, DeletePolicy)`（`Background`/`Foreground`，404 幂等），Attempt 终态**落库之后**异步 `Delete(Background)`；新增 GC 循环（`Config.GCInterval` 默认 10min，`TESTPLANE_JOB_GC_ENABLED` 默认 true）回收"Job 已终态且 Attempt 已终态/无对应 Attempt"的 Job 与孤儿 Pod |
| 12 | `testers/endpoint-probe/tester.yaml` 的 `placeholder` 是多行说明而非一行命令，`parameters.url.description` 里还写了"退出码 2"（`ARCHITECTURE §5.4` 要求一行示例、§5.8 只有 0/非 0） | **不改 Tester 内容**：内容一变就要升版本（K-3 版本钉死，注册表实测会拒绝启动——本轮试改后已回滚）。平台侧取 `placeholder` 首行作占位、退出码一律按 0 / 非 0 归类；待 `/testers/{name}/versions/{version}` 与版本升级流程就绪后，由 Tester 维护者在下一个版本里修（G-15） |

### 15.4 P1 优先项（按投入产出排序）

1. 真实 Genet 适配（替换 `internal/genet/mocks`，接口不变）。
2. OIDC + 会话 + CSRF + RBAC（`TESTPLANE_AUTH_MODE=oidc`）。
3. 失败续跑（`retry` / `retry-plan`）与 `POST /tasks` 独立 Task。
4. 指标索引（MongoDB）与 Run 对比、看板。
5. `oapi-codegen` / `sqlc` 与生成物 diff 门禁。
