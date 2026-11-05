package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 本文件是 tpctl 的子命令实现。每个子命令都是"解析参数 → 一次 REST 调用 → 打印"，
// 不引入 REST 之外的语义（见 agent-integration.md §3 的映射表）。

// ---- submit ---------------------------------------------------------------

type submitOutput struct {
	RunID       string       `json:"run_id"`
	Status      string       `json:"status"`
	Kind        string       `json:"kind"`
	SubmittedAt *string      `json:"submitted_at,omitempty"`
	Tasks       []submitTask `json:"tasks"`
}

type submitTask struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (a *app) cmdSubmit(args []string) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	var file, fileAlias string
	var dryRun, jsonOut bool
	fs.StringVar(&file, "f", "", "Manifest 文件路径（- 表示 stdin）")
	fs.StringVar(&fileAlias, "file", "", "同 -f")
	fs.BoolVar(&dryRun, "dry-run", false, "只校验、不落库（POST /runs?dry_run=true）")
	fs.BoolVar(&jsonOut, "json", a.jsonOut, "机器可读输出")

	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) > 0 {
		return a.usageError("submit 不接受位置参数: %v", pos)
	}
	if file == "" {
		file = fileAlias
	}
	if file == "" {
		return a.usageError("submit 需要 -f <manifest.yaml>")
	}

	manifest, err := a.readManifest(file)
	if err != nil {
		return a.usageError("%v", err)
	}
	q := url.Values{}
	if dryRun {
		q.Set("dry_run", "true")
	}
	contentType := "application/yaml"
	if looksLikeJSON(string(manifest)) {
		contentType = "application/json"
	}

	res, err := a.api.post(context.Background(), "/api/v1/runs", q, manifest, contentType)
	if err != nil {
		return a.fail(err)
	}

	if dryRun {
		return a.printDryRun(res.Body, jsonOut)
	}

	var body struct {
		Run struct {
			ID          string  `json:"id"`
			Status      string  `json:"status"`
			Kind        string  `json:"kind"`
			SubmittedAt *string `json:"submitted_at"`
		} `json:"run"`
		Tasks []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return a.fail(fmt.Errorf("控制面返回了无法解析的提交应答: %w", err))
	}
	out := submitOutput{
		RunID:       body.Run.ID,
		Status:      body.Run.Status,
		Kind:        body.Run.Kind,
		SubmittedAt: body.Run.SubmittedAt,
		Tasks:       make([]submitTask, 0, len(body.Tasks)),
	}
	for _, t := range body.Tasks {
		out.Tasks = append(out.Tasks, submitTask{ID: t.ID, Name: t.Name, Status: t.Status})
	}

	if jsonOut {
		a.printJSON(out)
		return exitOK
	}
	fmt.Fprintf(a.stdout, "已提交 %s（status=%s, kind=%s）\n", out.RunID, out.Status, out.Kind)
	for _, t := range out.Tasks {
		fmt.Fprintf(a.stdout, "  %s  %-24s %s\n", t.ID, t.Name, t.Status)
	}
	return exitOK
}

func (a *app) readManifest(path string) ([]byte, error) {
	const maxManifest = 1 << 20 // 07 §1：请求体 ≤ 1 MiB
	if path == "-" {
		data, err := io.ReadAll(io.LimitReader(a.stdin, maxManifest+1))
		if err != nil {
			return nil, fmt.Errorf("读取 stdin 失败: %w", err)
		}
		if len(data) > maxManifest {
			return nil, fmt.Errorf("Manifest 超过 1 MiB 上限")
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 Manifest %s 失败: %w", path, err)
	}
	if len(data) > maxManifest {
		return nil, fmt.Errorf("Manifest 超过 1 MiB 上限: %s", path)
	}
	return data, nil
}

func (a *app) printDryRun(body []byte, jsonOut bool) int {
	var dr struct {
		Valid  bool `json:"valid"`
		Issues []struct {
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &dr); err != nil {
		return a.fail(fmt.Errorf("控制面返回了无法解析的 dry-run 应答: %w", err))
	}
	if jsonOut {
		writeCompactJSON(a.stdout, body)
	} else if dr.Valid {
		fmt.Fprintln(a.stdout, "校验通过（dry-run，未落库）")
	} else {
		fmt.Fprintln(a.stdout, "校验失败（dry-run，未落库）：")
		for _, issue := range dr.Issues {
			fmt.Fprintf(a.stdout, "  %s  %s: %s\n", issue.Path, issue.Code, issue.Message)
		}
	}
	if !dr.Valid {
		return exitFailure
	}
	return exitOK
}

// ---- status / wait --------------------------------------------------------

type runSummary struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	SubmittedAt *string         `json:"submitted_at"`
	StartedAt   *string         `json:"started_at"`
	FinishedAt  *string         `json:"finished_at"`
	TaskCounts  json.RawMessage `json:"task_counts"`
	Error       json.RawMessage `json:"error"`
	Tasks       []taskSummary   `json:"tasks"`
}

type taskSummary struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	WaitingReason *string         `json:"waiting_reason"`
	Error         json.RawMessage `json:"error"`
}

