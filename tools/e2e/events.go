package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Event 是 07 §7 的事件信封（v2）：
//
//	id: 43
//	event: runner_finished
//	data: {"seq":43,"type":"runner_finished","task_id":"t-…","attempt_no":1,"runner_seq":3,
//	       "at":"…","payload":{"status":"failed","error_code":"RUNNER_NONZERO_EXIT"}}
//
// 也容忍把 from/to 放在顶层（不同实现常见），读取时以 payload 优先。
type Event struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	TaskID    string         `json:"task_id"`
	AttemptNo *int           `json:"attempt_no"`
	RunnerSeq *int           `json:"runner_seq"`
	At        string         `json:"at"`
	Payload   map[string]any `json:"payload"`
	From      string         `json:"from"`
	To        string         `json:"to"`
	Reason    string         `json:"reason"`
}

// Transition 取状态转移的两端；缺任一端返回 ok=false。
func (e Event) Transition() (from, to string, ok bool) {
	if e.Payload != nil {
		if v, ok := e.Payload["from"].(string); ok {
			from = v
		}
		if v, ok := e.Payload["to"].(string); ok {
			to = v
		}
	}
	if from == "" {
		from = e.From
	}
	if to == "" {
		to = e.To
	}
	return from, to, from != "" && to != ""
}

// IsRunnerLevel 判断事件是否挂在具体 runner 上（runner_seq 非空）。
func (e Event) IsRunnerLevel() bool {
	return e.RunnerSeq != nil && *e.RunnerSeq > 0
}

// 转移表：§2.1（Task）与 §2.2（runner）。这里是 e2e 的"合法性裁判"，
// 与设计状态机逐字对应；表外的一律算违约，不做"看起来合理就放行"。
var taskTransitions = map[string][]string{
	"submitted":           {"queued", "failed"},
	"queued":              {"running"},
	"running":             {"succeeded", "failed", "service_unavailable"},
	"cancelling":          {"canceled"},
	"succeeded":           {"queued"}, // retry：整 Task 新 attempt 全量重跑（§7.8）
	"failed":              {"queued"}, // retry（canceled 不可重试）
	"service_unavailable": {"queued"}, // retry
}

var runnerTransitions = map[string][]string{
	"pending":   {"queued", "skipped"},
	"queued":    {"preparing", "skipped", "canceled"},
	"preparing": {"running", "failed", "canceled"},
	"running":   {"succeeded", "failed", "canceled"},
}

var knownEventTypes = map[string]bool{
	"submitted":         true,
	"state_changed":     true,
	"runner_started":    true,
	"runner_finished":   true,
	"exports_harvested": true,
	"clean_finished":    true,
	"cleanup_failed":    true,
	"task_finished":     true,
	"report.written":    true,
	"resync_required":   true,
}

