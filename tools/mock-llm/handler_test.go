package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- 测试脚手架 ----------------------------------------------------------

type testServer struct {
	*httptest.Server
	mock *Server
}

func newTestServer(t *testing.T, cfg Config) *testServer {
	t.Helper()
	cfg.Quiet = true
	s := NewServer(cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testServer{Server: ts, mock: s}
}

func doRequest(t *testing.T, method, url, body string) (int, http.Header, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败：%v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// chatPayload 是最小请求体：endpoint-probe 真实发出的形状（max_tokens=1、stream=true）。
func chatPayload(stream bool, maxTokens int) string {
	b, _ := json.Marshal(map[string]any{
		"model":       "mock-llm",
		"messages":    []any{map[string]any{"role": "user", "content": "hello"}},
		"max_tokens":  maxTokens,
		"temperature": 0,
		"stream":      stream,
	})
	return string(b)
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, body string) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("错误体不是 JSON：%v；body=%q", err, body)
	}
	if e.Error.Message == "" || e.Error.Code == "" {
		t.Fatalf("错误体缺 message/code：%q", body)
	}
	return e
}

// ---- 基础端点 ------------------------------------------------------------

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodGet, ts.URL+"/healthz", "")
	if status != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("healthz 期望 200/ok，实际 %d/%q", status, body)
	}
}

func TestModels(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodGet, ts.URL+"/v1/models", "")
	if status != http.StatusOK {
		t.Fatalf("models 期望 200，实际 %d：%s", status, body)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("models 不是合法 JSON：%v", err)
	}
	// endpoint-probe 的 --model_name 省略路径依赖 .data[0].id
	if len(resp.Data) == 0 || resp.Data[0].ID != "mock-llm" {
		t.Fatalf("models 期望 data[0].id=mock-llm，实际 %s", body)
	}
}

// 不带 /v1 的路径也要能用：base URL 与探活路径的拼接方式不该只有一种。
func TestPathsWithoutVersionPrefix(t *testing.T) {
	ts := newTestServer(t, Config{})
	if status, _, body := doRequest(t, http.MethodGet, ts.URL+"/models", ""); status != http.StatusOK {
		t.Fatalf("/models 期望 200，实际 %d：%s", status, body)
	}
	if status, _, body := doRequest(t, http.MethodPost, ts.URL+"/chat/completions", chatPayload(false, 1)); status != http.StatusOK {
		t.Fatalf("/chat/completions 期望 200，实际 %d：%s", status, body)
	}
}

// ---- stream=true：SSE 帧 --------------------------------------------------

func TestChatCompletionsStream(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, header, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions", chatPayload(true, 3))
	if status != http.StatusOK {
		t.Fatalf("stream 期望 200，实际 %d：%s", status, body)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream Content-Type 期望 text/event-stream，实际 %q", ct)
	}
	// endpoint-probe 的判定就是 "响应体出现 data:"，这一条必须成立
	if !strings.Contains(body, "data: ") {
		t.Fatalf("stream 响应体没有 data: 帧：%q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream 最后一帧应为 data: [DONE]，实际结尾：%q", tail(body, 120))
	}

	type sseChunk struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}

	contentFrames := 0
	sawStop := false
	sawDone := false
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("SSE 帧必须以 data: 开头，实际 %q", frame)
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("SSE 帧不是合法 JSON：%v；frame=%q", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("SSE chunk object 期望 chat.completion.chunk，实际 %q", chunk.Object)
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("SSE chunk 期望 1 个 choice，实际 %d：%q", len(chunk.Choices), payload)
		}
		if chunk.Choices[0].Delta.Content != "" {
			contentFrames++
			if contentFrames == 1 && chunk.Choices[0].Delta.Role != "assistant" {
				t.Fatalf("首个内容帧应带 role=assistant，实际 %q", payload)
			}
		}
		if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason == "stop" {
			sawStop = true
		}
	}
	if !sawDone {
		t.Fatal("SSE 缺少 data: [DONE]")
	}
	if !sawStop {
		t.Fatal("SSE 缺少 finish_reason=stop 的收尾帧")
	}
	// max_tokens=3 → 恰好 3 个内容帧（探针用 1 时就是 1 个，首包即通过）
	if contentFrames != 3 {
		t.Fatalf("max_tokens=3 期望 3 个内容帧，实际 %d；body=%q", contentFrames, body)
	}
}