func parseRun(body []byte) (runSummary, error) {
	var run runSummary
	if err := json.Unmarshal(body, &run); err != nil {
		return runSummary{}, err
	}
	return run, nil
}

// statusOutput 是 status / wait 的 --json 输出。字段取自 Run 投影，不新增语义；
// terminal / outcome 是由 status 派生的便利字段（映射规则见 agent-integration.md §4.3）。
type statusOutput struct {
	RunID       string          `json:"run_id"`
	Name        string          `json:"name,omitempty"`
	Kind        string          `json:"kind,omitempty"`
	Status      string          `json:"status"`
	Terminal    bool            `json:"terminal"`
	SubmittedAt *string         `json:"submitted_at,omitempty"`
	StartedAt   *string         `json:"started_at,omitempty"`
	FinishedAt  *string         `json:"finished_at,omitempty"`
	TaskCounts  json.RawMessage `json:"task_counts,omitempty"`
	Error       json.RawMessage `json:"error"`
	Tasks       []statusTask    `json:"tasks,omitempty"`
}

type statusTask struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	WaitingReason *string `json:"waiting_reason,omitempty"`
}

type waitOutput struct {
	statusOutput
	Outcome        string  `json:"outcome"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
}

func buildStatusOutput(run runSummary) statusOutput {
	out := statusOutput{
		RunID:       run.ID,
		Name:        run.Name,
		Kind:        run.Kind,
		Status:      run.Status,
		Terminal:    isTerminalRunStatus(run.Status),
		SubmittedAt: run.SubmittedAt,
		StartedAt:   run.StartedAt,
		FinishedAt:  run.FinishedAt,
		TaskCounts:  run.TaskCounts,
		Error:       rawOrNil(run.Error),
	}
	for _, t := range run.Tasks {
		out.Tasks = append(out.Tasks, statusTask{
			ID: t.ID, Name: t.Name, Status: t.Status,
			Attempts: t.Attempts, WaitingReason: t.WaitingReason,
		})
	}
	return out
}

func (a *app) cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) != 1 {
		return a.usageError("用法: tpctl status <run_id>")
	}

	res, err := a.api.get(context.Background(), "/api/v1/runs/"+pathEscape(pos[0]), nil)
	if err != nil {
		return a.fail(err)
	}
	run, err := parseRun(res.Body)
	if err != nil {
		return a.fail(fmt.Errorf("控制面返回了无法解析的 Run: %w", err))
	}
	out := buildStatusOutput(run)
	if *jsonOut {
		a.printJSON(out)
	} else {
		a.printRunSummary(out)
	}
	return exitOK
}

func (a *app) cmdWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeoutStr := fs.String("timeout", "3600", "总等待时长（秒或 duration）")
	intervalStr := fs.String("interval", "3", "轮询间隔（秒或 duration）")
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) != 1 {
		return a.usageError("用法: tpctl wait <run_id> [--timeout 3600] [--interval 3]")
	}
	timeout, err := parseDurationLoose(*timeoutStr)
	if err != nil || timeout <= 0 {
		return a.usageError("--timeout 必须是正数（秒或 duration）")
	}
	interval, err := parseDurationLoose(*intervalStr)
	if err != nil || interval <= 0 {
		return a.usageError("--interval 必须是正数（秒或 duration）")
	}

	runID := pos[0]
	path := "/api/v1/runs/" + pathEscape(runID)
	started := time.Now()
	deadline := started.Add(timeout)

	var last statusOutput
	lastLogged := ""
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), minDuration(remaining, a.httpTimeout))
		res, err := a.api.get(ctx, path, nil)
		cancel()
		if err != nil {
			if !time.Now().Before(deadline) {
				break // 本次请求的截止时间正好落在总超时上，按超时处理
			}
			return a.fail(err)
		}
		run, perr := parseRun(res.Body)
		if perr != nil {
			return a.fail(fmt.Errorf("控制面返回了无法解析的 Run: %w", perr))
		}
		out := buildStatusOutput(run)
		last = out
		if out.Status != lastLogged {
			fmt.Fprintf(a.stderr, "tpctl: %s status=%s\n", runID, out.Status)
			lastLogged = out.Status
		}
		if out.Terminal {
			wo := waitOutput{
				statusOutput:   out,
				Outcome:        runOutcome(out.Status),
				ElapsedSeconds: roundSeconds(time.Since(started)),
			}
			a.printWaitResult(wo, *jsonOut)
			return waitExitCode(wo.Outcome)
		}
		if !time.Now().Before(deadline) {
			break
		}
		sleep := interval
		if rest := time.Until(deadline); rest < sleep {
			sleep = rest
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}

	wo := waitOutput{
		statusOutput:   last,
		Outcome:        "timeout",
		ElapsedSeconds: roundSeconds(time.Since(started)),
	}
	if wo.RunID == "" {
		wo.RunID = runID
	}
	a.printWaitResult(wo, *jsonOut)
	fmt.Fprintf(a.stderr, "tpctl: 等待超时（%s），最后状态: %s\n", timeout, statusOrUnknown(wo.Status))
	return exitTimeout
}

func (a *app) printWaitResult(wo waitOutput, jsonOut bool) {
	if jsonOut {
		a.printJSON(wo)
		return
	}
	a.printRunSummary(wo.statusOutput)
	fmt.Fprintf(a.stdout, "  outcome: %s（%.3fs）\n", wo.Outcome, wo.ElapsedSeconds)
}

func (a *app) printRunSummary(out statusOutput) {
	fmt.Fprintf(a.stdout, "%s  %s\n", out.RunID, out.Name)
	fmt.Fprintf(a.stdout, "  status: %s（terminal=%t）\n", out.Status, out.Terminal)
	if counts := parseTaskCounts(out.TaskCounts); counts != nil {
		fmt.Fprintf(a.stdout, "  tasks: %d total, %d succeeded, %d failed, %d skipped, %d running, %d pending\n",
			counts.Total, counts.Succeeded, counts.Failed, counts.Skipped, counts.Running, counts.Pending)
	}
	if out.SubmittedAt != nil {
		fmt.Fprintf(a.stdout, "  submitted_at: %s\n", *out.SubmittedAt)
	}
	if out.FinishedAt != nil {
		fmt.Fprintf(a.stdout, "  finished_at: %s\n", *out.FinishedAt)
	}
	if len(out.Error) > 0 {
		fmt.Fprintf(a.stdout, "  error: %s\n", compactJSON(out.Error))
	}
	for _, t := range out.Tasks {
		line := fmt.Sprintf("    %s %-24s %s", t.ID, t.Name, t.Status)
		if t.WaitingReason != nil && *t.WaitingReason != "" {
			line += "（waiting_reason=" + *t.WaitingReason + "）"
		}
		fmt.Fprintln(a.stdout, line)
	}
}

type taskCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func parseTaskCounts(raw json.RawMessage) *taskCounts {
	if len(raw) == 0 {
		return nil
	}
	var c taskCounts
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	return &c
}

// ---- report / logs --------------------------------------------------------

func (a *app) cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	md := fs.Bool("md", false, "打印 report.md 原文（GET /runs/{id}/report.md）")
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出（单行 JSON）")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) != 1 {
		return a.usageError("用法: tpctl report <run_id> [--md]")
	}

	path := "/api/v1/runs/" + pathEscape(pos[0]) + "/report"
	if *md {
		path += ".md"
	}
	res, err := a.api.get(context.Background(), path, nil)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			hint := "报告尚未生成（run.finished 后异步生成）"
			if apiErr.RetryAfter != "" {
				hint += "，建议 " + apiErr.RetryAfter + " 秒后重试"
			}
			fmt.Fprintf(a.stderr, "tpctl: %s\n", hint)
			return a.fail(err)
		}
		return a.fail(err)
	}

	if *md {
		_, _ = a.stdout.Write(res.Body)
		if len(res.Body) > 0 && res.Body[len(res.Body)-1] != '\n' {
			_, _ = io.WriteString(a.stdout, "\n")
		}
		return exitOK
	}
	if *jsonOut {
		writeCompactJSON(a.stdout, res.Body)
	} else {
		writePrettyJSON(a.stdout, res.Body)
	}
	return exitOK
}

func (a *app) cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	attempt := fs.Int("attempt", 1, "Attempt 序号（从 1 开始）")
	offset := fs.Int64("offset", 0, "起始字节偏移")
	tail := fs.Int("tail", 0, "只取最后 N 行")
	limit := fs.Int64("limit", 0, "本次返回的字节上限（服务端收敛到 ≤64 KiB）")
	stream := fs.String("stream", "", "stdout 或 stderr（缺省 stdout）")
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) != 1 {
		return a.usageError("用法: tpctl logs <task_id> [--attempt 1] [--offset N] [--tail N]")
	}
	if *attempt < 1 {
		return a.usageError("--attempt 必须 ≥1")
	}
	if *stream != "" && *stream != "stdout" && *stream != "stderr" {
		return a.usageError("--stream 只能是 stdout|stderr")
	}
	if *offset < 0 {
		return a.usageError("--offset 必须 ≥0")
	}
	if *limit < 0 || *limit > 65536 {
		return a.usageError("--limit 必须在 0..65536 之间")
	}
	if *tail < 0 {
		return a.usageError("--tail 必须 ≥0")
	}

	q := url.Values{}
	if *stream != "" {
		q.Set("stream", *stream)
	}
	if *offset > 0 {
		q.Set("offset", strconv.FormatInt(*offset, 10))
	}
	if *limit > 0 {
		q.Set("limit", strconv.FormatInt(*limit, 10))
	}
	if *tail > 0 {
		q.Set("tail", strconv.Itoa(*tail))
	}
	path := fmt.Sprintf("/api/v1/tasks/%s/attempts/%d/logs", pathEscape(pos[0]), *attempt)
	res, err := a.api.get(context.Background(), path, q)
	if err != nil {
		return a.fail(err)
	}

	// 续读坐标永远打到 stderr（stdout 保持日志原文/一个 JSON 文档）。
	if rawOffset := res.Header.Get("X-Log-Offset"); rawOffset != "" {
		fmt.Fprintf(a.stderr, "X-Log-Offset: %s\n", rawOffset)
	}
	if coverage := res.Header.Get("X-Log-Coverage"); coverage != "" {
		fmt.Fprintf(a.stderr, "X-Log-Coverage: %s\n", coverage)
	}

	if *jsonOut {
		out := map[string]any{
			"task_id":  pos[0],
			"attempt":  *attempt,
			"stream":   streamOrDefault(*stream),
			"offset":   *offset,
			"coverage": res.Header.Get("X-Log-Coverage"),
			"text":     string(res.Body),
		}
		if *tail > 0 {
			out["tail"] = *tail
		}
		if rawOffset := res.Header.Get("X-Log-Offset"); rawOffset != "" {
			if next, perr := strconv.ParseInt(strings.TrimSpace(rawOffset), 10, 64); perr == nil {
				out["next_offset"] = next
			} else {
				out["next_offset_raw"] = rawOffset
			}
		}
		a.printJSON(out)
		return exitOK
	}

	_, _ = a.stdout.Write(res.Body)
	if len(res.Body) > 0 && res.Body[len(res.Body)-1] != '\n' {
		_, _ = io.WriteString(a.stdout, "\n")
	}
	return exitOK
}

// ---- cancel ---------------------------------------------------------------

type cancelOutput struct {
	Target     string `json:"target"`
	ID         string `json:"id"`
	Accepted   bool   `json:"accepted"`
	HTTPStatus int    `json:"http_status"`
	Status     string `json:"status,omitempty"`
	Confirmed  bool   `json:"confirmed"`
}

func (a *app) cmdCancel(args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	taskID := fs.String("task", "", "取消单个 Task（与 run_id 二选一）")
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}

	if *taskID != "" {
		if len(pos) > 0 {
			return a.usageError("cancel --task <task_id> 不接受位置参数")
		}
		return a.cancelTarget("task", *taskID, *jsonOut)
	}
	if len(pos) != 1 {
		return a.usageError("用法: tpctl cancel <run_id> | --task <task_id>")
	}
	return a.cancelTarget("run", pos[0], *jsonOut)
}

func (a *app) cancelTarget(target, id string, jsonOut bool) int {
	var cancelPath, getPath string
	if target == "task" {
		cancelPath = "/api/v1/tasks/" + pathEscape(id) + "/cancel"
		getPath = "/api/v1/tasks/" + pathEscape(id)
	} else {
		cancelPath = "/api/v1/runs/" + pathEscape(id) + "/cancel"
		getPath = "/api/v1/runs/" + pathEscape(id)
	}

	res, err := a.api.post(context.Background(), cancelPath, nil, nil, "")
	if err != nil {
		return a.fail(err)
	}
	out := cancelOutput{Target: target, ID: id, Accepted: true, HTTPStatus: res.Status}

	// 202/200 后回读一次确认（best-effort）：确认失败不改变"已受理"的事实。
	if confirm, cerr := a.api.get(context.Background(), getPath, nil); cerr == nil {
		out.Status = statusFromBody(confirm.Body)
		out.Confirmed = out.Status != ""
	} else {
		out.Status = statusFromBody(res.Body)
	}

	if jsonOut {
		a.printJSON(out)
		return exitOK
	}
	fmt.Fprintf(a.stdout, "%s %s: %s（HTTP %d，已回读确认=%t）\n",
		target, id, statusOrUnknown(out.Status), out.HTTPStatus, out.Confirmed)
	return exitOK
}

func statusFromBody(body []byte) string {
	var v struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Status
}

// ---- testers / queue / profiles -------------------------------------------

func (a *app) cmdTesters(args []string) int {
	fs := flag.NewFlagSet("testers", flag.ContinueOnError)
	role := fs.String("role", "", "按角色过滤：test|pre_check")
	orphaned := fs.Bool("orphaned", false, "包含已从目录移除的 Tester")
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) > 0 {
		return a.usageError("testers 不接受位置参数")
	}
	if *role != "" && *role != "test" && *role != "pre_check" {
		return a.usageError("--role 只能是 test|pre_check")
	}
	q := url.Values{}
	if *role != "" {
		q.Set("role", *role)
	}
	if *orphaned {
		q.Set("orphaned", "true")
	}

	res, err := a.api.get(context.Background(), "/api/v1/testers", q)
	if err != nil {
		return a.fail(err)
	}
	if *jsonOut {
		writeCompactJSON(a.stdout, res.Body)
		return exitOK
	}

	var list struct {
		Items []struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Role        string `json:"role"`
			Description string `json:"description"`
			Orphaned    bool   `json:"orphaned"`
		} `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &list); err != nil {
		return a.fail(fmt.Errorf("控制面返回了无法解析的 Tester 列表: %w", err))
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(a.stdout, "（没有 Tester）")
		return exitOK
	}
	for _, t := range list.Items {
		suffix := ""
		if t.Orphaned {
			suffix = "  [orphaned]"
		}
		desc := t.Description
		if desc == "" {
			desc = "-"
		}
		fmt.Fprintf(a.stdout, "%s@%s  %-10s %s%s\n", t.Name, t.Version, t.Role, desc, suffix)
	}
	return exitOK
}

