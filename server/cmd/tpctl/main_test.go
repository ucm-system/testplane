package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// writeSSEFrames 把预拼好的帧写出并 flush，模拟控制面的 text/event-stream。
func writeSSEFrames(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("ResponseWriter 不支持 Flush")
	}
	for _, frame := range frames {
		if _, err := io.WriteString(w, frame); err != nil {
			return
		}
		flusher.Flush()
	}
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

func TestProfiles(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/profiles" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"items":[{"name":"k8s-npu","cluster":"cluster-01","namespace":"testplane","scheduler":"default","queue":null,"mounts":[{"name":"dataset","pvc":"data-pvc"}],"deploy_timeout_seconds":259200}],"has_more":false}`)
	})
	code, stdout, stderr := runCLI(t, srv.URL, "profiles")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	for _, want := range []string{"k8s-npu", "cluster=cluster-01", "mounts=dataset"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("profiles 输出缺少 %q：%s", want, stdout)
		}
	}

	code, stdout, _ = runCLI(t, srv.URL, "profiles", "--json")
	if code != exitOK {
		t.Fatalf("--json 退出码 = %d", code)
	}
	out := decodeJSON(t, stdout)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Errorf("profiles --json 输出 = %+v", out)
	}
}

func TestArtifactsRunAndTask(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/runs/r-1/artifacts":
			_, _ = io.WriteString(w, `{"items":[{"path":"report.json","kind":"report","size_bytes":12},{"path":"result.json","kind":"result","size_bytes":34,"task_id":"t-1"}],"by_kind":{"report":[{"path":"report.json","kind":"report","size_bytes":12}],"result":[{"path":"result.json","kind":"result","size_bytes":34,"task_id":"t-1"}]},"total_size_bytes":46}`)
		case "/api/v1/tasks/t-1/attempts/2/artifacts":
			_, _ = io.WriteString(w, `{"items":[{"path":"tasks/t-1/attempts/2/result.json","kind":"result","size_bytes":34}],"by_kind":{},"total_size_bytes":34}`)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	})

	code, stdout, stderr := runCLI(t, srv.URL, "artifacts", "r-1")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	for _, want := range []string{"产物 2 个", "[report]", "report.json", "[result]", "result.json"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("artifacts 输出缺少 %q：%s", want, stdout)
		}
	}

	code, stdout, _ = runCLI(t, srv.URL, "artifacts", "--task", "t-1", "--attempt", "2", "--json")
	if code != exitOK {
		t.Fatalf("--json 退出码 = %d", code)
	}
	out := decodeJSON(t, stdout)
	if out["total_size_bytes"] != float64(34) {
		t.Errorf("artifacts --json = %+v", out)
	}
}

func TestFilesToOutWithRange(t *testing.T) {
	var gotRange, gotPath string
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/r-1/files" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		gotRange = r.Header.Get("Range")
		gotPath = r.URL.Query().Get("path")
		w.Header().Set("Content-Range", "bytes 0-4/10")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "hello")
	})

	out := filepath.Join(t.TempDir(), "nested", "stdout.log")
	code, stdout, stderr := runCLI(t, srv.URL, "files", "r-1", "tasks/t-1/attempts/1/logs/stdout.log",
		"--range", "0-4", "--out", out, "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if gotRange != "bytes=0-4" || gotPath != "tasks/t-1/attempts/1/logs/stdout.log" {
		t.Errorf("请求形状不对: range=%q path=%q", gotRange, gotPath)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "hello" {
		t.Fatalf("落盘内容 = %q, err=%v", data, err)
	}
	sum := sha256.Sum256([]byte("hello"))
	outJSON := decodeJSON(t, stdout)
	if outJSON["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %v", outJSON["sha256"])
	}
	if outJSON["sha256_scope"] != "partial" || outJSON["size_bytes"] != float64(5) || outJSON["total_size_bytes"] != float64(10) {
		t.Errorf("files 输出 = %+v", outJSON)
	}
	if outJSON["out"] != out {
		t.Errorf("out = %v", outJSON["out"])
	}
}

func TestFilesToStdout(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain body")
	})
	code, stdout, stderr := runCLI(t, srv.URL, "files", "r-1", "raw/a.log")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if stdout != "plain body" {
		t.Errorf("stdout = %q，期望原样 body", stdout)
	}
	if !strings.Contains(stderr, "sha256=") || !strings.Contains(stderr, "HTTP 200") {
		t.Errorf("stderr 应带下载摘要，实际 %q", stderr)
	}
}

