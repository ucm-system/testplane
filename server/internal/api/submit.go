package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/orchestrator"
	"github.com/ucm-system/testplane/server/internal/store"
)

// maxBodyBytes 是请求体上限（07 §1、09 §9：1 MiB）。
const maxBodyBytes = 1 << 20

// handleSubmitTask 是 POST /api/v1/tasks（C-5.9 唯一提交端点）：
// 提交体是嵌套 task:（YAML 或 JSON），202 受理；dry_run=true 只校验（200）。
// **不做幂等**（D-37）：每次 POST 都创建新 Task。
//
// 提交校验 12 项在 orchestrator.Assembler.Assemble（C-5.1）；落库是
// store.SubmitTask（task + attempt#1 + 全部 task_runners + submitted 事件 + audit，
// C-2.5）。落库成功后：输入原文落盘 → 部署描述冻结 → Enqueue（queued）。
func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	if s.assemble == nil {
		fail(w, r, http.StatusServiceUnavailable, domain.CodeExecutorUnavailable,
			"Runner 注册表未接线，无法受理提交", nil, true)
		return
	}
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

	m, err := orchestrator.ParseManifest(body)
	if err != nil {
		writeFailure(w, r, domain.NewFailure(domain.CodeValidationError, err.Error(),
			map[string]any{"issues": []any{map[string]any{
				"path": "task", "code": domain.IssueCodeInputSyntax, "message": err.Error(),
			}}}))
		return
	}

	ctx := r.Context()
	taskID := store.NewTaskID(s.now())
	pipeline, err := s.assemble.Assemble(ctx, m, taskID, Actor(ctx), s.clock)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	if r.URL.Query().Get("dry_run") == "true" {
		writeJSON(w, http.StatusOK, dryRunResult(pipeline))
		return
	}

	created, runners, err := s.st.SubmitTask(ctx, store.SubmitParams{
		Task:    pipeline.Task,
		Runners: pipeline.Runners,
		Actor:   Actor(ctx),
		Role:    Role(ctx),
		SubmittedPayload: map[string]any{
			"name": pipeline.Task.Name,
			"mode": submitMode(pipeline),
		},
		At: s.now(),
	})
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	// 输入原文落盘（attempts/1/input/<seq>/params.<ext>；只读挂载源，§6.8）。
	// 失败不撤销提交：Task 已受理，对账收口（缺输入的 runner 由 Job 启动失败兜底）。
	if serr := s.persistInputs(pipeline); serr != nil {
		s.logger.Warn("submit: 输入落盘失败（Task 已受理）", "err", serr, "task_id", created.ID)
	}
	// 部署模式：冻结部署描述合并进 tasks.deploy（release_name/provider/spec_digest 只写一次）。
	if pipeline.DeployDoc != nil {
		if derr := s.st.RecordDeployState(ctx, created.ID, 1, "attempts/1/deploy-state.json", *pipeline.DeployDoc); derr != nil {
			s.logger.Warn("submit: 部署描述冻结失败", "err", derr, "task_id", created.ID)
		}
	}

	// submitted → queued（R1）。失败不撤销提交：Task 停在 submitted，
	// 由编排器的对账/恢复补齐（C-3.5）。
	if _, err := s.machines.Tasks.Enqueue(ctx, created.ID, Actor(ctx)); err != nil {
		s.logger.Warn("提交后 enqueue 失败，Task 留在 submitted 等待对账",
			"err", err, "task_id", created.ID, "request_id", RequestID(ctx))
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"task": map[string]any{
			"id":              created.ID,
			"status":          created.Status,
			"submitted_at":    created.SubmittedAt.UTC(),
			"current_attempt": created.CurrentAttempt,
		},
		"runners": acceptedRunners(runners),
	})
}

// submitMode 报告提交形态（审计载荷用）。
func submitMode(p *orchestrator.Pipeline) string {
	if p.DeployDoc != nil {
		return "deploy"
	}
	return "service"
}

// dryRunResult 是 ?dry_run=true 的应答（openapi DryRunResult）。
func dryRunResult(p *orchestrator.Pipeline) map[string]any {
	runners := make([]map[string]any, 0, len(p.Resolved.Runners))
	for _, rr := range p.Resolved.Runners {
		runners = append(runners, map[string]any{
			"seq": rr.Seq, "name": rr.Name, "runner": rr.Runner,
			"role": rr.Role, "injected": rr.Injected, "profile": rr.Profile,
		})
	}
	return map[string]any{
		"valid": true,
		"resolved_preview": map[string]any{
			"precheck": p.Resolved.Precheck,
			"runners":  runners,
		},
	}
}

// acceptedRunners 是 202 应答里的最终执行序（含注入行）。
func acceptedRunners(rows []*store.RunnerRun) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, rr := range rows {
		out = append(out, map[string]any{
			"seq": rr.Seq, "name": rr.Name, "runner": rr.RunnerName,
			"role": rr.Role, "status": rr.Status,
		})
	}
	return out
}

// persistInputs 把输入原文写进 <storage-root>/<task_id>/attempts/1/input/<seq>/params.<ext>。
func (s *Server) persistInputs(p *orchestrator.Pipeline) error {
	if s.cfg.StorageRoot == "" {
		return nil
	}
	for _, in := range p.Inputs {
		dir := filepath.Join(s.cfg.StorageRoot, p.Task.ID, "attempts", "1", "input", itoa(in.Seq))
		if err := os.MkdirAll(dir, 0o770); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "params."+extOf(in.Format)), []byte(in.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func extOf(format string) string {
	switch format {
	case domain.InputFormatYAML:
		return "yaml"
	case domain.InputFormatText:
		return "txt"
	default:
		return "jsonl"
	}
}
