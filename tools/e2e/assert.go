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

// assertTask 是 e2e 的核心断言（v2：Task 是一等对象，runner 是执行单元）：
//   - Task 终态 == --expect
//   - GET /tasks/{id}/report 非空（JSON 合法）
//   - GET /tasks/{id}/events/history 事件序列合法（seq 单调、转移在表内）
//   - 终态 runner 有日志；succeeded/failed 的 test/pre_check runner 有合法 result.json 或三选一产物
//   - SSE /tasks/{id}/events 连通性冒烟
func (a *App) assertTask(ctx context.Context, task *Task) error {
	a.task = task

	a.checkTaskBasics(task)
	a.checkRunnerSet(task)
	a.checkRunnerListComplete(ctx, task)
	a.checkFinalConsistency(task)
	a.checkFailureExpectations(task)
	a.checkPrecheckClassification(task)

	a.checkReport(ctx, task.ID)
	a.checkRunnerArtifacts(ctx, task)
	a.checkAttemptContinuity(task)
	a.collectAndValidateEvents(ctx, task)
	a.checkSSESmoke(ctx, task.ID)

	return a.connectErr
}

// recordError 统一记录 HTTP 层失败；连接类错误会冒泡成退出码 3。
func (a *App) recordError(checkName string, err error) {
	a.addCheck(checkName, false, err.Error())
	if IsConnectError(err) && a.connectErr == nil {
		a.connectErr = err
	}
}

func (a *App) checkTaskBasics(task *Task) {
	a.addCheck("task.id_present", task.ID != "", "id="+task.ID)
	if a.opts.TaskID != "" {
		a.addCheck("task.id_matches", task.ID == a.opts.TaskID,
			fmt.Sprintf("期望 %s，实际 %s", a.opts.TaskID, task.ID))
	}
	a.addCheck("task.terminal", isTerminalTask(task.Status),
		fmt.Sprintf("status=%s（终态集合：succeeded/failed/service_unavailable/canceled）", task.Status))
	a.addCheck("task.expected_status", containsString(a.opts.Expect, task.Status),
		fmt.Sprintf("期望 %s，实际 %s", strings.Join(a.opts.Expect, ","), task.Status))
	if task.SubmittedAt == "" {
		a.warn("Task 详情缺 submitted_at（07 §5.1）")
	}
	if !allTaskStatuses[task.Status] {
		a.addCheck("task.status_token", false, fmt.Sprintf("Task 状态 token 非法：%q", task.Status))
	}
}

func (a *App) checkRunnerSet(task *Task) {
	if len(task.Runners) == 0 {
		a.addCheck("task.runners_present", false, "Task 详情没有 runners[]（07 §5.1：详情带最新 attempt 的 runner 摘要）")
		return
	}
	a.addCheck("task.runners_present", true, fmt.Sprintf("%d 个 runner（含注入的 service-deploy/precheck/service-clean）", len(task.Runners)))

	var bad []string
	seenSeq := map[int]bool{}
	seenNames := map[string]bool{}
	for _, r := range task.Runners {
		if r.Name == "" || r.Seq <= 0 {
			bad = append(bad, fmt.Sprintf("runner 缺 name/seq：name=%q seq=%d", r.Name, r.Seq))
		}
		if !allRunnerStatuses[r.Status] {
			bad = append(bad, fmt.Sprintf("%s 的状态 token 非法：%q", r.Name, r.Status))
		}
		if seenSeq[r.Seq] {
			bad = append(bad, fmt.Sprintf("seq 重复：%d", r.Seq))
		}
		seenSeq[r.Seq] = true
		if seenNames[r.Name] {
			bad = append(bad, "runner 名重复："+r.Name)
		}
		seenNames[r.Name] = true
		if r.Status == "skipped" && !skippedReasons[r.StatusReason] {
			bad = append(bad, fmt.Sprintf("%s skipped 但 status_reason=%q（D-06：必须带 upstream_abort/canceled）", r.Name, r.StatusReason))
		}
	}
	a.addCheck("task.runners_valid", len(bad) == 0, firstN(bad, 5, "runner name/seq/状态 token/skip reason 合法"))
}

