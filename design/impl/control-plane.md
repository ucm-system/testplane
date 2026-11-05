# 控制面实现方案（状态机 / 调度 / API / 报告）

> 分册，隶属 [`../IMPLEMENTATION.md`](../IMPLEMENTATION.md)。不新增设计裁决；与 [`../ARCHITECTURE.md`](../ARCHITECTURE.md) 冲突时以 ARCHITECTURE 为准。
> 约定：`§x.y` = ARCHITECTURE；`C-x` = 本文；`E-x` / `W-x` / `D-x` 见同目录另两册。

---

## C-1 分层与依赖规则

```text
api/           HTTP、SSE、鉴权、错误映射（薄）
orchestrator/  提交校验、precheck 注入、Run 推进、abort、cleanup、终态判定
scheduler/     候选选取、端点槽位、全局并发、超时 watchdog
store/         唯一写库入口：事务、CAS、事件、锁
domain/        纯类型与规则（状态枚举、转移表、校验），零 IO
executor/ genet/ registry/ report/ events/ logcollect/   adapter，不写 tasks.status
```

规则：

1. 只有 `store` 能执行 SQL；其他包通过 store 的方法表达意图。
2. 只有 `store.Transition*` 能改 `runs.status` / `tasks.status` / `attempts.status`，且必须同事务写事件（§7.1、§12.5）。
3. adapter（executor / genet / registry / report）只回写"物理事实"（Job 状态、Endpoint、文件路径），状态推进由 orchestrator 在事务里做。
4. `domain` 可被任何层引用，反向不成立。

---

## C-2 数据模型

### C-2.1 设计原则

- **状态 token 统一小写**（`pending`…`canceled`），与 §8.4 示例一致；文档中的大写是概念名，前端展示大写徽标（见 I-12 G-12）。
- 时间一律 `timestamptz`（UTC 存储，展示按 `Asia/Shanghai`）。
- 不软删：Run/Task 的删除只发生在保留期清理，不做逻辑删除标记。
- 事件表 append-only，只由状态机写；审计另表。
- 所有"配额/上限"在提交时校验，运行期由 watchdog 兜底。

### C-2.2 表清单

| 表 | 作用 | 权威性 |
|---|---|---|
| `runs` | Run 快照、状态、Endpoint、部署信息 | 权威 |
| `tasks` | Task 定义快照、状态、槽位原因、重试信息 | 权威 |
| `attempts` | 每次物理执行 | 权威（与 K8s Job 对账） |
| `state_events` | 状态与生命周期事件流 | 权威（jsonl 由此派生） |
| `endpoint_leases` | 端点互斥槽位 | 权威 |
| `testers` | Tester 注册信息（含参数清单） | 权威（来源是目录扫描） |
| `clusters` / `execution_profiles` | 执行环境登记 | 权威 |
| `secret_refs` | Secret 引用（无明文） | 权威 |
| `artifacts` | 产物索引（下载用） | 派生，可重建 |
| `audit_log` | 操作审计 | 权威 |

### C-2.3 关键 DDL

> 完整表结构、约束、索引、JSON 形状与迁移策略以 [`views/03-data-view.md`](../views/03-data-view.md) 为唯一权威；本节是摘要，字段级冲突以数据视图为准。