// 探针的真实请求：max_tokens=1、stream=true，必须第一帧就出字。
func TestChatCompletionsProbeShape(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions", chatPayload(true, 1))
	if status != http.StatusOK {
		t.Fatalf("探针形状请求期望 200，实际 %d：%s", status, body)
	}
	if !strings.Contains(body, "data: ") {
		t.Fatalf("探针形状请求没有 data: 帧：%q", body)
	}
}

// ---- stream=false：普通 JSON ---------------------------------------------

func TestChatCompletionsNonStream(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, header, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions", chatPayload(false, 1))
	if status != http.StatusOK {
		t.Fatalf("non-stream 期望 200，实际 %d：%s", status, body)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("non-stream Content-Type 期望 application/json，实际 %q", ct)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("non-stream 不是合法 JSON：%v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("non-stream 期望 1 个 choice，实际 %d", len(resp.Choices))
	}
	// endpoint-probe 的 --stream=false 分支要求 choices[0].message.content 非空
	if strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		t.Fatalf("non-stream content 不能为空：%s", body)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("non-stream finish_reason 期望 stop，实际 %q", resp.Choices[0].FinishReason)
	}
}

// ---- 故障注入 ------------------------------------------------------------

func TestInjectedFailure500(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions?fail=500", chatPayload(true, 1))
	if status != http.StatusInternalServerError {
		t.Fatalf("?fail=500 期望 500，实际 %d：%s", status, body)
	}
	e := decodeError(t, body)
	if e.Error.Code != "service_unavailable" {
		t.Fatalf("500 的 error.code 期望 service_unavailable，实际 %q", e.Error.Code)
	}
}

func TestInjectedFailure401(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions?fail=401", chatPayload(true, 1))
	if status != http.StatusUnauthorized {
		t.Fatalf("?fail=401 期望 401，实际 %d：%s", status, body)
	}
	e := decodeError(t, body)
	if e.Error.Code != "invalid_api_key" {
		t.Fatalf("401 的 error.code 期望 invalid_api_key，实际 %q", e.Error.Code)
	}
	if e.Error.Type != "authentication_error" {
		t.Fatalf("401 的 error.type 期望 authentication_error，实际 %q", e.Error.Type)
	}
}

// 查询参数必须覆盖服务级默认值（否则 e2e 想临时换注入码就会被默认值粘住）。
func TestQueryOverridesDefaultFailure(t *testing.T) {
	ts := newTestServer(t, Config{FailStatus: 500})
	status, _, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions?fail=401", chatPayload(true, 1))
	if status != http.StatusUnauthorized {
		t.Fatalf("?fail=401（默认 500）期望 401，实际 %d：%s", status, body)
	}
}

func TestErrorObjectInHTTP200(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, header, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions?error_in_200=1", chatPayload(true, 1))
	if status != http.StatusOK {
		t.Fatalf("?error_in_200=1 期望 HTTP 200，实际 %d", status)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error-in-200 的 Content-Type 期望 application/json，实际 %q", ct)
	}
	e := decodeError(t, body)
	if e.Error.Code != "gateway_error" {
		t.Fatalf("error-in-200 的 error.code 期望 gateway_error，实际 %q", e.Error.Code)
	}
	// 关键：HTTP 200 但正文是 error 对象，且没有 SSE 帧——探针必须判"不可重试"
	if strings.Contains(body, "data:") {
		t.Fatalf("error-in-200 不应出现 data: 帧：%q", body)
	}
}

