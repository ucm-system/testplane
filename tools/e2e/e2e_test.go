package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake 控制面 ---------------------------------------------------------

// fakePlane 是一个只覆盖 e2e 用到端点的内存实现：
// 它同时充当"契约的活样例"，让 e2e 在没有真后端时也能被验证（红/绿两条路都测）。
type fakePlane struct {
	mu sync.Mutex

	prefix string
	runID  string

	statuses  []string
	statusIdx int
	run       Run

	submitStatus int
	submitBody   []byte
	submitCalls  int
	lastAuth     string
	lastBody     string

	reportBody  []byte
	report404   int
	reportCalls int

	events     []Event
	attempts   map[string][]Attempt
	logs       map[string]string
	results    map[string]string
	sseEnabled bool

	omitSeq0FromTaskList bool
}

func (f *fakePlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.mu.Unlock()

	path := r.URL.Path
	if f.prefix != "" {
		if !strings.HasPrefix(path, f.prefix) {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"code": "NOT_FOUND", "message": "wrong prefix: " + path},
			})
			return
		}
		path = strings.TrimPrefix(path, f.prefix)
	}

	switch {
	case r.Method == http.MethodPost && path == "/runs":
		f.handleSubmit(w, r)
	case r.Method == http.MethodGet && path == "/runs/"+f.runID:
		f.handleRun(w)
	case path == "/runs/"+f.runID+"/tasks":
		f.handleRunTasks(w)
	case path == "/runs/"+f.runID+"/report":
		f.handleReport(w)
	case path == "/runs/"+f.runID+"/report.md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, "# "+f.runID+"\n\nok\n")
	case path == "/runs/"+f.runID+"/events/history":
		writeFakeJSON(w, http.StatusOK, map[string]any{"events": f.events})
	case path == "/runs/"+f.runID+"/events":
		f.handleSSE(w, r)
	case strings.HasPrefix(path, "/tasks/"):
		f.handleTask(w, r, path)
	default:
		writeFakeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "NOT_FOUND", "message": "fake 没有路由：" + r.Method + " " + r.URL.Path},
		})
	}
}

func (f *fakePlane) handleSubmit(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.submitCalls++
	f.lastBody = string(body)
	status, respBody := f.submitStatus, f.submitBody
	f.mu.Unlock()

	if status != 0 && status != http.StatusAccepted {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}
	writeFakeJSON(w, http.StatusAccepted, map[string]any{
		"run": map[string]any{
			"id": f.runID, "status": "queued", "kind": "run",
			"submitted_at": "2026-11-05T02:00:00Z",
		},
		"tasks": []any{
			map[string]any{"id": "t-1", "name": "precheck", "status": "pending"},
			map[string]any{"id": "t-2", "name": "probe-a", "status": "pending"},
		},
	})
}

func (f *fakePlane) handleRun(w http.ResponseWriter) {
	f.mu.Lock()
	idx := f.statusIdx
	if idx < len(f.statuses)-1 {
		f.statusIdx++
	}
	status := f.run.Status
	if len(f.statuses) > 0 {
		if idx >= len(f.statuses) {
			idx = len(f.statuses) - 1
		}
		status = f.statuses[idx]
	}
	run := f.run
	run.Status = status
	f.mu.Unlock()
	writeFakeJSON(w, http.StatusOK, run)
}

func (f *fakePlane) handleRunTasks(w http.ResponseWriter) {
	f.mu.Lock()
	items := make([]Task, 0, len(f.run.Tasks))
	for _, t := range f.run.Tasks {
		if f.omitSeq0FromTaskList && t.Seq == 0 {
			continue
		}
		items = append(items, t)
	}
	f.mu.Unlock()
	writeFakeJSON(w, http.StatusOK, map[string]any{"items": items, "has_more": false})
}

func (f *fakePlane) handleReport(w http.ResponseWriter) {
	f.mu.Lock()
	f.reportCalls++
	call := f.reportCalls
	f.mu.Unlock()

	if call <= f.report404 {
		w.Header().Set("Retry-After", "0")
		writeFakeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "NOT_FOUND", "message": "report 还在生成"},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.reportBody)
}

func (f *fakePlane) handleSSE(w http.ResponseWriter, r *http.Request) {
	if !f.sseEnabled {
		writeFakeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "NOT_FOUND", "message": "SSE 未实现"},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	for _, ev := range f.events {
		payload, _ := json.Marshal(ev)
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, ": ping\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	select {
	case <-r.Context().Done():
	case <-time.After(3 * time.Second):
	}
}

