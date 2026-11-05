package executor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestPodErrorClassification 是票 #22 的验收：
//   - Evicted / OOMKilled → EXECUTOR_POD_ERROR，且 K8s 原文原因保留；
//   - Unschedulable **不许**判失败（仍留排队态：classifyOutcome 返回 terminal=false）。
func TestPodErrorClassification(t *testing.T) {
	t.Run("evicted → EXECUTOR_POD_ERROR", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase:  corev1.PodFailed,
			Reason: "Evicted",
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "Evicted", Message: "The node was low on resource: memory.",
				}},
			}},
		}}
		out, terminal := classifyOutcome(nil, pod)
		if !terminal {
			t.Fatal("Evicted 必须判终态")
		}
		if code := OutcomeCode(*out); code != CodeExecutorPodError {
			t.Fatalf("code = %q, 期望 %s", code, CodeExecutorPodError)
		}
		if out.K8sReason != "Evicted" {
			t.Fatalf("K8sReason = %q, 期望 Evicted", out.K8sReason)
		}
		if out.Phase != PhaseFailed {
			t.Fatalf("phase = %q, 期望 failed", out.Phase)
		}
	})

	t.Run("oomkilled → EXECUTOR_POD_ERROR", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "OOMKilled",
				}},
			}},
		}}
		out, terminal := classifyOutcome(nil, pod)
		if !terminal || OutcomeCode(*out) != CodeExecutorPodError {
			t.Fatalf("out = %+v code=%q", out, OutcomeCode(*out))
		}
		if out.K8sReason != "OOMKilled" {
			t.Fatalf("K8sReason = %q", out.K8sReason)
		}
	})

	t.Run("unschedulable 不判失败", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: "0/3 nodes are available: 3 Insufficient nvidia.com/gpu.",
			}},
		}}
		if out, terminal := classifyOutcome(nil, pod); terminal {
			t.Fatalf("Unschedulable 必须留排队态，却判了终态: %+v", out)
		}
	})

	t.Run("tester 自身非零退出仍归 TESTER_NONZERO_EXIT", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error",
				}},
			}},
		}}
		out, terminal := classifyOutcome(nil, pod)
		if !terminal || OutcomeCode(*out) != CodeTesterNonZeroExit {
			t.Fatalf("out = %+v code=%q（不应被 Pod 错误码吞掉）", out, OutcomeCode(*out))
		}
	})
}
