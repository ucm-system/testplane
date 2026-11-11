# 控制面实现方案（状态机 / 调度 / API / 报告）

> 分册，隶属 [`../IMPLEMENTATION.md`](../IMPLEMENTATION.md)。不新增设计裁决；与 [`../ARCHITECTURE.md`](../ARCHITECTURE.md) 冲突时以 ARCHITECTURE 为准（本文已按 **v2.0 / D-66～D-73** 同步：Task→attempt→runner 三层模型、线性串行管道、exports 收割、clean finally、整 Task 重试）。
> 约定：`§x.y` = ARCHITECTURE；`C-x` = 本文；`E-x` / `W-x` / `D-x` 见同目录另两册。

---

## C-1 分层与依赖规则

```text
api/           HTTP、SSE、鉴权、错误映射（薄）
orchestrator/  提交校验、管道组装（deploy/precheck/clean 注入）、Task 串行推进、
               exports 收割、abort_task、clean finally、终态判定
scheduler/     待执行 runner 选取、外接模式端点槽位、全局并发、超时 watchdog
store/         唯一写库入口：事务、CAS、事件、锁
domain/        纯类型与规则（状态枚举、转移表、校验），零 IO
executor/ deploystate/ registry/ report/ events/ logcollect/  adapter，不写 tasks.status
```

规则：

1. 只有 `store` 能执行 SQL；其他包通过 store 的方法表达意图。
2. 只有 `store.Transition*` 能改 `tasks.status`（Task）/ `task_runners.status`（runner 执行记录）/ `attempts.status`（attempt 汇总），且必须同事务写事件（§7.1、§12.5）。
3. adapter（executor / deploystate / registry / report）只回写"物理事实"（Job 状态、exports 落档路径、deploy-state 副本路径），状态推进由 orchestrator 在事务里做；平台不直连 Genet（D-70）。
4. `domain` 可被任何层引用，反向不成立。

---

## C-2 数据模型

### C-2.1 设计原则

- **状态 token 统一小写**（`pending`…`canceled`），与 §8.4 示例一致；文档中的大写是概念名，前端展示大写徽标（见 I-12 G-12）。
- **三层模型（D-68）**：`Task → attempt → runner`。Task 是稳定标识与重试单位；attempt 是 Task 的第 N 次全量执行（计数）；runner 在 attempt 内按序执行**恰好一次**，执行记录由 `(task_id, attempt_no, seq)` 唯一定位。
- **无 runner 级重试（D-69）**：runner 失败即本次 attempt 的失败事实，不存在 `retry_policy` / backoff 字段；重试入口只有整 Task 级。
- 时间一律 `timestamptz`（UTC 存储，展示按 `Asia/Shanghai`）。
- 不软删：Task 的删除只发生在保留期清理，不做逻辑删除标记。
- 事件表 append-only，只由状态机写；审计另表。
- 所有"配额/上限"在提交时校验，运行期由 watchdog 兜底。

### C-2.2 表清单

| 表 | 作用 | 权威性 |
|---|---|---|
| `tasks` | Task 快照、状态、管道 resolved 快照、attempt 计数（v1 的 `runs`） | 权威 |
| `attempts` | Task 级 attempt 行：第 N 次全量执行的输入落档 / exports 落档 / deploy-state 副本路径（v1 的 per-runner Attempt 上移，D-68） | 权威 |
| `task_runners` | runner 执行记录，`(task_id, attempt_no, seq)` 唯一；含冻结的定义快照与物理事实（v1 的 `tasks` + `attempts` 合并角色） | 权威（与 K8s Job 对账） |
| `state_events` | 状态与生命周期事件流 | 权威（jsonl 由此派生） |
| `endpoint_leases` | 外接模式端点互斥槽位（部署模式不使用，D-69） | 权威 |
| `runners` | Runner 注册信息（含参数清单；deploy/clean 角色仅内置） | 权威（来源是目录扫描） |
| `clusters` / `execution_profiles` | 执行环境登记 | 权威 |
| `secret_refs` | Secret 引用（无明文） | 权威 |
| `artifacts` | 产物索引（下载用） | 派生，可重建 |
| `audit_log` | 操作审计 | 权威 |

> **v1 → v2 对照**：`runs` → `tasks`；v1 内层 `tasks`（Runner 定义快照）的列并入 `task_runners` 的冻结快照；v1 per-runner `attempts` 的物理执行列并入 `task_runners`；`attempts` 保留为 Task 级计数与落档归组行。

### C-2.3 关键 DDL

> 完整表结构、约束、索引、JSON 形状与迁移策略以 [`views/03-data-view.md`](../views/03-data-view.md) 为唯一权威；本节是摘要，字段级冲突以数据视图为准。

```sql
-- 状态枚举收敛在一处，迁移时同步修改 CHECK
-- tasks.status（Task 级）: submitted|queued|running|cancelling|
--              succeeded|failed|service_unavailable|canceled
--（v2 无 deploying/testing/cleanup/completed_with_errors：RUNNING 的内部推进 = runner 管道，§7.4 / D-70）
CREATE TABLE tasks (
  id               text PRIMARY KEY,                     -- t-YYYYMMDD-XXXX
  name             text NOT NULL,
  status           text NOT NULL,
  manifest         jsonb NOT NULL,                       -- 原始提交（secrets 仅引用；顶层 task:）
  resolved         jsonb NOT NULL,                       -- 管道快照：注入的 deploy/precheck/clean、
                                                         -- 各 runner 的输入落档路径、timeout、导出注入清单
  service          jsonb,                                -- 外接模式静态值 {url, model_name, model_path}（与 deploy 互斥）
  deploy           jsonb,                                -- 部署模式提交快照 {provider, spec 原文}
                                                         --（release 运行态归 attempts/<n>/deploy-state.json，D-70）
  submitted_by     text NOT NULL,
  submitted_at     timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  error            jsonb,
  retry_of         text REFERENCES tasks(id),            -- P1 跨 Task 续跑（仅外接模式，§7.7）
  current_attempt  int  NOT NULL DEFAULT 0,              -- 已创建的最大 attempt_no；上限默认 3（§12.7）
  last_event_seq   bigint NOT NULL DEFAULT 0,            -- 事件序号分配器（原子自增）
  revision         bigint NOT NULL DEFAULT 1
);

-- attempts：Task 级 attempt 行（v2 上移，D-68）——第 N 次全量执行的落档归组与输入/导出路径
CREATE TABLE attempts (
  task_id       text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_no    int  NOT NULL,                           -- 从 1 递增
  trigger       text NOT NULL DEFAULT 'initial',         -- initial | retry
  status        text NOT NULL,                           -- running|succeeded|failed|canceled（由 task_runners 聚合推导落库）
  input_dir     text NOT NULL,                           -- <storage-root>/<task_id>/attempts/<n>/input/（只读挂载源）
  exports_dir   text NOT NULL,                           -- .../attempts/<n>/exports/（收割落档）
  deploy_state_ref text,                                 -- .../attempts/<n>/deploy-state.json 副本（平台归档）
  started_at    timestamptz,
  finished_at   timestamptz,
  PRIMARY KEY (task_id, attempt_no)
);

-- task_runners：runner 执行记录（v1 的 tasks 定义快照 + attempts 物理执行合并承载；D-68/D-69）
-- task_runners.status: pending|queued|preparing|running|succeeded|failed|canceled|skipped
CREATE TABLE task_runners (
  task_id            text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_no         int  NOT NULL,
  seq                int  NOT NULL,                      -- 从 1 递增（含注入的 deploy/precheck/clean）
  name               text NOT NULL,                      -- Manifest runners[].name；注入行为 service-deploy / precheck / service-clean
  role               text NOT NULL CHECK (role IN ('test','pre_check','deploy','clean')),
  runner_name        text NOT NULL,                      -- 注册表 name（内置 runner 亦登记）
  runner_version     text NOT NULL,
  image_ref          text NOT NULL,
  profile            text NOT NULL,
  input_ref          text,                               -- 输入原文落档（attempts/<n>/input/<seq>/params.<ext>）
  env                jsonb NOT NULL DEFAULT '{}',
  secret_env         jsonb NOT NULL DEFAULT '[]',
  on_failure         text NOT NULL,
  timeout_seconds    int  NOT NULL,
  status             text NOT NULL,
  status_reason      text,
  waiting_reason     text,                               -- slot_waiting|global_limit|queue_full|insufficient_resources|gang_pending
  endpoint_key       text,                               -- 仅外接模式非空（C-4.3）；部署模式为 NULL
  endpoint_exclusive boolean NOT NULL DEFAULT false,
  cancel_requested   boolean NOT NULL DEFAULT false,
  exports_ref        text,                               -- 收割落档 attempts/<n>/exports/<seq>.exports.env
  job_id             text NOT NULL UNIQUE,               -- 幂等键 (task_id, attempt_no, seq) 的派生（附录 B）
  job_namespace      text,
  phase              text,                               -- 附录 B 的 phase 原值
  exit_code          int,
  started_at         timestamptz,
  finished_at        timestamptz,
  log_ref            text,
  artifact_ref       text,
  result_ref         text,
  error              jsonb,
  executor_ref       jsonb,
  image_digest       text,                               -- 实际拉取镜像 digest（G-16）
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT task_runners_name_uq  UNIQUE (task_id, attempt_no, name),
  CONSTRAINT task_runners_pos_uq   UNIQUE (task_id, attempt_no, seq),
  CONSTRAINT task_runners_behavior_ck CHECK (on_failure IN ('continue','abort_task'))
);
CREATE INDEX task_runners_ready_idx ON task_runners (task_id, attempt_no, seq) WHERE status = 'queued';

-- 不变量：一个 Task 的当前 attempt 内，同一时刻至多一个进行中的 runner（严格串行，D-69）
CREATE UNIQUE INDEX task_runners_active_uq ON task_runners (task_id, attempt_no)
  WHERE status IN ('preparing','running');

-- 外键：attempt_no 必须存在于 attempts（省略，由应用层保证；迁移里补复合外键）
CREATE TABLE state_events (
  id         bigserial PRIMARY KEY,
  task_id    text NOT NULL,
  attempt_no int,
  seq        bigint NOT NULL,                            -- 每 Task 单调，SSE / jsonl 共用
  type       text NOT NULL,
  from_status text,
  to_status  text,
  reason     text,
  payload    jsonb NOT NULL DEFAULT '{}',
  actor      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, seq)
);

CREATE TABLE endpoint_leases (                            -- 仅外接模式写入（D-69）
  task_id      text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  seq          int  NOT NULL,                            -- 持有槽位的 runner 执行记录
  endpoint_key text NOT NULL,
  exclusive    boolean NOT NULL,
  acquired_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, seq)
);
CREATE UNIQUE INDEX endpoint_lease_excl_uq ON endpoint_leases (endpoint_key) WHERE exclusive;
CREATE INDEX endpoint_lease_key_idx ON endpoint_leases (endpoint_key);

CREATE TABLE runners (
  name          text NOT NULL,
  version       text NOT NULL,
  display_name  text NOT NULL,
  description   text NOT NULL,
  role          text NOT NULL CHECK (role IN ('test','pre_check','deploy','clean')),  -- deploy/clean 仅内置行
  builtin       boolean NOT NULL DEFAULT false,          -- service-deploy / service-clean / endpoint-probe
  on_failure    text NOT NULL CHECK (on_failure IN ('continue','abort_task')),
  entry         jsonb NOT NULL,                          -- exec form 数组（附录 C v2，§5.2）
  input_format  text NOT NULL DEFAULT 'jsonl',           -- jsonl|yaml|text
  image         text NOT NULL,
  resources     jsonb NOT NULL,
  concurrency   jsonb NOT NULL,
  mounts        jsonb NOT NULL DEFAULT '[]',
  k8s           jsonb NOT NULL DEFAULT '{}',
  exports       jsonb NOT NULL DEFAULT '[]',             -- 导出声明（§5.12）
  consumes      jsonb NOT NULL DEFAULT '[]',             -- 消费声明（组合期校验）
  parameters    jsonb NOT NULL DEFAULT '{}',
  env_decl      jsonb NOT NULL DEFAULT '{}',
  placeholder   text,
  source_file   text NOT NULL,
  content_sha   text NOT NULL,                           -- 归一化后的内容指纹
  orphaned      boolean NOT NULL DEFAULT false,          -- 目录里消失但历史 Task 仍引用
  removed_at    timestamptz,
  registered_at timestamptz NOT NULL DEFAULT now(),
  registered_by text NOT NULL,
  PRIMARY KEY (name, version)
);

CREATE TABLE clusters (
  name              text PRIMARY KEY,
  api_server        text NOT NULL,
  kubeconfig_key    text NOT NULL,                       -- 挂载目录下的 `<key>.kubeconfig` 文件名，不存内容
  description       text,
  last_check_at     timestamptz,                         -- 最近一次连通性检查
  last_check_result text,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE execution_profiles (
  name                    text PRIMARY KEY,
  cluster                 text NOT NULL REFERENCES clusters(name),
  namespace               text NOT NULL,
  mounts                  jsonb NOT NULL DEFAULT '[]',   -- [{name, pvc, sub_path, kind}]
  scheduler               text NOT NULL DEFAULT 'default', -- default|volcano
  queue                   text,                          -- volcano queue
  image_pull_secrets      jsonb NOT NULL DEFAULT '[]',
  resource_defaults       jsonb NOT NULL DEFAULT '{}',
  deploy_timeout_seconds  int NOT NULL DEFAULT 259200,   -- 72h（K-6）
  network_policy          jsonb NOT NULL DEFAULT '{}',
  created_by              text NOT NULL,
  updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE secret_refs (
  name         text PRIMARY KEY,                         -- Manifest 里写的字符串
  namespace    text NOT NULL,
  secret_name  text NOT NULL,
  key          text NOT NULL,
  description  text,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artifacts (
  task_id    text NOT NULL,
  attempt_no int,
  seq        int,
  path       text NOT NULL,                              -- 相对 <storage-root>
  size_bytes bigint NOT NULL,
  sha256     text,
  kind       text NOT NULL,                              -- result|raw|log|report|other
  indexed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, path)
);

CREATE TABLE audit_log (
  id          bigserial PRIMARY KEY,
  actor       text NOT NULL,
  role        text NOT NULL,
  action      text NOT NULL,                             -- submit|cancel|retry|register|rotate|...
  object_type text NOT NULL,
  object_id   text,
  detail      jsonb NOT NULL DEFAULT '{}',
  at          timestamptz NOT NULL DEFAULT now()
);
```

