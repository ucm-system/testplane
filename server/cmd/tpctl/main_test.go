package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// 本文件用 httptest 假控制面驱动 tpctl，断言命令输出、退出码与请求形状。
// 全部离线：不需要 PostgreSQL / K8s / 真控制面。

func runCLI(t *testing.T, baseURL string, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWithStdin(t, baseURL, "", args...)
}

func runCLIWithStdin(t *testing.T, baseURL, stdin string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("TESTPLANE_BASE_URL", baseURL)
	t.Setenv("TESTPLANE_TOKEN", "test-token")
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func newFakeControlPlane(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("stdout 不是 JSON: %v（原文 %q）", err, s)
	}
	return v
}

// ---- submit ---------------------------------------------------------------

func TestSubmitJSON(t *testing.T) {
	var gotPath, gotCT, gotAuth string
	var gotBody []byte
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"run":{"id":"r-1","status":"queued","kind":"run","submitted_at":"2026-11-05T01:00:00Z"},"tasks":[{"id":"t-0","name":"precheck","status":"pending"},{"id":"t-1","name":"acc","status":"pending"}]}`)
	})

	dir := t.TempDir()
	manifest := filepath.Join(dir, "run.yaml")
	if err := os.WriteFile(manifest, []byte("run:\n  name: smoke\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, srv.URL, "submit", "-f", manifest, "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if gotPath != "/api/v1/runs" || gotCT != "application/yaml" || gotAuth != "Bearer test-token" {
		t.Errorf("请求形状不对: path=%s ct=%s auth=%s", gotPath, gotCT, gotAuth)
	}
	if string(gotBody) != "run:\n  name: smoke\n" {
		t.Errorf("Manifest 被改写: %q", gotBody)
	}
	out := decodeJSON(t, stdout)
	if out["run_id"] != "r-1" || out["status"] != "queued" {
		t.Errorf("submit 输出 = %+v", out)
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 2 {
		t.Errorf("tasks 数 = %d，期望 2", len(tasks))
	}
}

func TestSubmitDryRunInvalidExitsOne(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dry_run") != "true" {
			t.Errorf("dry_run 查询参数缺失: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"valid":false,"issues":[{"path":"tests[0].args","code":"PUBLIC_PARAM_CONFLICT","message":"--url 由平台注入"}]}`)
	})
	dir := t.TempDir()
	manifest := filepath.Join(dir, "run.yaml")
	_ = os.WriteFile(manifest, []byte("run:\n  name: smoke\n"), 0o600)

	code, stdout, _ := runCLI(t, srv.URL, "submit", "-f", manifest, "--dry-run", "--json")
	if code != exitFailure {
		t.Fatalf("退出码 = %d，期望 1（校验失败）", code)
	}
	out := decodeJSON(t, stdout)
	if out["valid"] != false {
		t.Errorf("dry-run 输出 = %+v", out)
	}
}

func TestSubmitFromStdin(t *testing.T) {
	var gotCT string
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"run":{"id":"r-2","status":"queued","kind":"run"},"tasks":[]}`)
	})
	code, _, stderr := runCLIWithStdin(t, srv.URL, `{"run":{"name":"x"}}`, "submit", "-f", "-", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", gotCT)
	}
}

// ---- status / wait --------------------------------------------------------

func TestStatusJSON(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/r-1" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"r-1","name":"smoke","kind":"run","status":"testing","submitted_at":"2026-11-05T01:00:00Z","task_counts":{"total":2,"pending":1,"running":1,"succeeded":0,"failed":0,"skipped":0},"error":null,"tasks":[{"id":"t-0","name":"precheck","status":"succeeded","attempts":1},{"id":"t-1","name":"acc","status":"running","attempts":1}]}`)
	})
	code, stdout, stderr := runCLI(t, srv.URL, "status", "r-1", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["run_id"] != "r-1" || out["status"] != "testing" || out["terminal"] != false {
		t.Errorf("status 输出 = %+v", out)
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 2 {
		t.Errorf("tasks 数 = %d", len(tasks))
	}
}

func TestWaitRunOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		wantCode  int
		wantOut   string
		wantCalls int32
	}{
		{name: "succeeded", status: "succeeded", wantCode: exitOK, wantOut: "success", wantCalls: 1},
		{name: "failed", status: "failed", wantCode: exitFailure, wantOut: "failed", wantCalls: 1},
		{name: "completed_with_errors", status: "completed_with_errors", wantCode: exitFailure, wantOut: "failed", wantCalls: 1},
		{name: "service_unavailable", status: "service_unavailable", wantCode: exitFailure, wantOut: "failed", wantCalls: 1},
		{name: "canceled", status: "canceled", wantCode: exitCanceled, wantOut: "canceled", wantCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				_, _ = io.WriteString(w, `{"id":"r-1","status":"`+tc.status+`","task_counts":{"total":1,"succeeded":1,"failed":0}}`)
			})
			code, stdout, stderr := runCLI(t, srv.URL, "wait", "r-1", "--timeout", "5", "--interval", "0.01", "--json")
			if code != tc.wantCode {
				t.Fatalf("退出码 = %d，期望 %d（stderr=%s）", code, tc.wantCode, stderr)
			}
			out := decodeJSON(t, stdout)
			if out["outcome"] != tc.wantOut || out["terminal"] != true {
				t.Errorf("wait 输出 = %+v", out)
			}
			if atomic.LoadInt32(&calls) != tc.wantCalls {
				t.Errorf("轮询次数 = %d，期望 %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestWaitPollsUntilTerminal(t *testing.T) {
	var calls int32
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		status := "testing"
		if n >= 3 {
			status = "succeeded"
		}
		_, _ = io.WriteString(w, `{"id":"r-1","status":"`+status+`"}`)
	})
	code, stdout, stderr := runCLI(t, srv.URL, "wait", "r-1", "--timeout", "5", "--interval", "0.01", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["outcome"] != "success" {
		t.Errorf("wait 输出 = %+v", out)
	}
	if !strings.Contains(stderr, "status=testing") {
		t.Errorf("stderr 应包含状态变化进度，实际 %q", stderr)
	}
}