func (f *fakePlane) handleTask(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/tasks/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "NOT_FOUND"}})
		return
	}
	taskID := parts[0]

	switch {
	case len(parts) == 2 && parts[1] == "attempts":
		list := f.attempts[taskID]
		if list == nil {
			list = []Attempt{}
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"items": list})

	case len(parts) == 4 && parts[1] == "attempts" && parts[3] == "logs":
		key := taskID + ":" + parts[2] + ":" + r.URL.Query().Get("stream")
		body, ok := f.logs[key]
		if !ok {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": "没有该日志"}})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)

	case len(parts) == 4 && parts[1] == "attempts" && parts[3] == "result":
		key := taskID + ":" + parts[2]
		body, ok := f.results[key]
		if !ok {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": "没有 result.json"}})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)

	default:
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "NOT_FOUND"}})
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- 场景数据 ------------------------------------------------------------

func ev(seq int64, typ string, payload map[string]any) Event {
	return Event{Seq: seq, Type: typ, RunID: "r-test-1", Payload: payload}
}

func runEv(seq int64, from, to string) Event {
	return ev(seq, "run.state_changed", map[string]any{"from": from, "to": to})
}

func taskEv(seq int64, taskID, from, to string) Event {
	e := ev(seq, "task.state_changed", map[string]any{"from": from, "to": to})
	e.TaskID = taskID
	return e
}

func attemptEv(seq int64, typ, taskID, attemptID string) Event {
	e := ev(seq, typ, map[string]any{"attempt_id": attemptID, "task_id": taskID})
	e.TaskID = taskID
	return e
}

func happyEvents() []Event {
	return []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		taskEv(3, "t-1", "pending", "queued"),
		taskEv(4, "t-1", "queued", "preparing"),
		attemptEv(5, "attempt.started", "t-1", "a-1"),
		taskEv(6, "t-1", "preparing", "running"),
		attemptEv(7, "attempt.finished", "t-1", "a-1"),
		taskEv(8, "t-1", "running", "succeeded"),
		taskEv(9, "t-2", "pending", "queued"),
		runEv(10, "queued", "testing"),
		taskEv(11, "t-2", "queued", "preparing"),
		taskEv(12, "t-2", "preparing", "running"),
		attemptEv(13, "attempt.started", "t-2", "a-2"),
		attemptEv(14, "attempt.finished", "t-2", "a-2"),
		taskEv(15, "t-2", "running", "succeeded"),
		runEv(16, "testing", "cleanup"),
		runEv(17, "cleanup", "succeeded"),
		ev(18, "run.finished", map[string]any{"status": "succeeded"}),
	}
}

func newHappyPlane() *fakePlane {
	return &fakePlane{
		prefix:   "/api/v1",
		runID:    "r-test-1",
		statuses: []string{"queued", "testing", "succeeded"},
		run: Run{
			ID: "r-test-1", Name: "e2e-test", Kind: "run", Status: "succeeded",
			SubmittedAt: "2026-11-05T02:00:00Z", FinishedAt: "2026-11-05T02:01:00Z",
			Tasks: []Task{
				{ID: "t-1", Name: "precheck", Seq: 0, Status: "succeeded", Attempts: 1},
				{ID: "t-2", Name: "probe-a", Seq: 1, Status: "succeeded", Attempts: 1},
			},
		},
		reportBody: []byte(`{"run_id":"r-test-1","status":"succeeded","counts":{"tasks":2,"succeeded":2}}`),
		report404:  1, // 顺带验证 404 + Retry-After 的重试路径
		events:     happyEvents(),
		attempts: map[string][]Attempt{
			"t-1": {{ID: "a-1", Idx: 1, TaskID: "t-1", Status: "succeeded"}},
			"t-2": {{ID: "a-2", Idx: 1, TaskID: "t-2", Status: "succeeded"}},
		},
		logs: map[string]string{
			"t-1:1:stdout": "precheck: probing http://mock-llm:18080/v1\nprecheck: ready\n",
			"t-2:1:stdout": "probe-a: ok\n",
		},
		results: map[string]string{
			"t-1:1": `{"schema_version":"1.0","status":"succeeded","metrics":[]}`,
			"t-2:1": `{"schema_version":"1.0","status":"succeeded","metrics":[]}`,
		},
		sseEnabled: true,
	}
}

