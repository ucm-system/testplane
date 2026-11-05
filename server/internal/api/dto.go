package api

import (
	"encoding/json"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 本文件是"库行 → API 响应"的投影。只挑对外字段，不把 last_event_seq / revision
// 这类内部列泄出去（07 §5）。

// runDTO 是 Run 详情/列表项（openapi Run）。
type runDTO struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	CleanupPolicy string          `json:"cleanup_policy"`
	SubmittedBy   string          `json:"submitted_by"`
	SubmittedAt   time.Time       `json:"submitted_at"`
	StartedAt     *time.Time      `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
	Endpoint      json.RawMessage `json:"endpoint"`
	Deploy        json.RawMessage `json:"deploy"`
	Error         json.RawMessage `json:"error"`
	// RetryOf 是本 Run 由哪次重试产生（原 Run 的 ID）；nil 表示不是重试产生的。
	// 与 tasks.reused_from_task_id 一起表达"跨 Run 续跑"（03 §4.1）。
	RetryOf    *string        `json:"retry_of"`
	Revision   int64          `json:"revision"`
	TaskCounts *runTaskCounts `json:"task_counts,omitempty"`
	Tasks      []taskBriefDTO `json:"tasks,omitempty"`
}

// runTaskCounts 是 Run 的 Task 状态聚合（openapi Run.task_counts）。
type runTaskCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// taskBriefDTO 是 Run 详情里的 Task 摘要（openapi TaskSummary，一条 JOIN 取）。
type taskBriefDTO struct {
	ID            string  `json:"id"`
	Seq           int     `json:"seq"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	WaitingReason *string `json:"waiting_reason"`
	Attempts      int     `json:"attempts"`
	// ReusedFromTaskID 是本 Task 复用自哪个旧 Task（retry 的跨 Run 续跑，03 §4.2）；
	// nil 表示不是复用来的。07 §4.2 要求对外可见。
	ReusedFromTaskID *string         `json:"reused_from_task_id"`
	Error            json.RawMessage `json:"error"`
}

// taskDTO 是 Task 详情。
type taskDTO struct {
	ID                string          `json:"id"`
	RunID             string          `json:"run_id"`
	Seq               int             `json:"seq"`
	Name              string          `json:"name"`
	TesterName        string          `json:"tester_name"`
	TesterVersion     string          `json:"tester_version"`
	ImageRef          string          `json:"image_ref"`
	Profile           string          `json:"profile"`
	ArgsRaw           string          `json:"args_raw"`
	ArgsArgv          json.RawMessage `json:"args_argv"`
	InjectedArgv      json.RawMessage `json:"injected_argv"`
	Env               json.RawMessage `json:"env"`
	SecretEnv         json.RawMessage `json:"secret_env"`
	OnFailure         string          `json:"on_failure"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	RetryPolicy       json.RawMessage `json:"retry_policy"`
	Status            string          `json:"status"`
	StatusReason      *string         `json:"status_reason,omitempty"`
	WaitingReason     *string         `json:"waiting_reason,omitempty"`
	EndpointKey       *string         `json:"endpoint_key,omitempty"`
	EndpointExclusive bool            `json:"endpoint_exclusive"`
	CurrentAttempt    int             `json:"current_attempt"`
	// ReusedFromTaskID 见 taskBriefDTO（07 §4.2 / 03 §4.2）。
	ReusedFromTaskID *string         `json:"reused_from_task_id"`
	AvailableAt      *time.Time      `json:"available_at,omitempty"`
	CancelRequested  bool            `json:"cancel_requested"`
	ResultRef        *string         `json:"result_ref,omitempty"`
	Error            json.RawMessage `json:"error"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// attemptDTO 是 Attempt 详情（openapi Attempt：不暴露 job_id / job_namespace）。
type attemptDTO struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	Idx         int             `json:"idx"`
	Status      string          `json:"status"`
	Phase       *string         `json:"phase,omitempty"`
	ExitCode    *int32          `json:"exit_code,omitempty"`
	ImageDigest *string         `json:"image_digest,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	LogRef      *string         `json:"log_ref,omitempty"`
	ResultRef   *string         `json:"result_ref,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	ExecutorRef json.RawMessage `json:"executor_ref,omitempty"`
}

// testerDTO 是 Tester 目录项（tester.yaml 的对外投影）。
type testerDTO struct {
	Name        string                   `json:"name"`
	Version     string                   `json:"version"`
	DisplayName string                   `json:"display_name,omitempty"`
	Description string                   `json:"description,omitempty"`
	Role        string                   `json:"role"`
	OnFailure   string                   `json:"on_failure"`
	Entry       string                   `json:"entry"`
	Image       string                   `json:"image"`
	Resources   domain.ResourceRequest   `json:"resources"`
	Concurrency domain.ConcurrencyPolicy `json:"concurrency,omitempty"`
	Mounts      []domain.MountSpec       `json:"mounts,omitempty"`
	Parameters  map[string]domain.Param  `json:"parameters,omitempty"`
	Env         map[string]domain.EnvVar `json:"env,omitempty"`
	Placeholder string                   `json:"placeholder,omitempty"`

	// 注册表元数据（openapi Tester 的 allOf 第二支要求）。
	ContentSHA   string     `json:"content_sha,omitempty"`
	SourceFile   string     `json:"source_file,omitempty"`
	Orphaned     bool       `json:"orphaned"`
	RegisteredAt *time.Time `json:"registered_at,omitempty"`
	RegisteredBy string     `json:"registered_by,omitempty"`
}

