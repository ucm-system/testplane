package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tool 是 tools/list 的一项（MCP 规范：name / description / inputSchema）。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolList 返回 9 个工具的静态清单。inputSchema 是 JSON Schema 2020-12 的子集
// （object / string / boolean / number / integer / enum / default / required），
// 与 design/impl/agent-integration.md §5 的表逐字对应。
func toolList() []Tool {
	return []Tool{
		{
			Name:        "testplane_submit_run",
			Description: "提交一个 Run（POST /api/v1/runs）。manifest 是 07 §4.1 的 YAML 或 JSON 原文（顶层 run:）；dry_run=true 只校验不落库。**不做幂等**（D-37）：重复调用会创建多个 Run。",
			InputSchema: schemaObject(map[string]any{
				"manifest": schemaString("Run Manifest 原文（YAML 或 JSON 对象，顶层是 run:）"),
				"dry_run":  schemaBoolean("true 时只校验、不落库（POST /runs?dry_run=true）", false),
			}, "manifest"),
		},
		{
			Name:        "testplane_get_run",
			Description: "读取 Run 详情（GET /api/v1/runs/{run_id}），含 status、task_counts、tasks 摘要与 error；不等待。",
			InputSchema: schemaObject(map[string]any{
				"run_id": schemaString("Run ID（不透明字符串，客户端不得解析）"),
			}, "run_id"),
		},
		{
			Name:        "testplane_wait_run",
			Description: "轮询 Run 到终态或超时（GET /api/v1/runs/{run_id}）。返回 outcome：success | failed | canceled | timeout；timeout 不是错误，只表示这次等待没等到终态，可再次调用。",
			InputSchema: schemaObject(map[string]any{
				"run_id":           schemaString("Run ID"),
				"timeout_seconds":  schemaNumber("总等待时长（秒），默认 3600", 3600, 1, 86400),
				"interval_seconds": schemaNumber("轮询间隔（秒），默认 3", 3, 0.01, 60),
			}, "run_id"),
		},
		{
			Name:        "testplane_cancel_run",
			Description: "取消 Run（POST /api/v1/runs/{run_id}/cancel，202/200 后回读确认）。已处于 cancelling/canceled 时是幂等成功（HTTP 200）。",
			InputSchema: schemaObject(map[string]any{
				"run_id": schemaString("Run ID"),
			}, "run_id"),
		},
		{
			Name:        "testplane_list_testers",
			Description: "列出 Tester 目录（GET /api/v1/testers）。parameters 是文档语义，平台不渲染、不校验；orphaned=true 可看到已从目录移除但历史 Run 仍引用的 Tester。",
			InputSchema: schemaObject(map[string]any{
				"role":     schemaEnum("按角色过滤", "test", "pre_check"),
				"orphaned": schemaBoolean("是否包含已从目录移除的 Tester", false),
			}),
		},
		{
			Name:        "testplane_get_report",
			Description: "取 Run 报告：format=json 走 GET /runs/{run_id}/report，format=markdown 走 GET /runs/{run_id}/report.md。报告在 run.finished 后异步生成，未生成时返回 NOT_FOUND（带 retry_after）。",
			InputSchema: schemaObject(map[string]any{
				"run_id": schemaString("Run ID"),
				"format": schemaEnum("报告形态，默认 json", "json", "markdown"),
			}, "run_id"),
		},
		{
			Name:        "testplane_get_logs",
			Description: "读取 Attempt 日志（GET /tasks/{task_id}/attempts/{idx}/logs）。返回 JSON：text 是日志原文，next_offset 是响应头 X-Log-Offset（下次续读从这里开始），coverage 是 X-Log-Coverage。",
			InputSchema: schemaObject(map[string]any{
				"task_id": schemaString("Task ID"),
				"attempt": schemaInteger("Attempt 序号（从 1 开始），默认 1", 1, 1, 1000),
				"stream":  schemaEnum("stdout 或 stderr；缺省为 stdout", "stdout", "stderr"),
				"offset":  schemaInteger("起始字节偏移（缺省从头）", 0, 0, 0),
				"limit":   schemaInteger("本次返回的字节上限（服务端再收敛到 ≤64 KiB）", 0, 1, 65536),
				"tail":    schemaInteger("只取最后 N 行（与 offset 互斥，服务端以 tail 为准）", 0, 1, 1000000),
			}, "task_id"),
		},
		{
			Name:        "testplane_queue_status",
			Description: "平台槽位 + 平台侧等待原因 + Volcano 只读快照（GET /api/v1/queue-status）。三者语义不同，**不可相加**成一个排队数。",
			InputSchema: schemaObject(nil),
		},
		{
			Name:        "testplane_list_profiles",
			Description: "列出 Execution Profile（GET /api/v1/profiles）。注意：控制面 MVP 尚未实现该端点（IMPLEMENTATION §15.2），当前调用会返回 NOT_FOUND。",
			InputSchema: schemaObject(nil),
		},
	}
}