func (a *App) checkFinalConsistency(task *Task) {
	// 终态 Task 不允许有悬挂 runner（§7.4）。
	var dangling []string
	for _, r := range task.Runners {
		if !isTerminalRunner(r.Status) && isTerminalTask(task.Status) {
			dangling = append(dangling, fmt.Sprintf("%s=%s", r.Name, r.Status))
		}
	}
	a.addCheck("task.all_runners_terminal", len(dangling) == 0,
		firstN(dangling, 5, "Task 终态时所有 runner 均已终态"))

	switch task.Status {
	case "succeeded":
		var bad []string
		for _, r := range task.Runners {
			if r.Status == "failed" {
				bad = append(bad, fmt.Sprintf("%s=%s", r.Name, r.Status))
			}
		}
		a.addCheck("task.succeeded_no_failed_runners", len(bad) == 0,
			firstN(bad, 5, "无 failed runner"))
		a.addCheck("task.succeeded_has_runner", atLeastOneStatus(task.Runners, "succeeded"),
			"至少一个 runner succeeded")

	case "service_unavailable":
		// 两条产生路径（C-5.8 v2 / §7.5）：
		//   1) 部署 blocked：全部测试类 runner skipped，不执行；
		//   2) pre_check 可重试失败（窗口内一直不可达 / 5xx）：precheck failed，其余 skipped(upstream_abort)。
		var bad []string
		for _, r := range task.Runners {
			switch {
			case r.Status == "skipped":
				// 合法
			case r.Name == "precheck" && r.Status == "failed":
				// 可重试失败的 precheck 允许 failed；错误码/retryable 由 expect.* 检查覆盖
			default:
				bad = append(bad, fmt.Sprintf("%s=%s", r.Name, r.Status))
			}
		}
		a.addCheck("task.unavailable_runners_skipped", len(bad) == 0,
			firstN(bad, 5, "除 precheck 可重试失败外，其余 runner 均为 skipped"))
		if task.Error == nil {
			a.warn("service_unavailable 但没有 task.error（07 §5.1 的 error 字段；排障时缺原因）")
		} else {
			a.addCheck("task.error_present", true, task.Error.Code)
		}

	case "failed":
		if task.Error == nil {
			// failed 也可以是"存在测试类 runner failed 且放行走完"（§7.5 聚合），
			// 这时不带 task.error 是合法的；只有平台侧错误必须带 error。
			hasFailedRunner := false
			for _, r := range task.Runners {
				if r.Status == "failed" {
					hasFailedRunner = true
					break
				}
			}
			if !hasFailedRunner {
				a.addCheck("task.error_present", false, "failed 且没有任何 failed runner（平台侧错误）时必须带 task.error")
			}
		} else {
			a.addCheck("task.error_present", task.Error.Code != "",
				fmt.Sprintf("task.error.code=%q", task.Error.Code))
		}

	case "canceled":
		var bad []string
		for _, r := range task.Runners {
			if r.Status != "skipped" && r.Status != "canceled" && r.Status != "succeeded" {
				bad = append(bad, fmt.Sprintf("%s=%s", r.Name, r.Status))
			}
		}
		a.addCheck("task.canceled_runners", len(bad) == 0, firstN(bad, 5, "取消后 runner 均为 skipped/canceled"))
	}
}

// checkFailureExpectations 处理 --expect-error-code / --expect-runner-error-code /
// --expect-retryable 三个可选期望，并在 precheck 失败时要求带错误码（§7.5、07 §3）。
func (a *App) checkFailureExpectations(task *Task) {
	if a.opts.ExpectErrorCode != "" {
		ok := task.Error != nil && task.Error.Code == a.opts.ExpectErrorCode
		var actual string
		if task.Error != nil {
			actual = task.Error.Code
		}
		a.addCheck("expect.task_error_code", ok,
			fmt.Sprintf("期望 task.error.code=%s，实际 %q", a.opts.ExpectErrorCode, actual))
	}

	var matched *Failure
	var matchedRunner string
	if a.opts.ExpectRunnerErrCode != "" {
		for i := range task.Runners {
			if task.Runners[i].Error != nil && task.Runners[i].Error.Code == a.opts.ExpectRunnerErrCode {
				matched = task.Runners[i].Error
				matchedRunner = task.Runners[i].Name
				break
			}
		}
		a.addCheck("expect.runner_error_code", matched != nil,
			fmt.Sprintf("期望某个 runner 的 error.code=%s，实际未命中", a.opts.ExpectRunnerErrCode))
	}

	if a.opts.ExpectRetryable != nil {
		target := matched
		where := matchedRunner
		if target == nil {
			for i := range task.Runners {
				if task.Runners[i].Error != nil {
					target = task.Runners[i].Error
					where = task.Runners[i].Name
					break
				}
			}
		}
		if target == nil {
			target = task.Error
			where = "task.error"
		}
		if target == nil {
			a.addCheck("expect.retryable", false,
				fmt.Sprintf("没有找到任何 Failure 对象，无法校验 retryable（期望 %v）", *a.opts.ExpectRetryable))
		} else {
			a.addCheck("expect.retryable", target.Retryable == *a.opts.ExpectRetryable,
				fmt.Sprintf("%s.retryable=%v，期望 %v", where, target.Retryable, *a.opts.ExpectRetryable))
		}
	}

	// Pre-Check 失败必须带实例级错误码与 retryable（§7.5、07 §3）
	for i := range task.Runners {
		r := &task.Runners[i]
		if r.Name != "precheck" || r.Status != "failed" {
			continue
		}
		if r.Error == nil || r.Error.Code == "" {
			a.addCheck("precheck.failed_error", false, "precheck failed 但 runner 上没有 error.code")
			break
		}
		a.addCheck("precheck.failed_error", true,
			fmt.Sprintf("code=%s retryable=%v", r.Error.Code, r.Error.Retryable))
		break
	}
}

