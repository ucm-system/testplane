// Command argus 是 Argus 控制面的 Agent 亲和 CLI（v2，子命令式）。
//
// 定位：REST 的薄 adapter——子命令与 `server/api/openapi.yaml` 的端点一一对应，
// 不新增业务语义、不缓存、不写文件、不代理凭据。规格见 design/impl/agent-integration.md。
//
//	argus submit -f task.yaml --json
//	argus wait t-20261110-0001 --timeout 3600
//	argus report t-20261110-0001 --md
//	argus clean --task-id t-20261110-0001   # 泄漏重放（§9.3）
//	argus mcp                               # stdio 上启动 MCP server
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ucm-system/testplane/server/internal/mcp"
)

// 退出码语义（详见 agent-integration.md §4.3）：Agent 只靠退出码就能分支，
// 不需要解析输出。0/1/2/3 只在 wait 上区分"Task 的结局"。
const (
	exitOK          = 0
	exitFailure     = 1 // 命令失败，或 Task 的失败类终态（failed / service_unavailable）
	exitCanceled    = 2 // wait 等到 canceled
	exitTimeout     = 3 // wait 到总超时仍未终态
	exitUnreachable = 4 // 连不上控制面（DNS / 连接拒绝 / 传输超时）
	exitUsage       = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// app 持有一次调用的输出与客户端；测试直接调 run()。
type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	baseURL     string
	token       string
	httpTimeout time.Duration
	jsonOut     bool

	api *client
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("argus", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		baseURL    string
		token      string
		timeoutStr string
		jsonOut    bool
		stdio      bool
	)
	fs.StringVar(&baseURL, "base-url", envOr("AG_BASE_URL", "http://127.0.0.1:8080"),
		"控制面地址（$AG_BASE_URL）")
	fs.StringVar(&token, "token", envOr("AG_TOKEN", "dev"),
		"Bearer token（$AG_TOKEN；dev 模式默认 dev）")
	fs.StringVar(&timeoutStr, "timeout", "30s", "单次 HTTP 超时（秒或 Go duration）")
	fs.BoolVar(&jsonOut, "json", false, "机器可读输出（也可写在子命令之后）")
	fs.BoolVar(&stdio, "stdio", false, "以 stdio 启动 MCP server（等价 argus mcp）")

	pos, err := parseLeadingArgs(fs, argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return exitOK
		}
		fmt.Fprintf(stderr, "argus: %v\n", err)
		fmt.Fprintln(stderr, "运行 `argus help` 查看用法")
		return exitUsage
	}

	httpTimeout, err := parseDurationLoose(timeoutStr)
	if err != nil {
		fmt.Fprintf(stderr, "argus: --timeout %v\n", err)
		return exitUsage
	}
	if httpTimeout <= 0 {
		fmt.Fprintln(stderr, "argus: --timeout 必须大于 0")
		return exitUsage
	}

	// argus --stdio / argus mcp：直接把 stdio 交给 MCP server。
	if stdio && len(pos) == 0 {
		return runMCP(baseURL, token, httpTimeout, stdin, stdout, stderr)
	}
	if len(pos) > 0 && pos[0] == "mcp" {
		if len(pos) > 1 {
			fmt.Fprintln(stderr, "argus mcp: 不接受参数（全局选项请写在 mcp 之前）")
			return exitUsage
		}
		return runMCP(baseURL, token, httpTimeout, stdin, stdout, stderr)
	}

	if len(pos) == 0 {
		usage(stderr)
		return exitUsage
	}

	cmd := pos[0]
	rest := pos[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage(stdout)
		return exitOK
	}

	api, err := newClient(baseURL, token, httpTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "argus: %v\n", err)
		return exitUsage
	}
	a := &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		baseURL: baseURL, token: token, httpTimeout: httpTimeout,
		jsonOut: jsonOut, api: api,
	}

	switch cmd {
	case "submit":
		return a.cmdSubmit(rest)
	case "status":
		return a.cmdStatus(rest)
	case "wait":
		return a.cmdWait(rest)
	case "report":
		return a.cmdReport(rest)
	case "logs":
		return a.cmdLogs(rest)
	case "artifacts":
		return a.cmdArtifacts(rest)
	case "files":
		return a.cmdFiles(rest)
	case "cancel":
		return a.cmdCancel(rest)
	case "runners":
		return a.cmdRunners(rest)
	case "retry":
		return a.cmdRetry(rest)
	case "clean":
		return a.cmdClean(rest)
	case "queue":
		return a.cmdQueue(rest)
	case "profiles":
		return a.cmdProfiles(rest)
	default:
		fmt.Fprintf(stderr, "argus: 未知命令 %q\n", cmd)
		fmt.Fprintln(stderr, "运行 `argus help` 查看用法")
		return exitUsage
	}
}

