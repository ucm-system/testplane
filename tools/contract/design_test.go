package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadRealDesign(t *testing.T) *DesignCatalog {
	t.Helper()
	design, err := LoadDesign(DefaultDesignPath())
	if err != nil {
		t.Fatalf("LoadDesign: %v", err)
	}
	return design
}

func loadRealAllow(t *testing.T) map[string]string {
	t.Helper()
	allow, err := LoadAllowMissing(DefaultAllowMissingPath())
	if err != nil {
		t.Fatalf("LoadAllowMissing: %v", err)
	}
	return allow
}

// 设计端点集合必须能从 07-api.md §5（外加 §2.4 认证表、§5.5 运维段）解析出来，
// 而且不能只解析出 openapi 已有的那 25 个（否则又变成自证）。
func TestLoadRealDesign(t *testing.T) {
	design := loadRealDesign(t)
	if len(design.Endpoints) <= 25 {
		t.Fatalf("设计端点数 %d，应明显多于 openapi 的 25（否则解析失效）", len(design.Endpoints))
	}
	for _, want := range []string{
		"GET /api/v1/profiles",
		"POST /api/v1/admin/profiles",
		"GET /metrics",
		"GET /api/v1/auth/login",
		"GET /api/v1/me",
		"POST /api/v1/runs",
	} {
		if !design.Set[want] {
			t.Errorf("设计端点解析缺 %s；现有 %d 个", want, len(design.Endpoints))
		}
	}
}

func TestLoadAllowMissingRejectsMissingReason(t *testing.T) {
	dir := t.TempDir()
	cases := []string{
		"GET /api/v1/foo\n",                // 没有 # / 指针
		"GET /api/v1/foo #\n",              // 理由/指针为空
		"GET /api/v1/foo # 理由\n",           // 缺箭头
		"GET /api/v1/foo # 理由 → bad-ref\n", // 指针非法
		"GET /api/v1/foo # 理由 → IMPLEMENTATION.md:1\nGET /api/v1/foo # 理由 → IMPLEMENTATION.md:1\n", // 重复
	}
	for i, content := range cases {
		p := filepath.Join(dir, "allow-"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAllowMissing(p); err == nil {
			t.Errorf("第 %d 个白名单非法（缺理由/指针/重复）却通过了：%q", i+1, content)
		}
	}
}

func TestLoadAllowMissingOK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "allow.txt")
	content := "# comment\nGET /api/v1/foo # 已登记理由 → IMPLEMENTATION.md:439\nPOST /api/v1/bar # 另一个理由 → #19\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	allow, err := LoadAllowMissing(p)
	if err != nil {
		t.Fatalf("LoadAllowMissing: %v", err)
	}
	if allow["GET /api/v1/foo"] == "" || allow["POST /api/v1/bar"] == "" {
		t.Fatalf("白名单解析不完整：%v", allow)
	}
}