```sql
-- 状态枚举收敛在一处，迁移时同步修改 CHECK
-- runs.status: submitted|queued|deploying|testing|cleanup|cancelling|
--              succeeded|completed_with_errors|service_unavailable|failed|canceled
CREATE TABLE runs (
  id               text PRIMARY KEY,                     -- r-YYYYMMDD-XXXX
  name             text NOT NULL,
  kind             text NOT NULL DEFAULT 'run',           -- run | task（独立 Task 合成的占位 Run，§8.1）
  status           text NOT NULL,
  manifest         jsonb NOT NULL,                       -- 原始提交（secrets 仅引用）
  resolved         jsonb NOT NULL,                       -- 解析后的快照：node 化 precheck、argv、timeout
  endpoint         jsonb,                                -- {url, model_name, model_path, key, source}
  deploy           jsonb,                                -- {provider, parameters, deployment_id, state, queue_position, queued_at}
  cleanup_policy   text NOT NULL DEFAULT 'always',
  submitted_by     text NOT NULL,
  submitted_at     timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  error            jsonb,
  retry_of         text REFERENCES runs(id),             -- P1 跨 Run 续跑
  last_event_seq   bigint NOT NULL DEFAULT 0,            -- 事件序号分配器（原子自增）
  revision         bigint NOT NULL DEFAULT 1,
  CONSTRAINT runs_kind_ck CHECK (kind IN ('run','task')),
  CONSTRAINT runs_cleanup_ck CHECK (cleanup_policy IN ('always','on_failure','never'))
);

-- tasks.status: pending|queued|preparing|running|succeeded|failed|canceled|skipped
CREATE TABLE tasks (
  id                 text PRIMARY KEY,                   -- t-XXXXXX
  run_id             text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq                int  NOT NULL,                      -- Manifest 顺序（precheck = 0）
  name               text NOT NULL,
  tester_name        text NOT NULL,
  tester_version     text NOT NULL,
  image_ref          text NOT NULL,
  profile            text NOT NULL,
  args_raw           text NOT NULL,                      -- 提交者原文
  args_argv          jsonb NOT NULL,                     -- 词法切分结果
  injected_argv      jsonb NOT NULL DEFAULT '[]',        -- 公共参数
  env                jsonb NOT NULL DEFAULT '{}',
  secret_env         jsonb NOT NULL DEFAULT '[]',
  on_failure         text NOT NULL,
  timeout_seconds    int  NOT NULL,
  retry_policy       jsonb NOT NULL DEFAULT '{}',
  status             text NOT NULL,
  status_reason      text,
  waiting_reason     text,                               -- precheck_gate|slot_waiting|global_limit|
                                                         -- queue_full|insufficient_resources|gang_pending
  endpoint_key       text,                               -- deploy 模式提交时为 NULL，Endpoint ready 后回填（C-5.3）
  endpoint_exclusive boolean NOT NULL,
  current_attempt    int NOT NULL DEFAULT 0,
  available_at       timestamptz,                        -- 重试 backoff 到期时间（precheck 门控用 status=pending 表达）
  cancel_requested   boolean NOT NULL DEFAULT false,
  result_ref         text,
  reused_from_task_id text,                              -- P1 跨 Run 续跑的复用来源（§7.7）
  error              jsonb,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tasks_name_uq UNIQUE (run_id, name),
  CONSTRAINT tasks_seq_uq  UNIQUE (run_id, seq),
  CONSTRAINT tasks_behavior_ck CHECK (on_failure IN ('continue','abort_run'))
);
CREATE INDEX tasks_ready_idx ON tasks (status, available_at) WHERE status = 'queued';
CREATE INDEX tasks_run_idx   ON tasks (run_id, seq);

-- attempts.status: preparing|running|succeeded|failed|canceled
CREATE TABLE attempts (
  id             text PRIMARY KEY,                       -- <task>-a<n>
  task_id        text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  idx            int  NOT NULL,
  status         text NOT NULL,
  job_id         text NOT NULL UNIQUE,                   -- 附录 B 的 job_id（j-XXXXXX），幂等键；K8s 对象名见 E-2.3
  job_namespace  text,
  phase          text,                                   -- 附录 B 的 phase 原值
  exit_code      int,
  started_at     timestamptz,
  finished_at    timestamptz,
  log_ref        text,
  artifact_ref   text,
  result_ref     text,
  error          jsonb,
  executor_ref   jsonb,
  UNIQUE (task_id, idx)
);
-- 不变量：一个 Task 至多一个进行中的 Attempt
CREATE UNIQUE INDEX attempts_active_uq ON attempts (task_id)
  WHERE status IN ('preparing','running');

CREATE TABLE state_events (
  id         bigserial PRIMARY KEY,
  run_id     text NOT NULL,
  task_id    text,
  attempt_id text,
  seq        bigint NOT NULL,                            -- 每 Run 单调，SSE / jsonl 共用
  type       text NOT NULL,
  from_status text,
  to_status  text,
  reason     text,
  payload    jsonb NOT NULL DEFAULT '{}',
  actor      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, seq)
);

CREATE TABLE endpoint_leases (
  task_id      text PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  endpoint_key text NOT NULL,
  exclusive    boolean NOT NULL,
  acquired_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX endpoint_lease_excl_uq ON endpoint_leases (endpoint_key) WHERE exclusive;
CREATE INDEX endpoint_lease_key_idx ON endpoint_leases (endpoint_key);

CREATE TABLE testers (
  name          text NOT NULL,
  version       text NOT NULL,
  display_name  text NOT NULL,
  description   text NOT NULL,
  role          text NOT NULL CHECK (role IN ('test','pre_check')),
  on_failure    text NOT NULL CHECK (on_failure IN ('continue','abort_run')),
  entry         text NOT NULL,
  image         text NOT NULL,
  resources     jsonb NOT NULL,
  concurrency   jsonb NOT NULL,
  mounts        jsonb NOT NULL DEFAULT '[]',
  k8s           jsonb NOT NULL DEFAULT '{}',
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
  run_id     text NOT NULL,
  task_id    text,
  attempt_id text,
  path       text NOT NULL,                              -- 相对 <storage-root>
  size_bytes bigint NOT NULL,
  sha256     text,
  kind       text NOT NULL,                              -- result|raw|log|report|other
  indexed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, path)
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
| 列表页按状态 + 时间倒序 | `runs (status, submitted_at DESC)`、游标 `(submitted_at, id)` |
| 详情页按 Run 取 Task | `tasks (run_id, seq)` |
| 调度候选 | `tasks_ready_idx`（部分索引，只覆盖 queued） |
| SSE 回放 | `state_events (run_id, seq)` 唯一索引即覆盖 |
| 产物浏览 | `artifacts (run_id, task_id)` |
| 审计检索 | `audit_log (at DESC)`、`(object_id)` |

列表接口一律游标分页：**不透明 `cursor`**（服务端编码 `(submitted_at DESC, id DESC)`，客户端不解析），不做 `OFFSET`——§12.7 的 P95 < 300ms 在 Run 数上万后靠它。日志是唯一例外（字节 `offset`）。

### C-2.5 事务边界

| 操作 | 一个事务内包含 |
|---|---|
| 提交 Run | 插入 run → 插入全部 task → 插入 state_event(submitted) → 插入 audit |
| 状态推进 | CAS 更新状态 → 分配事件 seq → 插入事件 → （必要时）更新 waiting_reason |
| 启动 Attempt | 取槽位 → 检查全局并发 → 插入 attempt → task→preparing → 事件 |
| Attempt 结束 | CAS attempt → 校验产物摘要 → task 终态判定 → 事件 → （重试则 task→queued） |
| 取消 | 更新 intent/状态 → 事件；Job 删除在事务外异步执行 |
| abort_run | 批量 SKIPPED + cancel_requested → 事件；实际删除 Job 异步 |

事务内**禁止**外部 IO（K8s、Genet、NFS）。所有外部调用先做、拿结果再进事务；失败就把结果变成状态（`failed` 等），不要悬挂事务。

### C-2.6 迁移

- `golang-migrate`，只加不删；删除列要经过两个版本（先停写、后删）。
- 启动时 controller 校验 `schema_migrations` 版本，不匹配则拒绝启动（避免新旧代码写坏数据）。
- 状态枚举增加时：先扩 CHECK，再改代码，再收缩旧值（如果有）。

---

## C-3 状态机实现

### C-3.1 Task 转移表（唯一合法转移）

| from | to | 触发 | 附加动作 |
|---|---|---|---|
| `pending` | `queued` | Pre-Check 通过（或 `precheck: off`）、重试 backoff 到期、手动重试 | 计算 `available_at`、`waiting_reason` |
| `pending` | `skipped` | `upstream_abort` / `canceled` / `deploy_failed` | 写 `status_reason` |
| `queued` | `preparing` | 拿到端点槽位 + 全局 permit | 创建 Attempt（`idx = current_attempt+1`）、插入租约 |
| `queued` | `skipped` | `upstream_abort` / `canceled` | 释放（本就没有租约） |
| `preparing` | `running` | informer 观察到容器已启动 | 写 `started_at`、Attempt→running |
| `preparing` | `failed` | Job 创建失败 / 镜像拉取失败 / 挂载缺失 | Attempt→failed，记录错误码 |
| `preparing` | `canceled` | 取消 | 删除 Job，释放租约 |
| `running` | `succeeded` | 退出码 0 且 `result.json` 合法 | Attempt→succeeded，生成 Task report |
| `running` | `failed` | 非零退出 / 超时 / 结果缺失或非法 | Attempt→failed |
| `running` | `canceled` | 取消 | Attempt→canceled |
| `failed` | `queued` | `retry_on` 命中且未超 `max_attempts` | `available_at = now + backoff` |

终态：`succeeded` / `failed`（无剩余重试）/ `canceled` / `skipped`。**不可逆**。

> 同一 Run 内，Pre-Check 通过后其余 Task **一次性批量进入 `queued`**（C-4.5），实际并发由端点槽位与全局 permit 决定；不存在"按 seq 串行推进"的规则（§6.7 允许非独占 Task 并行）。
>
> `pending → canceled` 与 `queued → canceled` 不存在：按 §7.6，还没跑的 Task 取消记 `skipped(reason=canceled)`（见 I-12 G-1）。

### C-3.2 Run 转移表

| from | to | 触发 |
|---|---|---|
| `submitted` | `queued` | 落库完成，进入编排 |
| `queued` | `deploying` | Manifest 有 `deploy` |
| `queued` | `testing` | 无 `deploy` |
| `deploying` | `testing` | Genet 返回 Endpoint 且 `ready` |
| `deploying` | `service_unavailable` | Genet 受理但终态 `blocked` / 未就绪 |
| `testing` | `cleanup` | 全部 Task 终态（或 abort 收尾完成） |
| `cleanup` | `succeeded` / `completed_with_errors` / `service_unavailable` / `failed` | 终态判定（C-5.8） |
| 任意非终态 | `cancelling` → `canceled` | 用户取消 |
| 任意非终态 | `failed` | 平台侧错误（Genet 调用失败、Executor 不可用、内部异常） |

### C-3.3 转移函数（唯一写入口）

```go
// 失败返回 ErrConflict，由调用方决定是幂等忽略还是上报。
func (s *Store) TransitionTask(ctx context.Context, id string, from []TaskStatus,
    to TaskStatus, reason string, ev Event) (*Task, error) {

    tx, err := s.pool.Begin(ctx)               // 默认 ReadCommitted 足够：CAS + 行锁
    if err != nil { return nil, err }
    defer tx.Rollback(ctx)

    var t Task
    err = tx.QueryRow(ctx, `
        UPDATE tasks
           SET status = $2, status_reason = NULLIF($3, ''), updated_at = now()
         WHERE id = $1 AND status = ANY($4)
        RETURNING run_id, seq, status, status_reason, attempt, ...
    `, id, to, reason, from).Scan(...)
    if errors.Is(err, pgx.ErrNoRows) { return nil, ErrConflict }

    seq := nextSeq(ctx, tx, t.RunID)           // UPDATE runs SET last_event_seq = last_event_seq+1 RETURNING ...
    insertEvent(ctx, tx, t.RunID, seq, ev)     // type=task_state_changed, from/to, actor
    if err := tx.Commit(ctx); err != nil { return nil, err }

    s.notify(t.RunID, seq)                     // LISTEN/NOTIFY payload 只有 run_id+seq
    return &t, nil
}
```

要点：

- **CAS 即并发控制**：所有状态更新都带 `status = ANY($from)`，竞态下后到者拿到 `ErrConflict`，按"先落库者胜"处理（§6.6）。
- **事件 seq 原子分配**：靠 `runs.last_event_seq` 的行级自增，不用 `max(seq)+1`（避免读-改-写竞态）。
- 事务提交后才 `NOTIFY`，避免推出去的事件被回滚。

### C-3.4 不变量

| 不变量 | 保障方式 |
|---|---|
| 一个 Task 至多一个进行中的 Attempt | 部分唯一索引 `attempts_active_uq` |
| 终态不可逆 | CAS 的 `from` 集合不含终态；夜间一致性任务报警 |
| `attempts.idx` 从 1 连续递增 | 启动 Attempt 时 `current_attempt+1`，唯一约束兜底 |
| 每个 Run 的事件 seq 连续且唯一 | `runs.last_event_seq` + `UNIQUE(run_id, seq)` |
| Run 的 Task 集合与 Manifest 一致 | 提交事务内一次性插入；`resolved` 快照可复核 |
| 有 Endpoint 才能进 `testing` | 转移函数校验 `run.endpoint != null` |
| `queued` 的 Task 必须有 `endpoint_key` | C-4.5 调度守卫；deploy 模式在 ready 时回填 |

### C-3.5 重启恢复与对账

controller 抢到 Lease 后（或每隔 30s）：

1. 扫描 `attempts.status IN ('preparing','running')` 的 Attempt，按 `job_id` 查 K8s：
   - Job 不存在且 `started_at < now-90s`：连续 3 次（约 1 分钟）确认后判 `failed / EXECUTOR_UNAVAILABLE`，事件标注 `reconcile_exhausted`（不直接在一次查询后判失败）。
   - Job 存在但 phase 与库不一致（Pod 已成功/失败）：以外层事实推进状态，补写 `finished_at` 与退出码。
2. 清理无 Attempt 的 `preparing`（说明事务已回滚）：退回 `queued`。
3. 校验 `runs.status IN ('deploying','testing','cleanup')` 是否仍有推进条件；部署阶段重启则重新轮询 Genet（内部幂等键保证不重复部署）。
4. 恢复未完成的 `cancelling`：再次投递删除 Job 的动作。

对账只读 K8s 与 Genet，**不重建内存状态**——所有状态都在库里。

---

## C-4 调度

### C-4.1 触发

调度循环两种触发方式：`NOTIFY`（提交、终态、取消、部署就绪）即时唤醒 + 1s 定时兜底。每轮：

1. 取候选（C-4.2）→ 2. 逐个尝试取端点槽位与全局 permit（C-4.3/C-4.4）→ 3. 成功则 `queued → preparing` 并交给 executor 建 Job；失败写 `waiting_reason` 后结束本轮。

### C-4.2 候选查询

```sql
-- 候选不加行锁：并发由后续 CAS 与 Run 行锁兜住
SELECT t.id, t.run_id, t.seq, t.endpoint_key, t.endpoint_exclusive
  FROM tasks t
  JOIN runs r ON r.id = t.run_id
 WHERE t.status = 'queued'
   AND r.status = 'testing'
   AND (t.available_at IS NULL OR t.available_at <= now())
 ORDER BY r.submitted_at, t.seq
 LIMIT 50;

