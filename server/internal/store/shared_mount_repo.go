package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// SharedMountRepo 是 shared_mounts 表（D-65：运行时可管理的平台级统一共享挂载）。
//
// 语义要点：
//   - 本表是**运行时真源**；TESTPLANE_SHARED_MOUNTS 只在表为空时做一次性种子；
//   - 渲染读 `enabled=true` 的行；管理面写完整行；
//   - 平台只存 PVC 名（claim_name），**不创建** PV/PVC（§10.4 边界）。
type SharedMountRepo struct{ s *Store }

// SharedMount 是 shared_mounts 的一行。
type SharedMount struct {
	Name      string    `json:"name"`
	MountPath string    `json:"mount_path"`
	ReadOnly  bool      `json:"read_only"`
	ClaimName string    `json:"claim_name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedBy string    `json:"updated_by"`
}

const sharedMountCols = `name, mount_path, read_only, claim_name, enabled, created_at, updated_at, created_by, updated_by`

func scanSharedMount(row interface{ Scan(...any) error }) (*SharedMount, error) {
	var m SharedMount
	if err := row.Scan(&m.Name, &m.MountPath, &m.ReadOnly, &m.ClaimName, &m.Enabled,
		&m.CreatedAt, &m.UpdatedAt, &m.CreatedBy, &m.UpdatedBy); err != nil {
		return nil, err
	}
	m.CreatedAt = m.CreatedAt.UTC()
	m.UpdatedAt = m.UpdatedAt.UTC()
	return &m, nil
}

// List 返回全部（onlyEnabled=true 时只返回启用中的，渲染路径用这个）。
func (r *SharedMountRepo) List(ctx context.Context, tx pgx.Tx, onlyEnabled bool) ([]SharedMount, error) {
	q := `SELECT ` + sharedMountCols + ` FROM shared_mounts`
	if onlyEnabled {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY name`
	rows, err := r.s.db(tx).Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list shared mounts: %w", err)
	}
	defer rows.Close()
	var out []SharedMount
	for rows.Next() {
		m, err := scanSharedMount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan shared mount: %w", err)
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// Get 按 name 读一行；不存在返回包装 domain.ErrNotFound 的错误。
func (r *SharedMountRepo) Get(ctx context.Context, tx pgx.Tx, name string) (*SharedMount, error) {
	row := r.s.db(tx).QueryRow(ctx, `SELECT `+sharedMountCols+` FROM shared_mounts WHERE name = $1`, name)
	m, err := scanSharedMount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: shared mount %s: %w", name, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get shared mount %s: %w", name, err)
	}
	return m, nil
}

// Upsert 插入或整体更新一行（管理面 POST/PUT 共用）。
//
// 冲突语义：name 主键冲突由调用方先 Get 判定（POST 冲突 409）；
// mount_path 的唯一约束在这里翻译成 domain.ErrConflict。
func (r *SharedMountRepo) Upsert(ctx context.Context, tx pgx.Tx, m *SharedMount) error {
	now := r.s.now()
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO shared_mounts (name, mount_path, read_only, claim_name, enabled,
		                           created_at, updated_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $7)
		ON CONFLICT (name) DO UPDATE
		   SET mount_path = EXCLUDED.mount_path,
		       read_only  = EXCLUDED.read_only,
		       claim_name = EXCLUDED.claim_name,
		       enabled    = EXCLUDED.enabled,
		       updated_at = EXCLUDED.updated_at,
		       updated_by = EXCLUDED.updated_by`,
		m.Name, m.MountPath, m.ReadOnly, m.ClaimName, m.Enabled, now, m.UpdatedBy)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: shared mount path %s: %w", m.MountPath, domain.ErrConflict)
		}
		return fmt.Errorf("store: upsert shared mount %s: %w", m.Name, err)
	}
	m.UpdatedAt = now
	return nil
}

// Delete 删除一行；不存在返回 domain.ErrNotFound。
// 已提交的 Run 不受影响（Task 快照已冻结），只影响之后的渲染。
func (r *SharedMountRepo) Delete(ctx context.Context, tx pgx.Tx, name string) error {
	tag, err := r.s.db(tx).Exec(ctx, `DELETE FROM shared_mounts WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("store: delete shared mount %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: shared mount %s: %w", name, domain.ErrNotFound)
	}
	return nil
}

// SeedIfEmpty 在**表为空**时导入 env 种子（D-65：只导入一次，之后不再覆盖）。
//
// 返回导入条数；表里已有行时返回 0（哪怕种子内容变了也不碰，管理面才是真源）。
func (r *SharedMountRepo) SeedIfEmpty(ctx context.Context, seeds []domain.SharedMountSpec) (int, error) {
	if len(seeds) == 0 {
		return 0, nil
	}
	var existing int
	if err := r.s.db(nil).QueryRow(ctx, `SELECT count(*) FROM shared_mounts`).Scan(&existing); err != nil {
		return 0, fmt.Errorf("store: count shared mounts: %w", err)
	}
	if existing > 0 {
		return 0, nil
	}
	imported := 0
	err := r.s.InTx(ctx, func(tx pgx.Tx) error {
		// 事务内再确认一次，避免并发启动时重复导入（第二个会撞主键，忽略即可）。
		var n int
		if err := r.s.db(tx).QueryRow(ctx, `SELECT count(*) FROM shared_mounts`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		for _, seed := range seeds {
			m := &SharedMount{
				Name: seed.Name, MountPath: seed.MountPath, ReadOnly: seed.ReadOnly,
				ClaimName: seed.ClaimName, Enabled: true,
				CreatedBy: "system:seed", UpdatedBy: "system:seed",
			}
			if err := r.Upsert(ctx, tx, m); err != nil {
				return err
			}
			imported++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return imported, nil
}

// EffectiveSpecs 返回渲染用的 domain 形态（只取启用中的行）。
func (r *SharedMountRepo) EffectiveSpecs(ctx context.Context, tx pgx.Tx) ([]domain.SharedMountSpec, error) {
	rows, err := r.List(ctx, tx, true)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SharedMountSpec, 0, len(rows))
	for i := range rows {
		out = append(out, domain.SharedMountSpec{
			Name:      rows[i].Name,
			MountPath: rows[i].MountPath,
			ReadOnly:  rows[i].ReadOnly,
			ClaimName: rows[i].ClaimName,
		})
	}
	return out, nil
}
