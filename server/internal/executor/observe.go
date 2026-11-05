package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// 执行层对外暴露的错误码（附录 D 的子集）。JobOutcome 没有专门的 code 字段，
// 因此码以 "<CODE>: <人类可读细节>" 的形式放进 Message，由 OutcomeCode 取回。
const (
	CodeTesterNonZeroExit = "TESTER_NONZERO_EXIT"
	CodeTaskTimeout       = "TASK_TIMEOUT"
	CodeImagePullFailed   = "IMAGE_PULL_FAILED"
	CodeMountUnavailable  = "MOUNT_UNAVAILABLE"
	CodeInternalError     = "INTERNAL_ERROR"
	CodeExecutorUnavail   = "EXECUTOR_UNAVAILABLE"
)

// 观测相位（ports.JobOutcome.Phase 的取值）。
const (
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed"
	PhaseTimeout   = "timeout"
	PhaseUnknown   = "unknown"
)

// imagePullWaitingReasons 是判定 IMAGE_PULL_FAILED 的 waiting.reason 集合（§4.3）。
var imagePullWaitingReasons = map[string]struct{}{
	"ErrImagePull":        {},
	"ImagePullBackOff":    {},
	"InvalidImageName":    {},
	"ErrImageNeverPull":   {},
	"RegistryUnavailable": {},
}

// mountFailureHints 用于识别 PVC/挂载类失败（§4.3 的 MOUNT_UNAVAILABLE）。
var mountFailureHints = []string{"volume", "mount", "FailedMount", "FailedAttachVolume", "MountVolume"}

// Watch 阻塞到 Job 终态或 ctx 取消，终态时回调 obs 恰好一次（execution §4.1）。
//
// 实现：先 list+watch（带 field selector，避免全量扩散）；watch 断流、410 Gone
// 或 apiserver 异常时退回 2s 轮询 list（§4.2 的兜底），直到拿到终态或 ctx 取消。
func (e *Executor) Watch(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) error {
	if obs == nil {
		return errNilObserver
	}
	if h.JobName == "" {
		return fmt.Errorf("%w: job name is empty", ErrSpecInvalid)
	}
	// 先做一次即时判定：可能已经终态（重连/重启场景）。
	done, err := e.checkAndNotify(ctx, h, obs)
	if err != nil || done {
		return err
	}
	if err := e.watchLoop(ctx, h, obs); err != nil {
		return err
	}
	return nil
}

// watchLoop 先尝试 watch；watch 不可用或断流则转轮询，轮询负责等到终态。
func (e *Executor) watchLoop(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) error {
	finished, err := e.consumeWatch(ctx, h, obs)
	if err != nil {
		return err
	}
	if finished {
		return nil
	}
	return e.pollLoop(ctx, h, obs)
}

// consumeWatch 打开一次 watch 并消费事件。返回 (是否已到终态并回调, 错误)。
// 任何"watch 不可用"的情况都返回 (false, nil)，交给 pollLoop。
func (e *Executor) consumeWatch(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) (bool, error) {
	ns := e.namespaceOf(h)
	selector := "metadata.name=" + h.JobName

	list, err := e.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, nil
	}
	timeout := int64(e.opts.WatchTimeout.Seconds())
	w, err := e.client.BatchV1().Jobs(ns).Watch(ctx, metav1.ListOptions{
		FieldSelector:        selector,
		ResourceVersion:      list.ResourceVersion,
		TimeoutSeconds:       &timeout,
		AllowWatchBookmarks:  true,
	})
	if err != nil {
		return false, nil
	}
	defer w.Stop()

	ticker := time.NewTicker(e.opts.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return false, nil
			}
			if ev.Type == watch.Error {
				// 410 Gone / 鉴权失败等：交给轮询兜底。
				return false, nil
			}
			done, err := e.checkAndNotify(ctx, h, obs)
			if err != nil || done {
				return done, err
			}
		case <-ticker.C:
			// watch 静默时也主动核对一次：Job 终态但事件丢失、或 Pod 侧先出现失败原因。
			done, err := e.checkAndNotify(ctx, h, obs)
			if err != nil || done {
				return done, err
			}
		}
	}
}

