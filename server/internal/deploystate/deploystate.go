// Package deploystate 解析、校验并投影内置 service-deploy runner 产出的
// deploy-state.json（design/impl/execution.md §9.3/§9.5；D-70）。
//
// v2 职责重划：平台**不再直连 Genet**——与 Genet 的全部交互（OpenAPI 调用、轮询、
// 卸载、判干净）都在内置 service-deploy / service-clean runner 内执行。本包只读
// runner 写在产物目录里的 deploy-state.json：
//
//   - Parse：词法/结构校验 + 白名单字段提取（未知字段放行，前向兼容）；
//   - ErrorKind：把 phase/failure 归类为 genet_call_failed | blocked | timeout，
//     供编排层映射 DEPLOY_FAILED / DEPLOY_BLOCKED（ARCHITECTURE §7.5 / 附录 D）；
//   - Project：投影成 domain.DeployDoc 摘要（store.RecordDeployState 的输入，
//     03-data-view §4.4 的 deploy-state 副本合成）。
//
// 归档（attempts/<n>/deploy-state.json 副本 + tasks.deploy jsonb 合并 +
// deploy.state_changed 事件）由 store 层提供（第一波已实现：store.RecordDeployState）；
// 本包不发任何 HTTP、不判 runner 失败——坏文件由调用方决定"下轮重读"
// （execution §9.5：坏文件不判 runner 失败）。
//
// 文件分工：
//   - deploystate.go —— 类型 + Parse + 校验 + 归类；
//   - project.go     —— domain.DeployDoc 投影；
//   - fixtures/      —— deploy-state 样例（phase 全谱 + 归类用例）。
package deploystate

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Phase 是 deploy-state.json 的五值枚举（D-46/D-47 沿用，载体变更为
// deploy-state.json；ARCHITECTURE §7.4/§9.1）。平台五值没有 deploy "failed"
// 子态——Genet 终态失败折叠为 blocked（D-47）。
const (
	PhaseQueued    = "queued"
	PhaseAccepted  = "accepted"
	PhaseDeploying = "deploying"
	PhaseReady     = "ready"
	PhaseBlocked   = "blocked"
)

// ErrorKind 是本包对失败 deploy-state 的稳定归类 token（不是附录 D 的错误码）。
// 对外错误码见 FailureCode。
const (
	// ErrorKindGenetCallFailed：service-deploy 调 Genet 失败（连接/5xx/鉴权/同名冲突）。
	// → DEPLOY_FAILED（不可重试，Task failed，ARCHITECTURE §7.5 平台侧失败）。
	ErrorKindGenetCallFailed = "genet_call_failed"
	// ErrorKindBlocked：Genet 受理但未就绪 / blocked（含 superseded 等折叠）。
	// → DEPLOY_BLOCKED（可重试，Task service_unavailable）。
	ErrorKindBlocked = "blocked"
	// ErrorKindTimeout：超过部署窗口（默认 72h，AG_DEPLOY_TIMEOUT）。
	// → DEPLOY_BLOCKED(detail.reason=timeout)（ARCHITECTURE §7.4）。
	ErrorKindTimeout = "timeout"
)