-- 逐候选：先锁 Run（统一顺序 Run → Task，避免与取消/abort 死锁）
SELECT id FROM runs WHERE id = $run_id FOR UPDATE;
```

> 早期实现用 `FOR UPDATE OF t SKIP LOCKED` 锁候选行，会与"取消/abort 先锁 Run 再批量改 Task"形成反向顺序、可能死锁。现统一为 **Run 行锁 → advisory（端点 → 全局）→ CAS Task**；`SKIP LOCKED` 只用于对账等非状态转移的读。

`CAS`（`UPDATE tasks SET status='preparing' WHERE id=$1 AND status='queued'`）保证多个调度实例不重复领取；拿不到就跳过该候选，下轮重来。Run 行锁保证同 Run 的推进与取消/abort 串行，不会出现反向加锁顺序。

### C-4.3 端点槽位（§6.7 唯一实现）

以 **`endpoint_key` 的 advisory 事务锁**串行化同一端点的判定，再操作 `endpoint_leases`：

```sql
SELECT pg_advisory_xact_lock(hashtext($endpoint_key));   -- 冲突只影响并发度，不影响正确性

-- 已有独占持有者？
SELECT 1 FROM endpoint_leases WHERE endpoint_key=$1 AND exclusive LIMIT 1;
-- 我想独占时，是否有任何持有者？
SELECT 1 FROM endpoint_leases WHERE endpoint_key=$1 LIMIT 1;

INSERT INTO endpoint_leases (task_id, endpoint_key, exclusive) VALUES ($task, $key, $excl);
```

判定规则：独占型在"任何持有者存在"时拒绝；共享型在"存在独占持有者"时拒绝。Attempt 终态时 `DELETE ... WHERE task_id=$task`（与状态推进同事务，避免泄漏）。

`endpoint_key` 的计算（见 C-8.5）：有 `deploy` 时取 Genet 返回 `url` 的规范化值；无 `deploy` 时取 `service.url`；两者都是 `scheme://host:port`，不解析路径、不跟随重定向。

### C-4.4 全局并发上限

```sql
SELECT pg_advisory_xact_lock(hashtext('testplane:global-concurrency'));
SELECT count(*) FROM attempts WHERE status IN ('preparing','running');  -- < TESTPLANE_CONCURRENCY_MAX
```

代价：每个 Attempt 启动都串行一次（毫秒级），20 并发的量级足够。若未来需要更高吞吐，改为 `slots` 计数表 + 条件更新，接口不变。

### C-4.5 Pre-Check 门控与 Run 内并发

> 按 §6.7 字面实现：**平台提供并行能力，是否并行由 Tester 维护者通过 `endpoint_exclusive` 决定**（K-12）。平台不提供 Run 级串行/并行开关，也不额外加"同 Run 串行"规则；Pre-Check 是 Run 内唯一的序化点。

| 场景 | 行为 |
|---|---|
| 有 Pre-Check（auto / 显式） | Run 进入 `testing` 时只把 `seq=0` 置 `queued`；其余 Task 保持 `pending`，`waiting_reason=precheck_gate` |
| Pre-Check `succeeded` | 同事务批量 `pending → queued`（后续 Task 并行入池，由端点槽位/全局 permit 决定谁先跑） |
| Pre-Check 失败 | 按 §7.3 `abort_run` 处理：其余 `pending/queued → skipped(upstream_abort)`、`preparing/running → canceled` |
| `precheck: off` | Run 一进 `testing` 就批量入队 |
| 有独占型 Task（性能类 / Pre-Check） | 独占该 Endpoint 期间，同 Endpoint 上其他 Task 一律排队；独占释放后共享型可并行 |

调度守卫：Task 进入 `queued` 前必须 `endpoint_key IS NOT NULL`（deploy 模式在 Endpoint ready 时回填，C-5.3）；违反即拒绝调度并告警，**不得**用占位键绕过。

### C-4.6 公平性

FIFO 按 `(run.submitted_at, seq)`。不做优先级、不做抢占——平台没有调度决策权（§6.9）。同一 Run 的 Task 在 Pre-Check 通过后同时入池，彼此之间没有先后；跨 Run 才比较提交时间。

### C-4.7 waiting_reason

`QUEUED` 必须有原因（§6.9）。调度每轮对未拿到槽位的 Task 更新：

| 值 | 判定 |
|---|---|
| `precheck_gate` | `PENDING`：等待 Pre-Check 通过（C-4.5） |
| `slot_waiting` | 端点被其他 Task 独占/持有 |
| `global_limit` | 端点空闲但全局并发已满 |
| `queue_full` / `insufficient_resources` / `gang_pending` | K8s/Volcano 侧状态，来自 E-11 的只读快照 |

前三条是平台自身的等待，后三条是 K8s/Volcano 的等待——分开呈现，用户才知道该找谁。§6.9 只列了后三条 + `slot_waiting`；`precheck_gate` 与 `global_limit` 是实现补充的展示原因，拿到 `unknown` 时落 `detail` 而不是新增枚举值（E-11）。

### C-4.8 `GET /api/v1/queue-status` 的数据来源（两套排队，分开呈现）

同一个端点里有两类**互不换算**的排队，响应结构也按它们分成 `slots` / `waiting` / `volcano` 三层（`api/openapi.yaml §QueueStatus`）：

| 层 | 数据源 | 语义 | 时间尺度 | 可用性 |
|---|---|---|---|---|
| `slots` + `waiting`（`precheck_gate` / `slot_waiting` / `global_limit`） | **本进程 + DB**（`endpoint_leases`、`attempts` 计数、`tasks.waiting_reason`） | 平台 permit 与端点独占 | 秒~分钟 | 永远可读 |
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
| `queues[].name` | `metadata.name` | 统一写成 `"<cluster>/<queue>"`（零契约变更）：cluster 取 `TESTPLANE_CLUSTER_NAME`（默认 `cluster-01`），**P0 单集群也带前缀**，切多集群时 UI/契约不用改；cluster 为空则保持原名 |
| `queues[].state` | `.status.state` | `Open` / `Closed`；未知值原样透传，不硬映射成 `Closed` |
| `queues[].capability` | `.spec.capability` | `map[string]string`，**原文透传不做单位换算**；⚠ 队列未设 CPU/内存配额时 Volcano 会兜底成极大值，不可直接当进度条分母 |
| `queues[].allocated` | `.status.allocated` | 控制器回写、**有滞后**；无 `/metrics` 接入时只能用它，`detail` 注明来源 |
| `queues[].pending_jobs` | 该 queue 下 `podgroups[].status.phase == "Pending"` 的计数（PodGroup 的 namespace 范围由 `TESTPLANE_VOLCANO_NAMESPACES` 决定，默认 profile 的 namespace；`spec.queue` 为空的 PodGroup 归 `default` 队列） | **不要用** `queue.status.pending`（Volcano 自己的计数，语义可能含 Inqueue） |
| `queues[].longest_wait_seconds` | `now - min(podgroup.metadata.creationTimestamp)`，限定同 queue 且 `phase ∈ {Pending, Inqueue}` | 无排队 → `0`，不是负数、也不回退成队列创建时间 |
| `queues[].reason` | 同 queue 下 `phase ∈ {Pending,Inqueue}` 里**等得最久**那条的 `status.conditions[-1].message`（空则 `.reason`），**原文透传** | 归一成 `waiting_reason` 六值是 `waiting.by_reason` 的职责（需 `Task→PodGroup` 关联，见 C-4.8.4 #4），MVP 不做。契约已按自由文本落定：`server/api/openapi.yaml` 的 `VolcanoQueue.reason: type [string,'null']`，`design/views/07-api.md §5.3` 同步说明 |

**C-4.8.2 采样方式**

| 项 | 决定 | 理由 |
|---|---|---|
| 轮询 + 15s TTL 缓存，**不用 informer** | 与 `execution.md:412` 的既有约束一致 | 规模小（队列 3 个、PodGroup 数十~数百）；informer 要 watch 语义与常驻 reflector，与"15s 新鲜度"不匹配，多集群还更重 |
| **单飞**（`sync.Mutex` + `inflight`） | 并发请求最多一个在打 API | `/queue-status` 是 30s 轮询的公共端点，看板一开就是多客户端同时打 |
| 单查超时 5s；`/queue-status` 整体 3s 内必须返回 | 火山块失败即降级，不等 | E-11 |
| 失败退避 60s（连续失败后） | 退避窗口内不再打 API，仍回降级快照 | 集群不可达时不要每 15s 打一次（06 §2.1（3）） |
| PodGroup 的 namespace 范围 | `TESTPLANE_VOLCANO_NAMESPACES`（逗号分隔；默认 profile 的 namespace；`*` = 全部，一次集群级 List） | 只统计测试命名空间，避免把别的租户的 PodGroup 算进 `pending_jobs` |
| 先 `queues` 后 `podgroups` | 小的先取；后者失败即整体 `available:false` + `queues:[]`（与 openapi 一致） | 大 List 更容易超时 |
| RBAC | **只 `get,list`**；Queue 是 cluster-scoped → 必须 `ClusterRole`（`testplane-volcano-view`，见 06 §2.1） | 轮询不需要 `watch`；不能给 `queues/status` 写权限（平台只读，不改配额、不插队） |
| 实现落点 | `server/internal/k8s/volcano.go`（`k8s.Volcano` 实现 `api.VolcanoProvider`），`cmd/testplaned/main.go` 注入 `api.Deps.Volcano`；单测 `server/internal/k8s/volcano_test.go` 用 `client-go/dynamic/fake` 覆盖字段映射、四态、TTL、单飞、退避 | 生产代码不引 `volcano.sh/apis`，测试也不引 |

