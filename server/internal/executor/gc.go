package executor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// GC 相关常量（execution §6.3 的孤儿 GC）。
const (
	// HoldUntilAnnotation 是"日志 follower 还没收尾，先别删"的守卫（execution §6.3）。
	// 值二选一：RFC3339 时间戳，或 Unix 秒。
	HoldUntilAnnotation = "testplane.io/hold-until"
	// GCTerminalAge 是"终态 Job 至少留这么久"的窗口（execution §6.3「attempt 已终态且 Job 创建 > 1h」）。
	// finalizeAttemptAsync 是异步的（先落日志再删 Job），GC 抢在前面就会丢日志。
	GCTerminalAge = time.Hour
	// GCStaleRunningAge 是"运行中但明显卡死"的年龄上限（execution §6.3「运行中 > 25h」）。
	GCStaleRunningAge = 25 * time.Hour
	// gcOrphanGrace 是"没有 Attempt 行"时的容忍期，防 Submit 与写库的竞态。
	gcOrphanGrace = time.Minute
	// gcBatchLimit 是单轮 GC 处理的 Job 上限。
	gcBatchLimit = 500
)

// Delete 删除 Job（Pod 由 Job controller 级联删除）。404 视为成功（幂等）。
//
// 用途区分（execution §5.2 / §6）：
//   - 正常结束后的回收 → Background：结论已落库，Pod 不必等它真正消失；
//   - 取消/超时收口 → Foreground：确保没有进程继续写 NFS。
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

// HoldActive 报告该 Job 的 `testplane.io/hold-until` 是否还没过期（未过期 = 不许删）。
//
// 编排层在"终态删 Job"之前必须看这个：日志 follower 还在读 Pod 日志时删 Job 会丢尾部日志。
func (e *Executor) HoldActive(ctx context.Context, h ports.JobHandle) (bool, error) {
	ns := e.namespaceOf(h)
	job, err := e.client.BatchV1().Jobs(ns).Get(ctx, h.JobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("executor: read hold-until %s/%s: %w", ns, h.JobName, err)
	}
	until, ok := ParseHoldUntil(job.Annotations[HoldUntilAnnotation])
	if !ok {
		return false, nil
	}
	return time.Now().Before(until), nil
}

// ParseHoldUntil 解析 annotation 取值：RFC3339 或 Unix 秒；非法值按"没有守卫"处理。
func ParseHoldUntil(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, true
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0), true
	}
	return time.Time{}, false
}

// ManagedJob 是 GC 需要的最小 Job 视图。
type ManagedJob struct {
	Namespace string
	Name      string
	JobID     string // label testplane.io/job-id
	RunID     string // label testplane.io/run
	CreatedAt time.Time
	Finished  bool // 已有 Complete/Failed 条件
	Succeeded bool // Complete=True
}

// GCReport 是一次 GC 的结果。
type GCReport struct {
	DeletedJobs int
	DeletedPods int
	KeptJobs    int
	Errors      []string
}

// AttemptState 是某个 Job 对应 Attempt 在库里的状态。
type AttemptState struct {
	// Found=false 表示库里没有对应 Attempt（孤儿 Job，§6.3 第 1 条）。
	Found bool
	// Terminal=true 表示 Attempt 已终态（结论已落库，Job 可以回收）。
	Terminal bool
}

// AttemptStatesFunc 是**批量**查询：一次拿到一批 Job 的 Attempt 状态。
//
// 必须是批量的：单 Job 一次查询会造成 N+1（500 个 Job × 3 次查询），
// GC 每 10 分钟一轮，慢查询会把 DB 打满（票 #20-5）。
type AttemptStatesFunc func(ctx context.Context, jobNames []string) (map[string]AttemptState, error)

// GCOptions 是一次 GC 的输入。
type GCOptions struct {
	// AttemptStates 批量查库；nil 表示"所有 Job 都当成有 Attempt 且终态随 Job"（只做年龄与守卫判定）。
	AttemptStates AttemptStatesFunc
	// OnStaleRunning 在"运行中但超过 25h"的 Job 上被调用：把状态修成 FAILED(TASK_TIMEOUT)（§6.3）。
	OnStaleRunning func(ctx context.Context, job ManagedJob) error
	// Now 供测试注入时钟。
	Now func() time.Time
}

// CollectGarbage 回收"已结束但没人删"的 Job 与孤儿 Pod（execution §6.3）。
//
// 判据（按顺序，全部通过才删）：
//  1. `testplane.io/hold-until` 未过期 → 跳过（日志 follower 未收尾）；
//  2. `managed=true` 但 DB 无对应 Attempt（超过 1 分钟容忍期）→ 立即删（孤儿）；
//  3. Job 已终态 + 创建超过 1h + Attempt 已终态 → 删（正常路径没删干净的）；
//  4. 运行中 + 创建超过 25h → 修状态为 FAILED(TASK_TIMEOUT) 后删；
//  5. 裸 Pod（`tp-*` 且所属 Job 已不存在）→ 删。
//
// 只删本 namespace、带平台 label 的对象；`tp-` 前缀但 label 不符的一律不动（可能是人工调试）。
func (e *Executor) CollectGarbage(ctx context.Context, opts GCOptions) (GCReport, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	report := GCReport{}
	ns := e.namespace
	selector := LabelManaged + "=true"

	jobs, err := e.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return report, fmt.Errorf("executor: gc list jobs: %w", err)
	}
	live := make(map[string]struct{}, len(jobs.Items))
	names := make([]string, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		live[job.Name] = struct{}{}
		names = append(names, job.Name)
	}

	// 一次批量查库（票 #20-5：不要 N+1）。
	states := map[string]AttemptState{}
	if opts.AttemptStates != nil && len(names) > 0 {
		states, err = opts.AttemptStates(ctx, names)
		if err != nil {
			return report, fmt.Errorf("executor: gc attempt states: %w", err)
		}
	}

	processed := 0
	for i := range jobs.Items {
		if processed >= gcBatchLimit {
			break
		}
		processed++
		job := &jobs.Items[i]
		at := now()
		age := at.Sub(job.CreationTimestamp.Time)

		// 规则 1：hold-until 守卫（优先于一切删除动作）。
		if until, ok := ParseHoldUntil(job.Annotations[HoldUntilAnnotation]); ok && at.Before(until) {
			report.KeptJobs++
			continue
		}

		terminal, _ := jobTerminalState(job)
		state := states[job.Name] // 零值 = Found:false（孤儿）
		if opts.AttemptStates == nil {
			state = AttemptState{Found: true, Terminal: terminal}
		}

		switch {
		// 规则 2：孤儿 Job（DB 里没有对应 Attempt）。
		case !state.Found:
			// Attempt 先落库再 Submit，所以"没有行"就是真孤儿；只留 1 分钟容忍期防竞态。
			if age < gcOrphanGrace {
				report.KeptJobs++
				continue
			}
		// 规则 3：终态 + 超过 1h 窗口 + Attempt 已终态。
		case terminal && age >= GCTerminalAge && state.Terminal:
		// 规则 4：运行中但超过 25h（卡死）。
		case !terminal && age >= GCStaleRunningAge:
			if opts.OnStaleRunning != nil {
				if err := opts.OnStaleRunning(ctx, ManagedJob{
					Namespace: job.Namespace, Name: job.Name,
					JobID: job.Labels[LabelJobID], RunID: job.Labels[LabelRun],
					CreatedAt: job.CreationTimestamp.Time, Finished: terminal,
				}); err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("job %s: repair stale state: %v", job.Name, err))
					report.KeptJobs++
					continue
				}
			}
		default:
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
