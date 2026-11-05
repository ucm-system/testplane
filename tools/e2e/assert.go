package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// assertRun 是票 #3 的核心断言：
//   - Run 终态 == --expect
//   - GET /runs/{id}/report 非空（JSON 合法）
//   - GET /runs/{id}/events/history 事件序列合法（seq 单调、转移在表内）
//   - 每个 Task 的每个 Attempt 有日志；succeeded/failed 的 Attempt 有合法 result.json
//   - SSE /runs/{id}/events 连通性冒烟
func (a *App) assertRun(ctx context.Context, run *Run) error {
	a.run = run

	a.checkRunBasics(run)
	a.checkTaskSet(run)
	a.checkRunTaskListComplete(ctx, run)
	a.checkFinalConsistency(run)
	a.checkFailureExpectations(run)
	a.checkPrecheckClassification(run)

	a.checkReport(ctx, run.ID)
	a.checkTaskArtifacts(ctx, run)
	a.collectAndValidateEvents(ctx, run)
	a.checkSSESmoke(ctx, run.ID)

	return a.connectErr
}

// recordError 统一记录 HTTP 层失败；连接类错误会冒泡成退出码 3。
func (a *App) recordError(checkName string, err error) {
	a.addCheck(checkName, false, err.Error())
	if IsConnectError(err) && a.connectErr == nil {
		a.connectErr = err
	}
}

func (a *App) checkRunBasics(run *Run) {
	a.addCheck("run.id_present", run.ID != "", "id="+run.ID)
	if a.opts.RunID != "" {
		a.addCheck("run.id_matches", run.ID == a.opts.RunID,
			fmt.Sprintf("期望 %s，实际 %s", a.opts.RunID, run.ID))
	}
	a.addCheck("run.terminal", isTerminalRun(run.Status),
		fmt.Sprintf("status=%s（终态集合：%s）", run.Status, "succeeded/completed_with_errors/service_unavailable/failed/canceled"))
	a.addCheck("run.expected_status", containsString(a.opts.Expect, run.Status),
		fmt.Sprintf("期望 %s，实际 %s", strings.Join(a.opts.Expect, ","), run.Status))
	if run.Kind != "" && run.Kind != "run" {
		a.warn("Run 详情 kind=%q，期望 run（kind=task 只用于独立 Task 合成的占位 Run）", run.Kind)
	}
	if run.SubmittedAt == "" {
		a.warn("Run 详情缺 submitted_at（07 §5.1）")
	}
	if !allRunStatuses[run.Status] {
		a.addCheck("run.status_token", false, fmt.Sprintf("Run 状态 token 非法：%q", run.Status))
	}
}

func (a *App) checkTaskSet(run *Run) {
	if len(run.Tasks) == 0 {
		a.addCheck("run.tasks_present", false, "Run 详情没有 tasks[]（precheck:auto 至少应注入一个 Task）")
		return
	}
	a.addCheck("run.tasks_present", true, fmt.Sprintf("%d 个 Task", len(run.Tasks)))

	var badTasks, skippedWithAttempts []string
	seenNames := map[string]bool{}
	for _, t := range run.Tasks {
		if t.ID == "" || t.Name == "" {
			badTasks = append(badTasks, fmt.Sprintf("Task 缺 id/name：id=%q name=%q", t.ID, t.Name))
		}
		if !allTaskStatuses[t.Status] {
			badTasks = append(badTasks, fmt.Sprintf("%s 的状态 token 非法：%q", t.Name, t.Status))
		}
		if seenNames[t.Name] {
			badTasks = append(badTasks, "Task 名重复："+t.Name)
		}
		seenNames[t.Name] = true
		if t.Status == "skipped" && t.Attempts > 0 {
			skippedWithAttempts = append(skippedWithAttempts,
				fmt.Sprintf("%s skipped 却有 %d 个 Attempt（C-5.3/C-5.5：skipped 不产生 Attempt）", t.Name, t.Attempts))
		}
	}
	a.addCheck("run.tasks_valid", len(badTasks) == 0, firstN(badTasks, 5, "Task id/name/状态 token 合法"))
	a.addCheck("run.skipped_without_attempt", len(skippedWithAttempts) == 0,
		firstN(skippedWithAttempts, 5, "skipped Task 均无 Attempt"))
}

