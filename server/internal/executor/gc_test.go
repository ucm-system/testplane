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

func finishedJob(name string, succeeded bool) *batchv1.Job {
	cond := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}
	if succeeded {
		cond = batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "testplane",
			Labels: map[string]string{LabelManaged: "true", LabelApp: "testplane", LabelJobID: "j-x"},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond}},
	}
}

func runningJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "testplane",
			Labels: map[string]string{LabelManaged: "true", LabelApp: "testplane"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
}

func gcExecutor(objs ...interface{}) (*Executor, *fake.Clientset) {
	return nil, nil
}

func TestDeleteIsIdempotentOn404(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ex := NewWithNamespace(cs, "testplane", Options{})
	if err := ex.Delete(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "missing"}, ports.DeleteBackground); err != nil {
		t.Fatalf("Delete on missing job must be nil (404 == success): %v", err)
	}
}

func TestDeleteRemovesJob(t *testing.T) {
	cs := fake.NewSimpleClientset(finishedJob("tp-t-1-a1", true))
	ex := NewWithNamespace(cs, "testplane", Options{})
	if err := ex.Delete(context.Background(), ports.JobHandle{Namespace: "testplane", JobName: "tp-t-1-a1"}, ports.DeleteBackground); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), "tp-t-1-a1", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("job still exists: %v", err)
	}
}

func TestDeleteRejectsUnknownPolicy(t *testing.T) {
	cs := fake.NewSimpleClientset(finishedJob("tp-t-1-a1", true))
	ex := NewWithNamespace(cs, "testplane", Options{})
	if err := ex.Delete(context.Background(), ports.JobHandle{JobName: "tp-t-1-a1"}, ports.DeletePolicy("sideways")); err == nil {
		t.Fatal("expected error for unknown policy")
	}
}

func TestCollectGarbageDeletesOnlyFinishedUnreferencedJobs(t *testing.T) {
	cs := fake.NewSimpleClientset(
		finishedJob("tp-done-terminal", true),   // 终态 + Attempt 终态 → 删
		finishedJob("tp-done-orphan", false),    // 终态 + DB 无 Attempt → 删（孤儿）
		finishedJob("tp-done-still-active", true), // 终态但 Attempt 还"活着" → 保留
		runningJob("tp-running"),                // 还在跑 → 保留
	)
	ex := NewWithNamespace(cs, "testplane", Options{})
	report, err := ex.CollectGarbage(context.Background(), func(ctx context.Context, jobName string) (bool, bool, error) {
		switch jobName {
		case "tp-done-terminal":
			return true, true, nil
		case "tp-done-orphan":
			return false, false, nil
		case "tp-done-still-active":
			return true, false, nil
		default:
			return true, false, nil
		}
	})
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.DeletedJobs != 2 {
		t.Fatalf("deleted jobs = %d, want 2 (%+v)", report.DeletedJobs, report)
	}
	for _, name := range []string{"tp-done-terminal", "tp-done-orphan"} {
		if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("job %s should be deleted (err=%v)", name, err)
		}
	}
	for _, name := range []string{"tp-done-still-active", "tp-running"} {
		if _, err := cs.BatchV1().Jobs("testplane").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Errorf("job %s must be kept: %v", name, err)
		}
	}
}

func TestCollectGarbageRemovesOrphanPods(t *testing.T) {
	orphanPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tp-t-9-a1-xxx", Namespace: "testplane",
			Labels: map[string]string{LabelManaged: "true", labelJobName: "tp-gone"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	runningJob := runningJob("tp-live")
	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tp-live-abc", Namespace: "testplane",
			Labels: map[string]string{LabelManaged: "true", labelJobName: "tp-live"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewSimpleClientset(runningJob, orphanPod, livePod)
	ex := NewWithNamespace(cs, "testplane", Options{})
	report, err := ex.CollectGarbage(context.Background(), func(context.Context, string) (bool, bool, error) {
		return true, false, nil
	})
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.DeletedPods != 1 {
		t.Fatalf("deleted pods = %d, want 1 (%+v)", report.DeletedPods, report)
	}
	if _, err := cs.CoreV1().Pods("testplane").Get(context.Background(), "tp-t-9-a1-xxx", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphan pod must be deleted")
	}
	if _, err := cs.CoreV1().Pods("testplane").Get(context.Background(), "tp-live-abc", metav1.GetOptions{}); err != nil {
		t.Errorf("pod of a live job must be kept: %v", err)
	}
}

func TestListManagedJobs(t *testing.T) {
	cs := fake.NewSimpleClientset(finishedJob("tp-a", true), runningJob("tp-b"))
	ex := NewWithNamespace(cs, "testplane", Options{})
	jobs, err := ex.ListManagedJobs(context.Background())
	if err != nil {
		t.Fatalf("ListManagedJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	if !jobs[0].Finished || jobs[1].Finished {
		t.Errorf("finished flags = %+v", jobs)
	}
	_ = time.Now()
}
