-- ============================================================================
-- Argus v2 初始表结构（PostgreSQL 16）
-- 用途：单次 bootstrap，一次建出 control-plane 需要的全部表、约束与索引。
-- 来源：design/views/03-data-view.md §3.2「完整 DDL」逐字转写（表结构唯一权威）。
-- 注意：该文件由设计文档生成，改表必须同步改 design/views/03-data-view.md 与设计视图。
-- 形态：纯顺序 SQL，无 golang-migrate 标记（无 -- +migrate Up），每条语句以分号结尾。
--
-- v2 术语（ARCHITECTURE §0.2 / D-66～D-73）：runs→tasks、内层 tasks→task_runners、
-- attempts→task_attempts（Task 级计数行）、testers→runners。
-- 本迁移是 pre-release 的整体替换：v1 迁移链（0001~0003）已删除，旧库需重建。
-- ============================================================================

-- ============ 枚举注释（实现为 CHECK，不用 PG enum，便于灰度） ============
-- tasks.status:         submitted|queued|running|succeeded|failed|service_unavailable|cancelling|canceled
--                       （v2 无 deploying/testing/cleanup/completed_with_errors，§7.4）
-- task_runners.status:  pending|queued|preparing|running|succeeded|failed|skipped|canceled
-- task_attempts.status: running|succeeded|failed|canceled
-- task_runners.waiting_reason: slot_waiting|global_limit|queue_full|insufficient_resources|gang_pending（§6.9 / C-4.7）
-- task_runners.status_reason(skipped): upstream_abort|canceled

CREATE TABLE tasks (
  id               text PRIMARY KEY,                        -- t-YYYYMMDD-XXXX
  name             text NOT NULL CHECK (char_length(name) <= 200),   -- D-58
  status           text NOT NULL,
  manifest         jsonb NOT NULL,                          -- 提交原文（嵌套 task:，secrets 只有引用）
  resolved         jsonb NOT NULL,                          -- 冻结解析结果（见 03 §4.2）
  endpoint         jsonb,                                   -- 见 03 §4.3
  deploy           jsonb,                                   -- 见 03 §4.4（含 deploy-state 副本合成；queue.position 恒 null，D-46）
  env              jsonb NOT NULL DEFAULT '{}',             -- Task 级明文；可回显（§12.4）
  secret_env       jsonb NOT NULL DEFAULT '[]',             -- [{name, secret_ref}]
  submitted_by     text NOT NULL,
  submitted_at     timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  error            jsonb,                                   -- Failure（03 §4.7）
  retry_of         text REFERENCES tasks(id),               -- P1 跨 Task 续跑（仅外接模式，§7.7）
  current_attempt  int NOT NULL DEFAULT 0 CHECK (current_attempt >= 0),
  cancel_requested boolean NOT NULL DEFAULT false,
  last_event_seq   bigint NOT NULL DEFAULT 0,               -- 事件 seq 分配器
  revision         bigint NOT NULL DEFAULT 1,               -- 乐观锁（可选）
  CONSTRAINT tasks_status_ck CHECK (status IN (
    'submitted','queued','running','succeeded','failed','service_unavailable','cancelling','canceled')),
  CONSTRAINT tasks_finished_ck CHECK (finished_at IS NULL OR finished_at >= submitted_at)
);
CREATE INDEX tasks_status_time_idx ON tasks (status, submitted_at DESC);
CREATE INDEX tasks_time_id_idx     ON tasks (submitted_at DESC, id DESC);