func newUnavailablePlane() *fakePlane {
	f := newHappyPlane()
	f.run.Status = "service_unavailable"
	f.run.Error = &Failure{Code: "PRECHECK_FAILED", Message: "窗口内未就绪", Retryable: true}
	f.run.Tasks = []Task{
		{ID: "t-1", Name: "precheck", Status: "skipped", StatusReason: "service_unavailable"},
		{ID: "t-2", Name: "probe-a", Status: "skipped", StatusReason: "service_unavailable"},
	}
	f.statuses = []string{"deploying", "service_unavailable"}
	f.reportBody = []byte(`{"run_id":"r-test-1","status":"service_unavailable"}`)
	f.attempts = map[string][]Attempt{}
	f.logs = map[string]string{}
	f.results = map[string]string{}
	f.events = []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		runEv(3, "queued", "deploying"),
		ev(4, "deploy.state_changed", map[string]any{"state": "blocked"}),
		taskEv(5, "t-1", "pending", "skipped"),
		taskEv(6, "t-2", "pending", "skipped"),
		runEv(7, "deploying", "service_unavailable"),
		ev(8, "run.finished", map[string]any{"status": "service_unavailable"}),
	}
	return f
}

func newPrecheck401Plane() *fakePlane {
	f := newHappyPlane()
	f.run.Status = "completed_with_errors"
	f.run.Tasks = []Task{
		{
			ID: "t-1", Name: "precheck", Status: "failed", Attempts: 1,
			Error: &Failure{
				Code: "PRECHECK_FAILED", Message: "探测返回 401",
				Detail:    map[string]any{"http_status": 401, "attempt": 1},
				Retryable: false,
			},
		},
		{ID: "t-2", Name: "probe-a", Status: "skipped", StatusReason: "upstream_abort"},
	}
	f.statuses = []string{"testing", "completed_with_errors"}
	f.reportBody = []byte(`{"run_id":"r-test-1","status":"completed_with_errors","failures":[{"code":"PRECHECK_FAILED","retryable":false}]}`)
	f.attempts = map[string][]Attempt{
		"t-1": {{ID: "a-1", Idx: 1, TaskID: "t-1", Status: "failed"}},
	}
	f.logs = map[string]string{
		"t-1:1:stdout": "precheck: HTTP 401：认证失败（检查 ENDPOINT_API_KEY）\n",
	}
	f.results = map[string]string{
		"t-1:1": `{"schema_version":"1.0","status":"failed","error":{"code":"PRECHECK_FAILED","retryable":false}}`,
	}
	f.events = []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		runEv(3, "queued", "testing"),
		taskEv(4, "t-1", "pending", "queued"),
		taskEv(5, "t-1", "queued", "preparing"),
		attemptEv(6, "attempt.started", "t-1", "a-1"),
		taskEv(7, "t-1", "preparing", "running"),
		attemptEv(8, "attempt.finished", "t-1", "a-1"),
		taskEv(9, "t-1", "running", "failed"),
		taskEv(10, "t-2", "pending", "skipped"),
		runEv(11, "testing", "cleanup"),
		runEv(12, "cleanup", "completed_with_errors"),
		ev(13, "run.finished", map[string]any{"status": "completed_with_errors"}),
	}
	return f
}

// ---- 测试脚手架 ----------------------------------------------------------

func startFake(t *testing.T, f *fakePlane) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return ts
}

// runCommand 直接走 parseArgs + execute + finish，捕获 stdout 的 JSON 与 stderr 摘要。
func runCommand(t *testing.T, argv []string) (int, resultJSON, string) {
	t.Helper()
	opts, command, err := parseArgs(argv)
	if err != nil {
		t.Fatalf("parseArgs 失败：%v；argv=%v", err, argv)
	}
	var out, errBuf bytes.Buffer
	opts.Out = &out
	opts.Err = &errBuf

	app := newApp(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runErr := app.execute(ctx, command)
	code := app.finish(command, time.Now(), runErr)

	var res resultJSON
	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatalf("stdout 没有机器可读结果；stderr：\n%s", errBuf.String())
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("stdout 不是一行 JSON：%v\n%s", err, line)
	}
	return code, res, errBuf.String()
}

func baseArgs(url string, extra ...string) []string {
	args := []string{
		"run",
		"--base-url", url,
		"--mode", "service",
		"--poll", "5ms",
		"--timeout", "5s",
		"--sse-timeout", "1s",
	}
	return append(args, extra...)
}