func (a *App) checkFinalConsistency(run *Run) {
	// 终态 Run 不允许有悬挂 Task（C-5.8 末段）。
	var dangling []string
	for _, t := range run.Tasks {
		if !isTerminalTask(t.Status) && isTerminalRun(run.Status) {
			dangling = append(dangling, fmt.Sprintf("%s=%s", t.Name, t.Status))
		}
	}
	a.addCheck("run.all_tasks_terminal", len(dangling) == 0,
		firstN(dangling, 5, "Run 终态时所有 Task 均已终态"))

	switch run.Status {
	case "succeeded":
		var bad []string
		for _, t := range run.Tasks {
			if t.Status == "failed" || t.Status == "canceled" {
				bad = append(bad, fmt.Sprintf("%s=%s", t.Name, t.Status))
			}
		}
		a.addCheck("run.succeeded_no_failed_tasks", len(bad) == 0,
			firstN(bad, 5, "无 failed/canceled Task"))
		a.addCheck("run.succeeded_has_task", atLeastOneStatus(run.Tasks, "succeeded"),
			"至少一个 Task succeeded")

	case "service_unavailable":
		// 两条产生路径（C-5.3 / C-5.8 #3、#4）：
		//   1) 部署未就绪或 blocked：全部 Task skipped，不产生 Attempt；
		//   2) pre_check 可重试失败（窗口内一直不可达 / 5xx）：precheck failed，其余 skipped(upstream_abort)。
		var bad []string
		for _, t := range run.Tasks {
			switch {
			case t.Status == "skipped":
				if t.Attempts > 0 {
					bad = append(bad, fmt.Sprintf("%s=skipped 但有 %d 个 Attempt", t.Name, t.Attempts))
				}
			case t.Name == "precheck" && t.Status == "failed":
				// 可重试失败的 precheck 允许 failed；错误码/retryable 由 expect.* 检查覆盖
			default:
				bad = append(bad, fmt.Sprintf("%s=%s", t.Name, t.Status))
			}
		}
		a.addCheck("run.unavailable_tasks_skipped", len(bad) == 0,
			firstN(bad, 5, "除 precheck 可重试失败外，其余 Task 均为 skipped 且无 Attempt"))
		if run.Error == nil {
			a.warn("service_unavailable 但没有 run.error（07 §5.1 的 error 字段；排障时缺原因）")
		} else {
			a.addCheck("run.error_present", true, run.Error.Code)
		}

	case "completed_with_errors":
		failures := 0
		var failedNames []string
		for _, t := range run.Tasks {
			if t.Status == "failed" || t.Status == "skipped" {
				failures++
				failedNames = append(failedNames, fmt.Sprintf("%s=%s", t.Name, t.Status))
			}
		}
		a.addCheck("run.has_failures", failures > 0,
			firstN(failedNames, 5, "completed_with_errors 至少有一个失败/跳过 Task"))

	case "failed":
		if run.Error == nil {
			a.addCheck("run.error_present", false, "failed 是平台侧错误，必须带 run.error")
		} else {
			a.addCheck("run.error_present", run.Error.Code != "",
				fmt.Sprintf("run.error.code=%q", run.Error.Code))
		}

	case "canceled":
		var bad []string
		for _, t := range run.Tasks {
			if t.Status != "skipped" && t.Status != "canceled" && t.Status != "succeeded" {
				bad = append(bad, fmt.Sprintf("%s=%s", t.Name, t.Status))
			}
		}
		a.addCheck("run.canceled_tasks", len(bad) == 0, firstN(bad, 5, "取消后 Task 均为 skipped/canceled"))
	}
}

