package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"gopkg.in/yaml.v3"

	"github.com/ucm-system/testplane/server/internal/store"
)

// maxBodyBytes 是请求体上限（07 §1、09 §9：1 MiB）。
const maxBodyBytes = 1 << 20

// handleSubmitRun 是 POST /api/v1/runs：提交 Run / 独立 Task（202），dry_run 只校验（200）。
func (s *Server) handleSubmitRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, r, http.StatusRequestEntityTooLarge, domain.CodeValidationError,
				"请求体超过 1 MiB", map[string]any{"limit_bytes": maxBodyBytes}, false)
			return
		}
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "读取请求体失败", nil, false)
		return
	}

	ctx := r.Context()
	prepared, err := s.submit.Prepare(ctx, body, User(ctx))
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	if r.URL.Query().Get("dry_run") == "true" {
		writeJSON(w, http.StatusOK, map[string]any{
			"valid": true,
			"resolved_preview": map[string]any{
				"kind":         prepared.Kind,
				"tasks":        len(prepared.Tasks),
				"precheck":     precheckPreview(prepared),
				"endpoint_key": prepared.Endpoint.Key,
			},
		})
		return
	}

	run := &store.Run{
		ID:            prepared.RunID,
		Name:          prepared.Name,
		Kind:          prepared.Kind,
		Manifest:      prepared.Manifest,
		Resolved:      prepared.Resolved,
		CleanupPolicy: prepared.CleanupPolicy,
		SubmittedBy:   User(ctx),
		SubmittedAt:   s.now(),
		Endpoint:      endpointJSON(prepared.Endpoint, "service"),
	}
	tasks := make([]*store.Task, 0, len(prepared.Tasks))
	for _, st := range prepared.Tasks {
		tasks = append(tasks, st.Task)
	}

	if _, _, err := s.st.CreateRun(ctx, store.SubmitParams{
		Run:              run,
		Tasks:            tasks,
		Actor:            Actor(ctx),
		Role:             Role(ctx),
		SubmittedPayload: map[string]any{"name": run.Name, "kind": run.Kind},
		At:               s.now(),
	}); err != nil {
		s.failErr(w, r, err)
		return
	}

	// C-5.1：落库后立刻转 queued（R1）。失败不撤销提交：Run 停在 submitted，
	// 由 controller 的对账/恢复逻辑补齐（C-3.5）。
	if _, err := s.machines.Runs.Enqueue(ctx, run.ID, "system:orchestrator"); err != nil {
		s.logger.Warn("提交后 enqueue 失败，Run 留在 submitted 等待对账",
			"err", err, "run_id", run.ID, "request_id", RequestID(ctx))
	}

	cur, err := s.st.Runs.Get(ctx, nil, run.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	respTasks := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		respTasks = append(respTasks, map[string]any{
			"id": t.ID, "name": t.Name, "status": t.Status,
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run": map[string]any{
			"id": cur.ID, "status": cur.Status, "kind": cur.Kind,
			"submitted_at": cur.SubmittedAt.UTC(),
		},
		"tasks": respTasks,
	})
}

func precheckPreview(p *Prepared) map[string]any {
	if len(p.Tasks) == 0 || p.Tasks[0].Task == nil || p.Tasks[0].Task.Seq != 0 {
		return map[string]any{"injected": false}
	}
	return map[string]any{
		"injected": true,
		"tester":   p.Tasks[0].Spec.Name,
		"version":  p.Tasks[0].Spec.Version,
		"seq":      0,
	}
}