func findCheck(res resultJSON, name string) *check {
	for i := range res.Checks {
		if res.Checks[i].Name == name {
			return &res.Checks[i]
		}
	}
	return nil
}

func mustCheck(t *testing.T, res resultJSON, name string, wantOK bool) *check {
	t.Helper()
	c := findCheck(res, name)
	if c == nil {
		t.Fatalf("结果里没有检查项 %q；已有：%v", name, checkNames(res))
	}
	if c.OK != wantOK {
		t.Fatalf("检查项 %q 期望 ok=%v，实际 ok=%v detail=%q", name, wantOK, c.OK, c.Detail)
	}
	return c
}

func checkNames(res resultJSON) []string {
	names := make([]string, 0, len(res.Checks))
	for _, c := range res.Checks {
		names = append(names, c.Name)
	}
	return names
}

// ---- 用例 ----------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	f := newHappyPlane()
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL))

	if code != 0 {
		t.Fatalf("期望退出码 0，实际 %d；stderr：\n%s", code, stderr)
	}
	if !res.OK {
		t.Fatalf("期望 ok=true，checks 失败项：%v", failedCheckDetails(res))
	}
	if res.RunID != "r-test-1" || res.Actual != "succeeded" {
		t.Fatalf("run_id/status 不符：%+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("happy path 不应有警告：%v", res.Warnings)
	}
	for _, name := range []string{
		"submit.http_status", "submit.run_id",
		"run.expected_status", "run.tasks_present", "run.all_tasks_terminal",
		"report.present", "report.json_valid",
		"events.history", "events.seq_monotonic", "events.transitions_legal",
		"events.final_state", "sse.connect",
		"task.precheck.attempt.1.logs", "task.precheck.attempt.1.result",
		"task.probe-a.attempt.1.logs", "task.probe-a.attempt.1.result",
	} {
		mustCheck(t, res, name, true)
	}
	// 状态轨迹至少有 queued/terminal 两层
	if len(res.StatusTrace) < 2 {
		t.Fatalf("状态轨迹太短：%v", res.StatusTrace)
	}
	// dev 鉴权：默认 Bearer dev
	if f.lastAuth != "Bearer dev" {
		t.Fatalf("默认 Authorization 期望 Bearer dev，实际 %q", f.lastAuth)
	}
	if f.submitCalls != 1 {
		t.Fatalf("期望只提交 1 次，实际 %d 次", f.submitCalls)
	}
	if !strings.Contains(f.lastBody, "run:") {
		t.Fatalf("提交的 Manifest 不含 run:: %q", f.lastBody)
	}
	// report 第一次 404 后重试成功
	if f.reportCalls < 2 {
		t.Fatalf("报告 404 重试路径未生效（reportCalls=%d）", f.reportCalls)
	}
}

func TestRunStrictHappyPath(t *testing.T) {
	ts := startFake(t, newHappyPlane())
	code, res, _ := runCommand(t, baseArgs(ts.URL, "--strict"))
	if code != 0 || !res.OK {
		t.Fatalf("--strict 下 happy path 应通过，code=%d warnings=%v", code, res.Warnings)
	}
}

func TestIllegalEventTransition(t *testing.T) {
	f := newHappyPlane()
	// queued -> succeeded 不在 Task 转移表内
	f.events[3] = taskEv(4, "t-1", "queued", "succeeded")
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("非法转移期望退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "events.transitions_legal", false)
	if !strings.Contains(c.Detail, "queued -> succeeded") {
		t.Fatalf("失败详情没指出非法边：%q", c.Detail)
	}
}

func TestEventSeqNotMonotonic(t *testing.T) {
	f := newHappyPlane()
	f.events[2].Seq = 2 // 与上一条 seq=2 重复
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("seq 非单调期望退出码 1，实际 %d", code)
	}
	mustCheck(t, res, "events.seq_monotonic", false)
}

