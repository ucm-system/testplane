package store

// 管理面（07 §5.4 admin）的写操作：Execution Profile / Cluster / Secret 引用维护，
// 以及人工清理产物的索引删除。
//
// 约定（05 §3.2 硬规则 1：只有 store 执行 SQL）：
//   - 事务边界仍由调用方控制：tx 传 nil 表示自动提交；
//   - 不在这里写审计：审计由 api 层用 AppendAudit（audit.go）统一记，
//     因为 actor/role 只有 HTTP 边界知道；
//   - 引用完整性在代码里**先判**（Profile 的 cluster 必须存在、删除 Cluster 前看
//     Profile 引用），数据库约束（REFERENCES / CHECK）只当最后一道防线（03 §6）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// ============================================================================
// Execution Profile 维护（POST/PUT/DELETE /admin/profiles）
// ============================================================================

// AdminProfile 是 execution_profiles 的完整投影（admin 维护路径用）。
//
// 与只读的 Profile（misc_repos.go）相比，这里多出 admin 才关心的列：
// network_policy / created_by / updated_at（03 §3.2 的 DDL 有这些列，
// 但 GET /profiles 的公开投影不暴露它们）。
type AdminProfile struct {
	Name                 string          `json:"name"`
	Cluster              string          `json:"cluster"`
	Namespace            string          `json:"namespace"`
	Mounts               json.RawMessage `json:"mounts"`
	Scheduler            string          `json:"scheduler"`
	Queue                *string         `json:"queue,omitempty"`
	ImagePullSecrets     json.RawMessage `json:"image_pull_secrets"`
	ResourceDefaults     json.RawMessage `json:"resource_defaults"`
	DeployTimeoutSeconds int             `json:"deploy_timeout_seconds"`
	NetworkPolicy        json.RawMessage `json:"network_policy"`
	CreatedBy            string          `json:"created_by"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

const profileAdminCols = `name, cluster, namespace, mounts, scheduler, queue, image_pull_secrets,
	resource_defaults, deploy_timeout_seconds, network_policy, created_by, updated_at`

func scanAdminProfile(row pgx.Row) (*AdminProfile, error) {
	var p AdminProfile
	err := row.Scan(&p.Name, &p.Cluster, &p.Namespace, &p.Mounts, &p.Scheduler, &p.Queue,
		&p.ImagePullSecrets, &p.ResourceDefaults, &p.DeployTimeoutSeconds,
		&p.NetworkPolicy, &p.CreatedBy, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Get 按名字取一个 Profile（admin 维护路径用）。不存在返回 domain.ErrNotFound。
func (r *ProfileRepo) Get(ctx context.Context, tx pgx.Tx, name string) (*AdminProfile, error) {
	p, err := scanAdminProfile(r.s.db(tx).QueryRow(ctx,
		`SELECT `+profileAdminCols+` FROM execution_profiles WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: profile %s: %w", name, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get profile %s: %w", name, err)
	}
	return p, nil
}

// Create 插入一个 Profile。名字冲突返回 domain.ErrConflict（调用方映射 409），
// 引用的 cluster 不存在返回 domain.ErrConflict（调用方应先校验并返回 400）。
func (r *ProfileRepo) Create(ctx context.Context, tx pgx.Tx, p *AdminProfile) error {
	err := r.s.db(tx).QueryRow(ctx, `
		INSERT INTO execution_profiles
		  (name, cluster, namespace, mounts, scheduler, queue, image_pull_secrets,
		   resource_defaults, deploy_timeout_seconds, network_policy, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING network_policy, created_by, updated_at`,
		p.Name, p.Cluster, p.Namespace, jsonOrEmptyArray(p.Mounts), p.Scheduler, p.Queue,
		jsonOrEmptyArray(p.ImagePullSecrets), jsonOrEmptyObject(p.ResourceDefaults),
		p.DeployTimeoutSeconds, jsonOrEmptyObject(p.NetworkPolicy), p.CreatedBy).
		Scan(&p.NetworkPolicy, &p.CreatedBy, &p.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create profile %s: 名字已存在: %w", p.Name, domain.ErrConflict)
		}
		if isForeignKeyViolation(err) {
			return fmt.Errorf("store: create profile %s: 引用的 cluster %q 不存在: %w",
				p.Name, p.Cluster, domain.ErrConflict)
		}
		return fmt.Errorf("store: create profile %s: %w", p.Name, err)
	}
	return nil
}