### C-2.4 索引与查询模式

| 查询 | 索引 |
|---|---|
| 列表页按状态 + 时间倒序 | `tasks (status, submitted_at DESC)`、游标 `(submitted_at, id)` |
| 详情页按 Task 取 runner 管道 | `task_runners (task_id, attempt_no, seq)` |
| 调度候选 | `task_runners_ready_idx`（部分索引，只覆盖 queued） |
| SSE 回放 | `state_events (task_id, seq)` 唯一索引即覆盖 |
| 产物浏览 | `artifacts (task_id, attempt_no, seq)` |
| 审计检索 | `audit_log (at DESC)`、`(object_id)` |

列表接口一律游标分页：**不透明 `cursor`**（服务端编码 `(submitted_at DESC, id DESC)`，客户端不解析），不做 `OFFSET`——§12.7 的 P95 < 300ms 在 Task 数上万后靠它。日志是唯一例外（字节 `offset`）。

### C-2.5 事务边界

| 操作 | 一个事务内包含 |
|---|---|
| 提交 Task | 插入 task → 插入 attempt#1 + 全部 task_runners（含注入行）→ 插入 state_event(submitted) → 插入 audit |
| runner 推进 | CAS task_runners 状态 → 分配事件 seq → 插入事件 → （必要时）更新 waiting_reason |
| 启动 runner | （外接模式）取端点槽位 → 检查全局并发 → task_runners→preparing → 事件 |
| runner 结束 | CAS task_runners → 产物契约校验（D-74 三选一）→ 收割 exports 并写 exports_ref（§5.12）→ 下一 runner 置 queued（串行推进）或 abort_task 传播 → 事件 |
| 整 Task 重试 | 校验 attempt 上限 → 插入 attempt#n+1 + 全量 task_runners（fresh 快照）→ 事件（`ATTEMPT_EXHAUSTED` 拒绝在事务外） |
| 取消 | 更新 intent/状态 → 事件；Job 删除在事务外异步执行；clean 的排程同样落库（§7.6） |
| abort_task | 批量 SKIPPED（传播到 clean 为止）+ cancel_requested → 事件；实际删除 Job 异步 |

事务内**禁止**外部 IO（K8s、NFS）。所有外部调用先做、拿结果再进事务；失败就把结果变成状态（`failed` 等），不要悬挂事务。平台不直连 Genet——deploy/clean 的对外交互都发生在 runner Job 内（D-70）。

### C-2.6 迁移

- `golang-migrate`，只加不删；删除列要经过两个版本（先停写、后删）。
- 启动时 controller 校验 `schema_migrations` 版本，不匹配则拒绝启动（避免新旧代码写坏数据）。
- 状态枚举增加时：先扩 CHECK，再改代码，再收缩旧值（如果有）。
- **v2 结构迁移**：`runs`→`tasks`、内层 `tasks`+`attempts` → `task_runners` 是一次破坏性重组（D-66/D-68）；按"expand（建新表 + 回填）→ 切写 → contract（删旧表）"三段发布，保证"回滚镜像不需要回滚库"。

---

## C-3 状态机实现

### C-3.1 runner 转移表（唯一合法转移；对应 §7.1）

| from | to | 触发 | 附加动作 |
|---|---|---|---|
| `pending` | `queued` | 前一 runner 出终态（串行推进，C-5.4）或为管道第一个 runner | 计算 `waiting_reason` |
| `pending` | `skipped` | `upstream_abort`（传播到 clean 为止）/ `canceled`（同上） | 写 `status_reason` |
| `queued` | `preparing` | 拿到全局 permit +（仅外接 `endpoint_exclusive` 时）端点槽位 | 创建 Job（幂等键 `(task_id, attempt_no, seq)`） |
| `queued` | `skipped` | `upstream_abort` / `canceled`（传播到 clean 为止） | 释放（本就没有租约） |
| `preparing` | `running` | informer 观察到容器已启动 | 写 `started_at` |
| `preparing` | `failed` | Job 创建失败 / 镜像拉取失败 / 挂载缺失 | 记录错误码 |
| `preparing` | `canceled` | 取消 | 删除 Job，释放租约 |
| `running` | `succeeded` | 退出码 0 且产物契约满足（D-74：test/pre_check 产出 `result.json` / `result.html` / `result.csv` **至少其一**；`result.json` 存在时必须合法；`exports.env` 可解析） | 收割 exports（C-5.4）、生成 runner report |
| `running` | `failed` | 非零退出 / 超时 / 结果缺失或非法 / `EXPORT_INVALID` | 记错误码 |
| `running` | `canceled` | 取消 | — |

终态：`succeeded` / `failed` / `canceled` / `skipped`。**全部不可逆**。

> **没有重试回路（D-69）**：`failed → queued` 边不存在——v1 的 `retry_on` / `max_attempts` / backoff 机制随 v2 取消；runner 失败就是本次 attempt 的失败事实，重试 = 整 Task 新 attempt（C-5.6）。抖动自愈归 runner entry 内部（§5.3、§5.8）。
>
> `pending → canceled` 与 `queued → canceled` 不存在：按 §7.6，还没跑的 runner 取消记 `skipped(reason=canceled)`，**传播到 clean 为止**——service-clean 本身永不被 SKIPPED（finally 语义，C-5.7）。

### C-3.2 Task 转移表（对应 §7.4）

| from | to | 触发 |
|---|---|---|
| `submitted` | `queued` | 落库完成，进入编排 |
| `queued` | `running` | 编排器取到全局并发余量，开始推进 runner 管道 |
| `running` | `succeeded` / `failed` / `service_unavailable` | service-clean 终态后按 C-5.8 判定落终态 |
| 任意非终态 | `cancelling` → `canceled` | 用户取消（clean 执行完才落 `canceled`，C-6.1） |
| `running` | `failed` | 平台侧错误（Executor 不可用、内部异常、service-deploy 归类 `DEPLOY_FAILED`） |
| `succeeded` / `failed` / `service_unavailable` | `queued` | 用户重试（`POST /tasks/{id}/retry`，Command=Retry）：`attempt_no+1` 全量重跑含 fresh deploy（§7.8）；**`canceled` 不可重试**；超上限 409 `ATTEMPT_EXHAUSTED`（C-3.4） |

> **没有独立的 `deploying` / `testing` / `cleanup` / `gating` 平台阶段（D-70 / D-02）**：`RUNNING` 的内部推进 = runner 管道按序执行 `[service-deploy?] → [pre_check?] → [test…] → [service-clean?]`。部署就是管道第一个 runner 在跑（排队经 deploy-state.json 呈现），拆除就是最后一个 runner 在跑。前端按 runner 时间线呈现（W-4.2）。

### C-3.3 转移函数（唯一写入口）

```go
// 失败返回 ErrConflict，由调用方决定是幂等忽略还是上报。
func (s *Store) TransitionRunnerRun(ctx context.Context, k RunnerRunKey, from []RunnerStatus,
    to RunnerStatus, reason string, ev Event) (*RunnerRun, error) {

    tx, err := s.pool.Begin(ctx)               // 默认 ReadCommitted 足够：CAS + 行锁
    if err != nil { return nil, err }
    defer tx.Rollback(ctx)

    var r RunnerRun
    err = tx.QueryRow(ctx, `
        UPDATE task_runners
           SET status = $3, status_reason = NULLIF($4, ''), updated_at = now()
         WHERE task_id = $1 AND attempt_no = $2 AND seq = $5 AND status = ANY($6)
        RETURNING task_id, attempt_no, seq, status, status_reason, ...
    `, k.TaskID, k.AttemptNo, to, reason, k.Seq, from).Scan(...)
    if errors.Is(err, pgx.ErrNoRows) { return nil, ErrConflict }

    seq := nextSeq(ctx, tx, k.TaskID)          // UPDATE tasks SET last_event_seq = last_event_seq+1 RETURNING ...
    insertEvent(ctx, tx, k.TaskID, seq, ev)    // type=runner.state_changed, from/to, actor
    if err := tx.Commit(ctx); err != nil { return nil, err }

    s.notify(k.TaskID, seq)                    // LISTEN/NOTIFY payload 只有 task_id+seq
    return &r, nil
}
```

要点：

- **CAS 即并发控制**：所有状态更新都带 `status = ANY($from)`，竞态下后到者拿到 `ErrConflict`，按"先落库者胜"处理（§6.6）。
- **事件 seq 原子分配**：靠 `tasks.last_event_seq` 的行级自增，不用 `max(seq)+1`（避免读-改-写竞态）。
- 事务提交后才 `NOTIFY`，避免推出去的事件被回滚。

### C-3.4 不变量

