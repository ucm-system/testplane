package main

// 实现侧校验：对运行中的控制面逐个打 MVP 的 25 个 operation，
// 断言"端点存在 + 状态码符合 spec + 响应形状符合 schema + 错误体是 Error"。
//
// 关键取舍：
//   - 核心只读端点要求精确的 2xx；
//   - 状态相关端点的 404 按语义区分：/report、/report.md 未就绪只允许出现在非终态 Run，
//     终态 Run 必须 200；/deploy 仅在 Run 没有 deploy 时允许 404；attempt result/logs 允许
//     尚未产出的 404；404 的响应体仍必须过 Error 形状校验；
//   - 会创建 2 个 fixture Run（A 用于读，B 用于取消），A 在结束时尽力取消。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// connectError 是"连不上控制面"，退出码 3。
type connectError struct {
	baseURL string
	err     error
}

func (e *connectError) Error() string {
	return fmt.Sprintf(`无法连接控制面 %s：%v
  先确认控制面已启动（PG + 迁移 + --role=all，监听 8080），或加 --mode spec 只跑规格侧校验。
  可先用 curl %s/healthz 自检。`, e.baseURL, e.err, strings.TrimRight(e.baseURL, "/"))
}

func (e *connectError) Unwrap() error { return e.err }

func isConnectError(err error) bool {
	var ce *connectError
	return errors.As(err, &ce)
}

// liveClient 是极简 HTTP 客户端（contract 模块不依赖 e2e 模块）。
type liveClient struct {
	baseURL string
	user    string
	token   string
	noAuth  bool
	hc      *http.Client
	verbose bool
}

func newLiveClient(opts Options) *liveClient {
	return &liveClient{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		user:    opts.User,
		token:   opts.Token,
		noAuth:  opts.NoAuth,
		hc:      &http.Client{Timeout: 20 * time.Second},
		verbose: opts.Verbose,
	}
}

func (c *liveClient) authorize(req *http.Request, accept string) {
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "testplane-contract/0.1")
	if c.noAuth {
		return
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
		return
	}
	if c.user != "" {
		req.Header.Set("Authorization", "Bearer "+c.user)
	}
}

func (c *liveClient) do(ctx context.Context, method, path string, payload []byte, contentType string, headers map[string]string) (int, http.Header, []byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("构造请求失败（%s %s）：%w", method, path, err)
	}
	if payload != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	c.authorize(req, "")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c.verbose {
		fmt.Fprintf(os.Stderr, "  [http] → %s %s\n", method, req.URL)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, fmt.Errorf("请求超时（%s %s）：%w", method, path, err)
		}
		return 0, nil, nil, &connectError{baseURL: c.baseURL, err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, raw, fmt.Errorf("读取响应失败（%s %s）：%w", method, path, err)
	}
	if c.verbose {
		fmt.Fprintf(os.Stderr, "  [http] ← %d %s（%d 字节）\n", resp.StatusCode, path, len(raw))
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// streamFirstLine 建立 SSE 连接并读第一行；窗口由 ctx 控制。
func (c *liveClient) streamFirstLine(ctx context.Context, path string) (int, http.Header, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("构造 SSE 请求失败：%w", err)
	}
	c.authorize(req, "text/event-stream")
	req.Header.Set("Last-Event-ID", "0")

	resp, err := (&http.Client{}).Do(req) // 不设 timeout：由 ctx 控制
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, "", fmt.Errorf("SSE 请求超时：%w", err)
		}
		return 0, nil, "", &connectError{baseURL: c.baseURL, err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, resp.Header, string(raw), nil
	}

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lineCh <- strings.TrimSpace(line)
	}()
	select {
	case line := <-lineCh:
		return resp.StatusCode, resp.Header, line, nil
	case <-ctx.Done():
		return resp.StatusCode, resp.Header, "", nil
	}
}

// Live 是本轮实现侧校验的状态。
type Live struct {
	spec       *Spec
	opts       Options
	client     *liveClient
	checks     []check
	warnings   []string
	covered    map[string]bool
	connectErr error
	runA       string
	taskA      string
	runB       string
	taskB      string
}

