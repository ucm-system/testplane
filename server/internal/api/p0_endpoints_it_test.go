package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// seedRunForFiles 建一个 Run（含 1 个 Task）并在 <storage-root>/<run>/ 下放几个产物文件。
func seedRunForFiles(t *testing.T, srv *Server, st *store.Store) (runID, taskID string) {
	t.Helper()
	runID = store.NewRunID(time.Now())
	taskID = store.NewTaskID()
	ctx := context.Background()
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "files-demo", Kind: "run",
			Manifest: []byte(`{}`), Resolved: []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "t1", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			OnFailure: domain.OnFailureContinue, TimeoutSeconds: 60, Status: string(domain.TaskPending),
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	base := filepath.Join(srv.cfg.StorageRoot, runID)
	dir := filepath.Join(base, "tasks", taskID, "attempts", "1", "logs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.log"), []byte("hello logs\n"), 0o640); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "report.md"), []byte("# report\n"), 0o640); err != nil {
		t.Fatalf("write report.md: %v", err)
	}
	// 产物索引（报告页/清单用）
	if err := st.Artifacts.UpsertBatch(ctx, nil, runID, []store.Artifact{
		{Path: "tasks/" + taskID + "/attempts/1/logs/stdout.log", SizeBytes: 11, Kind: "log",
			TaskID: &taskID},
		{Path: "report.md", SizeBytes: 9, Kind: "report"},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	return runID, taskID
}

func doGet(t *testing.T, srv *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

// TestListProfiles 覆盖 GET /api/v1/profiles（严格模式下种子必须有 k8s-npu）。
func TestListProfiles(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	rec := doGet(t, srv, "/api/v1/profiles")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Name      string            `json:"name"`
			Cluster   string            `json:"cluster"`
			Namespace string            `json:"namespace"`
			Mounts    []json.RawMessage `json:"mounts"`
			Scheduler string            `json:"scheduler"`
			Timeout   int               `json:"deploy_timeout_seconds"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(body.Items) == 0 {
		t.Fatal("种子应至少有一个 execution_profile（k8s-npu）")
	}
	var found bool
	for _, p := range body.Items {
		if p.Name == "k8s-npu" {
			found = true
			if p.Cluster != "cluster-01" || p.Namespace == "" || p.Scheduler == "" {
				t.Fatalf("k8s-npu 字段异常: %+v", p)
			}
			if p.Timeout <= 0 {
				t.Fatalf("deploy_timeout_seconds 应为正数: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("缺少 k8s-npu: %+v", body.Items)
	}
	if !strings.Contains(rec.Body.String(), `"mounts"`) {
		t.Fatalf("响应应包含挂载能力 mounts: %s", rec.Body.String())
	}
}

// TestGetTesterVersion 指定版本的 Tester 详情（含 parameters/env 声明）。
func TestGetTesterVersion(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	rec := doGet(t, srv, "/api/v1/testers/endpoint-probe/versions/1.0.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if dto["name"] != "endpoint-probe" || dto["version"] != "1.0.0" {
		t.Fatalf("返回了错误版本: %+v", dto)
	}
	if _, ok := dto["parameters"]; !ok {
		t.Fatalf("缺少 parameters: %+v", dto)
	}

	// 不存在的版本 → 404（TESTER_NOT_FOUND 语义）。
	rec = doGet(t, srv, "/api/v1/testers/endpoint-probe/versions/9.9.9")
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
		t.Fatalf("未知版本 status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestRunArtifacts 覆盖 Run 级产物清单（含 by_kind 分组与总大小）。
func TestRunArtifacts(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID, _ := seedRunForFiles(t, srv, st)

	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items  []map[string]any            `json:"items"`
		ByKind map[string][]map[string]any `json:"by_kind"`
		Total  int64                       `json:"total_size_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(body.Items) != 2 || body.Total != 20 {
		t.Fatalf("items=%d total=%d", len(body.Items), body.Total)
	}
	if len(body.ByKind["log"]) != 1 || len(body.ByKind["report"]) != 1 {
		t.Fatalf("by_kind 分组异常: %+v", body.ByKind)
	}
	if body.Items[0]["sha256"] != nil {
		t.Fatalf("未计算的 sha256 应为 null: %+v", body.Items[0])
	}

	// Run 不存在 → 404。
	if rec := doGet(t, srv, "/api/v1/runs/r-19990101-ZZZZ/artifacts"); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 Run status = %d", rec.Code)
	}
}

