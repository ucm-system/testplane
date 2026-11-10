package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const version = "0.2.0"

var errHelp = errors.New("help requested")

var commands = map[string]bool{
	"run": true, "submit": true, "wait": true, "assert": true,
	"version": true, "help": true,
}

const usageText = `e2e 是 Argus 的端到端驱动脚本（不是 go test）：
提交 Task → 轮询终态 → 断言报告/事件序列/runner 日志与 result.json。

用法：
  e2e [命令] [参数]

命令：
  run       提交 + 等待终态 + 全量断言（默认命令）
  submit    只提交，打印 task_id（配合 wait/assert 分步跑）
  wait      轮询指定 Task 到终态
  assert    对已终态的 Task 做全量断言
  version   打印版本
  help      打印本帮助

连接与身份：
  --base-url       控制面地址（默认 http://127.0.0.1:8080）
  --api-prefix     API 前缀（默认 /api/v1；/healthz 等运维端点不带前缀）
  --user           dev 模式身份（默认 dev；dev 鉴权：Authorization: Bearer <subject>）
  --token          显式 Bearer（优先于 --user）
  --no-auth        不发 Authorization（dev 模式允许匿名时用）

提交内容（不指定 --manifest 时用内置模板，v2：嵌套 task:、deploy XOR service、runners[].input）：
  --manifest          已有 Manifest 文件（YAML 或 JSON）
  --mode              deploy|service（默认 deploy；service 不需要 Genet）
  --mock-url          内置模板指向的 mock-llm base URL（默认 http://127.0.0.1:18080/v1）
  --model             模型名（默认 mock-llm）
  --profile           execution.profile（默认 k8s-npu；传空串则不写）
  --name              Task 名（默认 e2e-<时间戳>）
  --deploy-provider   deploy.provider（默认 genet）
  --runner            测试类 runner（默认 aisbench-sla）
  --runner-version    测试类 runner 版本（默认 0.5.0）
  --precheck-version  显式 precheck 的 endpoint-probe 版本（默认 1.0.0）
  --dry-run-first     提交前先 POST /tasks?dry_run=true（默认关）

等待与期望：
  --task-id        wait/assert 使用的 Task ID
  --expect         期望终态，逗号分隔（默认 succeeded）
                   可选 succeeded|failed|service_unavailable|canceled
                   （v2 没有 completed_with_errors——其语义并入 failed，D-73）
  --timeout        等待终态总时长（默认 5m）
  --poll           轮询间隔（默认 2s）
  --sse-timeout    SSE 冒烟/回退抓取的等待窗口（默认 10s）
  --report-wait    报告异步生成的等待窗口（默认 30s；C-7.2 的 SLA 是终态后 ≤2s）
  --expect-error-code         期望 task.error.code（可选）
  --expect-runner-error-code  期望某个失败 runner 的 error.code（可选）
  --expect-retryable          期望失败对象的 retryable=true|false（可选）

其它：
  --json-out       把机器可读结果另存为文件
  --strict         警告也算失败
  --allow-empty-logs  日志为空只警告（默认失败）
  --verbose        打印每个 HTTP 请求与 Manifest

输出：
  stderr  人类可读摘要（状态轨迹、失败项、警告）
  stdout  一行 JSON：{"ok":...,"task_id":...,"checks":[...],"warnings":[...]}

退出码：
  0  全部检查通过
  1  断言失败 / 控制面返回错误 / 等待超时
  2  参数或配置错误
  3  连不上控制面（会明确提示 8080 可能没起）

示例：
  e2e run
  e2e run --mode service --mock-url http://127.0.0.1:18080/v1 --poll 1s
  e2e submit --manifest task.yaml --json-out .data/e2e/submit.json
  e2e wait --task-id t-20261110-0001
  e2e assert --task-id t-20261110-0001 --expect service_unavailable \
      --expect-runner-error-code PRECHECK_FAILED --expect-retryable=true
`

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(argv []string) int {
	opts, command, err := parseArgs(argv)
	if err != nil {
		if errors.Is(err, errHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "e2e: %v\n\n提示：e2e --help 查看用法\n", err)
		return 2
	}
	if command == "version" {
		fmt.Fprintln(os.Stdout, version)
		return 0
	}
	if command == "help" {
		fmt.Fprint(os.Stdout, usageText)
		return 0
	}

	start := time.Now()
	app := newApp(opts)
	// 外层兜底超时：比等待终态的窗口多留 2 分钟给报告/事件/日志断言。
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout+2*time.Minute)
	defer cancel()

	runErr := app.execute(ctx, command)
	return app.finish(command, start, runErr)
}

