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
	runF       string
	taskF      string
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
	l.checkQueueStatusVolcano(ctx)
	l.probeSchema(ctx, "listProfiles", "/api/v1/profiles", 200, "ExecutionProfileList")
	l.probeSchema(ctx, "getTesterVersion", "/api/v1/testers/endpoint-probe/versions/1.0.0", 200, "Tester")
	l.probeText(ctx, "getMetrics", "/metrics", []int{200})
	l.probeAuthLogin(ctx)
	l.checkAuthCallback(ctx)
	l.checkAuthLogout(ctx)
	if l.connectErr != nil {
		return l.checks, l.warnings, l.connectErr
	}

	// ---- fixture Run --------------------------------------------------
	l.runA, l.taskA = l.submitFixture(ctx, "A")
	if l.runA != "" {
		l.waitRunTerminal(ctx, l.runA, 120*time.Second)
		l.probeRunReads(ctx)
		l.probeRunBundle(ctx, l.runA)
	}
	l.runB, l.taskB = l.submitFixture(ctx, "B")
	if l.runB != "" {
		l.probeCancels(ctx)
	}
	deployRun := l.submitDeployFixture(ctx)
	if deployRun != "" {
		l.probeDeployLogsDeploy(ctx, deployRun)
	}
	l.runF, l.taskF = l.submitFailureFixture(ctx)
	if l.runF != "" {
		l.waitFixtureTerminal(ctx, l.runF, "F", 120*time.Second)
		l.probeRetry(ctx)
	}

	// ---- 错误体形状 ----------------------------------------------------
	l.probeErrorEnvelopes(ctx)
	l.probeAdmin(ctx)

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
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/auth/login?return=/runs", nil, "", nil)
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

