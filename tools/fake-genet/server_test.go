package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock 是可推进的假时钟。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestServer(t *testing.T, readyAfter time.Duration) (*httptest.Server, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	store := NewSimStore(clock.Now, readyAfter)
	srv := NewServer(store, "testplane-mock", "tp-local-2026", "mock-llm", clock.Now)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, clock
}

func doJSON(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tp-local-2026")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// 一条完整生命周期：安装 → (pending) → ready → 列表 → 卸载 → 404。
func TestInstallReadyUninstall(t *testing.T) {
	hs, clock := newTestServer(t, 3*time.Second)
	name := "tp-abc123def456"

	// 1) 安装 → 202 pending-install。
	var installResp map[string]any
	if code := doJSON(t, http.MethodPost, hs.URL+"/api/open/helm/uc-packages/stack/releases",
		map[string]any{"releaseName": name, "values": "a: b", "noWait": true}, &installResp); code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202", code)
	}
	if installResp["status"] != StatusPendingInstall {
		t.Errorf("install resp = %+v", installResp)
	}

	// 2) 立刻查：pending + endpoint pending。
	var rel map[string]any
	if code := doJSON(t, http.MethodGet, hs.URL+"/api/open/helm/releases/"+name, nil, &rel); code != http.StatusOK {
		t.Fatalf("get status = %d", code)
	}
	if rel["status"] != StatusPendingInstall {
		t.Errorf("status = %v, want pending-install", rel["status"])
	}
	ci, _ := rel["callInfo"].(map[string]any)
	ep, _ := ci["endpoint"].(map[string]any)
	if ep["status"] != EndpointPending {
		t.Errorf("endpoint = %+v, want pending", ep)
	}

	// 3) 推进时钟 → deployed + endpoint ready + url 按 `<release>-mock-llm.<ns>.svc:18080/v1`。
	clock.Advance(4 * time.Second)
	if code := doJSON(t, http.MethodGet, hs.URL+"/api/open/helm/releases/"+name, nil, &rel); code != http.StatusOK {
		t.Fatalf("get status = %d", code)
	}
	if rel["status"] != StatusDeployed {
		t.Fatalf("status = %v, want deployed", rel["status"])
	}
	ci, _ = rel["callInfo"].(map[string]any)
	ep, _ = ci["endpoint"].(map[string]any)
	if ep["status"] != EndpointReady {
		t.Fatalf("endpoint = %+v, want ready", ep)
	}
	wantURL := "http://" + name + "-mock-llm.testplane-mock.svc:18080/v1"
	if ep["url"] != wantURL {
		t.Errorf("url = %v, want %s", ep["url"], wantURL)
	}
	if ci["model"] != "mock-llm" {
		t.Errorf("model = %v", ci["model"])
	}
	if pods, ok := rel["pods"].([]any); !ok || len(pods) == 0 {
		t.Errorf("pods missing: %+v", rel["pods"])
	}

	// 4) 列表（light=true 不带 callInfo）。
	var list map[string]any
	if code := doJSON(t, http.MethodGet, hs.URL+"/api/open/helm/releases?light=true", nil, &list); code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	items, _ := list["releases"].([]any)
	if len(items) != 1 {
		t.Fatalf("list = %+v", list)
	}

	// 5) 卸载 → 200，再查 404。
	var del map[string]any
	if code := doJSON(t, http.MethodDelete, hs.URL+"/api/open/helm/releases/"+name+"?force=true&keepHistory=false", nil, &del); code != http.StatusOK {
		t.Fatalf("delete status = %d", code)
	}
	if code := doJSON(t, http.MethodGet, hs.URL+"/api/open/helm/releases/"+name, nil, nil); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

func TestSameNameInstallReturns400(t *testing.T) {
	hs, _ := newTestServer(t, time.Second)
	name := "tp-dup"
	body := map[string]any{"releaseName": name, "noWait": true}
	if code := doJSON(t, http.MethodPost, hs.URL+"/api/open/helm/uc-packages/stack/releases", body, nil); code != http.StatusAccepted {
		t.Fatalf("first install = %d", code)
	}
	var errResp map[string]any
	if code := doJSON(t, http.MethodPost, hs.URL+"/api/open/helm/uc-packages/stack/releases", body, &errResp); code != http.StatusBadRequest {
		t.Fatalf("second install = %d, want 400", code)
	}
	if msg, _ := errResp["message"].(string); !strings.Contains(msg, "already has protected metadata") {
		t.Errorf("message = %q, want the real Genet wording", msg)
	}
}

func TestReleaseNameValidation(t *testing.T) {
	hs, _ := newTestServer(t, time.Second)
	cases := []string{"", "TP-Upper", "1starts-with-digit", strings.Repeat("a", 60)}
	for _, name := range cases {
		code := doJSON(t, http.MethodPost, hs.URL+"/api/open/helm/uc-packages/stack/releases",
			map[string]any{"releaseName": name}, nil)
		if code != http.StatusBadRequest {
			t.Errorf("releaseName %q => %d, want 400", name, code)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	hs, _ := newTestServer(t, time.Second)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/open/helm/releases/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	// healthz 不需要鉴权。
	hresp, err := http.Get(hs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", hresp.StatusCode)
	}
}

func TestUnknownReleaseIs404(t *testing.T) {
	hs, _ := newTestServer(t, time.Second)
	if code := doJSON(t, http.MethodGet, hs.URL+"/api/open/helm/releases/tp-missing", nil, nil); code != http.StatusNotFound {
		t.Errorf("get = %d, want 404", code)
	}
	if code := doJSON(t, http.MethodDelete, hs.URL+"/api/open/helm/releases/tp-missing", nil, nil); code != http.StatusNotFound {
		t.Errorf("delete = %d, want 404", code)
	}
}

// 真 helm 的端到端：默认跳过（CI 只用内存逻辑）。
// 本地验证：
//
//	FAKE_GENET_LIVE=1 go test -run TestLiveWithHelm ./tools/fake-genet
func TestLiveWithHelm(t *testing.T) {
	if testing.Short() || os.Getenv("FAKE_GENET_LIVE") == "" {
		t.Skip("需要真集群（FAKE_GENET_LIVE=1 且 helm/kubectl/kubeconfig 就绪）")
	}
	helmBin := os.Getenv("HELM_BIN")
	if helmBin == "" {
		helmBin = "helm"
	}
	if _, err := exec.LookPath(helmBin); err != nil {
		t.Skipf("helm 不可用: %v", err)
	}
	chartDir := os.Getenv("MOCK_LLM_CHART")
	if chartDir == "" {
		chartDir = "../../deploy/charts/mock-llm"
	}
	store := &HelmStore{
		Helm: helmBin, Kubectl: "kubectl", Kubeconfig: os.Getenv("KUBECONFIG"),
		Namespace: "testplane-mock", ChartDir: chartDir,
	}
	srv := NewServer(store, "testplane-mock", "tp-local-2026", "mock-llm", time.Now)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	name := "tp-live-test"
	_ = doJSON(t, http.MethodDelete, hs.URL+"/api/open/helm/releases/"+name+"?force=true", nil, nil)

	if code := doJSON(t, http.MethodPost, hs.URL+"/api/open/helm/uc-packages/stack/releases",
		map[string]any{"releaseName": name, "noWait": true}, nil); code != http.StatusAccepted {
		t.Fatalf("install = %d", code)
	}
	deadline := time.Now().Add(3 * time.Minute)
	var ready bool
	for time.Now().Before(deadline) {
		var rel map[string]any
		if code := doJSON(t, http.MethodGet, hs.URL+"/api/open/helm/releases/"+name, nil, &rel); code == http.StatusOK {
			ci, _ := rel["callInfo"].(map[string]any)
			ep, _ := ci["endpoint"].(map[string]any)
			if ep["status"] == EndpointReady {
				ready = true
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	if !ready {
		t.Fatal("release 未在 3 分钟内 ready")
	}
	if code := doJSON(t, http.MethodDelete, hs.URL+"/api/open/helm/releases/"+name+"?force=true", nil, nil); code != http.StatusOK {
		t.Fatalf("uninstall = %d", code)
	}
}

// ---- 失败分支：blocked / 500（验收矩阵用）--------------------------------

// -sim-blocked：endpoint 永远 blocked，status 仍是 deployed → 客户端应合成 blocked。
func TestSimBlockedEndpoint(t *testing.T) {
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	store := NewSimStore(clock.Now, time.Millisecond)
	srv := NewServer(store, "testplane-mock", "k", "mock-llm", clock.Now)
	srv.SimBlocked = true
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	req := func(method, path string, body any) *http.Response {
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		r, _ := http.NewRequest(method, hs.URL+path, reader)
		r.Header.Set("Authorization", "Bearer k")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := req(http.MethodPost, "/api/open/helm/uc-packages/stack/releases", map[string]any{"releaseName": "tp-blocked1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install = %d, want 202", resp.StatusCode)
	}
	clock.Advance(2 * time.Second)

	resp = req(http.MethodGet, "/api/open/helm/releases/tp-blocked1", nil)
	defer resp.Body.Close()
	var rel map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rel)
	if rel["status"] != StatusDeployed {
		t.Errorf("status = %v, want deployed", rel["status"])
	}
	ci, _ := rel["callInfo"].(map[string]any)
	ep, _ := ci["endpoint"].(map[string]any)
	if ep["status"] != EndpointBlocked {
		t.Fatalf("endpoint = %+v, want blocked", ep)
	}
	if msg, _ := ep["message"].(string); !strings.Contains(msg, "insufficient quota") {
		t.Errorf("message = %q", msg)
	}
}

// -sim-error 500：所有 /api/open/helm/* 都 500（模拟 Genet 故障）。
func TestSimError500(t *testing.T) {
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	store := NewSimStore(clock.Now, time.Millisecond)
	srv := NewServer(store, "testplane-mock", "k", "mock-llm", clock.Now)
	srv.SimError = http.StatusInternalServerError
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/open/helm/uc-packages/stack/releases"},
		{http.MethodGet, "/api/open/helm/releases/tp-x"},
		{http.MethodDelete, "/api/open/helm/releases/tp-x?force=true"},
		{http.MethodGet, "/api/open/helm/releases"},
	} {
		var reader *bytes.Reader
		if tc.method == http.MethodPost {
			raw, _ := json.Marshal(map[string]any{"releaseName": "tp-x"})
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		r, _ := http.NewRequest(tc.method, hs.URL+tc.path, reader)
		r.Header.Set("Authorization", "Bearer k")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s %s = %d, want 500", tc.method, tc.path, resp.StatusCode)
		}
	}
	// healthz 不受影响（QA 用它探活）。
	resp, err := http.Get(hs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}
