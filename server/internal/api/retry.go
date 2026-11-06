package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/report"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 重试系列（07-api.md §5.1、§8；03-data-view §4.1 的 runs.retry_of /
// tasks.reused_from_task_id）：
//
//   GET  /runs/{id}/retry-plan  → 200 计划（复用/重跑/受阻 + 是否可重试）
//   POST /runs/{id}/retry       → 202 新 Run（retry_of=原 Run）+ 计划
//   POST /tasks/{id}/retry      → 202 {task_id, attempt_no}
//
// 裁决（02-logical-view.md:430）：retry **不新建"同 Run 内重跑"**，而是创建新 Run 并
// 复用已成功且产物可校验的 Task（`reused_from_task_id`）。不改状态机，只加"复用校验"。

// retryReuseItem 是"将复用"的一条。
type retryReuseItem struct {
	TaskID string `json:"task_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// retryRerunItem 是"将重跑"的一条。
type retryRerunItem struct {
	TaskID  string          `json:"task_id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Failure *domain.Failure `json:"failure,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

// retryBlockedItem 是"想复用但被拒"的一条（产物缺失/不可校验）。
type retryBlockedItem struct {
	TaskID string `json:"task_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// retryPlanDTO 是 retry-plan 与 retry 的响应体（retry 多一个 new_run_id）。
type retryPlanDTO struct {
	RunID          string             `json:"run_id"`
	Retryable      bool               `json:"retryable"`
	Reason         string             `json:"reason,omitempty"`
	Reuse          []retryReuseItem   `json:"reuse"`
	Rerun          []retryRerunItem   `json:"rerun"`
	BlockedItems   []retryBlockedItem `json:"blocked_items"`
	ManifestDigest string             `json:"manifest_digest"`
	NewRunID       string             `json:"new_run_id,omitempty"`
	// RetryOf 只在 POST /runs/{id}/retry 的 202 响应里出现：新 Run 的 runs.retry_of
	// 恒等于本字段（= run_id）。给出它是为了让调用方**一次请求**就能断言来源关系，
	// 不必再 GET 新 Run；权威值仍以 `GET /runs/{new_run_id}` 的 `retry_of` 为准。
	RetryOf string `json:"retry_of,omitempty"`
}

// retryPlan 是内部计划（含落库需要的旧 Task 引用）。
type retryPlan struct {
	dto   retryPlanDTO
	reuse []*store.Task // 与 dto.Reuse 一一对应
	rerun []*store.Task // 与 dto.Rerun 一一对应
}

// buildRetryPlan 计算复用/重跑判定。
//
// 复用判定（不只看 DB 状态，**必须**验证 PVC 上的产物）：
//  1. Task 状态是 succeeded；
//  2. `<storage-root>/<run>/tasks/<task>/attempts/<current_attempt>/result.json` 存在；
//  3. 该文件能通过 `report.ValidateResult`（附录 A 的形状/枚举/唯一性校验）——
//     即"重跑一次也不会得到更好结论"的那部分才复用。
//
// 其余（failed/skipped/canceled，以及 succeeded 但产物不可校验的）进 rerun。
func (s *Server) buildRetryPlan(ctx context.Context, src *store.RetrySource) *retryPlan {
	plan := &retryPlan{}
	dto := &plan.dto
	dto.RunID = src.Run.ID
	dto.Reuse = []retryReuseItem{}
	dto.Rerun = []retryRerunItem{}
	dto.BlockedItems = []retryBlockedItem{}
	dto.ManifestDigest = manifestDigest(src.Run.Manifest)

	runTerminal := store.TerminalRunStatus(domain.RunStatus(src.Run.Status))
	retryInProgress := src.ActiveRetryID != ""

	for _, t := range src.Tasks {
		if t.Status != string(domain.TaskSucceeded) {
			dto.Rerun = append(dto.Rerun, retryRerunItem{
				TaskID: t.ID, Name: t.Name, Status: t.Status,
				Failure: taskFailure(t), Reason: "非成功终态",
			})
			plan.rerun = append(plan.rerun, t)
			continue
		}
		// succeeded：先验证产物，再决定复用还是重跑。
		//
		// 注意：**已复用的 Task 不得同时出现在 rerun / blocked_items 里**（票 #QA-1）——
		// 它本身没有 Attempt，产物在 reused/ 目录；只有 reused/ 下确实缺失或校验失败时
		// 才降级为重跑，并且理由要说清"复用的 result.json 缺失/校验失败"。
		reasonOf := "succeeded"
		if t.ReusedFromTaskID != nil && *t.ReusedFromTaskID != "" {
			reasonOf = "reused"
		}
		if reason := s.reuseBlockedReason(ctx, src.Run.ID, t); reason != "" {
			dto.BlockedItems = append(dto.BlockedItems, retryBlockedItem{
				TaskID: t.ID, Name: t.Name, Reason: reason,
			})
			dto.Rerun = append(dto.Rerun, retryRerunItem{
				TaskID: t.ID, Name: t.Name, Status: t.Status, Reason: "产物不可复用：" + reason,
			})
			plan.rerun = append(plan.rerun, t)
			continue
		}
		dto.Reuse = append(dto.Reuse, retryReuseItem{TaskID: t.ID, Name: t.Name, Reason: reasonOf})
		plan.reuse = append(plan.reuse, t)
	}

	switch {
	case !runTerminal:
		dto.Retryable = false
		dto.Reason = "run_not_terminal"
	case retryInProgress:
		dto.Retryable = false
		dto.Reason = "retry_in_progress"
	case src.Run.Status == string(domain.RunCanceled):
		dto.Retryable = false
		dto.Reason = "run_canceled"
	case len(dto.Rerun) == 0:
		dto.Retryable = false
		dto.Reason = "nothing_to_retry"
	default:
		dto.Retryable = true
	}
	return plan
}

// taskArtifactDir 是"某 Task 产物根目录"的**唯一真源**（绝对路径；storage_root 未配置
// 或该 Task 既没跑过也不是复用时返回空串）。
//
// 约定（唯一，禁止在别处再推一遍路径）：
//   - 复用来源（`reused_from_task_id` 非空，attempts=0）→ `<root>/<run>/tasks/<task>/reused`
//     ——复用不伪造 Attempt 目录（见 materializeReusedArtifacts）；
//   - 正常执行过的 Task → `<root>/<run>/tasks/<task>/attempts/<current_attempt>`。
//
// 注意：**没有** `attempt<=0 → 1` 的兜底。评审 #2 的 bug 就出在这里：复用 Task 的
// CurrentAttempt 是 0，被兜底成 attempts/1，于是第三代复用时 `pathUnder` 把所有文件拒掉，
// 新 Run 的复用 Task 落成 succeeded 但零产物。调用方拿到空串就是"没有产物"。
func (s *Server) taskArtifactDir(runID string, t *store.Task) string {
	if s.cfg == nil || strings.TrimSpace(s.cfg.StorageRoot) == "" || t == nil {
		return ""
	}
	if t.ReusedFromTaskID != nil && *t.ReusedFromTaskID != "" {
		return filepath.Join(s.cfg.StorageRoot, runID, "tasks", t.ID, "reused")
	}
	if t.CurrentAttempt <= 0 {
		return ""
	}
	return filepath.Join(s.cfg.StorageRoot, runID, "tasks", t.ID, "attempts", fmt.Sprint(t.CurrentAttempt))
}

// reuseBlockedReason 返回非空字符串表示"不能复用，原因如下"。
// 产物目录来自 taskArtifactDir（唯一真源），两条来源都要过 `report.ValidateResult`；
// 不通过的原因按来源区分措辞，避免把"复用的产物丢了"误报成"没跑过"。
func (s *Server) reuseBlockedReason(ctx context.Context, runID string, t *store.Task) string {
	reused := t.ReusedFromTaskID != nil && *t.ReusedFromTaskID != ""
	label := "result.json"
	if reused {
		label = "复用的 result.json"
	}
	dir := s.taskArtifactDir(runID, t)
	if dir == "" {
		if s.cfg == nil || strings.TrimSpace(s.cfg.StorageRoot) == "" {
			return "storage_root 未配置"
		}
		return label + " 不存在（该 Task 既没有 Attempt 也不是复用来源）"
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		if os.IsNotExist(err) {
			return label + " 不存在"
		}
		return "读取" + label + "失败: " + err.Error()
	}
	if f := report.ValidateResult(ctx, runID, t.ID, "", dir, s.resultMaxBytes()); f != nil {
		return fmt.Sprintf("%s 校验未通过（%s: %s）", label, f.Code, f.Message)
	}
	return ""
}

func (s *Server) resultMaxBytes() int64 {
	if s.cfg != nil && s.cfg.ResultMaxBytes > 0 {
		return s.cfg.ResultMaxBytes
	}
	return 100 << 20
}

// taskFailure 从 tasks.error 取失败对象（没有则 nil）。
func taskFailure(t *store.Task) *domain.Failure {
	if len(t.Error) == 0 {
		return nil
	}
	var f domain.Failure
	if err := json.Unmarshal(t.Error, &f); err != nil || f.Code == "" {
		return nil
	}
	return &f
}

func manifestDigest(manifest []byte) string {
	sum := sha256.Sum256(manifest)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// handleRetryPlan GET /runs/{id}/retry-plan（07 §5.1：200，预览将复用/将重跑）。
func (s *Server) handleRetryPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	src, err := s.st.LoadRetrySource(ctx, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	plan := s.buildRetryPlan(ctx, src)
	writeJSON(w, http.StatusOK, plan.dto)
}

// handleRetryRun POST /runs/{id}/retry（07 §5.1/§8：202 + 计划；已在执行 409）。
func (s *Server) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	src, err := s.st.LoadRetrySource(ctx, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	plan := s.buildRetryPlan(ctx, src)
	if !plan.dto.Retryable {
		fail(w, r, http.StatusConflict, domain.CodeConflict,
			"该 Run 现在不能重试: "+plan.dto.Reason,
			map[string]any{"reason": plan.dto.Reason, "run_id": runID}, false)
		return
	}

	now := s.now()
	newRunID := store.NewRunID(now)
	// 新 Task ID 一一对应旧 Task（复用与重跑都新建行，跨 Run 引用靠 reused_from_task_id）。
	newIDs := make(map[string]string, len(src.Tasks))
	for _, t := range src.Tasks {
		newIDs[t.ID] = store.NewTaskID()
	}
	reusedFrom := make(map[string]bool, len(plan.reuse))
	for _, t := range plan.reuse {
		reusedFrom[t.ID] = true
	}

	tasks := make([]*store.Task, 0, len(src.Tasks))
	reuseTasks := make([]*store.Task, 0, len(plan.reuse))
	for _, old := range src.Tasks {
		nt := *old // 深拷贝基础字段（下面按需覆盖 ID/状态/引用）
		nt.ID = newIDs[old.ID]
		nt.RunID = newRunID
		nt.CurrentAttempt = 0
		nt.CancelRequested = false
		nt.WaitingReason = nil
		nt.AvailableAt = nil
		nt.Error = nil
		nt.StatusReason = nil
		nt.ResultRef = nil
		nt.ReusedFromTaskID = nil
		nt.CreatedAt = now
		nt.UpdatedAt = now
		if reusedFrom[old.ID] {
			src := old.ID
			nt.Status = string(domain.TaskSucceeded)
			nt.ReusedFromTaskID = &src
			reuseTasks = append(reuseTasks, &nt)
		} else {
			nt.Status = string(domain.TaskPending)
		}
		tasks = append(tasks, &nt)
	}

	// resolved 重新解析：**不复用旧 precheck 判定**（服务可能已变）。
	resolved := refreshPrecheck(resolvedOf(src.Run), tasks)

	created, err := s.st.CreateRetryRun(ctx, store.RetryCreateParams{
		Source:   src.Run,
		Tasks:    tasks,
		Manifest: src.Run.Manifest,
		Resolved: resolved,
		Actor:    Actor(ctx),
		Role:     Role(ctx),
		At:       now,
	})
	if err != nil {
		if errors.Is(err, store.ErrRetryConflict) {
			fail(w, r, http.StatusConflict, domain.CodeConflict, err.Error(),
				map[string]any{"run_id": runID, "reason": store.RetryConflictReason(err)}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}

	// 事务外的最小副作用：把复用 Task 的产物硬链接进新 Run 目录，并重建产物索引；
	// 失败不撤销 Run（复用退化成"没有产物的 succeeded"，可再次 retry 修）。
	if err := s.materializeReusedArtifacts(ctx, src.Run.ID, created.ID, plan.reuse, newIDs); err != nil {
		s.logger.Warn("retry: 复用产物落地失败", "run_id", created.ID, "err", err)
	}

	// 入队（R1）：重跑的 Task 变 pending，由调度器按 Pre-Check 门控放行。
	if _, err := s.machines.Runs.Enqueue(ctx, created.ID, "system:orchestrator"); err != nil {
		s.logger.Warn("retry: 入队失败（对账会补）", "run_id", created.ID, "err", err)
	}

	plan.dto.NewRunID = created.ID
	plan.dto.RetryOf = src.Run.ID // = run_id；权威值以 GET /runs/{new} 的 retry_of 为准
	writeJSON(w, http.StatusAccepted, plan.dto)
}

// materializeReusedArtifacts 把旧 Run 里被复用 Task 的产物**硬链接**到新 Run 目录，
// 并按新路径重建 artifacts 索引。
//
// 为什么硬链接：产物就在同一块 PVC 上，硬链接零拷贝、不占额外空间、删任一 Run 都不
// 影响另一份（各自有独立 dentry）；跨设备（理论上 storage_root 换了盘）时退化成复制。
func (s *Server) materializeReusedArtifacts(ctx context.Context, oldRunID, newRunID string,
	reuse []*store.Task, newIDs map[string]string) error {

	if len(reuse) == 0 {
		return nil
	}
	if s.cfg == nil || strings.TrimSpace(s.cfg.StorageRoot) == "" {
		return nil
	}
	oldArts, err := s.st.Artifacts.ListByRun(ctx, nil, oldRunID)
	if err != nil {
		return err
	}
	byTask := map[string][]store.Artifact{}
	for _, a := range oldArts {
		if a.TaskID != nil {
			byTask[*a.TaskID] = append(byTask[*a.TaskID], a)
		}
	}

	rows := make([]store.Artifact, 0, len(reuse))
	for _, old := range reuse {
		newTaskID := newIDs[old.ID]
		// 唯一真源：复用来源指向 tasks/<task>/reused，而不是 attempts/<n>（评审 #2）。
		oldDir := s.taskArtifactDir(oldRunID, old)
		if oldDir == "" {
			continue
		}
		// 新目录：<new>/tasks/<newTask>/reused（不伪造成 attempt 目录——复用不产生新 Attempt）。
		relDir := filepath.ToSlash(filepath.Join(newRunID, "tasks", newTaskID, "reused"))
		newDir := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(relDir))
		if err := os.MkdirAll(newDir, 0o750); err != nil {
			return err
		}
		arts := byTask[old.ID]
		if len(arts) == 0 {
			// 索引缺失时按目录扫描兜底（保证 /runs/{new}/artifacts 有东西可列）。
			arts = walkArtifacts(oldDir, "")
		}
		for _, a := range arts {
			// 索引行是 storage-root 相对（含 run 前缀）；walk 兜底给的是绝对路径。
			oldAbs := filepath.FromSlash(a.Path)
			if !filepath.IsAbs(oldAbs) {
				oldAbs = filepath.Join(s.cfg.StorageRoot, oldAbs)
			}
			if !pathUnder(oldDir, oldAbs) {
				continue // 防御：只搬该 Task 自己的产物
			}
			relInTask := strings.TrimPrefix(filepath.ToSlash(oldAbs), filepath.ToSlash(oldDir)+"/")
			newRel := relDir + "/" + relInTask
			newAbs := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(newRel))
			if err := os.MkdirAll(filepath.Dir(newAbs), 0o750); err != nil {
				return err
			}
			if err := linkOrCopy(oldAbs, newAbs); err != nil {
				return err
			}
			row := a
			row.RunID = newRunID
			row.TaskID = &newTaskID
			row.AttemptID = nil
			row.Path = newRel
			row.IndexedAt = s.now()
			rows = append(rows, row)
			if strings.HasSuffix(relInTask, "result.json") {
				if err := s.st.SetTaskReuseRef(ctx, newTaskID, newRel); err != nil {
					return err
				}
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return s.st.InTx(ctx, func(tx pgx.Tx) error {
		return s.st.Artifacts.UpsertBatch(ctx, tx, newRunID, rows)
	})
}

// pathUnder 报告 target 是否在 base 目录之下（符号链接不跟随，防越界搬文件）。
func pathUnder(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// linkOrCopy 优先硬链接（零拷贝、同 PVC），跨设备时退化为复制。
func linkOrCopy(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return nil // 幂等：已存在即视为成功
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o640)
}

// walkArtifacts 扫描目录生成产物行（path 是 storage-root 相对，含 run 前缀）。
func walkArtifacts(dir, relPrefix string) []store.Artifact {
	var out []store.Artifact
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		out = append(out, store.Artifact{
			Path:      filepath.ToSlash(path), // 调用方会换成新相对路径
			SizeBytes: info.Size(),
			Kind:      "other",
		})
		return nil
	})
	return out
}

// refreshPrecheck 重新解析 resolved.precheck：
// 旧的 Pre-Check 判定不能被复用（服务/Endpoint 可能已经变了）。规则：
//   - seq=0 的 Pre-Check Task 被复用（已 succeeded）→ 新 Run 不再注入（injected=false）；
//   - 否则沿用旧的注入意图（injected 保持原值），让它在新 Run 里重新跑。
func refreshPrecheck(resolved map[string]any, tasks []*store.Task) []byte {
	injected := false
	if pc, ok := resolved["precheck"].(map[string]any); ok {
		if v, ok := pc["injected"].(bool); ok {
			injected = v
		}
	}
	if injected {
		for _, t := range tasks {
			if t.Seq == 0 && t.ReusedFromTaskID != nil {
				injected = false
				break
			}
		}
	}
	resolved["precheck"] = map[string]any{"injected": injected, "reparsed": true}
	raw, err := json.Marshal(resolved)
	if err != nil {
		return []byte(`{"precheck":{"injected":false,"reparsed":true}}`)
	}
	return raw
}

// resolvedOf 把 runs.resolved 解成 map（失败时给空对象）。
func resolvedOf(run *store.Run) map[string]any {
	out := map[string]any{}
	if len(run.Resolved) > 0 {
		_ = json.Unmarshal(run.Resolved, &out)
	}
	return out
}

// handleRetryTask POST /tasks/{id}/retry（07 §8：终态且非 canceled；在跑 409）。
func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID := chi.URLParam(r, "id")
	attemptNo, err := s.st.RetryTask(ctx, taskID, Actor(ctx), Role(ctx))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRetryConflict):
			fail(w, r, http.StatusConflict, domain.CodeConflict, err.Error(),
				map[string]any{"task_id": taskID, "reason": store.RetryConflictReason(err)}, false)
		case errors.Is(err, domain.ErrNotFound):
			fail(w, r, http.StatusNotFound, domain.CodeNotFound, "Task 不存在",
				map[string]any{"task_id": taskID}, false)
		default:
			s.failErr(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID, "attempt_no": attemptNo, "status": string(domain.TaskQueued),
	})
}
