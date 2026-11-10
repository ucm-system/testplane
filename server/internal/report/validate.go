// Package report 负责任务结果校验与 Task 报告生成（C-7）。
//
// 三条对外契约：
//   - ValidateRunnerOutput：runner 执行记录结束、落终态之前校验产物目录
//     （C-7.1 / D-74：三选一存在性 + result.json schema 2.0）；
//   - ParseExportsEnv：exports.env 词法解析（§5.12；失败供 orchestrator 判 EXPORT_INVALID）；
//   - WriteTaskReport：Task 终态后异步生成 report.json / report.md / metadata.json /
//     events.jsonl，并重建产物索引（C-7.2）。
//
// 报告一律取库中终态派生，不依赖"文件先于状态存在"；生成失败只重试并告警，
// 不回滚终态。
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

// 产物三选一的文件名（D-74）。 Runner 决定产出哪个；平台不强求 result.json。
var resultArtifactNames = []string{"result.json", "result.html", "result.csv"}

// Validator 校验一个 runner 执行记录的产物目录（C-7.1）。
type Validator struct {
	// MaxBytes 是 result.json 的技术上限（AG_RESULT_MAX_BYTES，默认 100 MiB），
	// 仅防 OOM，不是业务限制（K-10；html/csv 不受此限）。
	MaxBytes int64
}

// Validate 等价于"退出码按 0 处理 + test 角色"的校验（兼容旧入口名）。
func (v *Validator) Validate(ctx context.Context, taskID string, attemptNo, seq int, outputDir string) *domain.Failure {
	return v.ValidateRunnerOutput(ctx, domain.RunnerKey{TaskID: taskID, AttemptNo: attemptNo, Seq: seq},
		domain.RoleTest, outputDir, nil)
}

