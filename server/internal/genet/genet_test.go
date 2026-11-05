package genet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// fakeGenet 是 httptest 上的假 Genet：覆盖 §9 需要的全部观测形态。
type fakeGenet struct {
	mu sync.Mutex

	release *Release // nil 表示 404
	gets    int32
	posts   int32
	deletes int32

	// postBehavior："" = 202 成功；"exists" = 400 already has protected metadata。
	postBehavior string
	// deleteBehavior："" = 200；"404" = 404；"409" = 409 in progress。
	deleteBehavior string
	// queue 是 /api/open/queue 的返回（nil 表示不启用）。
	queue *QueueSnapshot
	// lastAuth 记录最近一次 Authorization 头（鉴权断言用）。
	lastAuth string
}

func (f *fakeGenet) setRelease(rel *Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release = rel
}

func (f *fakeGenet) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/open/helm/releases/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodGet:
			atomic.AddInt32(&f.gets, 1)
			rel := f.release
			f.mu.Unlock()
			if rel == nil {
				http.Error(w, `{"message":"helm release not found"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rel)
			return
		case http.MethodDelete:
			atomic.AddInt32(&f.deletes, 1)
			behavior := f.deleteBehavior
			f.mu.Unlock()
			switch behavior {
			case "404":
				http.Error(w, `{"message":"helm release not found"}`, http.StatusNotFound)
			case "409":
				http.Error(w, `{"message":"protected Helm operation is in progress"}`, http.StatusConflict)
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"message":"uninstalled","forcedPods":["p1"],"secretsDeleted":2,"cleanupErrors":[]}`))
			}
			return
		default:
			f.mu.Unlock()
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/open/helm/uc-packages/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		atomic.AddInt32(&f.posts, 1)
		behavior := f.postBehavior
		f.mu.Unlock()
		if behavior == "exists" {
			// 模拟"同名的 release 已经存在"：POST 拒绝的同时它已在 Genet 侧存在。
			f.mu.Lock()
			f.release = &Release{Name: "tp-test", Namespace: "user-alice", Status: ReleasePendingInstall}
			f.mu.Unlock()
			http.Error(w, `{"message":"release \"x\" already has protected metadata; install cannot replace an existing release"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"name":"tp-test","namespace":"user-alice","packageType":"stack","chart":"charts/x","chartVersion":"0.0.1","status":"pending-install"}`))
	})
	mux.HandleFunc("/api/open/queue/podgroups", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		q := f.queue
		f.mu.Unlock()
		if q == nil {
			http.Error(w, "not enabled", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "podGroups": q.PodGroups})
	})
	mux.HandleFunc("/api/open/queue", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		q := f.queue
		f.mu.Unlock()
		if q == nil {
			http.Error(w, "not enabled", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "queue": q.Queue})
	})
	return httptest.NewServer(mux)
}

