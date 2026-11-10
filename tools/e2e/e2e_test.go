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

// fakePlane 是一个只覆盖 e2e 用到端点的内存实现（v2：Task 一等对象，无 /runs）：
// 它同时充当"契约的活样例"，让 e2e 在没有真后端时也能被验证（红/绿两条路都测）。
type fakePlane struct {
	mu sync.Mutex

	prefix string
	taskID string

	statuses  []string
	statusIdx int
	task      Task

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
	logs       map[string]string // key: taskID:attemptNo:seq:stream
	results    map[string]string // key: taskID:attemptNo:seq
	sseEnabled bool

	omitSeq1FromRunnerList bool
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
	case r.Method == http.MethodPost && path == "/tasks":
		f.handleSubmit(w, r)
	case r.Method == http.MethodGet && path == "/tasks/"+f.taskID:
		f.handleTask(w)
	case path == "/tasks/"+f.taskID+"/runners":
		f.handleRunnerList(w)
	case path == "/tasks/"+f.taskID+"/report":
		f.handleReport(w)
	case path == "/tasks/"+f.taskID+"/report.md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, "# "+f.taskID+"\n\nok\n")
	case path == "/tasks/"+f.taskID+"/events/history":
		writeFakeJSON(w, http.StatusOK, map[string]any{"events": f.events})
	case path == "/tasks/"+f.taskID+"/events":
		f.handleSSE(w, r)
	case strings.HasPrefix(path, "/tasks/"):
		f.handleSub(w, r, path)
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
		"task": map[string]any{
			"id": f.taskID, "status": "queued", "submitted_at": "2026-11-10T02:00:00Z",
			"current_attempt": 0,
		},
		"runners": []any{
			map[string]any{"seq": 1, "name": "precheck", "runner": "endpoint-probe", "role": "pre_check", "status": "pending"},
			map[string]any{"seq": 2, "name": "probe-a", "runner": "aisbench-sla", "role": "test", "status": "pending"},
		},
	})
}

func (f *fakePlane) handleTask(w http.ResponseWriter) {
	f.mu.Lock()
	idx := f.statusIdx
	if idx < len(f.statuses)-1 {
		f.statusIdx++
	}
	status := f.task.Status
	if len(f.statuses) > 0 {
		if idx >= len(f.statuses) {
			idx = len(f.statuses) - 1
		}
		status = f.statuses[idx]
	}
	task := f.task
	task.Status = status
	f.mu.Unlock()
	writeFakeJSON(w, http.StatusOK, task)
}

func (f *fakePlane) handleRunnerList(w http.ResponseWriter) {
	f.mu.Lock()
	items := make([]RunnerExecution, 0, len(f.task.Runners))
	for _, r := range f.task.Runners {
		if f.omitSeq1FromRunnerList && r.Seq == 1 {
			continue
		}
		items = append(items, r)
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

// handleSub 处理 /tasks/{id}/attempts[/{n}/runners[/{seq}/...]] 与 attempt 级列表。
func (f *fakePlane) handleSub(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/tasks/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[1] != "attempts" {
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "NOT_FOUND"}})
		return
	}
	taskID := parts[0]

	switch {
	// /tasks/{id}/attempts
	case len(parts) == 2:
		list := f.attempts[taskID]
		if list == nil {
			list = []Attempt{}
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"items": list})

	// /tasks/{id}/attempts/{n}/runners
	case len(parts) == 4 && parts[2] == "runners":
		attemptNo := parts[1]
		items := make([]RunnerExecution, 0, len(f.task.Runners))
		for _, rr := range f.task.Runners {
			if f.omitSeq1FromRunnerList && rr.Seq == 1 {
				continue
			}
			items = append(items, rr)
		}
		_ = attemptNo
		writeFakeJSON(w, http.StatusOK, map[string]any{"items": items})

	// /tasks/{id}/attempts/{n}/runners/{seq}/logs
	case len(parts) == 6 && parts[5] == "logs":
		key := taskID + ":" + parts[2] + ":" + parts[4] + ":" + r.URL.Query().Get("stream")
		f.mu.Lock()
		body, ok := f.logs[key]
		f.mu.Unlock()
		if !ok {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": "没有该日志"}})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)

	// /tasks/{id}/attempts/{n}/runners/{seq}/result
	case len(parts) == 6 && parts[5] == "result":
		key := taskID + ":" + parts[2] + ":" + parts[4]
		f.mu.Lock()
		body, ok := f.results[key]
		f.mu.Unlock()
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
	return Event{Seq: seq, Type: typ, TaskID: "t-test-1", Payload: payload}
}

