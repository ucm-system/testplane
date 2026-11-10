package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件用 httptest 假控制面 + io.Pipe 驱动 stdio，断言 MCP 的协议面：
// initialize / tools/list / tools/call（成功与失败）、错误码、通知不应答。
// 全部离线，不依赖 PostgreSQL / K8s（与 IMPLEMENTATION §15 的测试门禁一致）。

type harness struct {
	ctrl  *httptest.Server
	in    *io.PipeWriter
	out   *bufio.Reader
	done  chan error
	base  string
	token string
}

// newHarness 起一个假控制面与一个通过 io.Pipe 驱动的 MCP server。
func newHarness(t *testing.T, handler http.Handler) *harness {
	t.Helper()
	ctrl := httptest.NewServer(handler)
	t.Cleanup(ctrl.Close)

	c, err := NewClient(ctrl.URL, "test-token", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return startHarness(t, c, ctrl)
}

func startHarness(t *testing.T, c *Client, ctrl *httptest.Server) *harness {
	t.Helper()
	srv := NewServer(c)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{ctrl: ctrl, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := srv.Serve(context.Background(), inR, outW)
		_ = outW.Close()
		h.done <- err
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outR.Close()
	})
	return h
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) writeRaw(t *testing.T, line string) {
	t.Helper()
	if _, err := fmt.Fprintf(h.in, "%s\n", line); err != nil {
		t.Fatalf("写 stdio 失败: %v", err)
	}
}

func (h *harness) call(t *testing.T, id int, method string, params any) rpcResp {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	h.writeRaw(t, string(b))
	return h.read(t)
}

func (h *harness) notify(t *testing.T, method string, params any) {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("编码通知: %v", err)
	}
	h.writeRaw(t, string(b))
}

func (h *harness) read(t *testing.T) rpcResp {
	t.Helper()
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := h.out.ReadBytes('\n')
		ch <- result{line: line, err: err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("读 MCP 应答失败: %v", got.err)
		}
		var resp rpcResp
		if err := json.Unmarshal(bytes.TrimSpace(got.line), &resp); err != nil {
			t.Fatalf("应答不是 JSON: %v（原文 %q）", err, got.line)
		}
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("等待 MCP 应答超时")
		return rpcResp{}
	}
}

// toolResult 解码 tools/call 的 result。
type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func decodeToolResult(t *testing.T, resp rpcResp) toolCallResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tools/call 返回协议错误: %+v", resp.Error)
	}
	var res toolCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解码 tools/call result: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("content 形状不符合预期: %+v", res.Content)
	}
	return res
}

func decodeToolJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("工具返回的 text 不是 JSON: %v（原文 %q）", err, text)
	}
	return v
}

// writeSSE 把预拼好的帧原样写出并 flush，模拟控制面的 text/event-stream。
func writeSSE(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
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

// ---- initialize / tools/list ---------------------------------------------

func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("initialize 不应访问控制面")
	}))

	resp := h.call(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0.1"},
	})
	if resp.Error != nil {
		t.Fatalf("initialize 失败: %+v", resp.Error)
	}
	if string(resp.ID) != "1" || resp.JSONRPC != "2.0" {
		t.Fatalf("JSON-RPC 外壳不对: %+v", resp)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("解码 initialize result: %v", err)
	}
	if result.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocolVersion = %q，期望回显客户端支持的 2025-03-26", result.ProtocolVersion)
	}
	if result.Capabilities.Tools == nil {
		t.Error("capabilities.tools 必须存在（即使为空对象）")
	}
	if result.ServerInfo.Name != "argus" || result.ServerInfo.Version != Version {
		t.Errorf("serverInfo = %+v", result.ServerInfo)
	}
}

func TestInitializeFallsBackToSupportedVersion(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	resp := h.call(t, 1, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("解码 initialize result: %v", err)
	}
	if result.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q，期望回退到 %q", result.ProtocolVersion, protocolVersion)
	}
}

