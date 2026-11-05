package executor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

func newFakeExecutor(objs ...runtime.Object) (*Executor, *fake.Clientset) {
	return newFakeExecutorWith(20*time.Millisecond, objs...)
}

func newFakeExecutorWith(poll time.Duration, objs ...runtime.Object) (*Executor, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objs...)
	return NewWithNamespace(cs, "testplane", Options{
		PollInterval: poll,
		WatchTimeout: 2 * time.Second,
		CancelWait:   200 * time.Millisecond,
		ForceWait:    200 * time.Millisecond,
	}), cs
}

func TestSubmitIsIdempotent(t *testing.T) {
	ex, cs := newFakeExecutor()
	ctx := context.Background()

	h1, err := ex.Submit(ctx, sampleSpec())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if h1.JobName != "tp-t-0001-a1" {
		t.Fatalf("handle = %+v", h1)
	}
	h2, err := ex.Submit(ctx, sampleSpec())
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if h1 != h2 {
		t.Errorf("idempotent submit returned different handles: %+v vs %+v", h1, h2)
	}
	jobs, err := cs.BatchV1().Jobs("testplane").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want exactly 1 (job_id idempotency, §2.3)", len(jobs.Items))
	}
}

func TestSubmitDetectsSpecConflict(t *testing.T) {
	ex, _ := newFakeExecutor()
	ctx := context.Background()
	if _, err := ex.Submit(ctx, sampleSpec()); err != nil {
		t.Fatal(err)
	}
	spec := sampleSpec()
	spec.Args = append(spec.Args, "--max_tokens=1")
	_, err := ex.Submit(ctx, spec)
	if !errors.Is(err, ErrSpecConflict) {
		t.Fatalf("err = %v, want ErrSpecConflict", err)
	}
}

func TestSubmitReportsRenderErrors(t *testing.T) {
	ex, _ := newFakeExecutor()
	spec := sampleSpec()
	spec.Image = ""
	if _, err := ex.Submit(context.Background(), spec); !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
}