CREATE TABLE task_attempts (
  task_id          text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_no       int  NOT NULL CHECK (attempt_no >= 1),
  status           text NOT NULL,
  trigger          text NOT NULL DEFAULT 'initial'
                   CHECK (trigger IN ('initial','retry')),
  input_ref        text,                                    -- attempts/<n>/input/
  exports_ref      text,                                    -- attempts/<n>/exports/（含合并注入清单）
  deploy_state_ref text,                                    -- attempts/<n>/deploy-state.json 副本
  error            jsonb,
  started_at       timestamptz,
  finished_at      timestamptz,
  PRIMARY KEY (task_id, attempt_no),
  CONSTRAINT task_attempts_status_ck CHECK (status IN ('running','succeeded','failed','canceled')),
  CONSTRAINT task_attempts_trigger_ck CHECK (
    trigger <> 'retry' OR attempt_no > 1)                   -- attempt 1 恒为 initial
);
CREATE UNIQUE INDEX task_attempts_active_uq ON task_attempts (task_id)
  WHERE status = 'running';                                  -- 至多一个活跃 attempt
CREATE INDEX task_attempts_status_idx ON task_attempts (status) WHERE status = 'running';

CREATE TABLE task_runners (
  task_id            text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_no         int  NOT NULL CHECK (attempt_no >= 1),
  seq                int  NOT NULL CHECK (seq >= 1),         -- 含注入的 deploy/precheck/clean，从 1 递增
  name               text NOT NULL,
  runner_name        text NOT NULL,
  runner_version     text NOT NULL,
  role               text NOT NULL CHECK (role IN ('test','pre_check','deploy','clean')),
  image_ref          text NOT NULL,                          -- 冻结引用；平台不解析
  image_digest       text,                                   -- 实际 digest（K8s imageID 解析）
  profile            text NOT NULL,
  input_ref          text NOT NULL,                          -- attempts/<n>/input/ 落档引用
  on_failure         text NOT NULL CHECK (on_failure IN ('continue','abort_task')),
  timeout_seconds    int  NOT NULL CHECK (timeout_seconds > 0 AND timeout_seconds <= 86400),
  status             text NOT NULL,
  status_reason      text,
  waiting_reason     text,
  endpoint_key       text,
  endpoint_exclusive boolean NOT NULL,                       -- 仅外接模式有效（§6.7）
  exit_code          int,
  error_code         text,                                   -- 附录 D 枚举
  error              jsonb,
  exports_ref        text,                                   -- attempts/<n>/exports/<seq>.exports.env
  result_ref         text,                                   -- attempts/<n>/runners/<seq>/result.json（三选一之一，D-74）
  log_ref            text,                                   -- attempts/<n>/runners/<seq>/logs/stdout.log
  job_id             text NOT NULL UNIQUE,                   -- 幂等键 (task_id, attempt_no, seq) 的派生（附录 B）
  job_namespace      text,
  phase              text,                                   -- 附录 B Executor phase 原值
  executor_ref       jsonb,                                  -- {cluster, namespace, job}
  cancel_requested   boolean NOT NULL DEFAULT false,
  started_at         timestamptz,
  finished_at        timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, attempt_no, seq),
  CONSTRAINT task_runners_name_uq UNIQUE (task_id, attempt_no, name),
  CONSTRAINT task_runners_status_ck CHECK (status IN (
    'pending','queued','preparing','running','succeeded','failed','skipped','canceled')),
  CONSTRAINT task_runners_skipped_reason_ck CHECK (status <> 'skipped' OR status_reason IN (
    'upstream_abort','canceled')),
  CONSTRAINT task_runners_waiting_ck CHECK (waiting_reason IS NULL OR waiting_reason IN (
    'slot_waiting','global_limit','queue_full','insufficient_resources','gang_pending'))
);
CREATE INDEX task_runners_sched_idx ON task_runners (status, seq)
  WHERE status IN ('pending','queued');
CREATE INDEX task_runners_endpoint_idx ON task_runners (endpoint_key)
  WHERE status IN ('preparing','running');
-- 不变量：一个 attempt 内至多一个进行中的 runner（严格串行，D-69）
CREATE UNIQUE INDEX task_runners_active_uq ON task_runners (task_id, attempt_no)
  WHERE status IN ('preparing','running');