func schemaObject(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func schemaString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func schemaBoolean(desc string, def bool) map[string]any {
	return map[string]any{"type": "boolean", "description": desc, "default": def}
}

func schemaNumber(desc string, def, min, max float64) map[string]any {
	return map[string]any{
		"type": "number", "description": desc, "default": def, "minimum": min, "maximum": max,
	}
}

func schemaInteger(desc string, def, min, max int64) map[string]any {
	s := map[string]any{"type": "integer", "description": desc, "minimum": min}
	if def > 0 {
		s["default"] = def
	}
	if max > 0 {
		s["maximum"] = max
	}
	return s
}

func schemaEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// ---- tools/call 分发 ------------------------------------------------------

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, *rpcError) {
	if s.client == nil {
		return errorResult("MCP server 未配置控制面客户端（--base-url 无效？）"), nil
	}
	switch name {
	case "testplane_submit_run":
		return s.toolSubmitRun(ctx, args)
	case "testplane_get_run":
		return s.toolGetRun(ctx, args)
	case "testplane_wait_run":
		return s.toolWaitRun(ctx, args)
	case "testplane_cancel_run":
		return s.toolCancelRun(ctx, args)
	case "testplane_list_testers":
		return s.toolListTesters(ctx, args)
	case "testplane_get_report":
		return s.toolGetReport(ctx, args)
	case "testplane_get_logs":
		return s.toolGetLogs(ctx, args)
	case "testplane_queue_status":
		return s.toolQueueStatus(ctx, args)
	case "testplane_list_profiles":
		return s.toolListProfiles(ctx, args)
	default:
		return nil, invalidParams("未知工具: " + name)
	}
}

func (s *Server) toolSubmitRun(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Manifest string `json:"manifest"`
		DryRun   bool   `json:"dry_run"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_submit_run: " + err.Error())
	}
	if strings.TrimSpace(a.Manifest) == "" {
		return nil, invalidParams("testplane_submit_run: manifest 不能为空")
	}

	q := url.Values{}
	if a.DryRun {
		q.Set("dry_run", "true")
	}
	contentType := "application/yaml"
	if looksLikeJSON(a.Manifest) {
		contentType = "application/json"
	}

	res, err := s.client.Post(ctx, "/api/v1/runs", q, []byte(a.Manifest), contentType)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolGetRun(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		RunID string `json:"run_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_get_run: " + err.Error())
	}
	if strings.TrimSpace(a.RunID) == "" {
		return nil, invalidParams("testplane_get_run: run_id 不能为空")
	}
	res, err := s.client.Get(ctx, "/api/v1/runs/"+pathEscape(a.RunID), nil)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolWaitRun(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		RunID           string  `json:"run_id"`
		TimeoutSeconds  float64 `json:"timeout_seconds"`
		IntervalSeconds float64 `json:"interval_seconds"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_wait_run: " + err.Error())
	}
	if strings.TrimSpace(a.RunID) == "" {
		return nil, invalidParams("testplane_wait_run: run_id 不能为空")
	}
	timeout := a.TimeoutSeconds
	if timeout == 0 {
		timeout = 3600
	}
	if timeout < 0 || timeout > 86400 {
		return nil, invalidParams("testplane_wait_run: timeout_seconds 必须在 1..86400 之间")
	}
	interval := a.IntervalSeconds
	if interval == 0 {
		interval = 3
	}
	if interval <= 0 || interval < 0.01 || interval > 60 {
		return nil, invalidParams("testplane_wait_run: interval_seconds 必须在 0.01..60 之间")
	}

	started := time.Now()
	deadline := started.Add(time.Duration(timeout * float64(time.Second)))
	path := "/api/v1/runs/" + pathEscape(a.RunID)

	var last runSnapshot
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		reqCtx, cancel := context.WithTimeout(ctx, remaining)
		res, err := s.client.Get(reqCtx, path, nil)
		cancel()
		if err != nil {
			if time.Now().After(deadline) || (errors.Is(err, context.DeadlineExceeded) && !time.Now().Before(deadline)) {
				break
			}
			return restError(err), nil
		}
		snap, perr := parseRunSnapshot(res.Body)
		if perr != nil {
			return errorResult("控制面返回的 Run 不是合法 JSON: " + perr.Error()), nil
		}
		last = snap

		terminal := isTerminalRunStatus(snap.Status)
		if terminal || !time.Now().Before(deadline) {
			outcome := runOutcome(snap.Status)
			if !terminal {
				outcome = "timeout"
			}
			return jsonResult(map[string]any{
				"run_id":          a.RunID,
				"status":          snap.Status,
				"terminal":        terminal,
				"outcome":         outcome,
				"task_counts":     rawOrNil(snap.TaskCounts),
				"error":           rawOrNil(snap.Error),
				"elapsed_seconds": roundSeconds(time.Since(started)),
			}), nil
		}

		wait := time.Duration(interval * float64(time.Second))
		if rest := time.Until(deadline); rest < wait {
			wait = rest
		}
		select {
		case <-ctx.Done():
			return errorResult("等待被取消: " + ctx.Err().Error()), nil
		case <-time.After(wait):
		}
	}

	return jsonResult(map[string]any{
		"run_id":          a.RunID,
		"status":          last.Status,
		"terminal":        false,
		"outcome":         "timeout",
		"task_counts":     rawOrNil(last.TaskCounts),
		"error":           rawOrNil(last.Error),
		"elapsed_seconds": roundSeconds(time.Since(started)),
	}), nil
}

func (s *Server) toolCancelRun(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		RunID string `json:"run_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_cancel_run: " + err.Error())
	}
	if strings.TrimSpace(a.RunID) == "" {
		return nil, invalidParams("testplane_cancel_run: run_id 不能为空")
	}
	path := "/api/v1/runs/" + pathEscape(a.RunID)
	res, err := s.client.Post(ctx, path+"/cancel", nil, nil, "")
	if err != nil {
		return restError(err), nil
	}

	out := map[string]any{
		"run_id":      a.RunID,
		"accepted":    true,
		"http_status": res.Status,
		"confirmed":   false,
	}
	if confirm, err := s.client.Get(ctx, path, nil); err == nil {
		if snap, perr := parseRunSnapshot(confirm.Body); perr == nil {
			out["status"] = snap.Status
			out["confirmed"] = true
		}
	} else if snap, perr := parseRunSnapshot(res.Body); perr == nil && snap.Status != "" {
		out["status"] = snap.Status
	}
	return jsonResult(out), nil
}

