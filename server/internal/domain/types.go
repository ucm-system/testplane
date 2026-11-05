// Package domain 是 TestPlane 的领域层：只放类型、常量、转移请求与哨兵错误。
//
// 依赖规则（design/views/05-development-view.md §3.1）：
//   - domain 不 import 任何其它内部包；
//   - 不 import pgx / net/http / client-go —— 领域层不认识数据库、HTTP 与 K8s。
//
// 状态集合以 design/views/02-logical-view.md §2 与 design/views/03-data-view.md §3.2 的
// CHECK 约束为准，三处必须同时改。
package domain

import (
	"errors"
	"time"
)

// ---- 状态枚举 ------------------------------------------------------------
//
// 每个实体一套强类型，加一个通用的 Status 供 Transition 的 From 集合使用：
// 转移请求要能表达"允许的前置状态集合"，那是跨实体同构的，不值得为每个实体写一遍。

type Status string

type RunStatus string
type TaskStatus string
type AttemptStatus string
type TesterState string

// Run 状态。终态：succeeded / completed_with_errors / service_unavailable / failed / canceled。
const (
	RunSubmitted           RunStatus = "submitted"
	RunQueued              RunStatus = "queued"
	RunDeploying           RunStatus = "deploying"
	RunTesting             RunStatus = "testing"
	RunCleanup             RunStatus = "cleanup"
	RunCancelling          RunStatus = "cancelling"
	RunSucceeded           RunStatus = "succeeded"
	RunCompletedWithErrors RunStatus = "completed_with_errors"
	RunServiceUnavailable  RunStatus = "service_unavailable"
	RunFailed              RunStatus = "failed"
	RunCanceled            RunStatus = "canceled"
)

// Task 状态。终态：succeeded / failed / canceled / skipped。
const (
	TaskPending   TaskStatus = "pending"
	TaskQueued    TaskStatus = "queued"
	TaskPreparing TaskStatus = "preparing"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
	TaskSkipped   TaskStatus = "skipped"
)

// Attempt（= Execution）状态。
const (
	AttemptPreparing AttemptStatus = "preparing"
	AttemptRunning   AttemptStatus = "running"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptCanceled  AttemptStatus = "canceled"
)

// Tester 状态（注册表状态，不是运行时状态，见 02 §2.4）。
const (
	TesterScanned    TesterState = "scanned"
	TesterValid      TesterState = "valid"
	TesterRejected   TesterState = "rejected"
	TesterRegistered TesterState = "registered"
	TesterOrphaned   TesterState = "orphaned"
)

// Task 的等待原因（tasks.waiting_reason CHECK 的六个取值）。
const (
	WaitPrecheckGate          = "precheck_gate"
	WaitSlotWaiting           = "slot_waiting"
	WaitGlobalLimit           = "global_limit"
	WaitQueueFull             = "queue_full"
	WaitInsufficientResources = "insufficient_resources"
	WaitGangPending           = "gang_pending"
)

// Task 被跳过时的原因（tasks.status_reason CHECK 的五个取值）。
const (
	SkipUpstreamAbort      = "upstream_abort"
	SkipCanceled           = "canceled"
	SkipDeployFailed       = "deploy_failed"
	SkipServiceUnavailable = "service_unavailable"
	SkipPlatformError      = "platform_error"
)

// Tester 角色与失败策略。
const (
	RoleTest     = "test"
	RolePreCheck = "pre_check"

	OnFailureContinue = "continue"
	OnFailureAbortRun = "abort_run"
)

// EntityType 标识转移作用于哪一种实体。
type EntityType string

const (
	EntityRun     EntityType = "run"
	EntityTask    EntityType = "task"
	EntityAttempt EntityType = "attempt"
	EntityTester  EntityType = "tester"
)

// ---- 转移请求 ------------------------------------------------------------

// Transition 是一次状态变更的完整描述；由调用方构造，Kernel 负责校验与落库。
// 字段与 design/views/02-logical-view.md §3.2 一一对应。
type Transition struct {
	Entity  EntityType     // run | task | attempt | tester
	ID      string         // run_id / task_id / attempt_id
	From    []Status       // 允许的前置状态集合（CAS 条件）；空表示不校验
	To      Status         // 目标状态
	Command string         // enqueue / deploy_ready / succeed / cancel ...
	Reason  string         // 写入 status_reason 或事件 reason
	Actor   string         // user:<id> | system:<component>
	Payload map[string]any // 事件附加信息（不含凭据）
	At      time.Time      // 事务时间；零值由 Kernel 取 now()
}

// Applied 是 Kernel.Apply 的结果。
type Applied struct {
	From Status
	To   Status
	Seq  int64 // 该 Run 的事件序号
	Noop bool  // 已是目标状态，没有写事件
}

// ---- 失败与错误 ----------------------------------------------------------

// Failure 是 Run / Task / Attempt 同构的失败结构（03 §4.7）。
type Failure struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Detail    map[string]any `json:"detail,omitempty"`
	Retryable bool           `json:"retryable"`
}