// 表格行被拆成两行/列数异常时必须显式失败，不能静默少解析。
func TestLoadDesignRejectsMalformedTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "07-api.md")
	content := `## 5. 端点目录

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/v1/ok | good |
| GET | /api/v1/broken
| GET | /api/v1/later | extra |
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDesign(p); err == nil {
		t.Fatal("端点表列数异常时应报错，不能静默通过")
	}
}

// 设计端点数量基线：少一个必须判红。
func TestDesignEndpointBaselineCatchesShrink(t *testing.T) {
	design := &DesignCatalog{
		Path:          "fixture.md",
		Set:           map[string]bool{},
		Endpoints:     []DesignEndpoint{{Method: "GET", Path: "/api/v1/only-one"}},
		BaselineCount: 2,
	}
	checks, _ := (&Spec{}).DesignChecks(design, nil)
	for _, c := range checks {
		if c.Name == "spec.design_endpoint_baseline" {
			if c.OK {
				t.Fatalf("设计端点从 2 降到 1 仍未判红：%s", c.Detail)
			}
			return
		}
	}
	t.Fatal("缺少 spec.design_endpoint_baseline 检查")
}

// 真实 openapi 与真实设计 + 真实白名单比对必须通过（当前 MVP 基线）。
func TestRealSpecDesignChecksPass(t *testing.T) {
	spec := loadRealSpec(t)
	checks, warnings := spec.DesignChecks(loadRealDesign(t), loadRealAllow(t))
	for _, c := range checks {
		if !c.OK {
			t.Errorf("设计端点检查失败：%s —— %s", c.Name, c.Detail)
		}
	}
	// 差集报告必须输出（白名单放行的 25 条）。
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "设计有、openapi 无") {
			found = true
		}
	}
	if !found {
		t.Errorf("缺少设计/openapi 差集报告；warnings=%v", warnings)
	}
}

// 未登记的“设计有、openapi 无”必须判红，不能自证通过。
func TestDesignChecksCatchUnregisteredMissing(t *testing.T) {
	design := &DesignCatalog{
		Path: "fixture.md",
		Set: map[string]bool{
			"GET /api/v1/present": true,
			"GET /api/v1/missing": true,
		},
		Endpoints: []DesignEndpoint{
			{Method: "GET", Path: "/api/v1/present"},
			{Method: "GET", Path: "/api/v1/missing"},
		},
	}
	spec := &Spec{Operations: []Operation{{Method: "GET", Path: "/api/v1/present"}}}
	checks, _ := spec.DesignChecks(design, nil)
	byName := map[string]check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["spec.design_endpoints_present"].OK {
		t.Fatal("未登记的设计缺失端点应判红")
	}
	if byName["spec.endpoints_match_design"].OK {
		t.Fatal("端点集合不一致时应判红")
	}
	if !strings.Contains(byName["spec.design_endpoints_present"].Detail, "/api/v1/missing") {
		t.Fatalf("报告未列出缺失端点：%s", byName["spec.design_endpoints_present"].Detail)
	}
}

// openapi 多出来的端点也必须判红（反向差集）。
func TestDesignChecksCatchOpenapiOnly(t *testing.T) {
	design := &DesignCatalog{
		Path: "fixture.md",
		Set:  map[string]bool{"GET /api/v1/present": true},
		Endpoints: []DesignEndpoint{
			{Method: "GET", Path: "/api/v1/present"},
		},
	}
	spec := &Spec{Operations: []Operation{
		{Method: "GET", Path: "/api/v1/present"},
		{Method: "GET", Path: "/api/v1/extra"},
	}}
	checks, _ := spec.DesignChecks(design, nil)
	byName := map[string]check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["spec.openapi_endpoints_in_design"].OK {
		t.Fatal("openapi 多出来的端点应判红")
	}
	if !strings.Contains(byName["spec.openapi_endpoints_in_design"].Detail, "/api/v1/extra") {
		t.Fatalf("报告未列出多余端点：%s", byName["spec.openapi_endpoints_in_design"].Detail)
	}
}

// /report、/report.md 的 404 只允许在非终态 Run 上；终态必须 200。
func TestArtifactResponseTerminalSemantics(t *testing.T) {
	spec := loadRealSpec(t)
	live := NewLive(spec, Options{BaseURL: "http://127.0.0.1:1"})
	errHeader := http.Header{}
	errHeader.Set("Content-Type", "application/json; charset=utf-8")
	errBody := []byte(`{"code":"NOT_FOUND","message":"报告尚未生成","retryable":false,"trace_id":"t-1"}`)

	if ok, detail := live.artifactResponseCheck("getRunReport", false, 404, errHeader, errBody, "Report", ""); !ok {
		t.Fatalf("非终态 Run 的 404 应允许：%s", detail)
	}
	if ok, detail := live.artifactResponseCheck("getRunReport", true, 404, errHeader, errBody, "Report", ""); ok {
		t.Fatal("终态 Run 的 /report 404 必须判红（这正是评审指出的“接受 404 一刀切”）")
	} else if !strings.Contains(detail, "必须返回 200") {
		t.Fatalf("终态 404 的失败原因不明确：%s", detail)
	}
	mdHeader := http.Header{}
	mdHeader.Set("Content-Type", "text/markdown; charset=utf-8")
	if ok, detail := live.artifactResponseCheck("getRunReportMarkdown", true, 200, mdHeader, []byte("# report\n"), "", "text/markdown"); !ok {
		t.Fatalf("终态 Run 的 report.md 200 应通过：%s", detail)
	}
	if ok, _ := live.artifactResponseCheck("getRunReport", true, 500, errHeader, errBody, "Report", ""); ok {
		t.Fatal("500 必须判红")
	}
}

// 终态 Run 的 /report 404 必须判红（模拟“注释掉 /report 路由”）。
func TestProbeRunReportTerminal404Fails(t *testing.T) {
	spec := loadRealSpec(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/api/v1/runs/r-test":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"succeeded"}`))
		case "/api/v1/runs/r-test/report":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NOT_FOUND","message":"报告未生成","retryable":false,"trace_id":"t"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NOT_FOUND","message":"x","retryable":false,"trace_id":"t"}`))
		}
	}))
	t.Cleanup(ts.Close)
	live := NewLive(spec, Options{BaseURL: ts.URL, User: "dev"})
	live.probeRunReport(context.Background(), "r-test", "/report", "Report", "getRunReport")
	for _, c := range live.checks {
		if c.Name == "live.getRunReport" {
			if c.OK {
				t.Fatal("终态 Run 的 /report 404 必须判红")
			}
			if !strings.Contains(c.Detail, "必须返回 200") {
				t.Fatalf("失败原因不明确：%s", c.Detail)
			}
			return
		}
	}
	t.Fatal("没有产生 live.getRunReport 检查")
}

func TestIsTerminalRunStatus(t *testing.T) {
	for _, s := range []string{"succeeded", "completed_with_errors", "service_unavailable", "failed", "canceled"} {
		if !isTerminalRunStatus(s) {
			t.Errorf("%s 应是终态", s)
		}
	}
	for _, s := range []string{"submitted", "queued", "deploying", "testing", "cleanup", "cancelling"} {
		if isTerminalRunStatus(s) {
			t.Errorf("%s 不应是终态", s)
		}
	}
}