// pollLoop 是 §4.2 的周期对账兜底：每 PollInterval list 一次，直到终态或 ctx 取消。
func (e *Executor) pollLoop(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) error {
	ticker := time.NewTicker(e.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			done, err := e.checkAndNotify(ctx, h, obs)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// checkAndNotify 判定一次；到终态则回调 observer（恰好一次，由调用方保证）。
func (e *Executor) checkAndNotify(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) (bool, error) {
	out, terminal, err := e.evaluate(ctx, h)
	if err != nil {
		return false, err
	}
	if !terminal {
		return false, nil
	}
	return true, obs.JobFinished(ctx, *out)
}

// evaluate 读 Job + Pod，映射成 JobOutcome（execution §4.3）。
func (e *Executor) evaluate(ctx context.Context, h ports.JobHandle) (*ports.JobOutcome, bool, error) {
	ns := e.namespaceOf(h)
	job, err := e.client.BatchV1().Jobs(ns).Get(ctx, h.JobName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, fmt.Errorf("executor: get job %s/%s: %w", ns, h.JobName, err)
		}
		// NotFound 时 fake/真实客户端都可能带一个零值对象回来，显式置空。
		job = nil
	}
	jobID := ""
	if job != nil {
		jobID = job.Labels[LabelJobID]
	}
	pods, err := e.listPods(ctx, ns, jobID, h.JobName)
	if err != nil {
		return nil, false, err
	}
	var pod *corev1.Pod
	if len(pods) > 0 {
		p := mostRelevantPod(pods)
		pod = &p
	}
	if job == nil {
		if pod == nil {
			return nil, false, fmt.Errorf("%w: %s/%s", ErrJobNotFound, ns, h.JobName)
		}
		// Job 已消失但 Pod 还在（少见）：只能按 Pod 事实判。
		out, terminal := classifyOutcome(nil, pod)
		if terminal {
			out.Handle = h
		}
		return out, terminal, nil
	}

	out, terminal := classifyOutcome(job, pod)
	if !terminal {
		return nil, false, nil
	}
	out.Handle = h
	return out, true, nil
}

// classifyOutcome 是纯函数：Job 终态优先，Pod 终止原因更细时以 Pod 为准（§4.3）。
func classifyOutcome(job *batchv1.Job, pod *corev1.Pod) (*ports.JobOutcome, bool) {
	out := &ports.JobOutcome{Phase: PhaseUnknown}
	finished := time.Now().UTC()
	if job != nil {
		if job.Status.CompletionTime != nil {
			finished = job.Status.CompletionTime.Time
		}
		if job.Status.StartTime != nil && job.Status.CompletionTime == nil && jobTerminal(job) {
			finished = time.Now().UTC()
		}
	}

	var (
		podExit      *int32
		podReason    string
		podMessage   string
		podCode      string
		podTerminal  bool
		imageDigest  string
	)
	if pod != nil {
		imageDigest = podImageDigest(pod)
		facts := podFacts(pod)
		podExit = facts.exitCode
		podReason = facts.terminatedReason
		podMessage = facts.message
		podCode = facts.code
		podTerminal = facts.terminal
	}
	out.ExitCode = podExit
	out.ImageDigest = imageDigest

	// 1. Pod 侧更细的失败原因（镜像拉取 / 挂载）优先——它们比 Job 条件更能说明"为什么"。
	if podCode == CodeImagePullFailed || podCode == CodeMountUnavailable {
		out.Phase = PhaseFailed
		out.Message = podCode + ": " + podMessage
		out.FinishedAt = finished
		return out, true
	}

	// 2. Pod 已终止：以退出码为准（矛盾时 Pod 更细）。
	if podTerminal {
		out.Phase = phaseFromPod(podExit, podReason)
		out.Message = messageFor(out.Phase, podExit, podReason, podMessage)
		out.FinishedAt = finished
		return out, true
	}

	// 3. Job 终态。
	if job != nil {
		for _, c := range job.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete:
				out.Phase = PhaseSucceeded
				out.Message = "job complete"
				out.FinishedAt = finished
				return out, true
			case batchv1.JobFailed:
				out.FinishedAt = finished
				if c.Reason == "DeadlineExceeded" {
					out.Phase = PhaseTimeout
					out.Message = CodeTaskTimeout + ": " + reasonText(c.Message, c.Reason)
					return out, true
				}
				out.Phase = PhaseFailed
				code := CodeTesterNonZeroExit
				if c.Reason == "BackoffLimitExceeded" {
					code = reasonCodeFromPods(job, podReason, podExit)
				}
				out.Message = code + ": " + reasonText(c.Message, c.Reason)
				return out, true
			}
		}
		// Job 没写 condition 但墓碑字段已到：兜底判失败。
		if job.Status.Failed > 0 && job.Status.Active == 0 && job.Status.Succeeded == 0 {
			out.Phase = PhaseFailed
			out.Message = CodeTesterNonZeroExit + ": job failed (" + reasonText("", "no condition") + ")"
			out.FinishedAt = finished
			return out, true
		}
	}
	return out, false
}

// jobTerminal 报告 Job 是否已有终态条件。
func jobTerminal(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}

func phaseFromPod(exit *int32, reason string) string {
	if reason == "OOMKilled" || reason == "Evicted" {
		return PhaseFailed
	}
	if exit != nil && *exit == 0 {
		return PhaseSucceeded
	}
	return PhaseFailed
}

