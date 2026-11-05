-- ============================================================================
-- TestPlane 初始表结构（PostgreSQL 16）
-- 用途：单次 bootstrap，一次建出 control-plane 需要的全部表、约束与索引。
-- 来源：design/views/03-data-view.md §3.2「完整 DDL」逐字转写（表结构唯一权威）。
-- 注意：该文件由设计文档生成，改表必须同步改 design/views/03-data-view.md 与设计视图。
-- 形态：纯顺序 SQL，无 golang-migrate 标记（无 -- +migrate Up），每条语句以分号结尾。
-- ============================================================================

-- ============ 枚举注释（实现为 CHECK，不用 PG enum，便于灰度） ============
-- runs.status:    submitted|queued|deploying|testing|cleanup|cancelling|
--                 succeeded|completed_with_errors|service_unavailable|failed|canceled
-- tasks.status:   pending|queued|preparing|running|succeeded|failed|canceled|skipped
-- attempts.status:preparing|running|succeeded|failed|canceled
-- tasks.waiting_reason: precheck_gate|slot_waiting|global_limit|queue_full|insufficient_resources|gang_pending

CREATE TABLE runs (
  id               text PRIMARY KEY,
  name             text NOT NULL,
  kind             text NOT NULL DEFAULT 'run'
                   CHECK (kind IN ('run','task')),          -- 'task' = 独立 Task 合成的占位 Run
  status           text NOT NULL,
  manifest         jsonb NOT NULL,                          -- 提交原文（secrets 只有引用）
  resolved         jsonb NOT NULL,                          -- 冻结解析结果（见 §4.2）
  endpoint         jsonb,                                   -- 见 §4.3
  deploy           jsonb,                                   -- 见 §4.4
  cleanup_policy   text NOT NULL DEFAULT 'always'
                   CHECK (cleanup_policy IN ('always','on_failure','never')),
  submitted_by     text NOT NULL,
  submitted_at     timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  error            jsonb,                                   -- Failure（§4.7）
  retry_of         text REFERENCES runs(id),                -- P1 跨 Run 续跑
  last_event_seq   bigint NOT NULL DEFAULT 0,               -- 事件 seq 分配器
  revision         bigint NOT NULL DEFAULT 1,               -- 乐观锁（可选）
  CONSTRAINT runs_status_ck CHECK (status IN (
    'submitted','queued','deploying','testing','cleanup','cancelling',
    'succeeded','completed_with_errors','service_unavailable','failed','canceled')),
  CONSTRAINT runs_finished_ck CHECK (finished_at IS NULL OR finished_at >= submitted_at)
);
CREATE INDEX runs_status_time_idx ON runs (status, submitted_at DESC);
CREATE INDEX runs_time_id_idx     ON runs (submitted_at DESC, id DESC);
CREATE INDEX runs_kind_time_idx   ON runs (kind, submitted_at DESC);

CREATE TABLE tasks (
  id                  text PRIMARY KEY,
  run_id              text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq                 int  NOT NULL CHECK (seq >= 0),       -- 0 = 平台注入的 Pre-Check
  name                text NOT NULL,
  tester_name         text NOT NULL,
  tester_version      text NOT NULL,
  image_ref           text NOT NULL,
  profile             text NOT NULL,
  args_raw            text NOT NULL,
  args_argv           jsonb NOT NULL,                       -- 词法切分结果
  injected_argv       jsonb NOT NULL DEFAULT '[]',
  env                 jsonb NOT NULL DEFAULT '{}',          -- 明文；可回显
  secret_env          jsonb NOT NULL DEFAULT '[]',          -- [{name, secret_ref}]
  on_failure          text NOT NULL CHECK (on_failure IN ('continue','abort_run')),
  timeout_seconds     int  NOT NULL CHECK (timeout_seconds > 0 AND timeout_seconds <= 86400),
  retry_policy        jsonb NOT NULL DEFAULT '{}',          -- §4.5
  status              text NOT NULL,
  status_reason       text,
  waiting_reason      text,
  endpoint_key        text,                                 -- deploy 模式 ready 后回填
  endpoint_exclusive  boolean NOT NULL,
  current_attempt     int NOT NULL DEFAULT 0 CHECK (current_attempt >= 0),
  available_at        timestamptz,
  cancel_requested    boolean NOT NULL DEFAULT false,
  result_ref          text,
  reused_from_task_id text,
  error               jsonb,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tasks_name_uq UNIQUE (run_id, name),
  CONSTRAINT tasks_seq_uq  UNIQUE (run_id, seq),
  CONSTRAINT tasks_status_ck CHECK (status IN (
    'pending','queued','preparing','running','succeeded','failed','canceled','skipped')),
  CONSTRAINT tasks_waiting_ck CHECK (waiting_reason IS NULL OR waiting_reason IN (
    'precheck_gate','slot_waiting','global_limit','queue_full','insufficient_resources','gang_pending')),
  CONSTRAINT tasks_queued_endpoint_ck CHECK (status <> 'queued' OR endpoint_key IS NOT NULL),
  CONSTRAINT tasks_skipped_reason_ck CHECK (status <> 'skipped' OR status_reason IN (
    'upstream_abort','canceled','deploy_failed','service_unavailable','platform_error'))
);
CREATE INDEX tasks_ready_idx ON tasks (status, available_at) WHERE status = 'queued';
CREATE INDEX tasks_run_idx   ON tasks (run_id, seq);
CREATE INDEX tasks_endpoint_idx ON tasks (endpoint_key) WHERE status IN ('preparing','running');