func TestWatchFallsBackToPollingAndNotifiesOnce(t *testing.T) {
	ex, cs := newFakeExecutor()
	ctx := context.Background()
	h, err := ex.Submit(ctx, sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	// watch 不可用 → 必须退回 2s 轮询（这里缩短为 20ms）。
	cs.PrependWatchReactor("jobs", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, errors.New("watch unavailable")
	})

	var calls int32
	obs := observerFunc(func(ctx context.Context, o ports.JobOutcome) error {
		atomic.AddInt32(&calls, 1)
		if o.Phase != PhaseSucceeded {
			t.Errorf("phase = %q", o.Phase)
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- ex.Watch(ctx, h, obs) }()

	// 模拟 Job 完成。
	time.Sleep(60 * time.Millisecond)
	job, err := cs.BatchV1().Jobs("testplane").Get(ctx, h.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	now := metav1.Now()
	job.Status.CompletionTime = &now
	if _, err := cs.BatchV1().Jobs("testplane").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		// fake tracker 没有分 status 子资源时，走普通 Update。
		if _, uerr := cs.BatchV1().Jobs("testplane").Update(ctx, job, metav1.UpdateOptions{}); uerr != nil {
			t.Fatalf("update job: %v / %v", err, uerr)
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return after job reached terminal state")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("observer calls = %d, want exactly 1", got)
	}
}

func TestWatchUsesWatchEvents(t *testing.T) {
	// 轮询间隔调大：本用例验证的是"watch 事件驱动"，不能被周期性核对抢先收口。
	ex, cs := newFakeExecutorWith(30 * time.Second)
	ctx := context.Background()
	h, err := ex.Submit(ctx, sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	fw := watch.NewFake()
	defer fw.Stop()
	established := make(chan struct{}, 1)
	cs.PrependWatchReactor("jobs", func(action k8stesting.Action) (bool, watch.Interface, error) {
		select {
		case established <- struct{}{}:
		default:
		}
		return true, fw, nil
	})

	var calls int32
	done := make(chan error, 1)
	go func() {
		done <- ex.Watch(ctx, h, observerFunc(func(ctx context.Context, o ports.JobOutcome) error {
			atomic.AddInt32(&calls, 1)
			return nil
		}))
	}()

	// 等 watch 真正建立，再推事件；不要用 sleep 猜时序。
	select {
	case <-established:
	case <-time.After(2 * time.Second):
		t.Fatal("watch was never established")
	}

	job, err := cs.BatchV1().Jobs("testplane").Get(ctx, h.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline",
	}}
	if _, err := cs.BatchV1().Jobs("testplane").Update(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update job: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("Watch returned before the event was delivered: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	fw.Modify(job)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not observe the watch event")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("observer calls = %d", got)
	}
}

func TestWatchRespectsCanceledContext(t *testing.T) {
	ex, _ := newFakeExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := ex.Submit(context.Background(), sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- ex.Watch(ctx, h, observerFunc(func(context.Context, ports.JobOutcome) error { return nil }))
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch ignored context cancellation")
	}
}

func TestCancelDeletesJobAndPods(t *testing.T) {
	ex, cs := newFakeExecutor()
	ctx := context.Background()
	h, err := ex.Submit(ctx, sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	// 造一个属于该 Job 的 Pod。
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tp-t-0001-a1-abcde",
			Namespace: "testplane",
			Labels: map[string]string{
				LabelManaged: "true",
				LabelJobID:   JobID("t-0001", 1),
				labelJobName: h.JobName,
			},
		},
	}
	if _, err := cs.CoreV1().Pods("testplane").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := ex.Cancel(ctx, h); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(ctx, h.JobName, metav1.GetOptions{}); err == nil {
		t.Errorf("job still exists after Cancel")
	}
	pods, err := cs.CoreV1().Pods("testplane").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("pods remaining after Cancel: %d", len(pods.Items))
	}
	// 幂等：再取消一次不报错。
	if err := ex.Cancel(ctx, h); err != nil {
		t.Errorf("second Cancel = %v, want nil", err)
	}
}

func TestWatchOnMissingJobReturnsNotFound(t *testing.T) {
	ex, _ := newFakeExecutor()
	err := ex.Watch(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "tp-x-a1"},
		observerFunc(func(context.Context, ports.JobOutcome) error { return nil }))
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestClassifyOutcomeFromPods(t *testing.T) {
	digest := "sha256:" + repeatHex(64)

	t.Run("nonzero exit", func(t *testing.T) {
		exit := int32(3)
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				ImageID: "docker-pullable://repo/app@" + digest,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: "Error",
				}},
			}},
		}}
		out, terminal := classifyOutcome(nil, pod, time.Unix(1_700_000_000, 0))
		if !terminal || out.Phase != PhaseFailed {
			t.Fatalf("out = %+v terminal=%v", out, terminal)
		}
		if OutcomeCode(*out) != CodeTesterNonZeroExit {
			t.Errorf("code = %q", OutcomeCode(*out))
		}
		if out.ImageDigest != digest {
			t.Errorf("digest = %q", out.ImageDigest)
		}
		if out.ExitCode == nil || *out.ExitCode != exit {
			t.Errorf("exitCode = %v", out.ExitCode)
		}
	})

	t.Run("oom killed", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "OOMKilled",
				}},
			}},
		}}
		out, terminal := classifyOutcome(nil, pod, time.Unix(1_700_000_000, 0))
		// Pod 异常终止统一归 EXECUTOR_POD_ERROR（票 #22），不再是笼统的 INTERNAL_ERROR。
		if !terminal || OutcomeCode(*out) != CodeExecutorPodError {
			t.Fatalf("out = %+v code=%q", out, OutcomeCode(*out))
		}
		if out.K8sReason != "OOMKilled" {
			t.Fatalf("K8sReason = %q, 期望 OOMKilled（原样保留）", out.K8sReason)
		}
	})

	t.Run("image pull failure", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: "Back-off pulling image",
				}},
			}},
		}}
		out, terminal := classifyOutcome(nil, pod, time.Unix(1_700_000_000, 0))
		if !terminal || OutcomeCode(*out) != CodeImagePullFailed {
			t.Fatalf("out = %+v code=%q", out, OutcomeCode(*out))
		}
	})

	t.Run("job deadline exceeded", func(t *testing.T) {
		job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "DeadlineExceeded", Message: "deadline",
		}}}}
		out, terminal := classifyOutcome(job, nil, time.Unix(1_700_000_000, 0))
		if !terminal || out.Phase != PhaseTimeout || OutcomeCode(*out) != CodeTaskTimeout {
			t.Fatalf("out = %+v", out)
		}
	})

	t.Run("running pod is not terminal", func(t *testing.T) {
		job := &batchv1.Job{Status: batchv1.JobStatus{Active: 1}}
		pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if out, terminal := classifyOutcome(job, pod, time.Unix(1_700_000_000, 0)); terminal {
			t.Fatalf("running pod must not be terminal: %+v", out)
		}
	})
}

