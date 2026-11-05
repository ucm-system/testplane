package api

// 审计查询（07 §5.4：GET /admin/audit）与人工清理产物
// （07 §5.4：DELETE /admin/runs/{run_id}/artifacts）。
//
// 审计检索：按 actor / action / object_type / object_id / 时间范围过滤，
// `ORDER BY at DESC, id DESC`，游标与其它 list 端点同构（07 §6）。
// 清理产物：**Run 非终态一律 409**（不让人工清理正在写入的目录），
// 终态则删 <storage-root>/<run_id> 与 artifacts 索引行，并记审计。

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// auditDTO 是 GET /admin/audit 的列表项（openapi AuditLog）。
type auditDTO struct {
	ID         int64           `json:"id"`
	Actor      string          `json:"actor"`
	Role       string          `json:"role"`
	Action     string          `json:"action"`
	ObjectType string          `json:"object_type"`
	ObjectID   *string         `json:"object_id"`
	Detail     json.RawMessage `json:"detail"`
	At         time.Time       `json:"at"`
}

func toAuditDTO(rec store.AuditRecord) auditDTO {
	detail := rec.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	return auditDTO{
		ID: rec.ID, Actor: rec.Actor, Role: rec.Role, Action: rec.Action,
		ObjectType: rec.ObjectType, ObjectID: rec.ObjectID, Detail: detail,
		At: rec.At.UTC(),
	}
}

// handleAdminListAudit 是 GET /api/v1/admin/audit。
func (s *Server) handleAdminListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := parseAuditTime(q.Get("from"))
	if err != nil {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"from 必须是 RFC3339 时间", map[string]any{"param": "from"}, false)
		return
	}
	until, err := parseAuditTime(q.Get("to"))
	if err != nil {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"to 必须是 RFC3339 时间", map[string]any{"param": "to"}, false)
		return
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"to 不能早于 from", map[string]any{"from": q.Get("from"), "to": q.Get("to")}, false)
		return
	}
	items, next, err := s.st.Audits.List(r.Context(), nil, store.AuditFilter{
		Actor:      strings.TrimSpace(q.Get("actor")),
		Action:     strings.TrimSpace(q.Get("action")),
		ObjectType: strings.TrimSpace(q.Get("object_type")),
		ObjectID:   strings.TrimSpace(q.Get("object_id")),
		Since:      since,
		Until:      until,
		Cursor:     q.Get("cursor"),
		Limit:      atoiDefault(q.Get("limit"), 50),
	})
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]auditDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toAuditDTO(item))
	}
	writeJSON(w, http.StatusOK, paged[auditDTO]{
		Items: out, NextCursor: cursorPtr(next), HasMore: next != "",
	})
}

// parseAuditTime 解析审计时间过滤参数；空串返回零值（不过滤）。
func parseAuditTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

// removeAllForCleanup 是产物目录删除函数；包级变量只为测试注入 fs 失败（issue #40-7），
// 生产行为与 os.RemoveAll 完全一致。
var removeAllForCleanup = os.RemoveAll

// handleAdminCleanupArtifacts 是 DELETE /api/v1/admin/runs/{run_id}/artifacts（200）。
//
// 这是保留期清理的例外通道（web-and-delivery §11）：只允许对**终态 Run** 执行，
// 非终态 409 CONFLICT（避免删掉正在写入的目录）。
//
// 顺序（issue #40-7）：先在同一个事务里删 artifacts 索引行 + 写 result=ok 审计，
// 提交后再删磁盘目录；目录删除失败时**补写一条 result=rejected 审计**
// （带 detail.directory 与 detail.error），再返回 5xx——不会再出现"目录没删掉、
// 审计却只有成功"的情况。索引是可重建的派生数据（03 §3.2），先删它不会丢事实。
func (s *Server) handleAdminCleanupArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "cleanup_artifacts", "run"

	runID := strings.TrimSpace(chi.URLParam(r, "run_id"))
	if runID == "" {
		s.rejectValidation(w, r, action, objectType, "",
			"缺少 run_id", map[string]any{"param": "run_id"})
		return
	}
	run, err := s.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, runID,
				http.StatusNotFound, domain.CodeNotFound,
				"Run 不存在", map[string]any{"run_id": runID}, false)
			return
		}
		s.failWriteErr(w, r, action, objectType, runID, err)
		return
	}
	status := domain.RunStatus(run.Status)
	if !status.IsTerminal() {
		s.rejectWrite(w, r, action, objectType, runID,
			http.StatusConflict, domain.CodeConflict,
			"Run 未到终态，不能人工清理产物",
			map[string]any{"run_id": runID, "status": run.Status}, false)
		return
	}

	// 目录名取自库里读出的 run.ID（不是 URL 原文），再做一次路径片段校验。
	component, err := safePathComponent(run.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	// 目录路径先算好（同一份写进 ok 审计与失败审计的 detail.directory）。
	dir := ""
	if root := strings.TrimSpace(s.cfg.StorageRoot); root != "" {
		dir = filepath.Join(root, component)
	}

	// 先做 DB 事务（删索引行 + result=ok 审计），提交后再删目录：
	// 审计失败时索引行回滚、目录也还没动——不会出现"清了磁盘却查不到记录"；
	// artifacts 索引是可重建的派生数据（03 §3.2），先删它不会丢事实。
	// 目录删除失败时补一条 rejected 审计（issue #40-7），不让 ok 成为唯一记录。
	var (
		entries    int64
		freedBytes int64
	)
	detail := map[string]any{
		"deleted_entries": int64(0), "freed_bytes": int64(0), "directory": dir,
	}
	entry := s.auditEntry(r, action, objectType, runID, detail)
	err = s.st.InAuditedTx(ctx, entry, func(tx pgx.Tx) error {
		n, b, err := s.st.Artifacts.DeleteByRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		entries, freedBytes = n, b
		detail["deleted_entries"] = n
		detail["freed_bytes"] = b
		entry.Detail["deleted_entries"] = n
		entry.Detail["freed_bytes"] = b
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrTx) {
			s.failAuditTx(w, r, action, objectType, runID, err)
			return
		}
		s.failWriteErr(w, r, action, objectType, runID, err)
		return
	}

	directoryRemoved := false
	if dir != "" {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			if err := removeAllForCleanup(dir); err != nil {
				// 索引已删（可重建），目录没删掉：补一条 rejected 结果审计（#40-7），
				// 再按 5xx 暴露，交给运维处理。
				s.logger.Error("删除 Run 产物目录失败", "err", err, "run_id", run.ID,
					"request_id", RequestID(ctx))
				s.auditRejected(ctx, Actor(ctx), Role(ctx), action, objectType, runID, map[string]any{
					"result": "rejected", "reason": "directory_remove_failed",
					"code": domain.CodeCleanupFailed, "directory": dir,
					"error": err.Error(), "deleted_entries": entries, "freed_bytes": freedBytes,
				})

				fail(w, r, http.StatusInternalServerError, domain.CodeCleanupFailed,
					"产物索引已清理，但删除产物目录失败",
					map[string]any{"run_id": runID, "deleted_entries": entries, "directory": dir}, true)
				return
			}
			directoryRemoved = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":            runID,
		"deleted_entries":   entries,
		"freed_bytes":       freedBytes,
		"directory_removed": directoryRemoved,
	})
}