**C-4.8.3 降级四态（红线：不可达 ≠ 0 排队）**

| 情形 | `volcano` 块 | 其他块 |
|---|---|---|
| CRD 未安装（List 返回 `IsNotFound` / `NoMatch`；结果缓存 5 分钟*） | `available:false, detail:"crd_missing", queues:[]` | 照常 |
| List 返回 `apierrors.IsForbidden` | `available:false, detail:"rbac_forbidden", queues:[]`（不吐原始 RBAC 报错，避免泄露 SA/角色名） | 照常 |
| 超时 / 连不上 | `available:false, detail:"scheduler_unavailable", queues:[]`；上次成功过则 `stale:true`（**不回放旧队列数字**） | 照常 |
| `TESTPLANE_VOLCANO_ENABLED=false`（平台未启用队列调度） | `available:false, detail:"disabled", queues:[]` | 照常 |

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
| `TESTPLANE_VOLCANO_ENABLED` | `true` | 关闭 → `detail:"disabled"`（**不是** `crd_missing`，也不是"0 排队"） |
| `TESTPLANE_VOLCANO_NAMESPACES` | profile 的 namespace（未配置时 = `TESTPLANE_NAMESPACE`，默认 `testplane`） | 逗号分隔；`*` / `all` = 全部 namespace（一次集群级 List）。**它决定 `pending_jobs` / `longest_wait_seconds` / `reason` 统计哪些 namespace 的 PodGroup**：不在列表里的 namespace（例如 `testplane-mock` 里的 PodGroup）**不计入**任何队列；多个 ns 会逐个 List 后合并，重复项去重 |
| `TESTPLANE_VOLCANO_TTL` | `15s` | 快照 TTL；非法值启动即失败（fail fast） |

本地演示/联调用夹具（12 个 Volcano CRD + 2 Queue + 5 PodGroup，**只装 CRD 不装 scheduler**，status 为手工假水位）见 [`deploy/k8s/volcano-fixtures/README.md`](../../deploy/k8s/volcano-fixtures/README.md)：装上后 `/queue-status` 的 `volcano` 块为 `available:true` 的真实 CRD 数据；删掉即回到 `crd_missing`。

---

## C-5 编排流程

### C-5.1 提交校验顺序

按顺序执行，遇到第一个错误即返回（字段级定位）；同一层可聚合多个 issue。

| 序 | 校验 | 错误码 |
|---|---|---|
| 1 | 请求体大小（Manifest ≤ 1 MiB）、YAML 可解析 | `VALIDATION_ERROR` |
| 2 | `manifest.schema.json` 结构校验（必填、类型、枚举、互斥） | `VALIDATION_ERROR` |
| 3 | `deploy` XOR `service`；两者都缺/都给 → 拒绝 | `VALIDATION_ERROR` |
| 4 | Task 数 ≤ 200 | `RUN_TOO_LARGE` |
| 5 | `tester name+version` 命中白名单 | `TESTER_NOT_FOUND` |
| 6 | `execution.profile` 已登记 | `EXECUTION_PROFILE_NOT_FOUND` |
| 7 | `args` 词法切分成功；公共参数冲突检查 | `VALIDATION_ERROR` |
| 8 | `on_failure` 不得弱化 `tester.yaml.on_failure`；`pre_check` Tester 必须 `abort_run` | `VALIDATION_ERROR` |
| 9 | `required` 的 env（Tester 声明）都有来源；`secrets` 引用存在 | `VALIDATION_ERROR` |
| 10 | `precheck: auto` 时注册表恰好一个 `role=pre_check` | `VALIDATION_ERROR` |
| 11 | `service` 模式：三个公共参数中 `required: true` 的必须有值（§5.4.1 第 4 条）；**其余 `parameters.required` 一律不校验**（§5.4） | `VALIDATION_ERROR` |
| 12 | `cleanup.policy` 枚举；超时 ≤ 24h | `VALIDATION_ERROR` |

通过后：生成 `resolved` 快照（注入 precheck Task、切分后的 argv、默认超时、Endpoint 来源）→ 落库（事务）→ `submitted` 事件 → 转 `queued` → NOTIFY。

`endpoint_key` 的取值时机：

- `service` 模式：提交时即可规范化计算，**非空**写入；
- `deploy` 模式：提交时 **NULL**（Endpoint 还不存在），Run 进入 `testing` 前由 C-5.3 回填所有 Task；
- 调度器只接受 `endpoint_key` 非空的 `queued` Task（C-4.5 守卫），不设占位键。

**校验只做结构与引用，不做参数语义校验**（§5.4）：不校验 `--dataset` 是否存在、不校验取值范围、不校验类型。

### C-5.2 precheck 注入

| 模式 | 行为 |
|---|---|
| `auto`（默认） | 取注册表唯一 `role=pre_check` 的 Tester；`seq=0`、`name=precheck`；`args=""`（公共参数照常末尾注入，见 I-12 G-11）；`execution.profile` 复用 `tests[0].execution.profile`，无 tests 时取 `precheck.execution.profile`；`on_failure` 取 Tester 声明（必为 `abort_run`） |
| `off` | 不注入。用户若在 `tests[]` 里显式列出 `role: pre_check` 的 Tester，按它自己的 `on_failure=abort_run` 处理，不额外特判 |
| 显式对象 | 提交者指定 tester/version/args/profile/timeout/retry；`on_failure` 仍不得弱化 |

命名冲突（已有 `tests[].name == "precheck"`）→ `VALIDATION_ERROR`。

### C-5.3 Deploy 阶段

Genet 侧的真实语义（调研 `genet-lifecycle.md`）：**一次部署 = 一次 Helm release**，没有部署单、没有作业号，定位符只有 `namespace + release_name`；就绪被拆成两根互不相干的轴——helm `status` 与 `callInfo.endpoint.status`。平台把两者合成自己的五态，并把快照冻结进 `runs.deploy`（形状见 `03-data-view.md §4.4`）。

**C-5.3.1 拉起（一次 POST，先落库再调用）**

| 步骤 | 动作 | 依据 |
|---|---|---|
| 1 | 由 `run_id + deploy_id` 派生确定性 `release_name`（`tp-` + `sha256` 前 12 位），**先写 `runs.deploy`**（含 `values`/`values_digest`/`package_type`/`template_path`/`image`/`model_path`/`set`） | Genet 没有幂等头；POST 超时不代表没创建（202 后是后台 goroutine，`<G>/backend/internal/handlers/helm.go:2981-2990`） |
| 2 | 恢复/重试时先 `GET /api/open/helm/releases/{release_name}`：200 → 直接进轮询；404 且未过 180s 宽限 → 等待，不重发 POST | 同名 release 二次 POST 是 **400**（`already has protected metadata`，`helm.go:2700-2718`），不是幂等成功 |
| 3 | `POST /api/open/helm/uc-packages/{stack\|pd}/releases`，**`noWait=true`**（恒真，理由见 §4.4） | 带 `wait=true` 时 `timeoutSec` 会把排队判成 `failed`，而 Pod 还在排队（`<G>/jenkins/Helm-Start-Service-MB.Jenkinsfile:21-26`） |
| 4 | 202 视为 `accepted`，记 `accepted_at`；4xx/5xx 按最终失败处理（403 资源窗/未绑 namespace、424 chart 未配置） | `helm.go:2933-2936`、`:94-104`、`openapi_helm.go:39-45` |

并发：单 Run 单飞一个 poller（`sync.Map[runID]`），`Poll` 本身幂等无副作用——Genet 后端用 `helm` 子进程，重复打会互相拖慢（调研 D 段）。

**C-5.3.2 状态映射（每次 Poll 按序判定，命中即返回）**

