package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Task 端点族（openapi /api/v1/tasks*）：列表 / facets / 详情 / 取消 / 重试 /
// clean 重放 / 事件历史 / 报告 / Manifest / runner 执行记录投影。

// parseTaskFilter 解析列表与 facets 共用的过滤参数（同一语义，D-61）。
func parseTaskFilter(r *http.Request) (store.TaskFilter, int, int, bool) {
	q := r.URL.Query()
	f := store.TaskFilter{
		Statuses:   splitCSV(q.Get("status")),
		Runners:    splitCSV(q.Get("runner")),
		Submitters: splitCSV(q.Get("submitter")),
		Cursor:     q.Get("cursor"),
		Limit:      atoiDefault(q.Get("limit"), 50),
	}
	page := atoiDefault(q.Get("page"), 0)
	pageSize := atoiDefault(q.Get("page_size"), 0)
	if page < 0 || pageSize < 0 {
		return f, 0, 0, false
	}
	// 默认 20、上限 100（07 §5.1）；>100 收敛到 100，避免一页拉爆。
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return f, page, pageSize, true
}

// fillTaskDTOs 把 store 行投影成 DTO 并补 runner_counts（一次批量查询，不 N+1）。
func (s *Server) fillTaskDTOs(ctx context.Context, items []store.Task, out []taskDTO) []taskDTO {
	if len(items) == 0 {
		return out
	}
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	counts, err := s.st.Tasks.RunnerCountsByTask(ctx, nil, ids)
	if err != nil {
		s.logger.Warn("tasks: runner counts 查询失败（本次不展示计数）", "err", err)
	}
	for i := range items {
		dto := toTaskDTO(&items[i])
		if c, ok := counts[dto.ID]; ok {
			dto.RunnerCounts = toRunnerCounts(c)
		}
		out = append(out, dto)
	}
	return out
}

func toRunnerCounts(c store.RunnerCounts) *runnerCounts {
	return &runnerCounts{
		Total: c.Total, Pending: c.Pending, Running: c.Running,
		Succeeded: c.Succeeded, Failed: c.Failed, Skipped: c.Skipped,
	}
}

// handleListTasks 是 GET /api/v1/tasks（游标分页，按提交时间倒序；D-61 双分页）。
//
// offset 模式（page/page_size）用"逐页游标推进"实现：第 N 页的起点游标 =
// 前一页末行的 (submitted_at, id)。total 通过 ListPage 的计数取得。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	f, page, pageSize, ok := parseTaskFilter(r)
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"page/page_size 不能为负", nil, false)
		return
	}

	out := make([]taskDTO, 0, pageSize)
	var (
		total *int
		next  = ""
		more  bool
	)
	if page > 0 {
		items, n, nextCursor, err := s.listTasksPage(r.Context(), f, page, pageSize)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		total = &n
		next, more = nextCursor, nextCursor != ""
		out = s.fillTaskDTOs(r.Context(), items, out)
	} else {
		items, cursor, err := s.st.Tasks.List(r.Context(), nil, f)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		next, more = cursor, cursor != ""
		out = s.fillTaskDTOs(r.Context(), items, out)
	}

	resp := taskListDTO{Items: out, HasMore: more}
	if page > 0 {
		p, ps := page, pageSize
		resp.Total, resp.Page, resp.PageSize = total, &p, &ps
	}
	if next != "" {
		resp.NextCursor = cursorPtr(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

// listTasksPage 用游标推进实现 offset 分页（page 从 1 起）。返回 next 游标
// （还有下一页时非空）。total 来自 store 的 Count（与 List 同口径的 SELECT count(*)，D-61）。
func (s *Server) listTasksPage(ctx context.Context, f store.TaskFilter, page, pageSize int) ([]store.Task, int, string, error) {
	total, err := s.st.Tasks.Count(ctx, nil, f)
	if err != nil {
		return nil, 0, "", err
	}
	if total == 0 {
		return nil, 0, "", nil
	}
	// 逐页推进游标：page-1 次翻页拿第 page 页的起点。
	ff := f
	ff.Limit = pageSize
	skip := (page - 1) * pageSize
	var items []store.Task
	cursor := ff.Cursor
	for left := skip; ; {
		ff.Cursor = cursor
		batch, next, err := s.st.Tasks.List(ctx, nil, ff)
		if err != nil {
			return nil, 0, "", err
		}
		if len(batch) == 0 {
			break
		}
		if len(batch) <= left {
			left -= len(batch)
			if next == "" {
				batch = nil
				break
			}
			cursor = next
			continue
		}
		items = batch[left:]
		break
	}
	nextCursor := ""
	if len(items) > 0 && skip+pageSize < total {
		last := items[len(items)-1]
		nextCursor = store.EncodeCursor(last.SubmittedAt, last.ID)
	}
	if len(items) > pageSize {
		items = items[:pageSize]
	}
	return items, total, nextCursor, nil
}

// handleTaskFacets 是 GET /api/v1/tasks/facets（D-61）：status/submitter/runner 三维
// 取值与精确计数（计数按维度独立：其它维度过滤生效、自己不生效）。
//
// 实现走 store 侧 GROUP BY 出口（store.Tasks.Facets）：与列表 WHERE 共用同一份
// taskFilterConds，口径互洽；单维度桶数上限 200（facetValueCap）。
func (s *Server) handleTaskFacets(w http.ResponseWriter, r *http.Request) {
	f, _, _, ok := parseTaskFilter(r)
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"page/page_size 不能为负", nil, false)
		return
	}
	facets, err := s.st.Tasks.Facets(r.Context(), nil, f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    facetBuckets(facets.Status),
		"submitter": facetBuckets(facets.Submitter),
		"runner":    facetBuckets(facets.Runner),
	})
}