func (s *Server) toolListTesters(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Role     string `json:"role"`
		Orphaned bool   `json:"orphaned"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_list_testers: " + err.Error())
	}
	q := url.Values{}
	if a.Role != "" {
		if a.Role != "test" && a.Role != "pre_check" {
			return nil, invalidParams("testplane_list_testers: role 只能是 test|pre_check")
		}
		q.Set("role", a.Role)
	}
	if a.Orphaned {
		q.Set("orphaned", "true")
	}
	res, err := s.client.Get(ctx, "/api/v1/testers", q)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolGetReport(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		RunID  string `json:"run_id"`
		Format string `json:"format"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_get_report: " + err.Error())
	}
	if strings.TrimSpace(a.RunID) == "" {
		return nil, invalidParams("testplane_get_report: run_id 不能为空")
	}
	format := strings.ToLower(strings.TrimSpace(a.Format))
	path := "/api/v1/runs/" + pathEscape(a.RunID) + "/report"
	switch format {
	case "", "json":
	case "markdown", "md":
		path += ".md"
	default:
		return nil, invalidParams("testplane_get_report: format 只能是 json|markdown")
	}

	res, err := s.client.Get(ctx, path, nil)
	if err != nil {
		return restError(err), nil
	}
	if format == "markdown" || format == "md" {
		return textResult(string(res.Body)), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolGetLogs(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID  string `json:"task_id"`
		Attempt int    `json:"attempt"`
		Stream  string `json:"stream"`
		Offset  *int64 `json:"offset"`
		Limit   *int64 `json:"limit"`
		Tail    *int   `json:"tail"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("testplane_get_logs: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("testplane_get_logs: task_id 不能为空")
	}
	if a.Attempt == 0 {
		a.Attempt = 1
	}
	if a.Attempt < 1 {
		return nil, invalidParams("testplane_get_logs: attempt 必须 ≥1")
	}
	if a.Stream != "" && a.Stream != "stdout" && a.Stream != "stderr" {
		return nil, invalidParams("testplane_get_logs: stream 只能是 stdout|stderr")
	}
	q := url.Values{}
	if a.Stream != "" {
		q.Set("stream", a.Stream)
	}
	offset := int64(0)
	if a.Offset != nil {
		if *a.Offset < 0 {
			return nil, invalidParams("testplane_get_logs: offset 必须 ≥0")
		}
		offset = *a.Offset
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > 65536 {
			return nil, invalidParams("testplane_get_logs: limit 必须在 1..65536 之间")
		}
		q.Set("limit", strconv.FormatInt(*a.Limit, 10))
	}
	if a.Tail != nil {
		if *a.Tail < 1 {
			return nil, invalidParams("testplane_get_logs: tail 必须 ≥1")
		}
		q.Set("tail", strconv.Itoa(*a.Tail))
	}

	path := fmt.Sprintf("/api/v1/tasks/%s/attempts/%d/logs", pathEscape(a.TaskID), a.Attempt)
	res, err := s.client.Get(ctx, path, q)
	if err != nil {
		return restError(err), nil
	}

	stream := a.Stream
	if stream == "" {
		stream = "stdout"
	}
	out := map[string]any{
		"task_id":  a.TaskID,
		"attempt":  a.Attempt,
		"stream":   stream,
		"offset":   offset,
		"coverage": res.Header.Get("X-Log-Coverage"),
		"text":     string(res.Body),
	}
	if a.Tail != nil {
		out["tail"] = *a.Tail
	}
	if rawOffset := res.Header.Get("X-Log-Offset"); rawOffset != "" {
		if next, perr := strconv.ParseInt(strings.TrimSpace(rawOffset), 10, 64); perr == nil {
			out["next_offset"] = next
		} else {
			out["next_offset_raw"] = rawOffset
		}
	}
	return jsonResult(out), nil
}

func (s *Server) toolQueueStatus(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	res, err := s.client.Get(ctx, "/api/v1/queue-status", nil)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolListProfiles(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	res, err := s.client.Get(ctx, "/api/v1/profiles", nil)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return isErrorJSON(map[string]any{"error": map[string]any{
				"http_status": ae.Status,
				"code":        "ENDPOINT_NOT_IMPLEMENTED",
				"message":     "控制面尚未实现 GET /api/v1/profiles（IMPLEMENTATION §15.2「P1 首批」）；当前没有可替代端点，不伪造数据。",
			}}), nil
		}
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

// ---- 结果与错误 -----------------------------------------------------------

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentItem{{Type: "text", Text: text}}}
}

func jsonResult(v any) toolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult("编码工具结果失败: " + err.Error())
	}
	return textResult(string(b))
}

func errorResult(reason string) toolResult {
	return toolResult{IsError: true, Content: []contentItem{{Type: "text", Text: reason}}}
}

func isErrorJSON(v any) toolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult("编码错误结果失败: " + err.Error())
	}
	return toolResult{IsError: true, Content: []contentItem{{Type: "text", Text: string(b)}}}
}

func jsonBodyResult(body []byte) (any, *rpcError) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return errorResult("控制面返回了非 JSON 应答: " + truncate(string(body), 200)), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult("编码应答失败: " + err.Error()), nil
	}
	return textResult(string(b)), nil
}

// restError 把 REST 错误映射成 MCP 的 isError 结果（协议层保持成功）：
// 控制面错误信封 -> {error:{http_status,code,message,retryable,trace_id,retry_after}}；
// 传输失败 -> code=UNREACHABLE。这样 Agent 能读到结构化原因，而不是一句纯文本。
func restError(err error) toolResult {
	var ae *APIError
	if errors.As(err, &ae) {
		e := map[string]any{
			"http_status": ae.Status,
			"code":        ae.Code,
			"message":     ae.Message,
			"retryable":   ae.Retryable,
		}
		if detail := rawOrNil(ae.Detail); detail != nil {
			e["detail"] = detail
		}
		if ae.TraceID != "" {
			e["trace_id"] = ae.TraceID
		}
		if ae.RetryAfter != "" {
			e["retry_after"] = ae.RetryAfter
		}
		return isErrorJSON(map[string]any{"error": e})
	}
	if IsTransportError(err) {
		return isErrorJSON(map[string]any{"error": map[string]any{
			"code":      "UNREACHABLE",
			"message":   err.Error(),
			"retryable": true,
		}})
	}
	return errorResult(err.Error())
}

// ---- 小工具 ---------------------------------------------------------------

// runSnapshot 是 wait / cancel 回读需要的最小 Run 投影（其余字段不透传）。
type runSnapshot struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	TaskCounts json.RawMessage `json:"task_counts"`
	Error      json.RawMessage `json:"error"`
}

func parseRunSnapshot(body []byte) (runSnapshot, error) {
	var snap runSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return runSnapshot{}, err
	}
	return snap, nil
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "succeeded", "completed_with_errors", "service_unavailable", "failed", "canceled":
		return true
	default:
		return false
	}
}

// runOutcome 把终态映射成跨入口一致的 outcome（CLI 退出码 0/1/2/3 与它一一对应）。
func runOutcome(status string) string {
	switch status {
	case "succeeded":
		return "success"
	case "canceled":
		return "canceled"
	case "completed_with_errors", "service_unavailable", "failed":
		return "failed"
	default:
		return "running"
	}
}

func decodeArgs(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("参数必须是单个 JSON 对象")
	}
	return nil
}

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

func pathEscape(id string) string { return url.PathEscape(id) }

func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	return raw
}

func roundSeconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*1000) / 1000
}
