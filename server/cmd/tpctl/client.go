package main

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

	"github.com/ucm-system/testplane/server/internal/mcp"
)

// 本文件是 tpctl 自己的最小 REST 客户端：只做"拼 URL + Bearer + 超时 + 错误信封解析"。
// 刻意不 import server/internal/api（那是服务端 handler/DTO 的包），也不引入任何 CLI 框架——
// 命令与 REST 端点一一对应，语义全部来自 API 契约（见 design/impl/agent-integration.md §3）。

const (
	// cliVersion 是 agent 适配层版本，不是控制面版本（控制面版本读 GET /api/v1/meta）。
	cliVersion = "0.1.0"

	maxResponseBytes = 32 << 20
)

type client struct {
	baseURL string
	token   string
	http    *http.Client

	// streamHTTP 专用于 SSE（无 TotalTimeout，由 ctx 的 deadline 控制）。
	streamHTTP *http.Client
}

func newClient(baseURL, token string, timeout time.Duration) (*client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, errors.New("base-url 不能为空（用 --base-url 或 TESTPLANE_BASE_URL）")
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
		timeout = 30 * time.Second
	}
	return &client{
		baseURL:    strings.TrimRight(raw, "/"),
		token:      token,
		http:       &http.Client{Timeout: timeout},
		streamHTTP: &http.Client{},
	}, nil
}

// apiResult 是一次 REST 调用的原始应答（非 2xx 时同时返回 *apiError，便于读 Retry-After）。
type apiResult struct {
	Status int
	Header http.Header
	Body   []byte
}

// apiError 是控制面错误信封（07 §3）的投影；非 JSON 错误体退化成 HTTP_<status>。
type apiError struct {
	Status     int
	Code       string
	Message    string
	Detail     json.RawMessage
	Retryable  bool
	TraceID    string
	RetryAfter string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	if e.Retryable {
		msg += "（可重试）"
	}
	if e.TraceID != "" {
		msg += " trace_id=" + e.TraceID
	}
	return msg
}

// transportError 表示"连不上控制面"（DNS / 连接拒绝 / 超时），CLI 映射成退出码 4。
type transportError struct {
	method string
	url    string
	err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("%s %s 不可达: %v", e.method, e.url, e.err)
}

func (e *transportError) Unwrap() error { return e.err }

func (c *client) get(ctx context.Context, path string, query url.Values) (*apiResult, error) {
	return c.do(ctx, http.MethodGet, path, query, nil, "", nil)
}

// getWithHeaders 是带额外请求头的 GET（小响应，如 32 MiB 内的 MCP 摘要）。
func (c *client) getWithHeaders(ctx context.Context, path string, query url.Values, headers map[string]string) (*apiResult, error) {
	return c.do(ctx, http.MethodGet, path, query, nil, "", headers)
}

// getStream 打开一个不缓冲整段 body 的 GET（files 下载用：单文件可到 1 GiB）。
// 2xx 时返回未读取的 *http.Response（调用方负责 Close）；非 2xx 转成 *apiError。
// 用无 TotalTimeout 的 streamHTTP，超时由调用方的 ctx 控制。
func (c *client) getStream(ctx context.Context, path string, query url.Values, headers map[string]string) (*http.Response, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenOrDefault(c.token))
	req.Header.Set("User-Agent", "testplane-tpctl/"+cliVersion)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, &transportError{method: http.MethodGet, url: target, err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, newAPIError(&apiResult{Status: resp.StatusCode, Header: resp.Header, Body: body})
	}
	return resp, nil
}

func (c *client) post(ctx context.Context, path string, query url.Values, body []byte, contentType string) (*apiResult, error) {
	if contentType == "" {
		contentType = "application/json"
	}
	return c.do(ctx, http.MethodPost, path, query, body, contentType, nil)
}

func (c *client) do(ctx context.Context, method, path string, query url.Values, body []byte, contentType string, headers map[string]string) (*apiResult, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenOrDefault(c.token))
	req.Header.Set("Accept", "application/json, text/plain, text/markdown")
	req.Header.Set("User-Agent", "testplane-tpctl/"+cliVersion)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &transportError{method: method, url: target, err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &transportError{method: method, url: target, err: err}
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%s %s 的应答超过 %d MiB 上限", method, target, maxResponseBytes>>20)
	}

	res := &apiResult{Status: resp.StatusCode, Header: resp.Header, Body: data}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return res, newAPIError(res)
	}
	return res, nil
}

// GetRun 实现 mcp.RunReader：GET /api/v1/runs/{id} 的原始 JSON。
func (c *client) GetRun(ctx context.Context, runID string) ([]byte, error) {
	res, err := c.get(ctx, "/api/v1/runs/"+url.PathEscape(runID), nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// OpenRunEvents 实现 mcp.RunReader：打开 SSE。不可用时返回 mcp.SSEUnavailableError，
// 由 mcp.Wait 自动降级为轮询；401/403 返回 *apiError（致命，不降级）。
func (c *client) OpenRunEvents(ctx context.Context, runID string, lastEventID int64) (io.ReadCloser, error) {
	if c.streamHTTP == nil {
		return nil, &mcp.SSEUnavailableError{Reason: "未初始化 SSE 客户端"}
	}
	target := c.baseURL + "/api/v1/runs/" + url.PathEscape(runID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &mcp.SSEUnavailableError{Reason: "构造事件流请求失败", Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+tokenOrDefault(c.token))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "testplane-tpctl/"+cliVersion)
	if lastEventID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, &mcp.SSEUnavailableError{Reason: "连接事件流失败", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		err := newAPIError(&apiResult{Status: resp.StatusCode, Header: resp.Header, Body: body})
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, err
		}
		return nil, &mcp.SSEUnavailableError{Reason: "事件流不可用（HTTP " + strconv.Itoa(resp.StatusCode) + "）", Err: err}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		return nil, &mcp.SSEUnavailableError{Reason: "事件流不可用（Content-Type=" + ct + "）"}
	}
	return resp.Body, nil
}

func newAPIError(res *apiResult) *apiError {
	e := &apiError{
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
	e.Message = truncateRunes(e.Message, 500)
	return e
}

func tokenOrDefault(token string) string {
	if strings.TrimSpace(token) == "" {
		return "dev"
	}
	return token
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