CREATE TABLE attempts (
  id             text PRIMARY KEY,                          -- <task>-a<n>
  task_id        text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  idx            int  NOT NULL CHECK (idx >= 1),
  status         text NOT NULL,
  job_id         text NOT NULL UNIQUE,                      -- 内部幂等键（Job 去重）
  job_namespace  text,
  phase          text,                                      -- 附录 B 原值
  image_digest   text,                                      -- 实际拉取到的 digest（K8s imageID）；同 tag 不同代码的唯一证据
  exit_code      int,
  started_at     timestamptz,
  finished_at    timestamptz,
  log_ref        text,
  artifact_ref   text,
  result_ref     text,
  error          jsonb,
  executor_ref   jsonb,                                     -- {cluster, namespace, job}
  CONSTRAINT attempts_status_ck CHECK (status IN ('preparing','running','succeeded','failed','canceled')),
  CONSTRAINT attempts_uq UNIQUE (task_id, idx)
);
CREATE UNIQUE INDEX attempts_active_uq ON attempts (task_id)
  WHERE status IN ('preparing','running');
CREATE INDEX attempts_status_idx ON attempts (status) WHERE status IN ('preparing','running');

-- append-only：状态事实流，派生 events.jsonl / SSE
CREATE TABLE state_events (
  id          bigserial PRIMARY KEY,
  run_id      text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  task_id     text,
  attempt_id  text,
  seq         bigint NOT NULL,                              -- 每 Run 单调
  type        text NOT NULL,
  from_status text,
  to_status   text,
  reason      text,
  payload     jsonb NOT NULL DEFAULT '{}',
  actor       text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT state_events_seq_uq UNIQUE (run_id, seq)
);
CREATE INDEX state_events_run_idx ON state_events (run_id, seq);

-- 端点互斥槽位（同一 endpoint_key 上：至多 1 个独占，或 N 个共享）
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
  concurrency   jsonb NOT NULL,                             -- {endpoint_exclusive: bool}
  mounts        jsonb NOT NULL DEFAULT '[]',
  k8s           jsonb NOT NULL DEFAULT '{}',
  parameters    jsonb NOT NULL DEFAULT '{}',                -- 文档语义，不参与运行校验
  env_decl      jsonb NOT NULL DEFAULT '{}',
  placeholder   text,
  source_file   text NOT NULL,
  content_sha   text NOT NULL,
  orphaned      boolean NOT NULL DEFAULT false,
  removed_at    timestamptz,
  registered_at timestamptz NOT NULL DEFAULT now(),
  registered_by text NOT NULL,
  PRIMARY KEY (name, version)
);
CREATE INDEX testers_role_idx ON testers (role) WHERE NOT orphaned;

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
  deploy_timeout_seconds int NOT NULL DEFAULT 259200        -- 72h
                         CHECK (deploy_timeout_seconds BETWEEN 3600 AND 604800),
  network_policy         jsonb NOT NULL DEFAULT '{}',
  created_by             text NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE secret_refs (
  name         text PRIMARY KEY,                            -- Manifest 里写的字符串
  namespace    text NOT NULL,
  secret_name  text NOT NULL,
  key          text NOT NULL,
  description  text,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- 产物索引（可重建）：路径相对 <storage-root>
CREATE TABLE artifacts (
  run_id     text NOT NULL,
  task_id    text,
  attempt_id text,
  path       text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
  sha256     text,
  kind       text NOT NULL CHECK (kind IN ('result','raw','log','report','other')),
  mtime      timestamptz,
  indexed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, path)
);
CREATE INDEX artifacts_attempt_idx ON artifacts (attempt_id);

CREATE TABLE audit_log (
  id          bigserial PRIMARY KEY,
  actor       text NOT NULL,
  role        text NOT NULL,
  action      text NOT NULL,                                -- submit|cancel|retry|rotate|...
  object_type text NOT NULL,
  object_id   text,
  detail      jsonb NOT NULL DEFAULT '{}',
  at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_at_idx     ON audit_log (at DESC);
CREATE INDEX audit_object_idx ON audit_log (object_type, object_id);