// Kernel / 门面层的哨兵错误（02 §3.6）。错误包装用 %w，判定用 errors.Is。
var (
	ErrConflict     = errors.New("state conflict")      // 并发修改或前置状态已变
	ErrIllegalState = errors.New("illegal transition")  // 转移表里没有这条边
	ErrPrecondition = errors.New("precondition failed") // 守卫不通过
	ErrNotFound     = errors.New("not found")           // 实体不存在
	ErrTx           = errors.New("transaction failed")  // DB 事务/连接失败
)

// ---- 端点与公共参数 ------------------------------------------------------

// Endpoint 是被测服务的接入点。url / model_name / model_path 是公共参数保留字
// （06 §5.4.1），平台注入到 Tester 的 argv 末尾。
type Endpoint struct {
	Key       string `json:"key"`
	BaseURL   string `json:"url"`                  // OpenAI 兼容 base URL，形如 http://host:8000/v1
	ModelName string `json:"model_name,omitempty"` // 服务端模型名
	ModelPath string `json:"model_path,omitempty"` // 本地权重路径，可为空
}

// ---- Tester 规格（tester.yaml 的领域表示）--------------------------------

// TesterSpec 是 tester.yaml 解析后的不可变元数据。镜像内自带，平台只读不写（K-3）。
type TesterSpec struct {
	Name        string            `json:"name" yaml:"name"`
	Version     string            `json:"version" yaml:"version"`
	DisplayName string            `json:"display_name,omitempty" yaml:"display_name"`
	Description string            `json:"description,omitempty" yaml:"description"`
	Role        string            `json:"role" yaml:"role"`             // test | pre_check
	OnFailure   string            `json:"on_failure" yaml:"on_failure"` // continue | abort_run
	Entry       string            `json:"entry" yaml:"entry"`           // 容器内入口，如 /tester/run.sh
	Image       string            `json:"image" yaml:"image"`           // 镜像引用，tag 或 digest
	Resources   ResourceRequest   `json:"resources" yaml:"resources"`
	Concurrency ConcurrencyPolicy `json:"concurrency,omitempty" yaml:"concurrency"`
	Mounts      []MountSpec       `json:"mounts,omitempty" yaml:"mounts"`
	Parameters  map[string]Param  `json:"parameters,omitempty" yaml:"parameters"`
	Placeholder string            `json:"placeholder,omitempty" yaml:"placeholder"`
	Env         map[string]EnvVar `json:"env,omitempty" yaml:"env"`
}

// ResourceRequest 是单次 Attempt 的资源需求。
type ResourceRequest struct {
	CPU    string     `json:"cpu" yaml:"cpu"`       // K8s quantity 字符串，如 "1"
	Memory string     `json:"memory" yaml:"memory"` // K8s quantity 字符串，如 "256Mi"
	GPU    GPURequest `json:"gpu,omitempty" yaml:"gpu"`
}

// GPURequest 是 GPU 资源需求；Count 为 0 表示不需要 GPU。
type GPURequest struct {
	Resource string `json:"resource,omitempty" yaml:"resource"` // 如 nvidia.com/gpu
	Count    int    `json:"count" yaml:"count"`
}

// ConcurrencyPolicy 是 Tester 维护者对并发的显式声明（K-12）：
// 平台提供并行能力，是否独占 Endpoint 由 Tester 决定。
type ConcurrencyPolicy struct {
	EndpointExclusive bool `json:"endpoint_exclusive,omitempty" yaml:"endpoint_exclusive"`
}

// MountSpec 是 Tester 声明的额外挂载。
type MountSpec struct {
	Name      string `json:"name" yaml:"name"`
	MountPath string `json:"mount_path" yaml:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty" yaml:"read_only"`
	Source    string `json:"source,omitempty" yaml:"source"`
}

// Param 是参数清单里的一项，仅供 Agent 与人类阅读；平台不渲染、不校验（05 §5.4）。
type Param struct {
	Type        string `json:"type,omitempty" yaml:"type"`
	Required    bool   `json:"required,omitempty" yaml:"required"`
	Default     string `json:"default,omitempty" yaml:"default"`
	Description string `json:"description,omitempty" yaml:"description"`
}

// EnvVar 是运行期环境变量的声明（值由 Run 级 secrets 注入，平台只声明不保存明文）。
type EnvVar struct {
	Required    bool   `json:"required,omitempty" yaml:"required"`
	Secret      bool   `json:"secret,omitempty" yaml:"secret"`
	Description string `json:"description,omitempty" yaml:"description"`
}

// Key 返回注册表主键：name+version。
func (t TesterSpec) Key() string { return t.Name + "@" + t.Version }

// IsTerminal 报告该状态是否终态。
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunSucceeded, RunCompletedWithErrors, RunServiceUnavailable, RunFailed, RunCanceled:
		return true
	}
	return false
}

// IsTerminal 报告该状态是否终态。
func (s TaskStatus) IsTerminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCanceled, TaskSkipped:
		return true
	}
	return false
}

// IsActive 报告该状态是否占用端点槽位。
func (s TaskStatus) IsActive() bool { return s == TaskPreparing || s == TaskRunning }

// Status 把强类型状态降级为转移请求里的通用状态。
func (s RunStatus) Status() Status     { return Status(s) }
func (s TaskStatus) Status() Status    { return Status(s) }
func (s AttemptStatus) Status() Status { return Status(s) }
func (s TesterState) Status() Status   { return Status(s) }