// testClock 是可推进的假时钟。
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// newTestClient 构造指向假 Genet 的客户端。
func newTestClient(t *testing.T, f *fakeGenet, clock *testClock) (*Client, *httptest.Server) {
	t.Helper()
	srv := f.server(t)
	t.Cleanup(srv.Close)
	c, err := New(Options{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		HTTPClient: srv.Client(),
		Now:        clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func manifestJSON(t *testing.T, man DeployManifest) []byte {
	t.Helper()
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func pendingRelease(name, status string, pods []Pod) *Release {
	return &Release{
		Name: name, Namespace: "user-alice", PackageType: "stack",
		Chart: "charts/x", ChartVersion: "0.0.1", Status: status,
		CallInfo: &CallInfo{Endpoint: &Endpoint{Status: EndpointPending}},
		Pods:     pods,
	}
}

// ---- §9.3 状态合成：九行映射 ---------------------------------------------

func TestPollNotFoundWithinGraceIsAccepted(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)

	if err := c.Start(context.Background(), ports.DeployRequest{
		RunID: "r-1", DeployID: "d-1", Manifest: manifestJSON(t, DeployManifest{PackageType: "stack", Values: "a: b"}),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start 里 GET 404 后 POST 成功，但假 Genet 不落 release → Poll 仍是 404。
	st, err := c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if st.Phase != PhaseAccepted {
		t.Fatalf("phase = %s, want accepted (404 within 180s grace)", st.Phase)
	}
	// 超过宽限仍未出现 → blocked。
	clock.Advance(181 * time.Second)
	st, err = c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if st.Phase != PhaseBlocked || st.Failure == nil || st.Failure.Code != domain.CodeDeployBlocked {
		t.Fatalf("state = %+v, want blocked/DEPLOY_BLOCKED", st)
	}
	if st.Failure.Retryable != true {
		t.Errorf("DEPLOY_BLOCKED must be retryable")
	}
}

func TestPollPendingUnscheduledIsQueued(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(pendingRelease("tp-x", ReleasePendingInstall, []Pod{
		{Name: "p0", Phase: "Pending", NodeName: ""},
	}))
	f.queue = &QueueSnapshot{
		Enabled: true, QueueName: "genet-shared",
		Queue:     &Queue{Name: "genet-shared", State: "Open", Pending: 3, Inqueue: 2, Running: 7},
		PodGroups: []PodGroup{{Name: "pg-1", Phase: "Inqueue", Conditions: []Condition{{Message: "insufficient huawei.com/Ascend910"}}}},
	}

	st, err := c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if st.Phase != PhaseQueued {
		t.Fatalf("phase = %s, want queued", st.Phase)
	}
	if !strings.Contains(st.Reason, "Inqueue") || !strings.Contains(st.Reason, "Ascend910") {
		t.Errorf("reason must carry PodGroup phase/condition, got %q", st.Reason)
	}
	// 没有位次：QueueSnapshot 没有 position 字段，reason 里也不该出现 "第 N 位"。
	if strings.Contains(st.Reason, "position") {
		t.Errorf("reason must not promise a queue position: %q", st.Reason)
	}
}

func TestPollPendingScheduledIsDeploying(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(pendingRelease("tp-x", ReleasePendingInstall, []Pod{
		{Name: "p0", Phase: "Running", NodeName: "gpu-node-1"},
	}))
	st, err := c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseDeploying {
		t.Fatalf("phase = %s, want deploying (pod scheduled)", st.Phase)
	}
}

func TestPollDeployedWithPendingEndpointIsDeploying(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(pendingRelease("tp-x", ReleaseDeployed, []Pod{{Name: "p0", NodeName: "gpu-node-1"}}))

	st, err := c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseDeploying || st.Ready {
		t.Fatalf("state = %+v, want deploying (deployed≠ready when endpoint pending)", st)
	}
}

func TestPollEndpointReadyIsReady(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(&Release{
		Name: "tp-x", Namespace: "user-alice", Status: ReleaseDeployed,
		CallInfo: &CallInfo{
			Model:    "Qwen3-32B",
			Endpoint: &Endpoint{Status: EndpointReady, Source: "external", URL: "http://141.111.32.68:23345", Host: "141.111.32.68", Port: 23345},
		},
		Pods: []Pod{{Name: "p0", NodeName: "gpu-node-1", Ready: true}},
	})

	st, err := c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ready || st.Phase != PhaseReady {
		t.Fatalf("state = %+v, want ready", st)
	}
	if st.Endpoint.BaseURL != "http://141.111.32.68:23345" {
		t.Errorf("endpoint url = %q", st.Endpoint.BaseURL)
	}
	if st.Endpoint.ModelName != "Qwen3-32B" {
		t.Errorf("model name = %q, want from callInfo.model", st.Endpoint.ModelName)
	}
	if st.Endpoint.Key != "http://141.111.32.68:23345" && st.Endpoint.Key != "http://141.111.32.68" {
		t.Errorf("endpoint key = %q", st.Endpoint.Key)
	}
}

func TestPollEndpointBlockedIsBlocked(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(&Release{
		Name: "tp-x", Status: ReleaseDeployed,
		CallInfo: &CallInfo{Endpoint: &Endpoint{Status: EndpointBlocked, Message: "服务 \"kthena-router\" not found"}},
	})
	st, _ := c.Poll(context.Background(), "r-1")
	if st.Phase != PhaseBlocked || st.Failure == nil {
		t.Fatalf("state = %+v, want blocked", st)
	}
	if st.Failure.Code != domain.CodeDeployBlocked || st.Failure.Retryable != true {
		t.Errorf("failure = %+v", st.Failure)
	}
	if !strings.Contains(st.Failure.Message, "kthena-router") {
		t.Errorf("message must carry endpoint.message, got %q", st.Failure.Message)
	}
	if reason, _ := st.Failure.Detail["reason"].(string); reason != "blocked" {
		t.Errorf("detail.reason = %v, want blocked", st.Failure.Detail["reason"])
	}
}

func TestPollTerminalReleaseStatusesFoldToBlocked(t *testing.T) {
	cases := map[string]string{
		ReleaseFailed:       "blocked",
		ReleaseSuperseded:   "superseded",
		ReleaseUninstalling: "uninstalling",
		ReleaseUninstalled:  "uninstalled",
	}
	for releaseStatus, wantReason := range cases {
		t.Run(releaseStatus, func(t *testing.T) {
			f := &fakeGenet{}
			clock := &testClock{now: time.Unix(1_700_000_000, 0)}
			c, _ := newTestClient(t, f, clock)
			f.setRelease(&Release{Name: "tp-x", Status: releaseStatus, Description: "helm said no"})
			st, err := c.Poll(context.Background(), "r-1")
			if err != nil {
				t.Fatal(err)
			}
			if st.Phase != PhaseBlocked {
				t.Fatalf("phase = %s, want blocked", st.Phase)
			}
			if st.Failure == nil || st.Failure.Code != domain.CodeDeployBlocked {
				t.Fatalf("failure = %+v, want DEPLOY_BLOCKED", st.Failure)
			}
			if got := st.Failure.Detail["reason"]; got != wantReason {
				t.Errorf("detail.reason = %v, want %s", got, wantReason)
			}
			if releaseStatus == ReleaseFailed && !strings.Contains(st.Failure.Message, "helm said no") {
				t.Errorf("message must carry release.description")
			}
		})
	}
}

func TestPollTimeoutIsBlocked(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	if err := c.Start(context.Background(), ports.DeployRequest{
		RunID: "r-1", DeployID: "d-1", Manifest: manifestJSON(t, DeployManifest{PackageType: "stack"}),
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(73 * time.Hour)
	st, err := c.Poll(context.Background(), "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseBlocked {
		t.Fatalf("phase = %s, want blocked", st.Phase)
	}
	if reason := st.Failure.Detail["reason"]; reason != "timeout" {
		t.Errorf("detail.reason = %v, want timeout", reason)
	}
	if st.Reason != "timeout" {
		t.Errorf("reason = %q, want timeout", st.Reason)
	}
}

// ---- 拉起：幂等与鉴权 -----------------------------------------------------

func TestStartIsIdempotentWhenReleaseExists(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(&Release{Name: ReleaseName("r-1", "d-1"), Status: ReleasePendingInstall})

	if err := c.Start(context.Background(), ports.DeployRequest{
		RunID: "r-1", DeployID: "d-1", Manifest: manifestJSON(t, DeployManifest{PackageType: "stack"}),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := atomic.LoadInt32(&f.posts); got != 0 {
		t.Errorf("POST count = %d, want 0 (GET hit first)", got)
	}
}

func TestStartHandlesSameName400AsSuccess(t *testing.T) {
	f := &fakeGenet{postBehavior: "exists"}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	// 时序：首次 GET 404 → POST 400（同时 release 在 Genet 侧已存在）→ 复核 GET 命中 → 视为成功。
	if err := c.Start(context.Background(), ports.DeployRequest{
		RunID: "r-1", DeployID: "d-1", Manifest: manifestJSON(t, DeployManifest{PackageType: "stack"}),
	}); err != nil {
		t.Fatalf("Start must tolerate same-name 400 when GET finds the release: %v", err)
	}
	if got := atomic.LoadInt32(&f.posts); got != 1 {
		t.Errorf("POST count = %d, want 1", got)
	}
}

func TestStartSendsBearerAuth(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	if err := c.Start(context.Background(), ports.DeployRequest{
		RunID: "r-1", DeployID: "d-1", Manifest: manifestJSON(t, DeployManifest{PackageType: "stack"}),
	}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	auth := f.lastAuth
	f.mu.Unlock()
	if auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", auth)
	}
}

func TestStartRejectsEmptyManifest(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	if err := c.Start(context.Background(), ports.DeployRequest{RunID: "r-1"}); err == nil {
		t.Fatal("expected error for empty manifest")
	}
}

func TestStartUsesFrozenReleaseName(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	man := DeployManifest{PackageType: "pd", ReleaseName: "tp-frozen0001", Values: "v: 1"}
	if err := c.Start(context.Background(), ports.DeployRequest{RunID: "r-1", DeployID: "d-1", Manifest: manifestJSON(t, man)}); err != nil {
		t.Fatal(err)
	}
	if got := c.ReleaseNameFor("r-1"); got != "tp-frozen0001" {
		t.Errorf("release name = %q, want frozen tp-frozen0001", got)
	}
}

// ---- 轮询：单飞与退避 -----------------------------------------------------

func TestPollSingleFlight(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(pendingRelease("tp-x", ReleasePendingInstall, []Pod{{Name: "p", NodeName: "n"}}))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Poll(context.Background(), "r-1")
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&f.gets); got > 2 {
		t.Errorf("GET count = %d, want single-flight (<=2, 允许 1 次串行重入)", got)
	}
}

func TestPollBacksOffAfterFirstCall(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(pendingRelease("tp-x", ReleasePendingInstall, nil))

	if _, err := c.Poll(context.Background(), "r-1"); err != nil {
		t.Fatal(err)
	}
	first := atomic.LoadInt32(&f.gets)
	// 间隔内再 Poll：命中缓存，不发请求。
	if _, err := c.Poll(context.Background(), "r-1"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.gets) != first {
		t.Errorf("cache miss: got %d requests, want %d", f.gets, first)
	}
	// 推进到退避窗口之后：重新发请求。
	clock.Advance(2 * time.Second)
	if _, err := c.Poll(context.Background(), "r-1"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.gets) <= first {
		t.Errorf("expected a fresh request after the backoff window")
	}
}

// ---- 取消 -----------------------------------------------------------------

func TestCancel404IsNotSuccess(t *testing.T) {
	f := &fakeGenet{deleteBehavior: "404"}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	err := c.Cancel(context.Background(), "r-1")
	if !errors.Is(err, ErrCancelNotYetApplied) {
		t.Fatalf("err = %v, want ErrCancelNotYetApplied (404 不能当成功)", err)
	}
}

func TestCancel409IsRetryable(t *testing.T) {
	f := &fakeGenet{deleteBehavior: "409"}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	if err := c.Cancel(context.Background(), "r-1"); !errors.Is(err, ErrCancelNotYetApplied) {
		t.Fatalf("err = %v, want retryable ErrCancelNotYetApplied", err)
	}
}

func TestCancelThenRetrySucceeds(t *testing.T) {
	f := &fakeGenet{deleteBehavior: "404"}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	if err := c.Cancel(context.Background(), "r-1"); !errors.Is(err, ErrCancelNotYetApplied) {
		t.Fatalf("first cancel err = %v", err)
	}
	f.mu.Lock()
	f.deleteBehavior = ""
	f.mu.Unlock()
	if err := c.Cancel(context.Background(), "r-1"); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if got := atomic.LoadInt32(&f.deletes); got != 2 {
		t.Errorf("DELETE count = %d, want 2", got)
	}
}

// ---- 构造与配置 -----------------------------------------------------------

func TestNewRequiresBaseURLAndKey(t *testing.T) {
	if _, err := New(Options{APIKey: "k"}); err == nil {
		t.Error("expected error for missing base URL")
	}
	if _, err := New(Options{BaseURL: "https://genet.example"}); err == nil {
		t.Error("expected error for missing API key")
	}
	if _, err := New(Options{BaseURL: "not-absolute", APIKey: "k"}); err == nil {
		t.Error("expected error for relative base URL")
	}
}

func TestReleaseNameIsStableAndDNS1123(t *testing.T) {
	a := ReleaseName("r-20261105-0001", "d-1")
	b := ReleaseName("r-20261105-0001", "d-1")
	c := ReleaseName("r-20261105-0001", "d-2")
	if a != b {
		t.Errorf("release name must be deterministic")
	}
	if a == c {
		t.Errorf("different deploy_id must yield a different release name")
	}
	if !strings.HasPrefix(a, "tp-") || len(a) != 15 {
		t.Errorf("release name = %q, want tp-+12 hex", a)
	}
	for _, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			t.Errorf("release name %q contains invalid char %q", a, r)
			break
		}
	}
}

func TestInstallPathByPackageType(t *testing.T) {
	if got := installPath("pd"); got != "/api/open/helm/uc-packages/pd/releases" {
		t.Errorf("pd path = %q", got)
	}
	if got := installPath("STACK"); got != "/api/open/helm/uc-packages/stack/releases" {
		t.Errorf("stack path = %q", got)
	}
	if got := installPath(""); got != "/api/open/helm/uc-packages/stack/releases" {
		t.Errorf("default path = %q", got)
	}
}

// 交叉校验：合成的 Failure 用的是裁决里的两个码。
func TestFailureCodes(t *testing.T) {
	f := &fakeGenet{}
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	c, _ := newTestClient(t, f, clock)
	f.setRelease(&Release{Name: "tp-x", Status: ReleaseFailed})
	st, _ := c.Poll(context.Background(), "r-1")
	if st.Failure == nil || st.Failure.Code != domain.CodeDeployBlocked {
		t.Fatalf("failure code = %+v, want DEPLOY_BLOCKED", st.Failure)
	}
	if !domain.KnownCode(domain.CodeDeployBlocked) {
		t.Errorf("%s must be part of the closed enum in appendix D", domain.CodeDeployBlocked)
	}
	if !domain.Retryable(domain.CodeDeployBlocked) {
		t.Errorf("%s must be retryable (C-5.8 #3)", domain.CodeDeployBlocked)
	}
	if domain.Retryable(domain.CodeDeployFailed) {
		t.Errorf("%s must not be retryable (platform-side failure)", domain.CodeDeployFailed)
	}
	_ = fmt.Sprintf("%v", st)
}
