// Package report 负责任务结果校验与 Run 报告生成（C-7）。
//
// 两条对外契约（ports.ResultValidator / ports.ReportWriter）：
//   - Validate：Attempt 结束、落终态之前校验产物目录里的 result.json；
//   - WriteRunReport：Run 终态后异步生成 report.json / report.md / metadata.json /
//     events.jsonl，并重建产物索引。
//
// 报告一律取库中终态派生，不依赖"文件先于状态存在"；生成失败只重试并告警，
// 不回滚终态（C-7.2）。
package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 校验相关的错误码（附录 D）。
const (
	absenceCodeMissing = domain.CodeResultMissing
	absenceCodeInvalid = domain.CodeResultInvalid
)

// Validator 实现 ports.ResultValidator：校验 <outputDir>/result.json（C-7.1）。
type Validator struct {
	// MaxBytes 是 result.json 的技术上限（TESTPLANE_RESULT_MAX_BYTES，默认 100 MiB），
	// 仅防 OOM，不是业务限制（K-10）。
	MaxBytes int64
}

// Validate 是 ports.ResultValidator 的签名：等价于"退出码按 0 处理"的校验。
func (v *Validator) Validate(ctx context.Context, runID, taskID, attemptID, outputDir string) *domain.Failure {
	return v.ValidateExit(ctx, runID, taskID, attemptID, outputDir, nil)
}

// ValidateExit 在 Validate 基础上带上 Tester 退出码（C-7.1 第 4 条）。
//
// 校验顺序：
//  1. 产物目录不存在 / result.json 缺失或 0 字节 → RESULT_MISSING；
//  2. JSON 非法、缺必需字段、schema_version 不是 1.0、超技术上限 → RESULT_INVALID；
//  3. metrics 唯一性 / statistic 枚举 / value 为 number|null；
//  4. 退出码 0 但 result.status=failed → RESULT_INVALID（契约不一致）；
//     退出码非 0 时优先用 result.error.code（附录 D 枚举），否则兜底 TESTER_NONZERO_EXIT。
func (v *Validator) ValidateExit(ctx context.Context, runID, taskID, attemptID, outputDir string,
	exitCode *int32) *domain.Failure {

	resultPath := filepath.Join(outputDir, "result.json")
	info, err := os.Stat(resultPath)
	if err != nil {
		if os.IsNotExist(err) || isNotDir(outputDir) {
			return v.missingFailure(runID, taskID, attemptID, resultPath,
				"产物目录里没有 result.json")
		}
		return v.missingFailure(runID, taskID, attemptID, resultPath, "读取 result.json 失败: "+err.Error())
	}
	if info.IsDir() {
		return v.missingFailure(runID, taskID, attemptID, resultPath, "result.json 是目录")
	}
	if info.Size() == 0 {
		return v.missingFailure(runID, taskID, attemptID, resultPath, "result.json 为 0 字节")
	}
	if v.MaxBytes > 0 && info.Size() > v.MaxBytes {
		return &domain.Failure{
			Code: domain.CodeResultInvalid,
			Message: fmt.Sprintf("result.json 超过技术上限 %d 字节", v.MaxBytes),
			Detail: map[string]any{
				"too_large": true, "size_bytes": info.Size(), "max_bytes": v.MaxBytes,
				"task_id": taskID, "attempt_id": attemptID,
			},
			Retryable: false,
		}
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		return v.missingFailure(runID, taskID, attemptID, resultPath, "读取 result.json 失败: "+err.Error())
	}
	if !utf8.Valid(data) {
		return v.invalidFailure(runID, taskID, attemptID, "result.json 不是合法 UTF-8")
	}
	var res Result
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&res); err != nil {
		return v.invalidFailure(runID, taskID, attemptID, "result.json 不是合法 JSON: "+err.Error())
	}

	issues := res.validateShape()
	if len(issues) > 0 {
		return v.invalidIssues(runID, taskID, attemptID, issues)
	}

	// 第 4 条：退出码与 status 的一致性。
	if exitCode != nil && *exitCode == 0 && res.Status == ResultStatusFailed {
		return &domain.Failure{
			Code:    domain.CodeResultInvalid,
			Message: "退出码为 0 但 result.status=failed，契约不一致",
			Detail: map[string]any{
				"exit_code": 0, "result_status": res.Status,
				"task_id": taskID, "attempt_id": attemptID,
			},
			Retryable: false,
		}
	}

	// 非零退出：用 result.error.code 归类（供 Pre-Check 区分可重试/不可重试）。
	if exitCode != nil && *exitCode != 0 {
		if res.Error != nil && res.Error.Code != "" && isKnownCode(res.Error.Code) {
			f := *res.Error
			f.Detail = mergeDetail(f.Detail, map[string]any{
				"exit_code": *exitCode, "task_id": taskID, "attempt_id": attemptID,
			})
			return &f
		}
		return &domain.Failure{
			Code:    domain.CodeTesterNonzeroExit,
			Message: fmt.Sprintf("Tester 以退出码 %d 结束", *exitCode),
			Detail: map[string]any{
				"exit_code": *exitCode, "task_id": taskID, "attempt_id": attemptID,
			},
			Retryable: false,
		}
	}

	return nil
}

// ValidateResult 是设计文档里点名的函数入口（C-7.1：report.ValidateResult）。
func ValidateResult(ctx context.Context, runID, taskID, attemptID, outputDir string, maxBytes int64) *domain.Failure {
	v := &Validator{MaxBytes: maxBytes}
	return v.ValidateExit(ctx, runID, taskID, attemptID, outputDir, nil)
}

