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
	if result.ServerInfo.Name != "testplane" || result.ServerInfo.Version != Version {
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
		"testplane_submit_run",
		"testplane_get_run",
		"testplane_wait_run",
		"testplane_cancel_run",
		"testplane_list_testers",
		"testplane_get_report",
		"testplane_get_logs",
		"testplane_queue_status",
		"testplane_list_profiles",
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

	submit := got["testplane_submit_run"]
	required, _ := submit.InputSchema["required"].([]any)
	if len(required) != 1 || required[0] != "manifest" {
		t.Errorf("submit_run.required = %v，期望 [manifest]", submit.InputSchema["required"])
	}
	props, _ := submit.InputSchema["properties"].(map[string]any)
	if _, ok := props["dry_run"]; !ok {
		t.Error("submit_run 缺少 dry_run 参数")
	}
	waitProps, _ := got["testplane_wait_run"].InputSchema["properties"].(map[string]any)
	if _, ok := waitProps["timeout_seconds"]; !ok {
		t.Error("wait_run 缺少 timeout_seconds 参数")
	}
	if got["testplane_queue_status"].InputSchema["additionalProperties"] != false {
		t.Error("queue_status 的 inputSchema 应为空对象（additionalProperties=false）")
	}
}

// ---- tools/call：成功路径 -------------------------------------------------

func TestToolsCallGetRunSuccess(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs/r-20261105-0001" {
			t.Errorf("意外的请求: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "testplane-mcp/") {
			t.Errorf("User-Agent = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r-20261105-0001","status":"testing","task_counts":{"total":2,"succeeded":1,"failed":0}}`)
	}))

	resp := h.call(t, 3, "tools/call", map[string]any{
		"name":      "testplane_get_run",
		"arguments": map[string]any{"run_id": "r-20261105-0001"},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("get_run 不应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["status"] != "testing" || body["id"] != "r-20261105-0001" {
		t.Errorf("get_run 返回 %+v", body)
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
		_, _ = io.WriteString(w, `{"valid":false,"issues":[{"path":"tests[0].args","code":"PUBLIC_PARAM_CONFLICT","message":"--url 由平台注入"}]}`)
	}))

	manifest := `{"run":{"name":"x","service":{"url":"http://mock:18080/v1"},"tests":[]}}`
	resp := h.call(t, 4, "tools/call", map[string]any{
		"name": "testplane_submit_run",
		"arguments": map[string]any{
			"manifest": manifest,
			"dry_run":  true,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("dry_run 的业务结果不是 isError: %s", res.Content[0].Text)
	}
	if gotPath != "/api/v1/runs" || gotQuery != "dry_run=true" {
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
		_, _ = io.WriteString(w, `{"run":{"id":"r-1","status":"queued","kind":"run"},"tasks":[]}`)
	}))
	resp := h.call(t, 5, "tools/call", map[string]any{
		"name":      "testplane_submit_run",
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
		_, _ = fmt.Fprintf(w, `{"id":"r-1","status":%q,"task_counts":{"total":2,"succeeded":2}}`, status)
	}))

	resp := h.call(t, 6, "tools/call", map[string]any{
		"name": "testplane_wait_run",
		"arguments": map[string]any{
			"run_id":           "r-1",
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
		_, _ = io.WriteString(w, `{"id":"r-1","status":"testing"}`)
	}))
	resp := h.call(t, 7, "tools/call", map[string]any{
		"name": "testplane_wait_run",
		"arguments": map[string]any{
			"run_id":           "r-1",
			"timeout_seconds":  0.15,
			"interval_seconds": 0.03,
		},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("timeout 不是 isError（它只是本次等待没等到终态）: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	if body["outcome"] != "timeout" || body["terminal"] != false || body["status"] != "testing" {
		t.Errorf("wait timeout 返回 %+v", body)
	}
}

func TestToolsCallCancelRun(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/r-1/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"r-1","status":"cancelling"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/r-1":
			_, _ = io.WriteString(w, `{"id":"r-1","status":"canceled"}`)
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
	}))
	resp := h.call(t, 8, "tools/call", map[string]any{
		"name":      "testplane_cancel_run",
		"arguments": map[string]any{"run_id": "r-1"},
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
		if r.URL.Path != "/api/v1/tasks/t-1/attempts/2/logs" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("tail"); got != "5" {
			t.Errorf("tail = %q", got)
		}
		w.Header().Set("X-Log-Offset", "4096")
		w.Header().Set("X-Log-Coverage", "2026-11-05T01:00:00Z/2026-11-05T01:01:00Z")
		_, _ = io.WriteString(w, "hello log\n")
	}))
	resp := h.call(t, 9, "tools/call", map[string]any{
		"name": "testplane_get_logs",
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
		if r.URL.Path != "/api/v1/runs/r-1/report.md" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "# r-1\n\n- 状态：succeeded\n")
	}))
	resp := h.call(t, 10, "tools/call", map[string]any{
		"name":      "testplane_get_report",
		"arguments": map[string]any{"run_id": "r-1", "format": "markdown"},
	})
	res := decodeToolResult(t, resp)
	if res.IsError {
		t.Fatalf("report 不应 isError: %s", res.Content[0].Text)
	}
	if res.Content[0].Text != "# r-1\n\n- 状态：succeeded\n" {
		t.Errorf("markdown 原文被改写: %q", res.Content[0].Text)
	}
}

// ---- tools/call：失败与协议错误 -------------------------------------------

func TestToolsCallHTTPErrorIsIsError(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"Run 不存在","detail":{"run_id":"r-404"},"retryable":false,"trace_id":"01JABC"}`)
	}))
	resp := h.call(t, 11, "tools/call", map[string]any{
		"name":      "testplane_get_run",
		"arguments": map[string]any{"run_id": "r-404"},
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

func TestToolsCallProfilesNotImplemented(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 控制面还没实现 GET /api/v1/profiles
	}))
	resp := h.call(t, 12, "tools/call", map[string]any{
		"name":      "testplane_list_profiles",
		"arguments": map[string]any{},
	})
	res := decodeToolResult(t, resp)
	if !res.IsError {
		t.Fatalf("profiles 未实现应 isError: %s", res.Content[0].Text)
	}
	body := decodeToolJSON(t, res.Content[0].Text)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "ENDPOINT_NOT_IMPLEMENTED" {
		t.Errorf("code = %v，期望 ENDPOINT_NOT_IMPLEMENTED", errObj["code"])
	}
	if !strings.Contains(fmt.Sprint(errObj["message"]), "P1") {
		t.Errorf("message 应说明它属于 P1 首批: %v", errObj["message"])
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
		"name":      "testplane_make_coffee",
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
		"name":      "testplane_get_run",
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
		"name":      "testplane_get_run",
		"arguments": map[string]any{"run_id": "r-1"},
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
