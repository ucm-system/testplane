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
	// seenNamespaces 记录客户端真正读 Pod/日志用的 namespace
	// （断言"用的是 runs.deploy.namespace 而不是配置回退"）。
	seenNamespaces []string
}

func (f *fakeDeployLogs) ListPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	f.seenNamespaces = append(f.seenNamespaces, namespace)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pods, nil
}

func (f *fakeDeployLogs) Logs(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error) {
	f.seenNamespaces = append(f.seenNamespaces, namespace)
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

	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-abc-stack-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(body.Pods) != 2 {
		t.Fatalf("pods = %d, want 2", len(body.Pods))
	}
	if len(body.Entries) != 1 {
		t.Fatalf("entries = %d, want 1（每次只服务一个 Pod）: %+v", len(body.Entries), body.Entries)
	}
	if body.Entries[0].Pod != "tp-abc-stack-1" || body.Entries[0].LineCount != 3 {
		t.Errorf("entry[0] = %+v", body.Entries[0])
	}
	// 另一个 Pod 的日志通过 ?pod= 取，仍然可达。
	rec, body = getDeployLogs(t, srv, runID, "?pod=tp-abc-stack-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("pod2 status = %d", rec.Code)
	}
	if len(body.Entries) != 1 || !strings.Contains(body.Entries[0].Text, "m2") {
		t.Fatalf("pod2 entries = %+v", body.Entries)
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

	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-page-stack-1&offset=0&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !body.Truncated || body.NextOffset != 2 {
		t.Fatalf("truncated=%v next_offset=%d, want true/2", body.Truncated, body.NextOffset)
	}
	if body.Entries[0].LineCount != 2 || body.Entries[0].Text != "a\nb" {
		t.Errorf("first page = %+v", body.Entries[0])
	}

	rec, body = getDeployLogs(t, srv, runID, "?pod=tp-page-stack-1&offset=2&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("page2 status = %d", rec.Code)
	}
	if body.Entries[0].Text != "c\nd" || body.NextOffset != 4 {
		t.Fatalf("page2 = %+v next=%d", body.Entries[0], body.NextOffset)
	}

	// 最后一页：没有更多 → truncated=false、next_offset=0。
	rec, body = getDeployLogs(t, srv, runID, "?pod=tp-page-stack-1&offset=4&limit=10")
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

	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-q-stack-1")
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

	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-gap-stack-1")
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
	rec, _ := getDeployLogs(t, srv, runID, "?pod=tp-f-stack-1")
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

// 评审 #4 的核心回归：两 Pod、limit 小于单 Pod 行数时，**第二个 Pod 的行必须能被取到**。
//
// 修前（多 Pod 共用一个 offset + next_offset 取 max）：p1=5 行、p2=2 行、limit=2 时，
// p2 一行都不会出现；修后（?pod= 必选、游标只在单 Pod 内）p2 用自己的 offset 直接可达。
func TestDeployLogs_TwoPodsPaging(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-two", "state": "ready",
		"accepted_at": started.Add(-time.Minute).Format(time.RFC3339),
	})
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{
			deployPod("tp-two-stack-1", true, 0, started),
			deployPod("tp-two-stack-2", true, 0, started),
		},
		logs: map[string]string{
			"tp-two-stack-1": "1a\n1b\n1c\n1d\n1e\n", // 5 行
			"tp-two-stack-2": "2a\n2b\n",             // 2 行（旧实现里永远轮不到）
		},
	}

	// p1 翻页：每次 2 行，走完自己的 5 行。
	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-two-stack-1&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("p1 page1 = %d", rec.Code)
	}
	if len(body.Pods) != 2 {
		t.Fatalf("pods = %d, want 2（pods[] 永远返回全部，供页签）", len(body.Pods))
	}
	if body.Entries[0].Text != "1a\n1b" || body.NextOffset != 2 {
		t.Fatalf("p1 page1 = %+v next=%d", body.Entries[0], body.NextOffset)
	}
	rec, body = getDeployLogs(t, srv, runID, "?pod=tp-two-stack-1&offset=2&limit=2")
	if rec.Code != http.StatusOK || body.Entries[0].Text != "1c\n1d" || body.NextOffset != 4 {
		t.Fatalf("p1 page2 = %+v next=%d", body.Entries, body.NextOffset)
	}

	// p2 用自己的 offset：一行都不丢（这就是评审点名的验收）。
	rec, body = getDeployLogs(t, srv, runID, "?pod=tp-two-stack-2&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("p2 = %d, want 200", rec.Code)
	}
	if len(body.Entries) != 1 || body.Entries[0].Pod != "tp-two-stack-2" {
		t.Fatalf("p2 entries = %+v", body.Entries)
	}
	if body.Entries[0].Text != "2a\n2b" {
		t.Fatalf("p2 text = %q, want 两行都在", body.Entries[0].Text)
	}
	if body.Truncated || body.NextOffset != 0 {
		t.Fatalf("p2 truncated=%v next=%d, want false/0", body.Truncated, body.NextOffset)
	}
}

