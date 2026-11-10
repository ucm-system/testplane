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

	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 集成测试：真 PG（AG_TEST_DATABASE_URL），没设就如实 skip。
// 覆盖 v2 契约的主链路：健康/元信息、Runner 目录、提交（校验 + dry_run + 202）、
// Task 列表/详情、部署快照（外接模式 deploy:null）、deploy/logs（0 Pod → 200+message）、
// clean 重放 409、取消/重试前置条件、产物路径穿越 400。

// fakeCatalog 是 Runner 注册表的假实现（不落库，resolve 全内存）。
type fakeCatalog struct {
	specs map[string]domain.RunnerSpec
	list  map[string][]domain.RunnerSpec
}

func (f *fakeCatalog) Resolve(_ context.Context, name, version string) (domain.RunnerSpec, error) {
	if version == "" {
		for k, sp := range f.specs {
			if strings.HasPrefix(k, name+"@") {
				return sp, nil
			}
		}
		return domain.RunnerSpec{}, fmt.Errorf("runner not found: %s: %w", name, domain.ErrNotFound)
	}
	if sp, ok := f.specs[name+"@"+version]; ok {
		return sp, nil
	}
	return domain.RunnerSpec{}, fmt.Errorf("runner not found: %s@%s: %w", name, version, domain.ErrNotFound)
}

func (f *fakeCatalog) List(_ context.Context, role string, _ bool) ([]domain.RunnerSpec, error) {
	if role == "" {
		var out []domain.RunnerSpec
		for _, sp := range f.list[domain.RolePreCheck] {
			out = append(out, sp)
		}
		return out, nil
	}
	return f.list[role], nil
}

func itTestCatalog() *fakeCatalog {
	c := &fakeCatalog{specs: map[string]domain.RunnerSpec{}, list: map[string][]domain.RunnerSpec{}}
	add := func(sp domain.RunnerSpec) {
		c.specs[sp.Name+"@"+sp.Version] = sp
		c.list[sp.Role] = append(c.list[sp.Role], sp)
	}
	deploy := domain.RunnerSpec{Name: "service-deploy", Version: "1.0.0", Role: domain.RoleDeploy,
		OnFailure: domain.OnFailureAbortTask, Entry: []string{"/runner/run.sh"},
		InputFormat: domain.InputFormatYAML, Image: "registry/argus/service-deploy:1.0.0"}
	clean := domain.RunnerSpec{Name: "service-clean", Version: "1.0.0", Role: domain.RoleClean,
		OnFailure: domain.OnFailureAbortTask, Entry: []string{"/runner/run.sh"},
		InputFormat: domain.InputFormatText, Image: "registry/argus/service-clean:1.0.0"}
	probe := domain.RunnerSpec{Name: "endpoint-probe", Version: "1.0.0", Role: domain.RolePreCheck,
		OnFailure: domain.OnFailureAbortTask, Entry: []string{"/runner/run.sh"},
		InputFormat: domain.InputFormatJSONL, Image: "registry/argus/endpoint-probe:1.0.0"}
	acc := domain.RunnerSpec{Name: "aisbench-acc", Version: "1.0.0", Role: domain.RoleTest,
		OnFailure: domain.OnFailureContinue, Entry: []string{"/runner/run.sh"},
		InputFormat: domain.InputFormatJSONL, Image: "registry/argus/aisbench-acc:1.0.0",
		Parameters: []domain.Param{{Name: "dataset", Type: "string", Required: true}}}
	add(deploy)
	add(clean)
	add(probe)
	add(acc)
	return c
}

// integrationServer 起一个真 DB + 假目录的 Server（v2 形状）。
func integrationServer(t *testing.T) (*Server, *store.Store, func()) {
	t.Helper()
	dsn, err := dbtest.URL("api")
	if err != nil {
		t.Fatalf("准备测试库: %v", err)
	}
	if dsn == "" {
		t.Skip("未设置 AG_TEST_DATABASE_URL")
	}
	st, err := store.New(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	cfg := &config.Config{
		APIPrefix:            "/api/v1",
		AuthMode:             "dev",
		StorageRoot:          t.TempDir(),
		RunnerTimeoutDefault: 4 * time.Hour,
		RunnerTimeoutMax:     24 * time.Hour,
		TaskMaxRunners:       200,
		AttemptMax:           3,
		MaxGlobalConcurrency: 20,
		SSEReplayWindow:      24 * time.Hour,
	}
	srv := New(Deps{
		Config:   cfg,
		Store:    st,
		Machines: store.NewMachines(st, nil),
		Catalog:  itTestCatalog(),
	})
	return srv, st, func() { st.Close() }
}

// do 是便捷请求：dev 模式 Bearer + admin 角色。
func (s *Server) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer qa-admin")
	if body != "" && method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v: %s", err, rec.Body.String())
	}
	return m
}