func (a *app) cmdQueue(args []string) int {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) > 0 {
		return a.usageError("queue 不接受位置参数")
	}

	res, err := a.api.get(context.Background(), "/api/v1/queue-status", nil)
	if err != nil {
		return a.fail(err)
	}
	if *jsonOut {
		writeCompactJSON(a.stdout, res.Body)
		return exitOK
	}

	var status struct {
		GeneratedAt *string `json:"generated_at"`
		Slots       struct {
			ConcurrencyMax int `json:"concurrency_max"`
			Active         int `json:"active"`
			Available      int `json:"available"`
		} `json:"slots"`
		Waiting struct {
			Total    int            `json:"total"`
			ByReason map[string]int `json:"by_reason"`
		} `json:"waiting"`
		Volcano struct {
			Available bool    `json:"available"`
			Detail    *string `json:"detail"`
		} `json:"volcano"`
	}
	if err := json.Unmarshal(res.Body, &status); err != nil {
		return a.fail(fmt.Errorf("控制面返回了无法解析的队列状态: %w", err))
	}
	if status.GeneratedAt != nil {
		fmt.Fprintf(a.stdout, "generated_at: %s\n", *status.GeneratedAt)
	}
	fmt.Fprintf(a.stdout, "slots: active %d/%d, available %d\n",
		status.Slots.Active, status.Slots.ConcurrencyMax, status.Slots.Available)
	fmt.Fprintf(a.stdout, "waiting: %d\n", status.Waiting.Total)
	for _, reason := range []string{"precheck_gate", "slot_waiting", "global_limit", "queue_full", "insufficient_resources", "gang_pending"} {
		if n := status.Waiting.ByReason[reason]; n > 0 {
			fmt.Fprintf(a.stdout, "  %-22s %d\n", reason, n)
		}
	}
	if status.Volcano.Available {
		fmt.Fprintln(a.stdout, "volcano: available")
	} else {
		detail := ""
		if status.Volcano.Detail != nil {
			detail = " (" + *status.Volcano.Detail + ")"
		}
		fmt.Fprintf(a.stdout, "volcano: unavailable%s（不可达不等于 0 排队）\n", detail)
	}
	return exitOK
}

