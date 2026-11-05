package main

// 本文件是 cmd 层的装配与适配：
//   - storeRepos：把 store 的组合事务出口适配成 orchestrator.Repos（backend-exec 定的接口）
//   - registryStorage：把 store.TesterRegistry 适配成 registry.Store（Record ↔ store.Tester）
//   - testerResolver：registry 的 Resolve/List 直接满足 api.TesterResolver
//
// 之所以放在 cmd：store 不能反向依赖 orchestrator / registry（05 §3.1）。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/orchestrator"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/registry"
	"github.com/ucm-system/testplane/server/internal/store"
)

// ============================================================================
// orchestrator.Repos 适配器
// ============================================================================

type storeRepos struct {
	st       *store.Store
	machines *store.Machines
	reg      *registry.Registry
}

var _ orchestrator.Repos = (*storeRepos)(nil)

// buildRunView 把 store.Run 投影成编排层看到的快照。
func (r *storeRepos) runView(run *store.Run) orchestrator.RunView {
	v := orchestrator.RunView{
		ID:            run.ID,
		Status:        domain.RunStatus(run.Status),
		Kind:          run.Kind,
		CleanupPolicy: run.CleanupPolicy,
		SubmittedAt:   run.SubmittedAt,
	}
	if len(run.Manifest) > 0 {
		var m map[string]any
		if json.Unmarshal(run.Manifest, &m) == nil {
			_, v.HasDeploy = m["deploy"]
			if nested, ok := m["run"].(map[string]any); ok {
				if _, has := nested["deploy"]; has {
					v.HasDeploy = true
				}
			}
		}
	}
	if len(run.Deploy) > 0 {
		var d struct {
			DeploymentID  string `json:"deployment_id"`
			State         string `json:"state"`
			QueuePosition int    `json:"queue_position"`
			QueueReason   string `json:"queue_reason"`
		}
		if json.Unmarshal(run.Deploy, &d) == nil {
			v.DeployID = d.DeploymentID
			v.DeployState = d.State
			v.QueuePosition = d.QueuePosition
			v.QueueReason = d.QueueReason
			if d.State != "" {
				v.HasDeploy = true
			}
		}
	}
	if len(run.Endpoint) > 0 {
		var ep domain.Endpoint
		if json.Unmarshal(run.Endpoint, &ep) == nil {
			v.Endpoint = &ep
		}
	}
	if len(run.Error) > 0 {
		var f domain.Failure
		if json.Unmarshal(run.Error, &f) == nil {
			v.Failure = &f
		}
	}
	return v
}

// buildTaskView 把 store.Task 投影成编排层快照（Args 已是含注入的完整 argv）。
func (r *storeRepos) taskView(t *store.Task) orchestrator.TaskView {
	v := orchestrator.TaskView{
		ID: t.ID, RunID: t.RunID, Seq: t.Seq, Name: t.Name,
		TesterName: t.TesterName, TesterVersion: t.TesterVersion,
		Image: t.ImageRef, OnFailure: t.OnFailure,
		TimeoutSeconds: t.TimeoutSeconds,
		Status:         domain.TaskStatus(t.Status),
		EndpointKey:    store.DerefStr(t.EndpointKey),
		Exclusive:      t.EndpointExclusive,
		CancelRequested: t.CancelRequested,
		AttemptNo:      t.CurrentAttempt,
		TesterRole:     "test",
	}
	if t.StatusReason != nil {
		v.StatusReason = *t.StatusReason
	}
	if t.WaitingReason != nil {
		v.WaitingReason = *t.WaitingReason
	}
	if len(t.Error) > 0 {
		var f domain.Failure
		if json.Unmarshal(t.Error, &f) == nil {
			v.Failure = &f
		}
	}
	// Args = 已注入公共参数的完整 argv；deploy 模式下回填前 csv 为空则退回 args_argv。
	if len(t.InjectedArgv) > 0 && string(t.InjectedArgv) != "[]" {
		_ = json.Unmarshal(t.InjectedArgv, &v.Args)
	}
	if len(v.Args) == 0 && len(t.ArgsArgv) > 0 {
		_ = json.Unmarshal(t.ArgsArgv, &v.Args)
	}
	if len(t.Env) > 0 {
		_ = json.Unmarshal(t.Env, &v.Env)
	}
	if len(t.SecretEnv) > 0 {
		var refs []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.SecretEnv, &refs) == nil {
			for _, ref := range refs {
				v.SecretEnv = append(v.SecretEnv, ref.Name)
			}
		}
	}
	// Tester 的 role / image / entry / resources / mounts 以注册表为准（Task 只冻结了 name/version/image_ref）。
	if r.reg != nil {
		if spec, err := r.reg.Resolve(context.Background(), t.TesterName, t.TesterVersion); err == nil {
			v.TesterRole = spec.Role
			if spec.Image != "" {
				v.Image = spec.Image
			}
			v.Entry = spec.Entry
			v.Resources = spec.Resources
			v.Mounts = spec.Mounts
		}
	}
	return v
}