// ValidateRunnerOutput 是 C-7.1 / D-74 的两段式校验入口。
//
// 第①段（三选一存在性，RESULT_MISSING）：role ∈ test|pre_check 时，产物目录里
// result.json / result.html / result.csv **至少其一存在且非 0 字节**；全缺 = RESULT_MISSING
// （D-74 重定义；deploy/clean 角色无产物契约，直接通过第①段）。
//
// 第②段（result.json schema 2.0，RESULT_INVALID）：**仅当 result.json 已产出时**——
// JSON 非法 / 非 UTF-8 / 缺 task_id / attempt_no / runner_seq / runner{} / metrics /
// schema_version != "2.0" / 超技术上限 / metric 唯一性与 statistic 枚举违反 → RESULT_INVALID。
// result.html / result.csv 平台不解析内容，只进产物索引。
//
// 退出码一致性（§5.8）：exit 0 但 result.status=failed → RESULT_INVALID（detail 记两侧）；
// 非零退出时仍读 result.error.code 归类（附录 D 枚举优先，否则回退 RUNNER_NONZERO_EXIT）。
func (v *Validator) ValidateRunnerOutput(ctx context.Context, k domain.RunnerKey,
	role, outputDir string, exitCode *int32) *domain.Failure {

	// ---- 第①段：三选一存在性 ----
	var resultInfo os.FileInfo
	var resultExists bool
	if !isNotDir(outputDir) {
		info, err := os.Stat(filepath.Join(outputDir, "result.json"))
		if err == nil && !info.IsDir() && info.Size() > 0 { // 0 字节视为缺失（C-7.1 第 1 条）
			resultExists = true
			resultInfo = info
		}
	}
	switch role {
	case domain.RoleTest, domain.RolePreCheck:
		hasAny := resultExists
		if !hasAny {
			for _, name := range resultArtifactNames[1:] {
				if info, err := os.Stat(filepath.Join(outputDir, name)); err == nil && !info.IsDir() && info.Size() > 0 {
					hasAny = true
					break
				}
			}
		}
		if !hasAny {
			return &domain.Failure{
				Code:    domain.CodeResultMissing,
				Message: "退出码 0 但三选一产物全部缺失（result.json / result.html / result.csv，D-74）",
				Detail: map[string]any{
					"output_dir": outputDir, "task_id": k.TaskID,
					"attempt_no": k.AttemptNo, "seq": k.Seq,
				},
				Retryable: false,
			}
		}
	default:
		// deploy / clean：无产物契约（§5.3），只走 result.json 存在时的结构校验。
	}

	// ---- 第②段：result.json 存在时的 schema 2.0 校验 ----
	if !resultExists {
		// 只产出了 html/csv：平台不校验内容；非零退出走失败归类兜底。
		return v.classifyNonzero(k, exitCode, nil)
	}

	if v.MaxBytes > 0 && resultInfo.Size() > v.MaxBytes {
		return &domain.Failure{
			Code:    domain.CodeResultInvalid,
			Message: fmt.Sprintf("result.json 超过技术上限 %d 字节", v.MaxBytes),
			Detail: map[string]any{
				"too_large": true, "size_bytes": resultInfo.Size(), "max_bytes": v.MaxBytes,
				"task_id": k.TaskID, "attempt_no": k.AttemptNo, "seq": k.Seq,
			},
			Retryable: false,
		}
	}

	data, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	if err != nil {
		return v.invalidFailure(k, "读取 result.json 失败: "+err.Error())
	}
	if !utf8.Valid(data) {
		return v.invalidFailure(k, "result.json 不是合法 UTF-8")
	}
	var res Result
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&res); err != nil {
		return v.invalidFailure(k, "result.json 不是合法 JSON: "+err.Error())
	}

	issues := res.validateShape()
	if len(issues) > 0 {
		return v.invalidIssues(k, issues)
	}

	// 与执行记录的一致性（C-7.1 第 4 条）：task_id / attempt_no / runner_seq 与本次执行一致。
	if res.TaskID != k.TaskID || res.AttemptNo != k.AttemptNo || res.RunnerSeq != k.Seq {
		return v.invalidIssues(k, []string{fmt.Sprintf(
			"result 标识与执行记录不一致（result.task_id=%s attempt_no=%d runner_seq=%d，期望 %s/%d/%d）",
			res.TaskID, res.AttemptNo, res.RunnerSeq, k.TaskID, k.AttemptNo, k.Seq)})
	}

	// 退出码与 status 的一致性（§5.8）。
	if exitCode != nil && *exitCode == 0 && res.Status == ResultStatusFailed {
		return &domain.Failure{
			Code:    domain.CodeResultInvalid,
			Message: "退出码为 0 但 result.status=failed，契约不一致",
			Detail: map[string]any{
				"exit_code": 0, "result_status": res.Status,
				"task_id": k.TaskID, "attempt_no": k.AttemptNo, "seq": k.Seq,
			},
			Retryable: false,
		}
	}

	return v.classifyNonzero(k, exitCode, &res)
}

// classifyNonzero 非零退出的失败归类（C-7.1 第 6 条）：result.json 的 error.code 属
// 附录 D → 用该码 + 实例 retryable；否则回退 RUNNER_NONZERO_EXIT。
func (v *Validator) classifyNonzero(k domain.RunnerKey, exitCode *int32, res *Result) *domain.Failure {
	if exitCode == nil || *exitCode == 0 {
		return nil
	}
	if res != nil && res.Error != nil && res.Error.Code != "" && domain.KnownCode(res.Error.Code) {
		f := *res.Error
		f.Detail = mergeDetail(f.Detail, map[string]any{
			"exit_code": *exitCode, "task_id": k.TaskID, "attempt_no": k.AttemptNo, "seq": k.Seq,
		})
		return &f
	}
	return &domain.Failure{
		Code:    domain.CodeRunnerNonzeroExit,
		Message: fmt.Sprintf("runner 以退出码 %d 结束", *exitCode),
		Detail: map[string]any{
			"exit_code": *exitCode, "task_id": k.TaskID, "attempt_no": k.AttemptNo, "seq": k.Seq,
		},
		Retryable: false,
	}
}

