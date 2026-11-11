package api

import (
	"encoding/json"
	"time"

	"github.com/ucm-system/testplane/server/internal/store"
)

// 本文件是"库行 → API 响应"的投影（v2 形状对齐 openapi schemas）。只挑对外字段，
// 不把 last_event_seq / revision 这类内部列泄出去（07 §5）。

// taskDTO 是 Task 列表项 / 详情的基础（openapi Task）。
type taskDTO struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Status         string          `json:"status"`
	SubmittedBy    string          `json:"submitted_by"`
	SubmittedAt    time.Time       `json:"submitted_at"`
	StartedAt      *time.Time      `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at"`
	Endpoint       json.RawMessage `json:"endpoint"`
	Deploy         json.RawMessage `json:"deploy"`
	Env            json.RawMessage `json:"env"`
	SecretEnv      json.RawMessage `json:"secret_env"`
	Error          json.RawMessage `json:"error"`
	RetryOf        *string         `json:"retry_of"`
	CurrentAttempt int             `json:"current_attempt"`
	RunnerCounts   *runnerCounts   `json:"runner_counts,omitempty"`
	Revision       int64           `json:"revision"`
}

// runnerCounts 是最新 attempt 的 runner 状态计数摘要（openapi Task.runner_counts）。
type runnerCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Canceled  int `json:"canceled"`
}

// attemptDTO 是 attempt 聚合行（openapi Attempt：Task 级计数与落档归组行，D-68）。
type attemptDTO struct {
	TaskID         string          `json:"task_id"`
	AttemptNo      int             `json:"attempt_no"`
	Status         string          `json:"status"`
	Trigger        string          `json:"trigger"`
	InputRef       *string         `json:"input_ref"`
	ExportsRef     *string         `json:"exports_ref"`
	DeployStateRef *string         `json:"deploy_state_ref"`
	Error          json.RawMessage `json:"error"`
	StartedAt      *time.Time      `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at"`
}

// runnerExecDTO 是 runner 执行记录投影（openapi RunnerExecution）。
type runnerExecDTO struct {
	TaskID            string          `json:"task_id"`
	AttemptNo         int             `json:"attempt_no"`
	Seq               int             `json:"seq"`
	Name              string          `json:"name"`
	RunnerName        string          `json:"runner_name"`
	RunnerVersion     string          `json:"runner_version"`
	Role              string          `json:"role"`
	ImageRef          string          `json:"image_ref"`
	ImageDigest       *string         `json:"image_digest"`
	Profile           string          `json:"profile"`
	InputRef          string          `json:"input_ref"`
	OnFailure         string          `json:"on_failure"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	Status            string          `json:"status"`
	StatusReason      *string         `json:"status_reason"`
	WaitingReason     *string         `json:"waiting_reason"`
	EndpointKey       *string         `json:"endpoint_key"`
	EndpointExclusive bool            `json:"endpoint_exclusive"`
	ExitCode          *int            `json:"exit_code"`
	ErrorCode         *string         `json:"error_code"`
	Error             json.RawMessage `json:"error"`
	ExportsRef        *string         `json:"exports_ref"`
	ResultRef         *string         `json:"result_ref"`
	LogRef            *string         `json:"log_ref"`
	StartedAt         *time.Time      `json:"started_at"`
	FinishedAt        *time.Time      `json:"finished_at"`
	ExecutorRef       json.RawMessage `json:"executor_ref"`
	CancelRequested   bool            `json:"cancel_requested"`
	Phase             *string         `json:"phase"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// taskDetailDTO 是 openapi TaskDetail：Task 字段平铺 + attempts + 最新 attempt 的 runners。
type taskDetailDTO struct {
	taskDTO
	Attempts []attemptDTO    `json:"attempts"`
	Runners  []runnerExecDTO `json:"runners"`
}

// runnerDTO 是 Runner 目录项（openapi Runner：runner.yaml 结构 + 注册表元数据）。
type runnerDTO struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Role        string          `json:"role"`
	Builtin     bool            `json:"builtin"`
	OnFailure   string          `json:"on_failure"`
	Entry       json.RawMessage `json:"entry"`
	InputFormat string          `json:"input_format"`
	Image       string          `json:"image"`
	Resources   json.RawMessage `json:"resources"`
	Concurrency json.RawMessage `json:"concurrency"`
	Mounts      json.RawMessage `json:"mounts"`
	Parameters  json.RawMessage `json:"parameters"`
	Exports     json.RawMessage `json:"exports"`
	Consumes    json.RawMessage `json:"consumes"`
	Env         json.RawMessage `json:"env"`
	Placeholder *string         `json:"placeholder"`
	CleanRunner *string         `json:"clean_runner"`

	// 注册表元数据（openapi Runner 的 allOf 第二支）。
	ContentSHA   string     `json:"content_sha"`
	SourceFile   string     `json:"source_file"`
	Orphaned     bool       `json:"orphaned"`
	Latest       bool       `json:"latest"`
	Retired      bool       `json:"retired"`
	RemovedAt    *time.Time `json:"removed_at"`
	RegisteredAt time.Time  `json:"registered_at"`
	RegisteredBy string     `json:"registered_by"`
}

