// Package mcp 实现 TestPlane 的 MCP（Model Context Protocol）server：
// stdio 传输 + JSON-RPC 2.0，把控制面的 REST 端点暴露成 9 个工具。
//
// 设计约束（详见 design/impl/agent-integration.md）：
//   - 不引入 MCP SDK：协议面只实现 initialize / tools/list / tools/call / ping；
//   - 薄 adapter：每个工具只做"取参数 → 调 REST → 原样或最小投影返回"，不新增业务语义；
//   - 不代理凭据：token 只用于本进程对控制面的调用，不出现在工具输出里；
//   - 不写文件、不执行任意命令。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Version 是 agent 适配层（MCP / CLI）的版本，不是控制面版本；
	// 控制面版本读 GET /api/v1/meta（见 design/impl/agent-integration.md §2）。
	Version = "0.1.0"

	// maxResponseBytes 是单次 REST 应答的读取上限。report.json 契约上限 20 MiB，
	// 这里留余量后拒绝，避免异常服务端把进程内存打满。
	maxResponseBytes = 32 << 20

	// defaultToken 与 dev 模式的约定一致（07 §2.1：开发模式无匿名端点，Bearer 值即 subject）。
	defaultToken = "dev"

	defaultHTTPTimeout = 30 * time.Second
)

// Client 是控制面 REST 的最小客户端：只覆盖 MCP 需要的提交 / 读取 / 取消。
// 它是**薄 adapter 的传输层**，不缓存、不重试、不解释业务字段。
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// streamHTTP 专用于 SSE（无 TotalTimeout，由 ctx 的 deadline 控制）。
	streamHTTP *http.Client
}