// ValidateResult 是设计文档点名的函数入口（C-7.1：report.ValidateResult）。
func ValidateResult(ctx context.Context, k domain.RunnerKey, role, outputDir string,
	maxBytes int64, exitCode *int32) *domain.Failure {
	v := &Validator{MaxBytes: maxBytes}
	return v.ValidateRunnerOutput(ctx, k, role, outputDir, exitCode)
}

func (v *Validator) invalidFailure(k domain.RunnerKey, msg string) *domain.Failure {
	return &domain.Failure{
		Code:    domain.CodeResultInvalid,
		Message: msg,
		Detail: map[string]any{
			"task_id": k.TaskID, "attempt_no": k.AttemptNo, "seq": k.Seq,
		},
		Retryable: false,
	}
}

func (v *Validator) invalidIssues(k domain.RunnerKey, issues []string) *domain.Failure {
	items := make([]any, 0, len(issues))
	for _, is := range issues {
		items = append(items, is)
	}
	return &domain.Failure{
		Code:    domain.CodeResultInvalid,
		Message: "result.json 不符合附录 A（schema 2.0）",
		Detail: map[string]any{
			"issues": items, "task_id": k.TaskID, "attempt_no": k.AttemptNo, "seq": k.Seq,
		},
		Retryable: false,
	}
}

func isNotDir(dir string) bool {
	st, err := os.Stat(dir)
	return err != nil || !st.IsDir()
}

// ---- result.json 的形状（附录 A schema 2.0）--------------------------------

// Result 是 result.json 的解析结果。未知字段忽略（前向兼容，K-10）。
type Result struct {
	SchemaVersion string          `json:"schema_version"`
	TaskID        string          `json:"task_id"`
	AttemptNo     int             `json:"attempt_no"`
	RunnerSeq     int             `json:"runner_seq"`
	Runner        RunnerRef       `json:"runner"`
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

// RunnerRef 是 result.json 里的 Runner 自述。
type RunnerRef struct {
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

// 结果状态（Runner 视角）。
const (
	ResultStatusSucceeded = "succeeded"
	ResultStatusFailed    = "failed"
)

// SchemaVersion2 是 result.json 的当前 schema 版本（附录 A 2.0）。
const SchemaVersion2 = "2.0"

// validStatistics 是 metrics[].statistic 的枚举（附录 A，9 值）。
var validStatistics = map[string]bool{
	"mean": true, "p50": true, "p90": true, "p95": true, "p99": true,
	"min": true, "max": true, "sum": true, "count": true,
}

// validateShape 校验必填字段与唯一性，返回字段级 issue 列表（C-7.1 第 3/5 条）。
func (r *Result) validateShape() []string {
	var issues []string
	add := func(format string, args ...any) { issues = append(issues, fmt.Sprintf(format, args...)) }

	if r.SchemaVersion != SchemaVersion2 {
		add("schema_version 必须是 %q，得到 %q", SchemaVersion2, r.SchemaVersion)
	}
	if r.TaskID == "" {
		add("缺少 task_id")
	}
	if r.AttemptNo < 1 {
		add("缺少 attempt_no（独立运行时填 1，§5.10）")
	}
	if r.RunnerSeq < 1 {
		add("缺少 runner_seq（独立运行时填 1，§5.10）")
	}
	if r.Runner.Name == "" {
		add("缺少 runner.name")
	}
	if r.Runner.Version == "" {
		add("缺少 runner.version")
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
		add("status=failed 时 error 必填（软要求用于失败归类，D-74）")
	}

	type seenKey struct{ name, statistic, labels string }
	seen := map[seenKey]bool{}
	for i, m := range r.Metrics {
		if m.Name == "" {
			add("metrics[%d].name 必填", i)
		} else if strings.Contains(m.Name, " ") || m.Name != strings.ToLower(m.Name) && hasUpper(m.Name) {
			// §5.9：name 用 snake_case（软校验：仅提示大写）。
			add("metrics[%d].name=%q 建议 snake_case（§5.9）", i, m.Name)
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

func hasUpper(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
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
