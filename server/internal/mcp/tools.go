package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"unicode/utf8"
)

// Tool 是 tools/list 的一项（MCP 规范：name / description / inputSchema）。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolCount 是 tools/list 的工具数（由静态清单推导，不双写；/meta 的 agent_caps 引用）。
func ToolCount() int { return len(toolList()) }

// toolList 返回 13 个工具的静态清单（agent-integration.md §5.3/§5.4，全部 task_id 化）。
// inputSchema 是 JSON Schema 2020-12 的子集，additionalProperties=false：
// 拼错的参数会被 -32602 拒绝，而不是被静默忽略。
func toolList() []Tool {
	return []Tool{
		{
			Name:        "argus_submit_task",
			Description: "提交 Task（POST /api/v1/tasks）。manifest 是 YAML 或 JSON 原文（顶层 task:；deploy XOR service）。dry_run=true 只校验不落库。**不做幂等**（D-37）：重复调用会创建多个 Task。",
			InputSchema: schemaObject(map[string]any{
				"manifest": schemaString("Task Manifest 原文（YAML 或 JSON 对象，顶层是 task:；deploy XOR service）"),
				"dry_run":  schemaBoolean("true 时只校验、不落库", false),
			}, "manifest"),
		},
		{
			Name:        "argus_get_task",
			Description: "读取 Task 详情（GET /api/v1/tasks/{task_id}），含 status、runner_counts、attempts 与最新 attempt 的 runners；不等待。",
			InputSchema: schemaObject(map[string]any{
				"task_id": schemaString("Task ID（不透明字符串，客户端不得解析）"),
			}, "task_id"),
		},
		{
			Name:        "argus_wait_task",
			Description: "轮询/订阅 Task 到终态或超时。follow=true 时先用 SSE（GET /tasks/{task_id}/events，按 Last-Event-ID 续传、断线重连 3 次），SSE 不可用自动降级轮询，结果里 wait_mode 标明实际方式。返回 outcome：success | failed | canceled | timeout；timeout 不是错误。",
			InputSchema: schemaObject(map[string]any{
				"task_id":          schemaString("Task ID"),
				"timeout_seconds":  schemaNumber("总等待时长（秒），默认 3600", 3600, 0.1, 86400),
				"interval_seconds": schemaNumber("轮询间隔（秒），默认 3；follow=true 时只在降级后使用", 3, 0.01, 60),
				"follow":           schemaBoolean("true 时优先用 SSE 事件流等待（服务端侧），仍一次返回", false),
			}, "task_id"),
		},
		{
			Name:        "argus_cancel_task",
			Description: "取消 Task（POST /api/v1/tasks/{task_id}/cancel，202/200 后回读确认）。取消会先执行 service-clean 再落 canceled（C-6.1）；已在 cancelling/canceled 时是幂等成功（HTTP 200）。",
			InputSchema: schemaObject(map[string]any{
				"task_id": schemaString("Task ID"),
			}, "task_id"),
		},
		{
			Name:        "argus_retry_task",
			Description: "整 Task 重试（POST /api/v1/tasks/{task_id}/retry）：新 attempt 全量重跑含 fresh deploy（部署模式会重新拉起服务）。前置 Task 终态；attempt 达上限返回 409 ATTEMPT_EXHAUSTED。",
			InputSchema: schemaObject(map[string]any{
				"task_id": schemaString("Task ID"),
			}, "task_id"),
		},
		{
			Name:        "argus_clean_task",
			Description: "泄漏重放（POST /api/v1/tasks/{task_id}/clean，§9.3）：用落档导出重拉一次 service-clean Job。attempt=0（默认）取最后一个 attempt；幂等；409 带 detail.reason ∈ {no_deploy, nothing_to_clean, clean_already_verified, task_running, clean_replay_in_progress}（nothing_to_clean = provider=existing 的登记型 Task，无可拆除对象）。",
			InputSchema: schemaObject(map[string]any{
				"task_id": schemaString("Task ID"),
				"attempt": schemaInteger("要重放 clean 的 attempt 序号；0 = 最后一个 attempt", 0, 0, 1000),
			}, "task_id"),
		},
		{
			Name:        "argus_list_runners",
			Description: "列出 Runner 目录（GET /api/v1/runners）。parameters 是 jsonl 输入的结构校验 schema（类型 + required）；exports/consumes 是组合期校验依据；orphaned=true 可看到已从目录移除但历史 Task 仍引用的 Runner。",
			InputSchema: schemaObject(map[string]any{
				"role":     schemaEnum("按角色过滤", "test", "pre_check"),
				"orphaned": schemaBoolean("是否包含已从目录移除的 Runner", false),
			}),
		},
		{
			Name:        "argus_get_report",
			Description: "取 Task 报告：format=json 走 GET /tasks/{task_id}/report，format=markdown 走 GET /tasks/{task_id}/report.md。报告在 Task 终态后异步生成，未生成时返回 NOT_FOUND（带 retry_after）。",
			InputSchema: schemaObject(map[string]any{
				"task_id": schemaString("Task ID"),
				"format":  schemaEnum("报告形态，默认 json", "json", "markdown"),
			}, "task_id"),
		},
		{
			Name:        "argus_get_logs",
			Description: "读取 runner 执行日志（GET /tasks/{task_id}/attempts/{n}/runners/{seq}/logs）。runner_seq 缺省取管道第一个失败 runner；返回 JSON：text 是日志原文，next_offset 是 X-Log-Offset（续读起点）。",
			InputSchema: schemaObject(map[string]any{
				"task_id":    schemaString("Task ID"),
				"attempt":    schemaInteger("attempt 序号（从 1 开始），默认 1", 1, 1, 1000),
				"runner_seq": schemaInteger("runner 执行序（从 1 开始）；缺省取第一个失败 runner", 0, 1, 1000),
				"stream":     schemaEnum("stdout 或 stderr；缺省 stdout", "stdout", "stderr"),
				"offset":     schemaInteger("起始字节偏移（缺省从头）", 0, 0, 0),
				"limit":      schemaInteger("本次返回的字节上限（服务端收敛到 ≤64 KiB）", 0, 1, 65536),
				"tail":       schemaInteger("只取最后 N 行（与 offset 互斥）", 0, 1, 1000000),
			}, "task_id"),
		},
		{
			Name:        "argus_list_artifacts",
			Description: "读取产物清单：只给 task_id 走 Task 级（GET /tasks/{id}/artifacts，含 exports 落档）；给 task_id+attempt+runner_seq 走 runner 级（GET /tasks/{id}/attempts/{n}/runners/{seq}/artifacts）。返回 {items[], by_kind, total_size_bytes}；path 可直接给 argus_get_file。",
			InputSchema: schemaObject(map[string]any{
				"task_id":    schemaString("Task ID"),
				"attempt":    schemaInteger("attempt 序号（runner 级时使用），默认 1", 1, 1, 1000),
				"runner_seq": schemaInteger("runner 执行序（给定时返回 runner 级清单）", 0, 1, 1000),
			}, "task_id"),
		},
		{
			Name:        "argus_get_file",
			Description: "读取 Task 产物文件（GET /tasks/{task_id}/files?path=）并返回**摘要**：path、size、sha256、前 2KB 文本或 base64 标记。默认最多取 1 MiB（Range 前缀），不会把二进制整个塞进 content；完整文件走 HTTP / CLI 下载。",
			InputSchema: schemaObject(map[string]any{
				"task_id":   schemaString("Task ID"),
				"path":      schemaString("产物路径；直接给 argus_list_artifacts 返回的 path（含 <task_id>/ 前缀会自动剥离）"),
				"range":     schemaString("可选 HTTP Range（如 bytes=0-1048575）；缺省按 max_bytes 取前缀"),
				"max_bytes": schemaInteger("未给 range 时最多读取的字节数，默认 1 MiB", 1048576, 1024, 33554432),
			}, "task_id", "path"),
		},
		{
			Name:        "argus_queue_status",
			Description: "平台槽位 + 平台侧等待原因 + Volcano 只读快照（GET /api/v1/queue-status）。三层语义不同，**不可相加**成一个排队数。",
			InputSchema: schemaObject(nil),
		},
		{
			Name:        "argus_list_profiles",
			Description: "列出已登记的 Execution Profile 与挂载能力（GET /api/v1/profiles）：name/cluster/namespace/scheduler/queue/mounts；提交 Task 时 execution.profile 必须命中这里。",
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
	case "argus_submit_task":
		return s.toolSubmitTask(ctx, args)
	case "argus_get_task":
		return s.toolGetTask(ctx, args)
	case "argus_wait_task":
		return s.toolWaitTask(ctx, args)
	case "argus_cancel_task":
		return s.toolCancelTask(ctx, args)
	case "argus_retry_task":
		return s.toolRetryTask(ctx, args)
	case "argus_clean_task":
		return s.toolCleanTask(ctx, args)
	case "argus_list_runners":
		return s.toolListRunners(ctx, args)
	case "argus_get_report":
		return s.toolGetReport(ctx, args)
	case "argus_get_logs":
		return s.toolGetLogs(ctx, args)
	case "argus_list_artifacts":
		return s.toolListArtifacts(ctx, args)
	case "argus_get_file":
		return s.toolGetFile(ctx, args)
	case "argus_queue_status":
		return s.toolQueueStatus(ctx, args)
	case "argus_list_profiles":
		return s.toolListProfiles(ctx, args)
	default:
		return nil, invalidParams("未知工具: " + name)
	}
}

func (s *Server) toolSubmitTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Manifest string `json:"manifest"`
		DryRun   bool   `json:"dry_run"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_submit_task: " + err.Error())
	}
	if strings.TrimSpace(a.Manifest) == "" {
		return nil, invalidParams("argus_submit_task: manifest 不能为空")
	}

	q := url.Values{}
	if a.DryRun {
		q.Set("dry_run", "true")
	}
	contentType := "application/yaml"
	if looksLikeJSON(a.Manifest) {
		contentType = "application/json"
	}

	res, err := s.client.Post(ctx, "/api/v1/tasks", q, []byte(a.Manifest), contentType)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolGetTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_get_task: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_get_task: task_id 不能为空")
	}
	res, err := s.client.Get(ctx, "/api/v1/tasks/"+pathEscape(a.TaskID), nil)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolWaitTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID          string  `json:"task_id"`
		TimeoutSeconds  float64 `json:"timeout_seconds"`
		IntervalSeconds float64 `json:"interval_seconds"`
		Follow          bool    `json:"follow"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_wait_task: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_wait_task: task_id 不能为空")
	}
	timeout := a.TimeoutSeconds
	if timeout == 0 {
		timeout = 3600
	}
	if timeout < 0.1 || timeout > 86400 {
		return nil, invalidParams("argus_wait_task: timeout_seconds 必须在 0.1..86400 之间")
	}
	interval := a.IntervalSeconds
	if interval == 0 {
		interval = 3
	}
	if interval < 0.01 || interval > 60 {
		return nil, invalidParams("argus_wait_task: interval_seconds 必须在 0.01..60 之间")
	}

	// SSE 是服务端侧等待：MCP 仍是同步 request/response，一次返回，不推 progress。
	res, err := Wait(ctx, s.client, a.TaskID, WaitOptions{
		Timeout:  time.Duration(timeout * float64(time.Second)),
		Interval: time.Duration(interval * float64(time.Second)),
		Follow:   a.Follow,
	})
	if err != nil {
		return restError(err), nil
	}
	snap, perr := parseTaskSnapshot(res.TaskJSON)
	if perr != nil {
		return errorResult("控制面返回的 Task 不是合法 JSON: " + perr.Error()), nil
	}

	out := map[string]any{
		"task_id":         a.TaskID,
		"status":          res.Status,
		"terminal":        isTerminalTaskStatus(res.Status),
		"outcome":         res.Outcome,
		"error":           rawOrNil(snap.Error),
		"elapsed_seconds": roundSeconds(res.Elapsed),
		"wait_mode":       res.Mode,
	}
	if res.FallbackReason != "" {
		out["fallback_reason"] = res.FallbackReason
	}
	if res.TimedOut {
		out["timed_out"] = true
	}
	if h, ok := hintForSnapshot(snap); ok {
		out["next_action"] = h.NextAction
		out["hint"] = h.Hint
	}
	return jsonResult(out), nil
}