func (a *app) cmdProfiles(args []string) int {
	fs := flag.NewFlagSet("profiles", flag.ContinueOnError)
	jsonOut := fs.Bool("json", a.jsonOut, "机器可读输出")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return a.flagError(err)
	}
	if len(pos) > 0 {
		return a.usageError("profiles 不接受位置参数")
	}

	res, err := a.api.get(context.Background(), "/api/v1/profiles", nil)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			const hint = "控制面尚未实现 GET /api/v1/profiles（IMPLEMENTATION §15.2「P1 首批」）；当前没有可替代端点，tpctl 不伪造数据。"
			if *jsonOut {
				writeJSONLine(a.stderr, map[string]any{"error": map[string]any{
					"http_status": 404,
					"code":        "ENDPOINT_NOT_IMPLEMENTED",
					"message":     hint,
				}})
			} else {
				fmt.Fprintf(a.stderr, "tpctl: %s\n", hint)
			}
			return exitFailure
		}
		return a.fail(err)
	}

	if *jsonOut {
		writeCompactJSON(a.stdout, res.Body)
		return exitOK
	}
	// 端点尚未落地，Profile 形状不在 openapi 里；按通用 JSON 原样列出，不猜字段。
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &list); err != nil {
		writePrettyJSON(a.stdout, res.Body)
		return exitOK
	}
	fmt.Fprintf(a.stdout, "%d 个 Profile：\n", len(list.Items))
	for _, item := range list.Items {
		fmt.Fprintf(a.stdout, "  %s\n", compactJSON(item))
	}
	return exitOK
}

