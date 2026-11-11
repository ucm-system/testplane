package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// 默认时序参数。生产值来自 config；测试里可以调短。
const (
	DefaultPollInterval = 2 * time.Second
	DefaultWatchTimeout = 5 * time.Minute
	DefaultCancelWait   = 60 * time.Second
	DefaultForceWait    = 30 * time.Second
	defaultDeleteGrace  = 30
	forceDeleteGrace    = 0
	cancelMarkerReason  = "canceled by platform"

	// 两个 job-name label：新版本是 batch.kubernetes.io/job-name，旧版是 job-name。
	labelJobNameV2 = "batch.kubernetes.io/job-name"
	labelJobName   = "job-name"
)

// Options 是执行器时序配置；零值使用默认值。
type Options struct {
	// PollInterval 是 watch 断流后退回 list 轮询的间隔（execution §4.2）。
	PollInterval time.Duration
	// WatchTimeout 是单次 watch 请求的服务端超时，到时自然重连。
	WatchTimeout time.Duration
	// CancelWait 是删除 Job 后等 Pod 消失的上限（execution §5.2）。
	CancelWait time.Duration
	// ForceWait 是强删 Pod 后再次等待的上限（execution §5.2 第 4 步）。
	ForceWait time.Duration
	// Logf 是可选日志钩子：渲染告警（如 secret_env_unbound）通过它落日志，
	// 不影响执行结果（execution §4.3.1 的 warning 口径）。
	Logf func(format string, args ...any)
	// Clock 是业务时间来源（05 §6.5）：JobOutcome.FinishedAt 等**会落库**的时间戳必须走它，
	// 默认 domain.SystemClock；不要在业务代码里直调 time.Now()（票 #31）。
	Clock domain.Clock
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.WatchTimeout <= 0 {
		o.WatchTimeout = DefaultWatchTimeout
	}
	if o.CancelWait <= 0 {
		o.CancelWait = DefaultCancelWait
	}
	if o.ForceWait <= 0 {
		o.ForceWait = DefaultForceWait
	}
	if o.Clock == nil {
		o.Clock = domain.SystemClock
	}
	return o
}

// Executor 是 ports.Executor 的 K8s 实现：client-go 直连 + list/watch（不上 informer）。
type Executor struct {
	client    kubernetes.Interface
	namespace string
	opts      Options
	// clock 是业务时间来源（Options.Clock，默认 SystemClock）。
	clock domain.Clock
	// now 是 clock.Now 的快捷方式（历史调用点保留）。
	now  func() time.Time
	logf func(format string, args ...any)
}

// 确保实现契约（签名与 ports.Executor 逐字一致）。
var _ ports.Executor = (*Executor)(nil)

// New 构造执行器。
func New(client kubernetes.Interface, opts Options) *Executor {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	o := opts.withDefaults()
	return &Executor{
		client:    client,
		namespace: DefaultNamespace,
		opts:      o,
		clock:     o.Clock,
		now:       o.Clock.Now,
		logf:      logf,
	}
}

// NewWithNamespace 显式指定 namespace（为空则用 argus）。
func NewWithNamespace(client kubernetes.Interface, namespace string, opts Options) *Executor {
	e := New(client, opts)
	if namespace != "" {
		e.namespace = namespace
	}
	return e
}

// Submit 创建 Job（幂等：同名同 spec-hash 视为成功，见 execution §2.3）。
// 幂等键 = (task_id, attempt_no, seq) 的派生（附录 B / D-68）。
func (e *Executor) Submit(ctx context.Context, spec ports.JobSpec) (ports.JobHandle, error) {
	job, warnings, err := RenderWithWarnings(spec)
	if err != nil {
		return ports.JobHandle{}, err
	}
	for _, w := range warnings {
		e.logf("executor: job %s/%s: %s", job.Namespace, job.Name, w)
	}
	want := job.Annotations[AnnSpecHash]
	ns := job.Namespace

	existing, err := e.client.BatchV1().Jobs(ns).Get(ctx, job.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		return e.handleForExisting(ctx, existing, want)
	case !apierrors.IsNotFound(err):
		return ports.JobHandle{}, fmt.Errorf("executor: get job %s/%s: %w", ns, job.Name, err)
	}

	created, err := e.client.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// 并发提交：读回来比对指纹，语义与"已存在"一致。
			got, gerr := e.client.BatchV1().Jobs(ns).Get(ctx, job.Name, metav1.GetOptions{})
			if gerr != nil {
				return ports.JobHandle{}, fmt.Errorf("executor: re-read job %s/%s: %w", ns, job.Name, gerr)
			}
			return e.handleForExisting(ctx, got, want)
		}
		return ports.JobHandle{}, fmt.Errorf("executor: create job %s/%s: %w", ns, job.Name, err)
	}
	return ports.JobHandle{Namespace: ns, JobName: created.Name}, nil
}