func NewLive(spec *Spec, opts Options) *Live {
	return &Live{
		spec:    spec,
		opts:    opts,
		client:  newLiveClient(opts),
		covered: map[string]bool{},
	}
}

func (l *Live) add(name string, ok bool, detail string) {
	l.checks = append(l.checks, check{Name: name, OK: ok, Detail: detail})
}

func (l *Live) warn(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func (l *Live) markOp(id string) { l.covered[id] = true }

func (l *Live) recordErr(name string, err error) {
	l.add(name, false, err.Error())
	var ce *connectError
	if errors.As(err, &ce) && l.connectErr == nil {
		l.connectErr = ce
	}
}

// Run 执行全部实现侧探针。
func (l *Live) Run() ([]check, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), l.opts.Timeout)
	defer cancel()

	// ---- 固定端点 ------------------------------------------------------
	l.probeText(ctx, "getHealthz", "/healthz", []int{200})
	l.probeText(ctx, "getReadyz", "/readyz", []int{200})
	l.probeSchema(ctx, "getMeta", "/api/v1/meta", 200, "Meta")
	l.probeSchema(ctx, "getMe", "/api/v1/me", 200, "Me")
	l.probeSchema(ctx, "listRuns", "/api/v1/runs?limit=5", 200, "RunList")
	l.probeSchema(ctx, "listTasks", "/api/v1/tasks?limit=5", 200, "TaskList")
	l.probeSchema(ctx, "listTesters", "/api/v1/testers?limit=50", 200, "TesterList")
	l.checkTesterRegistry(ctx)
	l.probeSchema(ctx, "getTester", "/api/v1/testers/endpoint-probe", 200, "Tester")
	l.probeSchema(ctx, "listClusters", "/api/v1/clusters", 200, "ClusterList")
	l.probeSchema(ctx, "getQueueStatus", "/api/v1/queue-status", 200, "QueueStatus")
	if l.connectErr != nil {
		return l.checks, l.warnings, l.connectErr
	}

	// ---- fixture Run --------------------------------------------------
	l.runA, l.taskA = l.submitFixture(ctx, "A")
	if l.runA != "" {
		l.probeRunReads(ctx)
	}
	l.runB, l.taskB = l.submitFixture(ctx, "B")
	if l.runB != "" {
		l.probeCancels(ctx)
	}

	// ---- 错误体形状 ----------------------------------------------------
	l.probeErrorEnvelopes(ctx)

	// ---- SSE ----------------------------------------------------------
	if l.runA != "" {
		l.probeSSE(ctx)
	}

	// ---- 覆盖率 --------------------------------------------------------
	l.checkCoverage()

	// 尽力清理：取消用于读探针的 Run
	if l.runA != "" {
		_, _, _, _ = l.client.do(ctx, http.MethodPost,
			"/api/v1/runs/"+url.PathEscape(l.runA)+"/cancel", nil, "", nil)
	}
	return l.checks, l.warnings, l.connectErr
}

// ---- 探针 --------------------------------------------------------------

func (l *Live) probeText(ctx context.Context, opID, path string, want []int) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	l.markOp(opID)
	if !containsStatus(want, status) {
		l.add("live."+opID, false, fmt.Sprintf("期望 HTTP %v，实际 %d：%s", want, status, truncate(raw, 300)))
		return
	}
	issues := l.contentTypeIssues(opID, status, hdr)
	if len(issues) > 0 {
		l.add("live."+opID, false, strings.Join(issues, "；"))
		return
	}
	l.add("live."+opID, true, fmt.Sprintf("HTTP %d %s", status, hdr.Get("Content-Type")))
}

func (l *Live) probeSchema(ctx context.Context, opID, path string, wantStatus int, schemaName string) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	l.markOp(opID)
	if status != wantStatus {
		l.add("live."+opID, false, fmt.Sprintf("期望 HTTP %d，实际 %d：%s", wantStatus, status, truncate(raw, 300)))
		return
	}
	issues := l.validateSuccess(opID, status, hdr, raw, schemaName)
	if len(issues) > 0 {
		l.add("live."+opID, false, strings.Join(issues, "；"))
		return
	}
	l.add("live."+opID, true, fmt.Sprintf("HTTP %d，%s required 齐全", status, schemaName))
}

