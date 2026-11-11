package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// waitResult 是轮询到终态后的结果：Task 快照 + 状态轨迹。
type waitResult struct {
	Task    *Task
	Trace   []string
	Elapsed time.Duration
}

// wait 轮询 GET /tasks/{id} 直到终态（07 §5.1、§7.4）。
// 状态轨迹只在"状态或 runner 摘要变化"时记录，避免刷屏。
func (a *App) wait(ctx context.Context, taskID string) (*waitResult, error) {
	start := time.Now()
	deadline := start.Add(a.opts.Timeout)
	lastStatus, lastSummary := "", ""
	trace := make([]string, 0, 16)
	var task *Task

	for {
		status, _, body, err := a.client.Get(ctx, "/tasks/"+url.PathEscape(taskID))
		if err != nil {
			return nil, err
		}
		if err := expectStatus(status, 200, body); err != nil {
			return nil, fmt.Errorf("查询 Task 失败：%w", err)
		}
		var next Task
		if err := json.Unmarshal(body, &next); err != nil {
			return nil, fmt.Errorf("GET /tasks/%s 返回的不是合法 JSON：%v；body=%s", taskID, err, truncate(string(body), 400))
		}
		if next.ID == "" {
			return nil, fmt.Errorf("GET /tasks/%s 响应缺 id（07 §5.1）", taskID)
		}
		task = &next

		summary := runnerStateSummary(task.Runners)
		if task.Status != lastStatus || summary != lastSummary {
			line := fmt.Sprintf("%s status=%s", time.Now().Format("15:04:05"), task.Status)
			if summary != "" {
				line += " runners: " + summary
			}
			trace = append(trace, line)
			a.logf("%s", line)
			lastStatus, lastSummary = task.Status, summary
		}

		if isTerminalTask(task.Status) {
			return &waitResult{Task: task, Trace: trace, Elapsed: time.Since(start)}, nil
		}
		if time.Now().After(deadline) {
			return &waitResult{Task: task, Trace: trace, Elapsed: time.Since(start)}, &timeoutError{
				TaskID:     taskID,
				LastStatus: task.Status,
				Timeout:    a.opts.Timeout,
				Trace:      trace,
			}
		}

		select {
		case <-ctx.Done():
			return &waitResult{Task: task, Trace: trace, Elapsed: time.Since(start)}, ctx.Err()
		case <-time.After(a.opts.Poll):
		}
	}
}

type timeoutError struct {
	TaskID     string
	LastStatus string
	Timeout    time.Duration
	Trace      []string
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("等待 Task %s 终态超时（%s），最后状态 %s；状态轨迹：%s",
		e.TaskID, e.Timeout, e.LastStatus, strings.Join(e.Trace, " | "))
}

// runnerStateSummary 生成 "precheck=succeeded, probe-a=running" 形态的摘要（最多 6 个）。
func runnerStateSummary(runners []RunnerExecution) string {
	if len(runners) == 0 {
		return ""
	}
	parts := make([]string, 0, len(runners))
	for i, r := range runners {
		if i == 6 {
			parts = append(parts, fmt.Sprintf("…(%d 个)", len(runners)))
			break
		}
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("seq%d", r.Seq)
		}
		parts = append(parts, name+"="+r.Status)
	}
	return strings.Join(parts, ", ")
}
