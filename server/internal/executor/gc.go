package executor

import (
	"context"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// GC 相关默认值（execution §6.3 的孤儿 GC）。
const (
	// DefaultGCInterval 是 GC 的默认周期（设计：每 10 分钟）。
	DefaultGCInterval = 10 * time.Minute
	// gcBatchLimit 是单轮 GC 处理的 Job 上限，避免一次 list 太多卡住主循环。
	gcBatchLimit = 500
)

// Delete 删除 Job（Pod 由 Job controller 级联删除）。404 视为成功（幂等）。
//
// 用途区分（execution §5.2 / §6）：
//   - 正常结束后的回收 → Background：结论已落库，Pod 不必等它真正消失；
//   - 取消/超时收口 → 走 Cancel（Foreground + 等 Pod 消失），确保没有进程继续写 NFS。
func (e *Executor) Delete(ctx context.Context, h ports.JobHandle, policy ports.DeletePolicy) error {
	ns := e.namespaceOf(h)
	prop := metav1.DeletePropagationBackground
	switch policy {
	case ports.DeleteForeground:
		prop = metav1.DeletePropagationForeground
	case ports.DeleteBackground, "":
		prop = metav1.DeletePropagationBackground
	default:
		return fmt.Errorf("%w: unknown delete policy %q", ErrSpecInvalid, policy)
	}
	opts := metav1.DeleteOptions{PropagationPolicy: &prop}
	if prop == metav1.DeletePropagationForeground {
		grace := int64(defaultDeleteGrace)
		opts.GracePeriodSeconds = &grace
	}
	err := e.client.BatchV1().Jobs(ns).Delete(ctx, h.JobName, opts)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("executor: delete job %s/%s: %w", ns, h.JobName, err)
	}
	return nil
}

// ManagedJob 是 GC 需要的最小 Job 视图。
type ManagedJob struct {
	Namespace  string
	Name       string
	JobID      string // label testplane.io/job-id
	RunID      string // label testplane.io/run（渲染时的 Run 标识）
	CreatedAt  time.Time
	Finished   bool // 已有 Complete/Failed 条件
	Succeeded  bool // Complete=True
}

// GCReport 是一次 GC 的结果。
type GCReport struct {
	DeletedJobs int
	DeletedPods int
	KeptJobs    int
	Errors      []string
}

// AttemptStateFunc 告诉 GC 该 Job 对应的 Attempt 状态：
// found=false 表示库里没有对应 Attempt（孤儿，§6.3 第 1 条）。
type AttemptStateFunc func(ctx context.Context, jobName string) (found bool, terminal bool, err error)

// CollectionsGC 负责回收"已结束但没人删"的 Job 与孤儿 Pod（execution §6.3 的最小实现）。
//
// 判据（按顺序）：
//  1. Job 已终态 + DB 无对应 Attempt → 删（孤儿 Job）；
//  2. Job 已终态 + Attempt 已终态 → 删（正常路径没删干净的）；
//  3. Job 还在跑 → 不动；
//  4. 裸 Pod（`tp-*` 且所属 Job 已不存在）→ 删。
//
// 只删本 namespace、带平台 label 的对象；`tp-` 前缀但 label 不符的一律不动（可能是人工调试）。
func (e *Executor) CollectGarbage(ctx context.Context, attemptState AttemptStateFunc) (GCReport, error) {
	report := GCReport{}
	ns := e.namespace
	selector := LabelManaged + "=true"

	jobs, err := e.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return report, fmt.Errorf("executor: gc list jobs: %w", err)
	}
	live := make(map[string]struct{}, len(jobs.Items))
	for _, job := range jobs.Items {
		live[job.Name] = struct{}{}
	}
	processed := 0
	for _, job := range jobs.Items {
		if processed >= gcBatchLimit {
			break
		}
		processed++
		terminal, _ := jobTerminalState(&job)
		if !terminal {
			report.KeptJobs++
			continue
		}
		found, attTerminal := false, false
		if attemptState != nil {
			found, attTerminal, err = attemptState(ctx, job.Name)
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("job %s: attempt state: %v", job.Name, err))
				report.KeptJobs++
				continue
			}
		}
		if found && !attTerminal {
			// Attempt 仍在进行（刚结束、状态还没落库）：下一轮再说。
			report.KeptJobs++
			continue
		}
		if err := e.Delete(ctx, ports.JobHandle{Namespace: ns, JobName: job.Name}, ports.DeleteBackground); err != nil {
			report.Errors = append(report.Errors, err.Error())
			report.KeptJobs++
			continue
		}
		report.DeletedJobs++
		delete(live, job.Name)
	}

	// 裸 Pod：所属 Job 已不存在（Job 被删后 Pod 可能残留，或人工造的调试 Pod）。
	pods, err := e.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return report, fmt.Errorf("executor: gc list pods: %w", err)
	}
	for _, pod := range pods.Items {
		jobName := podJobName(pod)
		if jobName == "" {
			continue
		}
		if _, ok := live[jobName]; ok {
			continue
		}
		// Job 确实不在名字集合里：再确认一次（可能是本轮的 list 缓存/竞态），避免误删。
		if _, err := e.client.BatchV1().Jobs(ns).Get(ctx, jobName, metav1.GetOptions{}); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("pod %s: probe job %s: %v", pod.Name, jobName, err))
			continue
		}
		if pod.Status.Phase == corev1.PodRunning {
			// 还在跑的 Pod 不动（可能有人工调试对象用同名 job label）。
			continue
		}
		if err := e.client.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			report.Errors = append(report.Errors, fmt.Sprintf("pod %s: %v", pod.Name, err))
			continue
		}
		report.DeletedPods++
	}
	if len(report.Errors) > 8 {
		report.Errors = report.Errors[:8]
	}
	return report, nil
}

// ListManagedJobs 返回本 namespace 里带平台 label 的 Job（对账/GC 观测用）。
func (e *Executor) ListManagedJobs(ctx context.Context) ([]ManagedJob, error) {
	ns := e.namespace
	list, err := e.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=true"})
	if err != nil {
		return nil, fmt.Errorf("executor: list jobs: %w", err)
	}
	out := make([]ManagedJob, 0, len(list.Items))
	for _, job := range list.Items {
		terminal, succeeded := jobTerminalState(&job)
		out = append(out, ManagedJob{
			Namespace: job.Namespace,
			Name:      job.Name,
			JobID:     job.Labels[LabelJobID],
			RunID:     job.Labels[LabelRun],
			CreatedAt: job.CreationTimestamp.Time,
			Finished:  terminal,
			Succeeded: succeeded,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// jobTerminalState 返回 (是否终态, 是否成功)。
func jobTerminalState(job *batchv1.Job) (terminal, succeeded bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true
		case batchv1.JobFailed:
			return true, false
		}
	}
	return false, false
}