func taskEv(seq int64, from, to string) Event {
	return ev(seq, "state_changed", map[string]any{"from": from, "to": to})
}

func runnerEv(seq int64, runnerSeq int, from, to string) Event {
	e := ev(seq, "state_changed", map[string]any{"from": from, "to": to})
	n := runnerSeq
	e.RunnerSeq = &n
	attempt := 1
	e.AttemptNo = &attempt
	return e
}

func runnerDone(seq int64, runnerSeq int, status string) Event {
	e := ev(seq, "runner_finished", map[string]any{"status": status, "exit_code": 0})
	n := runnerSeq
	e.RunnerSeq = &n
	attempt := 1
	e.AttemptNo = &attempt
	return e
}

func runnerStart(seq int64, runnerSeq int) Event {
	e := ev(seq, "runner_started", map[string]any{"status": "running"})
	n := runnerSeq
	e.RunnerSeq = &n
	attempt := 1
	e.AttemptNo = &attempt
	return e
}

func happyEvents() []Event {
	return []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		runnerEv(3, 1, "pending", "queued"),
		runnerEv(4, 1, "queued", "preparing"),
		runnerStart(5, 1),
		runnerEv(6, 1, "preparing", "running"),
		runnerEv(7, 1, "running", "succeeded"),
		runnerDone(8, 1, "succeeded"),
		runnerEv(9, 2, "pending", "queued"),
		runnerEv(10, 2, "queued", "preparing"),
		runnerStart(11, 2),
		runnerEv(12, 2, "preparing", "running"),
		runnerEv(13, 2, "running", "succeeded"),
		runnerDone(14, 2, "succeeded"),
		taskEv(15, "running", "succeeded"),
		ev(16, "task_finished", map[string]any{"status": "succeeded"}),
	}
}

func happyRunners() []RunnerExecution {
	return []RunnerExecution{
		{Seq: 1, Name: "precheck", RunnerName: "endpoint-probe", Role: "pre_check", Status: "succeeded"},
		{Seq: 2, Name: "probe-a", RunnerName: "aisbench-sla", Role: "test", Status: "succeeded"},
	}
}

func newHappyPlane() *fakePlane {
	return &fakePlane{
		prefix:   "/api/v1",
		taskID:   "t-test-1",
		statuses: []string{"queued", "running", "succeeded"},
		task: Task{
			ID: "t-test-1", Name: "e2e-test", Status: "succeeded",
			SubmittedAt: "2026-11-10T02:00:00Z", FinishedAt: "2026-11-10T02:01:00Z",
			CurrentAttempt: 1,
			RunnerCounts:   &RunnerCounts{Total: 2, Succeeded: 2},
			Attempts: []Attempt{
				{TaskID: "t-test-1", AttemptNo: 1, Status: "succeeded", Trigger: "initial"},
			},
			Runners: happyRunners(),
		},
		reportBody: []byte(`{"task_id":"t-test-1","status":"succeeded","counts":{"runners":2,"succeeded":2}}`),
		report404:  1, // 顺带验证 404 + Retry-After 的重试路径
		events:     happyEvents(),
		attempts: map[string][]Attempt{
			"t-test-1": {{TaskID: "t-test-1", AttemptNo: 1, Status: "succeeded", Trigger: "initial"}},
		},
		logs: map[string]string{
			"t-test-1:1:1:stdout": "precheck: probing http://mock-llm:18080/v1\nprecheck: ready\n",
			"t-test-1:1:2:stdout": "probe-a: ok\n",
		},
		results: map[string]string{
			"t-test-1:1:1": `{"schema_version":"2.0","task_id":"t-test-1","attempt_no":1,"runner_seq":1,"runner":"endpoint-probe","status":"succeeded","metrics":[]}`,
			"t-test-1:1:2": `{"schema_version":"2.0","task_id":"t-test-1","attempt_no":1,"runner_seq":2,"runner":"aisbench-sla","status":"succeeded","metrics":[]}`,
		},
		sseEnabled: true,
	}
}

