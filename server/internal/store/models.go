package store

import (
	"encoding/json"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 本文件是表行的直接投影（03 §3.1）。列名与顺序在本包内固定，
// 所有 SELECT 都显式列名，不用 `SELECT *`（03 §5 查询纪律）。
//
// v2 模型（D-66～D-68）：tasks（一次评测任务）/ task_attempts（Task 级 attempt 计数行）
// / task_runners（runner 执行记录，(task_id, attempt_no, seq) 唯一定位）/ runners（注册表）。

// Task 是 tasks 表的一行（v1 的 runs，D-66）。
type Task struct {
	ID              string     `json:"id"` // t-YYYYMMDD-XXXX
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	Manifest        []byte     `json:"manifest"`
	Resolved        []byte     `json:"resolved"`
	Endpoint        []byte     `json:"endpoint,omitempty"`
	Deploy          []byte     `json:"deploy,omitempty"`
	Env             []byte     `json:"env"`
	SecretEnv       []byte     `json:"secret_env"`
	SubmittedBy     string     `json:"submitted_by"`
	SubmittedAt     time.Time  `json:"submitted_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Error           []byte     `json:"error,omitempty"`
	RetryOf         *string    `json:"retry_of,omitempty"`
	CurrentAttempt  int        `json:"current_attempt"` // 0 = 尚未开始
	CancelRequested bool       `json:"cancel_requested"`
	LastEventSeq    int64      `json:"last_event_seq"`
	Revision        int64      `json:"revision"`
}

// Attempt 是 task_attempts 表的一行（Task 级 attempt 计数与落档归组行，D-68）。
type Attempt struct {
	TaskID         string     `json:"task_id"`
	AttemptNo      int        `json:"attempt_no"`
	Status         string     `json:"status"` // running|succeeded|failed|canceled
	Trigger        string     `json:"trigger"`
	InputRef       *string    `json:"input_ref,omitempty"`
	ExportsRef     *string    `json:"exports_ref,omitempty"`
	DeployStateRef *string    `json:"deploy_state_ref,omitempty"`
	Error          []byte     `json:"error,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

// RunnerRun 是 task_runners 表的一行（runner 执行记录；v1 的内层 tasks + 物理执行列，D-68）。
type RunnerRun struct {
	TaskID            string     `json:"task_id"`
	AttemptNo         int        `json:"attempt_no"`
	Seq               int        `json:"seq"`
	Name              string     `json:"name"`
	RunnerName        string     `json:"runner_name"`
	RunnerVersion     string     `json:"runner_version"`
	Role              string     `json:"role"`
	ImageRef          string     `json:"image_ref"`
	ImageDigest       *string    `json:"image_digest,omitempty"`
	Profile           string     `json:"profile"`
	InputRef          string     `json:"input_ref"`
	OnFailure         string     `json:"on_failure"`
	TimeoutSeconds    int        `json:"timeout_seconds"`
	Status            string     `json:"status"`
	StatusReason      *string    `json:"status_reason,omitempty"`
	WaitingReason     *string    `json:"waiting_reason,omitempty"`
	EndpointKey       *string    `json:"endpoint_key,omitempty"`
	EndpointExclusive bool       `json:"endpoint_exclusive"`
	ExitCode          *int       `json:"exit_code,omitempty"`
	ErrorCode         *string    `json:"error_code,omitempty"`
	Error             []byte     `json:"error,omitempty"`
	ExportsRef        *string    `json:"exports_ref,omitempty"`
	ResultRef         *string    `json:"result_ref,omitempty"`
	LogRef            *string    `json:"log_ref,omitempty"`
	JobID             string     `json:"job_id"`
	JobNamespace      *string    `json:"job_namespace,omitempty"`
	Phase             *string    `json:"phase,omitempty"`
	ExecutorRef       []byte     `json:"executor_ref,omitempty"`
	CancelRequested   bool       `json:"cancel_requested"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Key 返回执行记录定位三元组（D-68）。
func (r RunnerRun) Key() domain.RunnerKey {
	return domain.RunnerKey{TaskID: r.TaskID, AttemptNo: r.AttemptNo, Seq: r.Seq}
}

// Event 是 state_events 表的一行（append-only 事实流）。
type Event struct {
	ID         int64           `json:"-"`
	TaskID     string          `json:"task_id"`
	AttemptNo  *int            `json:"attempt_no"`
	RunnerSeq  *int            `json:"runner_seq"`
	Seq        int64           `json:"seq"`
	Type       string          `json:"type"`
	FromStatus *string         `json:"from"`
	ToStatus   *string         `json:"to"`
	Reason     *string         `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
	Actor      *string         `json:"actor"`
	CreatedAt  time.Time       `json:"at"`
}

// Lease 是 endpoint_leases 表的一行（外接模式互斥槽位；定位 = 执行记录三元组）。
type Lease struct {
	TaskID      string    `json:"task_id"`
	AttemptNo   int       `json:"attempt_no"`
	RunnerSeq   int       `json:"runner_seq"`
	EndpointKey string    `json:"endpoint_key"`
	Exclusive   bool      `json:"exclusive"`
	AcquiredAt  time.Time `json:"acquired_at"`
}

// Runner 是 runners 表的一行（Runner 注册表；v1 testers 表，D-66）。
type Runner struct {
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	DisplayName  string     `json:"display_name"`
	Description  string     `json:"description"`
	Role         string     `json:"role"`
	Builtin      bool       `json:"builtin"`
	OnFailure    string     `json:"on_failure"`
	Entry        []byte     `json:"entry"` // exec form 数组（jsonb）
	InputFormat  string     `json:"input_format"`
	Image        string     `json:"image"`
	Resources    []byte     `json:"resources"`
	Concurrency  []byte     `json:"concurrency"`
	Mounts       []byte     `json:"mounts"`
	K8s          []byte     `json:"k8s"`
	Parameters   []byte     `json:"parameters"`
	Exports      []byte     `json:"exports"`
	Consumes     []byte     `json:"consumes"`
	EnvDecl      []byte     `json:"env_decl"`
	Placeholder  *string    `json:"placeholder,omitempty"`
	CleanRunner  *string    `json:"clean_runner,omitempty"` // deploy 声明式 clean 配对（D-70）；空 = 无 clean 阶段
	SourceFile   string     `json:"source_file"`
	ContentSHA   string     `json:"content_sha"`
	Orphaned     bool       `json:"orphaned"`
	RemovedAt    *time.Time `json:"removed_at,omitempty"`
	RegisteredAt time.Time  `json:"registered_at"`
	RegisteredBy string     `json:"registered_by"`
}

// Cluster 是 clusters 表的一行（不回显凭据内容）。
type Cluster struct {
	Name            string     `json:"name"`
	APIServer       string     `json:"api_server"`
	KubeconfigKey   string     `json:"kubeconfig_key"`
	Description     *string    `json:"description,omitempty"`
	LastCheckAt     *time.Time `json:"last_check_at,omitempty"`
	LastCheckResult *string    `json:"last_check_result,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// AuditEntry 是 audit_log 的写入参数。
type AuditEntry struct {
	Actor      string
	Role       string
	Action     string
	ObjectType string
	ObjectID   string
	Detail     map[string]any
}

// JSONOrNil 把 nil/空字节切片转成 nil，便于序列化成 null。
func JSONOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// DerefStr 是 *string → string 的便捷取值。
func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