// eventDTO 是 openapi Event 的投影：时间统一 UTC（`Z`），可空字段显式 null。
type eventDTO struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	TaskID    string          `json:"task_id"`
	AttemptNo *int            `json:"attempt_no"`
	RunnerSeq *int            `json:"runner_seq"`
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
		Seq: e.Seq, Type: e.Type, TaskID: e.TaskID,
		AttemptNo: e.AttemptNo, RunnerSeq: e.RunnerSeq,
		At: e.CreatedAt.UTC(), From: e.FromStatus, To: e.ToStatus,
		Reason: e.Reason, Actor: e.Actor, Payload: payload,
	}
}

// artifactDTO 是 openapi Artifact 的投影。sha256 惰性：没算过就是 null（不现算，C-7.5）。
type artifactDTO struct {
	Path      string  `json:"path"`
	Kind      string  `json:"kind"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    *string `json:"sha256"`
	AttemptNo *int    `json:"attempt_no"`
	RunnerSeq *int    `json:"runner_seq"`
}

// artifactListDTO 是 Task / runner 两级产物清单的共同信封（07 §5.1、§5.2）。
type artifactListDTO struct {
	Items          []artifactDTO            `json:"items"`
	ByKind         map[string][]artifactDTO `json:"by_kind"`
	TotalSizeBytes int64                    `json:"total_size_bytes"`
}

func toArtifactDTO(a store.Artifact) artifactDTO {
	return artifactDTO{
		Path: a.Path, Kind: a.Kind, SizeBytes: a.SizeBytes,
		SHA256: a.SHA256, AttemptNo: a.AttemptNo, RunnerSeq: a.RunnerSeq,
	}
}

func groupArtifacts(rows []store.Artifact) artifactListDTO {
	out := artifactListDTO{
		Items:  make([]artifactDTO, 0, len(rows)),
		ByKind: map[string][]artifactDTO{},
	}
	for _, a := range rows {
		dto := toArtifactDTO(a)
		out.Items = append(out.Items, dto)
		out.ByKind[dto.Kind] = append(out.ByKind[dto.Kind], dto)
		out.TotalSizeBytes += dto.SizeBytes
	}
	return out
}

// paged 是游标分页的统一信封（07 §6 / openapi {items, next_cursor, has_more}）。
type paged[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

// taskListDTO 是 Task 列表信封（D-61：游标与 offset 两种分页互斥）。
type taskListDTO struct {
	Items      []taskDTO `json:"items"`
	NextCursor *string   `json:"next_cursor"`
	HasMore    bool      `json:"has_more"`
	// 以下三个只在 offset 模式下非 null（游标模式为 null，避免 count 成本）。
	Total    *int `json:"total"`
	Page     *int `json:"page"`
	PageSize *int `json:"page_size"`
}

// cursorPtr 把空串转成 nil（has_more=false 时 next_cursor 为 null）。
func cursorPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- 投影函数 ------------------------------------------------------------

func toTaskDTO(t *store.Task) taskDTO {
	return taskDTO{
		ID: t.ID, Name: t.Name, Status: t.Status,
		SubmittedBy: t.SubmittedBy, SubmittedAt: t.SubmittedAt.UTC(),
		StartedAt: utcPtr(t.StartedAt), FinishedAt: utcPtr(t.FinishedAt),
		Endpoint:       store.JSONOrNil(t.Endpoint),
		Deploy:         store.JSONOrNil(t.Deploy),
		Env:            store.JSONOrNil(t.Env),
		SecretEnv:      store.JSONOrNil(t.SecretEnv),
		Error:          store.JSONOrNil(t.Error),
		RetryOf:        t.RetryOf,
		CurrentAttempt: t.CurrentAttempt,
		Revision:       t.Revision,
	}
}

func toAttemptDTO(a *store.Attempt) attemptDTO {
	return attemptDTO{
		TaskID: a.TaskID, AttemptNo: a.AttemptNo, Status: a.Status, Trigger: a.Trigger,
		InputRef: a.InputRef, ExportsRef: a.ExportsRef, DeployStateRef: a.DeployStateRef,
		Error:     store.JSONOrNil(a.Error),
		StartedAt: utcPtr(a.StartedAt), FinishedAt: utcPtr(a.FinishedAt),
	}
}

func toRunnerExecDTO(rr *store.RunnerRun) runnerExecDTO {
	return runnerExecDTO{
		TaskID: rr.TaskID, AttemptNo: rr.AttemptNo, Seq: rr.Seq,
		Name: rr.Name, RunnerName: rr.RunnerName, RunnerVersion: rr.RunnerVersion,
		Role: rr.Role, ImageRef: rr.ImageRef, ImageDigest: rr.ImageDigest,
		Profile: rr.Profile, InputRef: rr.InputRef,
		OnFailure: rr.OnFailure, TimeoutSeconds: rr.TimeoutSeconds,
		Status: rr.Status, StatusReason: rr.StatusReason, WaitingReason: rr.WaitingReason,
		EndpointKey: rr.EndpointKey, EndpointExclusive: rr.EndpointExclusive,
		ExitCode: rr.ExitCode, ErrorCode: rr.ErrorCode,
		Error:      store.JSONOrNil(rr.Error),
		ExportsRef: rr.ExportsRef, ResultRef: rr.ResultRef, LogRef: rr.LogRef,
		StartedAt: utcPtr(rr.StartedAt), FinishedAt: utcPtr(rr.FinishedAt),
		ExecutorRef:     store.JSONOrNil(rr.ExecutorRef),
		CancelRequested: rr.CancelRequested,
		Phase:           rr.Phase,
		CreatedAt:       rr.CreatedAt.UTC(),
		UpdatedAt:       rr.UpdatedAt.UTC(),
	}
}

func runnerExecDTOs(in []store.RunnerRun) []runnerExecDTO {
	out := make([]runnerExecDTO, 0, len(in))
	for i := range in {
		out = append(out, toRunnerExecDTO(&in[i]))
	}
	return out
}

func attemptDTOs(in []store.Attempt) []attemptDTO {
	out := make([]attemptDTO, 0, len(in))
	for i := range in {
		out = append(out, toAttemptDTO(&in[i]))
	}
	return out
}

// toRunnerDTO 把注册表行投影成 openapi Runner（jsonb 列原样透传，不二次解析）。
func toRunnerDTO(n *store.Runner) runnerDTO {
	return runnerDTO{
		Name: n.Name, Version: n.Version, DisplayName: n.DisplayName,
		Description: n.Description, Role: n.Role, Builtin: n.Builtin,
		OnFailure:   n.OnFailure,
		Entry:       jsonOrArray(n.Entry),
		InputFormat: n.InputFormat,
		Image:       n.Image,
		Resources:   jsonOrObject(n.Resources),
		Concurrency: jsonOrObject(n.Concurrency),
		Mounts:      jsonOrArray(n.Mounts),
		Parameters:  jsonOrArray(n.Parameters),
		Exports:     jsonOrArray(n.Exports),
		Consumes:    jsonOrArray(n.Consumes),
		Env:         jsonOrObject(n.EnvDecl),
		Placeholder: n.Placeholder,
		CleanRunner: n.CleanRunner,
		ContentSHA:  n.ContentSHA, SourceFile: n.SourceFile,
		Orphaned: n.Orphaned, Retired: n.Orphaned,
		RemovedAt:    utcPtr(n.RemovedAt),
		RegisteredAt: n.RegisteredAt.UTC(),
		RegisteredBy: n.RegisteredBy,
	}
}

func jsonOrArray(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`[]`)
	}
	return json.RawMessage(b)
}

func jsonOrObject(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
