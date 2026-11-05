package executor

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// finishedJobAt 造一个终态 Job，创建时间可控（GC 的 1h 窗口要用）。
func finishedJobAt(name string, succeeded bool, age time.Duration) *batchv1.Job {
	cond := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}
	if succeeded {
		cond = batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "testplane",
			Labels:            map[string]string{LabelManaged: "true", LabelApp: "testplane", LabelJobID: "j-x"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond}},
	}
}

func finishedJob(name string, succeeded bool) *batchv1.Job {
	return finishedJobAt(name, succeeded, 2*time.Hour)
}

func runningJobAt(name string, age time.Duration) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "testplane",
			Labels:            map[string]string{LabelManaged: "true", LabelApp: "testplane"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Status: batchv1.JobStatus{Active: 1},
	}
}

func runningJob(name string) *batchv1.Job { return runningJobAt(name, time.Minute) }

// gcExecutor 造一个真实 Executor + fake clientset（票 #20-7：不要 `return nil, nil` 的空 helper）。
func gcExecutor(t *testing.T, objs ...interface{}) (*Executor, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	for _, o := range objs {
		switch v := o.(type) {
		case *batchv1.Job:
			if _, err := cs.BatchV1().Jobs("testplane").Create(context.Background(), v, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed job %s: %v", v.Name, err)
			}
		case *corev1.Pod:
			if _, err := cs.CoreV1().Pods("testplane").Create(context.Background(), v, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed pod %s: %v", v.Name, err)
			}
		default:
			t.Fatalf("gcExecutor: unsupported seed type %T", o)
		}
	}
	return NewWithNamespace(cs, "testplane", Options{}), cs
}

// states 是批量查询的固定返回值。
func states(m map[string]AttemptState) AttemptStatesFunc {
	return func(ctx context.Context, names []string) (map[string]AttemptState, error) {
		out := map[string]AttemptState{}
		for _, n := range names {
			if s, ok := m[n]; ok {
				out[n] = s
			}
		}
		return out, nil
	}
}

func TestDeleteIsIdempotentOn404(t *testing.T) {
	ex, _ := gcExecutor(t)
	if err := ex.Delete(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "missing"}, ports.DeleteBackground); err != nil {
		t.Fatalf("Delete on missing job must be nil (404 == success): %v", err)
	}
}

func TestDeleteRemovesJob(t *testing.T) {
	ex, cs := gcExecutor(t, finishedJob("tp-t-1-a1", true))
	if err := ex.Delete(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "tp-t-1-a1"}, ports.DeleteBackground); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-t-1-a1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("job still exists after Delete: %v", err)
	}
}

func TestDeleteRejectsUnknownPolicy(t *testing.T) {
	ex, _ := gcExecutor(t, finishedJob("tp-t-1-a1", true))
	if err := ex.Delete(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "tp-t-1-a1"}, ports.DeletePolicy("sideways")); err == nil {
		t.Fatal("expected error for unknown delete policy")
	}
}

// 票 #20-1：hold-until 未过期 → 不许删（日志 follower 未收尾）。
func TestCollectGarbageSkipsHeldJobs(t *testing.T) {
	job := finishedJob("tp-held", true)
	job.Annotations = map[string]string{HoldUntilAnnotation: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}
	ex, cs := gcExecutor(t, job)

	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-held": {Found: true, Terminal: true}}),
	})
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.DeletedJobs != 0 {
		t.Fatalf("deleted %d jobs, want 0 (hold-until 未过期)", report.DeletedJobs)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-held", metav1.GetOptions{}); err != nil {
		t.Fatalf("held job must still exist: %v", err)
	}
}

// hold-until 已过期 → 照常回收。
func TestCollectGarbageDeletesExpiredHold(t *testing.T) {
	job := finishedJob("tp-held-expired", true)
	job.Annotations = map[string]string{HoldUntilAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	ex, _ := gcExecutor(t, job)
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-held-expired": {Found: true, Terminal: true}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedJobs != 1 {
		t.Fatalf("deleted = %d, want 1", report.DeletedJobs)
	}
}

// 票 #20-2：终态 Job 但创建不到 1h → 不删（finalizeAttemptAsync 是异步的，抢跑会丢日志）。
func TestCollectGarbageKeepsYoungTerminalJob(t *testing.T) {
	ex, cs := gcExecutor(t, finishedJobAt("tp-young", true, 30*time.Minute))
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-young": {Found: true, Terminal: true}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedJobs != 0 {
		t.Fatalf("deleted %d, want 0 (age < 1h)", report.DeletedJobs)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-young", metav1.GetOptions{}); err != nil {
		t.Fatalf("young job must still exist: %v", err)
	}
}

// 终态 + 超过 1h + Attempt 已终态 → 删。
func TestCollectGarbageDeletesOldTerminalJob(t *testing.T) {
	ex, cs := gcExecutor(t, finishedJobAt("tp-old", true, 2*time.Hour))
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-old": {Found: true, Terminal: true}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedJobs != 1 {
		t.Fatalf("deleted = %d, want 1", report.DeletedJobs)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-old", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("job should be gone: %v", err)
	}
}

// 终态但 Attempt 还没终态 → 留着（结论没落库，多留一个 Job 比丢结论便宜）。
func TestCollectGarbageKeepsJobWithActiveAttempt(t *testing.T) {
	ex, _ := gcExecutor(t, finishedJobAt("tp-active", true, 2*time.Hour))
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-active": {Found: true, Terminal: false}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedJobs != 0 {
		t.Fatalf("deleted %d, want 0 (Attempt 未终态)", report.DeletedJobs)
	}
}