// facetBuckets 把 store 行投影成 openapi FacetBucket。
func facetBuckets(in []store.FacetBucket) []facetBucket {
	out := make([]facetBucket, 0, len(in))
	for _, b := range in {
		out = append(out, facetBucket{Value: b.Value, Count: b.Count})
	}
	return out
}

// facetBucket 是 openapi FacetBucket。
type facetBucket struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// resolveAttemptQuery 解析 ?attempt=（缺省取最新 attempt，07 §5.2）。
func (s *Server) resolveAttemptQuery(ctx context.Context, taskID, raw string) (int, *store.Task, error) {
	t, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		return 0, nil, err
	}
	if raw == "" {
		no := t.CurrentAttempt
		if no < 1 {
			no = 1
		}
		return no, t, nil
	}
	no, err := strconv.Atoi(raw)
	if err != nil || no < 1 {
		return 0, nil, domain.NewFailureError(domain.CodeValidationError,
			"attempt 必须是 ≥1 的整数", map[string]any{"attempt": raw})
	}
	return no, t, nil
}

// handleGetTask 是 GET /api/v1/tasks/{task_id}（openapi TaskDetail）。
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	attempts, err := s.st.Attempts.ListByTask(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, taskID, task.CurrentAttempt)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dto := taskDetailDTO{
		taskDTO:  toTaskDTO(task),
		Attempts: attemptDTOs(attempts),
		Runners:  runnerExecDTOs(runs),
	}
	if c, err := s.st.Tasks.RunnerCountsByTask(ctx, nil, []string{taskID}); err == nil {
		if rc, ok := c[taskID]; ok {
			dto.RunnerCounts = toRunnerCounts(rc)
		}
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleCancelTask 是 POST /api/v1/tasks/{task_id}/cancel（C-6.1）：
// store.CancelTask 落 cancelling + 批量 SKIPPED（传播到 clean 为止）+ cancel_requested；
// 编排器在同一进程的下一拍删 Job → 推进 clean → clean 完成落 canceled。
// 已在 cancelling/canceled 的重复取消按 200 幂等返回（07 §8）。
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	status := domain.TaskStatus(task.Status)
	switch status {
	case domain.TaskCancelling, domain.TaskCanceled:
		writeJSON(w, http.StatusOK, map[string]any{"task_id": taskID, "status": task.Status})
		return
	}
	if status.IsTerminal() {
		writeFailure(w, r, domain.NewFailure(domain.CodeCanceled,
			"Task 已结束，无法取消", map[string]any{"task_id": taskID, "status": task.Status}))
		return
	}

	entry := s.auditEntry(r, "cancel", "task", taskID, map[string]any{"status_before": task.Status})
	if _, err := s.st.CancelTask(ctx, taskID, Actor(ctx)); err != nil {
		s.failErr(w, r, err)
		return
	}
	_ = s.st.AppendAudit(ctx, entry)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID, "status": string(domain.TaskCancelling),
	})
}

