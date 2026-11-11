# 04 进程视图（Process View）

> **范围：** Argus 控制面的**运行时形态**——有哪些并发单元、各自按什么周期跑、一次 Task 的交互时序、关键路径靠什么锁保证正确、故障时在跑的东西会怎样、事件如何推到客户端、时间花在哪里、哪些循环必须单活。
> **不在范围：** 状态与转移表（见 [02 逻辑视图](02-logical-view.md) §2–§3，及 [`../ARCHITECTURE.md`](../ARCHITECTURE.md) §7）；表、列、索引（[03 数据视图](03-data-view.md)）；Job 渲染、挂载、日志分段细节（[`../impl/execution.md`](../impl/execution.md) §2–§3、§7）；副本数、探针、节点与容量（[06 物理视图](06-physical-view.md)）。
> **基线：** ARCHITECTURE v2.0、IMPLEMENTATION（K-1…K-12 已拍板）、C-3/C-4/C-5/C-6/C-9、E-4/E-5/E-7。**本文不新增设计裁决**：文中数字未标来源的都是 C-11 / E 分册的实现默认值，可配；`impl/` 分册未随 v2 更名前以其章节号定位。
> **读法：** §1 谁在跑；§2 按什么顺序跑；§3–§5 并发下靠什么保证正确；§6 慢在哪；§7 部署时受什么约束。

---

## 1. 进程与角色

### 1.1 三个角色，一个二进制

`argusd --role=api|controller|collector|all`（I-4.1）。同一镜像，角色只改启动参数与副本数。

| 角色 | 干什么 | 明确不干 | 状态 | 扩缩 |
|---|---|---|---|---|
| `api` | REST、SSE fanout、鉴权、校验、读投影 | 不建 Job、不推进状态、不调 Genet | 无状态 | 多副本，滚动 |
| `controller` | 调度（runner 串行放行）、编排推进、对账、超时、报告、`events.jsonl`、GC、保留期 | 不服务用户请求；不写产物内容；**不直连 Genet**（部署轮询在 service-deploy runner 内，§9.1） | 内存只有 cache（可丢） | **单活**（Lease） |
| `collector` | runner Job / 部署服务 Pod 日志跟随、deploy-state.json 归档（含 Pod 清单快照）、配额 walk | 不改 runner 状态、不判日志体积为失败 | 内存只有游标 | 多副本 + 分片 |
| `all` | 三者同进程（P0 默认，K-7） | — | — | 1 副本 |

**一条硬约束支撑下面所有判断：只有 `controller` 推进状态**（C-1 规则 2、C-2.5）。`api` 与 `collector` 只表达事实或只读，所以它们可多副本；反过来说，任何"顺手在 api 里改个状态"的实现都会破坏本视图。

### 1.2 循环清单

