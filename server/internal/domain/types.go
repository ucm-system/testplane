// Package domain 是 Argus 的领域层：只放类型、常量、转移请求与哨兵错误。
//
// 依赖规则（design/views/05-development-view.md §3.1）：
//   - domain 不 import 任何其它内部包；
//   - 不 import pgx / net/http / client-go —— 领域层不认识数据库、HTTP 与 K8s。
//
// 状态集合以 design/views/02-logical-view.md §2 与 design/views/03-data-view.md §3.2 的
// CHECK 约束为准，三处必须同时改。
//
// v2 术语（ARCHITECTURE §0.2 / D-66～D-68）：Run→Task、Task（Run 内步骤）→runner（执行记录）、
// Tester→Runner；runner 执行记录由 (task_id, attempt_no, seq) 三元组定位。
package domain

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ---- 状态枚举 ------------------------------------------------------------
//
// Task（平台层）与 runner（实例层）各一套强类型，加一个通用的 Status 供
// Transition 的 From 集合使用：转移请求要能表达"允许的前置状态集合"。

type Status string

type TaskStatus string   // tasks.status
type RunnerStatus string // task_runners.status（runner 执行记录）
type AttemptStatus string
type RunnerState string // runners 注册表状态（注册期，不是运行时状态）

// Task 状态。8 值（03-data-view §3.2；v2 无 deploying/testing/cleanup/completed_with_errors）。
// 终态：succeeded / failed / service_unavailable / canceled。
const (
	TaskSubmitted          TaskStatus = "submitted"
	TaskQueued             TaskStatus = "queued"
	TaskRunning            TaskStatus = "running"
	TaskCancelling         TaskStatus = "cancelling"
	TaskSucceeded          TaskStatus = "succeeded"
	TaskFailed             TaskStatus = "failed"
	TaskServiceUnavailable TaskStatus = "service_unavailable"
	TaskCanceled           TaskStatus = "canceled"
)

// runner 执行记录状态。8 值（02 §2、C-3.1）；终态：succeeded / failed / skipped / canceled。
// **没有重试回路**（D-69）：failed 是终态，重试 = 整 Task 新 attempt。
const (
	RunnerPending   RunnerStatus = "pending"
	RunnerQueued    RunnerStatus = "queued"
	RunnerPreparing RunnerStatus = "preparing"
	RunnerRunning   RunnerStatus = "running"
	RunnerSucceeded RunnerStatus = "succeeded"
	RunnerFailed    RunnerStatus = "failed"
	RunnerSkipped   RunnerStatus = "skipped"
	RunnerCanceled  RunnerStatus = "canceled"
)

// attempt（Task 级计数行）状态。终态不覆盖（03-data-view §3.1）。
const (
	AttemptRunning   AttemptStatus = "running"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptCanceled  AttemptStatus = "canceled"
)

// Runner 注册表状态（启动期扫描/校验，不是运行时状态）。
const (
	RunnerScanned    RunnerState = "scanned"
	RunnerValid      RunnerState = "valid"
	RunnerRejected   RunnerState = "rejected"
	RunnerRegistered RunnerState = "registered"
	RunnerOrphaned   RunnerState = "orphaned"
)

// runner 执行记录的等待原因（task_runners.waiting_reason；§6.9 QUEUED 必带原因；
// C-4.7：slot_waiting/global_limit 是平台自身等待，queue_full/insufficient_resources/
// gang_pending 是 K8s/Volcano 侧等待——分开呈现，用户才知道该找谁）。
const (
	WaitSlotWaiting           = "slot_waiting"
	WaitGlobalLimit           = "global_limit"
	WaitQueueFull             = "queue_full"
	WaitInsufficientResources = "insufficient_resources"
	WaitGangPending           = "gang_pending"
)

// runner 被跳过时的原因（task_runners.status_reason CHECK 的两个取值；D-06）。
const (
	SkipUpstreamAbort = "upstream_abort"
	SkipCanceled      = "canceled"
)

// Runner 角色与失败策略（附录 C v2；deploy/clean 仅内置，§5.1）。
const (
	RoleTest     = "test"
	RolePreCheck = "pre_check"
	RoleDeploy   = "deploy"
	RoleClean    = "clean"

	OnFailureContinue  = "continue"
	OnFailureAbortTask = "abort_task"
)

// Roles 是 role 的全集（注册校验与提交校验共用）。
var Roles = []string{RoleTest, RolePreCheck, RoleDeploy, RoleClean}

// ValidRole 报告 role 是否合法四值之一。
func ValidRole(role string) bool {
	switch role {
	case RoleTest, RolePreCheck, RoleDeploy, RoleClean:
		return true
	}
	return false
}

// InputFormat 是 Runner 输入格式（附录 C v2 / D-67）：决定平台校验深度与输入文件名。
const (
	InputFormatJSONL = "jsonl"
	InputFormatYAML  = "yaml"
	InputFormatText  = "text"
)

// ValidInputFormat 报告 input_format 是否三值之一。
func ValidInputFormat(f string) bool {
	switch f {
	case InputFormatJSONL, InputFormatYAML, InputFormatText:
		return true
	}
	return false
}

