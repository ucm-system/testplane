package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultModel 是 /v1/models 返回的模型 id，也是请求体没带 model 时的兜底。
const defaultModel = "mock-llm"

// replyTokens 是预置回复的 token 序列：stream 模式逐 token 输出，non-stream 模式拼成 content。
// 至少一个 token，保证 max_tokens=1 时 content 仍非空（endpoint-probe 的判定依赖这一点）。
var replyTokens = []string{"Argus ", "mock ", "LLM ", "is ", "ready."}

// Config 是假服务的默认行为；每个请求还可以用查询参数覆盖（见 Server.effectiveConfig）。
//
// 查询参数（与设计里的失败注入场景一一对应）：
//
//	?ready_after=3      服务启动后前 3 秒返回 503（服务"还没起来"）
//	?fail=500           直接返回该 HTTP 状态（500 / 401 / 429 ...）
//	?error_in_200=1     HTTP 200，但响应体是 error 对象（网关把错误包成 200）
//	?slow_ms=2000       响应前额外延迟 2 秒（就绪慢 / 首包慢）
//	?model=other        临时改返回的模型名
//
// 同名环境变量可设置默认值：MOCK_LLM_READY_AFTER / MOCK_LLM_FAIL /
// MOCK_LLM_ERROR_IN_200 / MOCK_LLM_SLOW_MS / MOCK_LLM_MODEL。
type Config struct {
	Model      string
	ReadyAfter time.Duration // 启动后前 N 秒返回 503
	FailStatus int           // 非 0 时 chat/completions 与 models 都返回该 HTTP 状态
	ErrorIn200 bool          // HTTP 200 但响应体是 error 对象
	Slow       time.Duration // 响应前额外延迟
	Quiet      bool          // 不打印请求日志
}

// Server 是假服务的状态。startedAt 是 ready_after 判定的锚点（服务启动时刻），
// 不随请求变化——"前 N 秒"指的是服务起来后的前 N 秒。
type Server struct {
	cfg       Config
	startedAt time.Time
	logger    *log.Logger
}

// NewServer 构造假服务；Model 为空时用 defaultModel。
func NewServer(cfg Config) *Server {
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	return &Server{
		cfg:       cfg,
		startedAt: time.Now(),
		logger:    log.New(os.Stderr, "mock-llm ", log.LstdFlags),
	}
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.route(sw, r)
	if !s.cfg.Quiet {
		s.logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	}
}

// route 按路径后缀分派，因此 /v1/models、/models、/openai/v1/models 都能用：
// endpoint-probe 会把 base URL 补成 <url>/v1 再拼 /models，Tester 使用者也可能直接写 /v1。
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/healthz":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")

	case strings.HasSuffix(path, "/models"):
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleModels(w, r)

	case strings.HasSuffix(path, "/chat/completions"):
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleCompletions(w, r)

	default:
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("mock-llm 没有这个路由：%s %s", r.Method, r.URL.Path))
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.effectiveConfig(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_mock_control", err.Error())
		return
	}
	if !s.checkReady(w, cfg) {
		return
	}
	s.sleep(r, cfg.Slow)
	if cfg.FailStatus != 0 {
		s.writeInjectedFailure(w, cfg.FailStatus)
		return
	}
	if cfg.ErrorIn200 {
		writeError(w, http.StatusOK, "gateway_error",
			"上游错误被网关包装成了 HTTP 200（?error_in_200=1）")
		return
	}
	w.Header().Set("X-Mock-Model", cfg.Model)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []any{map[string]any{
			"id":       cfg.Model,
			"object":   "model",
			"created":  s.startedAt.Unix(),
			"owned_by": "argus",
		}},
	})
}

// chatRequest 只解析判定用得上的字段；其余字段一律忽略（真实 OpenAI 客户端会带很多）。
type chatRequest struct {
	Model               string `json:"model"`
	Stream              bool   `json:"stream"`
	MaxTokens           *int   `json:"max_tokens"`
	MaxCompletionTokens *int   `json:"max_completion_tokens"`
}

func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.effectiveConfig(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_mock_control", err.Error())
		return
	}
	if !s.checkReady(w, cfg) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json",
			fmt.Sprintf("请求体不是合法 JSON：%v", err))
		return
	}
	if req.Model == "" {
		req.Model = cfg.Model
	}

	s.sleep(r, cfg.Slow)
	if r.Context().Err() != nil {
		return // 客户端提前断开（比如探针拿到首包就走了）
	}
	if cfg.FailStatus != 0 {
		s.writeInjectedFailure(w, cfg.FailStatus)
		return
	}
	if cfg.ErrorIn200 {
		writeError(w, http.StatusOK, "gateway_error",
			"上游错误被网关包装成了 HTTP 200（?error_in_200=1）")
		return
	}

	n := maxTokens(req)
	w.Header().Set("X-Mock-Model", cfg.Model)
	if req.Stream {
		s.streamCompletions(w, r, cfg, n)
		return
	}
	s.nonStreamCompletion(w, cfg, n)
}

