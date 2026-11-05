package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// fakeDeployLogs 是 deployLogClient 的测试替身：可控 Pod 列表、日志内容与错误。
type fakeDeployLogs struct {
	pods     []corev1.Pod
	logs     map[string]string
	listErr  error
	logsErr  error
	lastTail int64
}

func (f *fakeDeployLogs) ListPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pods, nil
}

func (f *fakeDeployLogs) Logs(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error) {
	if f.logsErr != nil {
		return "", f.logsErr
	}
	f.lastTail = tailLines
	return f.logs[pod], nil
}

// seedDeployRun 造一个 deploy 形态的 Run，并把 runs.deploy 写成给定快照。
func seedDeployRun(t *testing.T, st *store.Store, deploy map[string]any) string {
	t.Helper()
	ctx := context.Background()
	runID := store.NewRunID(time.Now())
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "deploy-logs", Kind: "run",
			Manifest: []byte(`{}`), Resolved: []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: []*store.Task{{
			ID: store.NewTaskID(), Seq: 0, Name: "t", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			OnFailure: domain.OnFailureContinue, TimeoutSeconds: 60, Status: string(domain.TaskPending),
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	snap := domain.DeploySnapshot{}
	if v, ok := deploy["namespace"].(string); ok {
		snap.Namespace = v
	}
	if v, ok := deploy["release_name"].(string); ok {
		snap.ReleaseName = v
	}
	if v, ok := deploy["state"].(string); ok {
		snap.State = v
	}
	if v, ok := deploy["accepted_at"].(string); ok {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			snap.AcceptedAt = &ts
		}
	}
	if err := st.RecordDeployState(ctx, runID, snap); err != nil {
		t.Fatalf("RecordDeployState: %v", err)
	}
	return runID
}

// deployPod 造一个 Pod（名字带 release 前缀）。
func deployPod(name string, ready bool, restarts int32, started time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "testplane-mock",
			CreationTimestamp: metav1.NewTime(started),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
		Status: corev1.PodStatus{
			StartTime: &metav1.Time{Time: started},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "server", Ready: ready, RestartCount: restarts,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(started),
				}},
			}},
		},
	}
}

func getDeployLogs(t *testing.T, srv *Server, runID, query string) (*httptest.ResponseRecorder, DeployLogsResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/deploy/logs"+query, nil))
	var body DeployLogsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

// ① 正常返回：两个 Pod + entries。
func TestDeployLogs_TwoPods(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-abc", "state": "ready",
		"accepted_at": started.Add(-time.Minute).Format(time.RFC3339),
	})
	fake := &fakeDeployLogs{
		pods: []corev1.Pod{
			deployPod("tp-abc-stack-1", true, 0, started),
			deployPod("tp-abc-stack-2", true, 0, started),
		},
		logs: map[string]string{
			"tp-abc-stack-1": "l1\nl2\nl3\n",
			"tp-abc-stack-2": "m1\nm2\n",
		},
	}
	srv.deployLogsKube = fake

	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(body.Pods) != 2 {
		t.Fatalf("pods = %d, want 2", len(body.Pods))
	}
	if len(body.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(body.Entries), body.Entries)
	}
	if body.Entries[0].Pod != "tp-abc-stack-1" || body.Entries[0].LineCount != 3 {
		t.Errorf("entry[0] = %+v", body.Entries[0])
	}
	if !strings.Contains(body.Entries[1].Text, "m2") {
		t.Errorf("entry[1].text = %q", body.Entries[1].Text)
	}
}

// ② offset/limit 分页与 next_offset。
func TestDeployLogs_Pagination(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-page", "state": "ready",
	})
	fake := &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-page-stack-1", true, 0, started)},
		logs: map[string]string{"tp-page-stack-1": "a\nb\nc\nd\ne\n"},
	}
	srv.deployLogsKube = fake

	rec, body := getDeployLogs(t, srv, runID, "?offset=0&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !body.Truncated || body.NextOffset != 2 {
		t.Fatalf("truncated=%v next_offset=%d, want true/2", body.Truncated, body.NextOffset)
	}
	if body.Entries[0].LineCount != 2 || body.Entries[0].Text != "a\nb" {
		t.Errorf("first page = %+v", body.Entries[0])
	}

	rec, body = getDeployLogs(t, srv, runID, "?offset=2&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("page2 status = %d", rec.Code)
	}
	if body.Entries[0].Text != "c\nd" || body.NextOffset != 4 {
		t.Fatalf("page2 = %+v next=%d", body.Entries[0], body.NextOffset)
	}

	// 最后一页：没有更多 → truncated=false、next_offset=0。
	rec, body = getDeployLogs(t, srv, runID, "?offset=4&limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("page3 status = %d", rec.Code)
	}
	if body.Truncated || body.NextOffset != 0 {
		t.Fatalf("page3 truncated=%v next=%d, want false/0", body.Truncated, body.NextOffset)
	}
	if body.Entries[0].Text != "e" {
		t.Errorf("page3 text = %q", body.Entries[0].Text)
	}
}

// ③ 排队期：没有 Pod → 200 + 空列表 + message（不是 404）。
func TestDeployLogs_QueuedHasMessage(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-q", "state": "queued",
	})
	srv.deployLogsKube = &fakeDeployLogs{pods: nil}

	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(body.Pods) != 0 || len(body.Entries) != 0 {
		t.Fatalf("pods/entries must be empty: %+v %+v", body.Pods, body.Entries)
	}
	if !strings.Contains(body.Message, "排队") {
		t.Errorf("message = %q, want 排队说明", body.Message)
	}
}

// ④ restartCount>0 → 产生 gap（不假装连续）。
func TestDeployLogs_RestartProducesGap(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-gap", "state": "ready",
		"accepted_at": started.Add(-5 * time.Minute).Format(time.RFC3339),
	})
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-gap-stack-1", true, 3, started)},
		logs: map[string]string{"tp-gap-stack-1": "x\n"},
	}

	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	gap := findGap(body.Gaps, "tp-gap-stack-1", "pod restarted")
	if gap == nil {
		t.Fatalf("gaps = %+v, want a restart gap", body.Gaps)
	}
	if !gap.To.After(gap.From) && !gap.To.Equal(gap.From) {
		t.Errorf("gap window invalid: %+v", gap)
	}
}

// ⑤ Run 不存在 → 404。
func TestDeployLogs_RunNotFound(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()
	rec, _ := getDeployLogs(t, srv, "r-19990101-ZZZZ", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ⑥ 没有 pods/log 权限（Forbidden）→ 502 EXECUTOR_UNAVAILABLE（不是 500）。
func TestDeployLogs_ForbiddenIsExecutorUnavailable(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-f", "state": "ready",
	})
	srv.deployLogsKube = &fakeDeployLogs{
		listErr: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("no pods/log")),
	}
	rec, _ := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != domain.CodeExecutorUnavailable {
		t.Fatalf("code = %q, want %s", errBody.Code, domain.CodeExecutorUnavailable)
	}
}

func findGap(gaps []DeployLogGap, pod, reason string) *DeployLogGap {
	for i := range gaps {
		if gaps[i].Pod == pod && gaps[i].Reason == reason {
			return &gaps[i]
		}
	}
	return nil
}