// checkFailureExpectations 处理 --expect-error-code / --expect-task-error-code /
// --expect-retryable 三个可选期望，并在 precheck 失败时要求带错误码（C-5.8、07 §3）。
func (a *App) checkFailureExpectations(run *Run) {
	if a.opts.ExpectErrorCode != "" {
		ok := run.Error != nil && run.Error.Code == a.opts.ExpectErrorCode
		var actual string
		if run.Error != nil {
			actual = run.Error.Code
		}
		a.addCheck("expect.run_error_code", ok,
			fmt.Sprintf("期望 run.error.code=%s，实际 %q", a.opts.ExpectErrorCode, actual))
	}

	var matched *Failure
	var matchedTask string
	if a.opts.ExpectTaskErrorCode != "" {
		for _, t := range run.Tasks {
			if t.Error != nil && t.Error.Code == a.opts.ExpectTaskErrorCode {
				matched = t.Error
				matchedTask = t.Name
				break
			}
		}
		a.addCheck("expect.task_error_code", matched != nil,
			fmt.Sprintf("期望某个 Task 的 error.code=%s，实际未命中", a.opts.ExpectTaskErrorCode))
	}

	if a.opts.ExpectRetryable != nil {
		target := matched
		where := matchedTask
		if target == nil {
			for _, t := range run.Tasks {
				if t.Error != nil {
					target = t.Error
					where = t.Name
					break
				}
			}
		}
		if target == nil {
			target = run.Error
			where = "run.error"
		}
		if target == nil {
			a.addCheck("expect.retryable", false,
				fmt.Sprintf("没有找到任何 Failure 对象，无法校验 retryable（期望 %v）", *a.opts.ExpectRetryable))
		} else {
			a.addCheck("expect.retryable", target.Retryable == *a.opts.ExpectRetryable,
				fmt.Sprintf("%s.retryable=%v，期望 %v", where, target.Retryable, *a.opts.ExpectRetryable))
		}
	}

	// Pre-Check 失败必须带实例级错误码与 retryable（01 UC-07、07 §3、C-5.8）
	for _, t := range run.Tasks {
		if t.Name != "precheck" || t.Status != "failed" {
			continue
		}
		if t.Error == nil || t.Error.Code == "" {
			a.addCheck("precheck.failed_error", false, "precheck failed 但 Task 上没有 error.code")
			break
		}
		a.addCheck("precheck.failed_error", true,
			fmt.Sprintf("code=%s retryable=%v", t.Error.Code, t.Error.Retryable))
		break
	}
}

func (a *App) checkReport(ctx context.Context, runID string) {
	if a.connectErr != nil {
		return
	}
	body, status, err := a.fetchReport(ctx, runID)
	if err != nil {
		a.recordError("report.present", err)
		return
	}
	if status != 200 {
		a.addCheck("report.present", false, fmt.Sprintf("HTTP %d：%s", status, truncate(string(body), 400)))
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		a.addCheck("report.present", false, "report.json 是空响应体")
		return
	}
	a.addCheck("report.present", true, fmt.Sprintf("%d 字节", len(trimmed)))

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		a.addCheck("report.json_valid", false, fmt.Sprintf("report 不是合法 JSON：%v", err))
		return
	}
	if len(obj) == 0 {
		a.addCheck("report.json_valid", false, "report 是空对象 {}")
		return
	}
	a.addCheck("report.json_valid", true, fmt.Sprintf("%d 个顶层字段", len(obj)))

	// report.md 是次要产物：缺了只警告（07 §5.1 的 report.md 与 report 异步生成）。
	mdStatus, _, md, err := a.client.Get(ctx, "/runs/"+url.PathEscape(runID)+"/report.md")
	if err != nil {
		a.warn("GET /report.md 失败：%v", err)
		return
	}
	if mdStatus == 200 && strings.TrimSpace(string(md)) != "" {
		a.addCheck("report.md", true, fmt.Sprintf("%d 字节", len(md)))
	} else {
		a.warn("GET /report.md 未返回内容（HTTP %d）", mdStatus)
	}
}