// NewClient 校验并构造客户端。baseURL 只接受 http/https（本地开发是明文）。
func NewClient(baseURL, token string, timeout time.Duration) (*Client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, errors.New("base-url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("base-url 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("base-url 只支持 http/https: %s", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("base-url 缺少主机名: %s", raw)
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	return &Client{
		BaseURL: strings.TrimRight(raw, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: timeout},
		// SSE 是长连接，不能吃 HTTP 超时；用独立 client，由请求 ctx 控制生死。
		streamHTTP: &http.Client{},
	}, nil
}

// Response 是一次 REST 调用的原始应答。非 2xx 时它仍会被返回（便于读 Retry-After 等头），
// 同时附带 *APIError。
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Get 发一个 GET；query 可为 nil。
func (c *Client) Get(ctx context.Context, path string, query url.Values) (*Response, error) {
	return c.do(ctx, http.MethodGet, path, query, nil, "", nil)
}

// GetWithHeaders 是带额外请求头的 GET（files 下载的 Range）。
func (c *Client) GetWithHeaders(ctx context.Context, path string, query url.Values, headers map[string]string) (*Response, error) {
	return c.do(ctx, http.MethodGet, path, query, nil, "", headers)
}

// Post 发一个 POST；body 为 nil 时表示空体（不带 Content-Type）。
func (c *Client) Post(ctx context.Context, path string, query url.Values, body []byte, contentType string) (*Response, error) {
	if contentType == "" {
		contentType = "application/json"
	}
	return c.do(ctx, http.MethodPost, path, query, body, contentType, nil)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, contentType string, headers map[string]string) (*Response, error) {
	if c == nil || c.HTTP == nil {
		return nil, errors.New("mcp: 未初始化控制面客户端")
	}
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("mcp: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenOrDefault(c.Token))
	req.Header.Set("Accept", "application/json, text/plain, text/markdown")
	req.Header.Set("User-Agent", "argus-mcp/"+Version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &TransportError{Method: method, URL: target, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &TransportError{Method: method, URL: target, Err: err}
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("mcp: %s %s 的应答超过 %d MiB 上限", method, target, maxResponseBytes>>20)
	}

	res := &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return res, newAPIError(res)
	}
	return res, nil
}

// GetTask 是 TaskReader 的实现：返回 GET /api/v1/tasks/{id} 的原始 JSON。
func (c *Client) GetTask(ctx context.Context, taskID string) ([]byte, error) {
	res, err := c.Get(ctx, "/api/v1/tasks/"+url.PathEscape(taskID), nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// OpenTaskEvents 打开 SSE（GET /api/v1/tasks/{id}/events），返回未读取的 body。
//
// 不可用（404/405/5xx/Content-Type 不是 text/event-stream/连接失败）→ *SSEUnavailableError，
// 由 Wait 降级为轮询；401/403 等鉴权错误 → *APIError，原样上抛（轮询也会失败）。
func (c *Client) OpenTaskEvents(ctx context.Context, taskID string, lastEventID int64) (io.ReadCloser, error) {
	if c == nil || c.streamHTTP == nil {
		return nil, errors.New("mcp: 未初始化 SSE 客户端")
	}
	target := c.BaseURL + "/api/v1/tasks/" + url.PathEscape(taskID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &SSEUnavailableError{Reason: "构造事件流请求失败", Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+tokenOrDefault(c.Token))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "argus-mcp/"+Version)
	if lastEventID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, &SSEUnavailableError{Reason: "连接事件流失败", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		err := apiErrorFromStream(resp)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, err
		}
		return nil, &SSEUnavailableError{Reason: "事件流不可用（HTTP " + strconv.Itoa(resp.StatusCode) + "）", Err: err}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		return nil, &SSEUnavailableError{Reason: "事件流不可用（Content-Type=" + ct + "）"}
	}
	return resp.Body, nil
}

// apiErrorFromStream 读一小段错误体并转成 *APIError（用于 SSE 端点的 4xx/5xx）。
func apiErrorFromStream(resp *http.Response) *APIError {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return newAPIError(&Response{Status: resp.StatusCode, Header: resp.Header, Body: data})
}

// APIError 是控制面错误信封（07 §3）的投影。非 JSON 的错误体（例如 chi 的 404 纯文本）
// 退化成 HTTP_<status> + 正文首部，保证 Agent 永远能拿到可读原因。
type APIError struct {
	Status     int
	Code       string
	Message    string
	Detail     json.RawMessage
	Retryable  bool
	TraceID    string
	RetryAfter string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	if e.Retryable {
		msg += "（可重试）"
	}
	if e.TraceID != "" {
		msg += " trace_id=" + e.TraceID
	}
	return msg
}

func newAPIError(res *Response) *APIError {
	e := &APIError{
		Status:     res.Status,
		Code:       fmt.Sprintf("HTTP_%d", res.Status),
		Message:    strings.TrimSpace(string(res.Body)),
		RetryAfter: res.Header.Get("Retry-After"),
	}
	var env struct {
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Detail    json.RawMessage `json:"detail"`
		Retryable bool            `json:"retryable"`
		TraceID   string          `json:"trace_id"`
	}
	if err := json.Unmarshal(res.Body, &env); err == nil && env.Code != "" {
		e.Code = env.Code
		e.Message = env.Message
		e.Detail = env.Detail
		e.Retryable = env.Retryable
		e.TraceID = env.TraceID
	}
	if e.Message == "" {
		e.Message = http.StatusText(res.Status)
	}
	e.Message = truncate(e.Message, 500)
	return e
}

// TransportError 表示"连不上控制面"（DNS / 连接拒绝 / 超时 / 读失败）；
// CLI 把它映射成退出码 4，MCP 把它映射成 code=UNREACHABLE 的 isError。
type TransportError struct {
	Method string
	URL    string
	Err    error
}

func (e *TransportError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s %s 不可达: %v", e.Method, e.URL, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// IsTransportError 判断错误是不是传输层失败（不是控制面返回的业务错误）。
func IsTransportError(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}

func tokenOrDefault(token string) string {
	if strings.TrimSpace(token) == "" {
		return defaultToken
	}
	return token
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