// releaseNamePattern 是 Genet 对 releaseName 的约束（D-44：arg- 前缀由 runner 派生，
// 这里只校验形态；`^[a-z][a-z0-9-]{0,52}$`）。
var releaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,52}$`)

// envKeyPattern 约束 exports 的键形态（§5.12：词法级 KV，凭据禁止入 exports）。
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// PodRef 是 deploy-state.json `pods[]` 的一项：service-deploy 周期写入的 Pod 拓扑
// 快照（§9.2），是部署 Pod 日志采集与多 Pod 拓扑还原的依据（D-26）。
type PodRef struct {
	Name     string `json:"name"`
	Role     string `json:"role,omitempty"` // engine | router | ...
	Node     string `json:"node,omitempty"` // 已调度才有值（queued 阶段为空）
	Restarts int    `json:"restarts,omitempty"`
	Ready    bool   `json:"ready,omitempty"`
}

// QueueWatermark 是队列水位（§9.1：**只有水位、没有位次**，D-46）。
// Position 键保留仅为兼容前端与 openapi，值恒 null。
type QueueWatermark struct {
	Phase    string `json:"phase,omitempty"` // PodGroup phase：Pending | Inqueue | Running | …
	Pending  int    `json:"pending"`
	Inqueue  int    `json:"inqueue"`
	Running  int    `json:"running"`
	Position *int   `json:"position"` // 恒 null（D-46）
}

// DeployFailure 是 service-deploy 失败时写进 deploy-state.json 的归类
// （§9.3 第 6 步）。Code 只有两个值（附录 D / D-47）：
// DEPLOY_FAILED（调 Genet 失败）与 DEPLOY_BLOCKED（blocked / 超窗）。
type DeployFailure struct {
	Code    string `json:"code"`              // DEPLOY_FAILED | DEPLOY_BLOCKED
	Reason  string `json:"reason,omitempty"`  // genet_call_failed | blocked | timeout | superseded | uninstalling | uninstalled
	Message string `json:"message,omitempty"` // Genet 原文（D-47：detail.reason 保留原文）
}

// DeployState 是 deploy-state.json 的解析结果（白名单字段；未知字段被 Parse
// 静默忽略以保证前向兼容——execution §9.5）。
type DeployState struct {
	// ReleaseName / Namespace 是定位键（namespace + releaseName，D-44）。
	ReleaseName string `json:"release_name"`
	Namespace   string `json:"namespace"`
	// GenetStatus 是 helm release.status 原值（pending-install / deployed / …）。
	// 原值保留：排障要能区分"Genet 说 deployed、平台说 deploying"（§9.5）。
	GenetStatus string `json:"genet_status"`
	// EndpointStatus 是 callInfo.endpoint.status 原值（pending | ready | blocked）。
	// ready 只代表地址可解析（D-45），可用性由 pre_check runner 判定。
	EndpointStatus string `json:"endpoint_status,omitempty"`
	// Phase 是五值枚举本体（queued|accepted|deploying|ready|blocked）。
	Phase string `json:"phase"`
	// Reason 是给人看的原因（PodGroup condition 原文 / 启发式判据）。
	Reason string `json:"reason,omitempty"`
	// WaitedSeconds 是部署排队/执行已等待时长（UI 展示用，无位次，D-46）。
	WaitedSeconds int64 `json:"waited_seconds,omitempty"`
	// LabelSelector 是部署 Pod 的发现依据（部署 Pod 日志采集，E-7.2）。
	LabelSelector string `json:"label_selector,omitempty"`
	// Pods 是 Pod 拓扑快照（§9.2）。
	Pods []PodRef `json:"pods,omitempty"`
	// Queue 是队列水位（无位次）。
	Queue *QueueWatermark `json:"queue,omitempty"`
	// Exports 是 ready 时 service-deploy 导出的四件套副本
	// （AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH / AG_DEPLOY_REF，§9.3 第 5 步）。
	// 注意：真正注入下游的通道是 exports.env 收割（D-71），这里只是状态副本。
	Exports map[string]string `json:"exports,omitempty"`
	// Failure 是失败归类（ready 时为 nil）。
	Failure *DeployFailure `json:"failure,omitempty"`
}

// Parse 解析并校验一份 deploy-state.json。
//
// 校验失败返回 error（坏文件**不判 runner 失败**：controller 下轮重读，§9.5）。
// 校验项：
//   - 必须是合法 JSON 对象；
//   - phase 必填且 ∈ 五值枚举（deploy-state.json 是唯一状态出口，§9.3）；
//   - release_name 非空时必须满足 Genet 形态（^[a-z][a-z0-9-]{0,52}$）；
//   - waited_seconds / pods[].restarts / queue 计数非负；
//   - exports 键必须是合法环境变量名且值不含换行（§5.12 词法约束）。
func Parse(b []byte) (*DeployState, error) {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, errors.New("deploystate: deploy-state.json is empty")
	}
	var s DeployState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("deploystate: decode deploy-state.json: %w", err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *DeployState) validate() error {
	switch s.Phase {
	case PhaseQueued, PhaseAccepted, PhaseDeploying, PhaseReady, PhaseBlocked:
	default:
		return fmt.Errorf("deploystate: phase %q not in {queued,accepted,deploying,ready,blocked}", s.Phase)
	}
	if s.ReleaseName != "" && !releaseNamePattern.MatchString(s.ReleaseName) {
		return fmt.Errorf("deploystate: release_name %q does not match ^[a-z][a-z0-9-]{0,52}$", s.ReleaseName)
	}
	if s.WaitedSeconds < 0 {
		return fmt.Errorf("deploystate: waited_seconds %d is negative", s.WaitedSeconds)
	}
	for i, p := range s.Pods {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("deploystate: pods[%d].name is empty", i)
		}
		if p.Restarts < 0 {
			return fmt.Errorf("deploystate: pods[%d].restarts is negative", i)
		}
	}
	if q := s.Queue; q != nil {
		if q.Pending < 0 || q.Inqueue < 0 || q.Running < 0 {
			return fmt.Errorf("deploystate: queue counts must be >= 0 (pending=%d inqueue=%d running=%d)",
				q.Pending, q.Inqueue, q.Running)
		}
	}
	for k, v := range s.Exports {
		if !envKeyPattern.MatchString(k) {
			return fmt.Errorf("deploystate: exports key %q is not a valid env name", k)
		}
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("deploystate: exports[%s] contains a newline (§5.12: 值不含换行)", k)
		}
	}
	return nil
}

// PodCount 返回 Pod 拓扑快照的数量（tasks.deploy.pod_count 的来源）。
func (s *DeployState) PodCount() int64 { return int64(len(s.Pods)) }

// ErrorKind 把 deploy-state 归类为三值之一；未失败（phase=ready 或仍在推进）返回 ""。
//
// 归类规则（§9.3 第 5-6 步 / D-47）：
//   - failure.code = DEPLOY_FAILED → genet_call_failed；
//   - failure.code = DEPLOY_BLOCKED → reason=timeout 归 timeout，其余归 blocked；
//   - 无 failure 但 phase=blocked（少见：runner 崩溃前只来得及改 phase）→ blocked；
//   - 其余（ready / queued / accepted / deploying）→ ""。
func (s *DeployState) ErrorKind() string {
	if s == nil {
		return ""
	}
	if f := s.Failure; f != nil {
		switch f.Code {
		case "DEPLOY_FAILED":
			return ErrorKindGenetCallFailed
		case "DEPLOY_BLOCKED":
			if f.Reason == ErrorKindTimeout {
				return ErrorKindTimeout
			}
			return ErrorKindBlocked
		}
		// 未知 code：按 reason 兜底（timeout 优先），再退 blocked。
		if f.Reason == ErrorKindTimeout {
			return ErrorKindTimeout
		}
		if s.Phase == PhaseBlocked {
			return ErrorKindBlocked
		}
		return ""
	}
	if s.Phase == PhaseBlocked {
		return ErrorKindBlocked
	}
	return ""
}

// FailureCode 返回对外错误码（附录 D）：未失败返回 ""；genet_call_failed →
// DEPLOY_FAILED；blocked / timeout → DEPLOY_BLOCKED。编排层按它映射 Task 终态
// （§7.5：DEPLOY_FAILED → failed；DEPLOY_BLOCKED → service_unavailable）。
func (s *DeployState) FailureCode() string {
	switch s.ErrorKind() {
	case ErrorKindGenetCallFailed:
		return "DEPLOY_FAILED"
	case ErrorKindBlocked, ErrorKindTimeout:
		return "DEPLOY_BLOCKED"
	default:
		return ""
	}
}