// Update 按名字整体更新一个 Profile；created_by 不变，updated_at 刷新。
// 不存在返回 domain.ErrNotFound。
func (r *ProfileRepo) Update(ctx context.Context, tx pgx.Tx, p *AdminProfile) error {
	err := r.s.db(tx).QueryRow(ctx, `
		UPDATE execution_profiles
		   SET cluster = $2, namespace = $3, mounts = $4, scheduler = $5, queue = $6,
		       image_pull_secrets = $7, resource_defaults = $8, deploy_timeout_seconds = $9,
		       network_policy = $10, updated_at = now()
		 WHERE name = $1
		RETURNING network_policy, created_by, updated_at`,
		p.Name, p.Cluster, p.Namespace, jsonOrEmptyArray(p.Mounts), p.Scheduler, p.Queue,
		jsonOrEmptyArray(p.ImagePullSecrets), jsonOrEmptyObject(p.ResourceDefaults),
		p.DeployTimeoutSeconds, jsonOrEmptyObject(p.NetworkPolicy)).
		Scan(&p.NetworkPolicy, &p.CreatedBy, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: update profile %s: %w", p.Name, domain.ErrNotFound)
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("store: update profile %s: 引用的 cluster %q 不存在: %w",
				p.Name, p.Cluster, domain.ErrConflict)
		}
		return fmt.Errorf("store: update profile %s: %w", p.Name, err)
	}
	return nil
}

// Delete 按名字删除一个 Profile；不存在返回 domain.ErrNotFound。
// **被 Run/Task 引用也允许删除**（07 §5.4 没禁止）：历史 Task 冻结了 profile 名，
// 引用计数只作为 detail 回报给调用方。
func (r *ProfileRepo) Delete(ctx context.Context, tx pgx.Tx, name string) error {
	tag, err := r.s.db(tx).Exec(ctx, `DELETE FROM execution_profiles WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("store: delete profile %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete profile %s: %w", name, domain.ErrNotFound)
	}
	return nil
}

// CountTaskRefs 数有多少 Task 引用了该 Profile（删除时作为 detail 回报）。
// 只查 tasks（03 §3.1：tasks.profile 是引用列；manifest 里的历史配置不参与计数）。
func (r *ProfileRepo) CountTaskRefs(ctx context.Context, tx pgx.Tx, name string) (int, error) {
	var n int
	if err := r.s.db(tx).QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE profile = $1`, name).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count task refs of profile %s: %w", name, err)
	}
	return n, nil
}

// ============================================================================
// Cluster 登记（POST/PUT/DELETE /admin/clusters）
// ============================================================================

// Create 登记一个集群。名字冲突返回 domain.ErrConflict。
// **只存 kubeconfig 文件名**（clusters.kubeconfig_key），凭据内容永不入库（06 §4.4）。
func (r *ClusterRepo) Create(ctx context.Context, tx pgx.Tx, c *Cluster) error {
	err := r.s.db(tx).QueryRow(ctx, `
		INSERT INTO clusters (name, api_server, kubeconfig_key, description)
		VALUES ($1,$2,$3,$4)
		RETURNING created_at`,
		c.Name, c.APIServer, c.KubeconfigKey, c.Description).Scan(&c.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create cluster %s: 名字已存在: %w", c.Name, domain.ErrConflict)
		}
		return fmt.Errorf("store: create cluster %s: %w", c.Name, err)
	}
	return nil
}

// Update 按名字更新集群（api_server / kubeconfig_key / description）。
// 不存在返回 domain.ErrNotFound。
func (r *ClusterRepo) Update(ctx context.Context, tx pgx.Tx, c *Cluster) error {
	tag, err := r.s.db(tx).Exec(ctx, `
		UPDATE clusters
		   SET api_server = $2, kubeconfig_key = $3, description = $4
		 WHERE name = $1`,
		c.Name, c.APIServer, c.KubeconfigKey, c.Description)
	if err != nil {
		return fmt.Errorf("store: update cluster %s: %w", c.Name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: update cluster %s: %w", c.Name, domain.ErrNotFound)
	}
	return nil
}

// Delete 按名字删除集群。仍有 Profile 引用时数据库会拒绝（23503），
// 调用方应当先查 ProfilesByCluster 并返回 409；这里做兜底映射。
func (r *ClusterRepo) Delete(ctx context.Context, tx pgx.Tx, name string) error {
	tag, err := r.s.db(tx).Exec(ctx, `DELETE FROM clusters WHERE name = $1`, name)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("store: delete cluster %s: 仍有 Profile 引用: %w", name, domain.ErrPrecondition)
		}
		return fmt.Errorf("store: delete cluster %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete cluster %s: %w", name, domain.ErrNotFound)
	}
	return nil
}