func TestWaitTimeoutExitsThree(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"r-1","status":"testing"}`)
	})
	code, stdout, stderr := runCLI(t, srv.URL, "wait", "r-1", "--timeout", "0.15", "--interval", "0.03", "--json")
	if code != exitTimeout {
		t.Fatalf("退出码 = %d，期望 3（stderr=%s）", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["outcome"] != "timeout" || out["status"] != "testing" {
		t.Errorf("wait 超时输出 = %+v", out)
	}
}

func TestWaitUnreachableExitsFour(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	code, _, stderr := runCLI(t, "http://"+addr, "wait", "r-1", "--timeout", "1")
	if code != exitUnreachable {
		t.Fatalf("退出码 = %d，期望 4（stderr=%s）", code, stderr)
	}
	if !strings.Contains(stderr, "连不上控制面") {
		t.Errorf("stderr = %q", stderr)
	}
}

// ---- report / logs / cancel ----------------------------------------------

func TestReportMarkdown(t *testing.T) {
	const md = "# r-1\n\n- 状态：succeeded\n"
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/r-1/report.md" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, md)
	})
	code, stdout, stderr := runCLI(t, srv.URL, "report", "r-1", "--md")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if stdout != md {
		t.Errorf("Markdown 原文被改写: %q", stdout)
	}
}

func TestReportJSONPrettyAndCompact(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"run_id":"r-1","status":"succeeded","counts":{"total":1,"succeeded":1,"failed":0,"skipped":0,"canceled":0},"tasks":[],"failures":[]}`)
	})
	code, pretty, _ := runCLI(t, srv.URL, "report", "r-1")
	if code != exitOK {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(pretty, "\n  \"run_id\"") {
		t.Errorf("默认输出应为缩进 JSON，实际 %q", pretty)
	}
	code, compact, _ := runCLI(t, srv.URL, "report", "r-1", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d", code)
	}
	if strings.Count(strings.TrimSpace(compact), "\n") != 0 {
		t.Errorf("--json 应输出单行，实际 %q", compact)
	}
}

func TestLogsReportsOffsetToStderr(t *testing.T) {
	var gotPath, gotTail string
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTail = r.URL.Query().Get("tail")
		w.Header().Set("X-Log-Offset", "4096")
		_, _ = io.WriteString(w, "hello log\n")
	})
	code, stdout, stderr := runCLI(t, srv.URL, "logs", "t-1", "--attempt", "2", "--tail", "5", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if gotPath != "/api/v1/tasks/t-1/attempts/2/logs" || gotTail != "5" {
		t.Errorf("请求形状不对: path=%q tail=%q", gotPath, gotTail)
	}
	if !strings.Contains(stderr, "X-Log-Offset: 4096") {
		t.Errorf("stderr 缺少 X-Log-Offset，实际 %q", stderr)
	}
	out := decodeJSON(t, stdout)
	if out["text"] != "hello log\n" || out["next_offset"] != float64(4096) {
		t.Errorf("logs 输出 = %+v", out)
	}

	// 非 JSON 模式：stdout 是日志原文，stderr 仍是续读坐标。
	code, stdout, _ = runCLI(t, srv.URL, "logs", "t-1", "--attempt", "2")
	if code != exitOK || stdout != "hello log\n" {
		t.Errorf("原文模式: code=%d stdout=%q", code, stdout)
	}
}