func TestToolsList(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("tools/list 不应访问控制面")
	}))
	resp := h.call(t, 2, "tools/list", nil)
	if resp.Error != nil {
		t.Fatalf("tools/list 失败: %+v", resp.Error)
	}
	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("解码 tools/list: %v", err)
	}

	want := []string{
		"argus_submit_task",
		"argus_get_task",
		"argus_wait_task",
		"argus_cancel_task",
		"argus_retry_task",
		"argus_clean_task",
		"argus_list_runners",
		"argus_get_report",
		"argus_get_logs",
		"argus_list_artifacts",
		"argus_get_file",
		"argus_queue_status",
		"argus_list_profiles",
	}
	if len(result.Tools) != len(want) {
		t.Fatalf("工具数 = %d，期望 %d", len(result.Tools), len(want))
	}
	got := map[string]Tool{}
	for _, tool := range result.Tools {
		got[tool.Name] = tool
		if tool.Description == "" {
			t.Errorf("%s 缺少 description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s 的 inputSchema.type 必须是 object", tool.Name)
		}
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("缺少工具 %s", name)
		}
	}

	submit := got["argus_submit_task"]
	required, _ := submit.InputSchema["required"].([]any)
	if len(required) != 1 || required[0] != "manifest" {
		t.Errorf("submit_task.required = %v，期望 [manifest]", submit.InputSchema["required"])
	}
	props, _ := submit.InputSchema["properties"].(map[string]any)
	if _, ok := props["dry_run"]; !ok {
		t.Error("submit_task 缺少 dry_run 参数")
	}
	waitProps, _ := got["argus_wait_task"].InputSchema["properties"].(map[string]any)
	if _, ok := waitProps["timeout_seconds"]; !ok {
		t.Error("wait_run 缺少 timeout_seconds 参数")
	}
	if _, ok := waitProps["follow"]; !ok {
		t.Error("wait_run 缺少 follow 参数")
	}
	fileProps, _ := got["argus_get_file"].InputSchema["properties"].(map[string]any)
	if _, ok := fileProps["range"]; !ok {
		t.Error("get_file 缺少 range 参数")
	}
	fileRequired, _ := got["argus_get_file"].InputSchema["required"].([]any)
	if len(fileRequired) != 2 {
		t.Errorf("get_file.required = %v，期望 [run_id path]", fileRequired)
	}
	if got["argus_queue_status"].InputSchema["additionalProperties"] != false {
		t.Error("queue_status 的 inputSchema 应为空对象（additionalProperties=false）")
	}
}

// ---- tools/call：成功路径 -------------------------------------------------

func TestToolsCallGetTaskSuccess(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/tasks/t-20261110-0001" {
			t.Errorf("意外的请求: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "argus-mcp/") {
			t.Errorf("User-Agent = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t-20261110-0001","status":"running","task_counts":{"total":2,"succeeded":1,"failed":0}}`)
	}))

	resp := h.call(t, 3, "tools/call", map[string]any{
		"name":      "argus_get_task",
		"arguments": map[string]any{"task_id": "t-20261110-0001"},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("get_task 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["status"] != "running" || body["id"] != "t-20261110-0001" {
		t.Errorf("get_task 返回 %+v", body)
	}
}

func TestToolsCallSubmitRunDryRun(t *testing.T) {
	var gotPath, gotQuery, gotCT, gotAuth string
	var gotBody []byte
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"valid":false,"issues":[{"path":"runners[0].input","code":"INPUT_SYNTAX","message":"--url 由平台注入"}]}`)
	}))

	manifest := `{"task":{"name":"x","service":{"url":"http://mock:18080/v1"},"runners":[]}}`
	resp := h.call(t, 4, "tools/call", map[string]any{
		"name": "argus_submit_task",
		"arguments": map[string]any{
			"manifest": manifest,
			"dry_run":  true,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("dry_run 的业务结果不是 isError: %s", res.Content[0].Text)
	}
	if gotPath != "/api/v1/tasks" || gotQuery != "dry_run=true" {
		t.Errorf("请求路径/查询 = %s?%s", gotPath, gotQuery)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", gotCT)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if string(gotBody) != manifest {
		t.Errorf("转发 body 被改写: %q", gotBody)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["valid"] != false {
		t.Errorf("dry_run 返回 %+v", body)
	}
}

func TestToolsCallSubmitRunYAML(t *testing.T) {
	var gotCT string
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, `{"task":{"id":"t-1","status":"queued","current_attempt":1},"runners":[]}`)
	}))
	resp := h.call(t, 5, "tools/call", map[string]any{
		"name":      "argus_submit_task",
		"arguments": map[string]any{"manifest": "run:\n  name: x\n"},
	})
	if res := decodeToolResult(t, resp); res.IsError {
		t.Fatalf("submit 失败: %s", res.Content[0].Text)
	}
	if gotCT != "application/yaml" {
		t.Errorf("Content-Type = %q，期望 application/yaml", gotCT)
	}
}

