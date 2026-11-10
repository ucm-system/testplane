package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

const usageText = `contract 是 Argus 的契约校验器：以 server/api/openapi.yaml 为唯一契约，
做"规格侧"与"实现侧"两层校验。

用法：
  contract [参数]
  contract --mode spec        # 只跑规格侧（不需要控制面，离线可用）
  contract --mode live        # 只跑实现侧（需要控制面已启动）
  contract --mode all         # 默认：规格侧 + 实现侧

规格侧（mode=spec）：
  校验 openapi 自身完整性，并把 openapi 与 design/views/07-api.md §5（+ §2.4 认证表）
  的端点集合做双向差集：设计有/实现无、实现有/设计无都输出报告；
  “设计有、实现无”的端点必须逐条登记在 allow-missing 白名单并写理由，否则判红。
  另校验 operationId 唯一、成功响应已定义、Error 引用、鉴权、path 参数、$ref、ErrorCode 枚举。

实现侧（mode=live）：
  对运行中的控制面逐个打 v2 的 59 个 operation：
    - 核心只读端点要求精确 2xx，并校验 spec 中响应 schema 的 required 字段；
    - 状态相关端点（report/report.md/deploy/runner result/logs）的 404 按语义区分：
      /report、/report.md 未就绪只允许出现在非终态 Task 上，终态 Task 必须 200；
      /deploy 仅在 Task 无部署（外接模式）时允许 404；404 的响应体仍必须是 Error；
    - 单独验证 404 与 400 的错误体形状；
    - 校验 SSE 的 Content-Type 与首行；
    - 用 GET /runners 证明 endpoint-probe 已被扫描入库（K-3，无运行时注册）；
    - 会创建 4 个 fixture Task（A 读 / B 取消 / deploy 形态 / F 失败后 retry+clean），
      结束时尽力取消 A。

参数：
  --spec          openapi.yaml 路径（默认从 cwd 向上找 server/api/openapi.yaml）
  --design        设计端点目录（默认从 cwd 向上找 design/views/07-api.md）
  --allow-missing “设计有、openapi 无”的白名单文件（默认 tools/contract/allow-missing.txt；
                  传 none 可关闭白名单做严格比对）
  --mode          spec | live | all（默认 all）
  --base-url      控制面地址（默认 http://127.0.0.1:8080）
  --user          dev 模式身份（默认 dev；Bearer <subject>）
  --token         显式 Bearer（优先于 --user）
  --no-auth       不发 Authorization
  --manifest      实现侧 fixture Task 的 Manifest（默认内置 service 模板）
  --mock-url      内置 Manifest 指向的 mock-llm（默认 http://127.0.0.1:18080/v1）
  --profile       内置 Manifest 的 execution.profile（默认 k8s-npu）
  --name          内置 Manifest 的 Task 名（默认 contract-<时间戳>）
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
  contract --mode live --manifest task.yaml --mock-url http://mock-llm.argus.svc.cluster.local:18080/v1
`

func parseArgs(argv []string) (Options, error) {
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		specPath      = fs.String("spec", "", "openapi.yaml 路径")
		designPath    = fs.String("design", "", "设计端点目录（07-api.md）")
		allowMissing  = fs.String("allow-missing", "", "“设计有、实现无”的白名单文件；none 关闭")
		baseURL       = fs.String("base-url", "http://127.0.0.1:8080", "控制面地址")
		user          = fs.String("user", "dev", "dev 模式身份")
		token         = fs.String("token", "", "显式 Bearer")
		noAuth        = fs.Bool("no-auth", false, "不发 Authorization")
		mode          = fs.String("mode", "all", "spec | live | all")
		manifest      = fs.String("manifest", "", "fixture Run 的 Manifest 文件")
		mockURL       = fs.String("mock-url", "http://127.0.0.1:18080/v1", "mock-llm base URL")
		profile       = fs.String("profile", "k8s-npu", "execution.profile")
		runName       = fs.String("name", "", "fixture Run 名")
		timeout       = fs.Duration("timeout", 2*time.Minute, "实现侧总超时")
		jsonOut       = fs.String("json-out", "", "机器可读结果另存文件")
		verbose       = fs.Bool("verbose", false, "打印每个 HTTP 请求")
		printBaseline = fs.Bool("print-design-baseline", false, "只打印设计端点基线（count+sha256）")
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
		SpecPath:            *specPath,
		DesignPath:          *designPath,
		AllowMissing:        *allowMissing,
		BaseURL:             strings.TrimRight(*baseURL, "/"),
		User:                *user,
		Token:               *token,
		NoAuth:              *noAuth,
		Mode:                *mode,
		Manifest:            *manifest,
		MockURL:             *mockURL,
		Profile:             *profile,
		RunName:             *runName,
		Timeout:             *timeout,
		JSONOut:             *jsonOut,
		Verbose:             *verbose,
		PrintDesignBaseline: *printBaseline,
	}
	if opts.SpecPath == "" {
		opts.SpecPath = DefaultSpecPath()
	}
	if opts.DesignPath == "" {
		opts.DesignPath = DefaultDesignPath()
	}
	if opts.DesignPath == "" {
		return Options{}, fmt.Errorf("找不到 design/views/07-api.md；用 --design 指定")
	}
	if opts.AllowMissing == "" {
		opts.AllowMissing = DefaultAllowMissingPath()
	}
	if opts.AllowMissing == "" && (opts.Mode == "spec" || opts.Mode == "all") {
		return Options{}, fmt.Errorf("找不到 tools/contract/allow-missing.txt；用 --allow-missing 指定，或传 none 关闭白名单")
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