func TestFilesStripsRunIDPrefix(t *testing.T) {
	var gotPath string
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		_, _ = io.WriteString(w, "data")
	})
	// artifacts 返回的 path 带 <run_id>/ 前缀；files 应自动剥成 run 内相对路径。
	code, _, stderr := runCLI(t, srv.URL, "files", "r-1", "r-1/tasks/t-1/attempts/1/result.json", "--json", "--out", filepath.Join(t.TempDir(), "f.json"))
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	if gotPath != "tasks/t-1/attempts/1/result.json" {
		t.Errorf("path 查询 = %q，期望剥离 run_id 前缀", gotPath)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestFilesStreamsLargeDownload(t *testing.T) {
	const size = 33 << 20 // 超过普通应答的 32 MiB 内存上限，必须走流式
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.CopyN(w, zeroReader{}, size)
	})
	out := filepath.Join(t.TempDir(), "big.bin")
	code, stdout, stderr := runCLI(t, srv.URL, "files", "r-1", "raw/big.bin", "--out", out, "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() != size {
		t.Fatalf("落盘大小 = %v, err=%v，期望 %d", info, err, size)
	}
	outJSON := decodeJSON(t, stdout)
	if outJSON["size_bytes"] != float64(size) || outJSON["sha256_scope"] != "full" {
		t.Errorf("large files 输出 = %+v", outJSON)
	}
}

func TestFilesJSONRequiresOut(t *testing.T) {
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("用法错误不应访问控制面")
	})
	code, _, _ := runCLI(t, srv.URL, "files", "r-1", "raw/a.log", "--json")
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 64", code)
	}
}

func TestWaitFollowSSE(t *testing.T) {
	var gets int32
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/runs/r-1":
			n := atomic.AddInt32(&gets, 1)
			status := "testing"
			if n >= 2 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"r-1","name":"smoke","status":%q,"task_counts":{"total":1,"succeeded":1}}`, status)
		case "/api/v1/runs/r-1/events":
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("Accept = %q", r.Header.Get("Accept"))
			}
			writeSSEFrames(t, w,
				"id: 1\nevent: run.state_changed\ndata: {\"seq\":1,\"type\":\"run.state_changed\",\"to\":\"succeeded\"}\n\n",
				"id: 2\nevent: run.finished\ndata: {\"seq\":2,\"type\":\"run.finished\",\"to\":\"succeeded\"}\n\n",
			)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	})
	code, stdout, stderr := runCLI(t, srv.URL, "wait", "r-1", "--follow", "--timeout", "5", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["wait_mode"] != "sse" || out["outcome"] != "success" || out["status"] != "succeeded" {
		t.Errorf("wait --follow 输出 = %+v", out)
	}
	if !strings.Contains(stderr, "status=testing") {
		t.Errorf("stderr 应带状态进度，实际 %q", stderr)
	}
}

func TestWaitFollowReconnects(t *testing.T) {
	var gets, events int32
	var lastID string
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/runs/r-1/events":
			if atomic.AddInt32(&events, 1) == 1 {
				// 发一条事件后断线（没有 run.finished）。
				writeSSEFrames(t, w, "id: 5\nevent: task.state_changed\ndata: {\"seq\":5,\"type\":\"task.state_changed\",\"to\":\"running\"}\n\n")
				return
			}
			lastID = r.Header.Get("Last-Event-ID")
			writeSSEFrames(t, w, "id: 6\nevent: run.finished\ndata: {\"seq\":6,\"type\":\"run.finished\",\"to\":\"succeeded\"}\n\n")
		case "/api/v1/runs/r-1":
			n := atomic.AddInt32(&gets, 1)
			status := "testing"
			if n >= 3 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"r-1","name":"smoke","status":%q}`, status)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	})
	code, stdout, stderr := runCLI(t, srv.URL, "wait", "r-1", "--follow", "--timeout", "5", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["wait_mode"] != "sse" || out["outcome"] != "success" {
		t.Errorf("重连输出 = %+v", out)
	}
	if lastID != "5" {
		t.Errorf("重连的 Last-Event-ID = %q，期望 5", lastID)
	}
	if atomic.LoadInt32(&events) != 2 {
		t.Errorf("SSE 连接次数 = %d，期望 2", events)
	}
}