| 循环 | 触发 | 周期 | 事务边界 | 失败重试 | 幂等键 |
|---|---|---|---|---|---|
| `api.http.serve` | 连接到达 | 常驻（`:8080`） | 每请求 1 个只读事务；写请求由 orchestrator 开 1 个短事务；SSE 长连接**不占事务** | 不内部重试，错误体带 `retryable` 交客户端 | 无（提交不做幂等，D-37） |
| `api.sse.fanout` | `LISTEN` 通知 / 新订阅 | 事件驱动，无周期 | 无（只读 `state_events`） | 查库失败：记日志、保持连接，下一条事件重查 | `(task_id, seq)` 去重 |
| `api.sse.heartbeat` | ticker | 15s `: ping`；空闲 60s 断开 | 无 | 客户端按 `Last-Event-ID` 重连 | `Last-Event-ID = seq` |
| `api.schedview.poll` | ticker | 15s（单查超时 5s） | 无（K8s/Volcano 只读，不写库） | 不可达 → `scheduler_unavailable`，保留上次快照并标 stale | 只读，无 |
| `controller.leader` | ticker | TTL 15s / 续约 5s | 无（K8s `Lease` 对象） | 续约失败立即停全部业务循环、转待命 | `argus-controller-leader` |
| `controller.scheduler.tick` | `NOTIFY` + 兜底 ticker | ≤ 1s | **每候选 1 个事务**：取槽位（仅外接模式）+ 建 runner 执行记录 + CAS `queued→preparing` + 事件 | 冲突（`ErrConflict` / 23505 / 40P01）→ 回 `queued`，下轮重来 | `job_id = j-<sha256(task_id/attempt_no/seq)[:8]>` |
| `controller.informer.jobs` | list/watch（每集群 1 factory） | `resyncPeriod 5m`（只对齐，不触发业务） | 无：handler 只更新内存 cache + 有界 channel(1024) | Reflector 自动重连；410 Gone 重 list；`HasSynced` 前不判定 | label `argus.io/job-id` |
| `controller.informer.pods` | 同上，二级过滤 `job-id` | 同上 | 同上 | channel 满 → 丢弃并置脏，由 30s 对账补 | 同上 |
| `controller.reconciler` | ticker + channel 唤醒 | 30s（`AG_RECONCILE_INTERVAL`） | 每条结论 1 个事务（CAS + 事件）；读 K8s 在事务外 | 单轮 ≤500 条、单次 5s；失败不阻塞下一轮 | `(task_id, attempt_no, seq, job_id)`；结论只认一次 |
| `controller.watchdog.timeout` | ticker | 10s | 两段：先落库 `failed/RUNNER_TIMEOUT`，再删 Job（不同事务，顺序不可换） | Job 删失败 → `CLEANUP_FAILED` + 1m/5m/15m 退避 ≤5 次 | 执行记录 + CAS `NOT IN (终态)` |
| `controller.report.writer` | `task_finished` 后异步 / 报告缺失对账 | 串行 1 worker，队列深度 ≤1000 | 只读库、只写 NFS，不开写事务 | 1m/5m/15m 退避 ≤5 次；未生成时对外 404 + `Retry-After` | 覆盖写（内容由库派生，重写即幂等） |
| `controller.events.jsonl` | `NOTIFY` + 1s 轮询兜底 | ≤1s（或攒 64 行） | 无（只读 `state_events`，只追加文件） | 重启后读文件尾 `seq` 与库比对，缺则补 | `(task_id, seq)`：已存在的 seq 跳过 |
| `controller.gc.orphan` | ticker | 10m | 无（删 K8s 对象 + `ResultSink` 回写状态） | 下轮重来；`hold-until` 未过期则跳过 | label `managed=true` + `job-id`；**不得**对 Genet 资源设 ownerReferences（§9.3） |
| `controller.retention.sweeper` | ticker（每日 03:00 Asia/Shanghai） | 24h | 每 Task 1 事务（删索引 + 审计），文件删除在事务之后 | 失败下一轮继续；先 `dry_run` 算体积再删 | `task_id`；保留期由平台配置统一决定（`cleanup.policy` 已取消，G-13/D-33） |
| `controller.consistency.check` | ticker | 每小时（不变量扫描）+ 每夜（全量） | 只读 + 只告警；**不自动改终态**（02 §5） | 失败下一轮；悬挂 Task 清单交 admin | `(task_id, check_name)` |
| `collector.leader` | ticker | TTL 15s / 续约 5s | 无（Lease） | 丢租约 → 停写快照归档，重新竞选 | `argus-collector-leader` |
| `collector.discover` | ticker | 30s（按 service-deploy 导出的 `label_selector` LIST，读 `deploy-state.json` 归档） | 无 | 失败沿用上次清单；Pod 消失写 `terminated` 标记 | `pod_name` |
| `collector.follower` | runner 进 `running` / 部署服务 Pod 出现 | 流式 `follow=true`，EOF 关闭 | 无（只写 NFS `runners/<seq>/logs/`） | 断线 `sinceTime=lastTS` 重连 + `log resumed` 标记；丢租约即停写 | Lease `argus-log-<jobhash>`；游标 = 服务端时间戳 + 200 行去重窗 |
| `collector.quota.walker` | ticker | 60s/attempt（单次 <50ms 量级） | 无（只读文件系统） | walk 失败 → `quota_check_skipped`，不判失败 | `(task_id, attempt_no)` + 目录路径 |
| `collector.deploy_state.archive` | ticker | 30s（temp + rename 原子写，归档 service-deploy 周期改写的 `deploy-state.json` 及其 Pod 清单快照，§9.2） | 无 | 写失败下轮重写，旧文件保留 | `(task_id, attempt_no)` 覆盖写 |

**"事务边界"这一列是硬约束**：事务内禁止 K8s / NFS IO（C-2.5）。凡是表里写"事务外"的调用，都必须先拿结果、再把结果变成状态。

**部署轮询不在本表**：Genet 轮询发生在 service-deploy runner 进程内（1s→15s 退避、总窗口 72h，平台配置），它周期改写 `deploy-state.json`，平台只归档；同理卸载由 service-clean 执行——控制面**没有** `genet.*` 循环（§9.1、D-70）。

### 1.3 角色 × 循环

