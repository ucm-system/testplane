package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// check 是一项断言结果；机器可读输出逐项列出，便于 CI 直接判红。
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type eventsSummary struct {
	Source   string   `json:"source"`
	Count    int      `json:"count"`
	FirstSeq int64    `json:"first_seq"`
	LastSeq  int64    `json:"last_seq"`
	Types    []string `json:"types"`
}

// resultJSON 是 stdout 上那一行机器可读结果。
type resultJSON struct {
	OK          bool           `json:"ok"`
	Command     string         `json:"command"`
	TaskID      string         `json:"task_id,omitempty"`
	Expected    []string       `json:"expected,omitempty"`
	Actual      string         `json:"actual,omitempty"`
	StatusTrace []string       `json:"status_trace,omitempty"`
	DurationMS  int64          `json:"duration_ms"`
	Events      *eventsSummary `json:"events,omitempty"`
	Checks      []check        `json:"checks"`
	Warnings    []string       `json:"warnings,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// Options 是全部命令行参数（--manifest 之外的内置模板参数也在内）。
type Options struct {
	BaseURL   string
	APIPrefix string
	User      string
	Token     string
	NoAuth    bool

	ManifestFile    string
	Mode            string
	MockURL         string
	Model           string
	Profile         string
	TaskName        string
	DeployProvider  string
	RunnerName      string
	RunnerVersion   string
	PrecheckVersion string

	Expect              []string
	ExpectErrorCode     string
	ExpectRunnerErrCode string
	ExpectRetryable     *bool

	Timeout    time.Duration
	Poll       time.Duration
	SSETimeout time.Duration
	ReportWait time.Duration

	TaskID         string
	DryRunFirst    bool
	Strict         bool
	AllowEmptyLogs bool
	JSONOut        string
	Verbose        bool

	Out io.Writer
	Err io.Writer
}

// App 持有一次运行的客户端、断言与轨迹。
type App struct {
	opts   Options
	client *Client

	checks   []check
	warnings []string

	task         *Task
	statusTrace  []string
	events       []Event
	eventsSource string
	connectErr   error
}

func newApp(opts Options) *App {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Err == nil {
		opts.Err = os.Stderr
	}
	a := &App{opts: opts}
	a.client = NewClient(ClientConfig{
		BaseURL: opts.BaseURL,
		Prefix:  opts.APIPrefix,
		User:    opts.User,
		Token:   opts.Token,
		NoAuth:  opts.NoAuth,
		Verbose: opts.Verbose,
		Logf: func(format string, args ...any) {
			if opts.Verbose {
				a.logf("  [http] "+format, args...)
			}
		},
	})
	return a
}

func (a *App) logf(format string, args ...any) {
	if a.opts.Err != nil {
		fmt.Fprintf(a.opts.Err, format+"\n", args...)
	}
}

func (a *App) addCheck(name string, ok bool, detail string) {
	a.checks = append(a.checks, check{Name: name, OK: ok, Detail: detail})
}

func (a *App) warn(format string, args ...any) {
	a.warnings = append(a.warnings, fmt.Sprintf(format, args...))
}

func (a *App) allChecksOK() bool {
	for _, c := range a.checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// finish 打印人类摘要 + 输出一行 JSON，返回进程退出码：
// 0 全通过；1 断言失败；2 参数/配置错误；3 连不上控制面。
func (a *App) finish(command string, start time.Time, runErr error) int {
	checks := a.checks
	if checks == nil {
		checks = []check{}
	}
	res := resultJSON{
		Command:     command,
		TaskID:      a.opts.TaskID,
		Expected:    a.opts.Expect,
		StatusTrace: a.statusTrace,
		DurationMS:  time.Since(start).Milliseconds(),
		Checks:      checks,
		Warnings:    a.warnings,
	}
	if a.task != nil {
		res.TaskID = a.task.ID
		res.Actual = a.task.Status
	}
	if len(a.events) > 0 {
		res.Events = summarizeEvents(a.events, a.eventsSource)
	}

	strictFail := a.opts.Strict && len(a.warnings) > 0
	if runErr != nil {
		res.Error = runErr.Error()
	} else if strictFail {
		res.Error = fmt.Sprintf("--strict：存在 %d 条警告", len(a.warnings))
	}
	res.OK = runErr == nil && a.allChecksOK() && !strictFail

	code := 0
	if !res.OK {
		code = 1
		var ce *ConnectError
		var ue *UsageError
		switch {
		case errors.As(runErr, &ce):
			code = 3
		case errors.As(runErr, &ue):
			code = 2
		}
	}

	a.printSummary(&res)
	a.writeJSON(&res)
	return code
}

func (a *App) printSummary(res *resultJSON) {
	if a.opts.Err == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n==== e2e %s ====\n", res.Command)
	if res.TaskID != "" {
		fmt.Fprintf(&b, "task_id: %s\n", res.TaskID)
	}
	if len(res.Expected) > 0 {
		fmt.Fprintf(&b, "期望   : %s\n", strings.Join(res.Expected, ", "))
	}
	if res.Actual != "" {
		fmt.Fprintf(&b, "实际   : %s\n", res.Actual)
	}
	fmt.Fprintf(&b, "耗时   : %.1fs\n", float64(res.DurationMS)/1000)
	if len(res.StatusTrace) > 0 {
		fmt.Fprintf(&b, "状态轨迹：\n  %s\n", strings.Join(res.StatusTrace, "\n  "))
	}
	if res.Events != nil {
		fmt.Fprintf(&b, "事件   : %d 条（%s，seq %d..%d）\n",
			res.Events.Count, res.Events.Source, res.Events.FirstSeq, res.Events.LastSeq)
	}

	passed := 0
	for _, c := range res.Checks {
		if c.OK {
			passed++
		}
	}
	fmt.Fprintf(&b, "检查   : %d/%d 通过\n", passed, len(res.Checks))
	for _, c := range res.Checks {
		if c.OK {
			continue
		}
		detail := c.Detail
		if detail == "" {
			detail = "(无详情)"
		}
		fmt.Fprintf(&b, "  [FAIL] %s —— %s\n", c.Name, detail)
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintf(&b, "警告   : %d 条\n", len(res.Warnings))
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, "  [warn] %s\n", w)
		}
	}
	if res.Error != "" {
		fmt.Fprintf(&b, "错误   : %s\n", res.Error)
	}
	if res.OK {
		fmt.Fprintln(&b, "结果   : PASS（退出码 0）")
	} else {
		fmt.Fprintln(&b, "结果   : FAIL")
	}
	fmt.Fprint(a.opts.Err, b.String())
}

func (a *App) writeJSON(res *resultJSON) {
	compact, err := json.Marshal(res)
	if err != nil {
		a.logf("e2e: 序列化结果失败：%v", err)
		return
	}
	if a.opts.Out != nil {
		fmt.Fprintln(a.opts.Out, string(compact))
	}
	if a.opts.JSONOut != "" {
		pretty, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			a.logf("e2e: 序列化 --json-out 失败：%v", err)
			return
		}
		if err := os.WriteFile(a.opts.JSONOut, append(pretty, '\n'), 0o644); err != nil {
			a.logf("e2e: 写 --json-out 失败：%v", err)
		}
	}
}

func summarizeEvents(events []Event, source string) *eventsSummary {
	types := make([]string, 0, 8)
	seen := map[string]bool{}
	var first, last int64
	for i, ev := range events {
		if i == 0 || (ev.Seq > 0 && (first == 0 || ev.Seq < first)) {
			if ev.Seq > 0 {
				first = ev.Seq
			}
		}
		if ev.Seq > last {
			last = ev.Seq
		}
		if !seen[ev.Type] {
			seen[ev.Type] = true
			types = append(types, ev.Type)
		}
	}
	sort.Strings(types)
	return &eventsSummary{Source: source, Count: len(events), FirstSeq: first, LastSeq: last, Types: types}
}