// tail_lines 与 offset 互斥：同时给出 → 400（不让人猜"窗口内行号"）。
func TestDeployLogs_TailLinesConflictsWithOffset(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-tail", "state": "ready",
	})
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-tail-stack-1", true, 0, started)},
		logs: map[string]string{"tp-tail-stack-1": "a\nb\nc\n"},
	}
	rec, _ := getDeployLogs(t, srv, runID, "?pod=tp-tail-stack-1&tail_lines=2&offset=1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != domain.CodeValidationError {
		t.Errorf("code = %q", errBody.Code)
	}
	// 只给 tail_lines（不带 offset）→ 正常，且确实把 tail_lines 传给了 K8s。
	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-tail-stack-1&tail_lines=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("tail-only status = %d", rec.Code)
	}
	if srv.deployLogsKube.(*fakeDeployLogs).lastTail != 2 {
		t.Errorf("tail_lines 未透传给 K8s: %d", srv.deployLogsKube.(*fakeDeployLogs).lastTail)
	}
	if len(body.Entries) != 1 {
		t.Errorf("entries = %+v", body.Entries)
	}
}

// `?pod=` 缺省 + **只有 1 个 Pod** → 直接服务它（票 #40-5：CLI/agent 不必先查 Pod 名）。
func TestDeployLogs_PodOptionalSinglePod(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-one", "state": "ready",
	})
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-one-stack-1", true, 0, time.Now())},
		logs: map[string]string{"tp-one-stack-1": "only1\nonly2\n"},
	}
	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（单 Pod 不必指定 pod）: %s", rec.Code, rec.Body.String())
	}
	if len(body.Entries) != 1 || body.Entries[0].Pod != "tp-one-stack-1" {
		t.Fatalf("entries = %+v, want 服务唯一那个 Pod", body.Entries)
	}
	if !strings.Contains(body.Entries[0].Text, "only2") {
		t.Fatalf("text = %q", body.Entries[0].Text)
	}
}

// `?pod=` 缺省 + **多个 Pod** → 409 CONFLICT（pod_ambiguous + available_pods），绝不默默取第一个。
func TestDeployLogs_PodOptionalAmbiguousConflict(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-many", "state": "ready",
	})
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{
			deployPod("tp-many-stack-1", true, 0, started),
			deployPod("tp-many-stack-2", true, 0, started),
		},
		logs: map[string]string{"tp-many-stack-1": "a\n", "tp-many-stack-2": "b\n"},
	}
	rec, _ := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409（多 Pod 缺省必须要求指定）: %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Code   string         `json:"code"`
		Detail map[string]any `json:"detail"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Detail["reason"] != "pod_ambiguous" {
		t.Fatalf("detail = %v, want reason=pod_ambiguous", errBody.Detail)
	}
	pods, _ := errBody.Detail["available_pods"].([]any)
	if len(pods) != 2 {
		t.Fatalf("available_pods = %v, want 两个可选 Pod", errBody.Detail["available_pods"])
	}
}

// `?pod=` 缺省 + **0 个 Pod** → 200 + 排队/回收的 message（不是 409/404）。
func TestDeployLogs_PodOptionalNoPods(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-zero", "state": "queued",
	})
	srv.deployLogsKube = &fakeDeployLogs{pods: nil}
	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(body.Message, "排队") {
		t.Fatalf("message = %q, want 排队说明", body.Message)
	}
	if body.Window != "full" {
		t.Fatalf("window = %q, want full（排队期也是全文窗口）", body.Window)
	}
}

// tail 窗口一次性：`truncated=false`、`next_offset=0`、`window=tail`（票 #40-4）。
func TestDeployLogs_TailWindowIsOneShot(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-tail2", "state": "ready",
	})
	// tail_lines=50 > limit=10：旧实现会返回 10 行 + next_offset>0（自相矛盾）。
	lines := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		lines = append(lines, fmt.Sprintf("L%02d", i))
	}
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-tail2-stack-1", true, 0, started)},
		logs: map[string]string{"tp-tail2-stack-1": strings.Join(lines, "\n") + "\n"},
	}
	rec, body := getDeployLogs(t, srv, runID, "?tail_lines=50&limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body.Window != "tail" {
		t.Fatalf("window = %q, want tail", body.Window)
	}
	if body.Truncated || body.NextOffset != 0 {
		t.Fatalf("truncated=%v next_offset=%d，tail 窗口必须一次性（false/0）", body.Truncated, body.NextOffset)
	}
	if len(body.Entries) != 1 || body.Entries[0].LineCount != 50 {
		t.Fatalf("entries = %+v, want 整个 tail 窗口 50 行", body.Entries)
	}
	// 全文窗口仍可续页，且 window=full。
	rec, body = getDeployLogs(t, srv, runID, "?offset=0&limit=10")
	if rec.Code != http.StatusOK || body.Window != "full" || !body.Truncated || body.NextOffset != 10 {
		t.Fatalf("full 页 = window:%q truncated:%v next:%d", body.Window, body.Truncated, body.NextOffset)
	}
}

// 指定的 Pod 不在当前列表里（打错 / 已回收）→ 404，并给出可选 Pod。
func TestDeployLogs_PodNotFound(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-nf", "state": "ready",
	})
	srv.deployLogsKube = &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-nf-stack-1", true, 0, time.Now())},
		logs: map[string]string{"tp-nf-stack-1": "x\n"},
	}
	rec, _ := getDeployLogs(t, srv, runID, "?pod=tp-nf-ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var errBody struct {
		Detail map[string]any `json:"detail"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Detail["reason"] != "pod_not_found" {
		t.Errorf("detail = %v", errBody.Detail)
	}
}