// handleRetryTask 是 POST /api/v1/tasks/{task_id}/retry（C-5.6 / §7.8）：
// 整 Task 新 attempt 全量重跑含 fresh deploy；超限 409 ATTEMPT_EXHAUSTED。
func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	if _, err := s.st.Tasks.Get(ctx, nil, taskID); err != nil {
		s.failErr(w, r, err)
		return
	}
	attemptNo, err := s.st.RetryTask(ctx, taskID, Actor(ctx), Role(ctx), s.cfg.AttemptMax)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID, "attempt_no": attemptNo, "status": string(domain.TaskQueued),
	})
}

// handleReplayClean 是 POST /api/v1/tasks/{task_id}/clean（§9.3 / D-72 泄漏重放）。
// 编排层未接线（单 API 副本）时降级 503 EXECUTOR_UNAVAILABLE（可重试）。
func (s *Server) handleReplayClean(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	if s.replay == nil {
		fail(w, r, http.StatusServiceUnavailable, domain.CodeExecutorUnavailable,
			"编排层未接线，无法重放 clean；请稍后重试或直接在编排节点执行",
			map[string]any{"task_id": taskID}, true)
		return
	}
	var body struct {
		Attempt int  `json:"attempt"`
		Force   bool `json:"force"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "读取请求体失败", nil, false)
		return
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
				"请求体不是合法 JSON", nil, false)
			return
		}
	}
	res, err := s.replay.ReplayClean(ctx, taskID, body.Attempt, body.Force, Actor(ctx), Role(ctx))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":    taskID,
		"attempt_no": res.Key.AttemptNo,
		"runner": map[string]any{
			"seq":         res.Key.Seq,
			"name":        res.Name,
			"runner_name": res.RunnerName,
			"status":      string(res.Status),
		},
	})
}

// handleTaskEventsHistory 是 GET /api/v1/tasks/{task_id}/events/history（JSON 时间线首屏）。
func (s *Server) handleTaskEventsHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	if _, err := s.st.Tasks.Get(ctx, nil, taskID); err != nil {
		s.failErr(w, r, err)
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since_seq"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	if limit <= 0 || limit > sseReplayLimit {
		limit = sseReplayLimit
	}
	events, err := s.st.Events.ListByTask(ctx, nil, taskID, since, limit)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]eventDTO, 0, len(events))
	for i := range events {
		out = append(out, toEventDTO(events[i]))
	}
	latest, err := s.st.Events.LatestSeq(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	hasMore := len(events) > 0 && events[len(events)-1].Seq < latest
	var next *string
	if hasMore {
		v := strconv.FormatInt(events[len(events)-1].Seq, 10)
		next = &v
	}
	writeJSON(w, http.StatusOK, paged[eventDTO]{Items: out, NextCursor: next, HasMore: hasMore})
}

// handleTaskManifest 是 GET /api/v1/tasks/{task_id}/manifest（openapi ManifestSnapshot）。
func (s *Server) handleTaskManifest(w http.ResponseWriter, r *http.Request) {
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	task, err := s.st.Tasks.Get(r.Context(), nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	resp := map[string]any{
		"task_id":  task.ID,
		"manifest": json.RawMessage(task.Manifest),
		"resolved": nil,
	}
	if len(task.Resolved) > 0 {
		resp["resolved"] = json.RawMessage(task.Resolved)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTaskReport 是 GET /api/v1/tasks/{task_id}/report：未就绪返回 404 + Retry-After: 3。
func (s *Server) handleTaskReport(w http.ResponseWriter, r *http.Request) {
	s.serveTaskFile(w, r, "report.json")
}

// handleTaskReportMarkdown 是 GET /api/v1/tasks/{task_id}/report.md。
func (s *Server) handleTaskReportMarkdown(w http.ResponseWriter, r *http.Request) {
	s.serveTaskFile(w, r, "report.md")
}

// serveTaskFile 从 <storage-root>/<task_id>/<name> 读文件；不存在按 07 §5.1 返回 404+Retry-After。
func (s *Server) serveTaskFile(w http.ResponseWriter, r *http.Request, name string) {
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	if _, err := s.st.Tasks.Get(r.Context(), nil, taskID); err != nil {
		s.failErr(w, r, err)
		return
	}
	path := filepath.Join(s.cfg.StorageRoot, taskID, name)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Retry-After", "3")
			fail(w, r, http.StatusNotFound, domain.CodeNotFound,
				"报告尚未生成", map[string]any{"task_id": taskID, "file": name}, true)
			return
		}
		s.failErr(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if strings.HasSuffix(name, ".md") {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// runnerKeyFromURL 解析 (task_id, attempt?, seq) 三元组；attempt 缺省取最新。
func (s *Server) runnerKeyFromURL(r *http.Request) (domain.RunnerKey, *store.RunnerRun, error) {
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		return domain.RunnerKey{}, nil, domain.NewFailureError(domain.CodeValidationError, "非法 task id", nil)
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "seq"))
	if err != nil || seq < 1 {
		return domain.RunnerKey{}, nil, domain.NewFailureError(domain.CodeValidationError,
			"runner seq 必须是 ≥1 的整数", map[string]any{"seq": chi.URLParam(r, "seq")})
	}
	attemptRaw := ""
	if chi.URLParam(r, "attempt_no") != "" {
		attemptRaw = chi.URLParam(r, "attempt_no")
	} else {
		attemptRaw = r.URL.Query().Get("attempt")
	}
	attemptNo, _, qerr := s.resolveAttemptQuery(r.Context(), taskID, attemptRaw)
	err = qerr
	if err != nil {
		return domain.RunnerKey{}, nil, err
	}
	k := domain.RunnerKey{TaskID: taskID, AttemptNo: attemptNo, Seq: seq}
	rr, err := s.st.RunnerRuns.Get(r.Context(), nil, k)
	if err != nil {
		return domain.RunnerKey{}, nil, err
	}
	return k, rr, nil
}

// handleListTaskRunners 是 GET /api/v1/tasks/{task_id}/runners（缺省最新 attempt，?attempt= 切换）。
func (s *Server) handleListTaskRunners(w http.ResponseWriter, r *http.Request) {
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	attemptNo, _, err := s.resolveAttemptQuery(r.Context(), taskID, r.URL.Query().Get("attempt"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(r.Context(), nil, taskID, attemptNo)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paged[runnerExecDTO]{
		Items: runnerExecDTOs(runs), HasMore: false,
	})
}

// handleGetTaskRunner 是 GET /api/v1/tasks/{task_id}/runners/{seq}。
func (s *Server) handleGetTaskRunner(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRunnerExecDTO(rr))
}

// handleListTaskAttempts 是 GET /api/v1/tasks/{task_id}/attempts（attempt_no 升序）。
func (s *Server) handleListTaskAttempts(w http.ResponseWriter, r *http.Request) {
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	if _, err := s.st.Tasks.Get(r.Context(), nil, taskID); err != nil {
		s.failErr(w, r, err)
		return
	}
	attempts, err := s.st.Attempts.ListByTask(r.Context(), nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paged[attemptDTO]{Items: attemptDTOs(attempts), HasMore: false})
}

// handleListAttemptRunners 是 GET /api/v1/tasks/{task_id}/attempts/{attempt_no}/runners。
func (s *Server) handleListAttemptRunners(w http.ResponseWriter, r *http.Request) {
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	attemptNo, err := strconv.Atoi(chi.URLParam(r, "attempt_no"))
	if err != nil || attemptNo < 1 {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"attempt_no 必须是 ≥1 的整数", nil, false)
		return
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(r.Context(), nil, taskID, attemptNo)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paged[runnerExecDTO]{Items: runnerExecDTOs(runs), HasMore: false})
}

// handleGetAttemptRunner 是 GET /api/v1/tasks/{task_id}/attempts/{attempt_no}/runners/{seq}。
func (s *Server) handleGetAttemptRunner(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRunnerExecDTO(rr))
}

// runnerDirFor 是 runner 产物目录（attempts/<n>/runners/<seq>/；与编排层同一坐标系）。
func (s *Server) runnerDirFor(taskID string, attemptNo, seq int) string {
	return filepath.Join(s.cfg.StorageRoot, taskID, "attempts", strconv.Itoa(attemptNo),
		"runners", strconv.Itoa(seq))
}
