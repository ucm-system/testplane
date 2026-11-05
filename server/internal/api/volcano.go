package api

import (
	"context"
	"strings"
)

// Volcano 快照的四个降级态（openapi `VolcanoSnapshot.detail` 枚举）。
// 红线：**不可达 ≠ 0 排队**——任何一态都不得渲染成"空闲/无排队"。
const (
	VolcanoCRDMissing         = "crd_missing"           // 集群没装 Volcano
	VolcanoRBACForbidden      = "rbac_forbidden"        // 有 CRD 但无权读
	VolcanoSchedulerUnavailable = "scheduler_unavailable" // 超时/连不上/未接入
	VolcanoDisabled           = "disabled"              // 装了但调度未启用
)

// VolcanoQueue 是 openapi `VolcanoQueue` 的投影（只读快照，不做单位换算）。
type VolcanoQueue struct {
	Name               string            `json:"name"`
	State              *string           `json:"state"`
	Capability         map[string]string `json:"capability"`
	Allocated          map[string]string `json:"allocated"`
	PendingJobs        int               `json:"pending_jobs"`
	LongestWaitSeconds int               `json:"longest_wait_seconds"`
	Reason             *string           `json:"reason"`
}

// VolcanoSnapshot 是 `queue-status.volcano` 块。Provider 为 nil 时默认降级为
// scheduler_unavailable（未接入调度器探针 ≠ 没有排队）。
type VolcanoSnapshot struct {
	Available bool           `json:"available"`
	Detail    *string        `json:"detail"`
	Queues    []VolcanoQueue `json:"queues"`
	Stale     bool           `json:"stale,omitempty"`
}

// VolcanoProvider 由执行层/编排层实现（dynamic client 读 Volcano CRD，15s TTL + 单飞）。
// 只读、不得阻塞 `/queue-status`：实现方必须自己带超时并在失败时返回降级快照。
type VolcanoProvider interface {
	VolcanoSnapshot(ctx context.Context) VolcanoSnapshot
}

// normalizeVolcanoDetail 把任意 detail 收敛到四值枚举；空值 → scheduler_unavailable。
func normalizeVolcanoDetail(detail string) string {
	switch strings.TrimSpace(detail) {
	case VolcanoCRDMissing:
		return VolcanoCRDMissing
	case VolcanoRBACForbidden:
		return VolcanoRBACForbidden
	case VolcanoDisabled:
		return VolcanoDisabled
	case VolcanoSchedulerUnavailable:
		return VolcanoSchedulerUnavailable
	default:
		return VolcanoSchedulerUnavailable
	}
}

// volcanoBlock 取 `queue-status.volcano` 块：有 Provider 就透传（detail 收敛四值），
// 没有就降级为 scheduler_unavailable。**任何失败都只影响本块**，绝不 500、
// 也绝不把"不可达"渲染成 0 排队（D-46 的红线）。
func (s *Server) volcanoBlock(ctx context.Context) VolcanoSnapshot {
	if s.volcano == nil {
		d := VolcanoSchedulerUnavailable
		return VolcanoSnapshot{Available: false, Detail: &d, Queues: []VolcanoQueue{}}
	}
	snap := s.volcano.VolcanoSnapshot(ctx)
	d := normalizeVolcanoDetail(derefOr(snap.Detail, ""))
	snap.Detail = &d
	if snap.Queues == nil {
		snap.Queues = []VolcanoQueue{}
	}
	if snap.Available {
		// 可用时 detail 必须为空（openapi 语义：detail 解释降级原因）。
		snap.Detail = nil
	}
	return snap
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