func (s *Server) toolCancelTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_cancel_task: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_cancel_task: task_id 不能为空")
	}
	path := "/api/v1/tasks/" + pathEscape(a.TaskID)
	res, err := s.client.Post(ctx, path+"/cancel", nil, nil, "")
	if err != nil {
		return restError(err), nil
	}

	out := map[string]any{
		"task_id":     a.TaskID,
		"accepted":    true,
		"http_status": res.Status,
		"confirmed":   false,
	}
	if confirm, err := s.client.Get(ctx, path, nil); err == nil {
		if snap, perr := parseTaskSnapshot(confirm.Body); perr == nil {
			out["status"] = snap.Status
			out["confirmed"] = true
		}
	} else if snap, perr := parseTaskSnapshot(res.Body); perr == nil && snap.Status != "" {
		out["status"] = snap.Status
	}
	return jsonResult(out), nil
}

// toolRetryTask 是整 Task 重试（C-5.6）：新 attempt 全量重跑含 fresh deploy。
func (s *Server) toolRetryTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_retry_task: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_retry_task: task_id 不能为空")
	}
	res, err := s.client.Post(ctx, "/api/v1/tasks/"+pathEscape(a.TaskID)+"/retry", nil, nil, "")
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

// toolCleanTask 是泄漏重放（§9.3）：attempt=0 表示最后一个 attempt。
func (s *Server) toolCleanTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID  string `json:"task_id"`
		Attempt int    `json:"attempt"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_clean_task: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_clean_task: task_id 不能为空")
	}
	if a.Attempt < 0 {
		return nil, invalidParams("argus_clean_task: attempt 必须 ≥0（0 = 最后一个 attempt）")
	}
	q := url.Values{}
	if a.Attempt > 0 {
		q.Set("attempt", strconv.Itoa(a.Attempt))
	}
	body := []byte("{}")
	if a.Attempt > 0 {
		body, _ = json.Marshal(map[string]any{"attempt": a.Attempt})
	}
	res, err := s.client.Post(ctx, "/api/v1/tasks/"+pathEscape(a.TaskID)+"/clean", q, body, "application/json")
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolListRunners(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Role     string `json:"role"`
		Orphaned bool   `json:"orphaned"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_list_runners: " + err.Error())
	}
	q := url.Values{}
	if a.Role != "" {
		if a.Role != "test" && a.Role != "pre_check" {
			return nil, invalidParams("argus_list_runners: role 只能是 test|pre_check")
		}
		q.Set("role", a.Role)
	}
	if a.Orphaned {
		q.Set("orphaned", "true")
	}
	res, err := s.client.Get(ctx, "/api/v1/runners", q)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

