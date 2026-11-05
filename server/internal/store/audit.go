package store

// audit_log 的读模型与统一写入口（03 §3.2、07 §5.4、C-10）。
//
// 写：所有写操作都由 api 层调 Store.AppendAudit —— 单个入口，handler 里不拼 SQL。
// 读：GET /admin/audit 的过滤（actor / action / object_type / object_id / 时间范围）
// 与游标分页（ORDER BY at DESC, id DESC，游标不透明，07 §6）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditRecord 是 audit_log 的一行（只读投影）。
type AuditRecord struct {
	ID         int64           `json:"id"`
	Actor      string          `json:"actor"`
	Role       string          `json:"role"`
	Action     string          `json:"action"`
	ObjectType string          `json:"object_type"`
	ObjectID   *string         `json:"object_id"`
	Detail     json.RawMessage `json:"detail"`
	At         time.Time       `json:"at"`
}

// AuditFilter 是 GET /admin/audit 的查询条件（空字段不过滤）。
type AuditFilter struct {
	Actor      string
	Action     string
	ObjectType string
	ObjectID   string
	// Since / Until 都是闭区间；零值表示不限。
	Since  time.Time
	Until  time.Time
	Cursor string
	Limit  int
}

const auditCols = `id, actor, role, action, object_type, object_id, detail, at`

// List 按 at DESC, id DESC 返回一页审计；next 为空表示没有下一页。
func (r *AuditRepo) List(ctx context.Context, tx pgx.Tx, f AuditFilter) ([]AuditRecord, string, error) {
	limit := normalizeLimit(f.Limit)
	args := []any{}
	where := []string{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Actor != "" {
		add("actor = $%d", f.Actor)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.ObjectType != "" {
		add("object_type = $%d", f.ObjectType)
	}
	if f.ObjectID != "" {
		add("object_id = $%d", f.ObjectID)
	}
	if !f.Since.IsZero() {
		add("at >= $%d", f.Since.UTC())
	}
	if !f.Until.IsZero() {
		add("at <= $%d", f.Until.UTC())
	}
	ts, idText, err := DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	if !ts.IsZero() {
		id, parseErr := strconv.ParseInt(idText, 10, 64)
		if parseErr != nil {
			return nil, "", fmt.Errorf("%w: 审计游标 id 非法", ErrBadCursor)
		}
		args = append(args, ts, id)
		where = append(where, fmt.Sprintf("(at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	q := `SELECT ` + auditCols + ` FROM audit_log`
	if len(where) > 0 {
		q += " WHERE "
		for i, w := range where {
			if i > 0 {
				q += " AND "
			}
			q += w
		}
	}
	q += fmt.Sprintf(" ORDER BY at DESC, id DESC LIMIT $%d", len(args))

	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	out := make([]AuditRecord, 0, limit)
	for rows.Next() {
		var rec AuditRecord
		if err := rows.Scan(&rec.ID, &rec.Actor, &rec.Role, &rec.Action, &rec.ObjectType,
			&rec.ObjectID, &rec.Detail, &rec.At); err != nil {
			return nil, "", fmt.Errorf("store: scan audit: %w", err)
		}
		if rec.Detail == nil {
			rec.Detail = json.RawMessage(`{}`)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: iterate audit: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(last.At, strconv.FormatInt(last.ID, 10))
	}
	return out, next, nil
}

// AppendAudit 是写审计的唯一入口（C-10：所有写操作都要留痕）。
//
// 注意：这里**不吞错**——调用方决定"审计失败是否阻断主流程"。
// 管理面写操作的做法是记日志后继续，避免因为审计表问题把已经生效的变更报成失败。
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	return s.Audits.Insert(ctx, nil, e)
}

// AppendAuditTx 在调用方的事务里写审计（需要"业务变更 + 审计"原子落库时用）。
func (s *Store) AppendAuditTx(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	return s.Audits.Insert(ctx, tx, e)
}