// handleForExisting 处理"同名 Job 已存在"：指纹一致直接复用，不一致报冲突。
// 指纹缺失（人工创建的同前缀 Job）也按冲突处理——宁可人工介入，不要悄悄混用。
func (e *Executor) handleForExisting(ctx context.Context, job *batchv1.Job, want string) (ports.JobHandle, error) {
	got := job.Annotations[AnnSpecHash]
	if got != want {
		return ports.JobHandle{}, fmt.Errorf("%w: job %s/%s spec-hash %q != %q",
			ErrSpecConflict, job.Namespace, job.Name, got, want)
	}
	return ports.JobHandle{
		Namespace: job.Namespace,
		JobName:   job.Name,
		PodName:   e.findPodName(ctx, job.Namespace, job.Labels[LabelJobID], job.Name),
	}, nil
}

// findPodName 找该 Job 当前最重要的 Pod：Running 优先，其次最新创建。
func (e *Executor) findPodName(ctx context.Context, namespace, jobID, jobName string) string {
	pods, err := e.listPods(ctx, namespace, jobID, jobName)
	if err != nil || len(pods) == 0 {
		return ""
	}
	return mostRelevantPod(pods).Name
}

// listPods 用 platform label 二级过滤 Pod（§4.1：不依赖 controller 注入的 job-name）。
// jobID 为空时退化为按 job-name 过滤，仍然不会跨 Job 混用。
func (e *Executor) listPods(ctx context.Context, namespace, jobID, jobName string) ([]corev1.Pod, error) {
	selector := LabelManaged + "=true"
	if jobID != "" {
		selector += "," + LabelJobID + "=" + jobID
	}
	list, err := e.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("executor: list pods (%s): %w", selector, err)
	}
	out := make([]corev1.Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if jobName != "" && podJobName(p) != jobName {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// podJobName 取 Pod 上由 Job controller 注入的 job 名（新旧两个 label 都认）。
func podJobName(p corev1.Pod) string {
	if v := p.Labels[labelJobNameV2]; v != "" {
		return v
	}
	return p.Labels[labelJobName]
}

func mostRelevantPod(pods []corev1.Pod) corev1.Pod {
	rank := func(p corev1.Pod) int {
		switch p.Status.Phase {
		case corev1.PodRunning:
			return 0
		case corev1.PodPending:
			return 1
		default:
			return 2
		}
	}
	sorted := make([]corev1.Pod, len(pods))
	copy(sorted, pods)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := rank(sorted[i]), rank(sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i].CreationTimestamp.After(sorted[j].CreationTimestamp.Time)
	})
	return sorted[0]
}

