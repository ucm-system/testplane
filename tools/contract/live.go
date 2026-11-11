package main

// 实现侧校验：对运行中的控制面逐个打 v2 的 operation（59 个），
// 断言"端点存在 + 状态码符合 spec + 响应形状符合 schema + 错误体是 Error"。
//
// 关键取舍（v2：Task 是一等对象，没有 /runs）：
//   - 核心只读端点要求精确的 2xx；
//   - 状态相关端点的 404 按语义区分：/report、/report.md 未就绪只允许出现在非终态 Task，
//     终态 Task 必须 200；/deploy 仅在 Task 无部署（外接模式）时允许 404；
//     runner 级 result/logs 允许尚未产出的 404（产物三选一 D-74）；404 的响应体仍必须过 Error 形状校验；
//   - fixture：A（读探针，service 模式）+ B（取消）+ deploy 形态（deploy/logs）+ F（失败 → retry/clean）；
//   - 写端点（admin）只断言校验/冲突/404 的错误契约，不创建真实资源。

import (
	"archive/zip"
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
	req.Header.Set("User-Agent", "argus-contract/0.1")
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
	taskA      string // 读探针 fixture（service 模式，跑到终态）
	taskB      string // 取消 fixture
	taskD      string // deploy 形态 fixture（deploy/logs 探针）
	taskF      string // 失败 fixture（retry / clean 探针）
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
	l.probeSchema(ctx, "listTasks", "/api/v1/tasks?limit=5", 200, "TaskList")
	l.probeSchema(ctx, "listTaskFacets", "/api/v1/tasks/facets", 200, "TaskFacets")
	l.checkRunnerRegistry(ctx)
	l.probeSchema(ctx, "listClusters", "/api/v1/clusters", 200, "ClusterList")
	l.probeSchema(ctx, "getQueueStatus", "/api/v1/queue-status", 200, "QueueStatus")
	l.checkQueueStatusVolcano(ctx)
	l.probeSchema(ctx, "listProfiles", "/api/v1/profiles", 200, "ExecutionProfileList")
	l.probeSchema(ctx, "listSecretRefs", "/api/v1/secrets?limit=20", 200, "SecretRefList")
	l.probeSchema(ctx, "listSharedMounts", "/api/v1/shared-mounts?limit=20", 200, "SharedMountList")
	l.probeText(ctx, "getMetrics", "/metrics", []int{200})
	l.probeAuthLogin(ctx)
	l.checkAuthCallback(ctx)
	l.checkAuthLogout(ctx)
	if l.connectErr != nil {
		return l.checks, l.warnings, l.connectErr
	}

	// ---- fixture Task --------------------------------------------------
	l.taskA = l.submitFixture(ctx, "A")
	if l.taskA != "" {
		l.waitTaskTerminal(ctx, l.taskA, "A", 120*time.Second)
		l.probeTaskReads(ctx)
		l.probeTaskBundle(ctx, l.taskA)
		l.probeLatestRunner(ctx, l.taskA)
	}
	l.taskB = l.submitFixture(ctx, "B")
	if l.taskB != "" {
		l.probeCancels(ctx)
	}
	l.taskD = l.submitDeployFixture(ctx)
	if l.taskD != "" {
		l.probeDeployLogsDeploy(ctx, l.taskD)
	}
	l.taskF = l.submitFailureFixture(ctx)
	if l.taskF != "" {
		l.waitTaskTerminal(ctx, l.taskF, "F", 120*time.Second)
		l.probeRetryAndClean(ctx)
	}

	// ---- 错误体形状 ----------------------------------------------------
	l.probeErrorEnvelopes(ctx)
	l.probeAdmin(ctx)

	// ---- SSE ----------------------------------------------------------
	if l.taskA != "" {
		l.probeSSE(ctx)
	}

	// ---- 覆盖率 --------------------------------------------------------
	l.checkCoverage()

	// 尽力清理：取消用于读探针的 Task（若上一步没取消成功也无所谓）
	if l.taskA != "" {
		_, _, _, _ = l.client.do(ctx, http.MethodPost,
			"/api/v1/tasks/"+url.PathEscape(l.taskA)+"/cancel", nil, "", nil)
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

// checkQueueStatusVolcano 断言 queue-status 的 volcano 块形态（E-11）：
// 必须有 volcano 对象、available 布尔、queues 数组；不可达降级也不允许整块缺失。
func (l *Live) checkQueueStatusVolcano(ctx context.Context) {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/queue-status", nil, "", nil)
	if err != nil {
		l.recordErr("live.queue_status.volcano", err)
		return
	}
	if status != http.StatusOK {
		l.add("live.queue_status.volcano", false, fmt.Sprintf("HTTP %d", status))
		return
	}
	var body struct {
		Volcano json.RawMessage `json:"volcano"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		l.add("live.queue_status.volcano", false, "响应不是 JSON："+err.Error())
		return
	}
	if len(body.Volcano) == 0 || string(body.Volcano) == "null" {
		l.add("live.queue_status.volcano", false, "缺少 volcano 块")
		return
	}
	var v struct {
		Available *bool             `json:"available"`
		Queues    []json.RawMessage `json:"queues"`
	}
	if err := json.Unmarshal(body.Volcano, &v); err != nil {
		l.add("live.queue_status.volcano", false, "volcano 不是对象："+err.Error())
		return
	}
	if v.Available == nil {
		l.add("live.queue_status.volcano", false, "volcano.available 缺失")
		return
	}
	if v.Queues == nil {
		l.add("live.queue_status.volcano", false, "volcano.queues 缺失（不可达时也必须是 []）")
		return
	}
	l.add("live.queue_status.volcano", true,
		fmt.Sprintf("available=%v，queues=%d", *v.Available, len(v.Queues)))
}

// probeAuthLogin 按模式断言：OIDC 302（Location 非空）或 dev 模式 404（Error 形状）。
func (l *Live) probeAuthLogin(ctx context.Context) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/auth/login?return=/tasks", nil, "", nil)
	if err != nil {
		l.recordErr("live.authLogin", err)
		return
	}
	l.markOp("authLogin")
	switch status {
	case http.StatusFound:
		loc := hdr.Get("Location")
		if loc == "" {
			l.add("live.authLogin", false, "HTTP 302 但没有 Location")
			return
		}
		l.add("live.authLogin", true, "HTTP 302，Location 非空")
	case http.StatusNotFound:
		if issues := l.validateError("authLogin", status, hdr, raw); len(issues) > 0 {
			l.add("live.authLogin", false, strings.Join(issues, "；"))
			return
		}
		l.add("live.authLogin", true, "HTTP 404（dev 模式），Error 形状正确")
	default:
		l.add("live.authLogin", false, fmt.Sprintf("期望 302 或 404，实际 %d：%s", status, truncate(raw, 300)))
	}
}

// checkAuthCallback 只断言跳转或认证失败两种契约形态（dev 没有完整 IdP code 流程）。
func (l *Live) checkAuthCallback(ctx context.Context) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/auth/callback?code=contract&state=contract", nil, "", nil)
	if err != nil {
		l.recordErr("live.authCallback", err)
		return
	}
	l.markOp("authCallback")
	switch status {
	case http.StatusFound:
		if hdr.Get("Location") == "" {
			l.add("live.authCallback", false, "HTTP 302 但没有 Location")
			return
		}
		l.add("live.authCallback", true, "HTTP 302，Location 非空")
	case http.StatusUnauthorized:
		if issues := l.validateError("authCallback", status, hdr, raw); len(issues) > 0 {
			l.add("live.authCallback", false, strings.Join(issues, "；"))
			return
		}
		l.add("live.authCallback", true, "HTTP 401，Error 形状正确")
	case http.StatusNotFound:
		// dev 模式与 /auth/login 一致返回 404；openapi 已声明该 dev 404 + Error。
		if issues := l.validateError("authCallback", status, hdr, raw); len(issues) > 0 {
			l.add("live.authCallback", false, strings.Join(issues, "；"))
			return
		}
		l.add("live.authCallback", true, "HTTP 404（dev 模式，spec 已声明），Error 形状正确")
	default:
		l.add("live.authCallback", false, fmt.Sprintf("期望 302 或 401，实际 %d：%s", status, truncate(raw, 300)))
	}
}

// checkAuthLogout 在 dev/admin Bearer 下应直接 200 且 body 有 ok=true。
func (l *Live) checkAuthLogout(ctx context.Context) {
	status, _, raw, err := l.client.do(ctx, http.MethodPost, "/auth/logout", nil, "", nil)
	if err != nil {
		l.recordErr("live.authLogout", err)
		return
	}
	l.markOp("authLogout")
	if status != http.StatusOK {
		l.add("live.authLogout", false, fmt.Sprintf("期望 HTTP 200，实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	var body struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.OK == nil {
		l.add("live.authLogout", false, "响应缺 ok 字段："+truncate(raw, 200))
		return
	}
	l.add("live.authLogout", true, fmt.Sprintf("HTTP 200，ok=%v", *body.OK))
}

// probeTaskFileBothForms 同时探 /files 的两种 path 形态：
//   - Task 相对形态（契约坐标系）：attempts/1/runners/<seq>/result.json
//   - 误用绝对形态：<task_id>/report.json —— 契约声明 path 是 Task 目录相对路径，
//     400/404 必须是 Error 形状（不判红，但错误体要合格）。
func (l *Live) probeTaskFileBothForms(ctx context.Context, taskID string) {
	id := url.PathEscape(taskID)
	// 相对形态：report.json 在终态后必然落档
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet,
		"/api/v1/tasks/"+id+"/files?path=report.json", nil, "", nil)
	if err != nil {
		l.recordErr("live.downloadTaskFile", err)
		return
	}
	l.markOp("downloadTaskFile")
	ok, detail := l.artifactResponseCheck("downloadTaskFile", false, status, hdr, raw, "", "")
	detail = "relative=>HTTP " + strconv.Itoa(status) + "（" + detail + "）"

	// 绝对形态（历史习惯）：必须给出带 Error 形状的正确拒绝，或恰好存在的 200
	st2, hdr2, raw2, err2 := l.client.do(ctx, http.MethodGet,
		"/api/v1/tasks/"+id+"/files?path="+url.QueryEscape(taskID+"/report.json"), nil, "", nil)
	form2 := "prefixed=>HTTP " + strconv.Itoa(st2)
	switch {
	case err2 != nil:
		l.recordErr("live.downloadTaskFile", err2)
		return
	case st2 == http.StatusOK || st2 == http.StatusPartialContent:
		// 服务端选择兼容绝对路径 → 必须是成功响应
	case st2 == http.StatusNotFound || st2 == http.StatusBadRequest:
		if issues := l.validateError("downloadTaskFile", st2, hdr2, raw2); len(issues) > 0 {
			form2 += "（Error 形状不合格：" + strings.Join(issues, "；") + "）"
			ok = false
		} else {
			form2 += "（Error 形状正确）"
		}
	default:
		form2 += "（非契约状态码）"
		ok = false
	}
	l.add("live.downloadTaskFile", ok, detail+"；"+form2)
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

// ---- 状态相关端点：404 按 Task 状态 / deploy 是否存在区分 --------------------

// isTerminalTaskStatus 判断 Task 是否已终态（v2：无 completed_with_errors，D-73）。
func isTerminalTaskStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "service_unavailable", "canceled":
		return true
	}
	return false
}

// liveTaskFacts 是判定 /report、/report.md、/deploy 语义所需的 Task 事实。
type liveTaskFacts struct {
	Status         string
	HasDeploy      bool
	CurrentAttempt int
	RunnerSeqs     []int // 最新 attempt 的全部 runner seq
}

func (l *Live) fetchTaskFacts(ctx context.Context, taskID string) (liveTaskFacts, error) {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, "", nil)
	if err != nil {
		return liveTaskFacts{}, err
	}
	if status != 200 {
		return liveTaskFacts{}, fmt.Errorf("GET /tasks/%s 期望 200，实际 %d", taskID, status)
	}
	var body struct {
		Status         string          `json:"status"`
		Deploy         json.RawMessage `json:"deploy"`
		CurrentAttempt int             `json:"current_attempt"`
		Runners        []struct {
			Seq int `json:"seq"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return liveTaskFacts{}, fmt.Errorf("解析 Task 详情失败：%w", err)
	}
	hasDeploy := len(body.Deploy) > 0 && string(body.Deploy) != "null"
	facts := liveTaskFacts{Status: body.Status, HasDeploy: hasDeploy, CurrentAttempt: body.CurrentAttempt}
	for _, r := range body.Runners {
		facts.RunnerSeqs = append(facts.RunnerSeqs, r.Seq)
	}
	return facts, nil
}

// probeTaskDeploy 校验 /deploy：Task 有 deploy 时必须 200；外接模式允许 404。
func (l *Live) probeTaskDeploy(ctx context.Context, taskID string) {
	facts, err := l.fetchTaskFacts(ctx, taskID)
	if err != nil {
		l.recordErr("live.getTaskDeploy", err)
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/deploy", nil, "", nil)
	if err != nil {
		l.recordErr("live.getTaskDeploy", err)
		return
	}
	l.markOp("getTaskDeploy")
	ok, detail := l.artifactResponseCheck("getTaskDeploy", facts.HasDeploy, status, hdr, raw, "DeploySnapshot", "")
	if ok && facts.HasDeploy {
		detail = "Task 有 deploy，HTTP 200，DeploySnapshot required 齐全"
	}
	l.add("live.getTaskDeploy", ok, detail)
}

// probeTaskReport 校验 /report 与 /report.md：
//   - 非终态 Task 允许 404（尚未生成），但 Error 形状要正确；
//   - 终态 Task 必须 200（C-7.2 报告在终态生成；终态 404 是真 bug）。
func (l *Live) probeTaskReport(ctx context.Context, taskID, suffix, schemaName, opID string) {
	facts, err := l.fetchTaskFacts(ctx, taskID)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+suffix, nil, "", nil)
	if err != nil {
		l.recordErr("live."+opID, err)
		return
	}
	l.markOp(opID)
	textCT := ""
	if suffix == "/report.md" {
		textCT = "text/markdown"
	}
	terminal := isTerminalTaskStatus(facts.Status)
	ok, detail := l.artifactResponseCheck(opID, terminal, status, hdr, raw, schemaName, textCT)
	if ok && terminal && status == 200 {
		detail = fmt.Sprintf("Task 已终态（%s），HTTP 200", facts.Status)
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

// waitTaskTerminal 轮询 fixture Task 到终态；等不到就 FAIL，不允许后续 /report 探测降级成 404。
func (l *Live) waitTaskTerminal(ctx context.Context, taskID, label string, timeout time.Duration) bool {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	checkName := "live.fixture" + label + ".terminal"
	for {
		status, _, raw, err := l.client.do(wctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, "", nil)
		if err != nil {
			l.recordErr(checkName, err)
			return false
		}
		if status != http.StatusOK {
			l.add(checkName, false, fmt.Sprintf("GET /tasks/%s 返回 %d", taskID, status))
			return false
		}
		var body struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &body)
		if isTerminalTaskStatus(body.Status) {
			l.add(checkName, true,
				fmt.Sprintf("status=%s，耗时 %.1fs", body.Status, time.Since(start).Seconds()))
			return true
		}
		if wctx.Err() != nil {
			l.add(checkName, false,
				fmt.Sprintf("%s 内未到终态，最后 status=%s", timeout, body.Status))
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (l *Live) submitFixture(ctx context.Context, label string) string {
	name := "live.submitTask"
	detailPrefix := "fixture " + label
	if label != "A" {
		name = "live.submitTask.second"
	}
	manifest, ct := l.manifestBytes()

	if label == "A" {
		l.checkDryRun(ctx, manifest, ct)
	}

	status, hdr, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/tasks", manifest, ct, nil)
	if err != nil {
		l.recordErr(name, err)
		return ""
	}
	l.markOp("submitTask")
	if status != http.StatusAccepted {
		l.add(name, false, fmt.Sprintf("%s：期望 HTTP 202，实际 %d：%s", detailPrefix, status, truncate(raw, 500)))
		return ""
	}
	issues := l.validateSuccess("submitTask", status, hdr, raw, "TaskAccepted")
	var accepted struct {
		Task    map[string]any   `json:"task"`
		Runners []map[string]any `json:"runners"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		issues = append(issues, "202 响应不是 JSON 对象："+err.Error())
	} else {
		taskID := asString(accepted.Task["id"])
		if taskID == "" {
			issues = append(issues, "202 的 task.id 为空")
		}
		if len(accepted.Runners) == 0 {
			issues = append(issues, "202 的 runners[] 为空（07 §4.2 应返回含注入行的最终执行序）")
		} else {
			rawRunner, _ := json.Marshal(accepted.Runners[0])
			if missing := l.spec.requireFields("AcceptedRunner", rawRunner); len(missing) > 0 {
				issues = append(issues, "runners[0] 缺字段："+strings.Join(missing, ", "))
			}
		}
		if accepted.Task != nil {
			rawTask, _ := json.Marshal(accepted.Task)
			if missing := l.spec.requireFields("AcceptedTaskRef", rawTask); len(missing) > 0 {
				issues = append(issues, "task 摘要缺字段："+strings.Join(missing, ", "))
			}
		}
		if len(issues) == 0 {
			l.add(name, true, fmt.Sprintf("%s：HTTP 202，task_id=%s，%d 个 runner（含注入）",
				detailPrefix, taskID, len(accepted.Runners)))
			return taskID
		}
	}
	if len(issues) > 0 {
		l.add(name, false, detailPrefix+"："+strings.Join(issues, "；"))
	}
	return ""
}

// opIDFor 按 method+path 找 operationId，避免把新端点的 opID 写死在探针里。
func (l *Live) opIDFor(method, path string) string {
	for _, op := range l.spec.Operations {
		if strings.EqualFold(op.Method, method) && op.Path == path {
			return op.ID
		}
	}
	return ""
}

// submitDeployFixture 提交一个 deploy 形态 Task（用于验证 deploy/logs；不额外计入检查数）。
func (l *Live) submitDeployFixture(ctx context.Context) string {
	profile := l.profile()
	slaVersion := l.runnerVersion(ctx, "aisbench-sla")
	name := "contract-deploy-" + time.Now().Format("20060102-150405")
	manifest := fmt.Sprintf(`{"task":{"name":%q,"deploy":{"provider":"genet","spec":"model: mock-llm\n"},"precheck":"off","runners":[{"name":"probe","runner":"aisbench-sla","version":%q,"execution":{"profile":%q},"input":"{\"sla\":[\"ttft.avg:999999\"],\"work_path\":\"/tmp/aisbench-ws\",\"aisbench_cmd\":\"/runner/tool/tests/stub_aisbench.py\",\"input_len\":64,\"output_len\":8,\"data_num\":4,\"start\":1,\"max\":2,\"max_probes\":2}"}]}}`,
		name, slaVersion, profile)
	status, _, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/tasks", []byte(manifest), "application/json", nil)
	if err != nil {
		l.recordErr("live.submitTask.deploy", err)
		return ""
	}
	l.markOp("submitTask")
	if status != http.StatusAccepted {
		l.add("live.submitTask.deploy", false, fmt.Sprintf("期望 202，实际 %d：%s", status, truncate(raw, 500)))
		return ""
	}
	var accepted struct {
		Task map[string]any `json:"task"`
	}
	_ = json.Unmarshal(raw, &accepted)
	taskID := asString(accepted.Task["id"])
	if taskID == "" {
		l.add("live.submitTask.deploy", false, "202 缺 task.id")
	}
	return taskID
}

// probeDeployLogsDeploy 验证 deploy/logs：pod 缺省 0/1 个 Pod → 200，多 Pod → 409 pod_ambiguous；
// 带 pod → 200 + pods/entries 数组。
func (l *Live) probeDeployLogsDeploy(ctx context.Context, taskID string) {
	opID := l.opIDFor(http.MethodGet, "/api/v1/tasks/{task_id}/deploy/logs")
	if opID == "" {
		l.add("live.getTaskDeployLogs", false, "openapi 缺少 GET /tasks/{task_id}/deploy/logs")
		return
	}
	base := "/api/v1/tasks/" + url.PathEscape(taskID) + "/deploy/logs"
	// 等 deploy 快照出现（提交冻结即有，防御性等待）。
	deadline := time.Now().Add(90 * time.Second)
	for {
		status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/deploy", nil, "", nil)
		if err == nil && status == http.StatusOK {
			var body struct {
				Deploy json.RawMessage `json:"deploy"`
			}
			_ = json.Unmarshal(raw, &body)
			var m map[string]any
			_ = json.Unmarshal(body.Deploy, &m)
			if len(m) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			l.add("live.getTaskDeployLogs", false, "deploy fixture 90s 内没有出现 deploy 快照")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 1) 缺 pod：`pod` 可选。0 或 1 个 Pod → 200；多个 Pod → 409 `pod_ambiguous`。
	st, hdr, raw, err := l.client.do(ctx, http.MethodGet, base, nil, "", nil)
	l.markOp(opID)
	switch {
	case err != nil:
		l.recordErr("live.getTaskDeployLogs.no_pod", err)
	case st == http.StatusOK:
		if issues := l.validateSuccess(opID, st, hdr, raw, "DeployLogs"); len(issues) > 0 {
			l.add("live.getTaskDeployLogs.no_pod", false, strings.Join(issues, "；"))
		} else {
			l.add("live.getTaskDeployLogs.no_pod", true, "HTTP 200（pod 缺省 = 0/1 个 Pod 时直接服务）")
		}
	case st == http.StatusConflict:
		if issues := l.validateError(opID, st, hdr, raw); len(issues) > 0 {
			l.add("live.getTaskDeployLogs.no_pod", false, strings.Join(issues, "；"))
		} else if !strings.Contains(string(raw), "pod_ambiguous") {
			l.add("live.getTaskDeployLogs.no_pod", false, "409 但 detail.reason 不是 pod_ambiguous："+truncate(raw, 200))
		} else {
			l.add("live.getTaskDeployLogs.no_pod", true, "HTTP 409 pod_ambiguous（多 Pod 必须显式指定）")
		}
	default:
		l.add("live.getTaskDeployLogs.no_pod", false, fmt.Sprintf("期望 200 或 409，实际 %d：%s", st, truncate(raw, 300)))
	}

	// 2) 带实际 Pod → 200 + pods/entries 数组。
	podName := "contract-no-pod"
	dst, _, draw, derr := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/deploy", nil, "", nil)
	if derr == nil && dst == http.StatusOK {
		var d struct {
			Pods []struct {
				Name string `json:"name"`
			} `json:"pods"`
		}
		_ = json.Unmarshal(draw, &d)
		if len(d.Pods) > 0 && d.Pods[0].Name != "" {
			podName = d.Pods[0].Name
		}
	}
	path := base + "?pod=" + url.QueryEscape(podName)
	st, hdr, raw, err = l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	if err != nil {
		l.recordErr("live.getTaskDeployLogs", err)
		return
	}
	if st == http.StatusOK {
		if issues := l.validateSuccess(opID, st, hdr, raw, "DeployLogs"); len(issues) > 0 {
			l.add("live.getTaskDeployLogs", false, strings.Join(issues, "；"))
			return
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		var pods, entries []any
		_ = json.Unmarshal(body["pods"], &pods)
		_ = json.Unmarshal(body["entries"], &entries)
		l.add("live.getTaskDeployLogs", true, fmt.Sprintf("HTTP 200，pod=%s pods=%d entries=%d", podName, len(pods), len(entries)))
		return
	}
	if st == http.StatusBadGateway {
		// 没有 pods/log 权限是可解释的环境错误；仍证明请求形状正确。
		l.add("live.getTaskDeployLogs", true, "HTTP 502 EXECUTOR_UNAVAILABLE（缺 pods/log，端点本身可达）")
		return
	}
	l.add("live.getTaskDeployLogs", false, fmt.Sprintf("pod=%s 期望 200，实际 %d：%s", podName, st, truncate(raw, 300)))
}

// probeTaskBundle 覆盖 GET /tasks/{id}/bundle：zip 魔数 + MANIFEST.json + counts 与 /artifacts 同源。
func (l *Live) probeTaskBundle(ctx context.Context, taskID string) {
	const checkName = "live.getTaskBundle"
	opID := l.opIDFor(http.MethodGet, "/api/v1/tasks/{task_id}/bundle")
	if opID == "" {
		l.add(checkName, false, "openapi 缺少 GET /tasks/{task_id}/bundle")
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/bundle", nil, "", nil)
	l.markOp(opID)
	if err != nil {
		l.recordErr(checkName, err)
		return
	}
	if status != http.StatusOK {
		l.add(checkName, false, fmt.Sprintf("期望 200，实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/zip") {
		l.add(checkName, false, fmt.Sprintf("Content-Type=%q，期望 application/zip", ct))
		return
	}
	if len(raw) < 2 || raw[0] != 'P' || raw[1] != 'K' {
		l.add(checkName, false, "响应前 2 字节不是 PK（不是 zip）")
		return
	}
	zr, zerr := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if zerr != nil {
		l.add(checkName, false, "zip.NewReader: "+zerr.Error())
		return
	}
	var manifest *zip.File
	for _, f := range zr.File {
		if f.Name == "MANIFEST.json" {
			manifest = f
			break
		}
	}
	if manifest == nil {
		l.add(checkName, false, "包内缺 MANIFEST.json")
		return
	}
	rc, oerr := manifest.Open()
	if oerr != nil {
		l.add(checkName, false, "打开 MANIFEST.json: "+oerr.Error())
		return
	}
	mb, _ := io.ReadAll(rc)
	_ = rc.Close()
	var man struct {
		Counts struct {
			Artifacts int `json:"artifacts"`
		} `json:"counts"`
	}
	if uerr := json.Unmarshal(mb, &man); uerr != nil {
		l.add(checkName, false, "MANIFEST.json 不是 JSON: "+uerr.Error())
		return
	}
	// 与 /artifacts 条数同源（D-56：MANIFEST 的 counts 必须能和清单对上，不能各算各的）
	ast, _, araw, aerr := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/artifacts", nil, "", nil)
	if aerr != nil || ast != http.StatusOK {
		l.add(checkName, false, "无法用 /artifacts 交叉校验 MANIFEST.counts.artifacts")
		return
	}
	l.markOp("listTaskArtifacts")
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(araw, &list)
	if man.Counts.Artifacts != len(list.Items) {
		l.add(checkName, false, fmt.Sprintf("MANIFEST.counts.artifacts=%d 与 /artifacts items=%d 不一致", man.Counts.Artifacts, len(list.Items)))
		return
	}
	l.add(checkName, true, fmt.Sprintf("HTTP 200 application/zip（%d 字节），MANIFEST.json 存在，artifacts=%d 一致", len(raw), man.Counts.Artifacts))
}

// submitFailureFixture 提交一个必然失败的 service Task（endpoint 不可达），用于 retry / clean。
func (l *Live) submitFailureFixture(ctx context.Context) string {
	name := "retry-" + time.Now().Format("20060102-150405")
	manifest := fmt.Sprintf(`task:
  name: %q
  service:
    url: "http://127.0.0.1:1/v1"
    model_name: mock-llm
  precheck:
    runner: endpoint-probe
    version: %q
    execution: { profile: %q }
    input: |
      {"wait_timeout_seconds":2,"retry_interval_seconds":1}
  runners: []
`, name, l.runnerVersion(ctx, "endpoint-probe"), l.profile())
	status, _, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/tasks", []byte(manifest), "application/yaml", nil)
	if err != nil {
		l.recordErr("live.submitTask.failure", err)
		return ""
	}
	l.markOp("submitTask")
	if status != http.StatusAccepted {
		l.add("live.submitTask.failure", false, fmt.Sprintf("期望 202，实际 %d：%s", status, truncate(raw, 500)))
		return ""
	}
	var accepted struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		l.add("live.submitTask.failure", false, "202 响应缺 task")
		return ""
	}
	taskID := asString(accepted.Task["id"])
	if taskID == "" {
		l.add("live.submitTask.failure", false, "task.id 为空")
		return ""
	}
	l.add("live.submitTask.failure", true, "HTTP 202，task_id="+taskID)
	return taskID
}

func (l *Live) profile() string {
	if l.opts.Profile != "" {
		return l.opts.Profile
	}
	return "k8s-npu"
}

// runnerVersion 从注册表取该 Runner 的最新版本；取不到给占位值，让提交失败信息可读。
func (l *Live) runnerVersion(ctx context.Context, name string) string {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runners/"+url.PathEscape(name), nil, "", nil)
	if err == nil && status == http.StatusOK {
		var body struct {
			Version string `json:"version"`
		}
		_ = json.Unmarshal(raw, &body)
		if body.Version != "" {
			return body.Version
		}
	}
	return "0.0.0"
}

// probeRetryAndClean 覆盖 POST /tasks/{id}/retry 与 POST /tasks/{id}/clean。
func (l *Live) probeRetryAndClean(ctx context.Context) {
	// retry：终态 Task → 新 attempt 全量重跑。
	retryOp := l.opIDFor(http.MethodPost, "/api/v1/tasks/{task_id}/retry")
	if retryOp == "" {
		l.add("live.retryTask", false, "openapi 缺少 POST /tasks/{task_id}/retry")
		return
	}
	retryPath := "/api/v1/tasks/" + url.PathEscape(l.taskF) + "/retry"
	st, th, tr, terr := l.client.do(ctx, http.MethodPost, retryPath, nil, "", nil)
	l.markOp(retryOp)
	if terr != nil {
		l.recordErr("live.retryTask", terr)
		return
	}
	if st != http.StatusAccepted {
		l.add("live.retryTask", false, fmt.Sprintf("期望 202，实际 %d：%s", st, truncate(tr, 300)))
		return
	}
	if issues := l.validateSuccess(retryOp, st, th, tr, "TaskRetryAccepted"); len(issues) > 0 {
		l.add("live.retryTask", false, strings.Join(issues, "；"))
		return
	}
	var accepted struct {
		TaskID    string `json:"task_id"`
		AttemptNo int    `json:"attempt_no"`
		Status    string `json:"status"`
	}
	_ = json.Unmarshal(tr, &accepted)
	ok := accepted.TaskID == l.taskF && accepted.AttemptNo >= 2 && accepted.Status == "queued"
	l.add("live.retryTask", ok, fmt.Sprintf("HTTP 202，task_id=%s attempt_no=%d status=%s",
		accepted.TaskID, accepted.AttemptNo, accepted.Status))

	// 重试落地后 Task 非终态：立刻再 retry 必须 409（非终态不可重试）。
	st2, h2, r2, err2 := l.client.do(ctx, http.MethodPost, retryPath, nil, "", nil)
	if err2 != nil {
		l.recordErr("live.retryTask.non_terminal", err2)
	} else if st2 != http.StatusConflict {
		l.add("live.retryTask.non_terminal", false,
			fmt.Sprintf("非终态 Task 再 retry 期望 409，实际 %d：%s", st2, truncate(r2, 300)))
	} else if issues := l.validateError(retryOp, st2, h2, r2); len(issues) > 0 {
		l.add("live.retryTask.non_terminal", false, strings.Join(issues, "；"))
	} else {
		l.add("live.retryTask.non_terminal", true, "HTTP 409（非终态拒绝重试），Error 形状正确")
	}

	// 等 attempt 2 跑完再探 clean（clean 需要 Task 终态语义）。
	if !l.waitTaskTerminal(ctx, l.taskF, "F2", 120*time.Second) {
		return
	}

	// clean：fixture F 是 service 模式（无部署）→ 409 no_deploy（detail.reason），Error 形状必须正确。
	cleanOp := l.opIDFor(http.MethodPost, "/api/v1/tasks/{task_id}/clean")
	if cleanOp == "" {
		l.add("live.replayTaskClean", false, "openapi 缺少 POST /tasks/{task_id}/clean")
		return
	}
	cleanPath := "/api/v1/tasks/" + url.PathEscape(l.taskF) + "/clean"
	cst, ch, cr, cerr := l.client.do(ctx, http.MethodPost, cleanPath, []byte(`{}`), "application/json", nil)
	l.markOp(cleanOp)
	if cerr != nil {
		l.recordErr("live.replayTaskClean", cerr)
		return
	}
	switch {
	case cst == http.StatusAccepted:
		if issues := l.validateSuccess(cleanOp, cst, ch, cr, "CleanReplayAccepted"); len(issues) > 0 {
			l.add("live.replayTaskClean", false, strings.Join(issues, "；"))
		} else {
			l.add("live.replayTaskClean", true, "HTTP 202（clean 重放受理）")
		}
	case cst == http.StatusConflict:
		if issues := l.validateError(cleanOp, cst, ch, cr); len(issues) > 0 {
			l.add("live.replayTaskClean", false, strings.Join(issues, "；"))
		} else if !strings.Contains(string(cr), "no_deploy") {
			l.add("live.replayTaskClean", false, "409 但 detail.reason 不是 no_deploy："+truncate(cr, 200))
		} else {
			l.add("live.replayTaskClean", true, "HTTP 409 no_deploy（外接模式无部署可拆），Error 形状正确")
		}
	default:
		l.add("live.replayTaskClean", false, fmt.Sprintf("期望 202 或 409，实际 %d：%s", cst, truncate(cr, 300)))
	}
}

// probeAdmin 覆盖管理面读端点与写端点的“错误契约”：
// 契约不创建 profile/cluster/secret/shared-mount（避免污染环境），写操作只断言校验/冲突/404 的 Error 形态。
func (l *Live) probeAdmin(ctx context.Context) {
	type op struct {
		method, path, body string
		allowed            []int
	}
	read := []op{
		{http.MethodGet, "/api/v1/admin/audit?limit=20", "", []int{200}},
		{http.MethodGet, "/api/v1/admin/secrets?limit=20", "", []int{200}},
		{http.MethodGet, "/api/v1/admin/shared-mounts?limit=20", "", []int{200}},
	}
	for _, o := range read {
		base := strings.SplitN(o.path, "?", 2)[0]
		opID := l.opIDFor(o.method, base)
		if opID == "" {
			l.add("live."+strings.TrimPrefix(base, "/api/v1/"), false, "openapi 缺少 "+o.method+" "+base)
			continue
		}
		status, hdr, raw, err := l.client.do(ctx, o.method, o.path, nil, "", nil)
		l.markOp(opID)
		if err != nil {
			l.recordErr("live."+opID, err)
			continue
		}
		if status != http.StatusOK {
			l.add("live."+opID, false, fmt.Sprintf("期望 200，实际 %d：%s", status, truncate(raw, 300)))
			continue
		}
		if issues := l.validateSuccess(opID, status, hdr, raw, ""); len(issues) > 0 {
			l.add("live."+opID, false, strings.Join(issues, "；"))
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			l.add("live."+opID, false, "响应不是 JSON 对象："+err.Error())
			continue
		}
		if _, ok := body["items"]; !ok {
			l.add("live."+opID, false, "缺 items 数组")
			continue
		}
		l.add("live."+opID, true, "HTTP 200，items 存在")
	}

	writes := []op{
		{http.MethodPost, "/api/v1/admin/profiles", "{}", []int{400}},
		{http.MethodPut, "/api/v1/admin/profiles", "{}", []int{400}},
		{http.MethodDelete, "/api/v1/admin/profiles?name=contract-no-such", "", []int{404}},
		{http.MethodPost, "/api/v1/admin/clusters", "{}", []int{400}},
		{http.MethodPut, "/api/v1/admin/clusters", "{}", []int{400}},
		{http.MethodDelete, "/api/v1/admin/clusters?name=contract-no-such", "", []int{404}},
		{http.MethodPost, "/api/v1/admin/secrets", "{}", []int{400}},
		{http.MethodPost, "/api/v1/admin/secrets/contract-no-such/rotate", "{}", []int{400, 404, 409}},
		{http.MethodPost, "/api/v1/admin/shared-mounts", "{}", []int{400}},
		{http.MethodPut, "/api/v1/admin/shared-mounts", "{}", []int{400}},
		{http.MethodDelete, "/api/v1/admin/shared-mounts/contract-no-such", "", []int{404}},
		{http.MethodDelete, "/api/v1/admin/tasks/t-contract-no-such/artifacts", "", []int{404}},
	}
	for _, o := range writes {
		base := strings.SplitN(o.path, "?", 2)[0]
		template := base
		if strings.HasPrefix(base, "/api/v1/admin/secrets/") && strings.HasSuffix(base, "/rotate") {
			template = "/api/v1/admin/secrets/{name}/rotate"
		}
		if strings.HasPrefix(base, "/api/v1/admin/tasks/") && strings.HasSuffix(base, "/artifacts") {
			template = "/api/v1/admin/tasks/{task_id}/artifacts"
		}
		if strings.HasPrefix(base, "/api/v1/admin/shared-mounts/") {
			template = "/api/v1/admin/shared-mounts/{name}"
		}
		opID := l.opIDFor(o.method, template)
		if opID == "" {
			l.add("live."+strings.TrimPrefix(base, "/api/v1/"), false, "openapi 缺少 "+o.method+" "+template)
			continue
		}
		var body []byte
		ct := ""
		if o.body != "" {
			body, ct = []byte(o.body), "application/json"
		}
		status, hdr, raw, err := l.client.do(ctx, o.method, o.path, body, ct, nil)
		l.markOp(opID)
		if err != nil {
			l.recordErr("live."+opID+".negative", err)
			continue
		}
		allowed := false
		for _, s := range o.allowed {
			if status == s {
				allowed = true
			}
		}
		if !allowed {
			l.add("live."+opID+".negative", false, fmt.Sprintf("期望错误码 %v，实际 %d：%s", o.allowed, status, truncate(raw, 300)))
			continue
		}
		if issues := l.validateError(opID, status, hdr, raw); len(issues) > 0 {
			l.add("live."+opID+".negative", false, strings.Join(issues, "；"))
			continue
		}
		l.add("live."+opID+".negative", true, fmt.Sprintf("HTTP %d，Error 形状正确（写端点只验错误契约）", status))
	}
}

func (l *Live) checkDryRun(ctx context.Context, manifest []byte, contentType string) {
	status, hdr, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/tasks?dry_run=true", manifest, contentType, nil)
	if err != nil {
		l.recordErr("live.submitTask.dry_run", err)
		return
	}
	l.markOp("submitTask")
	if status != http.StatusOK {
		l.add("live.submitTask.dry_run", false, fmt.Sprintf("期望 HTTP 200，实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	issues := l.validateSuccess("submitTask", status, hdr, raw, "DryRunResult")
	if len(issues) > 0 {
		l.add("live.submitTask.dry_run", false, strings.Join(issues, "；"))
		return
	}
	var result struct {
		Valid *bool `json:"valid"`
	}
	_ = json.Unmarshal(raw, &result)
	if result.Valid == nil {
		l.add("live.submitTask.dry_run", false, "dry-run 响应缺 valid 字段（07 §4.1）")
		return
	}
	l.add("live.submitTask.dry_run", true, fmt.Sprintf("valid=%v", *result.Valid))
}

func (l *Live) probeTaskReads(ctx context.Context) {
	id := url.PathEscape(l.taskA)
	l.probeSchema(ctx, "getTask", "/api/v1/tasks/"+id, 200, "TaskDetail")
	l.probeSchema(ctx, "listTaskAttempts", "/api/v1/tasks/"+id+"/attempts?limit=50", 200, "AttemptList")
	l.probeSchema(ctx, "listTaskRunners", "/api/v1/tasks/"+id+"/runners?limit=50", 200, "RunnerExecutionList")
	l.probeSchema(ctx, "listTaskEvents", "/api/v1/tasks/"+id+"/events/history?since_seq=0&limit=100", 200, "EventList")
	l.probeSchema(ctx, "getTaskManifest", "/api/v1/tasks/"+id+"/manifest", 200, "ManifestSnapshot")
	l.probeTaskDeploy(ctx, l.taskA)
	l.probeTaskReport(ctx, l.taskA, "/report", "Report", "getTaskReport")
	l.probeTaskReport(ctx, l.taskA, "/report.md", "", "getTaskReportMarkdown")
	l.probeTaskFileBothForms(ctx, l.taskA)
	l.probeLatestRunner(ctx, l.taskA)
}

// probeLatestRunner 覆盖最新 attempt 的 runner 读取端点（getTaskRunner / result / logs / artifacts）。
func (l *Live) probeLatestRunner(ctx context.Context, taskID string) {
	facts, err := l.fetchTaskFacts(ctx, taskID)
	if err != nil || len(facts.RunnerSeqs) == 0 {
		msg := "Task 详情没有 runners[]，无法校验 runner 级端点"
		if err != nil {
			msg = err.Error()
		}
		for _, n := range []string{"getTaskRunner", "getTaskRunnerResult", "getTaskRunnerLogs", "listTaskRunnerArtifacts"} {
			l.add("live."+n, false, msg)
		}
		return
	}
	n := facts.CurrentAttempt
	if n < 1 {
		n = 1
	}
	seq := facts.RunnerSeqs[0]
	id := url.PathEscape(taskID)
	attemptNo := strconv.Itoa(n)
	seqStr := strconv.Itoa(seq)

	l.probeSchema(ctx, "getTaskRunner", fmt.Sprintf("/api/v1/tasks/%s/runners/%s", id, seqStr), 200, "RunnerExecution")
	l.probeSchema(ctx, "listTaskRunnerArtifacts", fmt.Sprintf("/api/v1/tasks/%s/runners/%s/artifacts", id, seqStr), 200, "ArtifactList")
	// result / logs 是状态相关产物：成功路径 200；只产出 html/csv 或平台准备期失败时 404（Error 形状）。
	l.probeOptional(ctx, "getTaskRunnerResult", fmt.Sprintf("/api/v1/tasks/%s/runners/%s/result", id, seqStr), 200, "Result", 404, "")
	l.probeOptional(ctx, "getTaskRunnerLogs", fmt.Sprintf("/api/v1/tasks/%s/runners/%s/logs?stream=stdout&limit=4096", id, seqStr), 200, "", 404, "text/plain")

	// attempt 级权威形态（07 §5.2）。
	l.probeSchema(ctx, "listAttemptRunners", fmt.Sprintf("/api/v1/tasks/%s/attempts/%s/runners", id, attemptNo), 200, "RunnerExecutionList")
	l.probeSchema(ctx, "getAttemptRunner", fmt.Sprintf("/api/v1/tasks/%s/attempts/%s/runners/%s", id, attemptNo, seqStr), 200, "RunnerExecution")
	l.probeSchema(ctx, "listAttemptRunnerArtifacts", fmt.Sprintf("/api/v1/tasks/%s/attempts/%s/runners/%s/artifacts", id, attemptNo, seqStr), 200, "ArtifactList")
	l.probeOptional(ctx, "getAttemptRunnerResult", fmt.Sprintf("/api/v1/tasks/%s/attempts/%s/runners/%s/result", id, attemptNo, seqStr), 200, "Result", 404, "")
	l.probeOptional(ctx, "getAttemptRunnerLogs", fmt.Sprintf("/api/v1/tasks/%s/attempts/%s/runners/%s/logs?stream=stdout&limit=4096", id, attemptNo, seqStr), 200, "", 404, "text/plain")
}

func (l *Live) probeCancels(ctx context.Context) {
	cancelOp := l.opIDFor(http.MethodPost, "/api/v1/tasks/{task_id}/cancel")
	if cancelOp == "" {
		l.add("live.cancelTask", false, "openapi 缺少 POST /tasks/{task_id}/cancel")
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodPost,
		"/api/v1/tasks/"+url.PathEscape(l.taskB)+"/cancel", nil, "", nil)
	if err != nil {
		l.recordErr("live.cancelTask", err)
		return
	}
	l.markOp(cancelOp)
	// 取消：202（受理 cancelling）或 200（幂等 cancelling/canceled）；
	// 409 = Task 已终态后才取消（fixture 竞态），也是 spec 声明的形态，Error 形状必须正确。
	switch {
	case status == http.StatusAccepted || status == http.StatusOK:
		issues := l.validateSuccess(cancelOp, status, hdr, raw, "CancelAccepted")
		if len(issues) > 0 {
			l.add("live.cancelTask", false, strings.Join(issues, "；"))
			return
		}
		l.add("live.cancelTask", true, fmt.Sprintf("HTTP %d，CancelAccepted required 齐全", status))
	case status == http.StatusConflict:
		issues := l.validateError(cancelOp, status, hdr, raw)
		if len(issues) > 0 {
			l.add("live.cancelTask", false, strings.Join(issues, "；"))
			return
		}
		l.add("live.cancelTask", true, "HTTP 409（Task 已终态，spec 声明的取消拒绝），Error 形状正确")
	default:
		l.add("live.cancelTask", false, fmt.Sprintf("期望 202/200（或 409 已终态），实际 %d：%s", status, truncate(raw, 300)))
	}
}

// probeErrorEnvelopes 单独验证错误体形状（404 与 400）。
func (l *Live) probeErrorEnvelopes(ctx context.Context) {
	// 404：合法路由 + 不存在的 Task
	path := "/api/v1/tasks/t-contract-missing-0000"
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	if err != nil {
		l.recordErr("live.error_envelope_404", err)
	} else {
		l.markOp("getTask")
		issues := l.validateError("getTask", status, hdr, raw)
		if len(issues) > 0 {
			l.add("live.error_envelope_404", false, strings.Join(issues, "；"))
		} else {
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			l.add("live.error_envelope_404", true, fmt.Sprintf("HTTP 404，code=%v", obj["code"]))
		}
	}

	// 400：deploy 与 service 同时出现（D-27 互斥）
	invalid := []byte(`{"task":{"name":"contract-invalid","deploy":{"provider":"genet","spec":"x"},"service":{"url":"http://127.0.0.1:1/v1"},"runners":[]}}`)
	status, hdr, raw, err = l.client.do(ctx, http.MethodPost, "/api/v1/tasks", invalid, "application/json", nil)
	if err != nil {
		l.recordErr("live.error_envelope_400", err)
		return
	}
	l.markOp("submitTask")
	issues := l.validateError("submitTask", status, hdr, raw)
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
		"/api/v1/tasks/"+url.PathEscape(l.taskA)+"/events")
	if err != nil {
		l.recordErr("live.streamTaskEvents", err)
		return
	}
	l.markOp("streamTaskEvents")
	if status != http.StatusOK {
		l.add("live.streamTaskEvents", false, fmt.Sprintf("期望 HTTP 200，实际 %d：%s", status, truncate([]byte(first), 300)))
		return
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		l.add("live.streamTaskEvents", false, fmt.Sprintf("Content-Type=%q，期望 text/event-stream", ct))
		return
	}
	if first == "" {
		l.add("live.streamTaskEvents", false, fmt.Sprintf("%s 内没有收到任何行（事件回放或心跳）", window))
		return
	}
	l.add("live.streamTaskEvents", true, fmt.Sprintf("HTTP 200 text/event-stream；首行 %q", truncate([]byte(first), 80)))
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
		got := hdr.Get("Content-Type")
		if !contentTypeAllowed(resp.ContentTypes, got) {
			issues = append(issues, fmt.Sprintf("Content-Type=%q，spec 允许 %v", got, resp.ContentTypes))
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
	if !contentTypeAllowed(resp.ContentTypes, got) {
		return []string{fmt.Sprintf("Content-Type=%q，spec 允许 %v", got, resp.ContentTypes)}
	}
	return nil
}

// contentTypeAllowed 允许多个合法媒体类型（文件下载按扩展名返回 json/plain/markdown/octet-stream）。
func contentTypeAllowed(want []string, got string) bool {
	for _, w := range want {
		if strings.HasPrefix(got, w) {
			return true
		}
	}
	return false
}

// checkRunnerRegistry 用 GET /runners 证明 endpoint-probe 已被扫描入库（K-3：无运行时注册）。
func (l *Live) checkRunnerRegistry(ctx context.Context) {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runners?limit=100", nil, "", nil)
	if err != nil {
		l.recordErr("live.registry.endpoint_probe", err)
		return
	}
	l.markOp("listRunners")
	if status != http.StatusOK {
		l.add("live.registry.endpoint_probe", false, fmt.Sprintf("GET /runners 返回 %d", status))
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
		l.add("live.registry.endpoint_probe", false, "GET /runners 里没有 endpoint-probe（K-3：镜像内 Runner 应被扫描入库）")
		return
	}
	l.add("live.registry.endpoint_probe", true, fmt.Sprintf("%d 个 Runner，含 endpoint-probe", len(list.Items)))

	// getRunner 与 getRunnerVersion：注册表只读投影（附录 C）。
	l.probeSchema(ctx, "getRunner", "/api/v1/runners/endpoint-probe", 200, "Runner")
	l.probeSchema(ctx, "getRunnerVersion", "/api/v1/runners/endpoint-probe/versions/1.0.0", 200, "Runner")

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
		firstN(missing, 8, fmt.Sprintf("%d 个 operation 全部被探针覆盖", len(l.spec.Operations))))
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
	manifest := fmt.Sprintf(`task:
  name: %q
  service:
    url: %s
    model_name: mock-llm
  precheck:
    runner: endpoint-probe
    version: %q
    execution: { profile: %s }
    input: |
      {"wait_timeout_seconds":5,"retry_interval_seconds":1}
  runners:
    - name: contract-a
      runner: aisbench-sla
      version: %q
      execution: { profile: %s }
      timeout_seconds: 600
      input: |
        {"sla":["ttft.avg:999999"],"work_path":"/tmp/aisbench-ws","aisbench_cmd":"/runner/tool/tests/stub_aisbench.py","input_len":64,"output_len":8,"data_num":4,"start":1,"max":2,"max_probes":2}
`, name, l.opts.MockURL, l.runnerVersion(context.Background(), "endpoint-probe"), profile,
		l.runnerVersion(context.Background(), "aisbench-sla"), profile)
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