// ---- 输出与状态小工具 -----------------------------------------------------

func (a *app) flagError(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		usage(a.stdout)
		return exitOK
	}
	return a.usageError("%v", err)
}

func writeCompactJSON(w io.Writer, body []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		_, _ = w.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			_, _ = io.WriteString(w, "\n")
		}
		return
	}
	buf.WriteByte('\n')
	_, _ = w.Write(buf.Bytes())
}

func compactJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func pathEscape(id string) string { return url.PathEscape(id) }

func streamOrDefault(stream string) string {
	if stream == "" {
		return "stdout"
	}
	return stream
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case "succeeded", "completed_with_errors", "service_unavailable", "failed", "canceled":
		return true
	default:
		return false
	}
}

// runOutcome 与 MCP 的 outcome、CLI 退出码一一对应（agent-integration.md §4.3）。
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

func waitExitCode(outcome string) int {
	switch outcome {
	case "success":
		return exitOK
	case "canceled":
		return exitCanceled
	case "failed":
		return exitFailure
	default:
		return exitTimeout
	}
}

func statusOrUnknown(status string) string {
	if status == "" {
		return "未知"
	}
	return status
}

func roundSeconds(d time.Duration) float64 {
	// 保留毫秒精度，避免输出 0.30000000000000004 这类噪声。
	return float64(d.Milliseconds()) / 1000
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
