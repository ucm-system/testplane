package domain

import "time"

// DeployDoc 是 `tasks.deploy` 列的 jsonb 形状（03-data-view §4.4）。
//
// v2 裁决（deploy/clean 解耦）：提交体的 deploy 块是**runner 引用**（与 tests[] 同构），
// 平台不感知 genet/mock/existing 之类的 provider 名——deploy: {runner, version, input}
// 按 name+version 从注册表解析后注入为 seq=1 的 deploy 行；clean 是否注入由 deploy
// runner 的 runner.yaml（clean_runner 声明）决定。部署动作由该 runner 执行，它周期改写
// `deploy-state.json`（`attempts/<n>/deploy-state.json`），平台归档副本并把摘要合并进本列。
// 因此本列是两部分的合成：**提交时冻结的 runner 引用**（平台不解析 input 语义）+
// **最新归档的部署状态副本摘要**。
//
// 合并由 store 用 jsonb `||` 增量完成：冻结键（runner/version/…）只在提交时写一次，
// 之后每次归档只带"本次变化的键"，整段覆盖会把冻结信息冲掉。
type DeployDoc struct {
	// ---- 提交冻结（deploy runner 引用，与 tests[] 同构；平台不解析 input 语义）----
	Runner  string `json:"runner,omitempty"`  // deploy 角色的 runner 名（如 genet-deploy / existing-deploy）
	Version string `json:"version,omitempty"` // 解析后的 runner 版本
	Input   string `json:"input,omitempty"`   // 该 runner 的输入原文（落档 attempts/<n>/input/1/）

	SpecDigest string `json:"spec_digest,omitempty"` // Input 的 sha256（审计用）

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