// fetchReport 处理"报告异步生成"：未就绪时按 404 + Retry-After 重试，最多 30s。
func (a *App) fetchReport(ctx context.Context, runID string) ([]byte, int, error) {
	path := "/runs/" + url.PathEscape(runID) + "/report"
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, header, body, err := a.client.Get(ctx, path)
		if err != nil {
			return nil, 0, err
		}
		if status == 200 || status != 404 || time.Now().After(deadline) {
			return body, status, nil
		}
		wait := retryAfterSeconds(header, 2*time.Second, 5*time.Second)
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// checkTaskArtifacts 检查每个有 Attempt 的 Task：日志非空、result.json 合法、
// attempts.idx 从 1 连续（C-3.4 不变量）。
func (a *App) checkTaskArtifacts(ctx context.Context, run *Run) {
	if a.connectErr != nil {
		return
	}
	for _, task := range run.Tasks {
		if task.Attempts <= 0 {
			continue
		}

		attempts, fromAPI, err := a.fetchAttempts(ctx, task.ID)
		if err != nil {
			a.recordError(fmt.Sprintf("task.%s.attempts", task.Name), err)
			if a.connectErr != nil {
				return
			}
			continue
		}
		if !fromAPI || len(attempts) == 0 {
			if fromAPI {
				a.warn("Task %s 详情里 attempts 为空，按摘要的 attempts=%d 合成 1..N", task.Name, task.Attempts)
			} else {
				a.warn("Task %s 的 Attempt 列表不可读（GET /tasks/{id}/attempts 与详情都拿不到），按摘要 attempts=%d 合成 1..N", task.Name, task.Attempts)
			}
			attempts = make([]Attempt, 0, task.Attempts)
			for i := 1; i <= task.Attempts; i++ {
				attempts = append(attempts, Attempt{Idx: i, Status: task.Status})
			}
		} else {
			if len(attempts) != task.Attempts {
				a.warn("Task %s 的 attempts 列表有 %d 条，但 Run 详情摘要写 %d", task.Name, len(attempts), task.Attempts)
			}
			sort.Slice(attempts, func(i, j int) bool { return attempts[i].Number() < attempts[j].Number() })
			var gaps []string
			for i, at := range attempts {
				if at.Number() != i+1 {
					gaps = append(gaps, fmt.Sprintf("第 %d 条 attempt 的 idx=%d（期望 %d，C-3.4：从 1 连续递增）", i+1, at.Number(), i+1))
				}
			}
			a.addCheck(fmt.Sprintf("task.%s.attempts_continuous", task.Name), len(gaps) == 0,
				firstN(gaps, 3, "attempts.idx 从 1 连续递增"))
		}

		for _, at := range attempts {
			idx := at.Number()
			if idx == 0 {
				idx = 1
			}
			attemptStatus := at.Status
			if attemptStatus == "" {
				attemptStatus = task.Status
			}
			attemptErr := at.Error
			if attemptErr == nil {
				attemptErr = task.Error
			}
			prefix := fmt.Sprintf("task.%s.attempt.%d", task.Name, idx)

			logs, logStatus, streamName, err := a.fetchLogs(ctx, task.ID, idx)
			switch {
			case err != nil:
				a.recordError(prefix+".logs", err)
				if a.connectErr != nil {
					return
				}
			case logStatus != 200 && attemptErr != nil && isPlatformSetupFailure(attemptErr.Code):
				// Job 根本没建起来（executor/镜像/挂载失败）时没有 Pod 日志是正常的；
				// 这类平台准备期失败只警告，别把"没日志"误判成日志接口坏了。
				a.warn("Task %s attempt %d 没有日志（平台准备期失败 %s，HTTP %d）",
					task.Name, idx, attemptErr.Code, logStatus)
			case logStatus != 200:
				a.addCheck(prefix+".logs", false, fmt.Sprintf("HTTP %d（GET /tasks/{id}/attempts/%d/logs）", logStatus, idx))
			case strings.TrimSpace(string(logs)) == "":
				if a.opts.AllowEmptyLogs {
					a.warn("Task %s attempt %d 的 %s 日志为空（--allow-empty-logs 放行）", task.Name, idx, streamName)
				} else {
					a.addCheck(prefix+".logs", false, fmt.Sprintf("%s 日志为空（GET /tasks/{id}/attempts/%d/logs）", streamName, idx))
				}
			default:
				a.addCheck(prefix+".logs", true, fmt.Sprintf("%s %d 字节", streamName, len(logs)))
			}

			if attemptStatus != "succeeded" && attemptStatus != "failed" {
				continue
			}
			body, status, err := a.fetchResult(ctx, task.ID, idx)
			if err != nil {
				a.recordError(prefix+".result", err)
				if a.connectErr != nil {
					return
				}
				continue
			}
			if status != 200 && attemptErr != nil && isPlatformSetupFailure(attemptErr.Code) {
				a.warn("Task %s attempt %d 没有 result.json（平台准备期失败 %s，HTTP %d）",
					task.Name, idx, attemptErr.Code, status)
				continue
			}
			if status != 200 {
				a.addCheck(prefix+".result", false, fmt.Sprintf("HTTP %d（GET /tasks/{id}/attempts/%d/result）", status, idx))
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				a.addCheck(prefix+".result", false, fmt.Sprintf("result.json 不是合法 JSON：%v", err))
				continue
			}
			if len(obj) == 0 {
				a.addCheck(prefix+".result", false, "result.json 是空对象 {}")
				continue
			}
			a.addCheck(prefix+".result", true, fmt.Sprintf("%d 个顶层字段", len(obj)))
		}
	}
}

// fetchAttempts 先走 GET /tasks/{id}/attempts，404 时回退到 GET /tasks/{id} 的 attempts 数组。
func (a *App) fetchAttempts(ctx context.Context, taskID string) ([]Attempt, bool, error) {
	path := "/tasks/" + url.PathEscape(taskID) + "/attempts"
	status, _, body, err := a.client.Get(ctx, path)
	if err != nil {
		return nil, false, err
	}
	if status == 200 {
		if list, ok := decodeAttemptList(body); ok {
			return list, true, nil
		}
		return nil, false, fmt.Errorf("GET /tasks/%s/attempts 响应无法解析：%s", taskID, truncate(string(body), 300))
	}
	if status != 404 {
		return nil, false, fmt.Errorf("GET /tasks/%s/attempts 期望 200，实际 HTTP %d：%s", taskID, status, truncate(string(body), 300))
	}

	// 回退：Task 详情里可能带 attempts 数组
	status, _, body, err = a.client.Get(ctx, "/tasks/"+url.PathEscape(taskID))
	if err != nil {
		return nil, false, err
	}
	if status != 200 {
		return nil, false, fmt.Errorf("GET /tasks/%s 期望 200，实际 HTTP %d", taskID, status)
	}
	if list, ok := decodeAttemptList(body); ok {
		return list, true, nil
	}
	return nil, false, nil
}

// decodeAttemptList 接受裸数组或 {items|attempts|executions:[...]} 三种形状。
func decodeAttemptList(raw []byte) ([]Attempt, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false
	}
	if trimmed[0] == '[' {
		var list []Attempt
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, false
		}
		return list, true
	}
	var obj struct {
		Items      []Attempt `json:"items"`
		Attempts   []Attempt `json:"attempts"`
		Executions []Attempt `json:"executions"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil, false
	}
	switch {
	case obj.Attempts != nil:
		return obj.Attempts, true
	case obj.Items != nil:
		return obj.Items, true
	case obj.Executions != nil:
		return obj.Executions, true
	}
	return nil, false
}

// fetchLogs 先读 stdout，空则读 stderr；任一非空即算"有日志"。
func (a *App) fetchLogs(ctx context.Context, taskID string, idx int) ([]byte, int, string, error) {
	lastStatus := 0
	var lastBody []byte
	for _, stream := range []string{"stdout", "stderr"} {
		path := fmt.Sprintf("/tasks/%s/attempts/%d/logs?stream=%s&limit=65536", url.PathEscape(taskID), idx, stream)
		status, _, body, err := a.client.Get(ctx, path)
		if err != nil {
			return nil, 0, stream, err
		}
		lastStatus, lastBody = status, body
		if status == 200 && strings.TrimSpace(string(body)) != "" {
			return body, status, stream, nil
		}
		if status != 200 && status != 404 {
			return body, status, stream, nil
		}
	}
	return lastBody, lastStatus, "stdout/stderr", nil
}

func (a *App) fetchResult(ctx context.Context, taskID string, idx int) ([]byte, int, error) {
	path := fmt.Sprintf("/tasks/%s/attempts/%d/result", url.PathEscape(taskID), idx)
	status, _, body, err := a.client.Get(ctx, path)
	return body, status, err
}

// collectAndValidateEvents 取事件并做合法性校验，同时记录摘要给机器输出。
func (a *App) collectAndValidateEvents(ctx context.Context, run *Run) {
	if a.connectErr != nil {
		return
	}
	events := a.collectEvents(ctx, run.ID)
	a.events = events
	a.validateEvents(events, run)
}

// fetchRun 供 assert/wait 之外的命令按 id 取 Run。
func (a *App) fetchRun(ctx context.Context, runID string) (*Run, error) {
	status, _, body, err := a.client.Get(ctx, "/runs/"+url.PathEscape(runID))
	if err != nil {
		return nil, err
	}
	if err := expectStatus(status, 200, body); err != nil {
		return nil, fmt.Errorf("查询 Run %s 失败：%w", runID, err)
	}
	var run Run
	if err := json.Unmarshal(body, &run); err != nil {
		return nil, fmt.Errorf("GET /runs/%s 返回的不是合法 JSON：%v", runID, err)
	}
	if run.ID == "" {
		return nil, fmt.Errorf("GET /runs/%s 响应缺 id", runID)
	}
	return &run, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// checkRunTaskListComplete 校验 GET /runs/{id}/tasks 覆盖 Run 详情里的全部 Task。
// 这条抓的是真实回归：分页实现若用 `seq > cursor` 且空 cursor 解为 0，
// 首屏会漏掉 seq=0 的 precheck。
func (a *App) checkRunTaskListComplete(ctx context.Context, run *Run) {
	status, _, body, err := a.client.Get(ctx, "/runs/"+url.PathEscape(run.ID)+"/tasks?limit=200")
	if err != nil {
		a.recordError("run.tasks_list_complete", err)
		return
	}
	if status != 200 {
		a.addCheck("run.tasks_list_complete", false,
			fmt.Sprintf("GET /runs/{id}/tasks 期望 200，实际 %d：%s", status, truncate(string(body), 300)))
		return
	}
	var page struct {
		Items   []Task `json:"items"`
		HasMore bool   `json:"has_more"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		a.addCheck("run.tasks_list_complete", false, "任务列表不是合法 JSON："+err.Error())
		return
	}
	seen := map[string]bool{}
	for _, t := range page.Items {
		seen[t.ID] = true
	}
	var missing []string
	for _, t := range run.Tasks {
		if !seen[t.ID] {
			missing = append(missing, fmt.Sprintf("seq=%d %s(%s) 状态=%s", t.Seq, t.Name, t.ID, t.Status))
		}
	}
	detail := "任务列表覆盖 Run 详情里的全部 Task（含 seq=0 precheck）"
	if page.HasMore {
		detail = "任务列表首页 has_more=true，未翻页；已拿到 " + strconv.Itoa(len(page.Items)) + " 条"
	}
	a.addCheck("run.tasks_list_complete", len(missing) == 0, firstN(missing, 5, detail))
}