func TestToolsCallWaitRunSucceeds(t *testing.T) {
	var calls int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		status := "running"
		if n >= 3 {
			status = "succeeded"
		}
		_, _ = fmt.Fprintf(w, `{"id":"t-1","status":%q,"task_counts":{"total":2,"succeeded":2}}`, status)
	}))

	resp := h.call(t, 6, "tools/call", map[string]any{
		"name": "argus_wait_task",
		"arguments": map[string]any{
			"task_id":          "t-1",
			"timeout_seconds":  5,
			"interval_seconds": 0.02,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("wait 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["outcome"] != "success" || body["terminal"] != true || body["status"] != "succeeded" {
		t.Errorf("wait 返回 %+v", body)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("轮询次数 = %d，期望 ≥3", calls)
	}
}

func TestToolsCallWaitRunTimesOut(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"t-1","status":"running"}`)
	}))
	resp := h.call(t, 7, "tools/call", map[string]any{
		"name": "argus_wait_task",
		"arguments": map[string]any{
			"task_id":          "t-1",
			"timeout_seconds":  0.15,
			"interval_seconds": 0.03,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("timeout 不是 isError（它只是本次等待没等到终态）: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["outcome"] != "timeout" || body["terminal"] != false || body["status"] != "running" {
		t.Errorf("wait timeout 返回 %+v", body)
	}
}

func TestToolsCallCancelTask(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/t-1/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"task_id":"t-1","status":"cancelling"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/t-1":
			_, _ = io.WriteString(w, `{"id":"t-1","status":"canceled"}`)
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
	}))
	resp := h.call(t, 8, "tools/call", map[string]any{
		"name":      "argus_cancel_task",
		"arguments": map[string]any{"task_id": "t-1"},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("cancel 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["accepted"] != true || body["confirmed"] != true || body["status"] != "canceled" {
		t.Errorf("cancel 返回 %+v", body)
	}
	if body["http_status"] != float64(http.StatusAccepted) {
		t.Errorf("http_status = %v，期望 202", body["http_status"])
	}
}

func TestToolsCallGetLogsReportsOffset(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/t-1":
			_, _ = io.WriteString(w, `{"id":"t-1","status":"failed","runners":[{"seq":3,"status":"failed","error_code":"RUNNER_NONZERO_EXIT"},{"seq":1,"status":"succeeded"}]}`)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/t-1/attempts/2/runners/3/logs":
			// 命中断言在下面
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("tail"); got != "5" {
			t.Errorf("tail = %q", got)
		}
		w.Header().Set("X-Log-Offset", "4096")
		w.Header().Set("X-Log-Coverage", "2026-11-05T01:00:00Z/2026-11-05T01:01:00Z")
		_, _ = io.WriteString(w, "hello log\n")
	}))
	resp := h.call(t, 9, "tools/call", map[string]any{
		"name": "argus_get_logs",
		"arguments": map[string]any{
			"task_id": "t-1",
			"attempt": 2,
			"tail":    5,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("logs 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["text"] != "hello log\n" {
		t.Errorf("text = %q", body["text"])
	}
	if body["next_offset"] != float64(4096) {
		t.Errorf("next_offset = %v，期望 4096", body["next_offset"])
	}
	if body["coverage"] != "2026-11-05T01:00:00Z/2026-11-05T01:01:00Z" {
		t.Errorf("coverage = %v", body["coverage"])
	}
}

func TestToolsCallGetReportMarkdown(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks/t-1/report.md" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "# t-1\n\n- 状态：succeeded\n")
	}))
	resp := h.call(t, 10, "tools/call", map[string]any{
		"name":      "argus_get_report",
		"arguments": map[string]any{"task_id": "t-1", "format": "markdown"},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("report 不应 isError: %s", res.Content[0].Text)
	}
	if res.Content[0].Text != "# t-1\n\n- 状态：succeeded\n" {
		t.Errorf("markdown 原文被改写: %q", res.Content[0].Text)
	}
}

// ---- tools/call：失败与协议错误 -------------------------------------------

func TestToolsCallHTTPErrorIsIsError(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"Task 不存在","detail":{"task_id":"t-404"},"retryable":false,"trace_id":"01JABC"}`)
	}))
	resp := h.call(t, 11, "tools/call", map[string]any{
		"name":      "argus_get_task",
		"arguments": map[string]any{"task_id": "t-404"},
	})
	if resp.Error != nil {
		t.Fatalf("REST 错误应映射成 isError，而不是协议错误: %+v", resp.Error)
	}
	res := decodeToolResult(t, resp)
	if !res.IsError {
		t.Fatalf("404 应返回 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "NOT_FOUND" || errObj["http_status"] != float64(404) || errObj["trace_id"] != "01JABC" {
		t.Errorf("错误对象 = %+v", errObj)
	}
}