// ready_after 的语义是"服务启动后前 N 秒"：503 带 Retry-After，过了窗口就 200。
func TestReadyAfter(t *testing.T) {
	ts := newTestServer(t, Config{ReadyAfter: 30 * time.Second})
	status, header, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions", chatPayload(true, 1))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("启动窗口内期望 503，实际 %d：%s", status, body)
	}
	if header.Get("Retry-After") == "" {
		t.Fatal("503 应带 Retry-After 头")
	}
	if e := decodeError(t, body); e.Error.Code != "starting" {
		t.Fatalf("503 的 error.code 期望 starting，实际 %q", e.Error.Code)
	}

	// 模拟时间已过窗口
	ts.mock.startedAt = time.Now().Add(-31 * time.Second)
	status, _, body = doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions", chatPayload(true, 1))
	if status != http.StatusOK {
		t.Fatalf("窗口过后期望 200，实际 %d：%s", status, body)
	}
}

func TestReadyAfterAppliesToModels(t *testing.T) {
	ts := newTestServer(t, Config{ReadyAfter: time.Hour})
	if status, _, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/models", ""); status != http.StatusServiceUnavailable {
		t.Fatalf("就绪窗口内 /v1/models 期望 503，实际 %d", status)
	}
}

// healthz 不受 ready_after 影响：进程活着和"能出 token"是两件事。
func TestHealthzIgnoresReadyAfter(t *testing.T) {
	ts := newTestServer(t, Config{ReadyAfter: time.Hour})
	if status, _, body := doRequest(t, http.MethodGet, ts.URL+"/healthz", ""); status != http.StatusOK {
		t.Fatalf("healthz 期望 200，实际 %d：%s", status, body)
	}
}

func TestSlowResponse(t *testing.T) {
	ts := newTestServer(t, Config{})
	start := time.Now()
	status, _, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions?slow_ms=150", chatPayload(false, 1))
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("slow_ms 期望最终 200，实际 %d：%s", status, body)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("slow_ms=150 期望至少 120ms，实际 %s", elapsed)
	}
}

// ---- 异常输入 ------------------------------------------------------------

func TestInvalidJSONBody(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodPost, ts.URL+"/v1/chat/completions", "{not json")
	if status != http.StatusBadRequest {
		t.Fatalf("非法 JSON 期望 400，实际 %d：%s", status, body)
	}
	if e := decodeError(t, body); e.Error.Code != "invalid_json" {
		t.Fatalf("非法 JSON 的 error.code 期望 invalid_json，实际 %q", e.Error.Code)
	}
}

func TestInvalidControlParameter(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodPost,
		ts.URL+"/v1/chat/completions?fail=abc", chatPayload(true, 1))
	if status != http.StatusBadRequest {
		t.Fatalf("非法控制参数期望 400（不能悄悄当正常请求），实际 %d：%s", status, body)
	}
	if e := decodeError(t, body); e.Error.Code != "invalid_mock_control" {
		t.Fatalf("非法控制参数的 error.code 期望 invalid_mock_control，实际 %q", e.Error.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, header, body := doRequest(t, http.MethodGet, ts.URL+"/v1/chat/completions", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /chat/completions 期望 405，实际 %d：%s", status, body)
	}
	if header.Get("Allow") != http.MethodPost {
		t.Fatalf("405 的 Allow 头期望 POST，实际 %q", header.Get("Allow"))
	}
}

func TestUnknownRoute(t *testing.T) {
	ts := newTestServer(t, Config{})
	status, _, body := doRequest(t, http.MethodGet, ts.URL+"/v1/unknown", "")
	if status != http.StatusNotFound {
		t.Fatalf("未知路由期望 404，实际 %d：%s", status, body)
	}
	decodeError(t, body)
}

// tail 只用于报错时截取结尾，避免把整段 SSE 打进日志。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