| # | Genet 观测（`GET /releases/{name}`，可选 `/queue/podgroups`） | `deploy.state` | 平台动作 |
|---|---|---|---|
| 1 | 404 且距 `accepted_at` **< 180s** | `accepted` | 视为"尚未 apply 出来"，继续轮询（宽限按**时间窗**，不按次数：退避后次数与时间不再线性） |
| 2 | 404 超过 180s | `blocked` | 视为"release 没能起来"（不是调不通）：`failure.code=DEPLOY_BLOCKED`（可重试）+ `detail.reason=blocked` → Run `service_unavailable`（D-23-C 裁决，与 `execution.md §9.3` 一致） |
| 3 | `status` 以 `pending-` 开头，且 `pods == []` 或所有 Pod `nodeName == ""` | `queued` | 记 `queued_at`；`queue` 取 `/queue/podgroups` 的 `phase` 与 `/queue` 的计数（**无位次**）；写 `deploy.state_changed`，展示已等待时长；可取消 |
| 4 | `status` 以 `pending-` 开头，已有 Pod 被调度（`nodeName != ""`）或已有 `ready=false` 的 Pod | `deploying` | 落 `pods[]` 快照 |
| 5 | `status == "deployed"` 但 `endpoint.status == "pending"` | `deploying` | **不可**当 ready：`noWait` 下 `deployed` 早于地址出现 |
| 6 | `endpoint.status == "ready"` 且 `url` 非空 | `ready` | 写 `runs.endpoint` + 回填 Task `endpoint_key`/`injected_argv`，Run → `testing`（同事务，见 C-5.3.4） |
| 7 | `endpoint.status == "blocked"` | `blocked` | 终态；`failure.message = endpoint.message`（自由文本） |
| 8 | `status ∈ {failed, superseded, uninstalling, uninstalled}` | `blocked` | 终态；`failure.message = release.description`（helm 原文） |
| 9 | 距 `accepted_at` 超 `deploy_timeout_seconds`（默认 72h）仍非终态 | `blocked` | `failure.code=DEPLOY_BLOCKED`（可重试）+ `detail.reason="timeout"` → Run `service_unavailable`（D-46/D-47） |

- `queued / accepted / deploying` 的切分是**平台启发式**（Genet 没有"受理"事件、没有队列状态），`state_detail` 里写明判据，别让用户以为是 Genet 的结论。
- 部署侧失败只折叠成**两个**错误码（附录 D，不新增 `DEPLOY_TIMEOUT`）：`DEPLOY_FAILED`（不可重试：平台侧调 Genet 失败 —— 连接/5xx/同名冲突/安装失败）→ Run `failed`；`DEPLOY_BLOCKED`（可重试：Genet 报 `blocked` 或超 72h 窗口）→ Run `service_unavailable`，细分靠 `detail.reason ∈ {blocked,timeout,superseded,uninstalling,uninstalled}`（D-47 的折叠在此表达）。
- 部署失败/阻塞时所有 Task 置 `skipped`（`reason: service_unavailable`），**不产生 Attempt**（§7.1）。
- `genet_release_status` / `endpoint_status` 两个原值必须落库：排障时要能区分"Genet 说 deployed、我们说 deploying"。

**C-5.3.3 轮询参数**

| 项 | 值 | 依据 |
|---|---|---|
| 单飞 | 每 Run 一个 poller，`sync.Map` 去重 | 后端是 helm 子进程，重复请求互相拖慢 |
| 退避 | 前 30s 每 1s，之后 5s 起指数退避、上限 15s | E-9 |
| 单请求超时 | 10s（connect 5s） | `execution.md:376` |
| 总窗口 | `deploy_timeout_seconds`（默认 72h）；排队计入，取消不受限 | E-9 / K-6 |
| 队列信息 | 每 5 次 Poll 取一次 `/queue` + `/queue/podgroups`；失败**不判 Run 失败**（仅展示） | 现网也是 best-effort（`Jenkinsfile:1856-1860`） |
| 404 宽限 | `accepted_at + 180s` | 现网 `RELEASE_NOT_FOUND_LIMIT=36`×5s，改为时间窗 |

**C-5.3.4 两道门：地址门 + 服务门**

`endpoint.status == "ready"` **只代表地址解析出来了**（router / hostIP / ClusterIP 三选一），既不保证 Pod Ready，也不保证能推理（`<G>/docs/HELM_RELEASE_CALL_ENDPOINT.md:44`；`helm.go:855-928`）。

所以放行 Task 是两道门，缺一不可：

1. **地址门（Deploy 阶段）**：`endpoint.status=ready` → 写 `runs.endpoint`、算 `endpoint_key`、回填该 Run 全部 Task 的 `endpoint_key` 与 `injected_argv`，Run `deploying → testing`（同事务，避免"已 testing 但 Task 没 key"）。
2. **服务门（Pre-Check 阶段）**：Run 进 `testing` 后**仍只放 Pre-Check（`seq=0` 且 `runs.resolved.precheck.injected=true`，见 C-4.5）**，Pre-Check 真正探活通过才放行其余 Task。Pre-Check 失败按 C-5.8：实例 `retryable=true` → `service_unavailable`，否则 `completed_with_errors`。

`precheck: "off"` 时没有服务门：地址门一过全部 Task 入池——这是用户显式选择的风险，报告里必须显示"未做服务探活"。

**C-5.3.5 取消与卸载（不可靠，必须补偿）**

取消 = **先落意图再删**：

1. 事务内写 `runs.deploy.cancel_requested_at`，Run → `cancelling`（C-5.2）；
2. 事务外 `DELETE /api/open/helm/releases/{release_name}?keepHistory=false&force=true`；
3. **404 一律视为成功**（release 尚未出现也算"没有可泄漏的东西"）；409/500（install 在飞，`helm.go:2438`）→ 不退避重试到底，交给对账：下一轮 Poll 见到任一状态就再删一次；
4. 卸载成功不改 Run 终态：Run 仍按用户取消落 `canceled`（R9）。

"删干净"的判据（两条同时成立，缺一不可）：

| 检查 | 成功条件 | 说明 |
|---|---|---|
| `GET /releases/{name}` | 404 | `keepHistory=false` 下应直接 404 |
| `GET /releases/{name}/pods` | HTTP 200 且 `items == []` | **必须**区分"查询失败"与"空列表"：查询失败不能判成功（`Jenkinsfile:3045`） |

- 5 分钟未确认 → `force=true` 重试 → 再确认 2 分钟（对齐现网 `CLEANUP_VERIFY_TIMEOUT_SEC=300`）；
- 仍失败 → `uninstall.state=failed` + `cleanup_errors[]` + `CLEANUP_FAILED` 事件 + 告警；**不改 Run 终态**（`execution.md:371`）；
- `uninstall.state ∈ pending|running|verified|failed`，`verified_at` 只在两条判据都通过时写。

> 不用 Genet 的"release TTL"——它根本没有；每日 23:00 的 GC 只挂起 Deployment/StatefulSet，**不删 Helm release**，也不豁免 `helm-release-name` 类 label（调研 R4，需实机验证）。泄漏只能靠平台的卸载+对账兜底。

**C-5.3.6 与 `execution.md §9` 的分工**

`execution.md §9` 描述适配层（`ports.Deployer` 的实现 + Genet HTTP 细节），本节描述**编排侧怎么用**这些状态。以下改名需两处同时生效（已按本节写法落地，`§9` 重写时对齐）：`deployment_id → deploy_id`、新增 `namespace` / `release_name` / `package_type` / `chart` / `chart_version` / `values` / `values_digest` / `template_path` / `image` / `model_path` / `model_name` / `set` / `no_wait` / `genet_release_status` / `endpoint_status` / `queue{phase,counts}` / `accepted_at` / `queued_at` / `ready_at` / `last_polled_at` / `failure` / `cancel_requested_at` / `uninstall`，移除 `queue_position` / `queue_reason` / `parameters_sha`。

### C-5.4 TESTING 推进

Run 进入 `testing` 时：

- **有 Pre-Check**：只把 `seq=0` 置 `queued`，其余保持 `pending`（`waiting_reason=precheck_gate`）；
- **`precheck: off`**：全部 `pending → queued`（并发由端点槽位决定）。

> "有 Pre-Check"的唯一判据是 **`runs.resolved.precheck.injected = true` 且 `tasks.seq = 0`**（落地 SQL 见 `03-data-view.md §4.2`）；**`task.seq == 0` 单独不构成 Pre-Check**——`precheck: "off"` 时首个测试 Task 的 seq 同样是 0，`endpoint-probe` 这类 Tester 也既可当 Pre-Check 也可当普通测试。门控与放行都读 `store.PrecheckTaskIDs` / `TaskView.IsPrecheck`，不看 `testers.role`。

Task 终态时（同事务）：

1. 失败且 `on_failure=abort_run`（Tester 与 Manifest 取严）→ 执行 C-5.5，终止同 Run 其余未完成 Task；
2. 失败但 `on_failure=continue` → **不影响其他 Task**（它们已在队列/运行中），只是该 Task 记 `failed`；Run 最终按 C-5.8 判 `completed_with_errors`；
3. Pre-Check `succeeded` → 批量 `pending → queued`；
4. 全部 Task 终态 → Run `cleanup`。

> 这里不再有"`seq=n` 成功才推进 `seq=n+1`"的串行语义——§6.7 允许非独占 Task 并行，串行链会直接砍掉精度类测试的并行能力。

### C-5.5 abort_run

```sql
-- 同事务：终止同 Run 内其余所有未完成 Task（不按 seq 区分——它们可能同时在跑）
UPDATE tasks SET status='skipped', status_reason='upstream_abort', updated_at=now()
 WHERE run_id=$run AND id <> $failedId AND status IN ('pending','queued');

UPDATE tasks SET cancel_requested=true, updated_at=now()
 WHERE run_id=$run AND id <> $failedId AND status IN ('preparing','running');
-- Run → cleanup 由"所有 Task 是否终态"驱动：被取消的 Task 完成后自然满足
```

异步动作：对 `cancel_requested` 的 Task 调用 executor 删除 Job（E-5），Attempt 结束置 `canceled`。**不等**删除完成才推进状态（§7.6 的"尽力而为、不阻塞"）。

### C-5.6 重试与续跑