func TestWaitFollowFallsBackToPoll(t *testing.T) {
	var gets int32
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/runs/r-1/events":
			http.Error(w, "no sse", http.StatusNotFound)
		case "/api/v1/runs/r-1":
			n := atomic.AddInt32(&gets, 1)
			status := "testing"
			if n >= 2 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"r-1","name":"smoke","status":%q}`, status)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	})
	code, stdout, stderr := runCLI(t, srv.URL, "wait", "r-1", "--follow", "--interval", "0.02", "--timeout", "5", "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d，stderr=%s", code, stderr)
	}
	out := decodeJSON(t, stdout)
	if out["wait_mode"] != "poll" || out["outcome"] != "success" || out["fallback_reason"] == "" {
		t.Errorf("降级输出 = %+v", out)
	}
	if !strings.Contains(stderr, "降级为轮询") {
		t.Errorf("stderr 应说明降级，实际 %q", stderr)
	}
}

func TestErrorNextActionCLI(t *testing.T) {
	// 1) 控制面错误：--json 时 error.next_action 出现在 stderr 的 JSON 文档里。
	srv := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"VALIDATION_ERROR","message":"字段不合法","retryable":false,"trace_id":"01J","detail":{"issues":[{"path":"tests[0].args","code":"ARGS_LEX_ERROR","message":"未闭合引号"}]}}`)
	})
	code, _, stderr := runCLI(t, srv.URL, "status", "r-1", "--json")
	if code != exitFailure {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(stderr), &errDoc); err != nil {
		t.Fatalf("stderr 应为错误 JSON: %v（原文 %q）", err, stderr)
	}
	errObj, _ := errDoc["error"].(map[string]any)
	if errObj["next_action"] == nil || !strings.Contains(fmt.Sprint(errObj["next_action"]), "issues") {
		t.Errorf("error.next_action = %v", errObj["next_action"])
	}

	// 2) Run 终态失败：wait 的 JSON 带 next_action，人读模式打 stderr。
	srv2 := newFakeControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"r-1","name":"smoke","status":"failed","error":{"code":"PRECHECK_FAILED","message":"precheck failed","retryable":false}}`)
	})
	code, stdout, stderr := runCLI(t, srv2.URL, "wait", "r-1", "--timeout", "5", "--json")
	if code != exitFailure {
		t.Fatalf("退出码 = %d，期望 1", code)
	}
	out := decodeJSON(t, stdout)
	if out["outcome"] != "failed" || !strings.Contains(fmt.Sprint(out["next_action"]), "precheck") {
		t.Errorf("wait 失败输出 = %+v", out)
	}
	if !strings.Contains(stderr, "next_action") {
		t.Errorf("stderr 应带 next_action，实际 %q", stderr)
	}
}

func TestErrorHintForEveryCode(t *testing.T) {
	// CLI 侧把共享映射接进 fail()：每个已知错误码都要在 --json stderr 里带 error.next_action。
	codes := []string{
		"VALIDATION_ERROR", "UNAUTHENTICATED", "FORBIDDEN", "NOT_FOUND", "CONFLICT",
		"TESTER_NOT_FOUND", "TESTER_VERSION_PINNED", "TESTER_INVALID",
		"EXECUTION_PROFILE_NOT_FOUND", "RUN_TOO_LARGE", "RESOURCE_EXHAUSTED",
		"ARTIFACT_QUOTA_EXCEEDED", "EXECUTOR_UNAVAILABLE", "EXECUTOR_POD_ERROR",
		"IMAGE_PULL_FAILED", "MOUNT_UNAVAILABLE", "ENDPOINT_UNREACHABLE", "PRECHECK_FAILED",
		"TASK_TIMEOUT", "TESTER_NONZERO_EXIT", "RESULT_MISSING", "RESULT_INVALID",
		"CLEANUP_FAILED", "CANCELED", "INTERNAL_ERROR", "DEPLOY_FAILED", "DEPLOY_BLOCKED",
		"SERVICE_UNAVAILABLE",
	}
	for _, code := range codes {
		var stdout, stderr bytes.Buffer
		app := &app{stdout: &stdout, stderr: &stderr, jsonOut: true}
		if got := app.fail(&apiError{Status: http.StatusBadRequest, Code: code, Message: "x"}); got != exitFailure {
			t.Fatalf("%s: 退出码 = %d，期望 1", code, got)
		}
		var doc struct {
			Error struct {
				Code       string `json:"code"`
				NextAction string `json:"next_action"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stderr.Bytes(), &doc); err != nil {
			t.Fatalf("%s: stderr 不是 JSON: %v（%q）", code, err, stderr.String())
		}
		if doc.Error.Code != code || doc.Error.NextAction == "" {
			t.Errorf("%s: error = %+v，缺 next_action", code, doc.Error)
		}
	}

	// 未知错误码不应编造 next_action。
	var stdout, stderr bytes.Buffer
	app := &app{stdout: &stdout, stderr: &stderr, jsonOut: true}
	app.fail(&apiError{Status: http.StatusBadRequest, Code: "SOME_NEW_CODE", Message: "x"})
	if strings.Contains(stderr.String(), "next_action") {
		t.Errorf("未知错误码不应有 next_action: %s", stderr.String())
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
		{name: "artifacts without id", args: []string{"artifacts"}},
		{name: "artifacts task with positional", args: []string{"artifacts", "r-1", "--task", "t-1"}},
		{name: "files without path", args: []string{"files", "r-1"}},
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