// legalTransition 判断 runner 级 / task 级的 from -> to 是否在转移表内。
// 额外规则：任意非终态 Task → cancelling 合法（R6 取消边）；runner 终态无出边（无重试回路，D-69）。
func legalTransition(runnerLevel bool, from, to string) bool {
	if from == "" || to == "" || from == to {
		return false
	}
	if runnerLevel {
		for _, next := range runnerTransitions[from] {
			if next == to {
				return true
			}
		}
		return false
	}
	if to == "cancelling" && !isTerminalTask(from) {
		return true
	}
	for _, next := range taskTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// collectEvents 优先用 JSON 事件历史；不可用时回退 SSE 抓取（只用于排障，
// 主断言仍要求 /events/history 可用）。返回空表示两条路都没拿到。
func (a *App) collectEvents(ctx context.Context, taskID string) []Event {
	path := fmt.Sprintf("/tasks/%s/events/history?since_seq=0&limit=1000", url.PathEscape(taskID))
	status, _, body, err := a.client.Get(ctx, path)
	if err != nil {
		a.addCheck("events.history", false, err.Error())
		return nil
	}
	if status == 200 {
		events, err := decodeEvents(body)
		if err != nil {
			a.addCheck("events.history", false, fmt.Sprintf("解析失败：%v", err))
			return nil
		}
		a.eventsSource = "history"
		a.addCheck("events.history", true, fmt.Sprintf("%d 条（GET %s）", len(events), path))
		return events
	}
	a.addCheck("events.history", false, fmt.Sprintf("HTTP %d：%s", status, apiError(status, body).Error()))

	a.warn("事件历史接口不可用，回退到 SSE GET /tasks/{id}/events 抓取；只验证连接期间的重放")
	events, sseErr := a.readSSE(ctx, taskID, a.opts.SSETimeout)
	if sseErr != nil {
		a.addCheck("events.sse", false, sseErr.Error())
		return nil
	}
	a.eventsSource = "sse"
	a.addCheck("events.sse", true, fmt.Sprintf("%d 条", len(events)))
	return events
}

func decodeEvents(raw []byte) ([]Event, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("响应体为空")
	}
	if trimmed[0] == '[' {
		var arr []Event
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("事件数组不是合法 JSON：%v", err)
		}
		return arr, nil
	}
	var obj struct {
		Events []Event `json:"events"`
		Items  []Event `json:"items"`
		Data   []Event `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("事件响应不是合法 JSON：%v", err)
	}
	switch {
	case obj.Events != nil:
		return obj.Events, nil
	case obj.Items != nil:
		return obj.Items, nil
	case obj.Data != nil:
		return obj.Data, nil
	}
	return nil, fmt.Errorf("事件响应既不是数组，也没有 events/items/data 字段：%s", truncate(string(trimmed), 300))
}

// validateEvents 是"事件序列合法"的裁判。
// 硬性：seq 单调、状态转移在表内、有 submitted、Task 终态事实与实际终态一致。
// 软性（--strict 时升级为失败）：缺 task_finished、runner 事件覆盖不全。
func (a *App) validateEvents(events []Event, task *Task) {
	if len(events) == 0 {
		a.addCheck("events.available", false, "没有拿到任何事件")
		return
	}
	a.addCheck("events.available", true, fmt.Sprintf("%d 条", len(events)))

	// task_id 一致性
	var badTaskIDs []string
	for _, ev := range events {
		if ev.TaskID != "" && ev.TaskID != task.ID {
			badTaskIDs = append(badTaskIDs, fmt.Sprintf("seq=%d task_id=%s", ev.Seq, ev.TaskID))
		}
	}
	a.addCheck("events.task_id_consistent", len(badTaskIDs) == 0,
		firstN(badTaskIDs, 3, fmt.Sprintf("事件属于本题 Task %s", task.ID)))

	// seq 严格单调
	allZero := true
	for _, ev := range events {
		if ev.Seq > 0 {
			allZero = false
			break
		}
	}
	if allZero {
		a.warn("事件流没有 seq 字段（全为 0），跳过单调性检查；07 §7 要求 seq 用于 Last-Event-ID 回放")
	} else {
		var prev int64
		var bad []string
		for _, ev := range events {
			if ev.Seq <= prev {
				bad = append(bad, fmt.Sprintf("seq=%d 未严格递增（上一条 seq=%d，type=%s）", ev.Seq, prev, ev.Type))
			}
			if ev.Seq > prev {
				prev = ev.Seq
			}
		}
		a.addCheck("events.seq_monotonic", len(bad) == 0, firstN(bad, 5, fmt.Sprintf("seq 严格递增（%d..%d）", events[0].Seq, prev)))
	}

	// 状态转移合法性（task 级 = runner_seq 为空的 state_changed；task_finished 是终态事实）
	var transitionBad []string
	terminalStatuses := map[string]bool{}
	terminalFact := "" // 最后一条终态事实（state_changed to=终态 或 task_finished.status）
	typeCount := map[string]int{}
	var unknownTypes []string
	for _, ev := range events {
		typeCount[ev.Type]++
		if !knownEventTypes[ev.Type] && ev.Type != "" {
			unknownTypes = append(unknownTypes, ev.Type)
		}
		switch {
		case ev.Type == "state_changed" && ev.IsRunnerLevel():
			from, to, ok := ev.Transition()
			who := fmt.Sprintf("runner_seq=%d", *ev.RunnerSeq)
			if ev.TaskID != "" {
				who += "@" + ev.TaskID
			}
			if !ok {
				transitionBad = append(transitionBad, fmt.Sprintf("seq=%d state_changed（%s）缺 from/to", ev.Seq, who))
				continue
			}
			if !legalTransition(true, from, to) {
				transitionBad = append(transitionBad, fmt.Sprintf("seq=%d state_changed（%s）%s -> %s 不在 runner 转移表内", ev.Seq, who, from, to))
			}
		case ev.Type == "task_finished":
			// task_finished 是终态**事实**而不是转移：只带 status（或 from/to 都缺），
			// 不参与转移表判定；终态一致性由 events.final_state / terminal_once 把关。
			to := eventStatus(ev)
			if _, t2, ok := ev.Transition(); ok {
				to = t2
			}
			if to == "" {
				transitionBad = append(transitionBad, fmt.Sprintf("seq=%d task_finished 缺 status", ev.Seq))
				continue
			}
			if isTerminalTask(to) {
				terminalStatuses[to] = true
				terminalFact = to
			}
		case ev.Type == "state_changed":
			from, to, ok := ev.Transition()
			who := ev.TaskID
			if !ok {
				transitionBad = append(transitionBad, fmt.Sprintf("seq=%d %s（%s）缺 from/to", ev.Seq, ev.Type, who))
				continue
			}
			if !legalTransition(false, from, to) {
				transitionBad = append(transitionBad, fmt.Sprintf("seq=%d %s（%s）%s -> %s 不在转移表内", ev.Seq, ev.Type, who, from, to))
			}
			if isTerminalTask(to) {
				terminalStatuses[to] = true
				terminalFact = to
			}
		default:
			continue
		}
	}
	a.addCheck("events.transitions_legal", len(transitionBad) == 0,
		firstN(transitionBad, 5, "所有 task/runner 状态转移都在 §2.1/§2.2 转移表内"))
	sort.Strings(unknownTypes)
	if len(unknownTypes) > 0 {
		a.warn("未知事件类型（07 §7 未列出）：%s", strings.Join(uniqueStrings(unknownTypes), ", "))
	}

	// 必需事件与收尾
	if typeCount["submitted"] == 0 {
		a.addCheck("events.submitted", false, "缺少 submitted 事件（07 §7）")
	} else {
		a.addCheck("events.submitted", true, "存在 submitted")
	}
	if typeCount["task_finished"] == 0 {
		a.warn("缺少 task_finished 事件（07 §7）；当前实现可能只发 state_changed")
	}
	terminalList := make([]string, 0, len(terminalStatuses))
	for status := range terminalStatuses {
		terminalList = append(terminalList, status)
	}
	sort.Strings(terminalList)
	switch {
	case len(terminalList) == 0:
		a.addCheck("events.terminal_once", false, "事件流里没有 Task 终态事件（state_changed/task_finished 都没有 terminal to）")
	case len(terminalList) > 1:
		a.addCheck("events.terminal_once", false, fmt.Sprintf("Task 终态事件出现多个不同目标：%s", strings.Join(terminalList, ", ")))
	default:
		a.addCheck("events.terminal_once", true, "Task 终态事件目标唯一："+terminalList[0])
	}
	// 终态一致性：最后一条终态事实（state_changed to=终态 或 task_finished.status）必须等于实际终态。
	// 终态后的 retry 边（terminal→queued）不算事实，不影响判定。
	if terminalFact == "" {
		a.addCheck("events.final_state", false, "事件流里没有 Task 终态事实，无法核对终态")
	} else if terminalFact != task.Status {
		a.addCheck("events.final_state", false, fmt.Sprintf("最后一条终态事实=%s，与 Task 实际终态 %s 不一致", terminalFact, task.Status))
	} else {
		a.addCheck("events.final_state", true, "终态事实与实际终态一致："+terminalFact)
	}

	// runner 事件覆盖（软性）：终态 runner 至少有 runner_finished。
	for _, r := range task.Runners {
		if !isTerminalRunner(r.Status) || r.Role == "deploy" || r.Role == "clean" {
			continue
		}
		finished := 0
		for _, ev := range events {
			if ev.Type == "runner_finished" && ev.RunnerSeq != nil && *ev.RunnerSeq == r.Seq {
				finished++
			}
		}
		if finished == 0 {
			a.warn("runner %s（seq=%d）已终态但事件里没有 runner_finished（07 §7）", r.Name, r.Seq)
		}
	}
}

// readSSE 连接 GET /tasks/{id}/events 并把 data: 帧解成事件，
// 收到终态事件或窗口耗尽即停。
func (a *App) readSSE(ctx context.Context, taskID string, window time.Duration) ([]Event, error) {
	sctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	resp, err := a.client.Stream(sctx, "/tasks/"+url.PathEscape(taskID)+"/events", map[string]string{"Last-Event-ID": "0"})
	if err != nil {
		return nil, fmt.Errorf("SSE 连接失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("SSE 期望 HTTP 200，实际 %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	var events []Event
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		events = append(events, ev)
		if ev.Type == "task_finished" {
			break
		}
		if ev.Type == "state_changed" && !ev.IsRunnerLevel() {
			if _, to, ok := ev.Transition(); ok && isTerminalTask(to) {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil && len(events) == 0 {
		return nil, fmt.Errorf("SSE 读取失败：%v", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%s 内没有收到任何 data: 事件", window)
	}
	return events, nil
}

// checkSSESmoke 是 SSE 连通性冒烟：终态 Task 上建立连接，断言 200 与
// Content-Type: text/event-stream，并在窗口内至少读到一行（id/data/: ping 均算）。
func (a *App) checkSSESmoke(ctx context.Context, taskID string) {
	sctx, cancel := context.WithTimeout(ctx, a.opts.SSETimeout)
	defer cancel()

	path := "/tasks/" + url.PathEscape(taskID) + "/events"
	resp, err := a.client.Stream(sctx, path, map[string]string{"Last-Event-ID": "0"})
	if err != nil {
		a.addCheck("sse.connect", false, err.Error())
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if resp.StatusCode != 200 {
		a.addCheck("sse.connect", false, fmt.Sprintf("HTTP %d（期望 200）", resp.StatusCode))
		return
	}
	if !strings.HasPrefix(contentType, "text/event-stream") {
		a.addCheck("sse.connect", false, fmt.Sprintf("Content-Type=%q，期望 text/event-stream", contentType))
		return
	}

	lineCh := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		line, _ := reader.ReadString('\n')
		lineCh <- strings.TrimSpace(line)
	}()

	select {
	case line := <-lineCh:
		if line == "" {
			a.addCheck("sse.first_line", false, "连接建立后立刻 EOF，没有收到任何行（终态 Task 应能回放历史事件）")
			return
		}
		a.addCheck("sse.connect", true, fmt.Sprintf("HTTP 200 text/event-stream；首行 %q", truncate(line, 80)))
	case <-time.After(a.opts.SSETimeout):
		a.addCheck("sse.connect", false, fmt.Sprintf("%s 内没有收到任何行（连心跳也没有）", a.opts.SSETimeout))
	}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// eventStatus 读 task_finished 这类事件里可能的 status 字段。
func eventStatus(ev Event) string {
	if ev.Payload == nil {
		return ""
	}
	if s, ok := ev.Payload["status"].(string); ok {
		return s
	}
	return ""
}

// firstN 生成断言 detail：空切片时用 okDetail，否则拼前 n 条。
func firstN(items []string, n int, okDetail string) string {
	if len(items) == 0 {
		return okDetail
	}
	if len(items) > n {
		items = append(items[:n], fmt.Sprintf("…共 %d 条", len(items)))
	}
	return strings.Join(items, "；")
}