// Pod 已被回收（列表为空且 deploy 已 ready）→ 200 + 显式 message + gap reason，
// 不静默返回空数组（MVP 直读 pods/log 的已知偏差，IMPLEMENTATION §15.2）。
func TestDeployLogs_RecycledPodIsExplicit(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	accepted := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-gone", "state": "ready",
		"accepted_at": accepted.Format(time.RFC3339),
	})
	srv.deployLogsKube = &fakeDeployLogs{pods: nil}

	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-gone-stack-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body.Message, "回收") {
		t.Fatalf("message = %q, want 明确说 Pod 已回收", body.Message)
	}
	if len(body.Gaps) != 1 || !strings.Contains(body.Gaps[0].Reason, "pod recycled") {
		t.Fatalf("gaps = %+v, want reason 说明已回收", body.Gaps)
	}
}

// ---- runs.deploy.namespace 写入/读取（QA deploy-logs-check 的缺口）----------------
//
// Deploy 的 Pod 在 **Genet 的 namespace** 里；`runs.deploy.namespace` 一度"结构体有、没人写"，
// 端点就回退到 TESTPLANE_NAMESPACE（testplane），结果 Pod 明明在 testplane-mock 里跑却读不到日志。

// ② `deploy/logs` 必须用 runs.deploy.namespace（Genet 的 ns），而不是配置回退。
func TestDeployLogs_UsesDeployNamespace(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	started := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-ns", "state": "ready",
	})
	fake := &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-ns-stack-1", true, 0, started)},
		logs: map[string]string{"tp-ns-stack-1": "ns-line\n"},
	}
	srv.deployLogsKube = fake

	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fake.seenNamespaces) == 0 {
		t.Fatal("fake client 没被调用")
	}
	for _, ns := range fake.seenNamespaces {
		if ns != "testplane-mock" {
			t.Fatalf("读取用的 namespace = %q，want testplane-mock（不能用配置回退）; 全部=%v", ns, fake.seenNamespaces)
		}
	}
	if strings.Contains(body.Message, "��有 namespace") {
		t.Fatalf("message = %q，不该出现 namespace 回退提示", body.Message)
	}
}

// ③ deploy 快照缺 namespace → 回退并**显式说明**（不静默）。
func TestDeployLogs_NamespaceFallbackIsExplained(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	// 只有 release_name、没有 namespace：模拟旧数据 / 写入端漏写。
	runID := seedDeployRun(t, st, map[string]any{
		"release_name": "tp-nonso", "state": "ready",
	})
	fake := &fakeDeployLogs{pods: nil}
	srv.deployLogsKube = fake

	rec, body := getDeployLogs(t, srv, runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(body.Message, "回退") {
		t.Fatalf("message = %q，缺 namespace 时必须在响应里说明是回退", body.Message)
	}
	// 回退目标是平台 namespace（integrationServer 的 Config 默认 testplane）
	for _, ns := range fake.seenNamespaces {
		if ns != "testplane" {
			t.Fatalf("回退 namespace = %q，want testplane", ns)
		}
	}
}

// ① `GET /runs/{id}/deploy` 里 namespace 非空，且等于 Genet 返回的 ns（写入端已验证；
// 这里验证投影端点把它原样暴露给调用方，QA 的 deploy-logs-check 靠这条链路定位 Pod）。
func TestRunDeployExposesNamespace(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-proj", "state": "ready",
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/deploy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Deploy map[string]any `json:"deploy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Deploy["namespace"] != "testplane-mock" {
		t.Fatalf("deploy.namespace = %v, want testplane-mock", body.Deploy["namespace"])
	}
}