// probeOptional 允许"spec 声明的状态依赖错误码"（如报告未生成 404）。
func (l *Live) probeOptional(ctx context.Context, opID, path string, successStatus int, schemaName string, allowedErrorStatus int, textContentType string) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	l.markOp(opID)
	switch {
	case status == successStatus:
		var issues []string
		if schemaName != "" {
			issues = l.validateSuccess(opID, status, hdr, raw, schemaName)
		} else {
			issues = l.contentTypeIssues(opID, status, hdr)
		}
		if len(issues) > 0 {
			l.add("live."+opID, false, strings.Join(issues, "；"))
			return
		}
		l.add("live."+opID, true, fmt.Sprintf("HTTP %d（成功路径）", status))
	case allowedErrorStatus != 0 && status == allowedErrorStatus:
		issues := l.validateError(opID, status, hdr, raw)
		if len(issues) > 0 {
			l.add("live."+opID, false, strings.Join(issues, "；"))
			return
		}
		l.add("live."+opID, true, fmt.Sprintf("HTTP %d（spec 声明的状态依赖错误，Error 形状正确）", status))
	default:
		l.add("live."+opID, false,
			fmt.Sprintf("期望 HTTP %d 或 %d，实际 %d：%s", successStatus, allowedErrorStatus, status, truncate(raw, 300)))
	}
}

// ---- 状态相关端点：404 按 Run 状态 / deploy 是否存在区分（票 #16）---------

// isTerminalRunStatus 判断 Run 是否已终态。
func isTerminalRunStatus(status string) bool {
	switch status {
	case "succeeded", "completed_with_errors", "service_unavailable", "failed", "canceled":
		return true
	}
	return false
}

// liveRunFacts 是判定 /report、/report.md、/deploy 语义所需的 Run 事实。
type liveRunFacts struct {
	Status    string
	HasDeploy bool
}