func TestToolsCallProfiles(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/profiles" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"items":[{"name":"k8s-npu","cluster":"cluster-01","namespace":"argus","mounts":[],"scheduler":"default","deploy_timeout_seconds":259200}],"has_more":false}`)
	}))
	resp := h.call(t, 12, "tools/call", map[string]any{
		"name":      "argus_list_profiles",
		"arguments": map[string]any{},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("profiles 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("profiles 返回 %+v", body)
	}
}

func TestToolsCallListArtifactsRunAndTask(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/t-1/artifacts":
			_, _ = io.WriteString(w, `{"items":[{"path":"report.json","kind":"report","size_bytes":12}],"by_kind":{"report":[{"path":"report.json","kind":"report","size_bytes":12}]},"total_size_bytes":12}`)
		case "/api/v1/tasks/t-1/attempts/2/runners/2/artifacts":
			_, _ = io.WriteString(w, `{"items":[{"path":"attempts/2/runners/2/result.json","kind":"result","size_bytes":34,"attempt_no":2,"runner_seq":2}],"by_kind":{"result":[]},"total_size_bytes":34}`)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))

	// Task 级
	resp := h.call(t, 20, "tools/call", map[string]any{
		"name":      "argus_list_artifacts",
		"arguments": map[string]any{"task_id": "t-1"},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("artifacts(run) 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["total_size_bytes"] != float64(12) {
		t.Errorf("run artifacts = %+v", body)
	}

	// Attempt 级
	resp = h.call(t, 21, "tools/call", map[string]any{
		"name":      "argus_list_artifacts",
		"arguments": map[string]any{"task_id": "t-1", "attempt": 2, "runner_seq": 2},
	})
	res = decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("artifacts(task) 不应 isError: %s", res.Content[0].Text)
	}
	body = decodeToolJSON(t, res.Content[0].Text)
	if body["total_size_bytes"] != float64(34) {
		t.Errorf("attempt artifacts = %+v", body)
	}

	// 缺 task_id → -32602。
	resp = h.call(t, 22, "tools/call", map[string]any{
		"name":      "argus_list_artifacts",
		"arguments": map[string]any{},
	})
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Errorf("缺 task_id 应返回 -32602，实际 %+v", resp.Error)
	}
}

