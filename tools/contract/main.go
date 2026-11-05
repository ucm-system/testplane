package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// check 是一项契约检查结果。
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// resultJSON 是 stdout 上那一行机器可读结果。
type resultJSON struct {
	OK         bool     `json:"ok"`
	Mode       string   `json:"mode"` // spec | live | all
	SpecPath   string   `json:"spec_path"`
	SpecOps    int      `json:"spec_operations"`
	DurationMS int64    `json:"duration_ms"`
	Checks     []check  `json:"checks"`
	Warnings   []string `json:"warnings,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// Options 是契约校验器的命令行参数。
type Options struct {
	SpecPath     string
	DesignPath   string
	AllowMissing string
	BaseURL      string
	User         string
	Token        string
	NoAuth       bool
	Mode         string // spec | live | all
	Manifest     string
	MockURL      string
	Profile      string
	RunName      string
	Timeout      time.Duration
	JSONOut      string
	Verbose      bool
}

func main() {
	code := runCLI(os.Args[1:])
	os.Exit(code)
}

func runCLI(argv []string) int {
	opts, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract: %v\n\n提示：contract --help 查看用法\n", err)
		return 2
	}
	if opts.Mode == "help" {
		fmt.Print(usageText)
		return 0
	}

	start := time.Now()
	spec, err := LoadSpec(opts.SpecPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract: %v\n", err)
		return 2
	}

	res := resultJSON{Mode: opts.Mode, SpecPath: spec.Path, SpecOps: len(spec.Operations)}
	var warnings []string

	if opts.Mode == "spec" || opts.Mode == "all" {
		checks, warns := spec.StaticChecks()
		res.Checks = append(res.Checks, checks...)
		warnings = append(warnings, warns...)

		// 端点集合：以 07-api.md §5（+ §2.4 认证表）为期望，与 openapi 做双向差集。
		design, derr := LoadDesign(opts.DesignPath)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "contract: %v\n", derr)
			return 2
		}
		allow, aerr := LoadAllowMissing(opts.AllowMissing)
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "contract: %v\n", aerr)
			return 2
		}
		dc, dw := spec.DesignChecks(design, allow)
		res.Checks = append(res.Checks, dc...)
		warnings = append(warnings, dw...)
	}

	var runErr error
	if opts.Mode == "live" || opts.Mode == "all" {
		live := NewLive(spec, opts)
		liveChecks, liveWarns, err := live.Run()
		res.Checks = append(res.Checks, liveChecks...)
		warnings = append(warnings, liveWarns...)
		runErr = err
	}

	res.Warnings = warnings
	if runErr != nil {
		res.Error = runErr.Error()
	}
	res.OK = runErr == nil && allOK(res.Checks)
	if !res.OK && res.Error == "" {
		res.Error = fmt.Sprintf("%d 项契约检查失败", countFailed(res.Checks))
	}
	res.DurationMS = time.Since(start).Milliseconds()

	printSummary(&res)
	writeJSON(&res, opts.JSONOut)

	if res.OK {
		return 0
	}
	if isConnectError(runErr) {
		return 3
	}
	return 1
}

// allOK 供 finish 使用。
func allOK(checks []check) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func countFailed(checks []check) int {
	n := 0
	for _, c := range checks {
		if !c.OK {
			n++
		}
	}
	return n
}

func printSummary(res *resultJSON) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n==== contract %s ====\n", res.Mode)
	fmt.Fprintf(&b, "spec   : %s（%d 个 operation）\n", res.SpecPath, res.SpecOps)
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
		fmt.Fprintln(&b, "结果   : PASS")
	} else {
		fmt.Fprintln(&b, "结果   : FAIL")
	}
	fmt.Fprint(os.Stderr, b.String())
}

func writeJSON(res *resultJSON, jsonOut string) {
	compact, err := json.Marshal(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract: 序列化结果失败：%v\n", err)
		return
	}
	fmt.Fprintln(os.Stdout, string(compact))
	if jsonOut != "" {
		pretty, err := json.MarshalIndent(res, "", "  ")
		if err == nil {
			if err := os.WriteFile(jsonOut, append(pretty, '\n'), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "contract: 写 --json-out 失败：%v\n", err)
			}
		}
	}
}