const manifestJSON = `{"task":{"name":"it-demo","service":{"url":"http://10.0.0.10:8000/v1","model_name":"Qwen3-32B"},
 "precheck":"auto",
 "runners":[{"name":"acc","runner":"aisbench-acc","version":"1.0.0","execution":{"profile":"k8s-npu"},
   "input":"{\"dataset\":\"/data/a\"}\n"}]}}`

func TestIT_HealthAndMeta(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()
	h := srv.Handler()

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRequest(http.MethodGet, path, nil)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, rec)
		if out.Code != http.StatusOK {
			t.Fatalf("%s 应 200: %d %s", path, out.Code, out.Body.String())
		}
	}

	rec := srv.do(t, http.MethodGet, "/api/v1/meta", "")
	if rec.Code != 200 {
		t.Fatalf("meta 应 200: %d", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["api_version"] != "v1" {
		t.Fatalf("api_version 应为 v1: %v", m["api_version"])
	}

	rec = srv.do(t, http.MethodGet, "/api/v1/me", "")
	if rec.Code != 200 {
		t.Fatalf("me 应 200: %d", rec.Code)
	}
	me := decodeBody(t, rec)
	if me["user"] != "qa-admin" {
		t.Fatalf("dev Bearer 身份应为 subject: %v", me["user"])
	}
}

func TestIT_SubmitValidationsAndAccept(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	if err := st.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// deploy XOR service：两者都给 → 400 VALIDATION_ERROR。
	bad := strings.Replace(manifestJSON, `"precheck":"auto",`,
		`"precheck":"auto","deploy":{"provider":"genet","spec":"model: x"},`, 1)
	rec := srv.do(t, http.MethodPost, "/api/v1/tasks", bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("deploy XOR service 应 400: %d %s", rec.Code, rec.Body.String())
	}
	errBody := decodeBody(t, rec)
	if errBody["code"] != "VALIDATION_ERROR" {
		t.Fatalf("错误码应为 VALIDATION_ERROR: %v", errBody["code"])
	}

	// runners 0 条 → 400。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks", `{"task":{"name":"x","service":{"url":"http://a:1"},"runners":[]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("0 runners 应 400: %d", rec.Code)
	}

	// dry_run：200 + valid。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks?dry_run=true", manifestJSON)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry_run 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if v, _ := decodeBody(t, rec)["valid"].(bool); !v {
		t.Fatalf("dry_run.valid 应为 true")
	}

	// 正式提交：202 + 注入的 precheck 在 runners[]。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks", manifestJSON)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("提交应 202: %d %s", rec.Code, rec.Body.String())
	}
	acc := decodeBody(t, rec)
	taskID, _ := acc["task"].(map[string]any)["id"].(string)
	if !strings.HasPrefix(taskID, "t-") {
		t.Fatalf("task id 形态错误: %v", taskID)
	}
	runners, _ := acc["runners"].([]any)
	if len(runners) != 2 {
		t.Fatalf("外接 auto 应注入 precheck：期望 2 行，得到 %d", len(runners))
	}

	// 列表 + 详情。
	rec = srv.do(t, http.MethodGet, "/api/v1/tasks", "")
	if rec.Code != 200 {
		t.Fatalf("列表应 200: %d", rec.Code)
	}
	list := decodeBody(t, rec)
	if items, _ := list["items"].([]any); len(items) != 1 {
		t.Fatalf("列表应含 1 条: %v", list)
	}

	rec = srv.do(t, http.MethodGet, "/api/v1/tasks/"+taskID, "")
	if rec.Code != 200 {
		t.Fatalf("详情应 200: %d %s", rec.Code, rec.Body.String())
	}
	detail := decodeBody(t, rec)
	if detail["status"] != "queued" {
		t.Fatalf("落库后应已 queued: %v", detail["status"])
	}

	// dry_run 校验：jsonl 语法错误 → 400 且 issues 带行号。
	badInput := strings.Replace(manifestJSON, `{"dataset":"/data/a"}\n`, `{not json}\n`, 1)
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks?dry_run=true", badInput)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏输入应 400: %d %s", rec.Code, rec.Body.String())
	}
	detail2, _ := decodeBody(t, rec)["detail"].(map[string]any)
	if detail2 == nil {
		t.Fatalf("错误应带 detail.issues")
	}

	// 取消：202 cancelling。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks/"+taskID+"/cancel", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("取消应 202: %d %s", rec.Code, rec.Body.String())
	}
	// 重复取消：200 幂等。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks/"+taskID+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("重复取消应 200: %d", rec.Code)
	}

	// 重试：cancelling 非终态 → 409 CONFLICT。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks/"+taskID+"/retry", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("非终态重试应 409: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIT_DeploySnapshotExternalMode(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	if err := st.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := srv.do(t, http.MethodPost, "/api/v1/tasks?dry_run=false", manifestJSON)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("提交应 202: %d %s", rec.Code, rec.Body.String())
	}
	taskID, _ := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)

	// 外接模式：deploy null + pods []。
	rec = srv.do(t, http.MethodGet, "/api/v1/tasks/"+taskID+"/deploy", "")
	if rec.Code != 200 {
		t.Fatalf("deploy 快照应 200: %d", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["deploy"] != nil {
		t.Fatalf("外接模式 deploy 应为 null: %v", m["deploy"])
	}

	// 外接模式 deploy/logs：200 + message（不是 404）。
	rec = srv.do(t, http.MethodGet, "/api/v1/tasks/"+taskID+"/deploy/logs", "")
	if rec.Code != 200 {
		t.Fatalf("外接模式 deploy/logs 应 200: %d %s", rec.Code, rec.Body.String())
	}
	if msg, _ := decodeBody(t, rec)["message"].(string); msg == "" {
		t.Fatalf("应带 message 说明")
	}
}

func TestIT_CleanReplayConflicts(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	if err := st.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := srv.do(t, http.MethodPost, "/api/v1/tasks", manifestJSON)
	taskID, _ := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)

	// Task 非终态 → 409 task_running。
	rec = srv.do(t, http.MethodPost, "/api/v1/tasks/"+taskID+"/clean", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("非终态 clean 重放应 409: %d %s", rec.Code, rec.Body.String())
	}
	detail, _ := decodeBody(t, rec)["detail"].(map[string]any)
	if reason, _ := detail["reason"].(string); reason != "task_running" {
		t.Fatalf("detail.reason 应为 task_running: %v", detail)
	}
}