| 不变量 | 保障方式 |
|---|---|
| 一个 attempt 内至多一个进行中的 runner（严格串行，D-69） | 部分唯一索引 `task_runners_active_uq` |
| 终态不可逆 | CAS 的 `from` 集合不含终态；夜间一致性任务报警 |
| `attempt_no` 从 1 递增且 ≤ 平台上限（默认 3） | 重试入口 `current_attempt+1`，超限 `ATTEMPT_EXHAUSTED` |
| `(task_id, attempt_no, seq)` 唯一 | `task_runners_pos_uq`（执行记录定位三元组，D-68） |
| 每个 Task 的事件 seq 连续且唯一 | `tasks.last_event_seq` + `UNIQUE(task_id, seq)` |
| Task 的 runner 集合与 resolved 快照一致 | 提交事务内一次性插入（含注入行）；`resolved` 可复核 |
| 重试不覆盖历史 attempt | 新 attempt 写新的 `attempts/<n+1>/` 行与目录；旧目录只读（§6.8 第 4 条） |
| service-clean 不被 SKIPPED | abort/取消的 SKIPPED 传播在 clean 处截止（§7.3、§7.6） |
| `queued` 的 runner 必须有 `endpoint_key`（仅外接模式） | C-4.3 调度守卫；提交时由静态 `service.url` 计算回填 |

### C-3.5 重启恢复与对账

controller 抢到 Lease 后（或每隔 30s）：

1. 扫描 `task_runners.status IN ('preparing','running')` 的执行记录，按 `job_id` 查 K8s：
   - Job 不存在且 `started_at < now-90s`：连续 3 次（约 1 分钟）确认后判 `failed / EXECUTOR_UNAVAILABLE`，事件标注 `reconcile_exhausted`（不直接在一次查询后判失败）。
   - Job 存在但 phase 与库不一致（Pod 已成功/失败）：以外层事实推进状态，补写 `finished_at` 与退出码。
2. 清理无 attempt 归属的 `preparing`（说明事务已回滚）：按串行规则重排（退回 `queued`）。
3. 校验 `tasks.status IN ('queued','running','cancelling')` 是否仍有推进条件；若 service-deploy 的 Job 已丢失而 Task 未终态 → 按 C-6.4 收口并排程 service-clean，不重新发起部署（fresh-deploy 只经整 Task 重试入口，§7.8）。
4. 恢复未完成的 `cancelling`：再次投递删除 Job 的动作，并确认 service-clean 已排程。

对账只读 K8s 与归档文件（deploy-state.json 副本），**不重建内存状态**——所有状态都在库里。

---

## C-4 调度

### C-4.1 触发

调度循环两种触发方式：`NOTIFY`（提交、runner 终态、取消、重试）即时唤醒 + 1s 定时兜底。每轮：

1. 取候选（C-4.2，串行管道下每个进行中 Task 至多一个 `queued` runner）→ 2. 尝试取全局 permit，仅外接模式 `endpoint_exclusive` 时再取端点槽位（C-4.3/C-4.4）→ 3. 成功则 `queued → preparing` 并交给 executor 建 Job；失败写 `waiting_reason` 后结束本轮。

### C-4.2 候选查询

```sql
-- 候选不加行锁：并发由后续 CAS 与 Task 行锁兜住；串行管道下每 Task 至多一条 queued
SELECT rr.task_id, rr.attempt_no, rr.seq, rr.endpoint_key, rr.endpoint_exclusive
  FROM task_runners rr
  JOIN tasks t ON t.id = rr.task_id
 WHERE rr.status = 'queued'
   AND t.status = 'running'
 ORDER BY t.submitted_at, rr.seq
 LIMIT 50;

-- 逐候选：先锁 Task（统一顺序 Task → task_runners，避免与取消/abort 死锁）
SELECT id FROM tasks WHERE id = $task_id FOR UPDATE;
```

> 早期实现用 `FOR UPDATE OF rr SKIP LOCKED` 锁候选行，会与"取消/abort 先锁 Task 再批量改 task_runners"形成反向顺序、可能死锁。现统一为 **Task 行锁 → advisory（端点 → 全局）→ CAS task_runners**；`SKIP LOCKED` 只用于对账等非状态转移的读。

`CAS`（`UPDATE task_runners SET status='preparing' WHERE task_id=$1 AND attempt_no=$2 AND seq=$3 AND status='queued'`）保证多个调度实例不重复领取；拿不到就跳过该候选，下轮重来。Task 行锁保证同 Task 的管道推进与取消/abort 串行，不会出现反向加锁顺序。

### C-4.3 端点槽位（v2 收窄：仅外接模式，§6.7 / D-69）

> **部署模式不建租约**：服务由本 Task 的 service-deploy 拉起、Task 私有，隔离由"Task 私有服务 + 管道串行"这一拓扑保证；`endpoint_exclusive` 声明为冗余但无害。只有**外接模式**（多个 Task 可能指向同一外部服务）才需要互斥——且只对声明 `concurrency.endpoint_exclusive: true` 的 runner 生效（探活与测量不得共享服务）。

以 **`endpoint_key` 的 advisory 事务锁**串行化同一端点的判定，再操作 `endpoint_leases`：

```sql
SELECT pg_advisory_xact_lock(hashtext($endpoint_key));   -- 冲突只影响并发度，不影响正确性

-- 我要独占时，是否有任何持有者？
SELECT 1 FROM endpoint_leases WHERE endpoint_key=$1 LIMIT 1;

INSERT INTO endpoint_leases (task_id, seq, endpoint_key, exclusive) VALUES ($task, $seq, $key, $excl);
```

判定规则：`endpoint_exclusive` 的 runner 在"任何持有者存在"时拒绝排队；非独占 runner 在外接模式下同样不与同 Task 串行冲突（同 Task 内本来严格串行）。runner 执行记录终态时 `DELETE ... WHERE task_id=$task AND seq=$seq`（与状态推进同事务，避免泄漏）。

`endpoint_key` 的计算（见 C-8.5）：外接模式取 `service.url` 的规范化值（`scheme://host:port`，不解析路径、不跟随重定向）；部署模式不计算（无租约）。

### C-4.4 全局并发上限

```sql
SELECT pg_advisory_xact_lock(hashtext('argus:global-concurrency'));
SELECT count(*) FROM task_runners WHERE status IN ('preparing','running');  -- < AG_CONCURRENCY_MAX
```

代价：每个 runner 启动都串行一次（毫秒级），20 并发的量级足够。若未来需要更高吞吐，改为 `slots` 计数表 + 条件更新，接口不变。

### C-4.5 管道顺序与 QUEUED 语义（v2 取代"门控 + 并发"，D-69）

> **Task 内 runner 严格串行（线性管道）**：编排器按 `seq` 推进——前一 runner 出终态后，下一个 runner 才进入 `queued`。v1 的"Pre-Check 门控 + 其余 Task 按 `endpoint_exclusive` 并行"模型取消（K-12 随之作废）；隔离由"Task 私有服务 + 串行"拓扑保证，不再依赖并发声明。

| 场景 | 行为 |
|---|---|
| 管道推进 | runner `seq=n` 出终态 → 同事务把 `seq=n+1` 置 `queued`（若存在）；失败按 `on_failure` 决定继续（C-5.4） |
| pre_check runner | 就是管道中第一个测试类 runner（`precheck: auto` 注入），串行天然保证"先探活后测试"，无需门控状态 |
| pre_check 失败 | `on_failure: abort_task`（注册强制）：后续 runner 置 `skipped(upstream_abort)`，**传播到 clean 为止**；随后执行 service-clean，Task 终态按 C-5.8 判 `service_unavailable` / `failed` |
| 外接模式独占 runner | 独占该 Endpoint 槽位期间，**其他 Task** 指向同一 `endpoint_key` 的独占 runner 排队（`slot_waiting`） |

> **提交下限（D-57）**：`runners[]` 至少 1 条，与 precheck 取值无关（pre_check 是平台注入的 runner，不是用户测试）。0 条直接 400 VALIDATION_ERROR，不落库、不建 Task。

调度守卫：外接模式下 runner 进入 `queued` 前必须 `endpoint_key IS NOT NULL`（提交时由 `service.url` 计算）；违反即拒绝调度并告警，**不得**用占位键绕过。部署模式不校验（无租约）。

### C-4.6 公平性

FIFO 按 `(task.submitted_at, task_runners.seq)`。不做优先级、不做抢占——平台没有调度决策权（§6.9）。同一 Task 内由串行管道决定先后；跨 Task 才比较提交时间。

### C-4.7 waiting_reason

`QUEUED` 必须有原因（§6.9）。调度每轮对未拿到槽位的 runner 更新：

| 值 | 判定 |
|---|---|
| `slot_waiting` | 外接模式：同 Endpoint 上有其他 Task 的独占持有者 |
| `global_limit` | 全局并发已满 |
| `queue_full` / `insufficient_resources` / `gang_pending` | K8s/Volcano 侧状态，来自 E-11 的只读快照 |

> v1 的 `precheck_gate` 取消（D-69 无门控；`PENDING` 状态本身就是"等前序 runner"）。`slot_waiting` / `global_limit` 是平台自身的等待，K8s 三项是集群侧等待——分开呈现，用户才知道该找谁。**部署排队（Genet 侧）不属于 `waiting_reason`**：它由 service-deploy runner 的 deploy-state.json（`phase` / 水位 / 已等待时长）承载，在 deploy 节点详情呈现（§6.9、E-9）。
>
> `PENDING` 的 runner 无需 waiting_reason：串行管道下它就是在等前一个 runner，UI 按"管道未到"展示。

### C-4.8 `GET /api/v1/queue-status` 的数据来源（两套排队，分开呈现）

同一个端点里有两类**互不换算**的排队，响应结构也按它们分成 `slots` / `waiting` / `volcano` 三层（`api/openapi.yaml §QueueStatus`）：

| 层 | 数据源 | 语义 | 时间尺度 | 可用性 |
|---|---|---|---|---|
| `slots` + `waiting`（`slot_waiting` / `global_limit`） | **本进程 + DB**（`endpoint_leases`、`task_runners` 计数与 `waiting_reason`） | 平台 permit 与外接模式端点独占 | 秒~分钟 | 永远可读 |
| `volcano`（`queue_full` / `gang_pending` / `insufficient_resources` 的原始水位） | **集群 CRD 直读**：`scheduling.volcano.sh/v1beta1` 的 `queues` 与 `podgroups` | 集群资源配额与 gang 调度 | 小时~天 | 可能不可达 → 降级 |

**为什么必须分开**：归责对象相反（平台 vs 集群管理员）、时间尺度差 1~2 个数量级、单位不同（permit 计数 vs 卡/CPU 配额）、可用性不同（DB 永可读 vs CRD 可能不可达）。合成一个"总排队 N"是错的，UI 也必须是两张独立的卡。

**C-4.8.1 读什么（CRD 字段路径）**

不引 `volcano.sh/apis`（避免被 Volcano 版本绑死），用 `dynamic.Interface` + `unstructured`（与 Genet 同策略，`<G>/backend/internal/k8s/volcano.go:32-48`）：

```go
const volcanoGroup = "scheduling.volcano.sh"
var (
    queueGVR    = schema.GroupVersionResource{Group: volcanoGroup, Version: "v1beta1", Resource: "queues"}
    podGroupGVR = schema.GroupVersionResource{Group: volcanoGroup, Version: "v1beta1", Resource: "podgroups"}
)
```