| 循环 | api | controller | collector |
|---|---|---|---|
| `http.serve` / `sse.fanout` / `sse.heartbeat` / `schedview.poll` | ● | ○ | ○ |
| `leader` / `scheduler.tick` / `reconciler` / `watchdog` | ○ | ● | ○ |
| `informer.jobs` / `informer.pods` | ○ | ● | ○（只读 Pod 用 `logs`/`LIST`，不建 informer） |
| `report.writer` / `events.jsonl` / `gc.orphan` / `retention.sweeper` / `consistency.check` | ○ | ● | ○ |
| `collector.leader` / `discover` / `follower` / `quota.walker` / `deploy_state.archive` | ○ | ○ | ● |

`--role=all` 时全部 ● 在同一进程；循环按角色开关挂载，**不因合并而共享事务**。

### 1.4 并发预算

| 角色 | 常驻 goroutine 量级 | 上限与说明 |
|---|---|---|
| `api` | 1 listener + 2/SSE 连接（读+写）+ 1/fanout 频道 | 1000 并发 SSE ≈ 2×10³ goroutine；每连接缓冲 256 事件、超 64 KiB 慢连接的帧丢弃并置脏 |
| `controller` | 约 10 个常驻循环 + 每候选短事务 + 每集群 1 factory（2 informer × 2 goroutine） | 调度 tick 内 ≤20 个事务；对账单轮 ≤500；cache 与活跃 Job 数成正比 |
| `collector` | 1 leader + 每被跟随对象 3 goroutine（read / pump / flush）+ 定时器组 | 每副本 ≤200 follower；单文件 4 KiB `bufio` + 1s flush |
| `all` | 三者相加，P0 单 Pod 峰值 ≈ 3×10³ | 4 vCPU / 2 GiB 起；SSE 连接数是第一变量，不是 runner 数 |

---

## 2. 一次 Task 的时序

### 2.0 业务流总图

```mermaid
flowchart TD
  S["提交 + 组合期校验<br/>（deploy XOR service、exports/consumes 闭合、输入词法/结构）"] --> Q["queued"]
  Q --> R["running：线性管道按 seq 推进<br/>（无 deploying / cleanup / precheck 阶段）"]
  R --> D{"seq=1 有 service-deploy?"}
  D -- "是（部署模式）" --> DP["service-deploy Job：<br/>调 Genet + 周期写 deploy-state.json"]
  DP -- "ready + exports 四件套" --> PC
  DP -- "blocked / 超窗<br/>（DEPLOY_BLOCKED）" --> SU["service_unavailable"]
  D -- "否（外接模式）" --> PC{"pre_check<br/>（precheck: auto 注入）"}
  PC -- "可重试失败" --> SU
  PC -- "不可重试失败（4xx/认证）" --> F["failed"]
  PC -- 成功 --> T["test runner… 严格串行"]
  T --> TRES{"runner 结果"}
  TRES -- "abort_task" --> SK["其余 runner skipped<br/>（传播到 clean 为止）"]
  TRES -- "continue（存在失败）" --> T
  T -- "全部测试类 runner 终态" --> CL["service-clean<br/>（finally：成功/失败/取消/超时都执行）"]
  SK --> CL
  CL --> FIN{"终态判定（§7.5）"}
  FIN --> OK["succeeded"]
  FIN --> F
  FIN --> SU
  FIN --> CAN["canceled（取消路径）"]
```

### 2.1 提交 → 排队 → 部署 → 串行管道 → clean → 报告

```mermaid
sequenceDiagram
  participant C as 客户端
  participant A as api
  participant O as orchestrator
  participant DB as PostgreSQL
  participant K as controller
  participant D as service-deploy Job
  participant G as Genet
  participant X as Executor/K8s
  C->>A: POST /tasks（Manifest）
  A->>O: 组合期校验 + Resolve（deploy XOR service、exports/consumes 闭合、输入词法/结构校验）
  O->>DB: 1 事务：tasks + task_runners（含注入的 deploy/precheck/clean）+ state_events(submitted) + audit_log
  A-->>C: 202 + task_id
  K->>DB: CAS submitted→queued→running（管道首个 runner 入队）
  K->>X: 创建 seq=1 service-deploy Job
  D->>G: POST /api/open/helm/.../releases（noWait=true，releaseName=arg-<hash>）
  G-->>D: queued（排队，可能数天）
  Note over D,G: runner 进程内轮询（1s→15s 退避），周期改写 deploy-state.json；平台归档；窗口 72h
  G-->>D: ready
  D-->>K: 退出 0 + exports（AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH / AG_DEPLOY_REF）
  K->>DB: 同事务：exports 收割落档 + 串行放行 seq=2（endpoint-probe）
  Note over K,X: 严格串行：前一 runner 终态才创建下一 Job；部署排队可见性不占控制面连接
  X-->>K: pre_check succeeded（informer）
  K->>DB: 放行 seq=3…（逐个串行推进）
  X-->>K: 各 test runner 依次 succeeded / failed（按 on_failure 传播）
  K->>DB: 测试类 runner 全部终态 → service-clean 入队（finally）
  X-->>K: service-clean 结束（幂等拆 release，失败只记 cleanup_failed）
  K->>DB: 终态判定（§7.5）+ 异步 report.writer
  DB-->>A: NOTIFY(task_id, seq)
  A-->>C: SSE：task_finished
```