func TestIT_RunnerCatalogEndpoints(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	// 注册表是启动期扫描落库的；这里直接 Upsert 两条做目录读（K-3 无运行时注册端点）。
	row := &store.Runner{
		Name: "aisbench-acc", Version: "1.0.0", Role: "test", OnFailure: "continue",
		Entry: []byte(`["/runner/run.sh"]`), InputFormat: "jsonl",
		Image:       "registry/argus/aisbench-acc:1.0.0",
		Resources:   []byte(`{"cpu":"1","memory":"256Mi"}`),
		Concurrency: []byte(`{}`), Mounts: []byte(`[]`), K8s: []byte(`{}`),
		Parameters: []byte(`[]`), Exports: []byte(`[]`), Consumes: []byte(`[]`),
		EnvDecl: []byte(`{}`), SourceFile: "runners/aisbench-acc/runner.yaml",
		ContentSHA: "deadbeef", RegisteredBy: "it",
	}
	if err := st.Runners.Upsert(context.Background(), nil, row); err != nil {
		t.Fatalf("upsert runner: %v", err)
	}

	rec := srv.do(t, http.MethodGet, "/api/v1/runners?role=test", "")
	if rec.Code != 200 {
		t.Fatalf("目录应 200: %d", rec.Code)
	}
	list := decodeBody(t, rec)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("test 角色应有 1 条: %v", list)
	}
	first, _ := items[0].(map[string]any)
	if first["latest"] != true {
		t.Fatalf("默认视图条目应 latest=true")
	}

	rec = srv.do(t, http.MethodGet, "/api/v1/runners/aisbench-acc", "")
	if rec.Code != 200 {
		t.Fatalf("详情应 200: %d", rec.Code)
	}
	if v, _ := decodeBody(t, rec)["version"].(string); v != "1.0.0" {
		t.Fatalf("版本不符: %v", decodeBody(t, rec)["version"])
	}

	rec = srv.do(t, http.MethodGet, "/api/v1/runners/aisbench-acc/versions/1.0.0", "")
	if rec.Code != 200 {
		t.Fatalf("指定版本应 200: %d", rec.Code)
	}
	rec = srv.do(t, http.MethodGet, "/api/v1/runners/aisbench-acc/versions/9.9.9", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的版本应 404: %d", rec.Code)
	}
}

func TestIT_FilesPathTraversalRejected(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	if err := st.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := srv.do(t, http.MethodPost, "/api/v1/tasks", manifestJSON)
	taskID, _ := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)

	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "C:/boot.ini", "a//b"} {
		rec = srv.do(t, http.MethodGet, "/api/v1/tasks/"+taskID+"/files?path="+bad, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path %q 应 400: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
}