func newUnavailablePlane() *fakePlane {
	f := newHappyPlane()
	f.task.Status = "service_unavailable"
	f.task.Error = &Failure{Code: "PRECHECK_FAILED", Message: "窗口内未就绪", Retryable: true}
	f.task.Runners = []RunnerExecution{
		{Seq: 1, Name: "precheck", RunnerName: "endpoint-probe", Role: "pre_check", Status: "failed",
			Error: &Failure{Code: "PRECHECK_FAILED", Message: "窗口内未就绪", Retryable: true}},
		{Seq: 2, Name: "probe-a", RunnerName: "aisbench-sla", Role: "test", Status: "skipped", StatusReason: "upstream_abort"},
	}
	f.statuses = []string{"queued", "service_unavailable"}
	f.reportBody = []byte(`{"task_id":"t-test-1","status":"service_unavailable"}`)
	f.logs = map[string]string{
		"t-test-1:1:1:stdout": "precheck: 第 3 次探测：未就绪 —— HTTP 500\n",
	}
	f.results = map[string]string{
		"t-test-1:1:1": `{"schema_version":"2.0","task_id":"t-test-1","attempt_no":1,"runner_seq":1,"runner":"endpoint-probe","status":"failed","error":{"code":"PRECHECK_FAILED","retryable":true}}`,
	}
	f.events = []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		runnerEv(3, 1, "pending", "queued"),
		runnerEv(4, 1, "queued", "preparing"),
		runnerStart(5, 1),
		runnerEv(6, 1, "preparing", "running"),
		runnerEv(7, 1, "running", "failed"),
		runnerDone(8, 1, "failed"),
		runnerEv(9, 2, "pending", "skipped"),
		taskEv(10, "running", "service_unavailable"),
		ev(11, "task_finished", map[string]any{"status": "service_unavailable"}),
	}
	return f
}