func TestCancelTask(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/t-1/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"task_id":"t-1","run_id":"r-1","status":"canceling"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/t-1":
			_, _ = io.WriteString(w, `{"id":"t-1","status":"canceled"}`)
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
	})
	code, stdout, stderr := runCLI(t, srv.URL, "cancel", "--task", "t-1", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["target"] != "task" || out["accepted"] != true || out["confirmed"] != true || out["status"] != "canceled" {
		t.Errorf("cancel 输出 = %+v", out)
	}
}

func TestCancelRun(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/r-1/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"r-1","status":"cancelling"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/r-1":
			_, _ = io.WriteString(w, `{"id":"r-1","status":"cancelling"}`)
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
	})
	code, stdout, _ := runCLI(t, srv.URL, "cancel", "r-1", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d", code)
	}
	out := decodeJSON(t, stdout)
	if out["target"] != "run" || out["status"] != "cancelling" {
		t.Errorf("cancel 输出 = %+v", out)
	}
}

// ---- testers / queue / profiles / 用法 -----------------------------------

func TestTestersJSON(t *testing.T) {
	var gotQuery string
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"items":[{"name":"endpoint-probe","version":"1.0.0","role":"pre_check"}],"has_more":false}`)
	})
	code, stdout, _ := runCLI(t, srv.URL, "testers", "--role", "pre_check", "--orphaned", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(gotQuery, "role=pre_check") || !strings.Contains(gotQuery, "orphaned=true") {
		t.Errorf("查询参数 = %q", gotQuery)
	}
	out := decodeJSON(t, stdout)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Errorf("testers 输出 = %+v", out)
	}
}

func TestQueueHuman(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"generated_at":"2026-11-05T01:00:00Z","slots":{"concurrency_max":20,"active":7,"available":13,"endpoints":[]},"waiting":{"total":2,"by_reason":{"precheck_gate":2}},"volcano":{"available":false,"detail":"scheduler_unavailable"}}`)
	})
	code, stdout, stderr := runCLI(t, srv.URL, "queue")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	for _, want := range []string{"active 7/20", "waiting: 2", "precheck_gate", "scheduler_unavailable"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("queue 输出缺少 %q：%s", want, stdout)
		}
	}
}

func TestProfilesNotImplemented(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // MVP 没有 GET /api/v1/profiles
	})
	code, _, stderr := runCLI(t, srv.URL, "profiles")
	if code != exitFailure {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "GET /api/v1/profiles") || !strings.Contains(stderr, "P1") {
		t.Errorf("stderr 应给出明确原因，实际 %q", stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("用法错误不应访问控制面: %s %s", r.Method, r.URL.Path)
	})
	tests := []struct {
		name string
		args []string
	}{
		{name: "no command", args: nil},
		{name: "unknown command", args: []string{"nope"}},
		{name: "submit without file", args: []string{"submit"}},
		{name: "status without id", args: []string{"status"}},
		{name: "cancel without target", args: []string{"cancel"}},
		{name: "wait bad interval", args: []string{"wait", "r-1", "--interval", "0"}},
		{name: "logs bad stream", args: []string{"logs", "t-1", "--stream", "both"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := runCLI(t, srv.URL, tc.args...)
			if code != exitUsage {
				t.Fatalf("退出码 = %d，期望 64", code)
			}
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, stdout, _ := runCLI(t, "http://127.0.0.1:1", "help")
	if code != exitOK {
		t.Fatalf("help 退出码 = %d", code)
	}
	if !strings.Contains(stdout, "tpctl") || !strings.Contains(stdout, "退出码") {
		t.Errorf("help 输出不完整: %s", stdout)
	}
	if code, _, _ := runCLI(t, "http://127.0.0.1:1", "--help"); code != exitOK {
		t.Errorf("--help 退出码 = %d", code)
	}
}

// TestGlobalFlagsBeforeCommand 验证 --base-url/--token 写在命令前时生效。
func TestGlobalFlagsBeforeCommand(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer custom-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"id":"r-1","status":"succeeded"}`)
	})
	t.Setenv("TESTPLANE_BASE_URL", "http://127.0.0.1:1") // 故意给错的默认值
	var stdout, stderr bytes.Buffer
	code := run([]string{"--base-url", srv.URL, "--token", "custom-token", "status", "r-1", "--json"},
		strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr.String())
	}
}