// handleListRuns 是 GET /api/v1/runs。
// runListDTO 是 Run 列表信封（openapi RunList）：游标模式与 offset 模式共用。
//
// D-61：UI 走 offset（page/page_size，额外回 total/page/page_size），
// agent/CLI 走 cursor；两者**互斥**，给了 page 就忽略 cursor。
type runListDTO struct {
	Items      []runDTO `json:"items"`
	NextCursor *string  `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
	// 以下三个只在 offset 模式下非 null（游标模式为 null，避免 count 成本）。
	Total    *int `json:"total"`
	Page     *int `json:"page"`
	PageSize *int `json:"page_size"`
}

// parseRunFilter 解析过滤参数（列表与 facets 共用同一份语义）。
func parseRunFilter(r *http.Request) (store.RunFilter, int, int, bool) {
	q := r.URL.Query()
	f := store.RunFilter{
		Statuses:   splitCSV(q.Get("status")),
		Kind:       q.Get("kind"),
		Submitters: splitCSV(q.Get("submitter")),
		Tester:     q.Get("tester"),
		Cursor:     q.Get("cursor"),
		Limit:      atoiDefault(q.Get("limit"), 50),
	}
	if f.Kind != "" && f.Kind != "run" && f.Kind != "task" {
		return f, 0, 0, false
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

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, page, pageSize, ok := parseRunFilter(r)
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"kind 只能是 run|task；page/page_size 不能为负", nil, false)
		return
	}

	out := make([]runDTO, 0, pageSize)
	var (
		total *int
		next  = ""
		more  bool
	)
	if page > 0 {
		// offset 模式：忽略 cursor（互斥，D-61）。
		f.Cursor = ""
		items, n, err := s.st.Runs.ListPage(ctx, nil, f, page, pageSize)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		total = &n
		more = page*pageSize < n
		out = s.fillRunDTOs(ctx, items, out)
	} else {
		// 游标模式（原行为，limit 默认 50）。
		items, cursor, err := s.st.Runs.List(ctx, nil, f)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		next, more = cursor, cursor != ""
		out = s.fillRunDTOs(ctx, items, out)
	}

	resp := runListDTO{Items: out, HasMore: more}
	if page > 0 {
		p, ps := page, pageSize
		resp.Total, resp.Page, resp.PageSize = total, &p, &ps
	}
	if next != "" {
		resp.NextCursor = cursorPtr(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

// fillRunDTOs 把 store 行投影成 DTO 并补 task_counts（一次批量查询，不 N+1）。
func (s *Server) fillRunDTOs(ctx context.Context, items []store.Run, out []runDTO) []runDTO {
	if len(items) == 0 {
		return out
	}
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	counts, err := s.st.Runs.TaskCounts(ctx, nil, ids)
	if err != nil {
		s.logger.Warn("runs: task counts 查询失败（本次不展示计数）", "err", err)
	}
	for i := range items {
		dto := toRunDTO(&items[i])
		if c, ok := counts[dto.ID]; ok {
			dto.TaskCounts = toRunTaskCounts(c)
		}
		out = append(out, dto)
	}
	return out
}

// handleRunFacets 是 GET /api/v1/runs/facets（D-61 / 07 §5.1）：
// 返回 status/submitter/kind/tester 四个维度的取值与计数，供表头下拉（Excel 式筛选）。
// 计数按维度独立（其它维度过滤生效、自己不生效），且与列表同一过滤参数族。
func (s *Server) handleRunFacets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, _, _, ok := parseRunFilter(r)
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"kind 只能是 run|task；page/page_size 不能为负", nil, false)
		return
	}
	// 四个维度的计数必须在同一事务快照里算，否则与列表/彼此之间可能瞬时不一致。
	var facets store.RunFacets
	if err := s.st.InTx(ctx, func(tx pgx.Tx) error {
		var ferr error
		facets, ferr = s.st.Runs.Facets(ctx, tx, f)
		return ferr
	}); err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, facets)
}

// handleGetRun 是 GET /api/v1/runs/{id}：详情 + Task 摘要（一条 JOIN，禁止 N+1）。
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	run, err := s.st.Runs.Get(ctx, nil, id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	summaries, err := s.st.Runs.TaskSummaries(ctx, nil, id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	counts, err := s.st.Runs.TaskCounts(ctx, nil, []string{id})
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dto := toRunDTO(run)
	if c, ok := counts[id]; ok {
		dto.TaskCounts = toRunTaskCounts(c)
	}
	dto.Tasks = make([]taskBriefDTO, 0, len(summaries))
	for _, t := range summaries {
		dto.Tasks = append(dto.Tasks, toTaskBriefDTO(t))
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleListRunTasks 是 GET /api/v1/runs/{id}/tasks（按 seq 游标分页）。
func (s *Server) handleListRunTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	q := r.URL.Query()
	items, next, err := s.st.Tasks.ListByRun(ctx, nil, runID, q.Get("cursor"), atoiDefault(q.Get("limit"), 50))
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

// handleCancelRun 是 POST /api/v1/runs/{id}/cancel（07 §8 的重复调用语义）。
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	run, err := s.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	status := domain.RunStatus(run.Status)
	switch {
	case status == domain.RunCancelling:
		writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": string(status)})
		return
	case status == domain.RunCanceled:
		writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": string(status)})
		return
	case status.IsTerminal():
		writeFailure(w, r, domain.NewFailure(domain.CodeCanceled,
			"Run 已结束，无法取消", map[string]any{"run_id": runID, "status": run.Status}))
		return
	}

	// 状态推进 + 审计同事务（PR #33 #6）：取消生效就一定有一条 audit_log。
	entry := s.auditEntry(r, "cancel", "run", runID, map[string]any{"status_before": run.Status})
	if _, err := s.machines.Runs.CancelAudited(ctx, runID, Actor(ctx), entry); err != nil {
		if errors.Is(err, domain.ErrTx) {
			s.failAuditTx(w, r, "cancel", "run", runID, err)
			return
		}
		if errors.Is(err, domain.ErrConflict) {
			// 竞态：读回事实，幂等返回。
			if cur, getErr := s.st.Runs.Get(ctx, nil, runID); getErr == nil {
				writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": cur.Status})
				return
			}
		}
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id": runID, "status": string(domain.RunCancelling),
	})
}

// handleRunEventsHistory 是 GET /api/v1/runs/{id}/events/history（JSON，时间线首屏）。
func (s *Server) handleRunEventsHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	if _, err := s.st.Runs.Get(ctx, nil, runID); err != nil {
		s.failErr(w, r, err)
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since_seq"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	if limit <= 0 || limit > sseReplayLimit {
		limit = sseReplayLimit
	}
	events, err := s.st.Events.ListByRun(ctx, nil, runID, since, limit)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]eventDTO, 0, len(events))
	for i := range events {
		out = append(out, toEventDTO(events[i]))
	}
	latest, err := s.st.Events.LatestSeq(ctx, nil, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	hasMore := len(events) > 0 && int64(events[len(events)-1].Seq) < latest
	var next *string
	if hasMore {
		v := strconv.FormatInt(events[len(events)-1].Seq, 10)
		next = &v
	}
	writeJSON(w, http.StatusOK, paged[eventDTO]{Items: out, NextCursor: next, HasMore: hasMore})
}

// handleRunManifest 是 GET /api/v1/runs/{id}/manifest（openapi ManifestSnapshot）：
// {run_id, manifest, resolved}；两者一经落库不再变。
func (s *Server) handleRunManifest(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.Runs.Get(r.Context(), nil, chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	resp := map[string]any{
		"run_id":   run.ID,
		"manifest": json.RawMessage(run.Manifest),
		"resolved": nil,
	}
	if len(run.Resolved) > 0 {
		resp["resolved"] = json.RawMessage(run.Resolved)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRunDeploy 是 GET /api/v1/runs/{id}/deploy（openapi DeploySnapshot）：
// runs.deploy + deploy/manifest.yaml + deploy/pods.json；service 模式返回 deploy:null、pods:[]。
// state 枚举按 C-5.3：queued / accepted / deploying / ready / blocked。
func (s *Server) handleRunDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	run, err := s.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dir := filepath.Join(s.cfg.StorageRoot, runID, "deploy")

	resp := map[string]any{
		"run_id": runID,
		"deploy": nil,
		"pods":   []any{},
	}
	if len(run.Deploy) > 0 {
		resp["deploy"] = json.RawMessage(run.Deploy)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "manifest.yaml")); err == nil {
		resp["manifest"] = yamlToJSON(data)
	}
	// D-59 / #43：Pod 拓扑来自落盘的 deploy/pods.json（collector 每 30s 原子写）。
	// 盘上没有就明确说明原因，不静默返回空数组。
	if doc, ok := readDeployPodsDoc(s.cfg.StorageRoot, runID); ok {
		resp["pods"] = doc.Pods
		resp["pods_generated_at"] = doc.GeneratedAt
	} else {
		resp["pods"] = []any{}
		if _, statErr := os.Stat(filepath.Join(dir, "pods.json")); statErr != nil {
			resp["pods_message"] = "deploy/pods.json 尚未生成：日志 collector 未运行或该 Run 还没有 Pod（D-59）"
		}
	}
	resp["deploy_timeout_seconds"] = int(s.cfg.DeployTimeout.Seconds())
	writeJSON(w, http.StatusOK, resp)
}

// handleRunReport 是 GET /api/v1/runs/{id}/report：未就绪返回 404 + Retry-After: 3。
func (s *Server) handleRunReport(w http.ResponseWriter, r *http.Request) {
	s.serveRunFile(w, r, "report.json")
}

// handleRunReportMarkdown 是 GET /api/v1/runs/{id}/report.md。
func (s *Server) handleRunReportMarkdown(w http.ResponseWriter, r *http.Request) {
	s.serveRunFile(w, r, "report.md")
}

// serveRunFile 从 <storage-root>/<run_id>/<name> 读文件；不存在按 07 §5.1 返回 404+Retry-After。
func (s *Server) serveRunFile(w http.ResponseWriter, r *http.Request, name string) {
	runID := chi.URLParam(r, "id")
	if _, err := s.st.Runs.Get(r.Context(), nil, runID); err != nil {
		s.failErr(w, r, err)
		return
	}
	path := filepath.Join(s.cfg.StorageRoot, runID, name)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Retry-After", "3")
			fail(w, r, http.StatusNotFound, domain.CodeNotFound,
				"报告尚未生成", map[string]any{"run_id": runID, "file": name}, true)
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
	switch {
	case strings.HasSuffix(name, ".md"):
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---- 小工具 --------------------------------------------------------------

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func endpointJSON(ep domain.Endpoint, source string) []byte {
	if ep.Key == "" {
		return nil
	}
	b, err := json.Marshal(map[string]any{
		"url":        ep.BaseURL,
		"model_name": ep.ModelName,
		"model_path": ep.ModelPath,
		"key":        ep.Key,
		"source":     source,
	})
	if err != nil {
		return nil
	}
	return b
}

// safePathComponent 拒绝路径穿越（C-7.5：不拼接用户输入中的 ..）。
func safePathComponent(v string) (string, error) {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, `/\`) {
		return "", fmt.Errorf("非法路径片段: %q", v)
	}
	return v, nil
}

// timeLayoutRFC3339 是响应里时间统一用的格式（UTC，带 Z）。
const timeLayoutRFC3339 = time.RFC3339

// yamlToJSON 把 deploy/manifest.yaml 的原文转成 JSON 值；解析失败退回原文。
func yamlToJSON(data []byte) any {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}