// Cancel 置取消标记并删除 Job，等 Pod 消失（execution §5.2）。已不存在视为成功（幂等）。
func (e *Executor) Cancel(ctx context.Context, h ports.JobHandle) error {
	ns := e.namespaceOf(h)
	job, err := e.client.BatchV1().Jobs(ns).Get(ctx, h.JobName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// Job 已经没了：可能自然结束并被 GC 了，仍需确认 Pod 不在。
		return e.ensurePodsGone(ctx, ns, h)
	case err != nil:
		return fmt.Errorf("executor: get job %s/%s: %w", ns, h.JobName, err)
	}

	// 取消标记失败不阻止删除（取消是尽力而为），但要让上游能看到原因：这里把
	// patch 错误并入返回错误路径只在删除也失败时才有意义，所以仅尝试一次。
	_ = e.markCanceled(ctx, ns, job)

	// 取消/超时收口用 Foreground：确保 Pod 真正消失再返回，避免容器继续写 NFS
	// （execution §5.2）。复用 Delete 的同一份实现，不再内联一份 propagation 逻辑。
	if delErr := e.Delete(ctx, h, ports.DeleteForeground); delErr != nil {
		return delErr
	}

	if err := e.waitPodsGone(ctx, ns, h, e.opts.CancelWait); err == nil {
		return nil
	}
	// 残留：强删 Pod（grace 0 + Foreground），必须确认删掉——否则会继续写 NFS（§5.2 第 3 步）。
	if err := e.forceDeletePods(ctx, ns, h); err != nil {
		return err
	}
	if err := e.waitPodsGone(ctx, ns, h, e.opts.ForceWait); err != nil {
		return fmt.Errorf("%w: job %s/%s", ErrCleanupIncomplete, ns, h.JobName)
	}
	return nil
}

func (e *Executor) namespaceOf(h ports.JobHandle) string {
	if h.Namespace != "" {
		return h.Namespace
	}
	return e.namespace
}

func (e *Executor) markCanceled(ctx context.Context, ns string, job *batchv1.Job) error {
	payload := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q,%q:%q}}}`,
		AnnCanceledAt, e.now().UTC().Format(time.RFC3339), AnnCanceledBy, cancelMarkerReason)
	if _, err := e.client.BatchV1().Jobs(ns).Patch(ctx, job.Name, types.MergePatchType, []byte(payload), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("executor: patch cancel marker on %s/%s: %w", ns, job.Name, err)
	}
	return nil
}

func (e *Executor) ensurePodsGone(ctx context.Context, ns string, h ports.JobHandle) error {
	if err := e.waitPodsGone(ctx, ns, h, e.opts.CancelWait); err == nil {
		return nil
	}
	if err := e.forceDeletePods(ctx, ns, h); err != nil {
		return err
	}
	return e.waitPodsGone(ctx, ns, h, e.opts.ForceWait)
}

// waitPodsGone 每 PollInterval 查一次 Pod，直到全部消失或超时。
func (e *Executor) waitPodsGone(ctx context.Context, ns string, h ports.JobHandle, timeout time.Duration) error {
	deadline := e.now().Add(timeout)
	for {
		pods, err := e.listPods(ctx, ns, "", h.JobName)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return nil
		}
		if e.now().After(deadline) {
			return fmt.Errorf("%w: %d pod(s) remain", ErrCleanupIncomplete, len(pods))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.opts.PollInterval):
		}
	}
}

// forceDeletePods 强删残留 Pod：grace 0、Foreground。
func (e *Executor) forceDeletePods(ctx context.Context, ns string, h ports.JobHandle) error {
	pods, err := e.listPods(ctx, ns, "", h.JobName)
	if err != nil {
		return err
	}
	policy := metav1.DeletePropagationForeground
	grace := int64(forceDeleteGrace)
	var firstErr error
	for _, p := range pods {
		derr := e.client.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{
			PropagationPolicy:  &policy,
			GracePeriodSeconds: &grace,
		})
		if derr != nil && !apierrors.IsNotFound(derr) && firstErr == nil {
			firstErr = fmt.Errorf("executor: force delete pod %s/%s: %w", ns, p.Name, derr)
		}
	}
	return firstErr
}

// Logs 返回 runner 的 stdout/stderr 流（明文、不脱敏，K-5）。
func (e *Executor) Logs(ctx context.Context, h ports.JobHandle) (io.ReadCloser, error) {
	ns := e.namespaceOf(h)
	pod := h.PodName
	if pod == "" {
		pod = e.findPodName(ctx, ns, "", h.JobName)
	}
	if pod == "" {
		return nil, fmt.Errorf("%w: no pod for job %s/%s", ErrJobNotFound, ns, h.JobName)
	}
	req := e.client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: RunnerContainerName,
		Follow:    true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("executor: stream logs %s/%s: %w", ns, pod, err)
	}
	return stream, nil
}

// errNilObserver 由 Watch 在参数非法时返回。
var errNilObserver = errors.New("executor: nil observer")