// 票 #20-3（前半）：managed=true 但 DB 无 attempt → 立即删（超过 1 分钟容忍期）。
func TestCollectGarbageDeletesOrphanJob(t *testing.T) {
	ex, cs := gcExecutor(t, finishedJobAt("tp-orphan", true, 10*time.Minute))
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{}), // 库里没有对应 Attempt
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedJobs != 1 {
		t.Fatalf("deleted = %d, want 1（孤儿 Job 立即删）", report.DeletedJobs)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-orphan", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan job should be gone: %v", err)
	}
}

// 票 #20-3（后半）：运行中且超过 25h → 修状态为 TASK_TIMEOUT 后删。
func TestCollectGarbageRepairsStaleRunningJob(t *testing.T) {
	ex, cs := gcExecutor(t, runningJobAt("tp-stale", 26*time.Hour))
	repaired := false
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-stale": {Found: true, Terminal: false}}),
		OnStaleRunning: func(ctx context.Context, job ManagedJob) error {
			if job.Name != "tp-stale" {
				t.Errorf("repair job = %q", job.Name)
			}
			repaired = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("OnStaleRunning must be called for a running job older than 25h")
	}
	if report.DeletedJobs != 1 {
		t.Fatalf("deleted = %d, want 1", report.DeletedJobs)
	}
	if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-stale", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale job should be gone: %v", err)
	}
}

// 运行中且不到 25h → 不动。
func TestCollectGarbageKeepsRunningJob(t *testing.T) {
	ex, _ := gcExecutor(t, runningJobAt("tp-running", 2*time.Hour))
	report, err := ex.CollectGarbage(context.Background(), GCOptions{
		AttemptStates: states(map[string]AttemptState{"tp-running": {Found: true, Terminal: false}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedJobs != 0 {
		t.Fatalf("deleted %d, want 0", report.DeletedJobs)
	}
}

// 票 #20-5：批量查询只发一次（不是每 Job 一次）。
func TestCollectGarbageQueriesAttemptStatesOnce(t *testing.T) {
	ex, _ := gcExecutor(t,
		finishedJobAt("tp-a", true, 2*time.Hour),
		finishedJobAt("tp-b", true, 2*time.Hour),
		finishedJobAt("tp-c", true, 2*time.Hour),
	)
	calls := 0
	var seen []string
	batch := func(ctx context.Context, names []string) (map[string]AttemptState, error) {
		calls++
		seen = append(seen, names...)
		out := map[string]AttemptState{}
		for _, n := range names {
			out[n] = AttemptState{Found: true, Terminal: true}
		}
		return out, nil
	}
	report, err := ex.CollectGarbage(context.Background(), GCOptions{AttemptStates: batch})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("AttemptStates called %d times, want 1（批量查询，票 #20-5）", calls)
	}
	if len(seen) != 3 {
		t.Fatalf("batch saw %d names, want 3: %v", len(seen), seen)
	}
	if report.DeletedJobs != 3 {
		t.Fatalf("deleted = %d, want 3", report.DeletedJobs)
	}
}

// 孤儿 Pod：所属 Job 已不存在且非 Running → 删。
func TestCollectGarbageRemovesOrphanPods(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tp-t-1-a1-pod", Namespace: "testplane",
			Labels: map[string]string{LabelManaged: "true", LabelJobID: "j-x", labelJobNameV2: "tp-t-1-a1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	ex, cs := gcExecutor(t, pod)
	report, err := ex.CollectGarbage(context.Background(), GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedPods != 1 {
		t.Fatalf("deleted pods = %d, want 1", report.DeletedPods)
	}
	if _, err := cs.CoreV1().Pods("testplane").Get(context.Background(), pod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan pod should be gone: %v", err)
	}
}

// hold-until 解析：RFC3339 与 Unix 秒都认，非法值当没有守卫。
func TestParseHoldUntil(t *testing.T) {
	if _, ok := ParseHoldUntil(""); ok {
		t.Error("empty must not parse")
	}
	if _, ok := ParseHoldUntil("not-a-time"); ok {
		t.Error("garbage must not parse")
	}
	ts := time.Now().UTC().Truncate(time.Second)
	if got, ok := ParseHoldUntil(ts.Format(time.RFC3339)); !ok || !got.Equal(ts) {
		t.Errorf("RFC3339 parse = %v, %v", got, ok)
	}
	if got, ok := ParseHoldUntil("1700000000"); !ok || got.Unix() != 1700000000 {
		t.Errorf("unix parse = %v, %v", got, ok)
	}
}

// HoldActive 读 Job 注解；Job 不存在视为没有守卫。
func TestHoldActive(t *testing.T) {
	held := finishedJobAt("tp-held", true, time.Minute)
	held.Annotations = map[string]string{HoldUntilAnnotation: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	ex, _ := gcExecutor(t, held)
	ok, err := ex.HoldActive(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "tp-held"})
	if err != nil || !ok {
		t.Fatalf("HoldActive = %v, %v; want true", ok, err)
	}
	ok, err = ex.HoldActive(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "tp-missing"})
	if err != nil || ok {
		t.Fatalf("missing job HoldActive = %v, %v; want false, nil", ok, err)
	}
}

// ListManagedJobs 是 GC 的观测面：列出带 label 的 Job 与终态。
func TestListManagedJobs(t *testing.T) {
	ex, _ := gcExecutor(t,
		finishedJobAt("tp-b", true, time.Hour),
		runningJob("tp-a"),
	)
	jobs, err := ex.ListManagedJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Name != "tp-a" || jobs[1].Name != "tp-b" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Finished || !jobs[1].Finished {
		t.Errorf("finished flags wrong: %+v", jobs)
	}
}