func TestToolsCallGetFileSummary(t *testing.T) {
	var gotRange, gotPath string
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRange = r.Header.Get("Range")
		if r.URL.Query().Get("path") != "attempts/1/logs/stdout.log" {
			t.Errorf("path 查询 = %q", r.URL.Query().Get("path"))
		}
		w.Header().Set("Content-Range", "bytes 0-15/16")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "hello log line\n")
	}))
	resp := h.call(t, 23, "tools/call", map[string]any{
		"name": "argus_get_file",
		"arguments": map[string]any{
			"task_id":   "t-1",
			"path":      "t-1/attempts/1/logs/stdout.log", // artifacts 风格：带 task_id 前缀
			"max_bytes": 1024,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("get_file 不应 isError: %s", res.Content[0].Text)
	}
	if gotPath != "/api/v1/tasks/t-1/files" || gotRange != "bytes=0-1023" {
		t.Errorf("请求形状不对: path=%q range=%q", gotPath, gotRange)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["api_path"] != "attempts/1/logs/stdout.log" {
		t.Errorf("api_path = %v，期望剥离 task_id 前缀", body["api_path"])
	}
	if body["encoding"] != "text" || body["preview"] != "hello log line\n" {
		t.Errorf("预览 = %+v", body)
	}
	if body["sha256_scope"] != "partial" || body["truncated"] != true {
		t.Errorf("sha256_scope/truncated = %v/%v", body["sha256_scope"], body["truncated"])
	}
	if body["total_size_bytes"] != float64(16) || body["size_bytes"] != float64(15) {
		t.Errorf("size = %v/%v", body["size_bytes"], body["total_size_bytes"])
	}

	// 二进制 → base64 标记
	h2 := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0x00, 0x01, 0xff, 0xfe})
	}))
	resp = h2.call(t, 24, "tools/call", map[string]any{
		"name":      "argus_get_file",
		"arguments": map[string]any{"task_id": "t-1", "path": "bin.dat", "max_bytes": 1024},
	})
	res = decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("get_file(binary) 不应 isError: %s", res.Content[0].Text)
	}
	body = decodeToolJSON(t, res.Content[0].Text)
	if body["encoding"] != "base64" || body["preview_base64"] != "AAH//g==" {
		t.Errorf("二进制预览 = %+v", body)
	}
}

func TestHintsCoverKnownCodes(t *testing.T) {
	// 附录 D 全部错误码 + 两个合成/终态码：每个都要有 next_action。
	required := []string{
		"VALIDATION_ERROR", "UNAUTHENTICATED", "FORBIDDEN", "NOT_FOUND", "CONFLICT",
		"RUNNER_NOT_FOUND", "RUNNER_VERSION_PINNED", "RUNNER_INVALID",
		"EXECUTION_PROFILE_NOT_FOUND", "TASK_TOO_LARGE", "ATTEMPT_EXHAUSTED",
		"RESOURCE_EXHAUSTED", "ARTIFACT_QUOTA_EXCEEDED", "EXECUTOR_UNAVAILABLE",
		"EXECUTOR_POD_ERROR", "IMAGE_PULL_FAILED", "MOUNT_UNAVAILABLE", "EXPORT_INVALID",
		"ENDPOINT_UNREACHABLE", "PRECHECK_FAILED", "RUNNER_TIMEOUT", "RUNNER_NONZERO_EXIT",
		"RESULT_MISSING", "RESULT_INVALID", "CLEANUP_FAILED", "CANCELED", "INTERNAL_ERROR",
		"DEPLOY_FAILED", "DEPLOY_BLOCKED",
		"SERVICE_UNAVAILABLE", "UNREACHABLE",
	}
	for _, code := range required {
		h, ok := HintForError(code)
		if !ok {
			t.Errorf("缺少错误码 %s 的提示", code)
			continue
		}
		if h.Code != code || h.NextAction == "" {
			t.Errorf("%s 的 hint 不完整: %+v", code, h)
		}
	}
	if len(errorHints) != len(required) {
		t.Errorf("hints 表有 %d 项，测试清单 %d 项；新增码时同步改两边", len(errorHints), len(required))
	}
	// 抽检 lead 指定的语义，防止后续改坏。
	checks := map[string]string{
		"PRECHECK_FAILED":      "precheck",
		"DEPLOY_FAILED":        "argus retry",
		"DEPLOY_BLOCKED":       "deploy",
		"RUNNER_TIMEOUT":       "timeout",
		"RESULT_MISSING":       "result.json",
		"FORBIDDEN":            "operator",
		"VALIDATION_ERROR":     "issues",
		"UNAUTHENTICATED":      "token",
		"ENDPOINT_UNREACHABLE": "deploy",
	}
	for code, want := range checks {
		h, _ := HintForError(code)
		if !strings.Contains(h.NextAction, want) && !strings.Contains(h.Hint, want) {
			t.Errorf("%s 的提示里应提到 %q：%+v", code, want, h)
		}
	}
	if _, ok := HintForError("SOME_NEW_CODE"); ok {
		t.Error("未知错误码不应有提示")
	}
	if _, ok := HintForTaskStatus("canceled"); !ok {
		t.Error("canceled 终态应有提示")
	}
}