// EntityType 标识转移作用于哪一种实体（02 §3.2：task | task_runner）。
type EntityType string

const (
	EntityTask      EntityType = "task"
	EntityRunnerRun EntityType = "task_runner"
)

// ---- RunnerKey（执行记录定位三元组，D-68）---------------------------------

// RunnerKey 定位一次 runner 执行：v2 取代 v1 的 attempt id / task id 二元定位。
type RunnerKey struct {
	TaskID    string
	AttemptNo int // 从 1 递增
	Seq       int // 从 1 递增（含注入的 service-deploy / precheck / service-clean）
}

// Valid 报告 key 是否可定位（attempt/seq 从 1 起）。
func (k RunnerKey) Valid() bool { return k.TaskID != "" && k.AttemptNo >= 1 && k.Seq >= 1 }

// ID 返回转移请求用的字符串形态（task:attempt:seq）。
func (k RunnerKey) ID() string {
	return strconv.Itoa(k.AttemptNo) + ":" + strconv.Itoa(k.Seq) + "@" + k.TaskID
}

// ParseRunnerKey 解析 RunnerKey.ID() 的字符串形态；解析不出返回 ok=false。
func ParseRunnerKey(id string) (RunnerKey, bool) {
	at := strings.LastIndexByte(id, '@')
	if at <= 0 {
		return RunnerKey{}, false
	}
	parts := strings.SplitN(id[:at], ":", 2)
	if len(parts) != 2 {
		return RunnerKey{}, false
	}
	no, err1 := strconv.Atoi(parts[0])
	seq, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || no < 1 || seq < 1 {
		return RunnerKey{}, false
	}
	return RunnerKey{TaskID: id[at+1:], AttemptNo: no, Seq: seq}, true
}

// ---- 转移请求 ------------------------------------------------------------

// Transition 是一次状态变更的完整描述；由调用方构造，Kernel 负责校验与落库。
// 字段与 design/views/02-logical-view.md §3.2 一一对应。
type Transition struct {
	Entity  EntityType     // task | task_runner
	ID      string         // task_id / RunnerKey.ID()
	From    []Status       // 允许的前置状态集合（CAS 条件）；空表示不校验
	To      Status         // 目标状态
	Command string         // enqueue / pipeline_start / succeed / cancel ...
	Reason  string         // 写入 status_reason 或事件 reason
	Actor   string         // user:<id> | system:<component>
	Payload map[string]any // 事件附加信息（不含凭据）
	At      time.Time      // 事务时间；零值由 Kernel 取 now()
}

// Applied 是 Kernel.Apply 的结果。
type Applied struct {
	From Status
	To   Status
	Seq  int64 // 该 Task 的事件序号
	Noop bool  // 已是目标状态，没有写事件
}

// ---- 失败与错误 ----------------------------------------------------------

// Failure 是 Task / attempt / runner 同构的失败结构（03 §4.7）。
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

// Endpoint 是被测服务的接入点。来源只有两处（deploy exports 或静态 service，
// D-27）；url / model_name / model_path 是公共参数保留字（§5.4.1），以
// AG_SERVED_* 环境变量注入（D-66），Runner 不可见 Endpoint 概念本身。
type Endpoint struct {
	Key       string `json:"key"`
	BaseURL   string `json:"url"`                  // OpenAI 兼容 base URL，形如 http://host:8000/v1
	ModelName string `json:"model_name,omitempty"` // 服务端模型名
	ModelPath string `json:"model_path,omitempty"` // 本地权重路径，可为空
	DeployRef string `json:"deploy_ref,omitempty"` // AG_DEPLOY_REF（deploy 导出，仅 clean 消费）
	Source    string `json:"source,omitempty"`     // 端点来源：genet（deploy 导出）| service（静态）
}

// ---- Runner 规格（runner.yaml 的领域表示，附录 C v2）----------------------

// RunnerSpec 是 runner.yaml 解析后的不可变元数据。镜像内自带，平台只读不写（K-3）。
type RunnerSpec struct {
	Name        string            `json:"name" yaml:"name"`
	Version     string            `json:"version" yaml:"version"`
	DisplayName string            `json:"display_name,omitempty" yaml:"display_name"`
	Description string            `json:"description,omitempty" yaml:"description"`
	Role        string            `json:"role" yaml:"role"` // test | pre_check | deploy | clean
	OnFailure   string            `json:"on_failure" yaml:"on_failure"`
	Entry       []string          `json:"entry" yaml:"entry"`               // exec form 数组（§5.2）
	InputFormat string            `json:"input_format" yaml:"input_format"` // jsonl | yaml | text
	Image       string            `json:"image" yaml:"image"`               // 镜像引用，tag 或 digest
	Resources   ResourceRequest   `json:"resources" yaml:"resources"`
	Concurrency ConcurrencyPolicy `json:"concurrency,omitempty" yaml:"concurrency"`
	Mounts      []MountSpec       `json:"mounts,omitempty" yaml:"mounts"`
	Parameters  []Param           `json:"parameters,omitempty" yaml:"parameters"` // 仅 jsonl；yaml/text 必须 []
	Exports     []string          `json:"exports,omitempty" yaml:"exports"`       // 本 Runner 产出的下游 key（§5.12）
	Consumes    []string          `json:"consumes,omitempty" yaml:"consumes"`     // 需要的上游 key（组合期校验闭合）
	Placeholder string            `json:"placeholder,omitempty" yaml:"placeholder"`
	Env         map[string]EnvVar `json:"env,omitempty" yaml:"env"`
}

