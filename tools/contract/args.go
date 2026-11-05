package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

const usageText = `contract 是 TestPlane 的契约校验器：以 server/api/openapi.yaml 为唯一契约，
做"规格侧"与"实现侧"两层校验。

用法：
  contract [参数]
  contract --mode spec        # 只跑规格侧（不需要控制面，离线可用）
  contract --mode live        # 只跑实现侧（需要控制面已启动）
  contract --mode all         # 默认：规格侧 + 实现侧

规格侧（mode=spec）：
  校验 25 个 operation 与 MVP 定稿集合一致、operationId 唯一、成功响应已定义、
  所有 JSON 错误响应引用 Error、Error 必填字段（code/message/retryable/trace_id）、
  鉴权覆盖、path 参数声明、$ref 可解析、ErrorCode 枚举。

实现侧（mode=live）：
  对运行中的控制面逐个打 25 个 operation：
    - 核心只读端点要求精确 2xx，并校验 spec 中响应 schema 的 required 字段；
    - 状态相关端点（report/report.md/deploy/attempt result/logs）允许 spec 声明的 404，
      但错误体仍必须是 Error；
    - 单独验证 404 与 400 的错误体形状；
    - 校验 SSE 的 Content-Type 与首行；
    - 用 GET /testers 证明 endpoint-probe 已被扫描入库（K-3，无运行时注册）；
    - 会创建 2 个 fixture Run（A 用于读、B 用于取消），结束时尽力取消 A。

参数：
  --spec          openapi.yaml 路径（默认从 cwd 向上找 server/api/openapi.yaml）
  --mode          spec | live | all（默认 all）
  --base-url      控制面地址（默认 http://127.0.0.1:8080）
  --user          dev 模式身份（默认 dev；Bearer <subject>）
  --token         显式 Bearer（优先于 --user）
  --no-auth       不发 Authorization
  --manifest      实现侧 fixture Run 的 Manifest（默认内置 service 模板）
  --mock-url      内置 Manifest 指向的 mock-llm（默认 http://127.0.0.1:18080/v1）
  --profile       内置 Manifest 的 execution.profile（默认 k8s-npu）
  --name          内置 Manifest 的 Run 名（默认 contract-<时间戳>）
  --timeout       实现侧总超时（默认 2m）
  --json-out      机器可读结果另存文件
  --verbose       打印每个 HTTP 请求

输出：
  stderr  人类可读摘要（失败项逐条给 detail）
  stdout  一行 JSON：{"ok":...,"mode":...,"spec_operations":25,"checks":[...],"warnings":[...]}

退出码：
  0  全部契约检查通过
  1  契约违约（spec 或实现）
  2  参数/规格文件错误
  3  连不上控制面

示例：
  contract --mode spec
  contract --mode live
  contract --mode all --json-out .data/contract.json
  contract --mode live --manifest run.yaml --mock-url http://mock-llm.testplane.svc.cluster.local:18080/v1
`

func parseArgs(argv []string) (Options, error) {
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		specPath = fs.String("spec", "", "openapi.yaml 路径")
		baseURL  = fs.String("base-url", "http://127.0.0.1:8080", "控制面地址")
		user     = fs.String("user", "dev", "dev 模式身份")
		token    = fs.String("token", "", "显式 Bearer")
		noAuth   = fs.Bool("no-auth", false, "不发 Authorization")
		mode     = fs.String("mode", "all", "spec | live | all")
		manifest = fs.String("manifest", "", "fixture Run 的 Manifest 文件")
		mockURL  = fs.String("mock-url", "http://127.0.0.1:18080/v1", "mock-llm base URL")
		profile  = fs.String("profile", "k8s-npu", "execution.profile")
		runName  = fs.String("name", "", "fixture Run 名")
		timeout  = fs.Duration("timeout", 2*time.Minute, "实现侧总超时")
		jsonOut  = fs.String("json-out", "", "机器可读结果另存文件")
		verbose  = fs.Bool("verbose", false, "打印每个 HTTP 请求")
	)

	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Options{Mode: "help"}, nil
		}
		return Options{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return Options{}, fmt.Errorf("无法识别的参数：%v", rest)
	}

	opts := Options{
		SpecPath: *specPath,
		BaseURL:  strings.TrimRight(*baseURL, "/"),
		User:     *user,
		Token:    *token,
		NoAuth:   *noAuth,
		Mode:     *mode,
		Manifest: *manifest,
		MockURL:  *mockURL,
		Profile:  *profile,
		RunName:  *runName,
		Timeout:  *timeout,
		JSONOut:  *jsonOut,
		Verbose:  *verbose,
	}
	if opts.SpecPath == "" {
		opts.SpecPath = DefaultSpecPath()
	}
	switch opts.Mode {
	case "spec", "live", "all":
	default:
		return Options{}, fmt.Errorf("--mode 只能是 spec / live / all，收到 %q", opts.Mode)
	}
	if !strings.HasPrefix(opts.BaseURL, "http://") && !strings.HasPrefix(opts.BaseURL, "https://") {
		return Options{}, fmt.Errorf("--base-url 需要 http(s):// 前缀，收到 %q", opts.BaseURL)
	}
	if opts.Timeout <= 0 {
		return Options{}, fmt.Errorf("--timeout 必须大于 0")
	}
	return opts, nil
}
