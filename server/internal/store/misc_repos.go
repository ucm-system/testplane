package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// ClusterRepo 是 clusters 表（只读投影给 API；admin 维护在 P1）。
type ClusterRepo struct{ s *Store }

// List 返回全部集群概要（不返回凭据内容，07 §5.3）。
func (r *ClusterRepo) List(ctx context.Context, tx pgx.Tx) ([]Cluster, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT name, api_server, kubeconfig_key, description, last_check_at, last_check_result, created_at
		  FROM clusters ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		var c Cluster
		if err := rows.Scan(&c.Name, &c.APIServer, &c.KubeconfigKey, &c.Description,
			&c.LastCheckAt, &c.LastCheckResult, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan cluster: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get 按名字取集群。
func (r *ClusterRepo) Get(ctx context.Context, tx pgx.Tx, name string) (*Cluster, error) {
	var c Cluster
	err := r.s.db(tx).QueryRow(ctx, `
		SELECT name, api_server, kubeconfig_key, description, last_check_at, last_check_result, created_at
		  FROM clusters WHERE name = $1`, name).
		Scan(&c.Name, &c.APIServer, &c.KubeconfigKey, &c.Description,
			&c.LastCheckAt, &c.LastCheckResult, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: cluster %s: %w", name, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get cluster %s: %w", name, err)
	}
	return &c, nil
}

// Upsert 登记/更新集群（seed 或 admin 维护）。
func (r *ClusterRepo) Upsert(ctx context.Context, tx pgx.Tx, c *Cluster) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO clusters (name, api_server, kubeconfig_key, description)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (name) DO UPDATE
		   SET api_server = EXCLUDED.api_server,
		       kubeconfig_key = EXCLUDED.kubeconfig_key,
		       description = EXCLUDED.description`,
		c.Name, c.APIServer, c.KubeconfigKey, c.Description)
	if err != nil {
		return fmt.Errorf("store: upsert cluster %s: %w", c.Name, err)
	}
	return nil
}

// ProfileRepo 是 execution_profiles 表（提交校验与执行层渲染需要）。
type ProfileRepo struct{ s *Store }

// Exists 报告 Profile 是否已登记（C-5.1 第 6 步）。
func (r *ProfileRepo) Exists(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	var ok bool
	err := r.s.db(tx).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM execution_profiles WHERE name = $1)`, name).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: profile exists %s: %w", name, err)
	}
	return ok, nil
}

// Profile 是 execution_profiles 的一行投影。
type Profile struct {
	Name                 string          `json:"name"`
	Cluster              string          `json:"cluster"`
	Namespace            string          `json:"namespace"`
	Mounts               json.RawMessage `json:"mounts"`
	Scheduler            string          `json:"scheduler"`
	Queue                *string         `json:"queue,omitempty"`
	ImagePullSecrets     json.RawMessage `json:"image_pull_secrets"`
	ResourceDefaults     json.RawMessage `json:"resource_defaults"`
	DeployTimeoutSeconds int             `json:"deploy_timeout_seconds"`
}

// List 返回全部 Profile（07 §5.3）。
func (r *ProfileRepo) List(ctx context.Context, tx pgx.Tx) ([]Profile, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT name, cluster, namespace, mounts, scheduler, queue, image_pull_secrets,
		       resource_defaults, deploy_timeout_seconds
		  FROM execution_profiles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list profiles: %w", err)
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.Name, &p.Cluster, &p.Namespace, &p.Mounts, &p.Scheduler,
			&p.Queue, &p.ImagePullSecrets, &p.ResourceDefaults, &p.DeployTimeoutSeconds); err != nil {
			return nil, fmt.Errorf("store: scan profile: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AuditRepo 写 audit_log（所有写操作在事务内记审计，C-10）。
type AuditRepo struct{ s *Store }

// Insert 追加一条审计；detail 序列化成 jsonb。
func (r *AuditRepo) Insert(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	detail := []byte("{}")
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("store: audit marshal detail: %w", err)
		}
		detail = b
	}
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO audit_log (actor, role, action, object_type, object_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		e.Actor, e.Role, e.Action, e.ObjectType, nullableText(e.ObjectID), detail)
	if err != nil {
		return fmt.Errorf("store: audit insert: %w", err)
	}
	return nil
}
