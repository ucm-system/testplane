package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ucm-system/testplane/server/internal/store"
)

// seedDeployDisk 在 storage root 下造 D-59 的落盘副本（日志 + pods.json）。
func seedDeployDisk(t *testing.T, root, runID string) {
	t.Helper()
	logDir := filepath.Join(root, runID, "deploy", "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "tp-rel-stack-1.log"),
		[]byte("disk-line-1\ndisk-line-2\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"pods": []any{map[string]any{
			"name": "tp-rel-stack-1", "phase": "Running", "node": "node-1", "ready": true,
			"containers": []any{map[string]any{"name": "server", "restarts": 0}},
			"logs":       []any{map[string]any{"file": "deploy/logs/tp-rel-stack-1.log", "truncated": false}},
		}},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(root, runID, "deploy", "pods.json"), raw, 0o640); err != nil {
		t.Fatal(err)
	}
}

// ④ 盘上有日志 → **不调 kube client**（fake 记调用次数=0）；盘上没有 → 降级 + message 说明实时窗口。
func TestDeployLogs_DiskFirstThenLiveFallback(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	root := t.TempDir()
	srv.cfg.StorageRoot = root

	// --- 盘上有副本：kube 调用必须为 0 ---
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-rel", "state": "ready",
	})
	seedDeployDisk(t, root, runID)
	fake := &fakeDeployLogs{}
	srv.deployLogsKube = fake

	rec, body := getDeployLogs(t, srv, runID, "?pod=tp-rel-stack-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.seenNamespaces) != 0 {
		t.Fatalf("盘上有日志却调了 kube client: %v", fake.seenNamespaces)
	}
	if len(body.Entries) != 1 || !strings.Contains(body.Entries[0].Text, "disk-line-1") {
		t.Fatalf("entries = %+v，应来自落盘副本", body.Entries)
	}
	if !strings.Contains(body.Message, "落盘副本") {
		t.Errorf("message 应说明来自落盘副本: %q", body.Message)
	}

	// --- 盘上没有：降级实时读 + message 标注"实时窗口" ---
	runID2 := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-live", "state": "ready",
	})
	fake2 := &fakeDeployLogs{
		pods: []corev1.Pod{deployPod("tp-live-stack-1", true, 0, time.Now().UTC())},
		logs: map[string]string{"tp-live-stack-1": "live-line\n"},
	}
	srv.deployLogsKube = fake2
	rec, body = getDeployLogs(t, srv, runID2, "?pod=tp-live-stack-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback status = %d", rec.Code)
	}
	if len(fake2.seenNamespaces) == 0 {
		t.Fatal("盘上没有日志时应该降级实时读")
	}
	if !strings.Contains(body.Message, "实时窗口") {
		t.Fatalf("降级时必须在 message 里标注实时窗口: %q", body.Message)
	}
	if len(body.Entries) != 1 || !strings.Contains(body.Entries[0].Text, "live-line") {
		t.Fatalf("降级内容 = %+v", body.Entries)
	}
}

// ⑤ `/deploy` 的 pods[] 来自 pods.json（非空）；缺失时给 pods_message（不静默空）。
func TestRunDeploy_PodsFromPodsJSON(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	root := t.TempDir()
	srv.cfg.StorageRoot = root

	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-rel", "state": "ready",
	})
	seedDeployDisk(t, root, runID)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/deploy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Pods []struct {
			Name  string `json:"name"`
			Phase string `json:"phase"`
		} `json:"pods"`
		Message string `json:"pods_message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Pods) != 1 || body.Pods[0].Name != "tp-rel-stack-1" {
		t.Fatalf("pods = %+v，应来自 pods.json", body.Pods)
	}

	// 没有 pods.json → 空数组 + 明确原因
	runID2 := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-nopods", "state": "queued",
	})
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID2+"/deploy", nil))
	var body2 struct {
		Pods    []any  `json:"pods"`
		Message string `json:"pods_message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body2)
	if len(body2.Pods) != 0 {
		t.Errorf("pods 应为空: %+v", body2.Pods)
	}
	if !strings.Contains(body2.Message, "pods.json") {
		t.Errorf("缺 pods.json 时必须说明原因，got %q", body2.Message)
	}
}

// ⑥ `/artifacts` 含 deploy/logs/<pod>.log（可下载）；bundle 的 logs/deploy/* 来自盘。
func TestRunArtifactsIncludeDeployLogs(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	root := t.TempDir()
	srv.cfg.StorageRoot = root
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-rel", "state": "ready",
	})
	seedDeployDisk(t, root, runID)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/artifacts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Items []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var foundLog, foundPods bool
	for _, it := range body.Items {
		if it.Path == "deploy/logs/tp-rel-stack-1.log" && it.Kind == "deploy_logs" {
			foundLog = true
		}
		if it.Path == "deploy/pods.json" {
			foundPods = true
		}
	}
	if !foundLog {
		t.Fatalf("artifacts 缺 deploy 日志: %+v", body.Items)
	}
	if !foundPods {
		t.Fatalf("artifacts 缺 pods.json: %+v", body.Items)
	}
}

// #45-2：Pod 归属判定统一（真 Genet 的 Pod 名既不是 tp- 开头也不含 run id）。
func TestPodBelongsToRun(t *testing.T) {
	run := &store.Run{ID: "r-20261106-AAAA"}
	cases := []struct {
		name    string
		pod     string
		release string
		want    bool
	}{
		{"genet release 前缀匹配", "tp-abcd1234ef56-unified-cache-stack-0", "tp-abcd1234ef56", true},
		{"别的 release", "tp-other1234567-stack-0", "tp-abcd1234ef56", false},
		{"release 未知但像平台 Job", "tp-t-abc-a1-xyz", "", true},
		{"release 未知但含 run id", "local-r-20261106-AAAA-pod", "", true},
		{"release 未知且无关", "some-random-pod", "", false},
	}
	for _, c := range cases {
		pod := corev1.Pod{}
		pod.Name = c.pod
		if got := podBelongsToRun(pod, run, c.release); got != c.want {
			t.Errorf("%s: podBelongsToRun(%q, release=%q) = %v, want %v", c.name, c.pod, c.release, got, c.want)
		}
	}
}

// bundle 的 logs/deploy/* 走盘（D-59）：不再实时抓，盘上有就直接打包。
func TestBundleDeployLogsFromDisk(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	root := t.TempDir()
	srv.cfg.StorageRoot = root
	runID := seedDeployRun(t, st, map[string]any{
		"namespace": "testplane-mock", "release_name": "tp-rel", "state": "ready",
	})
	seedDeployDisk(t, root, runID)

	run, err := st.Runs.Get(context.Background(), nil, runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	var files []bundleFile
	missing := map[string]string{}
	srv.appendDeployLogs(context.Background(), run, &files, func(path, reason string) { missing[path] = reason })

	found := false
	for _, f := range files {
		if f.name == "logs/deploy/tp-rel-stack-1.log" && strings.Contains(string(f.bytes), "disk-line-1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("bundle 没从盘上取 deploy 日志: files=%d missing=%v", len(files), missing)
	}
}