func TestExpectMismatch(t *testing.T) {
	ts := startFake(t, newHappyPlane())
	code, res, _ := runCommand(t, baseArgs(ts.URL, "--expect", "failed"))
	if code != 1 {
		t.Fatalf("期望不符应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "run.expected_status", false)
	if !strings.Contains(c.Detail, "succeeded") {
		t.Fatalf("期望详情缺实际值：%q", c.Detail)
	}
}

func TestServiceUnavailableRetryable(t *testing.T) {
	ts := startFake(t, newUnavailablePlane())
	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "service_unavailable",
		"--expect-error-code", "PRECHECK_FAILED",
		"--expect-retryable=true",
	))
	if code != 0 || !res.OK {
		t.Fatalf("service_unavailable 场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "run.unavailable_tasks_skipped", true)
	mustCheck(t, res, "expect.run_error_code", true)
	mustCheck(t, res, "expect.retryable", true)
	if res.Actual != "service_unavailable" {
		t.Fatalf("实际终态 %q", res.Actual)
	}
}

func TestServiceUnavailableRetryableMismatch(t *testing.T) {
	ts := startFake(t, newUnavailablePlane())
	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "service_unavailable",
		"--expect-retryable=false",
	))
	if code != 1 {
		t.Fatalf("retryable 期望不符应退出码 1，实际 %d", code)
	}
	mustCheck(t, res, "expect.retryable", false)
}

func TestPrecheck401NonRetryable(t *testing.T) {
	ts := startFake(t, newPrecheck401Plane())
	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "completed_with_errors",
		"--expect-task-error-code", "PRECHECK_FAILED",
		"--expect-retryable=false",
	))
	if code != 0 || !res.OK {
		t.Fatalf("401 场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "precheck.failed_error", true)
	mustCheck(t, res, "expect.task_error_code", true)
	mustCheck(t, res, "task.precheck.attempt.1.logs", true)
	mustCheck(t, res, "task.precheck.attempt.1.result", true)
}

func TestPrecheck401RetryableMismatch(t *testing.T) {
	ts := startFake(t, newPrecheck401Plane())
	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "completed_with_errors",
		"--expect-task-error-code", "PRECHECK_FAILED",
		"--expect-retryable=true",
	))
	if code != 1 {
		t.Fatalf("401 的 retryable=false，写 true 应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "expect.retryable", false)
	if !strings.Contains(c.Detail, "false") {
		t.Fatalf("retryable 详情没写实际值：%q", c.Detail)
	}
}

func TestEmptyLogsFailUnlessAllowed(t *testing.T) {
	f := newHappyPlane()
	f.logs["t-1:1:stdout"] = ""
	f.logs["t-1:1:stderr"] = ""
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("日志为空应失败，实际退出码 %d", code)
	}
	mustCheck(t, res, "task.precheck.attempt.1.logs", false)

	// 同一 fake 上再跑一次，允许空日志
	code, res, _ = runCommand(t, baseArgs(ts.URL, "--allow-empty-logs", "--expect", "succeeded"))
	if code != 0 {
		t.Fatalf("--allow-empty-logs 后应通过，实际 %d checks=%v", code, failedCheckDetails(res))
	}
	if len(res.Warnings) == 0 {
		t.Fatal("空日志应至少产生一条警告")
	}
}

func TestSSESmokeRequired(t *testing.T) {
	f := newHappyPlane()
	f.sseEnabled = false
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("SSE 不可用应失败，实际退出码 %d", code)
	}
	mustCheck(t, res, "sse.connect", false)
	// 事件历史仍然可用
	mustCheck(t, res, "events.history", true)
}

func TestConnectErrorIsClean(t *testing.T) {
	code, res, stderr := runCommand(t, []string{
		"run",
		"--base-url", "http://127.0.0.1:1",
		"--timeout", "2s",
		"--poll", "10ms",
		"--sse-timeout", "1s",
	})
	if code != 3 {
		t.Fatalf("连不上控制面期望退出码 3，实际 %d", code)
	}
	if res.OK {
		t.Fatal("连不上时 ok 必须为 false")
	}
	if !strings.Contains(res.Error, "无法连接控制面") {
		t.Fatalf("JSON error 没有清晰的连接失败说明：%q", res.Error)
	}
	if !strings.Contains(stderr, "make dev") {
		t.Fatalf("stderr 没提示怎么起控制面：\n%s", stderr)
	}
}