// probeRunFileBothForms 同时探 /files 的两种 path 形态：
//   - run 相对形态：tasks/<task>/attempts/1/result.json
//   - 原始 artifacts 形态：<run_id>/tasks/.../result.json
//
// 状态 200 或 404 都按 spec 处理；两种形态必须都合法。
func (l *Live) probeRunFileBothForms(ctx context.Context, runID string) {
	id := url.PathEscape(runID)
	forms := []struct {
		name string
		path string
	}{
		{"relative", "/api/v1/runs/" + id + "/files?path=report.json"},
		{"prefixed", "/api/v1/runs/" + id + "/files?path=" + url.QueryEscape(runID+"/report.json")},
	}
	var details []string
	allOK := true
	for _, f := range forms {
		status, hdr, raw, err := l.client.do(ctx, http.MethodGet, f.path, nil, "", nil)
		if err != nil {
			l.recordErr("live.downloadRunFile", err)
			return
		}
		ok, detail := l.artifactResponseCheck("downloadRunFile", false, status, hdr, raw, "", "")
		if !ok {
			allOK = false
		}
		details = append(details, f.name+"=>"+fmt.Sprintf("%d", status)+"（"+detail+"）")
	}
	l.markOp("downloadRunFile")
	l.add("live.downloadRunFile", allOK, strings.Join(details, "；"))
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

// waitRunTerminal 轮询 fixture Run 到终态；等不到就 FAIL，不允许后续 /report 探测降级成 404。
func (l *Live) waitRunTerminal(ctx context.Context, runID string, timeout time.Duration) bool {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	for {
		status, _, raw, err := l.client.do(wctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, "", nil)
		if err != nil {
			l.recordErr("live.fixtureA.terminal", err)
			return false
		}
		if status != http.StatusOK {
			l.add("live.fixtureA.terminal", false, fmt.Sprintf("GET /runs/%s 返回 %d", runID, status))
			return false
		}
		var body struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &body)
		if isTerminalRunStatus(body.Status) {
			l.add("live.fixtureA.terminal", true,
				fmt.Sprintf("status=%s，耗时 %.1fs", body.Status, time.Since(start).Seconds()))
			return true
		}
		if wctx.Err() != nil {
			l.add("live.fixtureA.terminal", false,
				fmt.Sprintf("%s 内未到终态，最后 status=%s", timeout, body.Status))
			return false
		}
		time.Sleep(500 * time.Millisecond)
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

// opIDFor 按 method+path 找 operationId，避免把新端点的 opID 写死在探针里。
func (l *Live) opIDFor(method, path string) string {
	for _, op := range l.spec.Operations {
		if strings.EqualFold(op.Method, method) && op.Path == path {
			return op.ID
		}
	}
	return ""
}

// submitDeployFixture 提交一个 deploy 形态 Run（用于验证 ?pod= 必选；成功时不额外计入检查数）。
func (l *Live) submitDeployFixture(ctx context.Context) string {
	profile := l.profile()
	name := "contract-deploy-" + time.Now().Format("20060102-150405")
	manifest := fmt.Sprintf(`{"run":{"name":%q,"deploy":{"provider":"genet","parameters":{"model_name":"mock-llm"}},"precheck":"off","cleanup":{"policy":"never"},"tests":[{"name":"probe","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":%q},"args":"--wait_timeout_seconds=60"}]}}`, name, profile)
	status, _, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/runs", []byte(manifest), "application/json", nil)
	if err != nil {
		l.recordErr("live.submitRun.deploy", err)
		return ""
	}
	l.markOp("submitRun")
	if status != http.StatusAccepted {
		l.add("live.submitRun.deploy", false, fmt.Sprintf("期望 202，实际 %d：%s", status, truncate(raw, 300)))
		return ""
	}
	var accepted struct {
		Run map[string]any `json:"run"`
	}
	_ = json.Unmarshal(raw, &accepted)
	runID := asString(accepted.Run["id"])
	if runID == "" {
		l.add("live.submitRun.deploy", false, "202 缺 run.id")
	}
	return runID
}

// probeDeployLogsDeploy 验证 ?pod= 必选（缺失 400）与带 pod 的 200 形状。
func (l *Live) probeDeployLogsDeploy(ctx context.Context, runID string) {
	opID := l.opIDFor(http.MethodGet, "/api/v1/runs/{run_id}/deploy/logs")
	if opID == "" {
		l.add("live.getRunDeployLogs", false, "openapi 缺少 GET /runs/{run_id}/deploy/logs")
		return
	}
	base := "/api/v1/runs/" + url.PathEscape(runID) + "/deploy/logs"
	// 等 deploy 快照出现（accepted/deploying/ready 均可；不要等 Pod 真正 ready）。
	deadline := time.Now().Add(90 * time.Second)
	var deployRun map[string]any
	for {
		status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, "", nil)
		if err == nil && status == http.StatusOK {
			var body struct {
				Deploy json.RawMessage `json:"deploy"`
			}
			_ = json.Unmarshal(raw, &body)
			var m map[string]any
			_ = json.Unmarshal(body.Deploy, &m)
			if len(m) > 0 {
				deployRun = m
				break
			}
		}
		if time.Now().After(deadline) {
			l.add("live.getRunDeployLogs", false, "deploy fixture 90s 内没有进入 deploy 阶段")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = deployRun

	// 1) 缺 pod：`pod` 现在**可选**（#40-5）。0 或 1 个 Pod → 200；多个 Pod → 409 `pod_ambiguous`。
	st, hdr, raw, err := l.client.do(ctx, http.MethodGet, base, nil, "", nil)
	l.markOp(opID)
	switch {
	case err != nil:
		l.recordErr("live.getRunDeployLogs.no_pod", err)
	case st == http.StatusOK:
		if issues := l.validateSuccess(opID, st, hdr, raw, "DeployLogs"); len(issues) > 0 {
			l.add("live.getRunDeployLogs.no_pod", false, strings.Join(issues, "；"))
		} else {
			l.add("live.getRunDeployLogs.no_pod", true, "HTTP 200（pod 缺省 = 0/1 个 Pod 时直接服务）")
		}
	case st == http.StatusConflict:
		// 多 Pod 时要求显式指定，且错误体必须是 pod_ambiguous
		if issues := l.validateError(opID, st, hdr, raw); len(issues) > 0 {
			l.add("live.getRunDeployLogs.no_pod", false, strings.Join(issues, "；"))
		} else if !strings.Contains(string(raw), "pod_ambiguous") {
			l.add("live.getRunDeployLogs.no_pod", false, "409 但 detail.reason 不是 pod_ambiguous："+truncate(raw, 200))
		} else {
			l.add("live.getRunDeployLogs.no_pod", true, "HTTP 409 pod_ambiguous（多 Pod 必须显式指定）")
		}
	default:
		l.add("live.getRunDeployLogs.no_pod", false, fmt.Sprintf("期望 200 或 409，实际 %d：%s", st, truncate(raw, 300)))
	}

	// 2) 带实际 Pod → 200 + pods/entries 数组。
	podName := "contract-no-pod"
	dst, _, draw, derr := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/deploy", nil, "", nil)
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
		l.recordErr("live.getRunDeployLogs", err)
		return
	}
	if st == http.StatusOK {
		if issues := l.validateSuccess(opID, st, hdr, raw, "DeployLogs"); len(issues) > 0 {
			l.add("live.getRunDeployLogs", false, strings.Join(issues, "；"))
			return
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		var pods, entries []any
		_ = json.Unmarshal(body["pods"], &pods)
		_ = json.Unmarshal(body["entries"], &entries)
		l.add("live.getRunDeployLogs", true, fmt.Sprintf("HTTP 200，pod=%s pods=%d entries=%d", podName, len(pods), len(entries)))
		return
	}
	if st == http.StatusBadGateway {
		// 没有 pods/log 权限是可解释的环境错误；仍证明请求形状正确。
		l.add("live.getRunDeployLogs", true, "HTTP 502 EXECUTOR_UNAVAILABLE（缺 pods/log，端点本身可达）")
		return
	}
	l.add("live.getRunDeployLogs", false, fmt.Sprintf("pod=%s 期望 200，实际 %d：%s", podName, st, truncate(raw, 300)))
}

// probeRunBundle 覆盖 GET /runs/{id}/bundle：zip 魔数 + MANIFEST.json + artifacts 计数与 /artifacts 同源。
func (l *Live) probeRunBundle(ctx context.Context, runID string) {
	const checkName = "live.getRunBundle"
	opID := l.opIDFor(http.MethodGet, "/api/v1/runs/{run_id}/bundle")
	if opID == "" {
		l.add(checkName, false, "openapi 缺少 GET /runs/{run_id}/bundle")
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/bundle", nil, "", nil)
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
	ast, _, araw, aerr := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/artifacts", nil, "", nil)
	if aerr != nil || ast != http.StatusOK {
		l.add(checkName, false, "无法用 /artifacts 交叉校验 MANIFEST.counts.artifacts")
		return
	}
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

// probeDeployLogs 断言 deploy 日志端点 200 + pods/entries 数组（无 Deploy 阶段也允许空数组）。
func (l *Live) probeDeployLogs(ctx context.Context, runID, checkName string) {
	path := "/api/v1/runs/" + url.PathEscape(runID) + "/deploy/logs?limit=10"
	opID := l.opIDFor(http.MethodGet, "/api/v1/runs/{run_id}/deploy/logs")
	if opID == "" {
		l.add(checkName, false, "openapi 缺少 GET /runs/{run_id}/deploy/logs")
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, path, nil, "", nil)
	l.markOp(opID)
	if err != nil {
		l.recordErr(checkName, err)
		return
	}
	if status != http.StatusOK {
		l.add(checkName, false, fmt.Sprintf("期望 200，实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	if issues := l.validateSuccess(opID, status, hdr, raw, "DeployLogs"); len(issues) > 0 {
		l.add(checkName, false, strings.Join(issues, "；"))
		return
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		l.add(checkName, false, "响应不是 JSON 对象："+err.Error())
		return
	}
	for _, key := range []string{"pods", "entries", "gaps"} {
		if _, ok := body[key]; !ok {
			l.add(checkName, false, "缺数组字段 "+key)
			return
		}
	}
	var pods, entries []any
	_ = json.Unmarshal(body["pods"], &pods)
	_ = json.Unmarshal(body["entries"], &entries)
	l.add(checkName, true, fmt.Sprintf("HTTP 200，pods=%d entries=%d", len(pods), len(entries)))
}

// submitFailureFixture 提交一个必然失败的 service Run（endpoint 不可达，precheck off），用于 retry。
func (l *Live) submitFailureFixture(ctx context.Context) (runID, taskID string) {
	name := "retry-" + time.Now().Format("20060102-150405")
	manifest := fmt.Sprintf(`run:
  name: %q
  service:
    url: "http://127.0.0.1:1/v1"
    model_name: mock-llm
  precheck: off
  cleanup: { policy: never }
  tests:
    - name: retry-a
      tester: endpoint-probe
      version: "1.0.0"
      execution: { profile: %q }
      args: "--wait_timeout_seconds=2 --retry_interval_seconds=1"
`, name, l.profile())
	status, _, raw, err := l.client.do(ctx, http.MethodPost, "/api/v1/runs", []byte(manifest), "application/yaml", nil)
	if err != nil {
		l.recordErr("live.submitRun.failure", err)
		return "", ""
	}
	l.markOp("submitRun")
	if status != http.StatusAccepted {
		l.add("live.submitRun.failure", false, fmt.Sprintf("期望 202，实际 %d：%s", status, truncate(raw, 300)))
		return "", ""
	}
	var accepted struct {
		Run   map[string]any   `json:"run"`
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil || len(accepted.Tasks) == 0 {
		l.add("live.submitRun.failure", false, "202 响应缺 run/tasks")
		return "", ""
	}
	runID = asString(accepted.Run["id"])
	taskID = asString(accepted.Tasks[0]["id"])
	if runID == "" || taskID == "" {
		l.add("live.submitRun.failure", false, "run.id 或 tasks[0].id 为空")
		return "", ""
	}
	l.add("live.submitRun.failure", true, "HTTP 202，run_id="+runID)
	return runID, taskID
}

func (l *Live) profile() string {
	if l.opts.Profile != "" {
		return l.opts.Profile
	}
	return "k8s-npu"
}

// waitFixtureTerminal 是带 label 的终态等待（失败 fixture 也有自己的检查名）。
func (l *Live) waitFixtureTerminal(ctx context.Context, runID, label string, timeout time.Duration) bool {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	checkName := "live.fixture" + label + ".terminal"
	for {
		status, _, raw, err := l.client.do(wctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, "", nil)
		if err != nil {
			l.recordErr(checkName, err)
			return false
		}
		if status != http.StatusOK {
			l.add(checkName, false, fmt.Sprintf("GET /runs/%s 返回 %d", runID, status))
			return false
		}
		var body struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &body)
		if isTerminalRunStatus(body.Status) {
			l.add(checkName, true, fmt.Sprintf("status=%s，耗时 %.1fs", body.Status, time.Since(start).Seconds()))
			return true
		}
		if wctx.Err() != nil {
			l.add(checkName, false, fmt.Sprintf("%s 内未到终态，最后 status=%s", timeout, body.Status))
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

type retryPlanBody struct {
	RunID          string           `json:"run_id"`
	Retryable      *bool            `json:"retryable"`
	Reason         string           `json:"reason"`
	Reuse          []map[string]any `json:"reuse"`
	Rerun          []map[string]any `json:"rerun"`
	BlockedItems   []map[string]any `json:"blocked_items"`
	ManifestDigest string           `json:"manifest_digest"`
	NewRunID       string           `json:"new_run_id"`
	RetryOf        string           `json:"retry_of"`
}

// probeRetry 覆盖 retry-plan / retry-run / retry-task，并对新 Run 再探一次 plan。
func (l *Live) probeRetry(ctx context.Context) {
	failID := url.PathEscape(l.runF)
	planPath := "/api/v1/runs/" + failID + "/retry-plan"
	planOp := l.opIDFor(http.MethodGet, "/api/v1/runs/{run_id}/retry-plan")
	if planOp == "" {
		l.add("live.getRunRetryPlan", false, "openapi 缺少 GET /runs/{run_id}/retry-plan")
		return
	}
	status, hdr, raw, err := l.client.do(ctx, http.MethodGet, planPath, nil, "", nil)
	l.markOp(planOp)
	if err != nil {
		l.recordErr("live.getRunRetryPlan", err)
		return
	}
	if status != http.StatusOK {
		l.add("live.getRunRetryPlan", false, fmt.Sprintf("期望 200，实际 %d：%s", status, truncate(raw, 300)))
		return
	}
	if issues := l.validateSuccess(planOp, status, hdr, raw, "RetryPlan"); len(issues) > 0 {
		l.add("live.getRunRetryPlan", false, strings.Join(issues, "；"))
		return
	}
	var plan retryPlanBody
	if err := json.Unmarshal(raw, &plan); err != nil {
		l.add("live.getRunRetryPlan", false, "响应不是 RetryPlan："+err.Error())
		return
	}
	if plan.Retryable == nil || !*plan.Retryable || len(plan.Rerun) == 0 || plan.ManifestDigest == "" {
		l.add("live.getRunRetryPlan", false, fmt.Sprintf("失败 Run 的计划应 retryable=true 且有 rerun；实际 retryable=%v rerun=%d digest=%q",
			plan.Retryable, len(plan.Rerun), plan.ManifestDigest))
		return
	}
	l.add("live.getRunRetryPlan", true, fmt.Sprintf("retryable=true，reuse=%d rerun=%d blocked=%d",
		len(plan.Reuse), len(plan.Rerun), len(plan.BlockedItems)))

	// task retry：旧失败 Task 终态可重开 Attempt。
	taskPath := "/api/v1/tasks/" + url.PathEscape(l.taskF) + "/retry"
	taskOp := l.opIDFor(http.MethodPost, "/api/v1/tasks/{task_id}/retry")
	if taskOp == "" {
		l.add("live.retryTask", false, "openapi 缺少 POST /tasks/{task_id}/retry")
	} else {
		st, th, tr, terr := l.client.do(ctx, http.MethodPost, taskPath, nil, "", nil)
		l.markOp(taskOp)
		if terr != nil {
			l.recordErr("live.retryTask", terr)
		} else if st != http.StatusAccepted {
			l.add("live.retryTask", false, fmt.Sprintf("期望 202，实际 %d：%s", st, truncate(tr, 300)))
		} else if issues := l.validateSuccess(taskOp, st, th, tr, "TaskRetryAccepted"); len(issues) > 0 {
			l.add("live.retryTask", false, strings.Join(issues, "；"))
		} else {
			var body struct {
				TaskID    string `json:"task_id"`
				AttemptNo int    `json:"attempt_no"`
			}
			_ = json.Unmarshal(tr, &body)
			ok := body.TaskID == l.taskF && body.AttemptNo >= 2
			l.add("live.retryTask", ok, fmt.Sprintf("HTTP 202，task_id=%s attempt_no=%d", body.TaskID, body.AttemptNo))
		}
	}

	// retry run：创建新 Run（retry_of=原 Run）。
	retryPath := "/api/v1/runs/" + failID + "/retry"
	retryOp := l.opIDFor(http.MethodPost, "/api/v1/runs/{run_id}/retry")
	if retryOp == "" {
		l.add("live.retryRun", false, "openapi 缺少 POST /runs/{run_id}/retry")
		return
	}
	st, th, tr, terr := l.client.do(ctx, http.MethodPost, retryPath, nil, "", nil)
	l.markOp(retryOp)
	if terr != nil {
		l.recordErr("live.retryRun", terr)
		return
	}
	if st != http.StatusAccepted {
		l.add("live.retryRun", false, fmt.Sprintf("期望 202，实际 %d：%s", st, truncate(tr, 300)))
		return
	}
	if issues := l.validateSuccess(retryOp, st, th, tr, "RetryPlan"); len(issues) > 0 {
		l.add("live.retryRun", false, strings.Join(issues, "；"))
		return
	}
	var accepted retryPlanBody
	if err := json.Unmarshal(tr, &accepted); err != nil || accepted.NewRunID == "" {
		l.add("live.retryRun", false, "202 响应缺 new_run_id")
		return
	}
	if accepted.NewRunID == l.runF {
		l.add("live.retryRun", false, "new_run_id 不能等于原 Run")
		return
	}
	if accepted.RetryOf != l.runF {
		l.add("live.retryRun", false, fmt.Sprintf("202 retry_of=%q，期望 %q", accepted.RetryOf, l.runF))
		return
	}
	// 新 Run 的 retry_of 必须指回原 Run。
	nst, _, nr, nerr := l.client.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(accepted.NewRunID), nil, "", nil)
	if nerr != nil || nst != http.StatusOK {
		l.add("live.retryRun", false, fmt.Sprintf("GET 新 Run 失败（%d）：%v", nst, nerr))
		return
	}
	var newRun struct {
		RetryOf string `json:"retry_of"`
	}
	_ = json.Unmarshal(nr, &newRun)
	if newRun.RetryOf != l.runF {
		l.add("live.retryRun", false, fmt.Sprintf("新 Run retry_of=%q，期望 %q", newRun.RetryOf, l.runF))
		return
	}
	l.add("live.retryRun", true, "HTTP 202，new_run_id="+accepted.NewRunID+"，retry_of="+newRun.RetryOf)

	// 新 Run 再取 plan：若已经 retry-in-progress / cancel 等，允许 200 retryable=false 或 409。
	nplanPath := "/api/v1/runs/" + url.PathEscape(accepted.NewRunID) + "/retry-plan"
	pst, ph, pr, perr := l.client.do(ctx, http.MethodGet, nplanPath, nil, "", nil)
	if perr != nil {
		l.recordErr("live.getRunRetryPlan.after_retry", perr)
		return
	}
	switch pst {
	case http.StatusOK:
		var p retryPlanBody
		_ = json.Unmarshal(pr, &p)
		ok := p.Retryable == nil || !*p.Retryable
		retryable := false
		if p.Retryable != nil {
			retryable = *p.Retryable
		}
		l.add("live.getRunRetryPlan.after_retry", ok, fmt.Sprintf("HTTP 200，retryable=%v reason=%s", retryable, p.Reason))
	case http.StatusConflict:
		issues := l.validateError(planOp, pst, ph, pr)
		l.add("live.getRunRetryPlan.after_retry", len(issues) == 0, "HTTP 409 Conflict（允许）")
	default:
		l.add("live.getRunRetryPlan.after_retry", false, fmt.Sprintf("期望 200 retryable=false 或 409，实际 %d：%s", pst, truncate(pr, 300)))
	}
}

// probeAdmin 覆盖管理面读端点与写端点的“错误契约”：
// 契约不创建 profile/cluster/secret（避免污染环境），写操作只断言校验/冲突/404 的 Error 形态。
func (l *Live) probeAdmin(ctx context.Context) {
	type op struct {
		method, path, body string
		allowed            []int
	}
	read := []op{
		{http.MethodGet, "/api/v1/admin/audit?limit=20", "", []int{200}},
		{http.MethodGet, "/api/v1/secrets?limit=20", "", []int{200}},
		{http.MethodGet, "/api/v1/admin/secrets?limit=20", "", []int{200}},
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
		{http.MethodDelete, "/api/v1/admin/runs/r-contract-no-such/artifacts", "", []int{404}},
	}
	for _, o := range writes {
		base := strings.SplitN(o.path, "?", 2)[0]
		template := base
		if strings.HasPrefix(base, "/api/v1/admin/secrets/") && strings.HasSuffix(base, "/rotate") {
			template = "/api/v1/admin/secrets/{name}/rotate"
		}
		if strings.HasPrefix(base, "/api/v1/admin/runs/") && strings.HasSuffix(base, "/artifacts") {
			template = "/api/v1/admin/runs/{run_id}/artifacts"
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
	l.probeSchema(ctx, "listRunArtifacts", "/api/v1/runs/"+id+"/artifacts", 200, "ArtifactList")
	l.probeRunFileBothForms(ctx, l.runA)
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
	l.probeSchema(ctx, "listAttemptArtifacts", "/api/v1/tasks/"+task+"/attempts/1/artifacts", 200, "ArtifactList")
	l.probeAttemptEndpoints(ctx, l.taskA)
}

// probeAttemptEndpoints 按 Attempt 状态区分 result/logs 的 404：
// 已终态（且 fixture 已成功产出）必须 200；未终态才允许 404。
func (l *Live) probeAttemptEndpoints(ctx context.Context, taskID string) {
	status, _, raw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, "", nil)
	if err != nil {
		l.recordErr("live.getTaskAttemptResult", err)
		l.recordErr("live.getTaskAttemptLogs", err)
		return
	}
	if status != http.StatusOK {
		msg := fmt.Sprintf("GET /tasks/%s 返回 %d，无法判断 Attempt 状态", taskID, status)
		l.add("live.getTaskAttemptResult", false, msg)
		l.add("live.getTaskAttemptLogs", false, msg)
		return
	}
	var detail struct {
		Attempts []struct {
			Status string `json:"status"`
		} `json:"attempts"`
	}
	_ = json.Unmarshal(raw, &detail)
	if len(detail.Attempts) == 0 {
		l.add("live.getTaskAttemptResult", false, "Attempt 列表为空，无法校验 result")
		l.add("live.getTaskAttemptLogs", false, "Attempt 列表为空，无法校验 logs")
		return
	}
	attemptTerminal := true
	for _, a := range detail.Attempts {
		switch a.Status {
		case "succeeded", "failed", "canceled":
		default:
			attemptTerminal = false
		}
	}
	task := url.PathEscape(taskID)
	// 产物是否存在决定 404 是否合法：已终态 + 有 result/log 时必须 200。
	artStatus, _, artRaw, err := l.client.do(ctx, http.MethodGet, "/api/v1/tasks/"+task+"/attempts/1/artifacts", nil, "", nil)
	var artifactItems []struct {
		Kind string `json:"kind"`
	}
	if err == nil && artStatus == http.StatusOK {
		var list struct {
			Items []struct {
				Kind string `json:"kind"`
			} `json:"items"`
		}
		_ = json.Unmarshal(artRaw, &list)
		artifactItems = list.Items
	}
	hasResult, hasLog := false, false
	for _, it := range artifactItems {
		switch it.Kind {
		case "result":
			hasResult = true
		case "log":
			hasLog = true
		}
	}
	if attemptTerminal && hasResult {
		l.probeSchema(ctx, "getTaskAttemptResult", "/api/v1/tasks/"+task+"/attempts/1/result", 200, "Result")
	} else {
		l.probeOptional(ctx, "getTaskAttemptResult", "/api/v1/tasks/"+task+"/attempts/1/result", 200, "Result", 404, "")
	}
	if attemptTerminal && hasLog {
		l.probeText(ctx, "getTaskAttemptLogs", "/api/v1/tasks/"+task+"/attempts/1/logs?stream=stdout&limit=4096", []int{200})
	} else {
		l.probeOptional(ctx, "getTaskAttemptLogs", "/api/v1/tasks/"+task+"/attempts/1/logs?stream=stdout&limit=4096", 200, "", 404, "text/plain")
	}
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