func (a *App) execute(ctx context.Context, command string) error {
	switch command {
	case "run":
		out, err := a.submit(ctx)
		if err != nil {
			return err
		}
		a.opts.TaskID = out.TaskID
		a.logf("已提交 Task %s，等待终态（最多 %s）…", out.TaskID, a.opts.Timeout)
		wr, err := a.wait(ctx, out.TaskID)
		if wr != nil {
			a.task = wr.Task
			a.statusTrace = wr.Trace
		}
		if err != nil {
			return err
		}
		a.logf("Task %s 到达终态 %s（%s），开始断言…", wr.Task.ID, wr.Task.Status, wr.Elapsed.Round(time.Millisecond))
		return a.assertTask(ctx, wr.Task)

	case "submit":
		out, err := a.submit(ctx)
		if err != nil {
			return err
		}
		a.opts.TaskID = out.TaskID
		a.logf("已提交 Task %s。继续：e2e wait --task-id=%s", out.TaskID, out.TaskID)
		return nil

	case "wait":
		if a.opts.TaskID == "" {
			return &UsageError{Msg: "wait 需要 --task-id"}
		}
		wr, err := a.wait(ctx, a.opts.TaskID)
		if wr != nil {
			a.task = wr.Task
			a.statusTrace = wr.Trace
		}
		if err != nil {
			return err
		}
		a.logf("Task %s 已到终态 %s（%s）。继续：e2e assert --task-id=%s", wr.Task.ID, wr.Task.Status, wr.Elapsed.Round(time.Millisecond), wr.Task.ID)
		return nil

	case "assert":
		if a.opts.TaskID == "" {
			return &UsageError{Msg: "assert 需要 --task-id"}
		}
		task, err := a.fetchTask(ctx, a.opts.TaskID)
		if err != nil {
			return err
		}
		a.task = task
		if !isTerminalTask(task.Status) {
			return fmt.Errorf("Task %s 还没到终态（当前 %s）；先跑 e2e wait --task-id=%s", task.ID, task.Status, task.ID)
		}
		return a.assertTask(ctx, task)

	default:
		return &UsageError{Msg: fmt.Sprintf("未知命令 %q（支持 run|submit|wait|assert|version|help）", command)}
	}
}