func TestSubmitRejected(t *testing.T) {
	f := newHappyPlane()
	f.submitStatus = http.StatusBadRequest
	f.submitBody = []byte(`{"error":{"code":"VALIDATION_ERROR","message":"tests[0].args 非法","detail":{"issues":[{"path":"tests[0].args","code":"ARGS_LEX_ERROR","message":"未闭合引号"}]},"retryable":false}}`)
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, []string{
		"submit", "--base-url", ts.URL,
	})
	if code != 1 {
		t.Fatalf("提交被拒期望退出码 1，实际 %d；stderr：\n%s", code, stderr)
	}
	if !strings.Contains(res.Error, "VALIDATION_ERROR") {
		t.Fatalf("错误信息缺错误码：%q", res.Error)
	}
	if res.RunID != "" {
		t.Fatalf("提交失败不应有 run_id，实际 %q", res.RunID)
	}
}

func TestAuthTokenAndNoAuth(t *testing.T) {
	f := newHappyPlane()
	ts := startFake(t, f)

	// --token 优先
	code, _, _ := runCommand(t, []string{"submit", "--base-url", ts.URL, "--token", "svc-token"})
	if code != 0 {
		t.Fatalf("submit 失败（token 模式），code=%d", code)
	}
	if f.lastAuth != "Bearer svc-token" {
		t.Fatalf("--token 期望 Bearer svc-token，实际 %q", f.lastAuth)
	}

	// --user 生效
	if _, _, _ = runCommand(t, []string{"submit", "--base-url", ts.URL, "--user", "zhangsan"}); f.lastAuth != "Bearer zhangsan" {
		t.Fatalf("--user 期望 Bearer zhangsan，实际 %q", f.lastAuth)
	}

	// --no-auth 不发头
	if _, _, _ = runCommand(t, []string{"submit", "--base-url", ts.URL, "--no-auth"}); f.lastAuth != "" {
		t.Fatalf("--no-auth 期望没有 Authorization，实际 %q", f.lastAuth)
	}
}

func TestSubmitWaitAssertPhases(t *testing.T) {
	ts := startFake(t, newHappyPlane())

	code, sub, _ := runCommand(t, []string{"submit", "--base-url", ts.URL})
	if code != 0 {
		t.Fatalf("submit 阶段失败，code=%d", code)
	}
	if sub.RunID != "r-test-1" {
		t.Fatalf("submit 的 run_id=%q", sub.RunID)
	}

	code, waitRes, _ := runCommand(t, []string{
		"wait", "--run-id", "r-test-1", "--base-url", ts.URL,
		"--poll", "5ms", "--timeout", "5s",
	})
	if code != 0 {
		t.Fatalf("wait 阶段失败，code=%d", code)
	}
	if waitRes.Actual != "succeeded" {
		t.Fatalf("wait 终态=%q", waitRes.Actual)
	}

	code, assertRes, _ := runCommand(t, []string{
		"assert", "--run-id", "r-test-1", "--base-url", ts.URL,
		"--sse-timeout", "1s",
	})
	if code != 0 || !assertRes.OK {
		t.Fatalf("assert 阶段失败，code=%d checks=%v", code, failedCheckDetails(assertRes))
	}
}

func TestAssertRequiresTerminal(t *testing.T) {
	f := newHappyPlane()
	f.statuses = []string{"queued"} // 永远不到终态
	ts := startFake(t, f)

	code, res, _ := runCommand(t, []string{
		"assert", "--run-id", "r-test-1", "--base-url", ts.URL,
		"--sse-timeout", "1s",
	})
	if code != 1 {
		t.Fatalf("非终态 assert 期望退出码 1，实际 %d", code)
	}
	if !strings.Contains(res.Error, "还没到终态") {
		t.Fatalf("错误信息应说明还没到终态：%q", res.Error)
	}
}

func TestCustomPrefix(t *testing.T) {
	f := newHappyPlane()
	f.prefix = "/custom-api/v1"
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL, "--api-prefix", "/custom-api/v1"))
	if code != 0 || !res.OK {
		t.Fatalf("自定义前缀应通过，code=%d stderr：\n%s", code, stderr)
	}
}

func TestAutoPrefixRejected(t *testing.T) {
	_, _, err := parseArgs([]string{"run", "--api-prefix", "auto"})
	if err == nil {
		t.Fatal("--api-prefix auto 应被拒绝（MVP 定稿 /api/v1）")
	}
	if !strings.Contains(err.Error(), "auto") {
		t.Fatalf("错误信息没提到 auto：%v", err)
	}
}

func failedCheckDetails(res resultJSON) []string {
	out := make([]string, 0, len(res.Checks))
	for _, c := range res.Checks {
		if !c.OK {
			out = append(out, c.Name+"："+c.Detail)
		}
	}
	return out
}