// TestAttemptArtifacts 覆盖 Attempt 级产物清单。
func TestAttemptArtifacts(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	_, taskID := seedRunForFiles(t, srv, st)

	rec := doGet(t, srv, "/api/v1/tasks/"+taskID+"/attempts/1/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0]["path"] != "tasks/"+taskID+"/attempts/1/logs/stdout.log" {
		t.Fatalf("items 异常: %+v", body.Items)
	}

	// 非法 idx → 400。
	if rec := doGet(t, srv, "/api/v1/tasks/"+taskID+"/attempts/0/artifacts"); rec.Code != http.StatusBadRequest {
		t.Fatalf("idx=0 status = %d", rec.Code)
	}
}

// TestRunFile_Download 覆盖整文件下载、Range 206、越界 416、穿越拒绝、缺失 404。
func TestRunFile_Download(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID, taskID := seedRunForFiles(t, srv, st)
	rel := "tasks/" + taskID + "/attempts/1/logs/stdout.log"

	// 1) 整文件 200
	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+rel)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello logs\n" {
		t.Fatalf("整文件: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("缺少 Accept-Ranges: %v", rec.Header())
	}

	// 2) Range → 206 + 局部内容
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/files?path="+rel, nil)
	req.Header.Set("Range", "bytes=6-9")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent {
		t.Fatalf("Range status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "logs" {
		t.Fatalf("Range body = %q, 期望 logs", rr.Body.String())
	}
	if cr := rr.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 6-9/") {
		t.Fatalf("Content-Range = %q", cr)
	}

	// 3) 越界 Range → 416
	req = httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/files?path="+rel, nil)
	req.Header.Set("Range", "bytes=999-1000")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range status = %d, 期望 416", rr.Code)
	}

	// 4) 目录穿越一律 400（绝对路径 / ../ / 盘符 / 空段）
	bad := []string{
		"../../etc/passwd",
		"..%2F..%2Fetc%2Fpasswd",
		"/etc/passwd",
		"C:%5CWindows%5Cwin.ini",
		"tasks//x",
	}
	for _, p := range bad {
		rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+p)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("路径 %q status = %d, 期望 400", p, rec.Code)
		}
	}

	// 5) 不存在的文件 → 404 NOT_FOUND
	rec = doGet(t, srv, "/api/v1/runs/"+runID+"/files?path=tasks/nope/b.txt")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("缺失文件 status = %d", rec.Code)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != domain.CodeNotFound {
		t.Fatalf("错误码 = %q, 期望 NOT_FOUND", errBody.Code)
	}

	// 6) 缺少 path → 400
	if rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files"); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 path status = %d", rec.Code)
	}
}

// TestRunFile_TooLargeRequiresRange 超过 1 GiB 且无 Range → 413。
func TestRunFile_TooLargeRequiresRange(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID, taskID := seedRunForFiles(t, srv, st)
	rel := "tasks/" + taskID + "/attempts/1/big.bin"
	full := filepath.Join(srv.cfg.StorageRoot, runID, "tasks", taskID, "attempts", "1", "big.bin")
	if err := os.WriteFile(full, []byte("x"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 用 Truncate 造稀疏大文件，避免真的写 1 GiB。
	if err := os.Truncate(full, maxFileDownloadBytes+1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+rel)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, 期望 413, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Range") {
		t.Fatalf("413 应提示用 Range: %s", rec.Body.String())
	}
}

// TestMetrics 覆盖 /metrics：不在 /api/v1 前缀下、含约定指标、SPA 不 fallback。
func TestMetrics(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	// 先制造一次请求计数（/api/v1/profiles）
	_ = doGet(t, srv, "/api/v1/profiles")

	rec := doGet(t, srv, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"testplane_http_requests_total{method=\"GET\",code=\"200\"}",
		"testplane_runs_total{status=",
		"testplane_tasks_total{status=",
		"testplane_attempts_active ",
		"testplane_endpoint_leases ",
		"testplane_queue_waiting{reason=\"slot_waiting\"}",
		"testplane_up 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics 缺少 %q\n%s", want, body)
		}
	}

	// 不带 /api/v1 前缀：/api/v1/metrics 应是 404，不会误命中。
	if rec := doGet(t, srv, "/api/v1/metrics"); rec.Code == http.StatusOK {
		t.Fatalf("/api/v1/metrics 不应返回 200")
	}
}