| 目标字段 | CRD 路径 | 说明 |
|---|---|---|
| `queues[].name` | `metadata.name` | 统一写成 `"<cluster>/<queue>"`（零契约变更）：cluster 取 `AG_CLUSTER_NAME`（默认 `cluster-01`），**P0 单集群也带前缀**，切多集群时 UI/契约不用改；cluster 为空则保持原名 |
| `queues[].state` | `.status.state` | `Open` / `Closed`；未知值原样透传，不硬映射成 `Closed` |
| `queues[].capability` | `.spec.capability` | `map[string]string`，**原文透传不做单位换算**；⚠ 队列未设 CPU/内存配额时 Volcano 会兜底成极大值，不可直接当进度条分母 |
| `queues[].allocated` | `.status.allocated` | 控制器回写、**有滞后**；无 `/metrics` 接入时只能用它，`detail` 注明来源 |
| `queues[].pending_jobs` | 该 queue 下 `podgroups[].status.phase == "Pending"` 的计数（PodGroup 的 namespace 范围由 `AG_VOLCANO_NAMESPACES` 决定，默认 profile 的 namespace；`spec.queue` 为空的 PodGroup 归 `default` 队列） | **不要用** `queue.status.pending`（Volcano 自己的计数，语义可能含 Inqueue） |
| `queues[].longest_wait_seconds` | `now - min(podgroup.metadata.creationTimestamp)`，限定同 queue 且 `phase ∈ {Pending, Inqueue}` | 无排队 → `0`，不是负数、也不回退成队列创建时间 |
| `queues[].reason` | 同 queue 下 `phase ∈ {Pending,Inqueue}` 里**等得最久**那条的 `status.conditions[-1].message`（空则 `.reason`），**原文透传** | 归一成 `waiting_reason` 六值是 `waiting.by_reason` 的职责（需 `Task→PodGroup` 关联，见 C-4.8.4 #4），MVP 不做。契约已按自由文本落定：`server/api/openapi.yaml` 的 `VolcanoQueue.reason: type [string,'null']`，`design/views/07-api.md §5.3` 同步说明 |

**C-4.8.2 采样方式**

| 项 | 决定 | 理由 |
|---|---|---|
| 轮询 + 15s TTL 缓存，**不用 informer** | 与 `execution.md:412` 的既有约束一致 | 规模小（队列 3 个、PodGroup 数十~数百）；informer 要 watch 语义与常驻 reflector，与"15s 新鲜度"不匹配，多集群还更重 |
| **单飞**（`sync.Mutex` + `inflight`） | 并发请求最多一个在打 API | `/queue-status` 是 30s 轮询的公共端点，看板一开就是多客户端同时打 |
| 单查超时 5s；`/queue-status` 整体 3s 内必须返回 | 火山块失败即降级，不等 | E-11 |
| 失败退避 60s（连续失败后） | 退避窗口内不再打 API，仍回降级快照 | 集群不可达时不要每 15s 打一次（06 §2.1（3）） |
| PodGroup 的 namespace 范围 | `AG_VOLCANO_NAMESPACES`（逗号分隔；默认 profile 的 namespace；`*` = 全部，一次集群级 List） | 只统计测试命名空间，避免把别的租户的 PodGroup 算进 `pending_jobs` |
| 先 `queues` 后 `podgroups` | 小的先取；后者失败即整体 `available:false` + `queues:[]`（与 openapi 一致） | 大 List 更容易超时 |
| RBAC | **只 `get,list`**；Queue 是 cluster-scoped → 必须 `ClusterRole`（`argus-volcano-view`，见 06 §2.1） | 轮询不需要 `watch`；不能给 `queues/status` 写权限（平台只读，不改配额、不插队） |
| 实现落点 | `server/internal/k8s/volcano.go`（`k8s.Volcano` 实现 `api.VolcanoProvider`），`cmd/argusd/main.go` 注入 `api.Deps.Volcano`；单测 `server/internal/k8s/volcano_test.go` 用 `client-go/dynamic/fake` 覆盖字段映射、四态、TTL、单飞、退避 | 生产代码不引 `volcano.sh/apis`，测试也不引 |

**C-4.8.3 降级四态（红线：不可达 ≠ 0 排队）**

| 情形 | `volcano` 块 | 其他块 |
|---|---|---|
| CRD 未安装（List 返回 `IsNotFound` / `NoMatch`；结果缓存 5 分钟*） | `available:false, detail:"crd_missing", queues:[]` | 照常 |
| List 返回 `apierrors.IsForbidden` | `available:false, detail:"rbac_forbidden", queues:[]`（不吐原始 RBAC 报错，避免泄露 SA/角色名） | 照常 |
| 超时 / 连不上 | `available:false, detail:"scheduler_unavailable", queues:[]`；上次成功过则 `stale:true`（**不回放旧队列数字**） | 照常 |
| `AG_VOLCANO_ENABLED=false`（平台未启用队列调度） | `available:false, detail:"disabled", queues:[]` | 照常 |

\* 404/discovery 结果的 5 分钟缓存**尚未实现**：当前由 15s TTL + 60s 退避间接覆盖，且不额外引 discovery（少一次与 RBAC 无关的调用）。

- **`stale` 没有时间阈值**（不是"超过 2×TTL 未刷新"）：判定条件是"**上一次刷新成功、本次刷新失败**"，与数据多旧无关；本次成功即清空。它挂在 `volcano` 块（快照）层级、**不是每个队列一份**，且置位时 `available:false` + `queues:[]` 照旧——`stale` 只表达"这个块曾经采到过"，绝不拿旧队列数字充数。
- 队列存在但无 PodGroup：`available:true`，`pending_jobs:0`、`longest_wait_seconds:0`、`reason:null`——UI 显示 `—`，**不要**写"0 个排队"这种绝对断言。
- CRD 里没有的队列不列（列事实，不列配置期望）。
- **任何降级都返 `available:false` + `queues:[]`**（唯一的降级出口 `volcanoUnavailable`），且不得影响 `slots` / `waiting`，也不得让 HTTP 非 200（500 只留给真正的平台故障）。

**C-4.8.4 MVP 边界（未确认项先不实现）**

1. `capability` 能否当分母：CPU/内存未设配额时是极大值 → **MVP 只透传**，UI 进度条用加速卡维度且需实机核对；
2. `queue_full` 的完整判定要 `deserved`（只在调度器内存 metrics 里，CRD 没有）→ **MVP 只按 `state=Closed`（且 queue 内有 Pending）判**，`detail` 写 `deserved_unavailable`；
3. `gang_pending` 要 `PodGroup.status.running`，该字段未被 Genet 侧代码读取过 → 双路判（有 `status.running` 用它比 `minMember`，否则退回 `phase=Pending` + conditions 关键词），**字段缺失不得判成"gang 已满足"**；
4. `waiting_reason` 的 K8s 三项需要 `Task → Job → Pod → PodGroup` 关联链路，当前编排层没有 → 先只做 `volcano` 展示层，`waiting.by_reason` 的 K8s 项保持 0 并在 `detail` 说明，**不造数**。

**C-4.8.5 配置（env，以代码为准）**

| env | 默认 | 说明 |
|---|---|---|
| `AG_VOLCANO_ENABLED` | `true` | 关闭 → `detail:"disabled"`（**不是** `crd_missing`，也不是"0 排队"） |
| `AG_VOLCANO_NAMESPACES` | profile 的 namespace（未配置时 = `AG_EXECUTOR_NAMESPACE`，默认 `argus`） | 逗号分隔；`*` / `all` = 全部 namespace（一次集群级 List）。**它决定 `pending_jobs` / `longest_wait_seconds` / `reason` 统计哪些 namespace 的 PodGroup**：不在列表里的 namespace（例如 `argus-mock` 里的 PodGroup）**不计入**任何队列；多个 ns 会逐个 List 后合并，重复项去重 |
| `AG_VOLCANO_TTL` | `15s` | 快照 TTL；非法值启动即失败（fail fast） |

本地演示/联调用夹具（12 个 Volcano CRD + 2 Queue + 5 PodGroup，**只装 CRD 不装 scheduler**，status 为手工假水位）见 [`deploy/k8s/volcano-fixtures/README.md`](../../deploy/k8s/volcano-fixtures/README.md)：装上后 `/queue-status` 的 `volcano` 块为 `available:true` 的真实 CRD 数据；删掉即回到 `crd_missing`。

---

## C-5 编排流程

### C-5.1 提交校验顺序

按顺序执行，遇到第一个错误即返回（字段级定位）；同一层可聚合多个 issue。

| 序 | 校验 | 错误码 |
|---|---|---|
| 1 | 请求体大小（Manifest ≤ 1 MiB）、YAML 可解析 | `VALIDATION_ERROR` |
| 2 | `manifest.schema.json` 结构校验（必填、类型、枚举、互斥；顶层 `task:`） | `VALIDATION_ERROR` |
| 3 | `deploy` XOR `service`；两者都缺/都给 → 拒绝（§10.1） | `VALIDATION_ERROR` |
| 4 | `runners[]` 数量 ∈ [1, 200] | 0 条 / 超 200 → `VALIDATION_ERROR` / `TASK_TOO_LARGE` |
| 5 | `runners[].runner + version` 命中白名单（`deploy`/`clean` 角色不可引用——由平台自动注入） | `RUNNER_NOT_FOUND` |
| 6 | `execution.profile` 已登记 | `EXECUTION_PROFILE_NOT_FOUND` |
| 7 | `input` 通过该 Runner 的 `input_format` 校验（C-8.4：jsonl 逐行 JSON.parse + 对象 + 按 `parameters` 校验类型与 required，报错带**行号 + 字段名**；yaml/text 仅语法可解析） | `VALIDATION_ERROR` |
| 8 | exports/consumes 闭合：每个 runner 的 `consumes` key 必须有上游导出、静态 `service` 值或 secret 来源；任何两个 runner 的 `exports` **不允许同名 key**（§5.12、D-71） | `VALIDATION_ERROR` |
| 9 | `on_failure` 不得弱化 `runner.yaml.on_failure`；`pre_check` 与内置 deploy/clean 必须 `abort_task` | `VALIDATION_ERROR` |
| 10 | `required` 的 env（Runner 声明）都有来源；`secrets` 引用存在 | `VALIDATION_ERROR` |
| 11 | `precheck: auto` 时注册表恰好一个 `role=pre_check`（0 个或多个都拒绝） | `VALIDATION_ERROR` |
| 12 | `runners[].timeout_seconds` ≤ 24h；`task.name` ≤ 200 字符（D-58）；attempt 上限为平台配置（默认 3，不进 Manifest） | `VALIDATION_ERROR` |

> ~~`args` 词法切分~~ 与 ~~`cleanup.policy` 枚举~~ 两项随 v2 取消（D-66/D-70）：业务输入走输入文件、有部署必有 clean（finally）。

通过后：生成 `resolved` 快照（**runner 管道序列**：注入的 service-deploy / precheck / service-clean 及各自 seq、各 runner 的输入落档路径、默认超时、导出注入清单初值）→ 落库（事务：task + attempt#1 + 全部 task_runners）→ `submitted` 事件 → 转 `queued` → NOTIFY。

`endpoint_key` 的取值时机：

- 外接模式（`service`）：提交时即可规范化计算，写入该 Task 全部 runner（"序号 0 的导出"）；
- 部署模式（`deploy`）：**不计算**（服务 Task 私有，无端点租约，C-4.3）；
- 外接模式调度守卫：只接受 `endpoint_key` 非空的 `queued` runner。

**校验只做词法/结构与引用，不做参数语义校验**（§5.4 / §13.1）：不校验 `dataset` 路径是否存在、不校验取值范围/枚举/跨字段（归 Runner）；`parameters` 未声明的 JSONL 字段放行。

### C-5.2 管道组装与 precheck 注入