func (a *App) checkReport(ctx context.Context, taskID string) {
	if a.connectErr != nil {
		return
	}
	body, status, err := a.fetchReport(ctx, taskID)
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
	mdStatus, _, md, err := a.client.Get(ctx, "/tasks/"+url.PathEscape(taskID)+"/report.md")
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

// fetchReport 处理"报告异步生成"：未就绪时按 404 + Retry-After 重试。
// C-7.2 的 SLA 是终态后 ≤2s；默认 30s 窗口足够，超过就是 hook 没触发。
func (a *App) fetchReport(ctx context.Context, taskID string) ([]byte, int, error) {
	path := "/tasks/" + url.PathEscape(taskID) + "/report"
	wait := a.opts.ReportWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
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

// checkRunnerArtifacts 检查最新 attempt 里每个终态的 test/pre_check runner：
// 日志非空、result.json 合法（产物三选一 D-74：只产出 html/csv 时 404 属正常，detail.produced 给证据）。
func (a *App) checkRunnerArtifacts(ctx context.Context, task *Task) {
	if a.connectErr != nil {
		return
	}
	attemptNo := task.CurrentAttempt
	if attemptNo < 1 {
		attemptNo = 1
	}
	for i := range task.Runners {
		r := &task.Runners[i]
		if r.Role == "deploy" || r.Role == "clean" {
			// deploy/clean 角色无 result 产物（07 §5.2），日志仍然要求非空。
		}
		if !isTerminalRunner(r.Status) {
			continue
		}
		if r.Status == "skipped" || r.Status == "canceled" {
			continue // 没跑的 runner 没有日志与产物
		}
		prefix := fmt.Sprintf("runner.%s", r.Name)

		logs, logStatus, err := a.fetchRunnerLogs(ctx, task.ID, attemptNo, r.Seq)
		switch {
		case err != nil:
			a.recordError(prefix+".logs", err)
			if a.connectErr != nil {
				return
			}
		case logStatus != 200 && r.Error != nil && isPlatformSetupFailure(r.Error.Code):
			// Job 根本没建起来（executor/镜像/挂载失败）时没有 Pod 日志是正常的；
			// 这类平台准备期失败只警告，别把"没日志"误判成日志接口坏了。
			a.warn("runner %s 没有日志（平台准备期失败 %s，HTTP %d）", r.Name, r.Error.Code, logStatus)
		case logStatus != 200:
			a.addCheck(prefix+".logs", false,
				fmt.Sprintf("HTTP %d（GET /tasks/{id}/attempts/%d/runners/%d/logs）", logStatus, attemptNo, r.Seq))
		case strings.TrimSpace(string(logs)) == "":
			if a.opts.AllowEmptyLogs {
				a.warn("runner %s 的日志为空（--allow-empty-logs 放行）", r.Name)
			} else {
				a.addCheck(prefix+".logs", false, fmt.Sprintf("日志为空（GET /tasks/{id}/attempts/%d/runners/%d/logs）", attemptNo, r.Seq))
			}
		default:
			a.addCheck(prefix+".logs", true, fmt.Sprintf("%d 字节", len(logs)))
		}

		// result.json：succeeded / failed 的 test / pre_check runner 必须可取。
		body, status, err := a.fetchRunnerResult(ctx, task.ID, attemptNo, r.Seq)
		if err != nil {
			a.recordError(prefix+".result", err)
			if a.connectErr != nil {
				return
			}
			continue
		}
		if status != 200 && r.Error != nil && isPlatformSetupFailure(r.Error.Code) {
			a.warn("runner %s 没有 result.json（平台准备期失败 %s，HTTP %d）", r.Name, r.Error.Code, status)
			continue
		}
		if status == 404 {
			// D-74：只产出 html / csv 也算满足三选一；detail.produced[] 应列实际产物。
			a.warn("runner %s 的 result.json 404（只产出 html/csv 也满足三选一，detail.produced 列实际产物）", r.Name)
			continue
		}
		if status != 200 {
			a.addCheck(prefix+".result", false,
				fmt.Sprintf("HTTP %d（GET /tasks/{id}/attempts/%d/runners/%d/result）", status, attemptNo, r.Seq))
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

// checkAttemptContinuity 校验 attempts[].attempt_no 从 1 连续（§7.2：重试只追加不覆盖）。
func (a *App) checkAttemptContinuity(task *Task) {
	if len(task.Attempts) == 0 {
		a.warn("Task 详情没有 attempts[]（07 §5.1：详情带 attempt 列表）")
		return
	}
	ats := append([]Attempt(nil), task.Attempts...)
	sort.Slice(ats, func(i, j int) bool { return ats[i].AttemptNo < ats[j].AttemptNo })
	var gaps []string
	for i, at := range ats {
		if at.AttemptNo != i+1 {
			gaps = append(gaps, fmt.Sprintf("第 %d 条 attempt 的 attempt_no=%d（期望 %d，§7.2：从 1 连续递增）", i+1, at.AttemptNo, i+1))
		}
		if !allAttemptStatuses[at.Status] {
			gaps = append(gaps, fmt.Sprintf("attempt %d 的状态 token 非法：%q", at.AttemptNo, at.Status))
		}
		if at.Trigger != "initial" && at.Trigger != "retry" {
			gaps = append(gaps, fmt.Sprintf("attempt %d 的 trigger 非法：%q", at.AttemptNo, at.Trigger))
		}
	}
	a.addCheck("task.attempts_continuous", len(gaps) == 0,
		firstN(gaps, 3, fmt.Sprintf("attempts.attempt_no 从 1 连续递增（%d 个 attempt）", len(ats))))
}

// fetchRunnerLogs 读 attempt 级 runner 日志：stdout 空、stderr 也空才算空。
func (a *App) fetchRunnerLogs(ctx context.Context, taskID string, attemptNo, seq int) ([]byte, int, error) {
	lastStatus := 0
	var lastBody []byte
	for _, stream := range []string{"stdout", "stderr"} {
		path := fmt.Sprintf("/tasks/%s/attempts/%d/runners/%d/logs?stream=%s&limit=65536",
			url.PathEscape(taskID), attemptNo, seq, stream)
		status, _, body, err := a.client.Get(ctx, path)
		if err != nil {
			return nil, 0, err
		}
		lastStatus, lastBody = status, body
		if status == 200 && strings.TrimSpace(string(body)) != "" {
			return body, status, nil
		}
		if status != 200 && status != 404 {
			return body, status, nil
		}
	}
	return lastBody, lastStatus, nil
}

func (a *App) fetchRunnerResult(ctx context.Context, taskID string, attemptNo, seq int) ([]byte, int, error) {
	path := fmt.Sprintf("/tasks/%s/attempts/%d/runners/%d/result", url.PathEscape(taskID), attemptNo, seq)
	status, _, body, err := a.client.Get(ctx, path)
	return body, status, err
}

// collectAndValidateEvents 取事件并做合法性校验，同时记录摘要给机器输出。
func (a *App) collectAndValidateEvents(ctx context.Context, task *Task) {
	if a.connectErr != nil {
		return
	}
	events := a.collectEvents(ctx, task.ID)
	a.events = events
	a.validateEvents(events, task)
}

// fetchTask 供 assert/wait 之外的命令按 id 取 Task。
func (a *App) fetchTask(ctx context.Context, taskID string) (*Task, error) {
	status, _, body, err := a.client.Get(ctx, "/tasks/"+url.PathEscape(taskID))
	if err != nil {
		return nil, err
	}
	if err := expectStatus(status, 200, body); err != nil {
		return nil, fmt.Errorf("查询 Task %s 失败：%w", taskID, err)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, fmt.Errorf("GET /tasks/%s 返回的不是合法 JSON：%v", taskID, err)
	}
	if task.ID == "" {
		return nil, fmt.Errorf("GET /tasks/%s 响应缺 id", taskID)
	}
	return &task, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// checkRunnerListComplete 校验 GET /tasks/{id}/runners 覆盖 Task 详情里的全部 runner。
// 这条抓的是真实回归：分页实现若用 `seq > cursor` 且空 cursor 解为 0，
// 首屏会漏掉 seq=1 的 service-deploy。
func (a *App) checkRunnerListComplete(ctx context.Context, task *Task) {
	status, _, body, err := a.client.Get(ctx, "/tasks/"+url.PathEscape(task.ID)+"/runners?limit=200")
	if err != nil {
		a.recordError("task.runners_list_complete", err)
		return
	}
	if status != 200 {
		a.addCheck("task.runners_list_complete", false,
			fmt.Sprintf("GET /tasks/{id}/runners 期望 200，实际 %d：%s", status, truncate(string(body), 300)))
		return
	}
	var page struct {
		Items   []RunnerExecution `json:"items"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		a.addCheck("task.runners_list_complete", false, "runner 列表不是合法 JSON："+err.Error())
		return
	}
	seen := map[int]bool{}
	for _, r := range page.Items {
		seen[r.Seq] = true
	}
	var missing []string
	for _, r := range task.Runners {
		if !seen[r.Seq] {
			missing = append(missing, fmt.Sprintf("seq=%d %s 状态=%s", r.Seq, r.Name, r.Status))
		}
	}
	detail := "runner 列表覆盖 Task 详情里的全部 runner（含 seq=1 的注入行）"
	if page.HasMore {
		detail = "runner 列表首页 has_more=true，未翻页；已拿到 " + strconv.Itoa(len(page.Items)) + " 条"
	}
	a.addCheck("task.runners_list_complete", len(missing) == 0, firstN(missing, 5, detail))
}

// isPlatformSetupFailure 判断"Job/容器根本没起来"的错误码：
// 这类 runner 没有 Pod 日志与 result.json 是正常的，不该按"产物缺失"判红。
func isPlatformSetupFailure(code string) bool {
	switch code {
	case "EXECUTOR_UNAVAILABLE", "IMAGE_PULL_FAILED", "MOUNT_UNAVAILABLE", "ARTIFACT_QUOTA_EXCEEDED":
		return true
	}
	return false
}

// isPrecheckRetryableFailure 判断错误码是否属于 §7.5 #4 的"可重试 precheck 失败"：
// 只有这类失败才把 Task 归为 service_unavailable。
func isPrecheckRetryableFailure(code string) bool {
	switch code {
	case "ENDPOINT_UNREACHABLE", "PRECHECK_FAILED", "RUNNER_TIMEOUT":
		return true
	}
	return false
}

// checkPrecheckClassification 把 §7.5 的终态归类规则落成断言：
//   - precheck 失败后其余 runner 必须 skipped（abort_task 传播，不能挂着 pending/running）；
//   - retryable=true 的探活失败 → service_unavailable；
//   - retryable=false 的探活失败（401/403、200 包错误、DNS/TLS）→ failed；
//   - 平台准备期失败（如 EXECUTOR_UNAVAILABLE）不适用上述归类，归 failed。
func (a *App) checkPrecheckClassification(task *Task) {
	var pre *RunnerExecution
	for i := range task.Runners {
		if task.Runners[i].Name == "precheck" {
			pre = &task.Runners[i]
			break
		}
	}
	if pre == nil || pre.Status != "failed" {
		return
	}

	var notSkipped []string
	for i := range task.Runners {
		r := &task.Runners[i]
		if r.Seq == pre.Seq {
			continue
		}
		if r.Status != "skipped" {
			notSkipped = append(notSkipped, fmt.Sprintf("%s=%s", r.Name, r.Status))
		} else if r.StatusReason != "" && r.StatusReason != "upstream_abort" {
			a.warn("precheck 失败后 %s 的 skip reason=%q（期望 upstream_abort；实际以 backend 为准）", r.Name, r.StatusReason)
		}
	}
	a.addCheck("task.abort_propagation", len(notSkipped) == 0,
		firstN(notSkipped, 5, "precheck 失败后其余 runner 全部 skipped（abort_task 传播）"))

	if pre.Error == nil {
		a.warn("precheck failed 但没有 error 对象，无法核对 retryable 与终态归类")
		return
	}
	switch task.Status {
	case "service_unavailable":
		ok := isPrecheckRetryableFailure(pre.Error.Code) && pre.Error.Retryable
		a.addCheck("task.precheck_retryable_consistent", ok,
			fmt.Sprintf("service_unavailable 的 precheck 失败应为可重试探活失败，实际 code=%s retryable=%v",
				pre.Error.Code, pre.Error.Retryable))
	case "failed":
		bad := isPrecheckRetryableFailure(pre.Error.Code) && pre.Error.Retryable
		a.addCheck("task.precheck_retryable_consistent", !bad,
			fmt.Sprintf("failed 不应由可重试探活失败导致，实际 code=%s retryable=%v",
				pre.Error.Code, pre.Error.Retryable))
	}
}

func atLeastOneStatus(runners []RunnerExecution, status string) bool {
	for _, r := range runners {
		if r.Status == status {
			return true
		}
	}
	return false
}