-- append-only：状态事实流，派生 events.jsonl / SSE
CREATE TABLE state_events (
  id          bigserial PRIMARY KEY,
  task_id     text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_no  int,
  runner_seq  int,
  seq         bigint NOT NULL,                              -- 每 Task 单调
  type        text NOT NULL,
  from_status text,
  to_status   text,
  reason      text,
  payload     jsonb NOT NULL DEFAULT '{}',
  actor       text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT state_events_seq_uq UNIQUE (task_id, seq)
);
CREATE INDEX state_events_task_idx ON state_events (task_id, seq);

-- `type` 的取值（ARCHITECTURE §12.6 + §7.6）：
--   submitted | state_changed | runner_started | runner_finished | exports_harvested
--   | clean_finished | cleanup_failed | task_finished | report.written | resync_required
--   | deploy.state_changed
--
-- `cleanup_failed` 只在「Task 已终态但 service-clean 未通过判干净校验（GET 404 + pods 空，D-54）」时写一条，
-- payload = {release_name, clean_state, cleanup_errors[], verified_at}；
-- 它不改 Task 终态，是给"资源没删干净"留证据（07 §7 的事件表与之一致），并进 §9.3 重放清单。

-- 端点互斥槽位（同一 endpoint_key 上：至多 1 个独占，或 N 个共享；仅外接模式，§6.7）
CREATE TABLE endpoint_leases (
  task_id      text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_no   int  NOT NULL,
  runner_seq   int  NOT NULL,
  endpoint_key text NOT NULL,
  exclusive    boolean NOT NULL,
  acquired_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, attempt_no, runner_seq)
);
CREATE UNIQUE INDEX endpoint_lease_excl_uq ON endpoint_leases (endpoint_key) WHERE exclusive;
CREATE INDEX endpoint_lease_key_idx ON endpoint_leases (endpoint_key);

CREATE TABLE runners (                                       -- Runner 注册表（v1 testers 表，D-66）
  name          text NOT NULL,
  version       text NOT NULL,
  display_name  text NOT NULL,
  description   text NOT NULL,
  role          text NOT NULL CHECK (role IN ('test','pre_check','deploy','clean')),
                -- deploy/clean 仅内置：注册校验拒绝用户目录声明（§5.1），库层不做角色×来源约束
  builtin       boolean NOT NULL DEFAULT false,              -- service-deploy / service-clean / endpoint-probe
  on_failure    text NOT NULL CHECK (on_failure IN ('continue','abort_task')),
  entry         jsonb NOT NULL,                              -- exec form 数组（如 ["/runner/run.sh"]，§5.2）
  input_format  text NOT NULL DEFAULT 'jsonl'
                CHECK (input_format IN ('jsonl','yaml','text')),   -- §5.4（D-67）
  image         text NOT NULL,
  resources     jsonb NOT NULL,
  concurrency   jsonb NOT NULL,                             -- {endpoint_exclusive: bool}，仅外接模式有效
  mounts        jsonb NOT NULL DEFAULT '[]',                -- [{name, mount_path, read_only, source}]
  k8s           jsonb NOT NULL DEFAULT '{}',
  parameters    jsonb NOT NULL DEFAULT '[]',                -- jsonl 结构校验 schema（类型+required，§5.4）；yaml/text 时必须 []
  exports       jsonb NOT NULL DEFAULT '[]',                -- 本 Runner 产出的下游 key（§5.12）
  consumes      jsonb NOT NULL DEFAULT '[]',                -- 需要的上游 key；组合期校验闭合
  env_decl      jsonb NOT NULL DEFAULT '{}',
  placeholder   text,                                       -- 与 input_format 同格式的合法片段（§5.4）
  source_file   text NOT NULL,
  content_sha   text NOT NULL,
  orphaned      boolean NOT NULL DEFAULT false,
  removed_at    timestamptz,
  registered_at timestamptz NOT NULL DEFAULT now(),
  registered_by text NOT NULL,
  PRIMARY KEY (name, version)
);
CREATE INDEX runners_role_idx ON runners (role) WHERE NOT orphaned;