提交后 orchestrator 把 Manifest 组装成**线性管道**（每个 runner 分配 `seq`，从 1 递增）：

```text
[有 deploy]  seq=1  service-deploy   （内置，role: deploy；输入 = Manifest deploy 描述，input_format: yaml）
[precheck≠off] seq=+1  <pre_check runner>（auto：注册表唯一 role=pre_check；name=precheck）
seq=+1…   用户 runners[]（仅 test / 显式 pre_check）
[有 deploy]  seq=+n  service-clean    （内置，role: clean；finally，C-5.7）
```

| 模式 | 行为 |
|---|---|
| `auto`（默认） | 取注册表唯一 `role=pre_check` 的 Runner；`name=precheck`；`input` 为空、`parameters: []`（公共参数经 env 注入）；`execution.profile` 复用首个 test runner 的 profile；`on_failure` 取 Runner 声明（必为 `abort_task`） |
| `off` | 不注入 pre_check。用户若在 `runners[]` 里显式列出 `role: pre_check` 的 Runner，按它自己的 `on_failure=abort_task` 处理，不额外特判 |
| 显式对象 | 提交者指定 runner/version/input/profile/timeout；`on_failure` 仍不得弱化 |

deploy / clean 不可被用户引用或手动添加：`runners[].runner` 命中 `role ∈ {deploy, clean}` → `RUNNER_NOT_FOUND`（§10.1）；两者由平台按 `deploy` 字段**自动配对注入**（提交了部署描述 → 前置 service-deploy、追加 service-clean；D-70）。

命名冲突（已有 `runners[].name == "precheck"` / `"service-deploy"` / `"service-clean"`）→ `VALIDATION_ERROR`。

### C-5.3 部署模式推进：service-deploy runner 与 deploy-state.json（v2 重写，D-70）

> **v1 的"平台 Deploy 阶段 + Genet 适配层轮询"取消**：与 Genet 的全部交互发生在内置 **service-deploy runner**（`runners/service-deploy/`）内部，平台不再直连 Genet。编排侧把 service-deploy 当普通 runner 推进，部署的可见性与状态经 runner 周期改写的 **`deploy-state.json`** 承载（Genet 交互契约见 E-9；`runs.deploy` 的 release 运行态列随 v1 适配层取消，运行态归 `attempts/<n>/deploy-state.json` 副本）。

**C-5.3.1 管道中的部署**

| 步骤 | 动作 | 依据 |
|---|---|---|
| 1 | 提交时把 Manifest 的 `deploy` 描述原样写入 service-deploy 的输入（`attempts/<n>/input/<deploy_seq>/params.yaml`，`input_format: yaml`，平台不解析语义）；`resolved` 冻结 Genet 部署描述（values/template/image/model_path/set） | §9.1、§10.1 |
| 2 | releaseName 由 **runner** 按 `arg- + sha256(task_id:attempt_no)[:12]` 派生（D-44，v1 前缀 `tp-` 废止）；确定性派生保证重放 clean 与幂等安装可用同一名字 | §9.1、E-9 |
| 3 | service-deploy 调 Genet OpenAPI 创建 release（`noWait=true` 恒真）并轮询，把 `phase ∈ queued|accepted|deploying|ready|blocked` + 水位 + `waited_seconds` + `label_selector` + pods 快照周期写入 `$AG_OUTPUT_DIR/deploy-state.json` | §9.1、E-9.2 |
| 4 | 服务 ready → runner 导出四件套 `AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH` / `AG_DEPLOY_REF`（releaseName 等）并退出 0 → 平台收割 exports（C-5.4）注入后续 runner | §5.12 |
| 5 | 失败 → runner 在 deploy-state.json 写明归类并退出非零：Genet 调用失败 → `DEPLOY_FAILED`（不可重试）；`blocked` / 超部署窗口（默认 72h，`AG_DEPLOY_TIMEOUT`）→ `DEPLOY_BLOCKED`（`detail.reason ∈ {blocked,timeout,superseded,uninstalling,uninstalled}`，可重试） | 附录 D、D-47 |

**C-5.3.2 deploy-state.json 的平台侧读取与归档**

- **读取**：controller 在 service-deploy `RUNNING` 期间周期（≈30s）读取其输出目录的 `deploy-state.json`（产物直落 NFS，无需进容器）；runner 每 ≈30s 原子改写（temp + rename），平台读到的是任意完整快照，解析失败不判失败、下轮重读。
- **归档**：每次成功读取同步一份到 `attempts/<n>/deploy-state.json`（`attempts.deploy_state_ref`）；runner 终态后做最后一份归档。UI 的部署状态卡（`phase` / 水位 / 已等待时长 / Pod 清单）与 `deploy.state_changed` 事件都以该副本为数据源。
- **归责文案**：`queued / accepted / deploying` 的切分是 runner 的启发式（Genet 无受理事件），UI 与排障手册标注"平台侧推断"（E-9 R3）。
- **部署排队可见性（D-46）**：水位有、位次无；`deploy-state.json` 不含位次字段。
- **`internal/genet` 适配层瘦化为 `internal/deploystate`**：deploy-state.json 的结构校验 + 归档 + 投影，**不再有任何 Genet HTTP 客户端**（v1 的 `ports.Deployer` Start/Poll/Cancel 取消）。genet-local（假 Genet）保留为本地联调后端，由 service-deploy runner 通过 env 指向（E-9.7）。

**C-5.3.3 ready 之后：服务门**

`deploy-state.json` 的 `phase=ready` **只代表地址解析出来了**（D-45 沿用），不保证能推理。放行测试的判据不变：pre_check runner 真正探活通过。区别在于 v2 里这就是**串行管道的自然顺序**——service-deploy（seq=1）→ precheck（seq=2）→ tests，无需门控状态机。`precheck: "off"` 时没有服务门：deploy 一过直接跑测试，报告里必须显示"未做服务探活"。

**C-5.3.4 部署期间的取消与 clean 兜底**

取消 Task 时（C-6.1）：正在 RUNNING 的 service-deploy Job 被终止（`CANCELED`）→ 管道跳到 service-clean。clean 的输入兜底：

- deploy 已导出 `AG_DEPLOY_REF` → 按导出拆除（幂等，`GET 404` = 成功）；
- deploy 未导出（排队中被取消 / 调用失败）→ clean 用确定性 releaseName（`arg- + sha256(task_id:attempt_no)[:12]`）调 Genet DELETE，release 不存在即成功——"没有可泄漏的东西"。该兜底仅写进 runner 的实现契约（E-9.3），平台侧不感知 Genet。

**C-5.3.5 失败归类与 Task 终态**

- `DEPLOY_FAILED`（Genet 调用失败：连接/5xx/同名冲突/安装失败，`detail.reason` 保留 Genet 原文）→ Task `failed`（平台侧，§7.5）；
- `DEPLOY_BLOCKED`（Genet `blocked` 或超 72h 窗口）→ Task `service_unavailable`（§7.5）；
- 部署失败/阻塞时管道内未执行的 runner 置 `skipped(reason=upstream_abort)`，**随后仍执行 service-clean**（幂等兜底），Task 才落终态。

### C-5.4 RUNNING 管道推进与 exports 收割

Task 进入 `running` 后，编排器按 `resolved` 快照中的管道序列推进：

- 初始：`seq=1`（有部署时即 service-deploy，否则第一个测试类 runner）置 `queued`；
- runner `seq=n` 出终态（同事务）：
  1. **exports 收割流水线**（仅 `succeeded`，D-71）：
     a. 读 `$AG_OUTPUT_DIR/exports.env`（= `attempts/<n>/runners/<seq>/exports.env`）；
     b. **词法校验**：逐行 `KEY=VALUE`、`#` 注释、值不含换行；解析失败的行整体拒绝收割 → runner 改判 `failed`（`EXPORT_INVALID`）；
     c. **落库落档**：写入 `attempts/<n>/exports/<seq>.exports.env` 与 `task_runners.exports_ref`，并记录注入清单（key、来源 seq、值）——事后可完整还原每次注入（§5.12）；
     d. **合并注入**：收割到的 KV 并入该 attempt 的"当前导出集合"（按管道线性顺序合并）；`seq=n+1` 的 Job env 由该集合 + Task 级 env/secrets 渲染（C-8.5）。
     失败（退出码 ≠ 0）或 SKIPPED 的 runner **不收割**。
  2. 失败且 `on_failure=abort_task`（Runner 与 Manifest 取严）→ 执行 C-5.5，SKIPPED 传播到 clean 为止，随后排程 service-clean；
  3. 失败但 `on_failure=continue` → 不影响后续 runner，该 runner 记 `failed`；Task 终态按 C-5.8 聚合（存在失败 → `failed`，v1 的 `COMPLETED_WITH_ERRORS` 语义并入，D-73）；
  4. `seq=n+1` 存在且未被 SKIPPED → 置 `queued`（严格串行：同一时刻至多一个非终态 runner，`task_runners_active_uq` 兜底）；
  5. 管道走完（最后一个 runner 出终态）→ 若含部署且 service-clean 尚未执行，排程 service-clean（C-5.7）。

> **"取值点统一"的实现**：外接模式的静态 `service` 三元组在提交时作为**"序号 0 的导出"**进入导出集合，部署模式由 service-deploy 的 exports 提供同一组 key——下游 runner 的 env 渲染逻辑完全一致（D-27）。

### C-5.5 abort_task

```sql
-- 同事务：SKIPPED 传播（严格串行下未执行的 runner 必为 pending/queued；传播到 clean 为止）
UPDATE task_runners SET status='skipped', status_reason='upstream_abort', updated_at=now()
 WHERE task_id=$task AND attempt_no=$att AND seq > $failedSeq
   AND status IN ('pending','queued') AND role <> 'clean';

UPDATE task_runners SET cancel_requested=true, updated_at=now()
 WHERE task_id=$task AND attempt_no=$att AND seq > $failedSeq
   AND status IN ('preparing','running') AND role <> 'clean';
-- clean（role='clean'）保持 pending：abort 后由编排器排程 service-clean（C-5.7）
```

异步动作：对 `cancel_requested` 的 task_runners 调用 executor 删除 Job（E-5），执行记录置 `canceled`。**不等**删除完成才推进 clean（§7.6 的"尽力而为、不阻塞"）。

### C-5.6 整 Task 重试（v2 新语义，D-69 / §7.8）

- **runner 级没有重试**：v1 的 `retry.max_attempts` / `retry_on` 白名单 / backoff 机制取消；抖动自愈（连接重试、等待就绪）由 runner entry 内部完成（§5.3、§5.11）。
- **重试 = 整 Task 新 attempt，全量重跑，含 fresh deploy**：`POST /tasks/{id}/retry`（Task 任意终态后可用）→ 校验 `attempt_no < 上限（默认 3）`，超限 409 `ATTEMPT_EXHAUSTED` → 同事务插入 `attempts[attempt_no+1]` + **全量 task_runners 快照**（含重新注入的 deploy/precheck/clean）→ 管道从头推进。
- **不得复用上一次 attempt 的已部署服务**（warm cache、残留连接与 KV cache 污染测量，对 `aisbench-prefix` / `aisbench-perf` 是正确性问题）：新 attempt 的 service-deploy 重新拉起服务，releaseName 按 `arg- + sha256(task_id:attempt_no)[:12]` 重新派生，与历史 attempt 天然不冲突。
- **历史不覆盖**：旧 attempt 的记录与 `attempts/<n>/` 目录只读；Task 级报告默认取最后 attempt，UI 可切换（§7.2）。
- 外接模式同样全量重跑（无"复用静态 service"的捷径——静态值虽相同，但执行与产物按新 attempt 重新落档）。
- 跨 Task 续跑（`retry_of` 关联、复用未失败 runner 的结果）是 **P1 能力，仅限外接模式**（§7.7）；v1 的同 Run 追加 Attempt 与 `reused/` 硬链接机制随 v2 取消。