func messageFor(phase string, exit *int32, reason, detail string) string {
	var b strings.Builder
	if phase == PhaseSucceeded {
		b.WriteString("container exited 0")
		return b.String()
	}
	code := CodeTesterNonZeroExit
	if reason == "OOMKilled" || reason == "Evicted" {
		code = CodeInternalError
	}
	b.WriteString(code)
	if reason != "" {
		b.WriteString(": ")
		b.WriteString(reason)
	}
	if exit != nil {
		fmt.Fprintf(&b, " (exit code %d)", *exit)
	}
	if detail != "" {
		b.WriteString(": ")
		b.WriteString(truncate(detail, 256))
	}
	return b.String()
}

func messageForOutcome(phase string, exit *int32, reason, detail string) string {
	if phase == PhaseSucceeded {
		return "container exited 0"
	}
	return messageFor(phase, exit, reason, detail)
}

func reasonText(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return truncate(primary, 256)
	}
	if fallback != "" {
		return fallback
	}
	return "no detail"
}

func reasonCodeFromPods(job *batchv1.Job, reason string, exit *int32) string {
	if reason == "OOMKilled" || reason == "Evicted" {
		return CodeInternalError
	}
	return CodeTesterNonZeroExit
}

// OutcomeCode 从 JobOutcome 取回附录 D 的错误码；成功时返回空串（execution §4.5）。
func OutcomeCode(o ports.JobOutcome) string {
	msg := strings.TrimSpace(o.Message)
	if i := strings.IndexByte(msg, ':'); i > 0 {
		candidate := strings.TrimSpace(msg[:i])
		if isKnownCode(candidate) {
			return candidate
		}
	} else if isKnownCode(msg) {
		return msg
	}
	switch o.Phase {
	case PhaseSucceeded:
		return ""
	case PhaseTimeout:
		return CodeTaskTimeout
	case PhaseFailed:
		return CodeTesterNonZeroExit
	default:
		return CodeInternalError
	}
}

func isKnownCode(code string) bool {
	switch code {
	case CodeTesterNonZeroExit, CodeTaskTimeout, CodeImagePullFailed,
		CodeMountUnavailable, CodeInternalError, CodeExecutorUnavail:
		return true
	}
	return false
}

// ParseImageDigest 从 Pod 的 imageID 里提取 digest（execution §4.3.1）。
// 输入形如 "docker-pullable://repo/name@sha256:abc..."，输出 "sha256:abc..."。
// 格式不符时返回空串——只记 warning，不影响状态。
func ParseImageDigest(imageID string) string {
	imageID = strings.TrimSpace(imageID)
	if imageID == "" {
		return ""
	}
	const marker = "sha256:"
	i := strings.LastIndex(imageID, marker)
	if i < 0 {
		return ""
	}
	hexPart := imageID[i+len(marker):]
	if len(hexPart) < 64 {
		return ""
	}
	hexPart = hexPart[:64]
	for _, r := range hexPart {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return marker + strings.ToLower(hexPart)
}

// podImageDigest 取 Pod 第一个容器（tester）的 imageID 并解析 digest。
func podImageDigest(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if d := ParseImageDigest(cs.ImageID); d != "" {
			return d
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if d := ParseImageDigest(cs.ImageID); d != "" {
			return d
		}
	}
	return ""
}

// podFacts 是 Pod 侧观测的归约结果。
type podFactsView struct {
	exitCode       *int32
	terminatedReason string
	message        string
	code           string
	terminal       bool
}

func podFacts(pod *corev1.Pod) podFactsView {
	var f podFactsView
	for _, cs := range pod.Status.ContainerStatuses {
		if state := cs.State.Terminated; state != nil {
			code := state.ExitCode
			f.exitCode = &code
			f.terminatedReason = state.Reason
			f.message = state.Message
			f.terminal = true
			continue
		}
		if waiting := cs.State.Waiting; waiting != nil {
			if _, ok := imagePullWaitingReasons[waiting.Reason]; ok {
				f.code = CodeImagePullFailed
				f.message = reasonText(waiting.Message, waiting.Reason)
				return f
			}
		}
	}
	// 调度/挂载类失败：PodScheduled=False 且 message 命中 volume/mount。
	for _, cond := range pod.Status.Conditions {
		if cond.Type != corev1.PodScheduled || cond.Status != corev1.ConditionFalse {
			continue
		}
		msg := cond.Message + " " + cond.Reason
		for _, hint := range mountFailureHints {
			if strings.Contains(msg, hint) {
				f.code = CodeMountUnavailable
				f.message = reasonText(cond.Message, cond.Reason)
				return f
			}
		}
	}
	return f
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