func runMCP(baseURL, token string, timeout time.Duration, stdin io.Reader, stdout, stderr io.Writer) int {
	api, err := mcp.NewClient(baseURL, token, timeout)
	if err != nil {
		fmt.Fprintf(stderr, "argus mcp: %v\n", err)
		return exitUsage
	}
	srv := mcp.NewServer(api)
	if err := srv.Serve(context.Background(), stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "argus mcp: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// ---- 参数与输出 -----------------------------------------------------------

// parseLeadingArgs 只解析命令**之前**的全局选项（与 docker/kubectl 相同）；
// 遇到第一个位置参数（子命令）就停下，把其余参数原样交给子命令自己的 FlagSet。
func parseLeadingArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}

// parseArgs 让子命令的 flag 与位置参数可以交错（`argus wait r-1 --timeout 10`）。
// Go 标准 flag 在第一个非 flag 处就停止解析，这里循环续解剩余部分。
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseDurationLoose 接受纯数字（秒，可带小数）或 Go duration（30s / 500ms）。
// wait 的 --timeout 3600 与全局的 --timeout 30s 因此共用同一种解析。
func parseDurationLoose(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("时长不能为空")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f < 0 {
			return 0, fmt.Errorf("时长不能为负: %s", s)
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("无法解析 %q（用秒数或 30s/500ms）", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("时长不能为负: %s", s)
	}
	return d, nil
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

func (a *app) usageError(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "argus: "+format+"\n", args...)
	fmt.Fprintln(a.stderr, "运行 `argus help` 查看用法")
	return exitUsage
}

// fail 把错误映射成退出码并输出可读原因；--json 时额外输出结构化错误对象到 stderr
// （stdout 保持干净，便于调用方解析成功结果）。
func (a *app) fail(err error) int {
	var apiErr *apiError
	var netErr *transportError

	switch {
	case errors.As(err, &apiErr):
		hint, hasHint := mcp.HintForError(apiErr.Code)
		if a.jsonOut {
			payload := map[string]any{
				"http_status": apiErr.Status,
				"code":        apiErr.Code,
				"message":     apiErr.Message,
				"retryable":   apiErr.Retryable,
			}
			if detail := rawOrNil(apiErr.Detail); detail != nil {
				payload["detail"] = detail
			}
			if apiErr.TraceID != "" {
				payload["trace_id"] = apiErr.TraceID
			}
			if apiErr.RetryAfter != "" {
				payload["retry_after"] = apiErr.RetryAfter
			}
			if hasHint {
				payload["next_action"] = hint.NextAction
				if hint.Hint != "" {
					payload["hint"] = hint.Hint
				}
			}
			writeJSONLine(a.stderr, map[string]any{"error": payload})
		} else {
			fmt.Fprintf(a.stderr, "argus: %v\n", apiErr)
			if hasHint {
				fmt.Fprintf(a.stderr, "argus: 下一步: %s\n", hint.NextAction)
			}
		}
		return exitFailure

	case errors.As(err, &netErr):
		hint, _ := mcp.HintForError("UNREACHABLE")
		if a.jsonOut {
			writeJSONLine(a.stderr, map[string]any{"error": map[string]any{
				"code":        "UNREACHABLE",
				"message":     netErr.Error(),
				"retryable":   true,
				"base_url":    a.baseURL,
				"next_action": hint.NextAction,
				"hint":        hint.Hint,
			}})
		} else {
			fmt.Fprintf(a.stderr, "argus: 连不上控制面 %s（%v）\n", a.baseURL, netErr.err)
			fmt.Fprintf(a.stderr, "argus: 下一步: %s\n", hint.NextAction)
		}
		return exitUnreachable

	default:
		fmt.Fprintf(a.stderr, "argus: %v\n", err)
		return exitFailure
	}
}

func (a *app) printJSON(v any) {
	writeJSONLine(a.stdout, v)
}

func writeJSONLine(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writePrettyJSON(w io.Writer, body []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		_, _ = w.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			_, _ = io.WriteString(w, "\n")
		}
		return
	}
	buf.WriteByte('\n')
	_, _ = w.Write(buf.Bytes())
}

func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	return raw
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `argus %s — Argus 控制面 CLI（Agent 亲和，v2 子命令式）

用法:
  argus [全局选项] <命令> [命令选项] [参数]

命令:
  submit -f <task.yaml> [--dry-run]            提交 Task（顶层 task:；deploy XOR service）
  status <task_id>                             Task 摘要（状态/终态/runner 计数/错误码/next_action）
  wait <task_id> [--follow] [--timeout 3600] [--interval 3]
                                               等待终态；--follow 用 SSE，不可用时降级轮询
  report <task_id> [--md]                      报告 JSON / Markdown 原文
  logs <task_id> [--attempt 1] [--runner-seq N] [--offset N] [--tail N]
                                               runner 执行日志；X-Log-Offset 打到 stderr
  artifacts <task_id> [--attempt 1 --runner-seq N]
                                               产物清单（Task 级 / runner 级，按 kind 分组）
  files <task_id> <path> [--out <file>] [--range 0-1023]
                                               下载产物；--out 落盘并打印 sha256
  cancel <task_id>                             取消 Task（先执行 service-clean 再落 canceled）
  retry --task-id <task_id>                    整 Task 重试（新 attempt 全量重跑含 fresh deploy）
  clean --task-id <task_id> [--attempt N]      泄漏重放（§9.3；幂等）
  runners [--role test|pre_check] [--orphaned] Runner 目录
  queue                                        队列与槽位（平台 + Volcano）
  profiles                                     Execution Profile 与挂载能力
  mcp                                          在 stdio 上启动 MCP server
  help                                         本帮助

全局选项（写在命令之前，与 docker/kubectl 相同）:
  --base-url <url>   默认 $AG_BASE_URL 或 http://127.0.0.1:8080
  --token <token>    默认 $AG_TOKEN；dev 模式默认 dev
  --timeout <dur>    单次 HTTP 超时，默认 30s（纯数字按秒）
  --json             机器可读输出（也可写在命令后）
  --stdio            等价于 mcp 子命令

退出码:
  0 成功   1 失败（命令失败，或 Task 的失败类终态）
  2 已取消  3 等待超时  4 连不上控制面  64 用法错误

输出约定: --json 时 stdout 只写一个 JSON 文档；进度/告警写 stderr。
`, cliVersion)
}