// Key 返回注册表主键：name+version。
func (r RunnerSpec) Key() string { return r.Name + "@" + r.Version }

// EntryString 返回 entry 数组的 shell 无关展示形态（日志与报告用）。
func (r RunnerSpec) EntryString() string { return strings.Join(r.Entry, " ") }

// ResourceRequest 是单次执行的资源需求。
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

// ConcurrencyPolicy 是 Runner 维护者对并发的显式声明（§6.7 v2 收窄）：
// endpoint_exclusive 仅外接模式有效（部署模式声明为冗余但无害）。
type ConcurrencyPolicy struct {
	EndpointExclusive bool `json:"endpoint_exclusive,omitempty" yaml:"endpoint_exclusive"`
}

// MountSpec 是 Runner 声明的额外挂载。
type MountSpec struct {
	Name      string `json:"name" yaml:"name"`
	MountPath string `json:"mount_path" yaml:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty" yaml:"read_only"`
	Source    string `json:"source,omitempty" yaml:"source"` // PVC 名，缺省用平台 storage PVC
}

// ParamTypes 是 parameters[].type 的合法取值（§5.4：平台只校验类型与 required 存在性）。
var ParamTypes = []string{"string", "number", "integer", "boolean", "object", "array"}

// ValidParamType 报告参数类型是否合法。
func ValidParamType(t string) bool {
	switch t {
	case "string", "number", "integer", "boolean", "object", "array":
		return true
	}
	return false
}

// Param 是参数清单里的一项。平台按它对 jsonl 输入做**类型 + required 存在性**校验
// （D-67）；枚举/范围/跨字段约束仍归 Runner。
type Param struct {
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"`
	Required    bool   `json:"required,omitempty" yaml:"required"`
	Default     string `json:"default,omitempty" yaml:"default"`
	Description string `json:"description,omitempty" yaml:"description"`
}

// EnvVar 是运行期环境变量的声明（§12.4：值由 Task 级 env/secrets 注入，平台只声明不保存明文）。
type EnvVar struct {
	Required    bool   `json:"required,omitempty" yaml:"required"`
	Secret      bool   `json:"secret,omitempty" yaml:"secret"`
	Description string `json:"description,omitempty" yaml:"description"`
}

// ---- 终态判定 ------------------------------------------------------------

// IsTerminal 报告 Task 状态是否终态。
func (s TaskStatus) IsTerminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskServiceUnavailable, TaskCanceled:
		return true
	}
	return false
}

// IsTerminal 报告 runner 执行记录状态是否终态。
func (s RunnerStatus) IsTerminal() bool {
	switch s {
	case RunnerSucceeded, RunnerFailed, RunnerSkipped, RunnerCanceled:
		return true
	}
	return false
}

// IsActive 报告该状态是否占用端点槽位 / 全局并发 permit。
func (s RunnerStatus) IsActive() bool { return s == RunnerPreparing || s == RunnerRunning }

// IsTerminal 报告 attempt 是否终态。
func (s AttemptStatus) IsTerminal() bool {
	switch s {
	case AttemptSucceeded, AttemptFailed, AttemptCanceled:
		return true
	}
	return false
}

// IsTerminal 报告注册表状态是否终态（rejected 是"不会自动前进"的唯一状态）。
func (s RunnerState) IsTerminal() bool { return s == RunnerRejected }

// Status 把强类型状态降级为转移请求里的通用状态。
func (s TaskStatus) Status() Status    { return Status(s) }
func (s RunnerStatus) Status() Status  { return Status(s) }
func (s RunnerState) Status() Status   { return Status(s) }
func (s AttemptStatus) Status() Status { return Status(s) }

// VersionLess 比较两个 Runner 版本号：a < b 返回 true。
//
// 规则（与注册表 Scan 的排序一致，抽出来做唯一真源，禁止再实现一遍）：
//  1. 去掉首个 `-` 之后的预发布/元数据（`0.2.0-rc1` 与 `0.2.0` 比较时只看 0.2.0）；
//  2. 按 `.` 切三段，逐段数字比较（非数字按 0）；
//  3. 三段都相同再看原始字符串（保证比较是严格弱序）。
func VersionLess(a, b string) bool {
	pa, pb := ParseVersion(a), ParseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return a < b
}

// VersionGreater 报告 a > b（VersionLess 的反向，语义更直观）。
func VersionGreater(a, b string) bool { return VersionLess(b, a) }

// ParseVersion 把版本号解析成三段的数值（缺省段为 0；非数字段为 0）。
func ParseVersion(v string) [3]int {
	core := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(core, ".", 3) {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out
}