四处需要点明：

1. **提交只有 1 个事务**；`202` 之后的一切都在 controller 里推进，api 不参与。
2. **部署排队不是控制面的等待**：Genet 轮询在 service-deploy runner 进程内；平台没有 `DEPLOYING` 阶段，可见性由 `deploy-state.json` 承载（§7.4、§9.1）。
3. **串行放行是唯一的推进序**：`seq+1` 以 `seq` 终态为前提；没有 v1 的门控批量入池与并行调度（v2 取消 D-32 模型）。
4. **clean 是 finally**：成功/失败/取消/超时四条路径都执行；clean 失败只记 `cleanup_failed` 不改终态（§7.6）。

### 2.2 单个 runner 的执行细节

```mermaid
sequenceDiagram
  participant S as scheduler
  participant DB as PostgreSQL
  participant E as Executor
  participant X as K8s apiserver
  participant I as informer/reconciler
  participant F as collector follower
  S->>DB: BEGIN；SELECT 候选（无行锁，ORDER BY task.submitted_at, seq LIMIT 50）
  S->>DB: SELECT tasks … FOR UPDATE（锁定候选所属 Task，≤50ms）
  S->>DB: 外接模式：pg_advisory_xact_lock(endpoint_key) → 判独占 → INSERT endpoint_leases
  S->>DB: pg_advisory_xact_lock(global) → count(active) ≤ 20
  S->>DB: 建 runner 执行记录(task_id,attempt_no,seq) + CAS queued→preparing + seq/事件 → COMMIT（≤50ms）
  S->>E: Submit(JobSpec 2.0：args 恒空、mounts 含 input、labels argus.io/*)
  E->>X: GET Job → 比 spec-hash → Create argus-<task>-<attempt>-<seq>
  X-->>I: watch 事件 → cache → 唤醒 reconciler
  I->>DB: 事务：CAS preparing→running + started_at
  X-->>F: pods/log follow
  F->>F: runners/<seq>/logs/stdout.log（4 KiB/1s flush）
  X-->>I: Job Complete / Pod exit 0
  I->>I: 校验产物契约（三选一：result.json / result.html / result.csv 至少其一，D-74）+ 收割 exports.env（缺失/非法/导出非法各有码）
  I->>DB: 事务：CAS running→succeeded + 释放租约 + exports 落档 + 事件
  Note over I,DB: 终态后由 report.writer 异步生成 runner report；串行放行 seq+1
```

要点：**Job 创建在事务外**（C-2.5），失败走 `preparing→failed`（`IMAGE_PULL_FAILED` / `MOUNT_UNAVAILABLE` / `EXECUTOR_UNAVAILABLE`）；**没有重试回路**（v2 取消 `retry_on`，`FAILED` 是终态，§7.8）。**租约释放与终态同事务**（C-4.3，仅外接模式），否则崩溃会泄漏槽位。

### 2.3 取消与自然结束的竞态（含部署排队期取消）

```mermaid
sequenceDiagram
  participant U as 用户
  participant A as api+orchestrator
  participant K as controller
  participant DB as PostgreSQL
  participant W as watchdog/informer
  participant X as K8s
  participant CL as service-clean Job
  U->>A: POST /tasks/{id}/cancel
  A->>DB: 事务：CAS 非终态→cancelling；pending/queued→skipped(canceled)；当前 runner cancel_requested=true
  K->>X: 对 cancel_requested 的执行记录：Delete Job（Background, grace 30s）
  Note over K,X: 2s 轮询 Pod ≤60s；仍在则 Foreground+grace 0；再 30s 仍残留 → CLEANUP_FAILED
  W->>DB: 同一窗口内：CAS running→canceled（WHERE status NOT IN 终态）
  alt 取消先落库
    DB-->>W: 0 行 → 只补 exit_code，不改状态
  else 自然结束先落库
    DB-->>K: 0 行（ErrConflict）→ 记 state_race_lost，不覆盖
  end
  K->>X: 含部署时创建 service-clean Job（finally，幂等拆 release）
  CL-->>K: clean 结束（失败只记 cleanup_failed，进 §9.3 重放清单）
  K->>DB: clean 终结 或 2 分钟兜底 → canceled
```

