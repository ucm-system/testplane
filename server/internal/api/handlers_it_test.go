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

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 集成测试：真 PG（TESTPLANE_TEST_DATABASE_URL），没设就 skip。
// 覆盖 handler 层的响应形状与 QA 报过的回归（详情空 body、空游标漏 precheck）。

type fakeResolver struct {
	specs map[string]domain.TesterSpec
}

func (f *fakeResolver) Resolve(ctx context.Context, name, version string) (domain.TesterSpec, error) {
	if version == "" {
		for k, sp := range f.specs {
			if strings.HasPrefix(k, name+"@") {
				return sp, nil
			}
		}
		return domain.TesterSpec{}, storeTestNotFound(name)
	}
	if sp, ok := f.specs[name+"@"+version]; ok {
		return sp, nil
	}
	return domain.TesterSpec{}, storeTestNotFound(name)
}

func (f *fakeResolver) List(ctx context.Context, role string, includeOrphaned bool) ([]domain.TesterSpec, error) {
	out := make([]domain.TesterSpec, 0, len(f.specs))
	for _, sp := range f.specs {
		if role != "" && sp.Role != role {
			continue
		}
		out = append(out, sp)
	}
	return out, nil
}

func storeTestNotFound(name string) error {
	// 包一层 domain.ErrNotFound，让 failErr 映射成 404 NOT_FOUND（与真实注册表一致）。
	return fmt.Errorf("tester not found: %s: %w", name, domain.ErrNotFound)
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func integrationServer(t *testing.T) (*Server, *store.Store, func()) {
	t.Helper()
	dsn, err := dbtest.URL("api")
	if err != nil {
		t.Fatalf("准备测试库: %v", err)
	}
	if dsn == "" {
		t.Skip("未设置 TESTPLANE_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	if err := st.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := &config.Config{
		APIPrefix: "/api/v1", StorageRoot: t.TempDir(),
		RunMaxTasks: 200, TaskTimeoutDefault: time.Hour, TaskTimeoutMax: 24 * time.Hour,
		MaxGlobalConcurrency: 4, DeployTimeout: 72 * time.Hour,
	}
	resolver := &fakeResolver{specs: map[string]domain.TesterSpec{
		"endpoint-probe@1.0.0": {
			Name: "endpoint-probe", Version: "1.0.0", Role: domain.RolePreCheck,
			OnFailure: domain.OnFailureAbortRun, Entry: "/tester/run.sh", Image: "probe:1",
			Concurrency: domain.ConcurrencyPolicy{EndpointExclusive: true},
			Parameters: map[string]domain.Param{
				"url": {Type: "string", Required: true, Description: "服务地址"},
			},
			Env: map[string]domain.EnvVar{
				"ENDPOINT_API_KEY": {Secret: true, Description: "Bearer token"},
			},
		},
		"probe-again@1.0.0": {
			Name: "probe-again", Version: "1.0.0", Role: domain.RoleTest,
			OnFailure: domain.OnFailureContinue, Entry: "/tester/run.sh", Image: "probe:1",
		},
	}}
	srv := New(Deps{Config: cfg, Store: st, Machines: store.NewMachines(st, nil), Resolver: resolver})
	return srv, st, func() { st.Close() }
}

// TestGetRunDetail_NonEmptyBody 是 QA 报的 P0 回归：详情 200 但 body 为 0 字节。
func TestGetRunDetail_NonEmptyBody(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	run := &store.Run{
		ID: store.NewRunID(time.Now()), Name: "detail-demo", Kind: "run",
		Manifest: []byte(`{"run":{"name":"detail-demo"}}`), Resolved: []byte(`{"kind":"run","tasks":[]}`),
		CleanupPolicy: "always", SubmittedBy: "dev",
	}
	tasks := []*store.Task{{
		ID: store.NewTaskID(), Seq: 0, Name: "precheck",
		TesterName: "endpoint-probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
		Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureAbortRun,
		TimeoutSeconds: 600, Status: string(domain.TaskPending),
	}}
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{Run: run, Tasks: tasks, Actor: "user:dev"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// 造一个带 error 的终态 Run + Task（QA 复现条件：error 非空时序列化失败）。
	if err := st.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE runs SET status='completed_with_errors',
			       error = '{"code":"EXECUTOR_UNAVAILABLE","message":"x","retryable":true}'::jsonb
			 WHERE id=$1`, run.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE tasks SET status='failed',
			       error = '{"code":"EXECUTOR_UNAVAILABLE","message":"x","retryable":true}'::jsonb
			 WHERE run_id=$1`, run.ID)
		return err
	}); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("详情 body 为空（QA 报的回归）")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body 不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if body["id"] != run.ID {
		t.Fatalf("body.id = %v, 期望 %s", body["id"], run.ID)
	}
	if _, ok := body["task_counts"]; !ok {
		t.Fatal("详情缺少 task_counts")
	}
	tasks2, _ := body["tasks"].([]any)
	if len(tasks2) != 1 {
		t.Fatalf("tasks 长度 = %d, 期望 1", len(tasks2))
	}
	first, _ := tasks2[0].(map[string]any)
	if first["id"] != tasks[0].ID {
		t.Fatalf("首个 task 不是 precheck: %v", first["id"])
	}

	// 列表同样不能为空且要带 task_counts。
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs?kind=run", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("列表异常: status=%d len=%d", rec.Code, rec.Body.Len())
	}
	var list map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
}