- 同 Run 重试：Attempt 结束为 `failed` 时，若错误码在 `retry_on`（且 `retryable: true`）且 `idx < max_attempts` → `failed → queued`，`available_at = now + backoff`，**不新建 Task**。
- Task 级手动重试（`POST /tasks/{id}/retry`）：仅在终态且未取消时允许；新建 Attempt（`failed/skipped → queued`），立即执行。
- 跨 Run 续跑（P1）：新 Run 的 `retry_of` 指向原 Run；复用条件 = Tester 版本、镜像引用、参数快照、Endpoint 标识四者未变（§7.7），复用 Task 不重新执行，报告中标注来源。

### C-5.7 Cleanup

`cleanup.policy`：`always` 总是卸载；`on_failure` 仅当存在非 `succeeded` 的 Task；`never` 不卸载。

顺序（实现以此为准，2026-11 校正）：停止 collector 对新日志的跟随 → 判定并**落库 Run 终态** → **事务外**调用 Genet uninstall（`DELETE /api/open/helm/releases/{name}`，超时 5 分钟）→ 校验"删干净"（`GET 404` 且 pods 为空）→ 写 `runs.deploy.uninstall{state,verified_at,force,cleanup_errors[]}`；失败写 `cleanup.failed` 事件 + 指标并置 `uninstall.state=failed`，**不回溯 Run 终态**（§6.6），由对账（`ListDeployCleanupPending`）重试 → **异步**生成 Run 报告（失败重试，不改终态）→ `report.written`。

理由：卸载是外部 HTTP，不能放在状态机事务里；先落终态保证"结论不丢"，卸载失败只是资源未回收，必须可重试而不能把已经成立的终态翻掉。`cleanup.policy=never` 的快路径也必须跳过并写 `uninstall.state=skipped`（与对账路径同口径，不再只在对账里判策略）。

### C-5.8 终态判定优先级

从上到下，先命中者胜：

```text
1. 用户取消完成                 → canceled
2. 平台侧错误导致 Run 无法继续（Genet 调用失败、K8s API 持续不可用、内部异常、
   **非 Pre-Check Task 的 `TASK_TIMEOUT`**）→ failed；**单个 Task 的 `EXECUTOR_UNAVAILABLE` 失败**不在此列，按 Task 失败与 `continue`/`abort_run` 处理
3. 部署受理但未就绪 / blocked    → service_unavailable
4. pre_check Task 失败**且错误可重试**（`ENDPOINT_UNREACHABLE` / `PRECHECK_FAILED` / `TASK_TIMEOUT`）→ service_unavailable
5. 所有非 SKIPPED 的 Task 均 succeeded → succeeded
6. 其余                        → completed_with_errors
```

第 2 条里的 `TASK_TIMEOUT` 只对**非 Pre-Check** Task 生效（判据同上：`resolved.precheck.injected=true` 且 `seq=0` 才是 Pre-Check）；Pre-Check 自己的超时仍走第 4 条按实例 `retryable` 判定，两者不冲突。

第 4 条严格按 §7.5：只有"窗口内一直没就绪"这类可重试错误才归 `SERVICE_UNAVAILABLE`。认证失败、4xx 等 `retryable: false` 的 Pre-Check 失败意味着**服务在、但配置/权限不对**，归 `COMPLETED_WITH_ERRORS`，让用户按 §7.5 的指引去"看具体失败 Task"（其错误码与 `retryable` 已在 Task 报告里）。这样不改写 §7.5 的语义，也满足 P0 #4 的验收条件。

进入平台侧终态（`failed` / `canceled`）前，所有未终态 Task 必须收口：`pending`/`queued` → `skipped`（`reason: platform_error` / `canceled`），`preparing`/`running` → 先取消再置 `canceled`。禁止出现"Run 已终态、Task 还停在 QUEUED"的悬挂状态（§8.5）。

### C-5.9 独立 Task 提交（§14.1 #2）

`POST /tasks` 接受 `task:` 形态（§10.3）。落地方式：

1. **禁止 `deploy`**（独立 Task 不拉服务），`service.url` 必填。
2. 合成一个占位 Run：`kind='task'`、`id=r-YYYYMMDD-XXXX`、`name=task.name`、`cleanup_policy=never`、无 deploy；`manifest` 存原始 `task:` 对象，`resolved` 标记 `kind: task`。
3. Task 与 Run 内 Task **完全同构**：`seq=0`；若 `precheck: auto` 注入，则 precheck 为 `seq=0`、业务 Task 为 `seq=1`；同一个状态机、Attempt、产物路径与 `report.json` 结构（满足 §8.1"占位独立标识"）。
4. 校验复用 C-5.1（跳过 deploy 相关项）；precheck 语义与 Run 相同。
5. API 响应同时返回 `task_id` 与合成的 `run_id`；列表按 `runs.kind='task'` 过滤成独立 Task 视图，产物目录与报告路径不做特例。

---

## C-6 取消、超时

### C-6.1 取消 Run

1. `POST /runs/{id}/cancel` → CAS `→ cancelling`；写事件与 audit。
2. `deploying` 阶段取消：先调 Genet 取消排队/卸载（幂等），再置 `canceled`；取消不掉只记 `CLEANUP_FAILED`，不悬挂。
3. 事务外：PENDING/QUEUED 的 Task → `skipped(reason=canceled)`；PREPARING/RUNNING → `cancel_requested=true`，executor 删 Job。
4. 所有 Attempt 结束（或 2 分钟兜底）→ Run `canceled`。
5. 按 cleanup 策略卸载（§7.6 第 4 条）。

### C-6.2 取消单个 Task

只影响该 Task；若其 `on_failure=abort_run`（Tester 声明），按 C-5.5 处理其余 Task（§7.6 末段）。

### C-6.3 超时 watchdog

每 10s 扫描 `attempts.status IN ('preparing','running') AND started_at + timeout_seconds < now()`：

- 调用 executor 删除 Job（先写 `cancel_requested`，避免与自然结束竞态）；
- Attempt → `failed`，错误码 `TASK_TIMEOUT`；
- 后续按重试策略处理（`TASK_TIMEOUT` 可配在 `retry_on` 中）。

Job 侧另有 `activeDeadlineSeconds = timeout + 60s` 兜底（E-5），但**权威判定在平台**。

### C-6.4 竞态规则

- 先落库者胜：状态更新用 CAS；取消与自然结束同时到达时，失败方拿到 `ErrConflict` 后重读状态，不覆盖。
- 删除 Job 的失败只记 `CLEANUP_FAILED`，不回滚 Task 状态。

---

## C-7 报告与产物服务

### C-7.1 `result.json` 校验

时机：Attempt 结束（退出码 0 或非 0）之后、Attempt 落终态之前。

校验规则（附录 A）：

1. 文件不存在或 0 字节 → `RESULT_MISSING`（附录 D 原义）；
2. JSON 非法、schema 不合（含 `schema_version != "1.0"`、缺 `task_id`/`tester`/`metrics`）、或超过 `TESTPLANE_RESULT_MAX_BYTES`（默认 100 MiB，仅防 OOM 的技术上限，非业务限制）→ `RESULT_INVALID`（`detail=too_large` 用于超限）；
3. `metrics[]` 的 `name+statistic+labels` 唯一；`statistic` 枚举；`value` 为 number 或 null；
4. 一致性（§5.8）：退出码 0 但 `result.status = failed` 视为契约不一致，Task 判 `failed`，错误码 `RESULT_INVALID`（detail 记录两侧取值）；退出码非 0 时**仍读** `result.json` 的 `error` 归类：`code` 属附录 D → 用该码 + 实例 `retryable`，否则回退 `TESTER_NONZERO_EXIT`（E-8.1，供 Pre-Check 区分可重试/不可重试）；
5. 允许未知字段（前向兼容，I-11 K-10）；不允许缺失必填。

校验器放 `report.ValidateResult`，用 `schemas/result.schema.json` 做结构校验 + 代码补充唯一性/一致性规则。

### C-7.2 报告生成

> 下表路径均相对 `<storage-root>/<run_id>/`。

| 文件 | 时机 | 幂等 |
|---|---|---|
| `tasks/<task>/report.json` | Task 终态 | 覆盖写（内容由库派生） |
| `<run>/report.json` | Run 终态落库后（异步） | 同上 |
| `<run>/report.md` | 同上 | 同上 |
| `<run>/metadata.json` | 同上 | 裁决快照：版本、镜像引用 + 实际 `image_digest`、Profile、Endpoint、Manifest 原文 |
| `<run>/events.jsonl` | 持续追加 | 按 seq 幂等补齐（C-9） |

报告内容一律取库中终态，不依赖"文件先于状态存在"；生成失败只重试并告警，不回滚终态。`GET .../report` 在文件尚未生成时返回 404 + `Retry-After`。

Run `report.json` 严格按 §8.4：只做组织与索引，不做指标聚合（不求和、不求平均）。单 Run 报告 > 20 MiB 时写摘要 `report.json`（含 `truncated: true` 与 `report_ref`），完整版写 `report.full.json`。

### C-7.3 `report.md` 模板

`状态 / 耗时 / Task 表（名称、状态、Attempt 数、错误码）/ 失败清单 / 产物与报告链接`。不渲染指标图表（UI 负责）。

### C-7.4 `events.jsonl`