// ---- SSE 与 wait --follow ------------------------------------------------

func TestReadSSEParsesFrames(t *testing.T) {
	stream := ": ping\n\n" +
		"id: 7\nevent: state_changed\ndata: {\"seq\":7,\"to\":\"running\"}\n\n" +
		"id: 8\nevent: task_finished\ndata: line1\ndata: line2\n\n"
	var frames []SSEFrame
	if err := ReadSSE(strings.NewReader(stream), func(f SSEFrame) error {
		frames = append(frames, f)
		return nil
	}); err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("帧数 = %d，期望 2（注释不计）", len(frames))
	}
	if frames[0].ID != "7" || !frames[0].HasID || frames[0].Event != "state_changed" {
		t.Errorf("第一帧 = %+v", frames[0])
	}
	if frames[1].Event != "task_finished" || string(frames[1].Data) != "line1\nline2" {
		t.Errorf("第二帧 = %+v", frames[1])
	}
}

func TestWaitFollowUsesSSE(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/t-1":
			n := atomic.AddInt32(&gets, 1)
			status := "running"
			if n >= 2 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"t-1","status":%q,"task_counts":{"total":1,"succeeded":1}}`, status)
		case "/api/v1/tasks/t-1/events":
			if got := r.Header.Get("Accept"); got != "text/event-stream" {
				t.Errorf("Accept = %q", got)
			}
			writeSSE(t, w,
				"id: 1\nevent: state_changed\ndata: {\"seq\":1,\"type\":\"state_changed\",\"to\":\"succeeded\"}\n\n",
				"id: 2\nevent: task_finished\ndata: {\"seq\":2,\"type\":\"task_finished\",\"to\":\"succeeded\"}\n\n",
			)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "dev", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Wait(context.Background(), c, "t-1", WaitOptions{
		Timeout: 2 * time.Second, Interval: 20 * time.Millisecond, Follow: true,
	})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Mode != "sse" || res.Outcome != "success" || res.Status != "succeeded" {
		t.Errorf("Wait 结果 = %+v", res)
	}
	if res.FallbackReason != "" {
		t.Errorf("不应降级: %s", res.FallbackReason)
	}
}

func TestWaitFollowFallsBackToPoll(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/t-1/events":
			http.Error(w, `{"code":"NOT_FOUND","message":"no sse"}`, http.StatusNotFound)
		case "/api/v1/tasks/t-1":
			n := atomic.AddInt32(&gets, 1)
			status := "running"
			if n >= 2 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"t-1","status":%q}`, status)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "dev", 5*time.Second)

	var fallback string
	res, err := Wait(context.Background(), c, "t-1", WaitOptions{
		Timeout: 2 * time.Second, Interval: 20 * time.Millisecond, Follow: true,
		OnFallback: func(reason string) { fallback = reason },
	})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Mode != "poll" || res.Outcome != "success" || res.FallbackReason == "" || fallback == "" {
		t.Errorf("降级结果 = %+v，fallback=%q", res, fallback)
	}
}

