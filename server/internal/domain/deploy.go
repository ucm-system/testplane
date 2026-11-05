package domain

import "time"

// DeploySnapshot 是 `runs.deploy` 的**增量快照**：键名与 `views/03-data-view.md §4.4` 逐字对应，
// 编排层每次状态推进只填「本次变化的键」，由 store 用 jsonb 合并（`deploy || $1::jsonb`）落库。
//
// 为什么是增量而不是整段：Release 的冻结信息（release_name / provider / package_type /
// values_digest …）在 BeginDeploy 时写入一次，之后每次轮询只更新状态与时间线；
// 整段覆盖会把 release_name 冲掉，重启后就没法靠它找回 Genet 侧的 release（§9.2）。
type DeploySnapshot struct {
	// ---- 定位与幂等 ----
	DeployID    string `json:"deployment_id,omitempty"` // 兼容既有键名（store 侧一直这么写）
	Provider    string `json:"provider,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	ReleaseName string `json:"release_name,omitempty"`
	// ---- 包与版本 ----
	PackageType  string `json:"package_type,omitempty"`
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chart_version,omitempty"`
	// ---- 冻结输入的摘要（完整 values 在 runs.manifest 里，不重复存）----
	ValuesDigest string `json:"values_digest,omitempty"`
	ModelName    string `json:"model_name,omitempty"`
	// ---- 平台合成的状态 ----
	State string `json:"state,omitempty"`
	// QueuePosition 恒 null（Genet 没有位次，D-46）；用指针是为了显式写 null 而不是丢键。
	QueuePosition *int   `json:"queue_position"`
	QueueReason   string `json:"queue_reason,omitempty"`
	// ---- 两根轴的原值（§9.3）----
	ReleaseStatus  string `json:"genet_release_status,omitempty"` // helm 原值：pending-install|deployed|failed|…
	EndpointStatus string `json:"endpoint_status,omitempty"`      // callInfo.endpoint.status 原值
	// ---- 队列水位（无位次）----
	Queue *DeployQueue `json:"queue,omitempty"`
	// ---- Endpoint 快照（ready 后回填）----
	Endpoint *Endpoint `json:"endpoint,omitempty"`
	// ---- 时间线 ----
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	QueuedAt     *time.Time `json:"queued_at,omitempty"`
	ReadyAt      *time.Time `json:"ready_at,omitempty"`
	LastPolledAt *time.Time `json:"last_polled_at,omitempty"`
	// ---- 失败 / 取消 / 卸载 ----
	Failure           *Failure         `json:"failure,omitempty"`
	CancelRequestedAt *time.Time       `json:"cancel_requested_at,omitempty"`
	Uninstall         *DeployUninstall `json:"uninstall,omitempty"`
}

// DeployQueue 是队列水位（只有计数，没有位次）。
type DeployQueue struct {
	Phase   string `json:"phase,omitempty"` // PodGroup phase：Pending|Inqueue|Running|…
	Pending int    `json:"pending"`
	Inqueue int    `json:"inqueue"`
	Running int    `json:"running"`
}

// uninstall.state 的四个取值（execution §9.7）：不要散字符串。
const (
	UninstallPending  = "pending"  // 已决定卸载，尚未发出 DELETE
	UninstallRunning  = "running"  // DELETE 已发出，等确认
	UninstallVerified = "verified" // GET 404（且 pods 为空）已确认删干净
	UninstallFailed   = "failed"   // DELETE 失败或校验未过，等对账重试
	UninstallSkipped  = "skipped"  // 按 cleanup_policy 决定不卸载（never / on_failure 且全部成功）
)

// EventCleanupFailed 是清理失败的事件类型（state_events.type，03-data-view §4.6）。
const EventCleanupFailed = "cleanup.failed"

// DeployUninstall 是清理收口的记录（§9.7）。
type DeployUninstall struct {
	State         string     `json:"state"` // pending | running | verified | failed
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	Force         bool       `json:"force"`
	CleanupErrors []string   `json:"cleanup_errors"`
}

// QueueSnapshot 与 DeployQueue 的差别：QueueSnapshot 是"一次 Poll 的结果"，
// DeployQueue 是"落库的投影"。两者字段名一致，转换由编排层做。