### C-5.7 clean finally 与 Task 收尾（v2 重写，D-70）

**`cleanup.policy` 取消**：提交了 `deploy` 就必有 service-clean（平台自动追加），finally 语义——成功、失败、取消、超时四条路径全部执行；唯一不执行的路径是平台自身崩溃 / 节点丢失（clean Job 未被调度）→ 走 §9.3 泄漏重放兜底。

顺序（实现以此为准）：

1. 管道走完（成功 / abort / continue 失败）或收到取消/超时 → 编排器把 service-clean 置 `queued`（若它还是 pending）并正常调度执行——clean 与其他 runner 走**同一条** runner 通道（同一状态机、同一 Job 形态）；
2. clean runner 调 Genet 拆除（幂等：资源不存在 = 成功；判干净 = `GET 404` + pods 空，契约见 E-9.3）；
3. clean 终态落库后，Task 才按 C-5.8 落终态；
4. clean 自身失败：记 `cleanup_failed`（Task 报告字段）+ `CLEANUP_FAILED` 事件 + 指标，**不改变 Task 终态**（§7.6），进入 §9.3 重放清单（对账扫描"已终态、含部署、无 clean 成功记录"的 attempt）；
5. **异步**生成 Task 报告（失败重试，不改终态）→ `report.written`。

理由：clean 是 runner 而非平台阶段，平台事务里没有 Genet 调用；"先落 runner 事实、clean 结束才落 Task 终态"保证终态语义完整（§7.5 的 `canceled` 定义 = "收到取消请求**并完成 clean 与资源回收**"）。collector 停止采集部署 Pod 日志的时机 = clean 完成或 Task 终态（§8.6），不再是"进入 CLEANUP 阶段"。

### C-5.8 终态判定优先级（§7.5）

前置：service-clean 已出终态（finally，C-5.7）。从上到下，先命中者胜：

```text
1. 用户取消完成                  → canceled
2. 平台侧错误（Executor 不可用持续、内部异常、service-deploy 归类 DEPLOY_FAILED）→ failed
3. service-deploy 归类 DEPLOY_BLOCKED（受理但未就绪 / blocked / 超窗）
   或 pre_check runner 失败**且错误可重试**（ENDPOINT_UNREACHABLE / PRECHECK_FAILED / RUNNER_TIMEOUT）→ service_unavailable
4. 所有非 SKIPPED 的测试类 runner 均 succeeded → succeeded（clean 失败不影响，只打 cleanup_failed）
5. 其余（存在测试类 runner FAILED，含 continue 失败后走完的管道）→ failed
```

- v1 的 `completed_with_errors` 终态**取消**（D-73）：`continue` 失败的 Task 同样落 `failed`，失败清单看具体 runner 的错误码与 `retryable`。
- pre_check 以**不可重试**错误失败（4xx、认证失败）归 `failed`（第 5 条），由错误码定位；不记成服务不可用（D-35 沿用）。
- 排查方向由此明确：平台侧失败找平台，`service_unavailable` 找服务部署方，`failed` 看具体失败 runner。

进入平台侧终态（`failed` / `canceled`）前，所有未终态 runner 必须收口：`pending`/`queued` → `skipped`（`reason: platform_error` / `canceled`，传播到 clean 为止），`preparing`/`running` → 先取消再置 `canceled`；随后 service-clean 执行完才落终态。禁止出现"Task 已终态、runner 还停在 QUEUED"的悬挂状态（§8.5）。

### C-5.9 提交入口（v2：唯一端点）

v1 的"`POST /runs`（Run）+ `POST /tasks`（独立 Task，合成占位 Run）"双形态随 v2 取消（D-73）：**`POST /tasks` 是唯一提交端点**，顶层 `task:`，`deploy` XOR `service`（C-5.1 第 3 条）。`tasks` 表的 `kind` 列与合成占位 Run 机制移除；产物目录、报告、状态机只有一种形态（`<storage-root>/<task_id>/attempts/<n>/…`）。API 响应返回 `task_id`；列表即全量 Task 表（W-2 概览承载）。

---

## C-6 取消、超时、重试

### C-6.1 取消 Task（§7.6）

1. `POST /tasks/{id}/cancel` → CAS `→ cancelling`；写事件与 audit。
2. 事务外：`PENDING`/`QUEUED` 的 runner → `skipped(reason=canceled)`，**传播到 clean 为止**（service-clean 保持 pending）；`PREPARING`/`RUNNING` → `cancel_requested=true`，executor 删 Job 后置 `canceled`。
3. 部署模式取消（含部署排队中）：service-deploy Job 被终止即视为取消生效——Genet 侧的排队/卸载由 **service-clean runner** 兜底处理（幂等；release 未出现 = 没有可泄漏的东西）；平台不再直接调 Genet（D-70）。
4. service-clean 执行完成（幂等，自身失败只记 `cleanup_failed` 不改终态）→ Task 落 `canceled`。取消尽力而为但不阻塞：clean 重试若干次仍失败则落终态并进入 §9.3 重放清单，不悬挂在 `cancelling`。

### C-6.2 取消单个 runner

**v2 无单 runner 取消端点**（runner 不是编排/重试单位，D-69）：取消一律 Task 级（C-6.1）。v1 的"取消内层 Task + abort 传播"语义并入 C-5.5（`abort_task` 失败传播）与 C-6.1。

### C-6.3 超时 watchdog

每 10s 扫描 `task_runners.status IN ('preparing','running') AND started_at + timeout_seconds < now()`：

- 调用 executor 删除 Job（先写 `cancel_requested`，避免与自然结束竞态）；
- runner → `failed`，错误码 `RUNNER_TIMEOUT`（§6.6）；
- 后续按 `on_failure` 传播（C-5.4）；**没有任何自动重试**——重试入口只有整 Task retry（C-5.6）。超时路径同样执行 service-clean（finally）。
- service-deploy 的部署总窗口（默认 72h）以 runner 超时的形式配置（`AG_DEPLOY_TIMEOUT`），超窗归类 `DEPLOY_BLOCKED`（`detail.reason=timeout`，§7.4）。

Job 侧另有 `activeDeadlineSeconds = timeout + 60s` 兜底（E-5），但**权威判定在平台**。

### C-6.4 竞态规则

- 先落库者胜：状态更新用 CAS；取消与自然结束同时到达时，失败方拿到 `ErrConflict` 后重读状态，不覆盖。
- 删除 Job 的失败只记 `CLEANUP_FAILED`，不回滚 runner 状态。

---

## C-7 报告与产物服务

### C-7.1 产物校验（D-74 三选一 + `result.json` schema 2.0）

> **D-74（产物契约三选一）**：test / pre_check 的 runner 产出 `result.json` / `result.html` / `result.csv` **至少其一**，不强求 `result.json`。`RESULT_MISSING` 重定义为"退出码 0 但三选一产物**全部缺失**"；`RESULT_INVALID` **仅在 `result.json` 已产出但不合法时**适用（`result.html` / `result.csv` 平台不解析，只索引）；失败时**推荐**产出带 `error` 的 `result.json`（软要求，不强制）。

时机：runner 执行记录结束（退出码 0 或非 0）之后、落终态之前。

校验规则（附录 A v2，仅对 `result.json`）：

1. 退出码 0 且 `result.json` / `result.html` / `result.csv` **全部缺失（或全部 0 字节）** → `RESULT_MISSING`（D-74 重定义；deploy/clean 无此契约）；
2. `result.json` 存在但 JSON 非法、schema 不合（含 `schema_version != "2.0"`、缺 `task_id`/`attempt_no`/`runner_seq`/`runner{}`/`metrics`）、或超过 `AG_RESULT_MAX_BYTES`（默认 100 MiB，仅防 OOM 的技术上限，非业务限制）→ `RESULT_INVALID`（`detail=too_large` 用于超限）；仅产出 `result.html` / `result.csv` 时平台不校验内容，只进产物索引（kind=`result`，按扩展名区分）；
3. `result.json` 存在时：`metrics[]` 的 `name+statistic+labels` 唯一；`statistic` 枚举（9 值）；`value` 为 number 或 null；
4. `task_id` / `attempt_no` / `runner_seq` 与本次执行记录一致（独立运行时 `local`/`1`/`1` 的值不在平台路径出现）；`runner.name`/`version` 与注册表一致；
5. `metrics[]` 的 `name+statistic+labels` 唯一；`statistic` 枚举（9 值）；`value` 为 number 或 null；
6. 一致性（§5.8）：退出码 0 但 `result.status = failed` 视为契约不一致，runner 判 `failed`，错误码 `RESULT_INVALID`（detail 记录两侧取值）；退出码非 0 时**仍读** `result.json` 的 `error` 归类：`code` 属附录 D → 用该码 + 实例 `retryable`，否则回退 `RUNNER_NONZERO_EXIT`（E-8.1，供 pre_check 区分可重试/不可重试）；
7. 允许未知字段（前向兼容，I-11 K-10）；不允许缺失必填。失败路径：`result.json` 的 `error` 是**软要求**（D-74）——存在则用于归类（第 6 条），不存在则回退 `RUNNER_NONZERO_EXIT`，不因此加罚。

**exports.env 收割校验**在 runner 成功（exit 0）后进行（C-5.4）：词法不合格 → `EXPORT_INVALID`，runner 改判 `failed`。三件事分开：产物三选一管"结果可取"，`result.json` 管"结构化指标可读"，`exports.env` 管"注入链路可信"。

校验器放 `report.ValidateResult`，用 `schemas/result.schema.json` 做结构校验 + 代码补充唯一性/一致性规则。

### C-7.2 报告生成

> 下表路径均相对 `<storage-root>/<task_id>/`（§8.1）。

| 文件 | 时机 | 幂等 |
|---|---|---|
| `attempts/<n>/runners/<seq>/report.json` | runner 终态 | 覆盖写（内容由库派生；状态、输入快照、退出码、导出、产物引用） |
| `report.json` | Task 终态落库后（异步） | 同上 |
| `report.md` | 同上 | 同上 |
| `metadata.json` | 同上 | 裁决快照：版本、镜像引用 + 实际 `image_digest`、Profile、Manifest 原文 |
| `events.jsonl` | 持续追加 | 按 seq 幂等补齐（C-9） |
| `attempts/<n>/exports/<seq>.exports.env` + 注入清单 | runner 成功后收割（C-5.4） | 落档后只读 |

报告内容一律取库中终态，不依赖"文件先于状态存在"；生成失败只重试并告警，不回滚终态。`GET .../report` 在文件尚未生成时返回 404 + `Retry-After`。

Task `report.json` 严格按 §8.4：`attempt_no` + runner 汇总（`seq`/`name`/`role`/`status`/`exports_ref`/`result_ref`/`duration_ms`）+ 失败清单 + `cleanup_failed`，不做指标聚合（不求和、不求平均）。单 Task 报告 > 20 MiB 时写摘要 `report.json`（含 `truncated: true` 与 `report_ref`），完整版写 `report.full.json`。

### C-7.3 `report.md` 模板

`状态（含 attempt_no）/ 耗时 / runner 管道表（seq、名称、角色、状态、错误码）/ 失败清单 / 产物与报告链接`。不渲染指标图表（UI 负责）。

### C-7.4 `events.jsonl`