三条不变量：**先落库者胜**（CAS + `state_race_lost`，E-5.3）；**取消不悬挂**（clean 完成或 2 分钟兜底后落终态，`CANCELLING` 不是归宿）；**删 Job 失败不回滚状态**（只记 `CLEANUP_FAILED` + 告警，C-6.4）。

---

## 3. 关键路径的并发控制

### 3.1 锁与互斥清单

| # | 机制 | 位置 | 持有范围 | 顺序 | 失败 / 冲突处理 |
|---|---|---|---|---|---|
| 1 | `SELECT tasks … FOR UPDATE`（逐候选） | `scheduler.claim`（C-4.2 的候选集） | Task 行锁覆盖该候选整段 dispatch（≤50ms），不跨轮 | 第 1 | 同 Task 的其他推进等待或超时跳过；`lock_timeout=2s` → 本轮放回 |
| 2 | `pg_advisory_xact_lock(hashtext(endpoint_key))` | `scheduler.acquireEndpointLease`（C-4.3） | **仅外接模式**；覆盖"查持有者 → INSERT 租约"整段；xact 结束即释放 | 第 2 | `lock_timeout=2s` → 放弃该候选，写 `waiting_reason=slot_waiting`；只降并发，不影响正确性 |
| 3 | `pg_advisory_xact_lock('argus:global-concurrency')` | `scheduler.acquireGlobalPermit`（C-4.4） | 覆盖"count 活跃 + 建执行记录"；全局单点串行 | 第 3 | 2s 超时 → `global_limit`；20 并发下每次毫秒级，实测非瓶颈 |
| 4 | CAS `UPDATE … WHERE status IN ($from)`（`Kernel.Apply` 第 4 步） | `Kernel.Apply` / runner 门面 | 单行行锁，至提交；不跨语句 | 第 4（必须在 advisory 之后） | 返回 0 行 → `ErrConflict` → 重读，先落库者胜；终态不进 `from` 集合 |
| 5 | `tasks.last_event_seq` 原子自增（`Kernel.Apply` 第 5 步） | `store.nextSeq` | 单行 `UPDATE … RETURNING`，与状态写同事务 | 紧跟 CAS | 40P01 死锁 → 整事务退避 20ms 重试 1 次，仍失败记 `INTERNAL_ERROR` |
| 6 | 执行记录唯一约束 `(task_id, attempt_no, seq)` | 建 runner 执行记录 | 索引即约束 | 事务内 | 23505 → 判定竞态，回 `queued`，不重试本事务 |
| 7 | 部分唯一索引 `endpoint_lease_excl_uq` | 插独占租约（仅外接模式） | 一 Endpoint 至多 1 个独占持有者 | 事务内 | 23505 → 回退（advisory 已串行，正常不触发） |
| 8 | informer cache + 30s 对账 | `executor.Status` / `reconciler` | 内存，无锁；`resync 5m` 只对齐 | — | 未 `HasSynced` 不判定；cache 缺项 → `GET`；`unknown` 按 0/5/20/60s 四步复核，≤90s |
| 9 | K8s `Lease` 选主 | `controller.leader` | TTL 15s / 续约 5s；非 leader 不启业务循环 | — | 抢不到 → 待命但不退出；进程存活、`/readyz` 仍成功（见 §7） |
| 10 | collector 双层 Lease | `collector.leader` + `argus-log-<jobhash>` | 外层：分配、deploy-state 归档、回收死 follower；内层：单文件的唯一写权 | 先外后内 | 内层丢失立即停写；新持有者从 checkpoint 续采并写 `log gap`（E-7.3） |

> advisory 的键名以 C-4.4 字面为准；[02 §3.8](02-logical-view.md) 里的 `argus:global` 是同一把锁的简写，实现只能有一处常量。状态写入只经 `Kernel.Apply`（02 §3.3）；表里的 `scheduler.*` 是实现层函数名，不是契约。

### 3.2 加锁顺序与死锁规避

- **固定顺序**：实体的行锁统一为 **Task → runner 执行记录**（批量按 seq 升序）；调度事务先锁候选所属的 Task，再取 endpoint advisory（#2，仅外接模式）→ 全局 advisory（#3）→ CAS runner（#4）→ 写 `endpoint_leases` / 执行记录 → 写 `state_events`。任何代码不得倒序，也不得在持有 advisory 后再去锁另一个 Task。
- **一个事务只处理一个候选**：tick 内最多 20 个事务，禁止批量持有多个执行记录行锁——多候选批处理是死锁的唯一现实来源。
- **只用事务级 advisory**（`_xact_`），禁止会话级：连接池复用会让会话锁泄漏到别的请求上。
- **超时护栏**：`lock_timeout=2s`、`statement_timeout=3s`、调度事务 `SET LOCAL statement_timeout=40ms`、`idle_in_transaction_session_timeout=5s`。
- **争用可观测**：`scheduler_tx_slow_total`（>40ms）、`lock_wait_seconds`、`state_race_lost_total`、`reconcile_exhausted_total`。没有指标的自旋锁等于没有锁。

