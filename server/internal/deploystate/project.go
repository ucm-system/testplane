package deploystate

import (
	"github.com/ucm-system/testplane/server/internal/domain"
)

// Project 把一份已解析的 deploy-state 投影成 domain.DeployDoc 的**摘要增量**
// （03-data-view §4.4 的"deploy-state.json 副本合成"段），即
// store.RecordDeployState 的 snap 入参。
//
// 约束：
//   - 只带"deploy-state 承载的键"：phase / genet_status / reason / waited_seconds /
//     pod_count / label_selector / queue / failure——**绝不**整段覆盖 tasks.deploy
//     （store 侧 jsonb `||` 合并，release_name 等冻结键不能被冲掉）；
//   - queue.position 恒 nil（D-46：只有水位、没有位次）；
//   - failure 映射成 domain.Failure：code 取 FailureCode()，retryable 取附录 D
//     的码级默认（DEPLOY_BLOCKED=true / DEPLOY_FAILED=false），detail.reason
//     保留归类 token（blocked/timeout/genet_call_failed，D-47）。
//
// deployStateRef 是本次归档副本的引用（形如 attempts/1/deploy-state.json），
// 为空则不填 deploy_state_ref 键。
func (s *DeployState) Project(deployStateRef string) domain.DeployDoc {
	doc := domain.DeployDoc{
		Phase:         s.Phase,
		GenetStatus:   s.GenetStatus,
		Reason:        s.Reason,
		WaitedSeconds: ptrInt64(s.WaitedSeconds),
		PodCount:      ptrInt64(s.PodCount()),
		LabelSelector: s.LabelSelector,
	}
	if deployStateRef != "" {
		doc.DeployStateRef = deployStateRef
	}
	if q := s.Queue; q != nil {
		doc.Queue = &domain.DeployQueue{
			Phase:    q.Phase,
			Pending:  q.Pending,
			Inqueue:  q.Inqueue,
			Running:  q.Running,
			Position: nil, // 恒 null（D-46）
		}
	}
	if kind := s.ErrorKind(); kind != "" {
		code := s.FailureCode()
		msg := ""
		reason := kind
		if f := s.Failure; f != nil {
			msg = f.Message
			if f.Reason != "" {
				reason = f.Reason
			}
		}
		doc.Failure = &domain.Failure{
			Code:      code,
			Message:   msg,
			Detail:    map[string]any{"reason": reason},
			Retryable: domain.Retryable(code),
		}
	}
	return doc
}

func ptrInt64(v int64) *int64 { return &v }
