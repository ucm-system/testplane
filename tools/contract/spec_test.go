package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const realSpecPath = "../../server/api/openapi.yaml"

func loadRealSpec(t *testing.T) *Spec {
	t.Helper()
	if _, err := os.Stat(realSpecPath); err != nil {
		t.Skipf("找不到 %s（在 tools/contract 下运行测试）：%v", realSpecPath, err)
	}
	spec, err := LoadSpec(realSpecPath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	return spec
}

func TestRealSpecStaticChecksAllPass(t *testing.T) {
	spec := loadRealSpec(t)
	if got := len(spec.Operations); got != 25 {
		t.Fatalf("operation 数期望 25，实际 %d", got)
	}
	if len(spec.ErrorRequired) == 0 {
		t.Fatal("Error.required 为空")
	}
	checks, warnings := spec.StaticChecks()
	for _, c := range checks {
		if !c.OK {
			t.Errorf("规格侧检查失败：%s —— %s", c.Name, c.Detail)
		}
	}
	// 扁平/嵌套的提示必须存在（验收口径易混）
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "扁平") {
			found = true
		}
	}
	if !found {
		t.Errorf("缺少 Error 形状提示；warnings=%v", warnings)
	}
}

func TestSpecMissingErrorSchema(t *testing.T) {
	yaml := `openapi: 3.1.0
paths:
  /healthz:
    get:
      operationId: getHealthz
      responses:
        '200':
          description: ok
`
	path := writeTempSpec(t, yaml)
	if _, err := LoadSpec(path); err == nil {
		t.Fatal("缺少 components.schemas.Error 时应报错")
	}
}

func TestStaticChecksCatchOperationDrift(t *testing.T) {
	yaml := `openapi: 3.1.0
paths:
  /healthz:
    get:
      operationId: getHealthz
      responses:
        '200':
          description: ok
          content:
            text/plain: {}
  /api/v1/extra:
    get:
      operationId: getExtra
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
components:
  schemas:
    Error:
      type: object
      required: [code, message, retryable, trace_id]
      properties:
        code: { type: string }
        message: { type: string }
        retryable: { type: boolean }
        trace_id: { type: string }
`
	spec, err := LoadSpec(writeTempSpec(t, yaml))
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	checks, _ := spec.StaticChecks()
	// 端点集合检查现在来自设计文档（DesignChecks），不再有硬编码的 mvp 集合。
	designChecks, _ := spec.DesignChecks(loadRealDesign(t), loadRealAllow(t))
	checks = append(checks, designChecks...)
	byName := map[string]check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["spec.operation_count"].OK {
		t.Error("operation 数不一致时应失败")
	}
	if byName["spec.endpoints_match_design"].OK {
		t.Error("多出 /api/v1/extra 时应失败")
	}
	if !byName["spec.refs_resolvable"].OK {
		t.Errorf("合法 $ref 应通过：%s", byName["spec.refs_resolvable"].Detail)
	}
}

func TestRequireFieldsMeta(t *testing.T) {
	spec := loadRealSpec(t)
	okBody := []byte(`{"version":"0.1.0","api_version":"v1","features":{"dry_run":true}}`)
	if missing := spec.requireFields("Meta", okBody); len(missing) != 0 {
		t.Fatalf("合法 Meta 不应缺字段，实际 %v", missing)
	}
	badBody := []byte(`{"version":"0.1.0"}`)
	missing := spec.requireFields("Meta", badBody)
	if !containsString(missing, "api_version") || !containsString(missing, "features") {
		t.Fatalf("缺字段检测不完整：%v", missing)
	}
}

// Tester 是 allOf（TesterSpec + 注册表元数据），required 必须把两边合并。
func TestRequireFieldsAllOf(t *testing.T) {
	spec := loadRealSpec(t)
	missing := spec.requireFields("Tester", []byte(`{}`))
	if !containsString(missing, "content_sha") {
		t.Fatalf("allOf 第二支的 required 没被合并：%v", missing)
	}
	if len(missing) < 8 {
		t.Fatalf("Tester 的 required 合并结果太少：%v", missing)
	}
}