### 3.3 内部操作的幂等键

> **对外提交没有幂等**（D-37）：`POST /tasks` / `POST /tasks/{id}/retry` 每次都新建资源或新 attempt，重复提交 = 多个 Task。下表只是实现层的"重复执行不产生副作用"，不构成对调用方的承诺。

| 对象 | 键 | 冲突语义 |
|---|---|---|
| Job | `job_id = j-<sha256(task_id/attempt_no/seq)[:8]>`，K8s 名 `argus-<task_id>-<attempt>-<seq>` | `GET` 先查，`spec-hash` 相同即复用，不同 409（E-2.3） |
| Genet release | `releaseName = "arg-" + sha256(task_id:attempt_no)[:12]`（D-44 v2；由 service-deploy 在 runner 进程内保证） | 同名冲突按 `DEPLOY_FAILED` 归类；`noWait=true` 恒定 |
| 事件 | `(task_id, seq)` | 唯一约束；jsonl 与 SSE 都按它去重 |
| 日志文件 | `argus-log-<jobhash>` Lease | 单写者；续采靠时间戳游标 |
| 报告 | 覆盖写 | 内容由库派生，重算即重写 |

---

## 4. 故障恢复

| 故障 | 进行中的 Task / runner | 恢复动作 | 用户可见 |
|---|---|---|---|
| **controller 崩溃** | 状态全部停留在库中最后一次提交；`RUNNING` 的 Job 继续跑、Pod 不删；执行记录不再更新 | Lease 15s 过期后新 leader 接管；`HasSynced` 后 30s 内对账拉平；Job 缺失需连续 3 次（≈1min）确认才判 `EXECUTOR_UNAVAILABLE`；clean 未被调度的 attempt 进 §9.3 重放清单 | Task 状态"冻结"但合法；SSE 心跳仍在、无新事件；详情页标"待确认"（`executor_observation_unknown`）；不显示失败 |
| **api 副本摘流** | 不影响任何 Task；该副本上的 SSE 连接断开 | `Last-Event-ID` 重连到其他副本并从库回放（24h / 1000 条内）；无粘滞会话 | 流式刷新中断 1–2s 后自动续上；不丢事件，超窗则整页重建 |
| **DB 不可用** | controller **暂停推进**，不误判；Job 继续跑；已提交事务不丢 | `/readyz` 失败摘流；连接池退避重连；恢复后 30s 对账对齐；已跑的 `RUNNING` 不假定失败 | 写操作 503 + `Retry-After`；读接口降级失败；状态停在最后一次落库值，不出现"假终态" |
| **K8s API 不可达** | runner 停在 `PREPARING`（建 Job 失败）或 `QUEUED`；已在跑的 Pod 不受影响 | 建 Job 失败按平台内部操作对账重试（不产生新执行）；`unknown` 走四步复核，不直接判失败；持续不可达告警 `EXECUTOR_UNAVAILABLE` | `QUEUED/PREPARING` + `waiting_reason` 不变；错误码 503、`retryable: true`（仅告警分级）；不显示 runner 失败 |
| **Genet 不可达** | `running` 中的 service-deploy runner 自己轮询失败；尚无后续 runner | runner 内单请求 10s 超时；连续 5 次且累计 ≥60s → service-deploy 判失败（`DEPLOY_FAILED`，Task `failed` 平台侧）；72h 未 ready → `DEPLOY_BLOCKED`（Task `service_unavailable`）；**排队期间不可达不算 blocked** | 终态区分清楚：`failed` 找平台，`service_unavailable` 找部署方（§7.5） |
| **NFS 抖动** | 目录预创建失败则不建 Job；运行中则日志断流、产物可能写不进 | 预创建失败 → 保持 `QUEUED` + `EXECUTOR_UNAVAILABLE`，**不**混成 `MOUNT_UNAVAILABLE`；`result.json`（若产出，D-74）读取区分"不存在"与 I/O 错误（后者 1s/2s/4s 重试 3 次才收口）；日志写 gap 标记 | 重试中的 runner 保持 `QUEUED`；日志出现 `--- [log gap: …] ---`；不把 NFS 抖动记成测试失败 |
| **collector 崩溃** | runner 状态完全不受影响；该 Task 日志出现断档 | 新副本抢 `argus-log` 租约，从 checkpoint 续采并写 gap；`deploy-state.json` 归档由外层 leader 30s 内补齐 | 日志文件出现缺口标记（可正则识别）；UI 显示"采集中断"而非"服务无输出" |
| **Runner Pod 重启 / 驱逐** | `restartPolicy: Never` + `backoffLimit: 0` → 该执行记录直接失败，不重启续跑 | `OOMKilled` / `Evicted` → `failed` + `INTERNAL_ERROR`（`detail.k8s_reason` 保原文）；**无 runner 级重试**（v2），需整 Task 重试成新 attempt；`Unschedulable` 不判失败，超时后按 `RUNNER_TIMEOUT` 收口 | 该 runner `FAILED` 带 K8s 原因；其余 runner 按 `continue/abort_task` 收口；历史 attempt 不被覆盖 |
| **被测服务 Pod 重启** | Task 不受影响（Pod 不是执行记录）；服务可能短暂不可用 | 日志续写同一文件并插 `container restarted #N`（部署 Pod 日志在 `runners/<deploy_seq>/logs/`）；后续 runner 若撞上未就绪 → `ENDPOINT_UNREACHABLE`/`PRECHECK_FAILED`（可重试标记） | 日志可见重启标记；详情页展示重启次数与状态，不假设"只有一个实例"（§9.2） |