- 库是权威；writer 订阅 `state_events` 并按 `seq` 追加，遇已存在 seq 跳过；
- controller 重启后先读文件最后一行 seq，与库比较，缺则从库补齐；
- 每行一个事件对象，含 `seq`、`type`、`run_id`、`task_id`、`from`、`to`、`at`、`actor`、`payload`。

### C-7.5 产物索引与下载

- Attempt 结束后扫描 `attempts/<n>/`，落 `artifacts`（size；sha256 惰性/可选）；
- 下载接口按 `artifacts.path` 白名单读取，**不拼接用户输入**；路径校验拒绝 `..`、绝对路径、符号链接穿越；
- 大文件支持 `Range`；日志用尾部读取（`offset`/`limit`/`tail`），不整文件入内存。

### C-7.6 完整性检查

§8.5 的规则做成断言，在 Run 终态前执行：`succeeded` 必有合法 result；`failed` 必有错误码 + stderr 摘要；`skipped` 必有 reason。不满足则记 `INTERNAL_ERROR` 事件并告警（不改变已发生的 Task 结果）。

---

## C-8 API

### C-8.1 约定

- 前缀 `/api/v1`；JSON；时间 RFC3339；`X-Request-Id` 回显。
- 错误体：`{code, message, detail, retryable, trace_id}`，`code` 只用附录 D 枚举。
- 写操作**不做幂等**：每次 `POST /runs` / `POST /tasks` 都创建新资源，重复提交 = 多个 Run/Task；无 `Idempotency-Key`、不按内容判重（D-37、03 §4.9、07 §4.4）。内部操作（建 Job、Genet 部署）仍用内部幂等键保证重复执行无副作用。
- 分页：**不透明 `cursor`**（`?cursor=<opaque>&limit=50`，上限 200），服务端编码 `(submitted_at DESC, id DESC)`；客户端不解析。
- 请求体上限 1 MiB（Manifest 本身也限制，见 C-5.1）。
- OpenAPI 3.1 为唯一契约源，`oapi-codegen` 生成 server 接口；前端用 `openapi-typescript` 生成类型。

### C-8.2 端点表

> 端点契约以 [`views/07-api.md`](../views/07-api.md) 为权威；本节是摘要，路径/字段以 07 为准。

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| `GET` | `/auth/login` | - | 302 到 OIDC（`?return=` 仅站内相对路径） |
| `GET` | `/auth/callback` | - | 换会话、Set-Cookie、302 回跳 |
| `POST` | `/auth/logout` | viewer | 清会话 |
| `GET` | `/me` | viewer | 当前用户、角色、`csrf_token` |
| `POST` | `/runs` | operator | 提交 Run（YAML/JSON）；`?dry_run=true` 只校验 |
| `GET` | `/runs` | viewer | 列表：status / tester / submitter / 时间 / 游标 |
| `GET` | `/runs/{id}` | viewer | 详情 + Task 摘要 |
| `GET` | `/runs/{id}/tasks` | viewer | Task 列表 |
| `POST` | `/runs/{id}/cancel` | operator | 取消 |
| `POST` | `/runs/{id}/retry` | operator | 重跑该 Run 中失败/跳过的 Task（同 Run 内新 Attempt） |
| `GET` | `/runs/{id}/events` | viewer | SSE，支持 `Last-Event-ID` |
| `GET` | `/runs/{id}/events/history` | viewer | 事件历史（`?since_seq=&limit=`，JSON），用于详情页右栏时间线的首屏 |
| `GET` | `/runs/{id}/report` | viewer | `report.json` |
| `GET` | `/runs/{id}/report.md` | viewer | `report.md`（`text/markdown`） |
| `GET` | `/runs/{id}/manifest` | viewer | Manifest 快照（用于"照上次配置再来一次"与事后核对） |
| `GET` | `/runs/{id}/retry-plan` | viewer | 重试预览：将复用 / 将重跑的 Task（W-4.4） |
| `GET` | `/runs/{id}/deploy` | viewer | 部署快照（manifest/pods/状态） |
| `GET` | `/runs/{id}/deploy/logs` | viewer | `?pod=&segment=&tail=` |
| `GET` | `/runs/{id}/files` | viewer | `?path=` 产物下载（白名单 + Range） |
| `GET` | `/runs/{id}/artifacts` | viewer | Run 级产物清单（报告页与下载列表用） |
| `POST` | `/tasks` | operator | 独立提交 Task（C-5.9，`task:` 形态，`service` 必填 `url`，禁止 `deploy`）；`?dry_run=true` 只校验 |
| `GET` | `/tasks` | viewer | 列表 |
| `GET` | `/tasks/{id}` | viewer | 详情 + Attempt 列表 |
| `POST` | `/tasks/{id}/cancel` | operator | 取消 |
| `POST` | `/tasks/{id}/retry` | operator | 新建 Attempt |
| `GET` | `/tasks/{id}/attempts` | viewer | Attempt 列表 |
| `GET` | `/tasks/{id}/attempts/{idx}/result` | viewer | `result.json` |
| `GET` | `/tasks/{id}/attempts/{idx}/logs` | viewer | `?stream=stdout\|stderr&offset=&limit=&follow=1`（跟随为流式） |
| `GET` | `/tasks/{id}/attempts/{idx}/artifacts` | viewer | 产物清单 |
| `GET` | `/testers` | viewer | 目录（含参数清单，只读） |
| `GET` | `/testers/{name}` | viewer | 最新版本详情 |
| `GET` | `/testers/{name}/versions/{v}` | viewer | 指定版本 |
| `GET` | `/profiles` | viewer | 执行环境列表与挂载能力 |
| `GET` | `/clusters` | viewer | 集群概要（不返回凭据） |
| `GET` | `/secrets` | operator | 可用 Secret 的**名称与说明**（用于提交页下拉，不回显值） |
| `GET` | `/meta` | viewer | 版本 / API 版本 / 特性开关（前端版本对齐用） |
| `POST/PUT/DELETE` | `/admin/profiles` | admin | Profile 维护 |
| `POST` | `/admin/clusters` | admin | 集群登记（`kubeconfig_key` 指向挂载目录中的文件名，不存凭据内容） |
| `GET/POST` | `/admin/secrets` | admin | Secret 引用列表/登记（不回显值） |
| `POST` | `/admin/secrets/{name}/rotate` | admin | 轮换后校验引用可读、记录审计（C-10）；已运行 Job 不受影响 |
| `GET` | `/admin/audit` | admin | 审计查询 |
| `DELETE` | `/admin/runs/{id}/artifacts` | admin | 人工清理单个 Run 的产物（保留期清理的例外通道，见 web-and-delivery §11） |
| `GET` | `/queue-status` | viewer | 平台槽位 + Volcano 等待原因（看板与详情共用） |
| `GET` | `/healthz` `/readyz` `/metrics` | - | 运维 |

> `POST /runs/{id}/retry` 与 P1 的"跨 Run 续跑"不同：前者是同 Run 追加 Attempt，后者新建 Run 并写 `retry_of`，P1 实现（I-13）。

### C-8.3 Manifest 校验实现

- `schemas/manifest.schema.json`（JSON Schema 2020-12）由附录 B/C 与 §10.1 手工维护，CI 校验它与 OpenAPI 一致；
- 业务校验（引用、冲突、配额、env）在 `orchestrator.Validate`，返回 `[]ValidationIssue{field, code, message}`，HTTP 400 + `code: VALIDATION_ERROR`，`detail.issues` 列表；
- 字段路径用 JSON Pointer 风格（`tests[2].args`），UI 直接定位到对应输入。

### C-8.4 参数词法切分器

输入：`args_raw`（一段字符串）。输出：`[]string`。

规则（实现为手写扫描器，约 80 行，禁止用 `sh -c` 或 `strings.Fields`）：

| 规则 | 说明 |
|---|---|
| 空白（空格/Tab/换行）分隔 | 连续空白视为一个分隔 |
| 单引号 | 引号内全部字面量，无转义 |
| 双引号 | 引号内允许 `\"` `\\` 转义；**不做** `$VAR`、反引号、`$(...)` 展开 |
| 引号外反斜杠 | 按字面量保留（不解释） |
| 空参数 | `""` 产生空串 argv，合法 |
| 未闭合引号 | `VALIDATION_ERROR`（`ARGS_LEX_ERROR`） |
| NUL 字节 | 拒绝 |
| 限制 | argv 总数 ≤ 1000，单参数 ≤ 64 KiB |

反例（这些不能被解释）：`--x=$(whoami)`、`--x=a;rm -rf /`、`--x=*`、`--x=$HOME` —— 全部原样成为 argv 的值。

### C-8.5 公共参数注入

1. 取值：`deploy` 场景来自 Genet Endpoint；`service` 场景来自 Manifest；两者都投影成 `url` / `model_name` / `model_path`。
2. 冲突：对切分后的 token 做**精确匹配**——`token == "--url"` 或 `strings.HasPrefix(token, "--url=")`（其余两个同理）；命中即 400（G-6）。
3. 追加：按 `url, model_name, model_path` 固定顺序追加到 argv 末尾，只追加有值的；空值跳过（`required:false` 时由 Tester 兜底）。
4. `required` 检查仅限**三个公共参数**：`url` / `model_name` / `model_path` 中 `required: true` 且无值 → 400（§5.4.1 第 4 条）；Tester `parameters` 里其他 `required` **只供 Agent/人阅读，平台不校验**（§5.4、§13.1）。
5. `endpoint_key` 规范化：`net/url` 解析；无 scheme 补 `http`；host 小写；端口补默认（80/443）；key = `scheme://host:port`；解析失败时用 `raw:<sha256>`（仍保证同一字符串互斥）。