- 库是权威；writer 订阅 `state_events` 并按 `seq` 追加，遇已存在 seq 跳过；
- controller 重启后先读文件最后一行 seq，与库比较，缺则从库补齐；
- 每行一个事件对象，含 `seq`、`type`、`task_id`、`attempt_no`、`runner_seq`、`from`、`to`、`at`、`actor`、`payload`。

### C-7.5 产物索引与下载

- runner 执行记录结束后扫描 `attempts/<n>/`（含 `input/`、`exports/`、`runners/<seq>/`），落 `artifacts`（size；sha256 惰性/可选）；
- 下载接口按 `artifacts.path` 白名单读取，**不拼接用户输入**；路径校验拒绝 `..`、绝对路径、符号链接穿越；
- 大文件支持 `Range`；日志用尾部读取（`offset`/`limit`/`tail`），不整文件入内存。

### C-7.6 完整性检查

§8.5 的规则做成断言，在 Task 终态前执行：`succeeded`（test/pre_check）必有至少一份三选一产物（D-74；`result.json` 存在时必须合法）；`failed` 必有错误码 + stderr 摘要；`skipped` 必有 reason。不满足则记 `INTERNAL_ERROR` 事件并告警（不改变已发生的 runner 结果）。

---

## C-8 API

### C-8.1 约定

- 前缀 `/api/v1`；JSON；时间 RFC3339；`X-Request-Id` 回显。
- 错误体：`{code, message, detail, retryable, trace_id}`，`code` 只用附录 D（v2）枚举。
- 写操作**不做幂等**：每次 `POST /tasks` 都创建新资源，重复提交 = 多个 Task；无 `Idempotency-Key`、不按内容判重（D-37）。内部操作（建 Job，幂等键 `(task_id, attempt_no, seq)`；deploy/clean runner 的 releaseName 派生）仍用内部幂等键保证重复执行无副作用。
- 分页：**不透明 `cursor`**（`?cursor=<opaque>&limit=50`，上限 200），服务端编码 `(submitted_at DESC, id DESC)`；客户端不解析。
- 请求体上限 1 MiB（Manifest 本身也限制，见 C-5.1）；`runners[].input` 单框原文计入其中。
- **v2 路径更名（D-73）**：`/runs`→`/tasks`、`/testers`→`/runners`、`/runs/{id}/tasks/{tid}`→`/tasks/{id}/runners/{seq}`；旧路径不保留兼容别名（内部工具，一次切换）。
- OpenAPI 3.1 为唯一契约源，`oapi-codegen` 生成 server 接口；前端用 `openapi-typescript` 生成类型。

### C-8.2 端点表

> 端点契约以 [`views/07-api.md`](../views/07-api.md) 为权威；本节是摘要，路径/字段以 07 为准。v2 路径全面 `/tasks` / `/runners` 化（D-73）。

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| `GET` | `/auth/login` | - | 302 到 OIDC（`?return=` 仅站内相对路径） |
| `GET` | `/auth/callback` | - | 换会话、Set-Cookie、302 回跳 |
| `POST` | `/auth/logout` | viewer | 清会话 |
| `GET` | `/me` | viewer | 当前用户、角色、`csrf_token` |
| `POST` | `/tasks` | operator | **提交 Task**（顶层 `task:`，`deploy` XOR `service`）；`?dry_run=true` 只校验 |
| `GET` | `/tasks` | viewer | 列表：status / runner / submitter / 时间 / 游标（D-62：概览页承载全量 Task 表） |
| `GET` | `/tasks/{id}` | viewer | 详情 + runner 管道摘要（按 `attempt_no` 分组） |
| `POST` | `/tasks/{id}/cancel` | operator | 取消（先 clean 后终态，C-6.1） |
| `POST` | `/tasks/{id}/retry` | operator | **整 Task 重试**：新 attempt 全量重跑含 fresh deploy；超限 409 `ATTEMPT_EXHAUSTED`（§7.8） |
| `GET` | `/tasks/{id}/events` | viewer | SSE，支持 `Last-Event-ID` |
| `GET` | `/tasks/{id}/events/history` | viewer | 事件历史（`?since_seq=&limit=`，JSON），用于详情页右栏时间线的首屏 |
| `GET` | `/tasks/{id}/report` | viewer | `report.json` |
| `GET` | `/tasks/{id}/report.md` | viewer | `report.md`（`text/markdown`） |
| `GET` | `/tasks/{id}/manifest` | viewer | Manifest 快照（用于"照上次配置再来一次"与事后核对） |
| `GET` | `/tasks/{id}/deploy-state` | viewer | 最新 attempt 的 deploy-state.json 副本投影（`phase` / 水位 / 已等待时长 / Pod 清单；`?attempt=` 切换） |
| `GET` | `/tasks/{id}/attempts` | viewer | attempt 列表（`attempt_no`、trigger、状态、落档路径） |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/result` | viewer | 该 runner 的三选一产物（D-74：按存在优先级 `result.json` > `result.html` > `result.csv`；Content-Type 随扩展名；`?name=` 可指定） |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/logs` | viewer | `?stream=stdout\|stderr&offset=&limit=&follow=1`（跟随为流式） |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/artifacts` | viewer | 该 runner 产物清单 |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/logs/pods` | viewer | 部署 Pod 日志（仅 deploy runner 节点）：`?pod=`（必选）`&offset=&limit=&tail_lines=`；Pod 已回收时返回 `message` + `gaps[].reason` |
| `GET` | `/tasks/{id}/files` | viewer | `?path=` 产物下载（白名单 + Range；path 相对 `<storage-root>/<task_id>/`） |
| `GET` | `/tasks/{id}/artifacts` | viewer | Task 级产物清单（报告页与下载列表用） |
| `POST` | `/tasks/{id}/clean` | operator | **泄漏重放**（§9.3）：用落档导出重拉一次 service-clean Job；`?attempt=` 指定 attempt（缺省最后一个） |
| `GET` | `/runners` | viewer | Runner 目录（含 `entry` / `input_format` / `parameters` / `exports` / `consumes`，只读） |
| `GET` | `/runners/{name}` | viewer | 最新版本详情 |
| `GET` | `/runners/{name}/versions/{v}` | viewer | 指定版本 |
| `GET` | `/profiles` | viewer | 执行环境列表与挂载能力 |
| `GET` | `/clusters` | viewer | 集群概要（不返回凭据） |
| `GET` | `/secrets` | operator | 可用 Secret 的**名称与说明**（用于提交页下拉，不回显值） |
| `GET` | `/meta` | viewer | 版本 / API 版本 / 特性开关（前端版本对齐用） |
| `POST/PUT/DELETE` | `/admin/profiles` | admin | Profile 维护 |
| `POST` | `/admin/clusters` | admin | 集群登记（`kubeconfig_key` 指向挂载目录中的文件名，不存凭据内容） |
| `GET/POST` | `/admin/secrets` | admin | Secret 引用列表/登记（不回显值） |
| `POST` | `/admin/secrets/{name}/rotate` | admin | 轮换后校验引用可读、记录审计（C-10）；已运行 Job 不受影响 |
| `GET` | `/admin/audit` | admin | 审计查询 |
| `DELETE` | `/admin/tasks/{id}/artifacts` | admin | 人工清理单个 Task 的产物（保留期清理的例外通道，见 web-and-delivery §11） |
| `GET` | `/queue-status` | viewer | 平台槽位 + Volcano 等待原因（看板与详情共用） |
| `GET` | `/healthz` `/readyz` `/metrics` | - | 运维 |

> v1 的 `POST /runs/{id}/retry`（同 Run 追加 Attempt）、`GET /runs/{id}/retry-plan`（复用预览）、`GET /runs/{id}/deploy`（部署快照）随 v2 取消或并入上表：重试只有整 Task 一种（无复用计划）；部署快照由 `/deploy-state` 承载。P1 的跨 Task 续跑（`retry_of`，仅外接模式）另立端点，P0 不做（I-13）。

### C-8.3 Manifest 校验实现

- `schemas/manifest.schema.json`（JSON Schema 2020-12）由附录 B/C 与 §10.1 手工维护，CI 校验它与 OpenAPI 一致；
- 业务校验（引用、冲突、配额、env）在 `orchestrator.Validate`，返回 `[]ValidationIssue{field, code, message}`，HTTP 400 + `code: VALIDATION_ERROR`，`detail.issues` 列表；
- 字段路径用 JSON Pointer 风格（`runners[2].input`），UI 直接定位到对应输入。

### C-8.4 输入校验器（取代 v1 的 argv 词法切分器，D-66 / D-67）

输入：`runners[].input` 原文 + 该 Runner 的 `input_format` 与 `parameters`。输出：落盘的输入文件 + `[]ValidationIssue`。

| `input_format` | 校验深度 | 产物 |
|---|---|---|
| `jsonl`（默认） | **语法级 + 结构级**：逐行 `JSON.parse`（错误带**行号**）、校验"是对象"、按 `parameters` 校验**字段类型**与 **`required: true` 存在性**（错误带**字段名**）；未声明的字段**放行**（归 Runner 决定） | `attempts/<n>/input/<seq>/params.jsonl` |
| `yaml` / `text` | 仅语法级（可解析）；语义完全归 Runner；此时 `parameters` 必须为 `[]`（提交校验拒绝） | `params.yaml` / `params.txt` |

规则（实现为纯函数，无 IO，禁止 `sh -c` 参与任何路径）：

- 逐行处理保留行序（行序即执行序，§4.3）；空行跳过；`#` 开头为注释行跳过；
- 单行超 1 MiB、总行数超 10 万 → `VALIDATION_ERROR`（防滥用）；
- `parameters` 为空时只做"是对象"校验（不得省略 `parameters: []`，附录 C）；
- 范围 / 枚举 / 正则 / 跨字段约束**一律不校验**（§5.4：语义归 Runner；校验左移只做类型 + required）；
- `placeholder`（UI 占位）必须与 `input_format` 同格式，注册期校验。

校验失败直接拒绝提交：`VALIDATION_ERROR`，`detail.issues[]` 每条 `{line, field, reason}`（jsonl）；输入**原样落盘**为只读挂载源（§6.8），事后可审计（§5.7 输入快照）。

### C-8.5 env 渲染：导出合并注入（取代 v1 的公共参数 argv 追加，D-71）

runner Job 的 env 由**同一套注入通道**合成（附录 B v2：env 全 `AG_*`）：

1. **来源（按管道线性顺序合并）**：
   - 外接模式：Manifest `service` 三元组 → 投影为 `AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH`，作为**"序号 0 的导出"**注入全部 runner；
   - 部署模式：service-deploy 成功后收割 exports 提供同一组 key（内置契约，§5.12）；
   - 任意 runner 的 exports（`succeeded` 且词法合格才收割，C-5.4）；
   - Task 级 `env`（明文）与 `secrets`（`secret_env` 引用注入，不落 exports）。
2. **冲突处理**：组合期已拒绝重复 key（C-5.1 第 8 条）；运行期同一 key 后到者不覆盖先到者（管道序即优先级），并记 `detail.export_override_rejected` 事件告警（正常不应发生）。
3. **凭据不进 exports**（§5.12 / §12.4）：收割时若值命中 Task 级 secret 引用的明文，拒绝该行并告警（平台已知明文仅此一处比对，不做日志脱敏——K-5）。
4. **元信息 env**：`AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ` / `AG_PARAMS_FILE` / `AG_OUTPUT_DIR` 每 Job 恒注入（附录 B）。
5. **`endpoint_key` 规范化（仅外接模式租约用）**：`net/url` 解析 `service.url`；无 scheme 补 `http`；host 小写；端口补默认（80/443）；key = `scheme://host:port`；解析失败时用 `raw:<sha256>`（仍保证同一字符串互斥）。

