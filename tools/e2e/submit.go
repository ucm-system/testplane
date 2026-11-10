package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// submitOutcome 是 POST /tasks 的结果。
type submitOutcome struct {
	TaskID string
	Status int
	Body   []byte
}

// submit 提交 Task：POST /tasks，断言 202 + task.id（07 §4.2）。
// 可选先 dry-run（--dry-run-first，默认关）：?dry_run=true 只校验不落库，返回 200 DryRunResult。
func (a *App) submit(ctx context.Context) (*submitOutcome, error) {
	raw, contentType, err := a.loadManifest()
	if err != nil {
		return nil, err
	}
	if a.opts.Verbose {
		a.logf("Manifest（%s，%d 字节）：\n%s", contentType, len(raw), string(raw))
	}

	if a.opts.DryRunFirst {
		if err := a.dryRun(ctx, raw, contentType); err != nil {
			return nil, err
		}
	}

	status, header, body, err := a.client.Post(ctx, "/tasks", raw, contentType)
	if err != nil {
		return nil, err
	}
	_ = header
	if err := expectStatus(status, 202, body); err != nil {
		return nil, fmt.Errorf("提交 Task 失败：%w", err)
	}

	taskID := extractTaskID(body)
	if taskID == "" {
		return nil, fmt.Errorf("202 响应里没有 task id（期望 {\"task\":{\"id\":\"t-...\"}}）：%s", truncate(string(body), 800))
	}
	a.addCheck("submit.http_status", true, "HTTP 202 Accepted")
	a.addCheck("submit.task_id", true, taskID)
	if !responseHasRunners(body) {
		a.warn("202 响应里没有 runners[]（07 §4.2 期望返回含注入行的最终执行序）")
	}
	return &submitOutcome{TaskID: taskID, Status: status, Body: body}, nil
}

func (a *App) dryRun(ctx context.Context, raw []byte, contentType string) error {
	status, header, body, err := a.client.Post(ctx, "/tasks?dry_run=true", raw, contentType)
	if err != nil {
		return err
	}
	_ = header
	if status == 404 {
		return &UsageError{Msg: "开启了 --dry-run-first，但控制面没有 POST /tasks?dry_run（404）；去掉该参数后重试"}
	}
	if err := expectStatus(status, 200, body); err != nil {
		return fmt.Errorf("dry-run 失败：%w", err)
	}

	var res struct {
		Valid  *bool `json:"valid"`
		Issues []struct {
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Line    int    `json:"line"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("dry-run 响应不是合法 JSON：%v；body=%s", err, truncate(string(body), 600))
	}
	if res.Valid == nil {
		return &UsageError{Msg: "dry-run 响应缺 valid 字段（后端可能未实现 ?dry_run，请去掉 --dry-run-first）；body=" + truncate(string(body), 400)}
	}
	if !*res.Valid {
		lines := make([]string, 0, len(res.Issues))
		for _, issue := range res.Issues {
			if issue.Line > 0 {
				lines = append(lines, fmt.Sprintf("%s:%d %s（%s）", issue.Path, issue.Line, issue.Message, issue.Code))
			} else {
				lines = append(lines, fmt.Sprintf("%s: %s（%s）", issue.Path, issue.Message, issue.Code))
			}
		}
		return fmt.Errorf("Manifest 校验未通过（dry-run valid=false）：\n  %s", strings.Join(lines, "\n  "))
	}
	a.addCheck("submit.dry_run", true, "valid=true")
	return nil
}

// extractTaskID 兼容 07 §4.2 的 {task:{id}}，以及常见的扁平形态。
func extractTaskID(raw []byte) string {
	var resp struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	switch {
	case resp.Task.ID != "":
		return resp.Task.ID
	case resp.TaskID != "":
		return resp.TaskID
	default:
		return resp.ID
	}
}

func responseHasRunners(raw []byte) bool {
	var resp struct {
		Runners []json.RawMessage `json:"runners"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false
	}
	return len(resp.Runners) > 0
}