// TestSubmitRun_UnknownProfileRejected 验证"表为空就放行任意 profile"的引导期例外已移除：
// 未登记的 execution.profile 一律 400 + EXECUTION_PROFILE_NOT_FOUND（种子只保证 k8s-npu）。
func TestSubmitRun_UnknownProfileRejected(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: bad-profile
  precheck: "off"
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: t1
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: does-not-exist
`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "EXECUTION_PROFILE_NOT_FOUND") {
		t.Fatalf("错误码应为 EXECUTION_PROFILE_NOT_FOUND: %s", rec.Body.String())
	}
}

// TestMetrics_NoAuthRequiredEvenInOIDCMode 锁住"运维端点免鉴权"：
// /metrics 注册在根路由、不在 middlewareAuth 组内，切 OIDC 后仍必须公开（Prometheus 不带令牌）。
func TestMetrics_NoAuthRequiredEvenInOIDCMode(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()
	// 模拟切到 OIDC 模式（当前实现仍按 dev 放行，但路由分组不能变）。
	srv.cfg.AuthMode = "oidc"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	// 故意不带 Authorization 头。
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 在 OIDC 模式下必须免鉴权，得到 %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "testplane_up 1") {
		t.Fatalf("/metrics 内容异常: %s", rec.Body.String())
	}
}

// TestRunFile_AcceptsStorageRootRelativePath 锁住"两种形态都收"：
// /runs/{id}/artifacts 返回的 path 是 storage-root 相对（带 `<run_id>/` 前缀），
// 直接喂给 /files 必须 200 且内容正确——不能要求客户端自己剥前缀。
func TestRunFile_AcceptsStorageRootRelativePath(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID, taskID := seedRunForFiles(t, srv, st)

	// 取 artifacts 接口真实返回的 path（避免测试里手写死形态）。
	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("artifacts status = %d", rec.Code)
	}
	var body struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 artifacts: %v", err)
	}
	var runRel string
	for _, it := range body.Items {
		if strings.HasSuffix(it.Path, "logs/stdout.log") {
			runRel = it.Path
		}
	}
	if runRel == "" {
		t.Fatalf("artifacts 里没有 stdout.log 条目: %+v", body.Items)
	}
	// 真实写入路径（report.writer 的 indexArtifacts）是 storage-root 相对 = 带 `<run_id>/` 前缀；
	// 测试种子的索引行是 run 相对。两种形态都必须能被 /files 直接消费。
	fromArtifacts := runID + "/" + runRel

	rec = doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+urlQueryEscape(fromArtifacts))
	if rec.Code != http.StatusOK {
		t.Fatalf("带前缀下载 status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello logs\n" {
		t.Fatalf("带前缀下载内容 = %q, 期望 hello logs", rec.Body.String())
	}

	// run 相对形态仍然可用（回归，别只支持带前缀）。
	rel := "tasks/" + taskID + "/attempts/1/logs/stdout.log"
	if rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+rel); rec.Code != http.StatusOK {
		t.Fatalf("run 相对下载 status = %d", rec.Code)
	}
	// 种子里索引的 run 相对 path 也能直接下载（两种来源混存时都不挑客户端）。
	if rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+urlQueryEscape(runRel)); rec.Code != http.StatusOK {
		t.Fatalf("索引里的 run 相对下载 status = %d", rec.Code)
	}
}

// TestRunFile_PrefixedPathStillValidated 剥前缀不得削弱穿越校验。
func TestRunFile_PrefixedPathStillValidated(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	runID, taskID := seedRunForFiles(t, srv, st)

	cases := []string{
		runID + "/../../etc/passwd",
		runID + "/../" + runID + "/tasks",
		runID + "//tasks//x",
		runID + "/C:%5CWindows%5Cwin.ini",
	}
	for _, p := range cases {
		rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+urlQueryEscape(p))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("带前缀的非法路径 %q status = %d, 期望 400", p, rec.Code)
		}
	}
	// 剥完是合法相对路径时仍能读到（控制组，确认不是一律拒绝）。
	good := runID + "/tasks/" + taskID + "/attempts/1/logs/stdout.log"
	if rec := doGet(t, srv, "/api/v1/runs/"+runID+"/files?path="+urlQueryEscape(good)); rec.Code != http.StatusOK {
		t.Fatalf("控制组 status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// urlQueryEscape 用 QueryEscape 拼 path（与真实客户端一致）。
func urlQueryEscape(s string) string { return url.QueryEscape(s) }