`resolved` 快照里保存每个 runner 的输入原文落档路径与**注入清单**（key、来源 seq、值），保证事后可完整还原"当时注入了什么"（§5.7、§5.12）。`AG_SERVED_*` 等公共参数名固定（禁止 `AG_BASE_URL` 变体，§5.4.1）；Runner 不声明对 Endpoint 的依赖，依赖表达用 `consumes`（组合期校验闭合）。

### C-8.6 契约测试

- CI 对 `api/openapi.yaml` 跑 `spectral` lint；
- 生成客户端编译通过（防止 spec 破坏后端）；
- 每个错误码至少一个 handler 测试，断言 `code` 与 HTTP 状态（附录 D）；
- `schemas/` 用 `ajv`/`gojsonschema` 双向校验样例。

---

## C-9 事件与 SSE

### C-9.1 事件类型

| type | 载荷 |
|---|---|
| `task.submitted` | manifest 摘要、task_id |
| `task.state_changed` | from/to、reason（Task 级；v2 无 deploying/testing/cleanup 阶段转移，D-70） |
| `runner.state_changed` | attempt_no、runner_seq、from/to、reason、waiting_reason |
| `runner.started` / `runner.finished` | attempt_no、runner_seq、phase、exit_code、error（对应 §12.6 的 `runner_started` / `runner_finished`） |
| `exports.harvested` | attempt_no、runner_seq、keys[]、落档路径（§5.12） |
| `deploy.state_changed` | attempt_no、deploy-state `phase`（含 `queued`）、水位计数与原因原文（**无位次**，D-46）、已等待时长、pod 数、message（源自 deploy-state.json 副本，非平台轮询 Genet） |
| `clean.finished` / `task.finished` | 结果与计数（`cleanup_failed` 标志） |
| `report.written` | 报告路径 |
| `resync_required` | 回放越窗，客户端整页重建（C-9.2） |

事件只描述事实，不携带凭据；`payload` 里禁止出现 secret 值。**日志内容不走 SSE**（会把它变成大流量通道），日志走独立的流式读取接口（C-7.5）。

### C-9.2 SSE 实现

- 连接：`GET /tasks/{id}/events`；客户端用 **fetch + ReadableStream**（非原生 EventSource），以便带 `Last-Event-ID` 头、401 时跳 OIDC、后台标签页暂停（W-3）；`Last-Event-ID` = 上次收到的 `seq`；
- 回放：从库读 `seq > last` 的事件，**窗口 24h 且单次上限 1000 条**（`AG_SSE_REPLAY_HOURS`）；超出窗口或条数时先发 `resync_required`（客户端整页重建），不静默截断；
- 实时：`LISTEN argus_task_<id>`（或统一频道 + 过滤），`NOTIFY` 载荷只含 `task_id, seq`，进程再查库拿完整事件（避免 PG 8KB 载荷限制）；
- 心跳：15s 一次 `: ping`；空闲 60s 断开，客户端自动重连；
- 顺序：同一 Task 的 seq 单调，客户端按 seq 去重。

### C-9.3 多副本

每个 api 副本都 `LISTEN` 同一频道；收到通知后按需查库。`NOTIFY` 在事务提交后发送，避免推已回滚事件。不做跨副本会话粘滞。

---

## C-10 认证与授权

- **UI**：OIDC authorization code + PKCE；会话 Cookie（`HttpOnly; Secure; SameSite=Lax`）；写操作要求 `X-CSRF-Token`（双提交）。
- **服务调用**（Jenkins / Agent）：`Authorization: Bearer <JWT>`，由统一身份源签发，平台用 JWKS 校验；无匿名路径。
- **开发模式**：`--auth=dev` 显式开启，默认角色来自配置；生产镜像构建时该模式默认关闭并在 `/healthz` 里打印警告。
- **RBAC**（§12.1）：`viewer` 读；`operator` 提交/取消/重试/泄漏重放（`POST /tasks/{id}/clean`）；`admin` 另加 Profile/Cluster/Secret 维护与审计查询。**Runner 注册不在运行时进行**：只随平台镜像发布，由 CI 校验（K-3）。组到角色的映射放配置（`groups:role`），不在代码里硬编码。
- **审计**：所有写操作在事务内写 `audit_log`（actor、role、action、object、detail），失败也记录尝试。
- **回显**：`env` 明文可回显；`secrets` 只返回名称；Secret 值永不进 API 响应。
- **Secret 轮换**（§12.3）：值由 admin 在 Secret Store 侧更新（平台**无写 Secret 权限**）；随后调 `POST /admin/secrets/{name}/rotate`，平台重读引用做一次可读性校验并记 `secret_rotated` 审计。已运行 Job 的环境变量不可变，轮换只影响**新 Job**（新 runner 启动时取最新引用）；轮换失败不回滚已登记引用，只告警。**平台不读 Secret 值做日志脱敏**（K-5 已拍板日志明文）。

---

## C-11 配置项

| 配置 | 默认 | 说明 |
|---|---|---|
| `AG_LISTEN` | `:8080` | API 监听 |
| `AG_DB_DSN` | - | PostgreSQL DSN |
| `AG_DB_MAX_OPEN_CONNS` | `20` | 连接池上限（P95 的关键参数） |
| `AG_STORAGE_ROOT` | `/storage-root` | NFS 挂载点（对应 `<storage-root>`） |
| `AG_ROLE` | `all` | `api` / `controller` / `collector` / `all` |
| `AG_CONCURRENCY_MAX` | `20` | 全局并发 runner 数上限（§12.7） |
| `AG_TASK_MAX_RUNNERS` | `200` | 单 Task runner 数上限 |
| `AG_ATTEMPT_MAX` | `3` | 单 Task attempt 上限（重试拒绝超限，`ATTEMPT_EXHAUSTED`，§12.7） |
| `AG_RUNNER_TIMEOUT` | `4h` | 默认 runner 超时（Manifest `runners[].timeout_seconds` 可覆盖） |
| `AG_RUNNER_TIMEOUT_MAX` | `24h` | 覆盖上限 |
| `AG_DEPLOY_TIMEOUT` | `72h` | 部署总窗口（= service-deploy runner 的执行时限；排队计入，K-6） |
| `AG_RESULT_MAX_BYTES` | `100MiB` | `result.json` 技术上限（仅防 OOM；K-10 不设业务限制；html/csv 不受此限） |
| `AG_ARTIFACT_MAX_BYTES` | `10GiB` | 单 attempt 产物配额 |
| `AG_LOG_SEGMENT_MAX_BYTES` | `200MiB` | 日志分段阈值 |
| `AG_LOG_TAIL_LINES` | `2000` | 默认日志尾部行数 |
| `AG_RECONCILE_INTERVAL` | `30s` | 对账周期（E-4.2） |
| `AG_EXECUTOR_NAMESPACE` | `argus` | Job 与 PVC 所在 namespace（E-2） |
| `AG_RUNNERS_DIR` | `/app/runners` | 注册表扫描来源（镜像内；含内置 service-deploy / service-clean / endpoint-probe；启动时扫描一次，K-3） |
| `AG_SSE_HEARTBEAT` | `15s` | SSE 心跳 |
| `AG_SSE_REPLAY_HOURS` | `24` | SSE 回放窗口（C-9.2） |
| `AG_RETENTION_DAYS` | `30` | 产物保留期（v2 `cleanup.policy` 取消，保留期只由本配置决定，D-70） |
| `AG_OIDC_ISSUER` / `AG_OIDC_CLIENT_ID` / `AG_OIDC_CLIENT_SECRET` | - | 身份源；另配 `AG_OIDC_SCOPES`（默认 `openid profile email groups`）与 `AG_OIDC_ROLE_CLAIM`（默认 `groups`），见 §12.1 与 web-and-delivery §10 |
| `AG_SESSION_COOKIE` / `_TTL` / `_KEY` | `ag_session` / `8h` / - | 会话 |
| `AG_AUTH_MODE` | `oidc` | `dev` 仅在 dev 允许（web-and-delivery §14 红线） |
| ~~`AG_GENET_BASE_URL` / `_TOKEN_FILE`~~ | - | **随 v2 移出平台配置**（D-70）：Genet OpenAPI 地址与 Open API Key 下沉为 service-deploy / service-clean runner 的运行配置，经 Task 级 `secret_env` 注入；genet-local 地址同样经 env 传给 runner（本地联调） |
| `AG_CLUSTERS_DIR` / `_DEFAULT_CLUSTER` | `/etc/argus/clusters` / 空 | 多集群 kubeconfig 目录（K-4） |
| `AG_K8S_IN_CLUSTER` | `true` | 集群内运行；false 时用 kubeconfig |
| `AG_METRICS_ENABLED` | `true` | `/metrics` |

配置走环境变量 + 可选 YAML；启动时校验必填项与取值范围，错误直接退出（fail fast）。Env 到内部字段的映射在 `internal/config` 一处完成（如 `AG_ARTIFACT_MAX_BYTES → artifact_quota_bytes`、`AG_LOG_SEGMENT_MAX_BYTES → log_segment_bytes`），E 文档用内部字段名、C 文档用 env 名，二者指向同一值。

---

## C-12 容量与性能

| 维度 | P0 目标 | 实现要点 |
|---|---|---|
| 状态查询 P95 | < 300 ms（§12.7） | 游标分页、覆盖索引、避免详情页 N+1（Task 列表用一条 JOIN） |
| 单 Task 事件量 | 200 runner × ~6 事件 ≈ 1200 行 | 单 Task 事件流小，SSE 回放一次读得完 |
| 调度吞吐 | 20 并发 runner，单 runner 分钟级 | 调度事务毫秒级；瓶颈在 K8s 而非库 |
| 报告体积 | ≤ 20 MiB / Task | 超限外置摘要（C-7.2） |
| 日志读取 | 单次 ≤ 64 KiB 行数分页 | 尾读 + `offset`，`fs.ReadAt` 不整文件加载 |
| 事件写入 | 每状态转移 1 行 | 状态转移频率低（Task 级，不是请求级） |

数据库连接池：`max_conns = 4 × vCPU`（上限 40）；长事务禁止（外部 IO 不进事务）。

---

## C-13 与 execution 的接口边界

```go
// 完整签名以 E-1 为唯一蓝本；这里只保留形状，避免两处漂移。
type Executor interface {
	Submit(ctx context.Context, spec JobSpec) (JobRef, error) // 幂等
	Cancel(ctx context.Context, ref JobRef, mode CancelMode) error
	Status(ctx context.Context, ref JobRef) (JobStatus, error)
	Reconcile(ctx context.Context, refs []JobRef) ([]JobStatus, error)
}

// 结论不直接写库：执行层通过 control-plane 实现的
// EventSink.Emit / ResultSink.FinishRunnerRun 提交事实与结论，
// 状态转移（CAS、先落库者胜）仍在 control-plane 的事务内完成。
type RunnerRunKey struct{ TaskID string; AttemptNo, Seq int }
```

- executor **不写** `tasks.status` / `task_runners.status` 的编排字段；它通过 `ResultSink` 提交结论，由 control-plane 在事务内做 CAS（E-5.3）；
- 日志采集（collector）与 executor 同属执行层，但独立进程角色；
- Registry 只读 `runners/` 并算 diff + 错误清单，落库走 `RegistryStore`（control-plane 实现）；
- **Genet 直连适配层取消（D-70）**：`internal/genet` 瘦化为 `internal/deploystate`——只解析 service-deploy 产出的 deploy-state.json 并归档，返回结构化投影，不写库、不发 HTTP；
- 所有跨层调用都在事务外，结果再进事务落地。
