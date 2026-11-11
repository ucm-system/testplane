package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ClientConfig 是一次 e2e 运行对控制面的连接参数。
type ClientConfig struct {
	BaseURL string // --base-url，默认 http://127.0.0.1:8080
	Prefix  string // --api-prefix，默认 /api/v1
	User    string // dev 模式身份；dev 鉴权口径：Bearer <subject> 即用户名
	Token   string // 显式 Bearer，优先于 User
	NoAuth  bool   // 不发 Authorization
	Verbose bool
	Logf    func(format string, args ...any)
}

type Client struct {
	cfg ClientConfig
	hc  *http.Client
}

func NewClient(cfg ClientConfig) *Client {
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: 30 * time.Second},
	}
}

// URL 拼出 base + prefix + path，容错末尾斜杠。
func (c *Client) URL(path string) string {
	base := strings.TrimRight(c.cfg.BaseURL, "/")
	prefix := "/" + strings.Trim(c.cfg.Prefix, "/")
	if prefix == "/" {
		prefix = ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + prefix + path
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "argus-e2e/0.2")
	if c.cfg.NoAuth {
		return
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		return
	}
	if c.cfg.User != "" {
		// MVP dev 鉴权：Bearer <subject>，subject 即用户名；不带则服务端用 dev。
		req.Header.Set("Authorization", "Bearer "+c.cfg.User)
	}
}

// Get 发一个 GET，返回状态码、头与完整响应体。
func (c *Client) Get(ctx context.Context, path string) (int, http.Header, []byte, error) {
	return c.do(ctx, http.MethodGet, path, nil, "")
}

// PostJSON 发一个 POST；payload 原样发送，contentType 由调用方决定
// （Manifest 可以是 application/yaml 或 application/json）。
func (c *Client) Post(ctx context.Context, path string, payload []byte, contentType string) (int, http.Header, []byte, error) {
	return c.do(ctx, http.MethodPost, path, payload, contentType)
}

func (c *Client) do(ctx context.Context, method, path string, payload []byte, contentType string) (int, http.Header, []byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL(path), body)
	if err != nil {
		return 0, nil, nil, &UsageError{Msg: fmt.Sprintf("构造请求失败（%s %s）：%v", method, path, err)}
	}
	if payload != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.authorize(req)
	if c.cfg.Verbose && c.cfg.Logf != nil {
		c.cfg.Logf("→ %s %s", method, req.URL)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, fmt.Errorf("请求超时或被取消（%s %s）：%w", method, path, err)
		}
		return 0, nil, nil, &ConnectError{BaseURL: c.cfg.BaseURL, Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, raw, fmt.Errorf("读取响应失败（%s %s）：%w", method, path, err)
	}
	if c.cfg.Verbose && c.cfg.Logf != nil {
		c.cfg.Logf("← %d %s（%d 字节）", resp.StatusCode, path, len(raw))
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// Stream 建立 SSE 长连接。超时完全由 ctx 控制（不设 http.Client.Timeout，
// 否则 15s 心跳之前就会被掐断）。
func (c *Client) Stream(ctx context.Context, path string, extraHeaders map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL(path), nil)
	if err != nil {
		return nil, &UsageError{Msg: fmt.Sprintf("构造 SSE 请求失败（%s）：%v", path, err)}
	}
	c.authorize(req)
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if c.cfg.Verbose && c.cfg.Logf != nil {
		c.cfg.Logf("→ GET %s（SSE）", req.URL)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("SSE 请求超时/被取消：%w", err)
		}
		return nil, &ConnectError{BaseURL: c.cfg.BaseURL, Err: err}
	}
	return resp, nil
}

// ConnectError 是"连不上控制面"，与"接口返回了错误"严格区分：
// 前者退出码 3（并明确提示 8080 可能没起），后者是断言失败（退出码 1）。
type ConnectError struct {
	BaseURL string
	Err     error
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf(`无法连接控制面 %s：%v
  请先确认控制面已启动：make dev（PG + 迁移 + --role=all，监听 127.0.0.1:8080），
  或改用 --base-url 指向实际地址。本次没有创建任何 Task。`, e.BaseURL, e.Err)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// UsageError 是参数/配置错误，退出码 2。
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// APIError 是控制面返回的非 2xx 响应，错误模型见 07 §3：
// 扁平 {code,message,detail,retryable,trace_id}；也容忍嵌套 {error:{...}} 形态读取。
type APIError struct {
	Status    int
	Code      string
	Message   string
	Detail    map[string]any
	Retryable *bool
	TraceID   string
	Raw       []byte
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d", e.Status)
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, "：%s", e.Message)
	}
	if e.TraceID != "" {
		fmt.Fprintf(&b, "（trace_id=%s）", e.TraceID)
	}
	if e.Detail != nil {
		if d, err := json.Marshal(e.Detail); err == nil && len(d) > 2 {
			fmt.Fprintf(&b, " detail=%s", truncate(string(d), 600))
		}
	}
	if b.Len() == 0 || (e.Code == "" && e.Message == "") {
		fmt.Fprintf(&b, "：%s", truncate(string(e.Raw), 600))
	}
	return b.String()
}

// IsConnectError 供上层决定退出码。
func IsConnectError(err error) bool {
	var ce *ConnectError
	return errors.As(err, &ce)
}

func apiError(status int, raw []byte) *APIError {
	e := &APIError{Status: status, Raw: raw}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return e
	}
	body := envelope
	if nested, ok := envelope["error"].(map[string]any); ok {
		body = nested
	}
	if v, ok := body["code"].(string); ok {
		e.Code = v
	}
	if v, ok := body["message"].(string); ok {
		e.Message = v
	}
	if v, ok := body["detail"].(map[string]any); ok {
		e.Detail = v
	}
	if v, ok := body["retryable"].(bool); ok {
		e.Retryable = &v
	}
	if v, ok := body["trace_id"].(string); ok {
		e.TraceID = v
	}
	if e.Code == "" {
		if v, ok := envelope["code"].(string); ok {
			e.Code = v
		}
	}
	return e
}

// expectStatus 统一处理"期望某个状态码"的分支：2xx 之外的响应一律转成 APIError。
func expectStatus(status, want int, raw []byte) error {
	if status == want {
		return nil
	}
	return apiError(status, raw)
}

// expectAnyStatus 接受一组状态码（例如 dry-run 的 200、report 未就绪的 404）。
func expectAnyStatus(status int, want []int, raw []byte) error {
	for _, w := range want {
		if status == w {
			return nil
		}
	}
	return apiError(status, raw)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…（截断，共 " + strconv.Itoa(len(s)) + " 字节）"
}

// retryAfterSeconds 解析 404 + Retry-After（07 §5.1：报告未就绪时服务端返回 Retry-After）。
func retryAfterSeconds(header http.Header, fallback, max time.Duration) time.Duration {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return fallback
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil {
		if secs > max {
			return max
		}
		if secs < 0 {
			return fallback
		}
		return secs
	}
	return fallback
}