// isPlatformSetupFailure 判断"Job/容器根本没起来"的错误码：
// 这类 Attempt 没有 Pod 日志与 result.json 是正常的，不该按"产物缺失"判红。
func isPlatformSetupFailure(code string) bool {
	switch code {
	case "EXECUTOR_UNAVAILABLE", "IMAGE_PULL_FAILED", "MOUNT_UNAVAILABLE", "ARTIFACT_QUOTA_EXCEEDED":
		return true
	}
	return false
}

// isPrecheckRetryableFailure 判断错误码是否属于 C-5.8 #4 的"可重试 precheck 失败"：
// 只有这类失败才把 Run 归为 service_unavailable。
func isPrecheckRetryableFailure(code string) bool {
	switch code {
	case "ENDPOINT_UNREACHABLE", "PRECHECK_FAILED", "TASK_TIMEOUT":
		return true
	}
	return false
}

// checkPrecheckClassification 把 C-5.8 的终态归类规则落成断言：
//   - precheck 失败后其余 Task 必须 skipped（abort_run 传播，不能挂着 pending/running）；
//   - retryable=true 的探活失败 → service_unavailable；
//   - retryable=false 的探活失败（401/403、200 包错误、DNS/TLS）→ completed_with_errors；
//   - 平台准备期失败（如 EXECUTOR_UNAVAILABLE）不适用上述归类，归 completed_with_errors。
func (a *App) checkPrecheckClassification(run *Run) {
	var pre *Task
	for i := range run.Tasks {
		if run.Tasks[i].Name == "precheck" {
			pre = &run.Tasks[i]
			break
		}
	}
	if pre == nil || pre.Status != "failed" {
		return
	}

	var notSkipped []string
	for _, t := range run.Tasks {
		if t.ID == pre.ID {
			continue
		}
		if t.Status != "skipped" {
			notSkipped = append(notSkipped, fmt.Sprintf("%s=%s", t.Name, t.Status))
		} else if t.StatusReason != "" && t.StatusReason != "upstream_abort" {
			a.warn("precheck 失败后 %s 的 skip reason=%q（C-5.5 期望 upstream_abort；实际以 backend 为准）", t.Name, t.StatusReason)
		}
	}
	a.addCheck("run.abort_propagation", len(notSkipped) == 0,
		firstN(notSkipped, 5, "precheck 失败后其余 Task 全部 skipped（abort_run 传播）"))

	if pre.Error == nil {
		a.warn("precheck failed 但没有 error 对象，无法核对 retryable 与终态归类")
		return
	}
	switch run.Status {
	case "service_unavailable":
		ok := isPrecheckRetryableFailure(pre.Error.Code) && pre.Error.Retryable
		a.addCheck("run.precheck_retryable_consistent", ok,
			fmt.Sprintf("service_unavailable 的 precheck 失败应为可重试探活失败，实际 code=%s retryable=%v",
				pre.Error.Code, pre.Error.Retryable))
	case "completed_with_errors":
		bad := isPrecheckRetryableFailure(pre.Error.Code) && pre.Error.Retryable
		a.addCheck("run.precheck_retryable_consistent", !bad,
			fmt.Sprintf("completed_with_errors 不应由可重试探活失败导致，实际 code=%s retryable=%v",
				pre.Error.Code, pre.Error.Retryable))
	}
}

func atLeastOneStatus(tasks []Task, status string) bool {
	for _, t := range tasks {
		if t.Status == status {
			return true
		}
	}
	return false
}