// maxTokens 决定输出多少个 token：客户端给了正数就按它（但不超过预置 token 数），
// 没给则输出全部；至少 1 个，绝不返回空 content。
func maxTokens(req chatRequest) int {
	n := len(replyTokens)
	limit := req.MaxTokens
	if limit == nil || *limit <= 0 {
		limit = req.MaxCompletionTokens
	}
	if limit != nil && *limit > 0 && *limit < n {
		n = *limit
	}
	if n < 1 {
		n = 1
	}
	return n
}

// streamCompletions 输出 SSE：每个 token 一帧 data: {...}，最后一帧 finish_reason=stop，
// 再接 data: [DONE]。每帧都 Flush，保证探针立刻看到首包。
func (s *Server) streamCompletions(w http.ResponseWriter, r *http.Request, cfg Config, n int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "该连接不支持流式输出")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	writeChunk := func(delta map[string]any, finishReason any) {
		writeSSE(w, map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   cfg.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		})
		flusher.Flush()
	}

	for i := 0; i < n; i++ {
		if r.Context().Err() != nil {
			return
		}
		delta := map[string]any{"content": replyTokens[i]}
		if i == 0 {
			delta["role"] = "assistant"
		}
		writeChunk(delta, nil)
	}
	writeChunk(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) nonStreamCompletion(w http.ResponseWriter, cfg Config, n int) {
	content := strings.Join(replyTokens[:n], "")
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   cfg.Model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": n,
			"total_tokens":      n + 1,
		},
	})
}

// checkReady 实现 ?ready_after=N：服务启动后的前 N 秒，models 与 completions 都返回 503，
// 并带 Retry-After。healthz 不受影响（进程是活的，只是后端还没就绪）。
func (s *Server) checkReady(w http.ResponseWriter, cfg Config) bool {
	if cfg.ReadyAfter <= 0 {
		return true
	}
	elapsed := time.Since(s.startedAt)
	if elapsed >= cfg.ReadyAfter {
		return true
	}
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "starting",
		fmt.Sprintf("mock-llm 还在启动中，约 %.1f 秒后就绪", (cfg.ReadyAfter-elapsed).Seconds()))
	return false
}

// sleep 实现 ?slow_ms=N：先等再回，模拟慢模型 / 慢首包。
func (s *Server) sleep(r *http.Request, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.Context().Done():
	}
}

func (s *Server) writeInjectedFailure(w http.ResponseWriter, status int) {
	code := "internal_error"
	switch status {
	case http.StatusBadRequest:
		code = "bad_request"
	case http.StatusUnauthorized:
		code = "invalid_api_key"
	case http.StatusForbidden:
		code = "permission_denied"
	case http.StatusNotFound:
		code = "model_not_found"
	case http.StatusTooManyRequests:
		code = "rate_limit_exceeded"
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		code = "service_unavailable"
	}
	w.Header().Set("X-Mock-Fail-Status", strconv.Itoa(status))
	writeError(w, status, code, fmt.Sprintf("mock-llm 注入的失败：HTTP %d", status))
}

// effectiveConfig 把查询参数叠加到服务默认配置上。控制参数写错（比如 fail=abc）
// 直接 400，避免 e2e 里拼错参数却悄悄测了个"正常路径"。
func (s *Server) effectiveConfig(r *http.Request) (Config, error) {
	cfg := s.cfg
	q := r.URL.Query()

	if v := q.Get("ready_after"); v != "" {
		d, err := parseSecondsOrDuration(v)
		if err != nil || d < 0 {
			return cfg, fmt.Errorf("ready_after 需要非负秒数或时长（如 3 或 3s），收到 %q", v)
		}
		cfg.ReadyAfter = d
	}
	if v := q.Get("fail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 || n > 599 {
			return cfg, fmt.Errorf("fail 需要 100..599 的 HTTP 状态码，收到 %q", v)
		}
		cfg.FailStatus = n
	}
	if v := q.Get("error_in_200"); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("error_in_200 需要 true/false/1/0，收到 %q", v)
		}
		cfg.ErrorIn200 = b
	}
	if v := q.Get("slow_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("slow_ms 需要非负整数，收到 %q", v)
		}
		cfg.Slow = time.Duration(n) * time.Millisecond
	}
	if v := q.Get("model"); v != "" {
		cfg.Model = v
	}
	return cfg, nil
}

// parseSecondsOrDuration 解析 "3"（秒）或 "3s" / "500ms"。
func parseSecondsOrDuration(v string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return time.ParseDuration(v)
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("不是布尔值：%q", v)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError 输出 OpenAI 风格的错误对象：{"error":{"message","type","code"}}。
// ?error_in_200=1 时同样用它，只是 HTTP 状态是 200（endpoint-probe 会 grep 到 "error"）。
func writeError(w http.ResponseWriter, status int, code, message string) {
	errType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		errType = "authentication_error"
	case status >= 500 && status != http.StatusOK:
		errType = "server_error"
	case status == http.StatusOK:
		errType = "gateway_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
			"param":   nil,
		},
	})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
		fmt.Sprintf("该路径只接受 %s", allow))
}

func writeSSE(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

// statusWriter 记录实际写出的状态码，只用于请求日志。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
