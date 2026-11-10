package main

// 本文件只放 API 契约的"读侧"类型：字段名以 design/views/07-api.md（v2）与
// server/api/openapi.yaml 为准，全部容忍未知字段（响应允许新增字段，客户端不得因未知字段失败）。
// 状态 token 一律小写（07 §1、02 §2）。v2：Task 是一等对象（D-73），没有 /runs。

// Failure 是 Task / attempt / runner 同构的失败结构（03 §4.7、07 §3）。
type Failure struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Detail    map[string]any `json:"detail,omitempty"`
	Retryable bool           `json:"retryable"`
}

// Endpoint 是 Task 详情里的 Endpoint 投影（07 §5.1）。
type Endpoint struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	ModelName string `json:"model_name"`
	ModelPath string `json:"model_path"`
}

// DeployInfo 是 Task 详情里的部署快照摘要（07 §5.1、C-5.3）。
type DeployInfo struct {
	State         string `json:"state"`
	QueuePosition int    `json:"queue_position"`
	QueuedAt      string `json:"queued_at"`
	Message       string `json:"message"`
}

// RunnerCounts 是最新 attempt 的 runner 状态计数摘要（列表页与看板用）。
type RunnerCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Canceled  int `json:"canceled"`
}

// Attempt 是 Task 详情里的 attempt 聚合行（07 §5.2、D-68）：Task 的第 N 次全量执行。
type Attempt struct {
	TaskID    string   `json:"task_id"`
	AttemptNo int      `json:"attempt_no"`
	Status    string   `json:"status"`
	Trigger   string   `json:"trigger"`
	Error     *Failure `json:"error"`
}

// RunnerExecution 是 runner 执行记录投影（task_runners 行，D-68）：
// 定位键 (task_id, attempt_no, seq)，attempt 内按序执行恰好一次，没有自己的重试（D-69）。
type RunnerExecution struct {
	TaskID        string   `json:"task_id"`
	AttemptNo     int      `json:"attempt_no"`
	Seq           int      `json:"seq"`
	Name          string   `json:"name"`
	RunnerName    string   `json:"runner_name"`
	RunnerVersion string   `json:"runner_version"`
	Role          string   `json:"role"`
	Status        string   `json:"status"`
	StatusReason  string   `json:"status_reason"`
	WaitingReason string   `json:"waiting_reason"`
	ExitCode      *int     `json:"exit_code"`
	ErrorCode     string   `json:"error_code"`
	Error         *Failure `json:"error"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
}

// Task 是 GET /tasks/{id} 的响应体（TaskDetail：Task 投影 + attempts + 最新 attempt 的 runners）。
type Task struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Status         string            `json:"status"`
	SubmittedBy    string            `json:"submitted_by"`
	SubmittedAt    string            `json:"submitted_at"`
	FinishedAt     string            `json:"finished_at"`
	Endpoint       *Endpoint         `json:"endpoint"`
	Deploy         *DeployInfo       `json:"deploy"`
	Env            map[string]any    `json:"env"`
	Error          *Failure          `json:"error"`
	RetryOf        string            `json:"retry_of"`
	CurrentAttempt int               `json:"current_attempt"`
	RunnerCounts   *RunnerCounts     `json:"runner_counts"`
	Attempts       []Attempt         `json:"attempts"`
	Runners        []RunnerExecution `json:"runners"`
}

// Task 终态集合（02 §2.1、D-66）。v2 没有 completed_with_errors（并入 failed，D-73）。
var terminalTaskStatuses = map[string]bool{
	"succeeded":           true,
	"failed":              true,
	"service_unavailable": true,
	"canceled":            true,
}

var allTaskStatuses = map[string]bool{
	"submitted": true, "queued": true, "running": true,
	"succeeded": true, "failed": true, "service_unavailable": true,
	"cancelling": true, "canceled": true,
}

// runner 状态机（02 §2.2、D-68）：FAILED 是终态，没有重试回路（D-69）。
var allRunnerStatuses = map[string]bool{
	"pending": true, "queued": true, "preparing": true, "running": true,
	"succeeded": true, "failed": true, "skipped": true, "canceled": true,
}

var terminalRunnerStatuses = map[string]bool{
	"succeeded": true, "failed": true, "skipped": true, "canceled": true,
}

// attempt 聚合状态（03 §3.2）。
var allAttemptStatuses = map[string]bool{
	"running": true, "succeeded": true, "failed": true, "canceled": true,
}

func isTerminalTask(status string) bool   { return terminalTaskStatuses[status] }
func isTerminalRunner(status string) bool { return terminalRunnerStatuses[status] }

// skippedReasons 是 SKIPPED 必带的原因（D-06、task_runners_skipped_reason_ck）。
var skippedReasons = map[string]bool{
	"upstream_abort": true,
	"canceled":       true,
}

// 设计允许的终态期望值（--expect）。v2 值域：
// succeeded | failed | service_unavailable | canceled。
var knownExpectations = map[string]bool{
	"succeeded":           true,
	"failed":              true,
	"service_unavailable": true,
	"canceled":            true,
}