func (s *Server) toolGetReport(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID string `json:"task_id"`
		Format string `json:"format"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_get_report: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_get_report: task_id 不能为空")
	}
	format := strings.ToLower(strings.TrimSpace(a.Format))
	path := "/api/v1/tasks/" + pathEscape(a.TaskID) + "/report"
	switch format {
	case "", "json":
	case "markdown", "md":
		path += ".md"
	default:
		return nil, invalidParams("argus_get_report: format 只能是 json|markdown")
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
		TaskID    string `json:"task_id"`
		Attempt   int    `json:"attempt"`
		RunnerSeq int    `json:"runner_seq"`
		Stream    string `json:"stream"`
		Offset    *int64 `json:"offset"`
		Limit     *int64 `json:"limit"`
		Tail      *int   `json:"tail"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_get_logs: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_get_logs: task_id 不能为空")
	}
	if a.Attempt == 0 {
		a.Attempt = 1
	}
	if a.Attempt < 1 {
		return nil, invalidParams("argus_get_logs: attempt 必须 ≥1")
	}
	if a.RunnerSeq < 0 {
		return nil, invalidParams("argus_get_logs: runner_seq 必须 ≥1（0 = 自动选第一个失败 runner）")
	}
	if a.Stream != "" && a.Stream != "stdout" && a.Stream != "stderr" {
		return nil, invalidParams("argus_get_logs: stream 只能是 stdout|stderr")
	}
	// runner_seq 缺省：从详情里取管道第一个失败 runner（agent-integration.md §5.4）。
	if a.RunnerSeq == 0 {
		seq, err := s.firstFailedRunnerSeq(ctx, a.TaskID, a.Attempt)
		if err != nil {
			return restError(err), nil
		}
		if seq == 0 {
			return errorResult("该 attempt 没有失败或已结束的 runner，请显式给 runner_seq"), nil
		}
		a.RunnerSeq = seq
	}
	q := url.Values{}
	if a.Stream != "" {
		q.Set("stream", a.Stream)
	}
	offset := int64(0)
	if a.Offset != nil {
		if *a.Offset < 0 {
			return nil, invalidParams("argus_get_logs: offset 必须 ≥0")
		}
		offset = *a.Offset
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > 65536 {
			return nil, invalidParams("argus_get_logs: limit 必须在 1..65536 之间")
		}
		q.Set("limit", strconv.FormatInt(*a.Limit, 10))
	}
	if a.Tail != nil {
		if *a.Tail < 1 {
			return nil, invalidParams("argus_get_logs: tail 必须 ≥1")
		}
		q.Set("tail", strconv.Itoa(*a.Tail))
	}

	path := fmt.Sprintf("/api/v1/tasks/%s/attempts/%d/runners/%d/logs",
		pathEscape(a.TaskID), a.Attempt, a.RunnerSeq)
	res, err := s.client.Get(ctx, path, q)
	if err != nil {
		return restError(err), nil
	}

	stream := a.Stream
	if stream == "" {
		stream = "stdout"
	}
	out := map[string]any{
		"task_id":    a.TaskID,
		"attempt":    a.Attempt,
		"runner_seq": a.RunnerSeq,
		"stream":     stream,
		"offset":     offset,
		"coverage":   res.Header.Get("X-Log-Coverage"),
		"text":       string(res.Body),
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

// firstFailedRunnerSeq 从 Task 详情里选第一个失败（或已结束且带错误码）的 runner。
func (s *Server) firstFailedRunnerSeq(ctx context.Context, taskID string, attempt int) (int, error) {
	detail, err := s.client.Get(ctx, "/api/v1/tasks/"+pathEscape(taskID), nil)
	if err != nil {
		return 0, err
	}
	var v struct {
		Runners []struct {
			Seq    int     `json:"seq"`
			Status string  `json:"status"`
			Errors *string `json:"error_code"`
		} `json:"runners"`
		CurrentAttempt int `json:"current_attempt"`
	}
	if err := json.Unmarshal(detail.Body, &v); err != nil {
		return 0, nil // 详情读不出 runners：交由调用方显式指定
	}
	fallback := 0
	for _, rr := range v.Runners {
		switch rr.Status {
		case "failed", "canceled":
			return rr.Seq, nil
		case "succeeded", "skipped":
			if fallback == 0 {
				fallback = rr.Seq
			}
		}
	}
	return fallback, nil
}

func (s *Server) toolListArtifacts(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID    string `json:"task_id"`
		Attempt   int    `json:"attempt"`
		RunnerSeq int    `json:"runner_seq"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_list_artifacts: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return nil, invalidParams("argus_list_artifacts: task_id 不能为空")
	}
	var path string
	if a.RunnerSeq > 0 {
		if a.Attempt == 0 {
			a.Attempt = 1
		}
		if a.Attempt < 1 {
			return nil, invalidParams("argus_list_artifacts: attempt 必须 ≥1")
		}
		path = fmt.Sprintf("/api/v1/tasks/%s/attempts/%d/runners/%d/artifacts",
			pathEscape(a.TaskID), a.Attempt, a.RunnerSeq)
	} else {
		path = "/api/v1/tasks/" + pathEscape(a.TaskID) + "/artifacts"
	}
	res, err := s.client.Get(ctx, path, nil)
	if err != nil {
		return restError(err), nil
	}
	return jsonBodyResult(res.Body)
}

// toolGetFile 返回文件**摘要**（path/size/sha256/前 2KB 预览），不把二进制塞进 content。
// 默认用 Range 取前缀（1 MiB），避免大文件把 MCP 消息撑爆。
func (s *Server) toolGetFile(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		TaskID   string `json:"task_id"`
		Path     string `json:"path"`
		Range    string `json:"range"`
		MaxBytes int64  `json:"max_bytes"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, invalidParams("argus_get_file: " + err.Error())
	}
	if strings.TrimSpace(a.TaskID) == "" || strings.TrimSpace(a.Path) == "" {
		return nil, invalidParams("argus_get_file: task_id 与 path 都不能为空")
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = 1 << 20
	}
	if a.MaxBytes < 1024 || a.MaxBytes > 32<<20 {
		return nil, invalidParams("argus_get_file: max_bytes 必须在 1024..33554432 之间")
	}

	rangeHeader := strings.TrimSpace(a.Range)
	if rangeHeader != "" {
		if !strings.HasPrefix(strings.ToLower(rangeHeader), "bytes=") {
			rangeHeader = "bytes=" + rangeHeader
		}
	} else {
		rangeHeader = fmt.Sprintf("bytes=0-%d", a.MaxBytes-1)
	}

	// artifacts 返回的 path 是 storage-root 相对的（含 <task_id>/ 前缀），而
	// /files?path= 要的是 task 目录内相对路径；两种写法都接受，统一剥离前缀。
	apiPath := normalizeTaskFilePath(a.TaskID, a.Path)
	if apiPath == "" {
		return nil, invalidParams("argus_get_file: path 不能为空")
	}

	q := url.Values{}
	q.Set("path", apiPath)
	res, err := s.client.GetWithHeaders(ctx, "/api/v1/tasks/"+pathEscape(a.TaskID)+"/files", q,
		map[string]string{"Range": rangeHeader})
	if err != nil {
		return restError(err), nil
	}

	body := res.Body
	total := int64(len(body))
	scope := "full"
	if res.Status == http.StatusPartialContent {
		if t, ok := contentRangeTotal(res.Header.Get("Content-Range")); ok {
			total = t
		}
		if int64(len(body)) < total {
			scope = "partial"
		}
	}

	sum := sha256.Sum256(body)
	out := map[string]any{
		"task_id":          a.TaskID,
		"path":             a.Path,
		"api_path":         apiPath,
		"http_status":      res.Status,
		"content_type":     res.Header.Get("Content-Type"),
		"size_bytes":       len(body),
		"total_size_bytes": total,
		"sha256":           hex.EncodeToString(sum[:]),
		"sha256_scope":     scope,
		"truncated":        scope == "partial",
		"range":            rangeHeader,
	}
	if cr := res.Header.Get("Content-Range"); cr != "" {
		out["content_range"] = cr
	}

	preview := body
	if len(preview) > 2048 {
		preview = preview[:2048]
	}
	if looksText(preview) {
		out["encoding"] = "text"
		out["preview"] = string(preview)
	} else {
		out["encoding"] = "base64"
		out["preview_base64"] = base64.StdEncoding.EncodeToString(preview)
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
// 控制面错误信封 -> {error:{http_status,code,message,retryable,trace_id,retry_after,next_action,hint}}；
// 传输失败 -> code=UNREACHABLE。Agent 能直接读到结构化原因与下一步动作。
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
		if h, ok := HintForError(ae.Code); ok {
			e["next_action"] = h.NextAction
			if h.Hint != "" {
				e["hint"] = h.Hint
			}
		}
		return isErrorJSON(map[string]any{"error": e})
	}
	if IsTransportError(err) {
		e := map[string]any{
			"code":      "UNREACHABLE",
			"message":   err.Error(),
			"retryable": true,
		}
		if h, ok := HintForError("UNREACHABLE"); ok {
			e["next_action"] = h.NextAction
			e["hint"] = h.Hint
		}
		return isErrorJSON(map[string]any{"error": e})
	}
	return errorResult(err.Error())
}

