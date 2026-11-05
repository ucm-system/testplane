package main

// 本文件只放 API 契约的"读侧"类型：字段名以 design/views/07-api.md 为准，
// 全部容忍未知字段（响应允许新增字段，客户端不得因未知字段失败）。
// 状态 token 一律小写（07 §1、02 §2）。

// Failure 是 Run / Task / Attempt 同构的失败结构（03 §4.7、07 §3）。
type Failure struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Detail    map[string]any `json:"detail,omitempty"`
	Retryable bool           `json:"retryable"`
}

// Endpoint 是 Run 详情里的 Endpoint 投影（07 §5.1）。
type Endpoint struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	ModelName string `json:"model_name"`
	ModelPath string `json:"model_path"`
}

// DeployInfo 是 Run 详情里的部署快照（07 §5.1、C-5.3）。
type DeployInfo struct {
	State         string `json:"state"`
	QueuePosition int    `json:"queue_position"`
	QueuedAt      string `json:"queued_at"`
	Message       string `json:"message"`
}

// Task 是 Run 详情里的 Task 摘要（07 §5.1）。
type Task struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Seq           int      `json:"seq"`
	Status        string   `json:"status"`
	StatusReason  string   `json:"status_reason"`
	WaitingReason string   `json:"waiting_reason"`
	Attempts      int      `json:"attempts"`
	Error         *Failure `json:"error"`
}

// Run 是 GET /runs/{id} 的响应体（字段集合是 07 §5.1 的超集）。
type Run struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Kind          string      `json:"kind"`
	Status        string      `json:"status"`
	SubmittedBy   string      `json:"submitted_by"`
	SubmittedAt   string      `json:"submitted_at"`
	FinishedAt    string      `json:"finished_at"`
	CleanupPolicy string      `json:"cleanup_policy"`
	Endpoint      *Endpoint   `json:"endpoint"`
	Deploy        *DeployInfo `json:"deploy"`
	Error         *Failure    `json:"error"`
	Tasks         []Task      `json:"tasks"`
}

// Attempt 是 Task 详情里的 Attempt（07 §5.2）。
type Attempt struct {
	ID         string   `json:"id"`
	Idx        int      `json:"idx"`
	Index      int      `json:"index"`
	TaskID     string   `json:"task_id"`
	Status     string   `json:"status"`
	ExitCode   *int     `json:"exit_code"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	Error      *Failure `json:"error"`
}

// Number 返回 1 起的 attempt 序号；不同实现可能用 idx 或 index，取非零者。
func (a Attempt) Number() int {
	if a.Idx > 0 {
		return a.Idx
	}
	return a.Index
}

// 终态集合（02 §2、C-3.2）。
var terminalRunStatuses = map[string]bool{
	"succeeded":             true,
	"completed_with_errors": true,
	"service_unavailable":   true,
	"failed":                true,
	"canceled":              true,
}

var terminalTaskStatuses = map[string]bool{
	"succeeded": true,
	"failed":    true,
	"canceled":  true,
	"skipped":   true,
}

var allRunStatuses = map[string]bool{
	"submitted": true, "queued": true, "deploying": true, "testing": true,
	"cleanup": true, "cancelling": true,
	"succeeded": true, "completed_with_errors": true, "service_unavailable": true,
	"failed": true, "canceled": true,
}

var allTaskStatuses = map[string]bool{
	"pending": true, "queued": true, "preparing": true, "running": true,
	"succeeded": true, "failed": true, "canceled": true, "skipped": true,
}

func isTerminalRun(status string) bool { return terminalRunStatuses[status] }
func isTerminalTask(status string) bool { return terminalTaskStatuses[status] }

// 设计允许的终态期望值（--expect）。
var knownExpectations = map[string]bool{
	"succeeded":             true,
	"completed_with_errors": true,
	"service_unavailable":   true,
	"failed":                true,
	"canceled":              true,
}
