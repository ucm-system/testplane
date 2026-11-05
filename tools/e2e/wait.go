package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// waitResult 是轮询到终态后的结果：Run 快照 + 状态轨迹。
type waitResult struct {
	Run     *Run
	Trace   []string
	Elapsed time.Duration
}

// wait 轮询 GET /runs/{id} 直到终态（07 §5.1、C-3.2）。
// 状态轨迹只在"状态或 Task 摘要变化"时记录，避免刷屏。
func (a *App) wait(ctx context.Context, runID string) (*waitResult, error) {
	start := time.Now()
	deadline := start.Add(a.opts.Timeout)
	lastStatus, lastSummary := "", ""
	trace := make([]string, 0, 16)
	var run *Run

	for {
		status, _, body, err := a.client.Get(ctx, "/runs/"+url.PathEscape(runID))
		if err != nil {
			return nil, err
		}
		if err := expectStatus(status, 200, body); err != nil {
			return nil, fmt.Errorf("查询 Run 失败：%w", err)
		}
		var next Run
		if err := json.Unmarshal(body, &next); err != nil {
			return nil, fmt.Errorf("GET /runs/%s 返回的不是合法 JSON：%v；body=%s", runID, err, truncate(string(body), 400))
		}
		if next.ID == "" {
			return nil, fmt.Errorf("GET /runs/%s 响应缺 id（07 §5.1）", runID)
		}
		run = &next

		summary := taskStateSummary(run.Tasks)
		if run.Status != lastStatus || summary != lastSummary {
			line := fmt.Sprintf("%s status=%s", time.Now().Format("15:04:05"), run.Status)
			if summary != "" {
				line += " tasks: " + summary
			}
			trace = append(trace, line)
			if a.cfg.Logf != nil {
				a.cfg.Logf("%s", line)
			}
			lastStatus, lastSummary = run.Status, summary
		}

		if isTerminalRun(run.Status) {
			return &waitResult{Run: run, Trace: trace, Elapsed: time.Since(start)}, nil
		}
		if time.Now().After(deadline) {
			return &waitResult{Run: run, Trace: trace, Elapsed: time.Since(start)}, &timeoutError{
				RunID:      runID,
				LastStatus: run.Status,
				Timeout:    a.opts.Timeout,
				Trace:      trace,
			}
		}

		select {
		case <-ctx.Done():
			return &waitResult{Run: run, Trace: trace, Elapsed: time.Since(start)}, ctx.Err()
		case <-time.After(a.opts.Poll):
		}
	}
}

type timeoutError struct {
	RunID      string
	LastStatus string
	Timeout    time.Duration
	Trace      []string
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("等待 Run %s 终态超时（%s），最后状态 %s；状态轨迹：%s",
		e.RunID, e.Timeout, e.LastStatus, strings.Join(e.Trace, " | "))
}

// taskStateSummary 生成 "precheck=pending, probe-a=pending" 形态的摘要（最多 6 个）。
func taskStateSummary(tasks []Task) string {
	if len(tasks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tasks))
	for i, t := range tasks {
		if i == 6 {
			parts = append(parts, fmt.Sprintf("…(%d 个)", len(tasks)))
			break
		}
		name := t.Name
		if name == "" {
			name = t.ID
		}
		parts = append(parts, name+"="+t.Status)
	}
	return strings.Join(parts, ", ")
}
