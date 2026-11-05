package store

import (
	"encoding/json"
	"time"
)

// 本文件是表行的直接投影（03 §3.1）。列名与顺序在本包内固定，
// 所有 SELECT 都显式列名，不用 `SELECT *`（03 §5 查询纪律）。

// Run 是 runs 表的一行。
type Run struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Manifest      []byte     `json:"manifest"`
	Resolved      []byte     `json:"resolved"`
	Endpoint      []byte     `json:"endpoint,omitempty"`
	Deploy        []byte     `json:"deploy,omitempty"`
	CleanupPolicy string     `json:"cleanup_policy"`
	SubmittedBy   string     `json:"submitted_by"`
	SubmittedAt   time.Time  `json:"submitted_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Error         []byte     `json:"error,omitempty"`
	RetryOf       *string    `json:"retry_of,omitempty"`
	LastEventSeq  int64      `json:"last_event_seq"`
	Revision      int64      `json:"revision"`
}

// Task 是 tasks 表的一行。
type Task struct {
	ID                string     `json:"id"`
	RunID             string     `json:"run_id"`
	Seq               int        `json:"seq"`
	Name              string     `json:"name"`
	TesterName        string     `json:"tester_name"`
	TesterVersion     string     `json:"tester_version"`
	ImageRef          string     `json:"image_ref"`
	Profile           string     `json:"profile"`
	ArgsRaw           string     `json:"args_raw"`
	ArgsArgv          []byte     `json:"args_argv"`
	InjectedArgv      []byte     `json:"injected_argv"`
	Env               []byte     `json:"env"`
	SecretEnv         []byte     `json:"secret_env"`
	OnFailure         string     `json:"on_failure"`
	TimeoutSeconds    int        `json:"timeout_seconds"`
	RetryPolicy       []byte     `json:"retry_policy"`
	Status            string     `json:"status"`
	StatusReason      *string    `json:"status_reason,omitempty"`
	WaitingReason     *string    `json:"waiting_reason,omitempty"`
	EndpointKey       *string    `json:"endpoint_key,omitempty"`
	EndpointExclusive bool       `json:"endpoint_exclusive"`
	CurrentAttempt    int        `json:"current_attempt"`
	AvailableAt       *time.Time `json:"available_at,omitempty"`
	CancelRequested   bool       `json:"cancel_requested"`
	ResultRef         *string    `json:"result_ref,omitempty"`
	ReusedFromTaskID  *string    `json:"reused_from_task_id,omitempty"`
	Error             []byte     `json:"error,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Attempt 是 attempts 表的一行。
type Attempt struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"task_id"`
	Idx          int        `json:"idx"`
	Status       string     `json:"status"`
	JobID        string     `json:"job_id"`
	JobNamespace *string    `json:"job_namespace,omitempty"`
	Phase        *string    `json:"phase,omitempty"`
	ImageDigest  *string    `json:"image_digest,omitempty"`
	ExitCode     *int32     `json:"exit_code,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	LogRef       *string    `json:"log_ref,omitempty"`
	ArtifactRef  *string    `json:"artifact_ref,omitempty"`
	ResultRef    *string    `json:"result_ref,omitempty"`
	Error        []byte     `json:"error,omitempty"`
	ExecutorRef  []byte     `json:"executor_ref,omitempty"`
}

// Event 是 state_events 表的一行（append-only 事实流）。
type Event struct {
	ID         int64           `json:"-"`
	RunID      string          `json:"run_id"`
	TaskID     *string         `json:"task_id"`
	AttemptID  *string         `json:"attempt_id"`
	Seq        int64           `json:"seq"`
	Type       string          `json:"type"`
	FromStatus *string         `json:"from"`
	ToStatus   *string         `json:"to"`
	Reason     *string         `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
	Actor      *string         `json:"actor"`
	CreatedAt  time.Time       `json:"at"`
}

// Lease 是 endpoint_leases 表的一行。
type Lease struct {
	TaskID      string    `json:"task_id"`
	EndpointKey string    `json:"endpoint_key"`
	Exclusive   bool      `json:"exclusive"`
	AcquiredAt  time.Time `json:"acquired_at"`
}

// Tester 是 testers 表的一行。
type Tester struct {
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	DisplayName  string     `json:"display_name"`
	Description  string     `json:"description"`
	Role         string     `json:"role"`
	OnFailure    string     `json:"on_failure"`
	Entry        string     `json:"entry"`
	Image        string     `json:"image"`
	Resources    []byte     `json:"resources"`
	Concurrency  []byte     `json:"concurrency"`
	Mounts       []byte     `json:"mounts"`
	K8s          []byte     `json:"k8s"`
	Parameters   []byte     `json:"parameters"`
	EnvDecl      []byte     `json:"env_decl"`
	Placeholder  *string    `json:"placeholder,omitempty"`
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
