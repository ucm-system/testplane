package domain

import "time"

// DeployDoc 是 `tasks.deploy` 列的 jsonb 形状（03-data-view §4.4）。
//
// v2 裁决（D-70）：平台**不再直连 Genet**——部署动作由内置 service-deploy runner 执行，
// 它周期改写 `deploy-state.json`（`attempts/<n>/deploy-state.json`），平台归档副本并把
// 摘要合并进本列。因此本列是两部分的合成：**提交时冻结的部署描述**（service-deploy 的
// 输入，平台不解析语义）+ **最新归档的部署状态副本摘要**。
//
// 合并由 store 用 jsonb `||` 增量完成：冻结键（release_name/provider/…）只在提交时写一次，
// 之后每次归档只带"本次变化的键"，整段覆盖会把冻结信息冲掉。
type DeployDoc struct {
	// ---- 提交冻结（service-deploy 的输入，平台不解析语义）----
	Provider string `json:"provider,omitempty"` // genet | mock | existing
	Spec     string `json:"spec,omitempty"`     // 部署描述原文（落档 attempts/<n>/input/；existing 时为空）
	// Service 是 existing 变体的输入：已有服务的三元组（不落 deploy.spec）。
	Service     *ExistingService `json:"service,omitempty"`
	SpecDigest  string           `json:"spec_digest,omitempty"`
	PackageType string           `json:"package_type,omitempty"` // stack | pd

	// ---- 定位（平台派生，D-44）----
	Namespace   string `json:"namespace,omitempty"`    // Genet API Key 绑定，调用方不能指定
	ReleaseName string `json:"release_name,omitempty"` // "arg-" + sha256(task_id:attempt_no)[:12]

	// ---- deploy-state.json 副本合成（平台归档，非平台轮询）----
	DeployStateRef string `json:"deploy_state_ref,omitempty"` // attempts/<n>/deploy-state.json
	Phase          string `json:"phase,omitempty"`            // queued|accepted|deploying|ready|blocked（原值）
	GenetStatus    string `json:"genet_status,omitempty"`     // helm 原值：pending-install|deployed|failed|superseded|…
	Reason         string `json:"reason,omitempty"`
	WaitedSeconds  *int64 `json:"waited_seconds,omitempty"`
	PodCount       *int64 `json:"pod_count,omitempty"`
	LabelSelector  string `json:"label_selector,omitempty"`

	// ---- 队列：只有水位，没有位次 ----
	Queue *DeployQueue `json:"queue,omitempty"`

	// ---- Endpoint（deploy exports 收割后回填，§4.3）----
	Endpoint *Endpoint `json:"endpoint,omitempty"`

	// ---- 时间线 ----
	QueuedAt *time.Time `json:"queued_at,omitempty"`
	ReadyAt  *time.Time `json:"ready_at,omitempty"`

	// ---- 失败 / 取消 ----
	Failure           *Failure   `json:"failure,omitempty"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
}

// DeployQueue 是队列水位（只有计数，没有位次；D-46：position 恒 null）。
type DeployQueue struct {
	Phase    string `json:"phase,omitempty"` // PodGroup phase：Pending|Inqueue|Running|…
	Pending  int    `json:"pending"`
	Inqueue  int    `json:"inqueue"`
	Running  int    `json:"running"`
	Position *int   `json:"position"` // 恒 null；键保留仅为兼容前端与 openapi
}

// DeployStatePhase 是 service-deploy 写入 deploy-state.json 的 phase 枚举（§9.1）。
var DeployStatePhase = []string{"queued", "accepted", "deploying", "ready", "blocked"}

// EventDeployStateChanged 是部署状态归档的事件类型（state_events.type）。
const EventDeployStateChanged = "deploy.state_changed"

// ExistingService 是 existing-deploy 变体的输入三元组（provider=existing 时必填）。
type ExistingService struct {
	URL       string `json:"url"`
	ModelName string `json:"model_name"`
	ModelPath string `json:"model_path,omitempty"`
}