func TestWaitFollowReconnectsWithLastEventID(t *testing.T) {
	var eventsCalls int32
	var gets int32
	var lastID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/t-1":
			n := atomic.AddInt32(&gets, 1)
			status := "running"
			if n >= 3 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"t-1","status":%q}`, status)
		case "/api/v1/tasks/t-1/events":
			n := atomic.AddInt32(&eventsCalls, 1)
			if n == 1 {
				// 发一条事件就断线（不写 task_finished）。
				writeSSE(t, w, "id: 5\nevent: task.state_changed\ndata: {\"seq\":5,\"type\":\"task.state_changed\",\"to\":\"running\"}\n\n")
				return
			}
			lastID = r.Header.Get("Last-Event-ID")
			writeSSE(t, w, "id: 6\nevent: task_finished\ndata: {\"seq\":6,\"type\":\"task_finished\",\"to\":\"succeeded\"}\n\n")
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "dev", 5*time.Second)

	res, err := Wait(context.Background(), c, "t-1", WaitOptions{
		Timeout: 3 * time.Second, Interval: 20 * time.Millisecond, Follow: true,
		ReconnectBackoff: 10 * time.Millisecond, MaxReconnects: 3,
	})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Mode != "sse" || res.Outcome != "success" {
		t.Errorf("重连结果 = %+v", res)
	}
	if lastID != "5" {
		t.Errorf("重连的 Last-Event-ID = %q，期望 5", lastID)
	}
	if atomic.LoadInt32(&eventsCalls) != 2 {
		t.Errorf("SSE 连接次数 = %d，期望 2", eventsCalls)
	}
}

func TestWaitFollowSuppressesReplayNoise(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/t-1":
			n := atomic.AddInt32(&gets, 1)
			status := "running"
			if n >= 2 {
				status = "succeeded"
			}
			_, _ = fmt.Fprintf(w, `{"id":"t-1","status":%q}`, status)
		case "/api/v1/tasks/t-1/events":
			// 从 seq=1 回放：queued→running 是历史，running→succeeded 是新的。
			writeSSE(t, w,
				"id: 1\nevent: state_changed\ndata: {\"seq\":1,\"to\":\"queued\"}\n\n",
				"id: 2\nevent: state_changed\ndata: {\"seq\":2,\"to\":\"running\"}\n\n",
				"id: 3\nevent: state_changed\ndata: {\"seq\":3,\"to\":\"running\"}\n\n",
				"id: 4\nevent: task_finished\ndata: {\"seq\":4,\"to\":\"succeeded\"}\n\n",
			)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "dev", 5*time.Second)

	var statuses []string
	res, err := Wait(context.Background(), c, "t-1", WaitOptions{
		Timeout: 2 * time.Second, Interval: 20 * time.Millisecond, Follow: true,
		OnStatus: func(status string) { statuses = append(statuses, status) },
	})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Mode != "sse" || res.Outcome != "success" {
		t.Errorf("Wait 结果 = %+v", res)
	}
	want := []string{"running", "succeeded"}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Errorf("OnStatus 序列 = %v，期望 %v（回放的历史 queued 应被抑制）", statuses, want)
	}
}

func TestToolsCallWaitRunFollow(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/t-1/events":
			writeSSE(t, w, "id: 3\nevent: task_finished\ndata: {\"seq\":3,\"type\":\"task_finished\",\"to\":\"failed\"}\n\n")
		case "/api/v1/tasks/t-1":
			if atomic.AddInt32(&gets, 1) == 1 {
				_, _ = io.WriteString(w, `{"id":"t-1","status":"running"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"t-1","status":"failed","error":{"code":"PRECHECK_FAILED","message":"precheck failed","retryable":false}}`)
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "dev", 5*time.Second)
	h := startHarness(t, c, srv)

	resp := h.call(t, 25, "tools/call", map[string]any{
		"name": "argus_wait_task",
		"arguments": map[string]any{
			"task_id": "t-1", "timeout_seconds": 5, "follow": true,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("wait(follow) 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["wait_mode"] != "sse" || body["outcome"] != "failed" {
		t.Errorf("wait(follow) 返回 %+v", body)
	}
	if !strings.Contains(fmt.Sprint(body["next_action"]), "precheck") {
		t.Errorf("失败终态应带 next_action，实际 %+v", body["next_action"])
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	resp := h.call(t, 13, "resources/list", nil)
	if resp.Error == nil || resp.Error.Code != codeMethodMissing {
		t.Fatalf("未知方法应返回 %d，实际 %+v", codeMethodMissing, resp)
	}
	if string(resp.ID) != "13" {
		t.Errorf("错误应答必须回显 id，实际 %s", resp.ID)
	}
}

func TestUnknownToolReturnsInvalidParams(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	resp := h.call(t, 14, "tools/call", map[string]any{
		"name":      "argus_make_coffee",
		"arguments": map[string]any{},
	})
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("未知工具应返回 %d，实际 %+v", codeInvalidParams, resp)
	}
}

func TestMissingRequiredArgumentReturnsInvalidParams(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("参数非法时不应访问控制面")
	}))
	resp := h.call(t, 15, "tools/call", map[string]any{
		"name":      "argus_get_task",
		"arguments": map[string]any{},
	})
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("缺 run_id 应返回 %d，实际 %+v", codeInvalidParams, resp)
	}
}

func TestMalformedJSONReturnsParseError(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.writeRaw(t, "{not json")
	resp := h.read(t)
	if resp.Error == nil || resp.Error.Code != codeParseError {
		t.Fatalf("坏 JSON 应返回 %d，实际 %+v", codeParseError, resp)
	}
	if string(resp.ID) != "null" {
		t.Errorf("解析错误应答的 id 应为 null，实际 %s", resp.ID)
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.notify(t, "notifications/initialized", map[string]any{})
	// 通知不应答；随后的 ping 应答必须是收到的第一条消息。
	resp := h.call(t, 16, "ping", nil)
	if resp.Error != nil {
		t.Fatalf("ping 失败: %+v", resp.Error)
	}
	if string(resp.ID) != "16" {
		t.Fatalf("通知被应答了：收到 id=%s", resp.ID)
	}
}

func TestTransportErrorBecomesIsError(t *testing.T) {
	// 拿一个刚关闭的本地端口，保证连接被拒绝（不用外网）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	c, err := NewClient("http://"+addr, "dev", 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	h := startHarness(t, c, nil)
	resp := h.call(t, 17, "tools/call", map[string]any{
		"name":      "argus_get_task",
		"arguments": map[string]any{"task_id": "t-1"},
	})
	res := decodeToolResult(t, resp)
	if !res.IsError {
		t.Fatalf("连不上控制面应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "UNREACHABLE" {
		t.Errorf("code = %v，期望 UNREACHABLE", errObj["code"])
	}
}

func TestToolsCallRetryTaskExhausted(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/t-1/retry" {
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"ATTEMPT_EXHAUSTED","message":"attempt 上限","detail":{"attempt_max":3},"retryable":false,"trace_id":"01JRE"}`)
	}))
	resp := h.call(t, 30, "tools/call", map[string]any{
		"name":      "argus_retry_task",
		"arguments": map[string]any{"task_id": "t-1"},
	})
	res := decodeToolResult(t, resp)
	if !res.IsError {
		t.Fatalf("ATTEMPT_EXHAUSTED 应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "ATTEMPT_EXHAUSTED" || errObj["http_status"] != float64(409) {
		t.Errorf("错误对象 = %+v", errObj)
	}
	if !strings.Contains(fmt.Sprint(errObj["next_action"]), "attempt") {
		t.Errorf("应带 next_action: %+v", errObj)
	}
}

func TestToolsCallCleanTaskForwardsAttempt(t *testing.T) {
	var gotQuery, gotBody string
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks/t-1/clean" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"task_id":"t-1","attempt_no":1,"runner":{"seq":5,"name":"service-clean-replay","runner_name":"service-clean","status":"queued"}}`)
	}))
	resp := h.call(t, 31, "tools/call", map[string]any{
		"name":      "argus_clean_task",
		"arguments": map[string]any{"task_id": "t-1", "attempt": 1},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("clean 不应 isError: %s", res.Content[0].Text)
	}
	if gotBody != `{"attempt":1}` {
		t.Errorf("body = %q", gotBody)
	}
	_ = gotQuery
}