func TestOutcomeCodeDefaults(t *testing.T) {
	cases := []struct {
		out  ports.JobOutcome
		want string
	}{
		{ports.JobOutcome{Phase: PhaseSucceeded}, ""},
		{ports.JobOutcome{Phase: PhaseTimeout}, CodeTaskTimeout},
		{ports.JobOutcome{Phase: PhaseFailed}, CodeTesterNonZeroExit},
		{ports.JobOutcome{Phase: PhaseUnknown}, CodeInternalError},
		{ports.JobOutcome{Phase: PhaseFailed, Message: CodeMountUnavailable + ": no pvc"}, CodeMountUnavailable},
	}
	for _, tc := range cases {
		if got := OutcomeCode(tc.out); got != tc.want {
			t.Errorf("OutcomeCode(%+v) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

func TestRenderResourceRequestPassesThroughDomainTypes(t *testing.T) {
	// 编译期就确认 render 接受 domain.ResourceRequest / domain.MountSpec。
	spec := sampleSpec()
	spec.Mounts = []domain.MountSpec{{Name: "calib", MountPath: "/opt/calib", ReadOnly: true, Source: "calib-pvc"}}
	job, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "calib" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "calib-pvc" {
			found = true
		}
	}
	if !found {
		t.Errorf("extra mount volume missing: %+v", job.Spec.Template.Spec.Volumes)
	}
}

type observerFunc func(context.Context, ports.JobOutcome) error

func (f observerFunc) JobFinished(ctx context.Context, o ports.JobOutcome) error { return f(ctx, o) }

func repeatHex(n int) string {
	const alphabet = "abcdef0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[i%len(alphabet)]
	}
	return string(b)
}

// 票 #31：FinishedAt 必须来自注入时钟（不是直调 time.Now）。
func TestClassifyOutcomeUsesInjectedClock(t *testing.T) {
	fixed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	// Job 终态但没有 CompletionTime（失败/被删）→ 走时钟。
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			StartTime: &metav1.Time{Time: fixed.Add(-time.Minute)},
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	out, terminal := classifyOutcome(job, nil, fixed)
	if !terminal {
		t.Fatal("expected terminal outcome")
	}
	if !out.FinishedAt.Equal(fixed) {
		t.Fatalf("FinishedAt = %v, want %v（必须来自注入时钟）", out.FinishedAt, fixed)
	}

	// 有 CompletionTime 时以 K8s 事实为准（时钟只兜底）。
	completed := fixed.Add(-time.Hour)
	job2 := &batchv1.Job{
		Status: batchv1.JobStatus{
			CompletionTime: &metav1.Time{Time: completed},
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	out2, _ := classifyOutcome(job2, nil, fixed)
	if !out2.FinishedAt.Equal(completed) {
		t.Fatalf("FinishedAt = %v, want CompletionTime %v", out2.FinishedAt, completed)
	}
}