`resolved` 快照里同时保存 `args_raw`、`args_argv`、`injected_argv` 三份，保证事后可完整还原"当时传了什么"（§5.7）。

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
| `run.submitted` | manifest 摘要、run_id、kind |
| `run.state_changed` | from/to、reason |
| `task.state_changed` | task_id、from/to、reason、waiting_reason |
| `attempt.started` / `attempt.finished` | attempt_id、phase、exit_code、error |
| `deploy.state_changed` | state（含 `queued`）、水位计数与原因原文（**无位次**，D-46）、已等待时长、pod 数、message |
| `cleanup.started` / `run.finished` | 结果与计数 |
| `report.written` | 报告路径 |
| `resync_required` | 回放越窗，客户端整页重建（C-9.2） |

事件只描述事实，不携带凭据；`payload` 里禁止出现 secret 值。**日志内容不走 SSE**（会把它变成大流量通道），日志走独立的流式读取接口（C-7.5）。

### C-9.2 SSE 实现

- 连接：`GET /runs/{id}/events`；客户端用 **fetch + ReadableStream**（非原生 EventSource），以便带 `Last-Event-ID` 头、401 时跳 OIDC、后台标签页暂停（W-3）；`Last-Event-ID` = 上次收到的 `seq`；
- 回放：从库读 `seq > last` 的事件，**窗口 24h 且单次上限 1000 条**（`TESTPLANE_SSE_REPLAY_HOURS`）；超出窗口或条数时先发 `resync_required`（客户端整页重建），不静默截断；
- 实时：`LISTEN testplane_run_<id>`（或统一频道 + 过滤），`NOTIFY` 载荷只含 `run_id, seq`，进程再查库拿完整事件（避免 PG 8KB 载荷限制）；
- 心跳：15s 一次 `: ping`；空闲 60s 断开，客户端自动重连；
- 顺序：同一 Run 的 seq 单调，客户端按 seq 去重。

### C-9.3 多副本

每个 api 副本都 `LISTEN` 同一频道；收到通知后按需查库。`NOTIFY` 在事务提交后发送，避免推已回滚事件。不做跨副本会话粘滞。

---

## C-10 认证与授权

- **UI**：OIDC authorization code + PKCE；会话 Cookie（`HttpOnly; Secure; SameSite=Lax`）；写操作要求 `X-CSRF-Token`（双提交）。
- **服务调用**（Jenkins / Agent）：`Authorization: Bearer <JWT>`，由统一身份源签发，平台用 JWKS 校验；无匿名路径。
- **开发模式**：`--auth=dev` 显式开启，默认角色来自配置；生产镜像构建时该模式默认关闭并在 `/healthz` 里打印警告。
- **RBAC**（§12.1）：`viewer` 读；`operator` 提交/取消/重试；`admin` 另加 Profile/Cluster/Secret 维护与审计查询。**Tester 注册不在运行时进行**：只随平台镜像发布，由 CI 校验（K-3）。组到角色的映射放配置（`groups:role`），不在代码里硬编码。
- **审计**：所有写操作在事务内写 `audit_log`（actor、role、action、object、detail），失败也记录尝试。
- **回显**：`env` 明文可回显；`secrets` 只返回名称；Secret 值永不进 API 响应。
- **Secret 轮换**（§12.3）：值由 admin 在 Secret Store 侧更新（平台**无写 Secret 权限**）；随后调 `POST /admin/secrets/{name}/rotate`，平台重读引用做一次可读性校验并记 `secret_rotated` 审计。已运行 Job 的环境变量不可变，轮换只影响**新 Attempt**；轮换失败不回滚已登记引用，只告警。**平台不读 Secret 值做日志脱敏**（K-5 已拍板日志明文）。

---

## C-11 配置项

| 配置 | 默认 | 说明 |
|---|---|---|
| `TESTPLANE_LISTEN` | `:8080` | API 监听 |
| `TESTPLANE_DB_DSN` | - | PostgreSQL DSN |
| `TESTPLANE_DB_MAX_OPEN_CONNS` | `20` | 连接池上限（P95 的关键参数） |
| `TESTPLANE_STORAGE_ROOT` | `/storage-root` | NFS 挂载点（对应 `<storage-root>`） |
| `TESTPLANE_ROLE` | `all` | `api` / `controller` / `collector` / `all` |
| `TESTPLANE_CONCURRENCY_MAX` | `20` | 全局并发 Task 上限（§12.7） |
| `TESTPLANE_RUN_MAX_TASKS` | `200` | 单 Run Task 上限 |
| `TESTPLANE_TASK_TIMEOUT` | `4h` | 默认 Task 超时 |
| `TESTPLANE_TASK_TIMEOUT_MAX` | `24h` | 上限 |
| `TESTPLANE_DEPLOY_TIMEOUT` | `72h` | Deploy 排队+部署总窗口（K-6，Profile 可覆盖） |
| `TESTPLANE_RESULT_MAX_BYTES` | `100MiB` | `result.json` 技术上限（仅防 OOM；K-10 不设业务限制） |
| `TESTPLANE_ARTIFACT_MAX_BYTES` | `10GiB` | 单 Attempt 产物配额 |
| `TESTPLANE_LOG_SEGMENT_MAX_BYTES` | `200MiB` | 日志分段阈值 |
| `TESTPLANE_LOG_TAIL_LINES` | `2000` | 默认日志尾部行数 |
| `TESTPLANE_RECONCILE_INTERVAL` | `30s` | 对账周期（E-4.2） |
| `TESTPLANE_EXECUTOR_NAMESPACE` | `testplane` | Job 与 PVC 所在 namespace（E-2） |
| `TESTPLANE_TESTERS_DIR` | `/app/testers` | 注册表扫描来源（镜像内；启动时扫描一次，K-3） |
| `TESTPLANE_SSE_HEARTBEAT` | `15s` | SSE 心跳 |
| `TESTPLANE_SSE_REPLAY_HOURS` | `24` | SSE 回放窗口（C-9.2） |
| `TESTPLANE_RETENTION_DAYS` | `30` | 产物保留期（G-13，与 cleanup.policy 解耦） |
| `TESTPLANE_OIDC_ISSUER` / `TESTPLANE_OIDC_CLIENT_ID` / `TESTPLANE_OIDC_CLIENT_SECRET` | - | 身份源；另配 `TESTPLANE_OIDC_SCOPES`（默认 `openid profile email groups`）与 `TESTPLANE_OIDC_ROLE_CLAIM`（默认 `groups`），见 §12.1 与 web-and-delivery §10 |
| `TESTPLANE_SESSION_COOKIE` / `_TTL` / `_KEY` | `tp_session` / `8h` / - | 会话 |
| `TESTPLANE_AUTH_MODE` | `oidc` | `dev` 仅在 dev 允许（web-and-delivery §14 红线） |
| `TESTPLANE_GENET_BASE_URL` / `_TOKEN_FILE` / `_TIMEOUT` | - | Genet 接入（令牌用文件，支持轮换热加载） |
| `TESTPLANE_CLUSTERS_DIR` / `_DEFAULT_CLUSTER` | `/etc/testplane/clusters` / 空 | 多集群 kubeconfig 目录（K-4） |
| `TESTPLANE_K8S_IN_CLUSTER` | `true` | 集群内运行；false 时用 kubeconfig |
| `TESTPLANE_METRICS_ENABLED` | `true` | `/metrics` |

配置走环境变量 + 可选 YAML；启动时校验必填项与取值范围，错误直接退出（fail fast）。Env 到内部字段的映射在 `internal/config` 一处完成（如 `TESTPLANE_ARTIFACT_MAX_BYTES → artifact_quota_bytes`、`TESTPLANE_LOG_SEGMENT_MAX_BYTES → log_segment_bytes`），E 文档用内部字段名、C 文档用 env 名，二者指向同一值。

---

## C-12 容量与性能

| 维度 | P0 目标 | 实现要点 |
|---|---|---|
| 状态查询 P95 | < 300 ms（§12.7） | 游标分页、覆盖索引、避免详情页 N+1（Task 列表用一条 JOIN） |
| 单 Run 事件量 | 200 Task × ~6 事件 ≈ 1200 行 | 单 Run 事件流小，SSE 回放一次读得完 |
| 调度吞吐 | 20 并发 Task，单 Task 分钟级 | 调度事务毫秒级；瓶颈在 K8s 而非库 |
| 报告体积 | ≤ 20 MiB / Run | 超限外置摘要（C-7.2） |
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
// EventSink.Emit / ResultSink.FinishAttempt 提交事实与结论，
// 状态转移（CAS、先落库者胜）仍在 control-plane 的事务内完成。
```

- executor **不写** `tasks.status`；它通过 `ResultSink` 提交结论，由 control-plane 在事务内做 CAS（E-5.3）；
- 日志采集（collector）与 executor 同属执行层，但独立进程角色；
- Registry 只读 `testers/` 并算 diff + 错误清单，落库走 `RegistryStore`（control-plane 实现）；
- Genet adapter 只调外部 API 并返回结构化结果，不写库；
- 所有跨层调用都在事务外，结果再进事务落地。