**共性规则**：故障不产生"猜出来的状态"。要么保持上一个已提交状态，要么落一个带错误码的终态；**绝不**因为观测不到就判失败（§12.5 第 2 条、附录 B `unknown` 语义）。

---

## 5. 事件与推送路径

### 5.1 写路径（DB 是唯一权威）

```text
状态事务 COMMIT
   └─ NOTIFY argus_task_<id>, payload={task_id, seq}   （提交后发，≤100B）
        ├─ api 每个副本 LISTEN（无粘滞）→ 按 (task_id, seq) 查 state_events → 分发给本副本订阅者
        │      └─ SSE 帧：id: <seq> / event: <type> / data: <JSON>
        └─ controller.events.jsonl 单 writer → 追加 <storage-root>/<task_id>/events.jsonl
```

- **`NOTIFY` 放在事务提交后**（C-3.3）：推已回滚的事件比晚 2ms 严重得多。payload 只带 `task_id + seq`，绕开 PG 8 KiB 限制，正文一律回查。
- **`events.jsonl` 单 writer**：只有 controller leader 内的一个 goroutine 写；输入是 NOTIFY 与 1s 轮询的并集，按 `seq` 排序去重；每 1s 或 64 行 flush。重启后读文件尾 `seq` 与库比对补齐（G-8：文件是派生，可整文件重建）。
- **文件与库不一回事**：库是权威，文件落后一个 flush 周期是允许的；反过来"文件有、库里没有"是缺陷。

### 5.2 断线回放与 `resync_required`

| 场景 | 行为 |
|---|---|
| 断线 ≤24h 且缺失 ≤1000 条 | 从 `Last-Event-ID` 之后回放，客户端按 `seq` 去重；不重排 |
| 超出窗口（>24h）或超条数 | **先发 `resync_required`**，客户端整页重建（拉 `/tasks/{id}` + `/events/history`），**不静默截断** |
| 慢客户端（缓冲满 256 或 64 KiB） | 丢弃该连接并置脏，客户端重连走回放；不阻塞其他订阅者 |
| 空闲 | 15s `: ping`；60s 无数据断开重连 |
| 事件类型 | 只用 C-9.1 的枚举；`payload` 不含凭据，**日志内容不走 SSE**（日志走 `…/runners/{seq}/logs?follow=1`） |

多副本下每个 api 都能服务任一订阅者，代价是 N 次回查；P0 的 N（api 副本）≤3，按 `queue_depth` 与 SSE 连接数决定是否加副本。

---

## 6. 时延预算

### 6.1 状态查询 P95 < 300ms（§12.7）

| 段 | 预算 | 手段 |
|---|---|---|
| TLS / HTTP 复用 | 0（长连接）/ 8ms（冷） | keep-alive；不做每请求握手 |
| 认证 + RBAC | 5ms | JWKS 内存缓存 10min，无远程校验 |
| 路由 + 请求解析 | 2ms | chi，无反射绑定 |
| 连接池获取 | 3ms | `max_conns=20`（4×vCPU，上限 40）；只读事务立即结束 |
| 查询 | 20ms | 详情 1 条 JOIN（执行记录按 `(task_id, attempt_no, seq)` 定位）；列表游标扫 ≤50 行（`tasks(status, submitted_at DESC)`），**不用 OFFSET** |
| 序列化 | 10ms | 预分配 buffer；runner 摘要一次带出，杜绝 N+1 |
| 内网传输 + 首字节 | 20ms | 同集群 |
| **合计（P95 预算）** | **≈60ms** | 对 300ms 留 5× 余量给 PG 抖动与 GC；慢查询 >50ms 记 `pg_slow_query_total` |