func newPrecheck401Plane() *fakePlane {
	f := newHappyPlane()
	f.task.Status = "failed"
	f.task.Runners = []RunnerExecution{
		{
			Seq: 1, Name: "precheck", RunnerName: "endpoint-probe", Role: "pre_check", Status: "failed",
			Error: &Failure{
				Code:      "PRECHECK_FAILED",
				Message:   "探测返回 401",
				Detail:    map[string]any{"http_status": 401, "attempt_no": 1},
				Retryable: false,
			},
		},
		{Seq: 2, Name: "probe-a", RunnerName: "aisbench-sla", Role: "test", Status: "skipped", StatusReason: "upstream_abort"},
	}
	f.statuses = []string{"queued", "failed"}
	f.reportBody = []byte(`{"task_id":"t-test-1","status":"failed","failures":[{"code":"PRECHECK_FAILED","retryable":false}]}`)
	f.logs = map[string]string{
		"t-test-1:1:1:stdout": "precheck: HTTP 401：认证失败（检查 ENDPOINT_API_KEY）\n",
	}
	f.results = map[string]string{
		"t-test-1:1:1": `{"schema_version":"2.0","task_id":"t-test-1","attempt_no":1,"runner_seq":1,"runner":"endpoint-probe","status":"failed","error":{"code":"PRECHECK_FAILED","retryable":false}}`,
	}
	f.events = []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		runnerEv(3, 1, "pending", "queued"),
		runnerEv(4, 1, "queued", "preparing"),
		runnerStart(5, 1),
		runnerEv(6, 1, "preparing", "running"),
		runnerEv(7, 1, "running", "failed"),
		runnerDone(8, 1, "failed"),
		runnerEv(9, 2, "pending", "skipped"),
		taskEv(10, "running", "failed"),
		ev(11, "task_finished", map[string]any{"status": "failed"}),
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
	if res.TaskID != "t-test-1" || res.Actual != "succeeded" {
		t.Fatalf("task_id/status 不符：%+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("happy path 不应有警告：%v", res.Warnings)
	}
	for _, name := range []string{
		"submit.http_status", "submit.task_id",
		"task.expected_status", "task.runners_present", "task.all_runners_terminal",
		"task.runners_valid", "task.runners_list_complete", "task.attempts_continuous",
		"report.present", "report.json_valid",
		"events.history", "events.seq_monotonic", "events.transitions_legal",
		"events.final_state", "sse.connect",
		"runner.precheck.logs", "runner.precheck.result",
		"runner.probe-a.logs", "runner.probe-a.result",
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
	if !strings.Contains(f.lastBody, "task:") {
		t.Fatalf("提交的 Manifest 不含嵌套 task:（v2 D-40）：%q", f.lastBody)
	}
	if strings.Contains(f.lastBody, "args:") || strings.Contains(f.lastBody, "tests:") {
		t.Fatalf("v2 Manifest 不应再含 args/tests：%q", f.lastBody)
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

func TestIllegalTaskTransition(t *testing.T) {
	f := newHappyPlane()
	// queued -> succeeded 不在 Task 转移表内
	f.events[2] = taskEv(3, "queued", "succeeded")
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

func TestIllegalRunnerTransition(t *testing.T) {
	f := newHappyPlane()
	// succeeded -> queued 不在 runner 转移表内（v2 没有 runner 级重试回路，D-69）
	f.events = append(f.events, runnerEv(17, 2, "succeeded", "queued"))
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("非法 runner 转移期望退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "events.transitions_legal", false)
	if !strings.Contains(c.Detail, "succeeded -> queued") {
		t.Fatalf("失败详情没指出非法 runner 边：%q", c.Detail)
	}
}

func TestRetryEdgeIsLegal(t *testing.T) {
	f := newHappyPlane()
	// 终态 failed → queued 是合法的 retry 边（§7.8；第一波已落码）
	f.events = append(f.events, taskEv(17, "failed", "queued"))
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 0 || !res.OK {
		t.Fatalf("retry 边应判合法，code=%d checks=%v", code, failedCheckDetails(res))
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
	c := mustCheck(t, res, "task.expected_status", false)
	if !strings.Contains(c.Detail, "succeeded") {
		t.Fatalf("期望详情缺实际值：%q", c.Detail)
	}
}

func TestExpectValueDomain(t *testing.T) {
	// v2 没有 completed_with_errors（并入 failed，D-73）——传它必须被拒
	if _, _, err := parseArgs([]string{"run", "--expect", "completed_with_errors"}); err == nil {
		t.Fatal("--expect completed_with_errors 应被拒绝（v2 值域不含它）")
	}
	for _, v := range []string{"succeeded", "failed", "service_unavailable", "canceled"} {
		if _, _, err := parseArgs([]string{"run", "--expect", v}); err != nil {
			t.Fatalf("--expect %s 应合法：%v", v, err)
		}
	}
}

func TestServiceUnavailableRetryable(t *testing.T) {
	ts := startFake(t, newUnavailablePlane())
	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "service_unavailable",
		"--expect-error-code", "PRECHECK_FAILED",
		"--expect-runner-error-code", "PRECHECK_FAILED",
		"--expect-retryable=true",
	))
	if code != 0 || !res.OK {
		t.Fatalf("service_unavailable 场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "task.unavailable_runners_skipped", true)
	mustCheck(t, res, "expect.task_error_code", true)
	mustCheck(t, res, "expect.runner_error_code", true)
	mustCheck(t, res, "expect.retryable", true)
	mustCheck(t, res, "task.precheck_retryable_consistent", true)
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
		"--expect", "failed",
		"--expect-runner-error-code", "PRECHECK_FAILED",
		"--expect-retryable=false",
	))
	if code != 0 || !res.OK {
		t.Fatalf("401 场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "precheck.failed_error", true)
	mustCheck(t, res, "task.precheck_retryable_consistent", true)
	mustCheck(t, res, "expect.runner_error_code", true)
	mustCheck(t, res, "runner.precheck.logs", true)
	mustCheck(t, res, "runner.precheck.result", true)
}

func TestPrecheck401RetryableMismatch(t *testing.T) {
	ts := startFake(t, newPrecheck401Plane())
	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "failed",
		"--expect-runner-error-code", "PRECHECK_FAILED",
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
	f.logs["t-test-1:1:1:stdout"] = ""
	f.logs["t-test-1:1:1:stderr"] = ""
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("日志为空应失败，实际退出码 %d", code)
	}
	mustCheck(t, res, "runner.precheck.logs", false)

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
	f.submitBody = []byte(`{"code":"VALIDATION_ERROR","message":"runners[0].input 非法","detail":{"issues":[{"path":"runners[0].input","code":"INPUT_TYPE_MISMATCH","line":2,"message":"第 2 行字段 limit 期望 integer，实际 string"}]},"retryable":false,"trace_id":"tx"}`)
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
	if res.TaskID != "" {
		t.Fatalf("提交失败不应有 task_id，实际 %q", res.TaskID)
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
	if sub.TaskID != "t-test-1" {
		t.Fatalf("submit 的 task_id=%q", sub.TaskID)
	}

	code, waitRes, _ := runCommand(t, []string{
		"wait", "--task-id", "t-test-1", "--base-url", ts.URL,
		"--poll", "5ms", "--timeout", "5s",
	})
	if code != 0 {
		t.Fatalf("wait 阶段失败，code=%d", code)
	}
	if waitRes.Actual != "succeeded" {
		t.Fatalf("wait 终态=%q", waitRes.Actual)
	}

	code, assertRes, _ := runCommand(t, []string{
		"assert", "--task-id", "t-test-1", "--base-url", ts.URL,
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
		"assert", "--task-id", "t-test-1", "--base-url", ts.URL,
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
		t.Fatal("--api-prefix auto 应被拒绝（契约定稿 /api/v1）")
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