func (l *Live) fetchRunFacts(ctx context.Context, runID string) (liveRunFacts, error) {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, "", nil)
	if err != nil {
		return liveRunFacts{}, err
	}
	if status != 200 {
		return liveRunFacts{}, fmt.Errorf("GET /runs/%s 期望 200，实际 %d", runID, status)
	}
	var body struct {
		Status string          `json:"status"`
		Deploy json.RawMessage `json:"deploy"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return liveRunFacts{}, fmt.Errorf("解析 Run 详情失败：%w", err)
	}
	hasDeploy := len(body.Deploy) > 0 && string(body.Deploy) != "null"
	return liveRunFacts{Status: body.Status, HasDeploy: hasDeploy}, nil
}

// probeRunDeploy 校验 /deploy：Run 有 deploy 时必须 200；没有 deploy（service 形态）允许 404。
func (l *Live) probeRunDeploy(ctx context.Context, runID string) {
	facts, err := l.fetchRunFacts(ctx, runID)
	if err != nil {
		l.recordErr("live.getRunDeploy", err)
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/deploy", nil, "", nil)
	if err != nil {
		l.recordErr("live.getRunDeploy", err)
		return
	}
	l.markOp("getRunDeploy")
	ok, detail := l.artifactResponseCheck("getRunDeploy", facts.HasDeploy, status, hdr, raw, "DeploySnapshot", "")
	if ok && facts.HasDeploy {
		detail = "Run 有 deploy，HTTP 200，DeploySnapshot required 齐全"
	}
	l.add("live.getRunDeploy", ok, detail)
}

// probeRunReport 校验 /report 与 /report.md：
//   - 非终态 Run 允许 404（尚未生成），但 Error 形状要正确；
//   - 终态 Run 必须 200（C-7.2 报告在终态生成；终态 404 是真 bug）。
func (l *Live) probeRunReport(ctx context.Context, runID, suffix, schemaName, opID string) {
	facts, err := l.fetchRunFacts(ctx, runID)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+suffix, nil, "", nil)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	l.markOp(opID)
	textCT := ""
	if suffix == "/report.md" {
		textCT = "text/markdown"
	}
	terminal := isTerminalRunStatus(facts.Status)
	ok, detail := l.artifactResponseCheck(opID, terminal, status, hdr, raw, schemaName, textCT)
	if ok && terminal && status == 200 {
		detail = fmt.Sprintf("Run 已终态（%s），HTTP 200", facts.Status)
	}
	l.add("live."+opID, ok, detail)
}

// artifactResponseCheck 是状态相关端点的纯判定（便于单测）：
// require200=true 时 404 一律失败；false 时 404 必须过 Error 形状。
func (l *Live) artifactResponseCheck(opID string, require200 bool, status int, hdr http.Header, raw []byte, schemaName, textContentType string) (bool, string) {
	switch {
	case status == 200:
		var issues []string
		if schemaName != "" {
			issues = l.validateSuccess(opID, status, hdr, raw, schemaName)
		} else {
			issues = l.contentTypeIssues(opID, status, hdr)
		}
		if len(issues) > 0 {
			return false, strings.Join(issues, "；")
		}
		return true, "HTTP 200（成功路径）"
	case status == 404:
		if require200 {
			return false, fmt.Sprintf("该端点此时必须返回 200，实际 404：%s", truncate(raw, 300))
		}
		if issues := l.validateError(opID, status, hdr, raw); len(issues) > 0 {
			return false, strings.Join(issues, "；")
		}
		return true, "HTTP 404（允许的未就绪状态，Error 形状正确）"
	default:
		return false, fmt.Sprintf("期望 HTTP 200 或 404，实际 %d：%s", status, truncate(raw, 300))
	}
}

func (l *Live) submitFixture(ctx context.Context, label string) (runID, taskID string) {
	name := "live.submitRun"
	detailPrefix := "fixture " + label
	if label != "A" {
		name = "live.submitRun.second"
	}
	manifest, ct := l.manifestBytes()

	if label == "A" {
		l.checkDryRun(ctx, manifest, ct)
	}

	status, hdr, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/runs", manifest, ct, nil)
	if err != nil {
		l.recordErr(name, err)
		return "", ""
	}
	l.markOp("submitRun")
	if status != http.StatusAccepted {
		l.add(name, false, fmt.Sprintf("%s：期望 HTTP 202，实际 %d：%s", detailPrefix, status, truncate(raw, 400)))
		return "", ""
	}
	issues := l.validateSuccess("submitRun", status, hdr, raw, "RunAccepted")
	var accepted struct {
		Run   map[string]any   `json:"run"`
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		issues = append(issues, "202 响应不是 JSON 对象："+err.Error())
	} else {
		runID = asString(accepted.Run["id"])
		if runID == "" {
			issues = append(issues, "202 的 run.id 为空")
		}
		if len(accepted.Tasks) == 0 {
			issues = append(issues, "202 的 tasks[] 为空（precheck:auto 至少应注入一个 Task）")
		} else {
			taskID = asString(accepted.Tasks[0]["id"])
			rawTask, _ := json.Marshal(accepted.Tasks[0])
			if missing := l.spec.requireFields("AcceptedTask", rawTask); len(missing) > 0 {
				issues = append(issues, "tasks[0] 缺字段："+strings.Join(missing, ", "))
			}
		}
		if accepted.Run != nil {
			rawRun, _ := json.Marshal(accepted.Run)
			if missing := l.spec.requireFields("AcceptedRun", rawRun); len(missing) > 0 {
				issues = append(issues, "run 摘要缺字段："+strings.Join(missing, ", "))
			}
		}
	}
	if len(issues) > 0 {
		l.add(name, false, detailPrefix+"："+strings.Join(issues, "；"))
		return "", ""
	}
	l.add(name, true, fmt.Sprintf("%s：HTTP 202，run_id=%s，%d 个 Task", detailPrefix, runID, len(accepted.Tasks)))
	return runID, taskID
}

func (l *Live) checkDryRun(ctx context.Context, manifest []byte, contentType string) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/runs?dry_run=true", manifest, contentType, nil)
	if err != nil {
		l.recordErr("live.submitRun.dry_run", err)
		return
	}
	l.markOp("submitRun")
	if status != http.StatusOK {
		l.add("live.submitRun.dry_run", false, fmt.Sprintf("期望 HTTP 200，实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	issues := l.validateSuccess("submitRun", status, hdr, raw, "DryRunResult")
	if len(issues) > 0 {
		l.add("live.submitRun.dry_run", false, strings.Join(issues, "；"))
		return
	}
	var result struct {
		Valid *bool `json:"valid"`
	}
	_ = json.Unmarshal(raw, &result)
	if result.Valid == nil {
		l.add("live.submitRun.dry_run", false, "dry-run 响应缺 valid 字段（07 §4.1）")
		return
	}
	l.add("live.submitRun.dry_run", true, fmt.Sprintf("valid=%v", *result.Valid))
}

func (l *Live) probeRunReads(ctx context.Context) {
	id := url.PathEscape(l.runA)
	l.probeSchema(ctx, "getRun", "/api/v1/runs/"+id, 200, "Run")
	l.probeSchema(ctx, "listRunTasks", "/api/v1/runs/"+id+"/tasks?limit=50", 200, "TaskList")
	l.checkRunTaskListComplete(ctx, l.runA)
	l.probeSchema(ctx, "listRunEvents", "/api/v1/runs/"+id+"/events/history?since_seq=0&limit=100", 200, "EventList")
	l.probeSchema(ctx, "getRunManifest", "/api/v1/runs/"+id+"/manifest", 200, "ManifestSnapshot")
	l.probeRunDeploy(ctx, l.runA)
	l.probeRunReport(ctx, l.runA, "/report", "Report", "getRunReport")
	l.probeRunReport(ctx, l.runA, "/report.md", "", "getRunReportMarkdown")

	if l.taskA == "" {
		l.add("live.getTask", false, "fixture Run 没有 Task，无法校验 getTask/attempts")
		l.add("live.listTaskAttempts", false, "fixture Run 没有 Task")
		l.add("live.getTaskAttemptResult", false, "fixture Run 没有 Task")
		l.add("live.getTaskAttemptLogs", false, "fixture Run 没有 Task")
		return
	}
	task := url.PathEscape(l.taskA)
	l.probeSchema(ctx, "getTask", "/api/v1/tasks/"+task, 200, "TaskDetail")
	l.probeSchema(ctx, "listTaskAttempts", "/api/v1/tasks/"+task+"/attempts?limit=50", 200, "AttemptList")
	l.probeOptional(ctx, "getTaskAttemptResult", "/api/v1/tasks/"+task+"/attempts/1/result", 200, "Result", 404, "")
	l.probeOptional(ctx, "getTaskAttemptLogs", "/api/v1/tasks/"+task+"/attempts/1/logs?stream=stdout&limit=4096", 200, "", 404, "text/plain")
}

// checkRunTaskListComplete 校验 GET /runs/{id}/tasks 覆盖 Run 详情里的全部 Task。
// 回归背景：分页实现若写成 `seq > cursor` 且空 cursor 解为 0，首屏会漏掉 seq=0 的 precheck。
func (l *Live) checkRunTaskListComplete(ctx context.Context, runID string) {
	id := url.PathEscape(runID)
	_, _, runRaw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+id, nil, "", nil)
	if err != nil {
		l.recordErr("live.listRunTasks_complete", err)
		return
	}
	_, _, listRaw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+id+"/tasks?limit=200", nil, "", nil)
	if err != nil {
		l.recordErr("live.listRunTasks_complete", err)
		return
	}
	var detail struct {
		Tasks []map[string]any `json:"tasks"`
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(runRaw, &detail); err != nil {
		l.add("live.listRunTasks_complete", false, "Run 详情不是合法 JSON："+err.Error())
		return
	}
	if err := json.Unmarshal(listRaw, &page); err != nil {
		l.add("live.listRunTasks_complete", false, "任务列表不是合法 JSON："+err.Error())
		return
	}
	seen := map[string]bool{}
	for _, t := range page.Items {
		seen[asString(t["id"])] = true
	}
	var missing []string
	for _, t := range detail.Tasks {
		taskID := asString(t["id"])
		if !seen[taskID] {
			missing = append(missing, fmt.Sprintf("seq=%v name=%v id=%v", t["seq"], t["name"], taskID))
		}
	}
	l.add("live.listRunTasks_complete", len(missing) == 0,
		firstN(missing, 5, "任务列表覆盖 Run 详情里的全部 Task（含 seq=0 precheck）"))
}

func (l *Live) probeCancels(ctx context.Context) {
	// Task 取消：202 或"已在取消中"的 200
	taskStatus, taskHdr, taskRaw, err := l.client.do(ctx, http.MethodPost,
		"/api/v1/tasks/"+url.PathEscape(l.taskB)+"/cancel", nil, "", nil)
	if err != nil {
		l.recordErr("live.cancelTask", err)
	} else {
		l.markOp("cancelTask")
		l.checkAccepted("live.cancelTask", "cancelTask", "TaskCancelAccepted", taskStatus, taskHdr, taskRaw)
	}

	runStatus, runHdr, runRaw, err := l.client.do(ctx, http.MethodPost,
		"/api/v1/runs/"+url.PathEscape(l.runB)+"/cancel", nil, "", nil)
	if err != nil {
		l.recordErr("live.cancelRun", err)
		return
	}
	l.markOp("cancelRun")
	l.checkAccepted("live.cancelRun", "cancelRun", "CancelAccepted", runStatus, runHdr, runRaw)
}

func (l *Live) checkAccepted(name, opID, schemaName string, status int, hdr http.Header, raw []byte) {
	if status != http.StatusAccepted && status != http.StatusOK {
		l.add(name, false, fmt.Sprintf("期望 202（或幂等 200），实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	issues := l.validateSuccess(opID, status, hdr, raw, schemaName)
	if len(issues) > 0 {
		l.add(name, false, strings.Join(issues, "；"))
		return
	}
	l.add(name, true, fmt.Sprintf("HTTP %d，%s required 齐全", status, schemaName))
}

// probeErrorEnvelopes 单独验证错误体形状（404 与 400）。
func (l *Live) probeErrorEnvelopes(ctx context.Context) {
	// 404：合法路由 + 不存在的 Run
	path := "/api/v1/runs/r-contract-missing-0000"
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	if err != nil {
		l.recordErr("live.error_envelope_404", err)
	} else {
		l.markOp("getRun")
		issues := l.validateError("getRun", status, hdr, raw)
		if len(issues) > 0 {
			l.add("live.error_envelope_404", false, strings.Join(issues, "；"))
		} else {
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			l.add("live.error_envelope_404", true, fmt.Sprintf("HTTP 404，code=%v", obj["code"]))
		}
	}

	// 400：deploy 与 service 同时出现（C-5.1 第 3 条）
	invalid := []byte(`{"run":{"name":"contract-invalid","deploy":{"provider":"genet","parameters":{}},"service":{"url":"http://127.0.0.1:1/v1"},"tests":[]}}`)
	status, hdr, raw, err = l.client.do(ctx, http.MethodPost, "/api/v1/runs", invalid, "application/json", nil)
	if err != nil {
		l.recordErr("live.error_envelope_400", err)
		return
	}
	l.markOp("submitRun")
	issues := l.validateError("submitRun", status, hdr, raw)
	if len(issues) > 0 {
		l.add("live.error_envelope_400", false, strings.Join(issues, "；"))
		return
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	l.add("live.error_envelope_400", true, fmt.Sprintf("HTTP %d，code=%v", status, obj["code"]))
}

func (l *Live) probeSSE(ctx context.Context) {
	window := 5 * time.Second
	if l.opts.Timeout < window {
		window = l.opts.Timeout
	}
	sctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	status, hdr, first, err := l.client.streamFirstLine(sctx,
		"/api/v1/runs/"+url.PathEscape(l.runA)+"/events")
	if err != nil {
		l.recordErr("live.streamRunEvents", err)
		return
	}
	l.markOp("streamRunEvents")
	if status != http.StatusOK {
		l.add("live.streamRunEvents", false, fmt.Sprintf("期望 HTTP 200，实际 %d：%s", status, truncate([]byte(first), 300)))
		return
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		l.add("live.streamRunEvents", false, fmt.Sprintf("Content-Type=%q，期望 text/event-stream", ct))
		return
	}
	if first == "" {
		l.add("live.streamRunEvents", false, fmt.Sprintf("%s 内没有收到任何行（事件回放或心跳）", window))
		return
	}
	l.add("live.streamRunEvents", true, fmt.Sprintf("HTTP 200 text/event-stream；首行 %q", truncate([]byte(first), 80)))
}

// ---- 校验工具 ----------------------------------------------------------

// validateSuccess 校验"status 已在 spec 声明 + Content-Type 正确 + schema 必填字段齐全"。
func (l *Live) validateSuccess(opID string, status int, hdr http.Header, raw []byte, schemaName string) []string {
	op, ok := l.spec.ByID[opID]
	if !ok {
		return []string{"spec 里找不到 operationId " + opID}
	}
	resp, declared := op.Responses[strconv.Itoa(status)]
	if !declared {
		return []string{fmt.Sprintf("HTTP %d 未在 %s 的 spec 响应中声明", status, opID)}
	}
	var issues []string
	if len(resp.ContentTypes) > 0 {
		want := resp.ContentTypes[0]
		got := hdr.Get("Content-Type")
		if !strings.HasPrefix(got, want) {
			issues = append(issues, fmt.Sprintf("Content-Type=%q，spec 期望 %q", got, want))
		}
	}
	if schemaName != "" {
		if resp.SchemaName != "" && resp.SchemaName != schemaName {
			issues = append(issues, fmt.Sprintf("spec 的响应 schema=%s，检查器期望 %s", resp.SchemaName, schemaName))
		}
		if missing := l.spec.requireFields(schemaName, raw); len(missing) > 0 {
			issues = append(issues, "缺必填字段："+strings.Join(missing, ", "))
		}
	}
	return issues
}

func (l *Live) validateError(opID string, status int, hdr http.Header, raw []byte) []string {
	op, ok := l.spec.ByID[opID]
	if !ok {
		return []string{"spec 里找不到 operationId " + opID}
	}
	resp, declared := op.Responses[strconv.Itoa(status)]
	if !declared {
		return []string{fmt.Sprintf("HTTP %d 未在 %s 的 spec 响应中声明", status, opID)}
	}
	if !hasContentType(resp.ContentTypes, "application/json") {
		return []string{fmt.Sprintf("HTTP %d 的 spec Content-Type=%v，不是 JSON 错误体", status, resp.ContentTypes)}
	}
	gotCT := hdr.Get("Content-Type")
	if !strings.HasPrefix(gotCT, "application/json") {
		return []string{fmt.Sprintf("Content-Type=%q，错误体必须是 application/json", gotCT)}
	}
	return l.errorEnvelopeIssues(raw)
}

func (l *Live) errorEnvelopeIssues(raw []byte) []string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return []string{"错误体不是 JSON 对象：" + err.Error()}
	}
	var issues []string
	for _, field := range l.spec.ErrorRequired {
		if _, ok := obj[field]; !ok {
			issues = append(issues, "Error 缺必填字段："+field)
		}
	}
	if _, nested := obj["error"]; nested {
		issues = append(issues, "错误体是 {error:{...}} 嵌套形态，spec 要求扁平 Error")
	}
	if code := asString(obj["code"]); code == "" {
		issues = append(issues, "Error.code 为空")
	}
	return issues
}

func (l *Live) contentTypeIssues(opID string, status int, hdr http.Header) []string {
	op, ok := l.spec.ByID[opID]
	if !ok {
		return []string{"spec 里找不到 operationId " + opID}
	}
	resp, declared := op.Responses[strconv.Itoa(status)]
	if !declared {
		return []string{fmt.Sprintf("HTTP %d 未在 spec 声明", status)}
	}
	if len(resp.ContentTypes) == 0 {
		return nil
	}
	got := hdr.Get("Content-Type")
	if !strings.HasPrefix(got, resp.ContentTypes[0]) {
		return []string{fmt.Sprintf("Content-Type=%q，spec 期望 %q", got, resp.ContentTypes[0])}
	}
	return nil
}

// checkTesterRegistry 用 GET /testers 证明 endpoint-probe 已被扫描入库（K-3：无运行时注册）。
func (l *Live) checkTesterRegistry(ctx context.Context) {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/testers?limit=100", nil, "", nil)
	if err != nil {
		l.recordErr("live.registry.endpoint_probe", err)
		return
	}
	l.markOp("listTesters")
	if status != http.StatusOK {
		l.add("live.registry.endpoint_probe", false, fmt.Sprintf("GET /testers 返回 %d", status))
		return
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(raw, &list)
	found := false
	for _, item := range list.Items {
		if asString(item["name"]) == "endpoint-probe" {
			found = true
			break
		}
	}
	if !found {
		l.add("live.registry.endpoint_probe", false, "GET /testers 里没有 endpoint-probe（K-3：镜像内 Tester 应被扫描入库）")
		return
	}
	l.add("live.registry.endpoint_probe", true, fmt.Sprintf("%d 个 Tester，含 endpoint-probe", len(list.Items)))

	if status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/clusters", nil, "", nil); err == nil && status == http.StatusOK {
		var clusters struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.Unmarshal(raw, &clusters)
		foundCluster := false
		for _, item := range clusters.Items {
			if asString(item["name"]) == "cluster-01" {
				foundCluster = true
			}
		}
		if !foundCluster {
			l.warn("GET /clusters 里没有 cluster-01（Lead 说 dev seed 会插 cluster-01；契约只警告不判失败）")
		}
	}
}

func (l *Live) checkCoverage() {
	var missing []string
	for _, op := range l.spec.Operations {
		if !l.covered[op.ID] {
			missing = append(missing, op.Method+" "+op.Path+"（"+op.ID+"）")
		}
	}
	l.add("live.all_operations_covered", len(missing) == 0,
		firstN(missing, 8, "25 个 operation 全部被探针覆盖"))
}

// ---- fixture Manifest --------------------------------------------------

func (l *Live) manifestBytes() ([]byte, string) {
	if l.opts.Manifest != "" {
		raw, err := os.ReadFile(l.opts.Manifest)
		if err != nil {
			l.warn("读取 --manifest 失败，回退内置模板：%v", err)
		} else if strings.TrimSpace(string(raw)) != "" {
			ct := "application/yaml"
			if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
				ct = "application/json"
			}
			return raw, ct
		}
	}
	name := l.opts.RunName
	if name == "" {
		name = "contract-" + time.Now().Format("20060102-150405")
	}
	profile := l.opts.Profile
	if profile == "" {
		profile = "k8s-npu"
	}
	manifest := fmt.Sprintf(`run:
  name: %q
  service:
    url: %s
    model_name: mock-llm
  precheck:
    tester: endpoint-probe
    version: "1.0.0"
    args: "--wait_timeout_seconds=5 --retry_interval_seconds=1"
  cleanup: { policy: never }
  tests:
    - name: contract-a
      tester: endpoint-probe
      version: "1.0.0"
      execution: { profile: %s }
      args: "--wait_timeout_seconds=5 --retry_interval_seconds=1"
`, name, l.opts.MockURL, profile)
	return []byte(manifest), "application/yaml"
}

// ---- 小工具 ------------------------------------------------------------

func containsStatus(list []int, want int) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…（截断，共 " + strconv.Itoa(len(s)) + " 字节）"
}
