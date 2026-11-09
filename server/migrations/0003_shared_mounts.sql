-- shared_mounts：平台级统一共享挂载（D-63 → D-65 升级为运行时可管理资源）。
--
-- 语义（ARCHITECTURE D-65）：
--   * 本表是**运行时真源**：executor 渲染所有 Tester Job（含 Pre-Check）时读"启用中的行"；
--   * TESTPLANE_SHARED_MOUNTS 只在**表为空**时作为首次启动的种子导入，之后不再覆盖；
--   * 只读引用 PVC（claim_name 必填）：内联 volumes[].nfs 会被 PodSecurity restricted 拒绝；
--   * 平台**不创建** PV/PVC（存储供给超出 §10.4 边界）。
CREATE TABLE IF NOT EXISTS shared_mounts (
  name        text PRIMARY KEY,
  mount_path  text NOT NULL,
  read_only   boolean NOT NULL DEFAULT true,
  claim_name  text NOT NULL,
  enabled     boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  created_by  text NOT NULL DEFAULT 'system:seed',
  updated_by  text NOT NULL DEFAULT 'system:seed',
  CONSTRAINT shared_mounts_path_uq UNIQUE (mount_path),
  CONSTRAINT shared_mounts_claim_ck CHECK (btrim(claim_name) <> ''),
  CONSTRAINT shared_mounts_path_ck CHECK (mount_path LIKE '/%')
);

-- 渲染时按 enabled 过滤，走一个部分索引即可。
CREATE INDEX IF NOT EXISTS shared_mounts_enabled_idx ON shared_mounts (enabled) WHERE enabled;