// eventDTO 是 openapi Event 的投影：时间统一 UTC（`Z`），可空字段显式 null。
type eventDTO struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	RunID     string          `json:"run_id"`
	TaskID    *string         `json:"task_id"`
	AttemptID *string         `json:"attempt_id"`
	At        time.Time       `json:"at"`
	From      *string         `json:"from"`
	To        *string         `json:"to"`
	Reason    *string         `json:"reason"`
	Actor     *string         `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
}

func toEventDTO(e store.Event) eventDTO {
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return eventDTO{
		Seq: e.Seq, Type: e.Type, RunID: e.RunID,
		TaskID: e.TaskID, AttemptID: e.AttemptID,
		At: e.CreatedAt.UTC(), From: e.FromStatus, To: e.ToStatus,
		Reason: e.Reason, Actor: e.Actor, Payload: payload,
	}
}

// taskDetailDTO 是 openapi TaskDetail：Task 字段平铺 + attempts 数组。'
type taskDetailDTO struct {
	taskDTO
	Attempts []attemptDTO `json:"attempts"`
}

// paged 是游标分页的统一信封（07 §6 / openapi {items, next_cursor, has_more}）。
type paged[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

// cursorPtr 把空串转成 nil（has_more=false 时 next_cursor 为 null）。
func cursorPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toRunDTO(r *store.Run) runDTO {
	return runDTO{
		ID:            r.ID,
		Name:          r.Name,
		Kind:          r.Kind,
		Status:        r.Status,
		CleanupPolicy: r.CleanupPolicy,
		SubmittedBy:   r.SubmittedBy,
		SubmittedAt:   r.SubmittedAt.UTC(),
		StartedAt:     utcPtr(r.StartedAt),
		FinishedAt:    utcPtr(r.FinishedAt),
		Endpoint:      store.JSONOrNil(r.Endpoint),
		Deploy:        store.JSONOrNil(r.Deploy),
		Error:         store.JSONOrNil(r.Error),
		RetryOf:       r.RetryOf,
	}
}

func toRunTaskCounts(c store.RunTaskCounts) *runTaskCounts {
	return &runTaskCounts{
		Total: c.Total, Pending: c.Pending, Running: c.Running,
		Succeeded: c.Succeeded, Failed: c.Failed, Skipped: c.Skipped,
	}
}

func toTaskBriefDTO(t store.TaskSummaryRow) taskBriefDTO {
	return taskBriefDTO{
		ID:               t.ID,
		Seq:              t.Seq,
		Name:             t.Name,
		Status:           t.Status,
		WaitingReason:    t.WaitingReason,
		Attempts:         t.Attempts,
		ReusedFromTaskID: t.ReusedFromTaskID,
		Error:            store.JSONOrNil(t.Error),
	}
}

func toTaskDTO(t *store.Task) taskDTO {
	return taskDTO{
		ID: t.ID, RunID: t.RunID, Seq: t.Seq, Name: t.Name,
		TesterName: t.TesterName, TesterVersion: t.TesterVersion, ImageRef: t.ImageRef,
		Profile: t.Profile, ArgsRaw: t.ArgsRaw,
		ArgsArgv:          store.JSONOrNil(t.ArgsArgv),
		InjectedArgv:      store.JSONOrNil(t.InjectedArgv),
		Env:               store.JSONOrNil(t.Env),
		SecretEnv:         store.JSONOrNil(t.SecretEnv),
		OnFailure:         t.OnFailure,
		TimeoutSeconds:    t.TimeoutSeconds,
		RetryPolicy:       store.JSONOrNil(t.RetryPolicy),
		Status:            t.Status,
		StatusReason:      t.StatusReason,
		WaitingReason:     t.WaitingReason,
		EndpointKey:       t.EndpointKey,
		EndpointExclusive: t.EndpointExclusive,
		CurrentAttempt:    t.CurrentAttempt,
		ReusedFromTaskID:  t.ReusedFromTaskID,
		AvailableAt:       utcPtr(t.AvailableAt),
		CancelRequested:   t.CancelRequested,
		ResultRef:         t.ResultRef,
		Error:             store.JSONOrNil(t.Error),
		CreatedAt:         t.CreatedAt.UTC(),
		UpdatedAt:         t.UpdatedAt.UTC(),
	}
}

func toAttemptDTO(a *store.Attempt) attemptDTO {
	return attemptDTO{
		ID: a.ID, TaskID: a.TaskID, Idx: a.Idx, Status: a.Status,
		Phase: a.Phase, ExitCode: a.ExitCode, ImageDigest: a.ImageDigest,
		StartedAt: utcPtr(a.StartedAt), FinishedAt: utcPtr(a.FinishedAt),
		LogRef: a.LogRef, ResultRef: a.ResultRef,
		Error:       store.JSONOrNil(a.Error),
		ExecutorRef: store.JSONOrNil(a.ExecutorRef),
	}
}

func toTesterDTO(s domain.TesterSpec) testerDTO {
	return testerDTO{
		Name: s.Name, Version: s.Version, DisplayName: s.DisplayName,
		Description: s.Description, Role: s.Role, OnFailure: s.OnFailure,
		Entry: s.Entry, Image: s.Image, Resources: s.Resources,
		Concurrency: s.Concurrency, Mounts: s.Mounts, Parameters: s.Parameters,
		Env: s.Env, Placeholder: s.Placeholder,
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
