package api

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

// 审计查询（07 §5.4：GET /admin/audit）与人工清理产物
// （07 §5.4：DELETE /admin/tasks/{task_id}/artifacts）。
//
// 审计检索：按 actor / action / object_type / object_id / 时间范围过滤，
// `ORDER BY at DESC, id DESC`，游标与其它 list 端点同构（07 §6）。
// 清理产物：**Task 非终态一律 409**（不让人工清理正在写入的目录），
// 终态则删 <storage-root>/<task_id> 与 artifacts 索引行，并记审计。

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

// handleAdminCleanupArtifacts 是 DELETE /api/v1/admin/tasks/{task_id}/artifacts（200）。
//
// 这是保留期清理的例外通道（web-and-delivery §11）：只允许对**终态 Task** 执行，
// 非终态 409 CONFLICT（避免删掉正在写入的目录）。Task 本身不删除。
//
// 顺序（issue #40-7）：先在同一个事务里删 artifacts 索引行 + 写 result=ok 审计，
// 提交后再删磁盘目录；目录删除失败时**补写一条 result=rejected 审计**，
// 再返回 5xx——不会再出现"目录没删掉、审计却只有成功"的情况。
func (s *Server) handleAdminCleanupArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "cleanup_artifacts", "task"

	taskID := strings.TrimSpace(chi.URLParam(r, "task_id"))
	if taskID == "" {
		s.rejectValidation(w, r, action, objectType, "",
			"缺少 task_id", map[string]any{"param": "task_id"})
		return
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, taskID,
				http.StatusNotFound, domain.CodeNotFound,
				"Task 不存在", map[string]any{"task_id": taskID}, false)
			return
		}
		s.failWriteErr(w, r, action, objectType, taskID, err)
		return
	}
	status := domain.TaskStatus(task.Status)
	if !status.IsTerminal() {
		s.rejectWrite(w, r, action, objectType, taskID,
			http.StatusConflict, domain.CodeConflict,
			"Task 未到终态，不能人工清理产物",
			map[string]any{"task_id": taskID, "status": task.Status}, false)
		return
	}

	// 目录名取自库里读出的 task.ID（不是 URL 原文），再做一次路径片段校验。
	component, ok := safePathComponent(task.ID)
	if !ok {
		s.failErr(w, r, err)
		return
	}

	// 目录路径先算好（同一份写进 ok 审计与失败审计的 detail.directory）。
	dir := ""
	if root := strings.TrimSpace(s.cfg.StorageRoot); root != "" {
		dir = filepath.Join(root, component)
	}

	var (
		entries    int64
		freedBytes int64
	)
	// 行数与字节数在删除前统计（索引是派生数据，删后查不到）。
	if rows, qerr := s.st.Artifacts.ListByTask(ctx, nil, taskID); qerr == nil {
		for _, a := range rows {
			entries++
			freedBytes += a.SizeBytes
		}
	}
	detail := map[string]any{
		"deleted_entries": entries, "freed_bytes": freedBytes, "directory": dir,
	}
	entry := s.auditEntry(r, action, objectType, taskID, detail)
	err = s.st.InAuditedTx(ctx, entry, func(tx pgx.Tx) error {
		return s.st.Artifacts.DeleteByTask(ctx, tx, taskID)
	})
	if err != nil {
		if errors.Is(err, domain.ErrTx) {
			s.failAuditTx(w, r, action, objectType, taskID, err)
			return
		}
		s.failWriteErr(w, r, action, objectType, taskID, err)
		return
	}

	directoryRemoved := false
	if dir != "" {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			if err := removeAllForCleanup(dir); err != nil {
				// 索引已删（可重建），目录没删掉：补一条 rejected 结果审计（#40-7），
				// 再按 5xx 暴露，交给运维处理。
				s.logger.Error("删除 Task 产物目录失败", "err", err, "task_id", task.ID,
					"request_id", RequestID(ctx))
				s.auditRejected(ctx, Actor(ctx), Role(ctx), action, objectType, taskID, map[string]any{
					"result": "rejected", "reason": "directory_remove_failed",
					"code": domain.CodeCleanupFailed, "directory": dir,
					"error": err.Error(), "deleted_entries": entries, "freed_bytes": freedBytes,
				})

				fail(w, r, http.StatusInternalServerError, domain.CodeCleanupFailed,
					"产物索引已清理，但删除产物目录失败",
					map[string]any{"task_id": taskID, "deleted_entries": entries, "directory": dir}, true)
				return
			}
			directoryRemoved = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":           taskID,
		"deleted_entries":   entries,
		"freed_bytes":       freedBytes,
		"directory_removed": directoryRemoved,
	})
}