CREATE TABLE clusters (
  name              text PRIMARY KEY,
  api_server        text NOT NULL,
  kubeconfig_key    text NOT NULL,                          -- 挂载目录内的文件名，不存内容
  description       text,
  last_check_at     timestamptz,
  last_check_result text,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE execution_profiles (
  name                   text PRIMARY KEY,
  cluster                text NOT NULL REFERENCES clusters(name),
  namespace              text NOT NULL,
  mounts                 jsonb NOT NULL DEFAULT '[]',
  scheduler              text NOT NULL DEFAULT 'default'
                         CHECK (scheduler IN ('default','volcano')),
  queue                  text,
  image_pull_secrets     jsonb NOT NULL DEFAULT '[]',
  resource_defaults      jsonb NOT NULL DEFAULT '{}',
  deploy_timeout_seconds int NOT NULL DEFAULT 259200        -- 72h：service-deploy 的部署总窗口（§12.7）
                         CHECK (deploy_timeout_seconds BETWEEN 3600 AND 604800),
  network_policy         jsonb NOT NULL DEFAULT '{}',
  created_by             text NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now()
);

-- shared_mounts：平台级统一共享挂载（D-63 → D-65 升级为运行时可管理资源）。
-- 本表是**运行时真源**：executor 渲染所有 runner Job 时读"启用中的行"；
-- AG_SHARED_MOUNTS 只在表为空时作为首次启动的种子导入，之后不再覆盖。
CREATE TABLE shared_mounts (
  name        text PRIMARY KEY,                           -- volume 名（渲染前会 sanitize/去重）
  mount_path  text NOT NULL,                              -- 容器内绝对路径（如 /models）
  read_only   boolean NOT NULL DEFAULT true,              -- 默认只读：模型权重是只读输入
  claim_name  text NOT NULL,                              -- 集群里已存在的 PVC 名（平台不创建 PV/PVC）
  enabled     boolean NOT NULL DEFAULT true,              -- false = 保留记录但不注入
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  created_by  text NOT NULL DEFAULT 'system:seed',        -- 首次启动的 env 种子 / 管理面操作人
  updated_by  text NOT NULL DEFAULT 'system:seed',
  CONSTRAINT shared_mounts_path_uq UNIQUE (mount_path),   -- 同一路径只允许一条（挂两次是配置错误）
  CONSTRAINT shared_mounts_claim_ck CHECK (btrim(claim_name) <> ''),
  CONSTRAINT shared_mounts_path_ck  CHECK (mount_path LIKE '/%')
);
CREATE INDEX shared_mounts_enabled_idx ON shared_mounts (enabled) WHERE enabled;  -- 渲染只读启用行

CREATE TABLE secret_refs (
  name         text PRIMARY KEY,                            -- Manifest 里写的字符串
  namespace    text NOT NULL,
  secret_name  text NOT NULL,
  key          text NOT NULL,
  description  text,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- 产物索引（可重建）：路径相对 <storage-root>/<task_id>/
CREATE TABLE artifacts (
  task_id    text NOT NULL,
  attempt_no int,
  runner_seq int,
  path       text NOT NULL,                                 -- 如 attempts/1/runners/3/result.json
  size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
  sha256     text,
  kind       text NOT NULL CHECK (kind IN ('result','raw','log','report','export','other')),
  mtime      timestamptz,
  indexed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, path)
);
CREATE INDEX artifacts_attempt_idx ON artifacts (task_id, attempt_no);

CREATE TABLE audit_log (
  id          bigserial PRIMARY KEY,
  actor       text NOT NULL,
  role        text NOT NULL,
  action      text NOT NULL,                                -- submit|cancel|retry|register|rotate|cleanup_artifacts|...
  object_type text NOT NULL,
  object_id   text,
  detail      jsonb NOT NULL DEFAULT '{}',
  at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_at_idx     ON audit_log (at DESC);
CREATE INDEX audit_object_idx ON audit_log (object_type, object_id);