// ProfilesByCluster 返回引用该集群的 Profile 名（删除集群前判冲突用）。
func (r *ClusterRepo) ProfilesByCluster(ctx context.Context, tx pgx.Tx, name string) ([]string, error) {
	rows, err := r.s.db(tx).Query(ctx,
		`SELECT name FROM execution_profiles WHERE cluster = $1 ORDER BY name`, name)
	if err != nil {
		return nil, fmt.Errorf("store: profiles of cluster %s: %w", name, err)
	}
	defer rows.Close()
	out := make([]string, 0, 4)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan profile of cluster %s: %w", name, err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ============================================================================
// Secret 引用（GET/POST /admin/secrets、POST /admin/secrets/{name}/rotate）
// ============================================================================

// SecretRef 是 secret_refs 的一行（03 §3.2）。
//
// **只存"从哪个 namespace 的哪个 Secret 的哪个 key 取"**：这里没有 value 列，
// 也不应该有——平台不读、不存、不回显 Secret 值（06 §4.4、C-10）。
type SecretRef struct {
	Name        string    `json:"name"`
	Namespace   string    `json:"namespace"`
	SecretName  string    `json:"secret_name"`
	Key         string    `json:"key"`
	Description *string   `json:"description,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

const secretRefCols = `name, namespace, secret_name, key, description, created_by, created_at`

// ListSecretRefs 按 (created_at DESC, name DESC) 游标分页返回引用。
// 与其它 list 端点同一套游标（07 §6），调用方只回传 next_cursor。
func (s *Store) ListSecretRefs(ctx context.Context, cursor string, limit int) ([]SecretRef, string, error) {
	limit = normalizeLimit(limit)
	args := []any{}
	where := ""
	ts, name, err := DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if !ts.IsZero() {
		args = append(args, ts, name)
		where = " WHERE (created_at, name) < ($1, $2)"
	}
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, `SELECT `+secretRefCols+` FROM secret_refs`+where+
		fmt.Sprintf(` ORDER BY created_at DESC, name DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list secret refs: %w", err)
	}
	defer rows.Close()
	out := make([]SecretRef, 0, limit)
	for rows.Next() {
		var r SecretRef
		if err := rows.Scan(&r.Name, &r.Namespace, &r.SecretName, &r.Key, &r.Description,
			&r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("store: scan secret ref: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: iterate secret refs: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(last.CreatedAt, last.Name)
	}
	return out, next, nil
}

// CreateSecretRef 登记一个 Secret 引用；名字冲突返回 domain.ErrConflict。
// 需要与审计同事务时用 CreateSecretRefTx（PR #33 #6）。
func (s *Store) CreateSecretRef(ctx context.Context, ref *SecretRef) error {
	return s.CreateSecretRefTx(ctx, nil, ref)
}

// CreateSecretRefTx 在调用方事务里登记 Secret 引用。
func (s *Store) CreateSecretRefTx(ctx context.Context, tx pgx.Tx, ref *SecretRef) error {
	err := s.db(tx).QueryRow(ctx, `
		INSERT INTO secret_refs (name, namespace, secret_name, key, description, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING created_at`,
		ref.Name, ref.Namespace, ref.SecretName, ref.Key, ref.Description, ref.CreatedBy).
		Scan(&ref.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create secret ref %s: 名字已存在: %w", ref.Name, domain.ErrConflict)
		}
		return fmt.Errorf("store: create secret ref %s: %w", ref.Name, err)
	}
	return nil
}

// GetSecretRef 按引用名取一行；不存在返回 domain.ErrNotFound。
func (s *Store) GetSecretRef(ctx context.Context, name string) (*SecretRef, error) {
	var r SecretRef
	err := s.pool.QueryRow(ctx, `SELECT `+secretRefCols+` FROM secret_refs WHERE name = $1`, name).
		Scan(&r.Name, &r.Namespace, &r.SecretName, &r.Key, &r.Description, &r.CreatedBy, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: secret ref %s: %w", name, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get secret ref %s: %w", name, err)
	}
	return &r, nil
}

// ============================================================================
// 人工清理产物（DELETE /admin/runs/{id}/artifacts）
// ============================================================================

// DeleteByRun 删除某 Run 的全部产物索引行，返回 (删除条数, 释放字节数)。
// 文件系统上的 run 目录由 api 层删除（store 不做文件 IO）。
func (r *ArtifactRepo) DeleteByRun(ctx context.Context, tx pgx.Tx, runID string) (int64, int64, error) {
	var (
		entries int64
		bytes   int64
	)
	if err := r.s.db(tx).QueryRow(ctx, `
		WITH del AS (DELETE FROM artifacts WHERE run_id = $1 RETURNING size_bytes)
		SELECT count(*), COALESCE(sum(size_bytes), 0) FROM del`, runID).
		Scan(&entries, &bytes); err != nil {
		return 0, 0, fmt.Errorf("store: delete artifacts of run %s: %w", runID, err)
	}
	return entries, bytes, nil
}

// ---- 错误归类 --------------------------------------------------------------

// isUniqueViolation 判断是否是唯一键冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation 判断是否是外键冲突（SQLSTATE 23503）。
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