单 Task ≤200 runner（`TASK_TOO_LARGE` 拦截）是这条预算的前提；再加 runner 数会先打爆序列化，不是查询。

### 6.2 调度一次事务 < 50ms

| 段 | 预算 |
|---|---|
| `BEGIN` | 1ms |
| 候选查询（执行记录就绪索引, `LIMIT 50`，不加行锁） | 3ms |
| endpoint advisory 等待（仅外接模式） | ≤10ms（常态 <0.5ms，持锁方只做 3 条语句） |
| 租约判定 + `INSERT` | 1ms |
| 全局计数（活跃行 ≤20） | 2ms |
| 建执行记录 + CAS runner + `seq` + 事件 | 3ms |
| `COMMIT`（本地盘 WAL fsync） | 3–6ms |
| **合计** | **20–25ms 典型，50ms 硬线** |

超 40ms 记 `scheduler_tx_slow_total`；每 tick 20 个事务仍能塞进 1s。NFS 不参与任何事务——这是这条预算成立的根本原因。

### 6.3 SSE 事件端到端 < 1s

| 段 | 预算 |
|---|---|
| 状态事务 COMMIT（含 fsync） | 3ms |
| `NOTIFY` 投递到 api（同机 PG） | 1–3ms |
| api 回查 `state_events`（唯一索引） | 5ms |
| fanout 到订阅者 channel | <1ms |
| 写 TCP + 客户端首字节（同集群） | 20ms |
| 客户端批量失效 + 渲染 | 50ms |
| **合计** | **P50 ≈80ms，P95 <1s** |

**不承诺跨集群**：controller / api / PG 同集群部署时上述成立；Genet 与被测服务在别的网络里，只影响 Task 推进速度，不影响事件路径。若把 api 与 PG 分置两集群，按"秒级 + 回放兜底"理解，不套用 1s 预算。

---

## 7. 进程视图 → 物理视图（[06](06-physical-view.md)）的约束

| 循环 / 组件 | 部署要求 | 理由 |
|---|---|---|
| controller 全部业务循环（scheduler / reconciler / watchdog / report.writer / events.jsonl / gc / retention） | **必须单活**：`replicas: 1` + `strategy: Recreate`，或 2 副本靠 Lease 待命 | 双写会重复推进状态；单活靠 Lease，不靠"只有一个 Pod"这个事实 |
| `api.http.serve` / `api.sse.fanout` | **多副本 ≥2**，`maxSurge 1 / maxUnavailable 0`；不要求会话粘滞 | 无内存状态；SSE 靠 `Last-Event-ID` 回放跨副本续订 |
| `collector.leader` | **单活**（全局 Lease），只做分配、deploy-state 归档、回收死 follower | 避免多副本重复写元数据 |
| `collector.follower` | **多副本分片**：分片键 `hash(job_id) % N`，每副本 ≤200 follower | 文件写权已由 `argus-log` 租约保证；分片只为摊吞吐 |
| `controller.informer.*` | 每副本各自一套（每集群 1 factory，2 类资源），**不选主** | 只读缓存；单活会让 failover 后重新 list 变慢 |
| `api.schedview.poll` | 每 api 副本各自 15s 缓存 | 只读、可短暂 stale，不共享 |
| 探针 | api `/readyz` = DB ping + NFS 可写 + 迁移版本匹配（**不含 K8s**）；controller `/readyz` **不因"非 leader"失败**，leader 状态走 `controller_is_leader` 指标 | K8s 抖动不该摘掉能读历史报告的 api；待命副本被摘流会导致真 leader 挂掉时无人接班 |
| 升级顺序 | controller（Recreate，在跑 Job 不删，由对账接管）→ collector（≤30s 租约接管并写 gap）→ api（滚动） | 每步都有可解释的短暂降级，不需要"同时重启" |
| 存储前提 | RWX PVC 必须全节点可见；`subPath` 只到 `attempts/<n>` / `attempts/<n>/runners/<seq>` | 产物直落最终路径（§6.8、D-68），单写者只依赖 `O_APPEND` + Lease |

**分片容量线**：队列深度 >200 个 `queued` runner，或调度事务 P95 >50ms 时，才考虑按 `task_id` 分片 controller（执行记录已按 `task_id` 聚集，DB 层预留）。P0 的量级（每日百级 Task、20 并发 runner）单活足够，不提前拆。