func TestValidateSuccessMeta(t *testing.T) {
	spec := loadRealSpec(t)
	live := NewLive(spec, Options{BaseURL: "http://127.0.0.1:1", User: "dev", Timeout: 2 * time.Second})

	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=utf-8")
	body := []byte(`{"version":"0.1.0","api_version":"v1","features":{}}`)
	if issues := live.validateSuccess("getMeta", 200, header, body, "Meta"); len(issues) != 0 {
		t.Fatalf("合法响应不应报错：%v", issues)
	}

	// 包了一层 data 的响应必须被抓住
	wrapped := []byte(`{"data":{"version":"0.1.0","api_version":"v1","features":{}}}`)
	issues := live.validateSuccess("getMeta", 200, header, wrapped, "Meta")
	if len(issues) == 0 {
		t.Fatal("响应被包一层 data 时应报缺字段")
	}

	// 错误 Content-Type 必须被抓住
	badHeader := http.Header{}
	badHeader.Set("Content-Type", "text/plain")
	if issues := live.validateSuccess("getMeta", 200, badHeader, body, "Meta"); len(issues) == 0 {
		t.Fatal("Content-Type 不符应报错")
	}

	// spec 未声明的状态码必须被抓住
	if issues := live.validateSuccess("getMeta", 418, header, body, "Meta"); len(issues) == 0 {
		t.Fatal("未声明的状态码应报错")
	}
}

func TestErrorEnvelopeIssues(t *testing.T) {
	spec := loadRealSpec(t)
	live := NewLive(spec, Options{BaseURL: "http://127.0.0.1:1", Timeout: 2 * time.Second})

	flat := []byte(`{"code":"NOT_FOUND","message":"没有","detail":{"run_id":"r-1"},"retryable":false,"trace_id":"tx"}`)
	if issues := live.errorEnvelopeIssues(flat); len(issues) != 0 {
		t.Fatalf("扁平 Error 不应报错：%v", issues)
	}

	nested := []byte(`{"error":{"code":"NOT_FOUND","message":"没有","retryable":false}}`)
	if issues := live.errorEnvelopeIssues(nested); len(issues) == 0 {
		t.Fatal("嵌套 {error:{...}} 必须被拒绝（spec 是扁平 Error）")
	}

	missing := []byte(`{"code":"NOT_FOUND","message":"没有"}`)
	issues := live.errorEnvelopeIssues(missing)
	if !strings.Contains(strings.Join(issues, "；"), "retryable") || !strings.Contains(strings.Join(issues, "；"), "trace_id") {
		t.Fatalf("缺字段没被指出：%v", issues)
	}
}

// 控制面整体不可用时，Run() 不应 panic，也不应把它误判成断言失败。
func TestLiveAgainst404Server(t *testing.T) {
	spec := loadRealSpec(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"fake","retryable":false,"trace_id":"tx"}`)
	}))
	t.Cleanup(ts.Close)

	live := NewLive(spec, Options{
		BaseURL: ts.URL, User: "dev", Timeout: 3 * time.Second,
		MockURL: "http://127.0.0.1:1/v1", Profile: "k8s-npu",
	})
	checks, _, err := live.Run()
	if err != nil {
		t.Fatalf("404 服务不等于连不上，不应返回 connectErr：%v", err)
	}
	if len(checks) == 0 {
		t.Fatal("没有产生检查项")
	}
	byName := map[string]check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["live.getHealthz"].OK {
		t.Error("healthz 返回 404 时不应通过")
	}
	if byName["live.all_operations_covered"].OK {
		t.Error("全部 404 时覆盖率检查不应通过")
	}
}

func TestValidateErrorViaSpec(t *testing.T) {
	spec := loadRealSpec(t)
	live := NewLive(spec, Options{BaseURL: "http://127.0.0.1:1", Timeout: 2 * time.Second})
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=utf-8")
	body := []byte(`{"code":"NOT_FOUND","message":"没有","retryable":false,"trace_id":"tx"}`)
	if issues := live.validateError("getRun", 404, header, body); len(issues) != 0 {
		t.Fatalf("合法 404 Error 不应报错：%v", issues)
	}
	// 404 未在 getMeta 的响应里声明
	if issues := live.validateError("getMeta", 404, header, body); len(issues) == 0 {
		t.Fatal("未声明的 404 应报错")
	}
}

// 编译期确认 JSON 结构可序列化（供 --json-out 使用）。
func TestResultJSONSerializable(t *testing.T) {
	res := resultJSON{OK: true, Mode: "spec", SpecPath: realSpecPath, SpecOps: 25, Checks: []check{{Name: "x", OK: true}}}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"checks"`) {
		t.Fatalf("JSON 缺 checks：%s", raw)
	}
}

func writeTempSpec(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/openapi.yaml"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时 spec：%v", err)
	}
	return path
}