func parseArgs(argv []string) (Options, string, error) {
	command, args, err := splitCommand(argv)
	if err != nil {
		return Options{}, "", err
	}

	fs := flag.NewFlagSet("e2e", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		baseURL         = fs.String("base-url", "http://127.0.0.1:8080", "控制面地址")
		apiPrefix       = fs.String("api-prefix", "/api/v1", "API 前缀")
		user            = fs.String("user", "dev", "dev 模式身份")
		token           = fs.String("token", "", "显式 Bearer")
		noAuth          = fs.Bool("no-auth", false, "不发 Authorization")
		manifestFile    = fs.String("manifest", "", "已有 Manifest 文件")
		mode            = fs.String("mode", "deploy", "deploy|service")
		mockURL         = fs.String("mock-url", "http://127.0.0.1:18080/v1", "mock-llm base URL")
		model           = fs.String("model", "mock-llm", "模型名")
		profile         = fs.String("profile", "k8s-npu", "execution.profile；空串表示不写")
		taskName        = fs.String("name", "", "Task 名")
		deployProvider  = fs.String("deploy-provider", "genet", "deploy.provider")
		runnerName      = fs.String("runner", "aisbench-sla", "测试类 runner 名称")
		runnerVersion   = fs.String("runner-version", "0.5.0", "测试类 runner 版本")
		precheckVersion = fs.String("precheck-version", "1.0.0", "显式 precheck 的 endpoint-probe 版本")
		expect          = fs.String("expect", "succeeded", "期望终态，逗号分隔")
		expectErrCode   = fs.String("expect-error-code", "", "期望 task.error.code")
		expectRunnerErr = fs.String("expect-runner-error-code", "", "期望失败 runner 的 error.code")
		expectRetry     = fs.String("expect-retryable", "", "期望 retryable=true|false")
		timeout         = fs.Duration("timeout", 5*time.Minute, "等待终态总时长")
		poll            = fs.Duration("poll", 2*time.Second, "轮询间隔")
		sseTimeout      = fs.Duration("sse-timeout", 10*time.Second, "SSE 等待窗口")
		reportWait      = fs.Duration("report-wait", 30*time.Second, "报告异步生成的等待窗口（SLA ≤2s，默认 30s 已足够）")
		taskID          = fs.String("task-id", "", "wait/assert 的 Task ID")
		dryRunFirst     = fs.Bool("dry-run-first", false, "提交前先 dry-run")
		strict          = fs.Bool("strict", false, "警告即失败")
		allowEmptyLogs  = fs.Bool("allow-empty-logs", false, "日志为空只警告")
		jsonOut         = fs.String("json-out", "", "机器可读结果另存文件")
		verbose         = fs.Bool("verbose", false, "打印每个 HTTP 请求")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, usageText)
			return Options{}, "", errHelp
		}
		return Options{}, command, &UsageError{Msg: err.Error()}
	}
	if rest := fs.Args(); len(rest) > 0 {
		return Options{}, command, &UsageError{Msg: fmt.Sprintf("无法识别的参数：%v（命令要写在参数前面）", rest)}
	}

	opts := Options{
		BaseURL:             strings.TrimRight(*baseURL, "/"),
		APIPrefix:           *apiPrefix,
		User:                *user,
		Token:               *token,
		NoAuth:              *noAuth,
		ManifestFile:        *manifestFile,
		Mode:                *mode,
		MockURL:             *mockURL,
		Model:               *model,
		Profile:             *profile,
		TaskName:            *taskName,
		DeployProvider:      *deployProvider,
		RunnerName:          *runnerName,
		RunnerVersion:       *runnerVersion,
		PrecheckVersion:     *precheckVersion,
		ExpectErrorCode:     *expectErrCode,
		ExpectRunnerErrCode: *expectRunnerErr,
		Timeout:             *timeout,
		Poll:                *poll,
		SSETimeout:          *sseTimeout,
		ReportWait:          *reportWait,
		TaskID:              *taskID,
		DryRunFirst:         *dryRunFirst,
		Strict:              *strict,
		AllowEmptyLogs:      *allowEmptyLogs,
		JSONOut:             *jsonOut,
		Verbose:             *verbose,
		Out:                 os.Stdout,
		Err:                 os.Stderr,
	}

	// 校验
	if opts.BaseURL == "" {
		return Options{}, command, &UsageError{Msg: "--base-url 不能为空"}
	}
	if !strings.HasPrefix(opts.BaseURL, "http://") && !strings.HasPrefix(opts.BaseURL, "https://") {
		return Options{}, command, &UsageError{Msg: fmt.Sprintf("--base-url 需要 http(s):// 前缀，收到 %q", opts.BaseURL)}
	}
	if opts.APIPrefix == "auto" {
		return Options{}, command, &UsageError{Msg: "--api-prefix 不再支持 auto：契约前缀是 /api/v1（07 §1）"}
	}
	opts.APIPrefix = strings.TrimRight(opts.APIPrefix, "/")
	if opts.APIPrefix != "" && !strings.HasPrefix(opts.APIPrefix, "/") {
		opts.APIPrefix = "/" + opts.APIPrefix
	}
	if opts.Mode != "deploy" && opts.Mode != "service" {
		return Options{}, command, &UsageError{Msg: fmt.Sprintf("--mode 只能是 deploy 或 service，收到 %q", opts.Mode)}
	}
	if opts.Timeout <= 0 {
		return Options{}, command, &UsageError{Msg: "--timeout 必须大于 0"}
	}
	if opts.Poll <= 0 {
		return Options{}, command, &UsageError{Msg: "--poll 必须大于 0"}
	}
	if opts.SSETimeout <= 0 {
		return Options{}, command, &UsageError{Msg: "--sse-timeout 必须大于 0"}
	}
	if opts.ReportWait <= 0 {
		return Options{}, command, &UsageError{Msg: "--report-wait 必须大于 0"}
	}

	expects, err := parseExpect(*expect)
	if err != nil {
		return Options{}, command, err
	}
	opts.Expect = expects

	if *expectRetry != "" {
		var b bool
		switch strings.ToLower(strings.TrimSpace(*expectRetry)) {
		case "true", "1", "yes":
			b = true
		case "false", "0", "no":
			b = false
		default:
			return Options{}, command, &UsageError{Msg: fmt.Sprintf("--expect-retryable 只能是 true 或 false，收到 %q", *expectRetry)}
		}
		opts.ExpectRetryable = &b
	}

	return opts, command, nil
}

func splitCommand(argv []string) (string, []string, error) {
	if len(argv) == 0 {
		return "run", nil, nil
	}
	if !strings.HasPrefix(argv[0], "-") {
		cmd := argv[0]
		if !commands[cmd] {
			return "", nil, &UsageError{Msg: fmt.Sprintf("未知命令 %q（支持 run|submit|wait|assert|version|help）", cmd)}
		}
		return cmd, argv[1:], nil
	}
	return "run", argv, nil
}

func parseExpect(v string) ([]string, error) {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !knownExpectations[p] {
			return nil, &UsageError{Msg: fmt.Sprintf("--expect 不支持 %q（可选 succeeded|failed|service_unavailable|canceled）", p)}
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, &UsageError{Msg: "--expect 不能为空"}
	}
	return out, nil
}