// TestSubmitRun_NestedManifest 验证 07 §4.1 的嵌套写法（precheck 注入 + 202）。
func TestSubmitRun_NestedManifest(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: nested-api
  precheck: auto
  service:
    url: http://127.0.0.1:8000/v1
    model_name: demo
  tests:
    - name: probe-again
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: k8s-npu
      args: "--timeout=5"
`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Run struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"run"`
		Tasks []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 202 失败: %v", err)
	}
	if resp.Run.ID == "" || resp.Run.Status != string(domain.RunQueued) {
		t.Fatalf("202 响应异常: %+v", resp.Run)
	}
	if len(resp.Tasks) != 2 || resp.Tasks[0].Name != "precheck" {
		t.Fatalf("precheck 未注入: %+v", resp.Tasks)
	}
	// 冻结的解析快照必须记下"注入了 Pre-Check"：JSON 路径 resolved.precheck.injected == true。
	// 终态判定（store.PrecheckTaskIDs）依赖这个字段，不是 Tester 角色。
	assertResolvedPrecheckInjected(t, srv, resp.Run.ID, true)
}

// TestSubmitRun_PrecheckOffResolvedFlag precheck:"off" 时必须落 resolved.precheck.injected=false，
// 即使测试用的是 role=pre_check 的 Tester（QA r-20261105-TA6M 的场景）。
func TestSubmitRun_PrecheckOffResolvedFlag(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: precheck-off
  precheck: "off"
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: probe-as-test
      tester: endpoint-probe
      version: "1.0.0"
      execution:
        profile: k8s-npu
`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 202: %v", err)
	}
	assertResolvedPrecheckInjected(t, srv, resp.Run.ID, false)
}

// assertResolvedPrecheckInjected 读 runs.resolved 并断言 precheck.injected 的值。
func assertResolvedPrecheckInjected(t *testing.T, srv *Server, runID string, want bool) {
	t.Helper()
	run, err := srv.st.Runs.Get(context.Background(), nil, runID)
	if err != nil {
		t.Fatalf("读 run %s: %v", runID, err)
	}
	var resolved struct {
		Precheck struct {
			Injected *bool `json:"injected"`
		} `json:"precheck"`
	}
	if err := json.Unmarshal(run.Resolved, &resolved); err != nil {
		t.Fatalf("解析 resolved: %v (%s)", err, string(run.Resolved))
	}
	if resolved.Precheck.Injected == nil {
		t.Fatalf("resolved.precheck.injected 缺失: %s", string(run.Resolved))
	}
	if *resolved.Precheck.Injected != want {
		t.Fatalf("resolved.precheck.injected = %v, 期望 %v (%s)",
			*resolved.Precheck.Injected, want, string(run.Resolved))
	}
}

// TestSubmitRun_FlatManifestCompat 旧平级写法仍被接受（兼容旧脚本）。
func TestSubmitRun_FlatManifestCompat(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: flat-api
service:
  url: http://127.0.0.1:8000/v1
tests:
  - name: probe-again
    tester: probe-again
    version: "1.0.0"
    execution:
      profile: k8s-npu
`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("平级写法应被兼容: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSubmitRun_EmptyTestsWithPrecheckOff 校验失败路径带 run. 前缀。
func TestSubmitRun_EmptyTestsWithPrecheckOff(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: empty-tests
  precheck: "off"
  service:
    url: http://127.0.0.1:8000/v1
  tests: []
`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body2 struct {
		Code   string `json:"code"`
		Detail struct {
			Issues []struct {
				Path string `json:"path"`
				Code string `json:"code"`
			} `json:"issues"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body2); err != nil {
		t.Fatalf("解析错误体: %v", err)
	}
	if body2.Code != domain.CodeValidationError || len(body2.Detail.Issues) == 0 {
		t.Fatalf("错误体异常: %+v", body2)
	}
	if body2.Detail.Issues[0].Path != "run.tests" {
		t.Fatalf("issue path = %q, 期望 run.tests", body2.Detail.Issues[0].Path)
	}
}

// TestGetTester_IncludesRegistryMetadata 覆盖 openapi Tester 的注册表元数据。
func TestGetTester_IncludesRegistryMetadata(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()

	row := &store.Tester{
		Name: "meta-probe", Version: "1.0.0", DisplayName: "Meta", Description: "d",
		Role: domain.RoleTest, OnFailure: domain.OnFailureContinue, Entry: "/t", Image: "img:1",
		Resources: []byte(`{}`), Concurrency: []byte(`{}`), Mounts: []byte("[]"),
		Parameters: []byte("{}"), EnvDecl: []byte("{}"),
		SourceFile: "testers/meta-probe/tester.yaml", ContentSHA: "abc123",
		RegisteredBy: "system:registry",
	}
	if err := st.Testers.Upsert(context.Background(), nil, row); err != nil {
		t.Fatalf("upsert tester: %v", err)
	}
	if err := st.Testers.MarkOrphaned(context.Background(), nil, row.Name, row.Version); err != nil {
		t.Fatalf("mark orphaned: %v", err)
	}

	resolver := &fakeResolver{specs: map[string]domain.TesterSpec{
		"meta-probe@1.0.0": {Name: row.Name, Version: row.Version, Role: domain.RoleTest,
			OnFailure: domain.OnFailureContinue, Entry: "/t", Image: "img:1"},
	}}
	srv.resolver = resolver
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/testers/meta-probe", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	for _, k := range []string{"content_sha", "source_file", "orphaned", "registered_at", "registered_by"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("缺少元数据字段 %s: %v", k, got)
		}
	}
	if got["orphaned"] != true {
		t.Fatalf("orphaned = %v, 期望 true", got["orphaned"])
	}
}

// TestStreamRunEvents_ReplaysFromLastEventID 断言 SSE 回放与帧格式。
func TestStreamRunEvents_ReplaysFromLastEventID(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	run := &store.Run{
		ID: store.NewRunID(time.Now()), Name: "sse-demo", Kind: "run",
		Manifest: []byte(`{}`), Resolved: []byte(`{}`), CleanupPolicy: "always",
		SubmittedBy: "dev",
	}
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{Run: run, Actor: "user:dev"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// SSE 在 Run 终态且事件发完后收尾，测试里直接落成 succeeded。
	if err := st.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runs SET status='succeeded' WHERE id=$1`, run.ID)
		return err
	}); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/events", nil)
	req.Header.Set("Last-Event-ID", "0")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "id: 1") || !strings.Contains(body, "event: run.submitted") {
		t.Fatalf("SSE 帧异常: %q", body)
	}
}