func (v *Validator) missingFailure(runID, taskID, attemptID, path, msg string) *domain.Failure {
	return &domain.Failure{
		Code:    absenceCodeMissing,
		Message: "未产出 result.json",
		Detail: map[string]any{
			"path": path, "reason": msg,
			"run_id": runID, "task_id": taskID, "attempt_id": attemptID,
		},
		Retryable: false,
	}
}

func (v *Validator) invalidFailure(runID, taskID, attemptID, msg string) *domain.Failure {
	return &domain.Failure{
		Code:    absenceCodeInvalid,
		Message: msg,
		Detail: map[string]any{
			"run_id": runID, "task_id": taskID, "attempt_id": attemptID,
		},
		Retryable: false,
	}
}

func (v *Validator) invalidIssues(runID, taskID, attemptID string, issues []string) *domain.Failure {
	items := make([]any, 0, len(issues))
	for _, is := range issues {
		items = append(items, is)
	}
	return &domain.Failure{
		Code:    absenceCodeInvalid,
		Message: "result.json 不符合附录 A",
		Detail: map[string]any{
			"issues": items, "run_id": runID, "task_id": taskID, "attempt_id": attemptID,
		},
		Retryable: false,
	}
}

func isNotDir(dir string) bool {
	st, err := os.Stat(dir)
	return err != nil || !st.IsDir()
}

// ---- result.json 的形状（附录 A）-----------------------------------------

// Result 是 result.json 的解析结果。未知字段忽略（前向兼容，K-10）。
type Result struct {
	SchemaVersion string          `json:"schema_version"`
	RunID         string          `json:"run_id"`
	TaskID        string          `json:"task_id"`
	AttemptID     string          `json:"attempt_id"`
	Tester        TesterRef       `json:"tester"`
	Status        string          `json:"status"`
	StartedAt     string          `json:"started_at"`
	FinishedAt    string          `json:"finished_at"`
	DurationMS    *int64          `json:"duration_ms,omitempty"`
	Workload      map[string]any  `json:"workload,omitempty"`
	Metrics       []Metric        `json:"metrics"`
	Error         *domain.Failure `json:"error"`
	Artifacts     []ArtifactRef   `json:"artifacts,omitempty"`
	Notes         string          `json:"notes,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`
}

// TesterRef 是 result.json 里的 Tester 自述。
type TesterRef struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	ImageRef string `json:"image_ref,omitempty"`
}

// Metric 是 metrics[] 的一项。
type Metric struct {
	Name       string         `json:"name"`
	Value      *float64       `json:"value"`
	Unit       string         `json:"unit"`
	Statistic  string         `json:"statistic"`
	LabelMap   map[string]any `json:"labels,omitempty"`
	SampleSize *int64         `json:"sample_size,omitempty"`
}

// ArtifactRef 是 artifacts[] 的一项。
type ArtifactRef struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// 结果状态（Tester 视角）。
const (
	ResultStatusSucceeded = "succeeded"
	ResultStatusFailed    = "failed"
)

// validStatistics 是 metrics[].statistic 的枚举（附录 A）。
var validStatistics = map[string]bool{
	"mean": true, "p50": true, "p90": true, "p95": true, "p99": true,
	"min": true, "max": true, "sum": true, "count": true,
}

// validateShape 校验必填字段与唯一性，返回字段级 issue 列表。
func (r *Result) validateShape() []string {
	var issues []string
	add := func(format string, args ...any) { issues = append(issues, fmt.Sprintf(format, args...)) }

	if r.SchemaVersion != "1.0" {
		add("schema_version 必须是 \"1.0\"，得到 %q", r.SchemaVersion)
	}
	if r.RunID == "" {
		add("缺少 run_id")
	}
	if r.TaskID == "" {
		add("缺少 task_id")
	}
	if r.AttemptID == "" {
		add("缺少 attempt_id")
	}
	if r.Tester.Name == "" {
		add("缺少 tester.name")
	}
	if r.Tester.Version == "" {
		add("缺少 tester.version")
	}
	switch r.Status {
	case ResultStatusSucceeded, ResultStatusFailed:
	default:
		add("status 只能是 succeeded|failed，得到 %q", r.Status)
	}
	if r.StartedAt == "" {
		add("缺少 started_at")
	}
	if r.FinishedAt == "" {
		add("缺少 finished_at")
	}
	if r.Metrics == nil {
		add("缺少 metrics（允许空数组，但不能缺字段）")
	}
	if r.Status == ResultStatusFailed && r.Error == nil {
		add("status=failed 时 error 必填")
	}

	type seenKey struct{ name, statistic, labels string }
	seen := map[seenKey]bool{}
	for i, m := range r.Metrics {
		if m.Name == "" {
			add("metrics[%d].name 必填", i)
		}
		if !validStatistics[m.Statistic] {
			add("metrics[%d].statistic=%q 不在枚举内", i, m.Statistic)
		}
		labels := labelsKey(m.LabelMap)
		k := seenKey{name: m.Name, statistic: m.Statistic, labels: labels}
		if seen[k] {
			add("metrics[%d] 与前面重复（name+statistic+labels 必须唯一）", i)
		}
		seen[k] = true
	}
	return issues
}

// labelsKey 把 labels 规范化成稳定字符串（map 顺序无关）。
func labelsKey(labels map[string]any) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(k)
		b.WriteString("=")
		v, _ := json.Marshal(labels[k])
		b.Write(v)
	}
	b.WriteString("}")
	return b.String()
}

func mergeDetail(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// isKnownCode 判断 result.error.code 是否属于附录 D 枚举。
func isKnownCode(code string) bool { return domain.KnownCode(code) }