// ---- 小工具 ---------------------------------------------------------------

// taskSnapshot 是 wait / cancel 回读需要的最小 Task 投影（其余字段不透传）。
type taskSnapshot struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
}

func parseTaskSnapshot(body []byte) (taskSnapshot, error) {
	var snap taskSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return taskSnapshot{}, err
	}
	return snap, nil
}

// hintForSnapshot 给终态 Task 找下一步提示：优先用 Task.error.code，其次用终态本身。
func hintForSnapshot(snap taskSnapshot) (Hint, bool) {
	var failure struct {
		Code string `json:"code"`
	}
	if len(snap.Error) > 0 && json.Unmarshal(snap.Error, &failure) == nil && failure.Code != "" {
		if h, ok := HintForError(failure.Code); ok {
			return h, true
		}
	}
	return HintForTaskStatus(snap.Status)
}

// contentRangeTotal 解析 `bytes 0-1023/2048` 的总长度。
func contentRangeTotal(header string) (int64, bool) {
	_, totalPart, ok := strings.Cut(header, "/")
	if !ok {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimSpace(totalPart), 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

// looksText 判断前 2KB 是否适合以文本预览：合法 UTF-8 且没有 NUL/控制字符（换行、Tab、CR 除外）。
func looksText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// isTerminalTaskStatus 是 Task 终态判定（v2 8 值中的 4 个终态；与 domain 同源语义）。
func isTerminalTaskStatus(status string) bool {
	switch status {
	case "succeeded", "service_unavailable", "failed", "canceled":
		return true
	default:
		return false
	}
}

// taskOutcome 把终态映射成跨入口一致的 outcome（CLI 退出码 0/1/2/3 与它一一对应）。
func taskOutcome(status string) string {
	switch status {
	case "succeeded":
		return "success"
	case "canceled":
		return "canceled"
	case "service_unavailable", "failed":
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

// normalizeTaskFilePath 容忍两种产物路径写法：
//   - artifacts 返回的 storage-root 相对路径（`<task_id>/attempts/...`）；
//   - /files?path= 需要的 task 目录内相对路径（`attempts/...`）。
func normalizeTaskFilePath(taskID, p string) string {
	p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "/"))
	if taskID != "" {
		p = strings.TrimPrefix(p, strings.TrimSpace(taskID)+"/")
	}
	return p
}

func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	return raw
}

func roundSeconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*1000) / 1000
}