func (r *storeRepos) GetRun(ctx context.Context, runID string) (orchestrator.RunView, error) {
	run, err := r.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		return orchestrator.RunView{}, err
	}
	return r.runView(run), nil
}

func (r *storeRepos) GetTask(ctx context.Context, taskID string) (orchestrator.TaskView, error) {
	t, err := r.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		return orchestrator.TaskView{}, err
	}
	return r.taskView(t), nil
}

func (r *storeRepos) ListTasks(ctx context.Context, runID string) ([]orchestrator.TaskView, error) {
	var out []orchestrator.TaskView
	cursor := ""
	for {
		batch, next, err := r.st.Tasks.ListByRun(ctx, nil, runID, cursor, 200)
		if err != nil {
			return nil, err
		}
		for i := range batch {
			out = append(out, r.taskView(&batch[i]))
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

func (r *storeRepos) Candidates(ctx context.Context, limit int) ([]orchestrator.Candidate, error) {
	rows, err := r.st.CandidatesDetailed(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.Candidate, 0, len(rows))
	for _, c := range rows {
		out = append(out, orchestrator.Candidate{
			TaskID: c.TaskID, RunID: c.RunID, Seq: c.Seq,
			EndpointKey: c.EndpointKey, Exclusive: c.Exclusive,
			TesterRole: c.TesterRole, SubmittedAt: c.SubmittedAt,
		})
	}
	return out, nil
}

func (r *storeRepos) CountActiveAttempts(ctx context.Context) (int, error) {
	return r.st.Attempts.CountActive(ctx, nil)
}

func (r *storeRepos) ListExpiredAttempts(ctx context.Context, now time.Time) ([]orchestrator.AttemptRef, error) {
	rows, err := r.st.ExpiredAttempts(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.AttemptRef, 0, len(rows))
	for _, e := range rows {
		out = append(out, orchestrator.AttemptRef{
			AttemptID: e.AttemptID, TaskID: e.TaskID, RunID: e.RunID,
			AttemptNo: e.AttemptNo, JobName: e.JobName,
		})
	}
	return out, nil
}

func (r *storeRepos) ListRunsQueued(ctx context.Context, limit int) ([]orchestrator.RunView, error) {
	runs, err := r.st.ListRunsByStatus(ctx, []string{string(domain.RunQueued)}, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.RunView, 0, len(runs))
	for i := range runs {
		out = append(out, r.runView(&runs[i]))
	}
	return out, nil
}

func (r *storeRepos) ListRunsInDeploy(ctx context.Context, limit int) ([]orchestrator.RunView, error) {
	runs, err := r.st.ListRunsByStatus(ctx, []string{string(domain.RunDeploying)}, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.RunView, 0, len(runs))
	for i := range runs {
		out = append(out, r.runView(&runs[i]))
	}
	return out, nil
}

func (r *storeRepos) ListCancellingRuns(ctx context.Context, limit int) ([]orchestrator.RunView, error) {
	runs, err := r.st.ListRunsByStatus(ctx, []string{string(domain.RunCancelling)}, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.RunView, 0, len(runs))
	for i := range runs {
		out = append(out, r.runView(&runs[i]))
	}
	return out, nil
}

func (r *storeRepos) BeginTesting(ctx context.Context, runID string) (int, error) {
	return r.st.BeginTestingService(ctx, runID)
}

func (r *storeRepos) AttemptByJobName(ctx context.Context, jobName string) (orchestrator.AttemptRef, error) {
	a, taskID, runID, err := r.st.AttemptByJobName(ctx, jobName)
	if err != nil {
		return orchestrator.AttemptRef{}, err
	}
	return orchestrator.AttemptRef{
		AttemptID: a.ID, TaskID: taskID, RunID: runID, AttemptNo: a.Idx, JobName: jobName,
	}, nil
}

// ListActiveTasks 返回对账输入集合（preparing/running 的 Task）。
func (r *storeRepos) ListActiveTasks(ctx context.Context, limit int) ([]orchestrator.TaskView, error) {
	rows, err := r.st.ListActiveTasks(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.TaskView, 0, len(rows))
	for i := range rows {
		out = append(out, r.taskView(&rows[i]))
	}
	return out, nil
}

// AttemptByTask 读该 Task 当前进行中的 Attempt（含 executor_ref 里的 Job 名）。
func (r *storeRepos) AttemptByTask(ctx context.Context, taskID string) (orchestrator.AttemptRef, error) {
	a, jobName, err := r.st.ActiveAttemptOfTask(ctx, taskID)
	if err != nil {
		return orchestrator.AttemptRef{}, err
	}
	return orchestrator.AttemptRef{
		AttemptID: a.ID, TaskID: a.TaskID, AttemptNo: a.Idx, JobName: jobName,
	}, nil
}

// DeleteLeaseByTask 幂等删除该 Task 的端点租约（C-4.3 泄漏兜底）。
func (r *storeRepos) DeleteLeaseByTask(ctx context.Context, taskID string) error {
	return r.st.Leases.Delete(ctx, nil, taskID)
}

// ListStaleLeases 返回 Task 已终态（或已不存在）但租约还在的 task_id。
func (r *storeRepos) ListStaleLeases(ctx context.Context, limit int) ([]string, error) {
	return r.st.ListStaleLeases(ctx, limit)
}

func (r *storeRepos) ClaimForExecution(ctx context.Context, req orchestrator.ClaimRequest) (orchestrator.ClaimResult, bool, string, error) {
	res, claimed, reason, err := r.st.ClaimForExecution(ctx, store.ClaimRequest{
		TaskID:       req.TaskID,
		EndpointKey:  req.EndpointKey,
		Exclusive:    req.Exclusive,
		GlobalLimit:  req.GlobalLimit,
		AttemptIdx:   req.AttemptIdx,
		JobID:        req.JobID,
		JobNamespace: req.JobNamespace,
		ExecutorRef:  req.ExecutorRef,
	})
	if err != nil || !claimed {
		return orchestrator.ClaimResult{}, claimed, reason, err
	}
	attemptNo := 0
	if res.Attempt != nil {
		attemptNo = res.Attempt.Idx
	}
	return orchestrator.ClaimResult{
		AttemptID: res.AttemptID,
		JobName:   res.JobName,
		AttemptNo: attemptNo,
	}, true, "", nil
}

func (r *storeRepos) SetWaitingReason(ctx context.Context, taskID, reason string) error {
	return r.st.Tasks.SetWaitingReason(ctx, nil, taskID, reason)
}

func (r *storeRepos) MarkAttemptRunning(ctx context.Context, attemptID string, h ports.JobHandle) error {
	return r.st.MarkAttemptRunning(ctx, attemptID, h.Namespace, h.JobName)
}

func (r *storeRepos) MarkCanceled(ctx context.Context, attemptID string) error {
	return r.st.MarkAttemptCanceled(ctx, attemptID)
}

func (r *storeRepos) QueueTasks(ctx context.Context, runID string, onlySeqZero bool) ([]string, error) {
	return r.st.QueueTasksTransition(ctx, runID, onlySeqZero)
}

func (r *storeRepos) FinishAttempt(ctx context.Context, in orchestrator.FinishInput) (orchestrator.FinishOutcome, error) {
	finished := in.FinishedAt
	if finished.IsZero() {
		finished = time.Now().UTC()
	}
	res, err := r.st.FinishAttempt(ctx, store.AttemptFinish{
		AttemptID: in.AttemptID, TaskID: in.TaskID, RunID: in.RunID,
		Succeeded: in.Phase == "succeeded", Phase: in.Phase,
		ExitCode: in.ExitCode, ImageDigest: in.ImageDigest,
		Failure: in.Failure, FinishedAt: finished,
	})
	if err != nil {
		return orchestrator.FinishOutcome{}, err
	}
	out := orchestrator.FinishOutcome{
		Applied:             res.AttemptApplied,
		TaskTerminal:        res.TaskTerminal,
		RunAllTerminal:      res.RunAllTerminal,
		AbortTriggered:      res.AbortTriggered,
		NeedsQueueUnblocked: res.PreCheckPassed && res.TaskTerminal,
	}
	for _, attemptID := range res.CancelAttempts {
		out.CancelJobs = append(out.CancelJobs, orchestrator.AttemptRef{AttemptID: attemptID})
	}
	return out, nil
}

func (r *storeRepos) AbortRun(ctx context.Context, in orchestrator.AbortInput) (orchestrator.AbortOutcome, error) {
	skipped, attempts, err := r.st.AbortOtherTasks(ctx, in.RunID, in.FailedTask, in.Reason)
	if err != nil {
		return orchestrator.AbortOutcome{}, err
	}
	out := orchestrator.AbortOutcome{Skipped: skipped}
	for _, id := range attempts {
		out.CancelJobs = append(out.CancelJobs, orchestrator.AttemptRef{AttemptID: id})
	}
	return out, nil
}

func (r *storeRepos) BeginDeploy(ctx context.Context, runID, deployID string) error {
	_, err := r.machines.Runs.BeginDeploy(ctx, runID, deployID)
	return err
}

func (r *storeRepos) RecordDeployState(ctx context.Context, runID string, st ports.DeployState) error {
	return r.st.RecordDeployState(ctx, runID, st.Phase, st.Reason, 0)
}

func (r *storeRepos) DeployReady(ctx context.Context, runID string, ep domain.Endpoint) (int, error) {
	if _, err := r.machines.Runs.DeployReady(ctx, runID, ep); err != nil {
		return 0, err
	}
	tasks, err := r.ListTasks(ctx, runID)
	if err != nil {
		return 0, err
	}
	return len(tasks), nil
}

func (r *storeRepos) DeployBlocked(ctx context.Context, runID, reason string, f *domain.Failure) error {
	failure := domain.NewFailure(domain.CodePrecheckFailed, reason, nil)
	if f != nil {
		failure = *f
	}
	_, err := r.machines.Runs.DeployBlocked(ctx, runID, failure)
	return err
}

func (r *storeRepos) FinishDeployAsFailed(ctx context.Context, runID string, f *domain.Failure) error {
	failure := domain.NewFailure(domain.CodeInternalError, "部署失败", nil)
	if f != nil {
		failure = *f
	}
	_, err := r.machines.Runs.Fail(ctx, runID, failure)
	return err
}

func (r *storeRepos) BeginCleanup(ctx context.Context, runID, reason string) error {
	_, err := r.machines.Runs.BeginCleanup(ctx, runID, reason)
	return err
}

func (r *storeRepos) FinishRun(ctx context.Context, runID string, status domain.RunStatus, f *domain.Failure) error {
	if status == domain.RunCanceled {
		_, err := r.machines.Runs.Canceled(ctx, runID)
		return err
	}
	_, err := r.machines.Runs.Finish(ctx, runID, store.RunOutcome{Status: status, Failure: f})
	return err
}

func (r *storeRepos) RequestCancelRun(ctx context.Context, runID, actor string) (orchestrator.CancelOutcome, error) {
	skipped, attempts, err := r.st.RequestCancelRunTx(ctx, runID, actor)
	if err != nil {
		return orchestrator.CancelOutcome{}, err
	}
	out := orchestrator.CancelOutcome{Skipped: skipped, AllStopped: len(attempts) == 0}
	for _, id := range attempts {
		out.CancelJobs = append(out.CancelJobs, orchestrator.AttemptRef{AttemptID: id})
	}
	return out, nil
}

func (r *storeRepos) RequestCancelTask(ctx context.Context, taskID, actor string) (orchestrator.CancelOutcome, error) {
	skipped, attempts, err := r.st.RequestCancelTaskTx(ctx, taskID, actor)
	if err != nil {
		return orchestrator.CancelOutcome{}, err
	}
	out := orchestrator.CancelOutcome{Skipped: skipped, AllStopped: len(attempts) == 0}
	for _, id := range attempts {
		out.CancelJobs = append(out.CancelJobs, orchestrator.AttemptRef{AttemptID: id})
	}
	return out, nil
}

// ============================================================================
// registry.Store 适配器（Record ↔ store.Tester）
// ============================================================================

type registryStorage struct {
	reg *store.TesterRegistry
}

var _ registry.Store = (*registryStorage)(nil)

func (s *registryStorage) UpsertTester(ctx context.Context, rec registry.Record) (bool, error) {
	row := &store.Tester{
		Name: rec.Spec.Name, Version: rec.Spec.Version,
		DisplayName:  rec.Spec.DisplayName,
		Description:  rec.Spec.Description,
		Role:         rec.Spec.Role,
		OnFailure:    rec.Spec.OnFailure,
		Entry:        rec.Spec.Entry,
		Image:        rec.Spec.Image,
		Resources:    mustJSON(rec.Spec.Resources),
		Concurrency:  mustJSON(rec.Spec.Concurrency),
		Mounts:       mustJSON(rec.Spec.Mounts),
		K8s:          []byte("{}"),
		Parameters:   mustJSON(rec.Spec.Parameters),
		EnvDecl:      mustJSON(rec.Spec.Env),
		Placeholder:  nullable(rec.Spec.Placeholder),
		SourceFile:   rec.SourceFile,
		ContentSHA:   rec.ContentSHA,
		Orphaned:     rec.Orphaned,
		RegisteredBy: "system:registry",
	}
	return s.reg.UpsertTester(ctx, row)
}

func (s *registryStorage) GetTester(ctx context.Context, name, version string) (registry.Record, error) {
	row, err := s.reg.GetTester(ctx, name, version)
	if err != nil {
		return registry.Record{}, err
	}
	return toRegistryRecord(row)
}

func (s *registryStorage) ListTesters(ctx context.Context) ([]registry.Record, error) {
	rows, err := s.reg.ListTesters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Record, 0, len(rows))
	for i := range rows {
		rec, err := toRegistryRecord(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func toRegistryRecord(t *store.Tester) (registry.Record, error) {
	spec := domain.TesterSpec{
		Name: t.Name, Version: t.Version,
		DisplayName: t.DisplayName, Description: t.Description,
		Role: t.Role, OnFailure: t.OnFailure,
		Entry: t.Entry, Image: t.Image,
		Placeholder: store.DerefStr(t.Placeholder),
	}
	if err := unmarshalInto(t.Resources, &spec.Resources); err != nil {
		return registry.Record{}, fmt.Errorf("registry adapter: resources %s@%s: %w", t.Name, t.Version, err)
	}
	if err := unmarshalInto(t.Concurrency, &spec.Concurrency); err != nil {
		return registry.Record{}, fmt.Errorf("registry adapter: concurrency %s@%s: %w", t.Name, t.Version, err)
	}
	if err := unmarshalInto(t.Mounts, &spec.Mounts); err != nil {
		return registry.Record{}, fmt.Errorf("registry adapter: mounts %s@%s: %w", t.Name, t.Version, err)
	}
	if err := unmarshalInto(t.Parameters, &spec.Parameters); err != nil {
		return registry.Record{}, fmt.Errorf("registry adapter: parameters %s@%s: %w", t.Name, t.Version, err)
	}
	if err := unmarshalInto(t.EnvDecl, &spec.Env); err != nil {
		return registry.Record{}, fmt.Errorf("registry adapter: env %s@%s: %w", t.Name, t.Version, err)
	}
	return registry.Record{
		Spec: spec, ContentSHA: t.ContentSHA, SourceFile: t.SourceFile, Orphaned: t.Orphaned,
	}, nil
}

func unmarshalInto(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
