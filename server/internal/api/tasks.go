package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// maxLogChunk 是单次日志返回上限（07 §5.2：≤64 KiB）。
const maxLogChunk = 64 << 10

// handleListTasks 是 GET /api/v1/tasks：跨 Run 列表（游标分页）。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TaskFilter{
		Statuses: splitCSV(q.Get("status")),
		Tester:   q.Get("tester"),
		RunID:    q.Get("run_id"),
		Cursor:   q.Get("cursor"),
		Limit:    atoiDefault(q.Get("limit"), 50),
	}
	items, next, err := s.st.Tasks.List(r.Context(), nil, f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]taskDTO, 0, len(items))
	for i := range items {
		out = append(out, toTaskDTO(&items[i]))
	}
	writeJSON(w, http.StatusOK, paged[taskDTO]{Items: out, NextCursor: cursorPtr(next), HasMore: next != ""})
}

// handleGetTask 是 GET /api/v1/tasks/{id}（openapi TaskDetail）：
// Task 字段平铺 + attempts 数组。
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	task, err := s.st.Tasks.Get(ctx, nil, chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	attempts, err := s.st.Attempts.ListByTask(ctx, nil, task.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, taskDetailDTO{taskDTO: toTaskDTO(task), Attempts: attemptDTOs(attempts)})
}

// handleCancelTask 是 POST /api/v1/tasks/{id}/cancel。
//
// 未开始的 Task 取消记 skipped(reason=canceled)（C-3.1：不存在 pending→canceled）；
// 进行中的走 T7/T10 → canceled。
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID := chi.URLParam(r, "id")
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	status := domain.TaskStatus(task.Status)
	if status == domain.TaskCanceled || status == domain.TaskSkipped {
		writeJSON(w, http.StatusOK, map[string]any{"task_id": taskID, "status": task.Status})
		return
	}
	if status.IsTerminal() {
		writeFailure(w, r, domain.NewFailure(domain.CodeCanceled, "Task 已结束，无法取消",
			map[string]any{"task_id": taskID, "status": task.Status}))
		return
	}

	switch status {
	case domain.TaskPending, domain.TaskQueued:
		_, err = s.machines.Tasks.Skip(ctx, taskID, domain.SkipCanceled)
	case domain.TaskPreparing, domain.TaskRunning:
		_, err = s.machines.Tasks.Cancel(ctx, taskID, Actor(ctx))
	default:
		err = fmt.Errorf("store: cancel task %s: 状态 %s 不支持取消: %w", taskID, status, domain.ErrPrecondition)
	}
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			if cur, getErr := s.st.Tasks.Get(ctx, nil, taskID); getErr == nil {
				writeJSON(w, http.StatusOK, map[string]any{"task_id": taskID, "status": cur.Status})
				return
			}
		}
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID, "status": string(domain.TaskCanceled),
	})
}

// handleListAttempts 是 GET /api/v1/tasks/{id}/attempts（openapi AttemptList）。
func (s *Server) handleListAttempts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID := chi.URLParam(r, "id")
	if _, err := s.st.Tasks.Get(ctx, nil, taskID); err != nil {
		s.failErr(w, r, err)
		return
	}
	attempts, err := s.st.Attempts.ListByTask(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paged[attemptDTO]{Items: attemptDTOs(attempts)})
}

// handleAttemptResult 是 GET /api/v1/tasks/{id}/attempts/{idx}/result：返回 result.json 原文。
func (s *Server) handleAttemptResult(w http.ResponseWriter, r *http.Request) {
	path, taskID, idx, ok := s.attemptFilePath(w, r, "result.json")
	if !ok {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Retry-After", "3")
			fail(w, r, http.StatusNotFound, domain.CodeNotFound, "result.json 尚未产出",
				map[string]any{"task_id": taskID, "attempt_idx": idx}, true)
			return
		}
		s.failErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleAttemptLogs 是 GET /api/v1/tasks/{id}/attempts/{idx}/logs：
// ?stream=stdout|stderr&offset=<字节>&limit=<字节，≤64KiB>；带 X-Log-Offset 响应头。
func (s *Server) handleAttemptLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stream := q.Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	if stream != "stdout" && stream != "stderr" {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"stream 只能是 stdout|stderr", nil, false)
		return
	}
	path, taskID, idx, ok := s.attemptFilePath(w, r, filepath.Join("logs", stream+".log"))
	if !ok {
		return
	}

	offset := int64(atoiDefault(q.Get("offset"), 0))
	if offset < 0 {
		offset = 0
	}
	limit := atoiDefault(q.Get("limit"), maxLogChunk)
	if limit <= 0 || limit > maxLogChunk {
		limit = maxLogChunk
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fail(w, r, http.StatusNotFound, domain.CodeNotFound, "日志尚未产出",
				map[string]any{"task_id": taskID, "attempt_idx": idx, "stream": stream}, true)
			return
		}
		s.failErr(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		s.failErr(w, r, err)
		return
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		s.failErr(w, r, err)
		return
	}
	buf = buf[:n]

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Log-Offset", strconv.FormatInt(offset+int64(n), 10))
	// 日志区间需要 collector 提供；MVP 拿不到就留空（07 §5.2）。
	w.Header().Set("X-Log-Coverage", "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// attemptFilePath 校验 task 存在、idx 合法并返回产物路径。
func (s *Server) attemptFilePath(w http.ResponseWriter, r *http.Request, rel string) (string, string, int, bool) {
	ctx := r.Context()
	taskID, err := safePathComponent(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return "", "", 0, false
	}
	idxRaw := chi.URLParam(r, "idx")
	idx, err := strconv.Atoi(idxRaw)
	if err != nil || idx < 1 {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "attempt idx 必须是 ≥1 的整数", nil, false)
		return "", "", 0, false
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return "", "", 0, false
	}
	path := filepath.Join(s.cfg.StorageRoot, task.RunID, "tasks", task.ID,
		"attempts", strconv.Itoa(idx), rel)
	return path, taskID, idx, true
}

func attemptDTOs(in []store.Attempt) []attemptDTO {
	out := make([]attemptDTO, 0, len(in))
	for i := range in {
		out = append(out, toAttemptDTO(&in[i]))
	}
	return out
}
