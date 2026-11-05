package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 提交链路（C-5.1 / C-5.2 / C-8.3）：
//
//	字段校验 → tester 解析 → 公共参数注入 → 一个事务里建 Run + Tasks。
//
// **不做任何提交幂等**（D-37）：每次 POST 都创建新 Run，没有幂等键、不判重。

// ---- 提交原文的领域形状（03 §4.1、ARCHITECTURE §10.1）--------------------

// Manifest 是提交原文。权威形状是 07 §4.1 的嵌套写法（`run:` 下承载全部字段）；
// 同时兼容 03-data-view §4.1 的旧平级写法（顶层 deploy/service/tests…）。
type Manifest struct {
	Run  *RunBody  `yaml:"run" json:"run"`
	Task *TaskSpec `yaml:"task" json:"task"` // 独立 Task 形态

	// 兼容层：顶层平级键，normalize() 后与 Run.* 指向同一份数据（嵌套优先）。
	Deploy   *DeploySpec       `yaml:"deploy" json:"deploy"`
	Service  *ServiceSpec      `yaml:"service" json:"service"`
	Precheck any               `yaml:"precheck" json:"precheck"`
	Env      map[string]string `yaml:"env" json:"env"`
	Secrets  map[string]string `yaml:"secrets" json:"secrets"`
	Tests    []TestSpec        `yaml:"tests" json:"tests"`
	Cleanup  *CleanupSpec      `yaml:"cleanup" json:"cleanup"`
}

// RunBody 是 `run:` 块（07 §4.1）：deploy 与 service 二选一。
type RunBody struct {
	Name     string            `yaml:"name" json:"name"`
	Deploy   *DeploySpec       `yaml:"deploy" json:"deploy"`
	Service  *ServiceSpec      `yaml:"service" json:"service"`
	Precheck any               `yaml:"precheck" json:"precheck"`
	Env      map[string]string `yaml:"env" json:"env"`
	Secrets  map[string]string `yaml:"secrets" json:"secrets"`
	Tests    []TestSpec        `yaml:"tests" json:"tests"`
	Cleanup  *CleanupSpec      `yaml:"cleanup" json:"cleanup"`
}

// normalize 把嵌套写法展开成扁平字段（下游只认扁平）：嵌套优先，顶层只补空。
func (m *Manifest) normalize() {
	if m.Run == nil {
		return
	}
	if m.Deploy == nil {
		m.Deploy = m.Run.Deploy
	}
	if m.Service == nil {
		m.Service = m.Run.Service
	}
	if m.Precheck == nil {
		m.Precheck = m.Run.Precheck
	}
	if len(m.Env) == 0 {
		m.Env = m.Run.Env
	}
	if len(m.Secrets) == 0 {
		m.Secrets = m.Run.Secrets
	}
	if len(m.Tests) == 0 {
		m.Tests = m.Run.Tests
	}
	if m.Cleanup == nil {
		m.Cleanup = m.Run.Cleanup
	}
}

// nested 报告提交体是否为嵌套写法（决定 detail.issues[].path 前缀）。
func (m *Manifest) nested() bool { return m.Run != nil }

// field 给字段路径加前缀：嵌套写法 `run.tests[0].args`，平级写法 `tests[0].args`。
func (m *Manifest) field(format string, args ...any) string {
	s := format
	if len(args) > 0 {
		s = fmt.Sprintf(format, args...)
	}
	if m.nested() {
		return "run." + s
	}
	return s
}

// DeploySpec 是 deploy: 块（有 deploy 时走 Genet 部署）。
type DeploySpec struct {
	Provider   string         `yaml:"provider" json:"provider"`
	Parameters map[string]any `yaml:"parameters" json:"parameters"`
}

// ServiceSpec 是 service: 块（不部署，直连已有服务）。
type ServiceSpec struct {
	URL       string `yaml:"url" json:"url"`
	ModelName string `yaml:"model_name" json:"model_name"`
	ModelPath string `yaml:"model_path" json:"model_path"`
}

// TestSpec 是 tests[] 里的一项。
type TestSpec struct {
	Name           string            `yaml:"name" json:"name"`
	Tester         string            `yaml:"tester" json:"tester"`
	Version        string            `yaml:"version" json:"version"`
	Execution      ExecutionSpec     `yaml:"execution" json:"execution"`
	Args           string            `yaml:"args" json:"args"`
	OnFailure      string            `yaml:"on_failure" json:"on_failure"`
	TimeoutSeconds int               `yaml:"timeout_seconds" json:"timeout_seconds"`
	Retry          *RetrySpec        `yaml:"retry" json:"retry"`
	Env            map[string]string `yaml:"env" json:"env"`
}

// TaskSpec 是独立 Task 形态的 task: 块（C-5.9）。
type TaskSpec struct {
	Name           string            `yaml:"name" json:"name"`
	Tester         string            `yaml:"tester" json:"tester"`
	Version        string            `yaml:"version" json:"version"`
	Execution      ExecutionSpec     `yaml:"execution" json:"execution"`
	Args           string            `yaml:"args" json:"args"`
	OnFailure      string            `yaml:"on_failure" json:"on_failure"`
	TimeoutSeconds int               `yaml:"timeout_seconds" json:"timeout_seconds"`
	Retry          *RetrySpec        `yaml:"retry" json:"retry"`
	Env            map[string]string `yaml:"env" json:"env"`
}

// ExecutionSpec 是执行环境选择。
type ExecutionSpec struct {
	Profile string `yaml:"profile" json:"profile"`
}

// RetrySpec 是 tasks.retry_policy 的提交形状（03 §4.5）。
type RetrySpec struct {
	MaxAttempts    int      `yaml:"max_attempts" json:"max_attempts"`
	BackoffSeconds int      `yaml:"backoff_seconds" json:"backoff_seconds"`
	RetryOn        []string `yaml:"retry_on" json:"retry_on"`
}

// CleanupSpec 是 cleanup: 块。
type CleanupSpec struct {
	Policy string `yaml:"policy" json:"policy"`
}

// PrecheckSpec 是显式 precheck 对象。
type PrecheckSpec struct {
	Tester         string        `yaml:"tester" json:"tester"`
	Version        string        `yaml:"version" json:"version"`
	Execution      ExecutionSpec `yaml:"execution" json:"execution"`
	Args           string        `yaml:"args" json:"args"`
	TimeoutSeconds int           `yaml:"timeout_seconds" json:"timeout_seconds"`
	OnFailure      string        `yaml:"on_failure" json:"on_failure"`
	Retry          *RetrySpec    `yaml:"retry" json:"retry"`
}

// SubmittedTask 是提交校验后待落库的 Task（带解析后的 tester 快照）。
type SubmittedTask struct {
	Task *store.Task
	Spec domain.TesterSpec
}

// Prepared 是校验通过后的提交计划。
type Prepared struct {
	RunID         string // r-YYYYMMDD-XXXX
	Kind          string // run | task
	Name          string
	Manifest      []byte // 原文快照
	Resolved      []byte // 冻结解析结果（03 §4.2）
	CleanupPolicy string
	Endpoint      domain.Endpoint // service 模式在提交时就有值；deploy 模式为零值
	Tasks         []SubmittedTask
}

// Submitter 做提交校验与解析。它不落库（落库由 handler 调 store.CreateRun）。
type Submitter struct {
	resolver       TesterResolver
	profiles       profileChecker
	logger         *slog.Logger
	MaxTasks       int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

// profileChecker 是 execution_profiles 的存在性检查（窄接口，便于测试）。
type profileChecker interface {
	Exists(ctx context.Context, tx pgx.Tx, name string) (bool, error)
	List(ctx context.Context, tx pgx.Tx) ([]store.Profile, error)
}

// SetLogger 注入 logger（New 里已注入，测试可覆盖）。
func (s *Submitter) SetLogger(l *slog.Logger) { s.logger = l }

// ---- 解析 -----------------------------------------------------------------

// ParseManifest 解析 YAML 或 JSON 形态的提交原文。未知字段忽略（07 §1）。
func ParseManifest(body []byte) (*Manifest, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "", Code: domain.IssueCodeRequired, Message: "请求体为空",
		}}}
	}
	var m Manifest
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: "", Code: domain.IssueCodeTypeMismatch, Message: "JSON 解析失败: " + err.Error(),
			}}}
		}
		m.normalize()
		return &m, nil
	}
	if err := yaml.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "", Code: domain.IssueCodeTypeMismatch, Message: "YAML 解析失败: " + err.Error(),
		}}}
	}
	m.normalize()
	return &m, nil
}

// precheckMode 与显式对象。
type precheckMode struct {
	mode   string // auto | off | explicit
	spec   *PrecheckSpec
	source map[string]any
}

func normalizePrecheck(raw any) (precheckMode, error) {
	switch v := raw.(type) {
	case nil:
		return precheckMode{mode: "auto"}, nil
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		switch s {
		case "", "auto":
			return precheckMode{mode: "auto"}, nil
		case "off":
			return precheckMode{mode: "off"}, nil
		}
		return precheckMode{}, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "precheck", Code: domain.IssueCodeEnum,
			Message: fmt.Sprintf("precheck 只能是 auto|off|对象，得到 %q", v),
		}}}
	case map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return precheckMode{}, err
		}
		var ps PrecheckSpec
		if err := json.Unmarshal(b, &ps); err != nil {
			return precheckMode{}, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: "precheck", Code: domain.IssueCodeTypeMismatch, Message: "precheck 对象解析失败: " + err.Error(),
			}}}
		}
		return precheckMode{mode: "explicit", spec: &ps, source: v}, nil
	default:
		return precheckMode{}, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "precheck", Code: domain.IssueCodeTypeMismatch, Message: "precheck 类型不支持",
		}}}
	}
}

// Prepare 执行 C-5.1 的校验顺序并产出提交计划。
// 返回的 error 一定是 *domain.ValidationError 或 domain.Failure 语义的错误。
func (s *Submitter) Prepare(ctx context.Context, body []byte, submittedBy string) (*Prepared, error) {
	// 1. 解析
	m, err := ParseManifest(body)
	if err != nil {
		return nil, err
	}

	// 2. 结构校验（聚合同一层的 issue）
	kind := "run"
	if m.Task != nil && m.Run == nil {
		kind = "task"
	}
	ve := s.validateStructure(m, kind)
	if !ve.Empty() {
		return nil, ve
	}

	// 3. deploy XOR service
	if !validateDeployService(m) {
		return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "", Code: domain.IssueCodeDeployService,
			Message: "deploy 与 service 必须二选一（两者都缺或都给都拒绝）",
		}}}
	}
	// 独立 Task 形态禁止 deploy，service.url 必填（C-5.9）
	if kind == "task" && m.Deploy != nil {
		return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "deploy", Code: domain.IssueCodeDeployService, Message: "独立 Task 不允许 deploy",
		}}}
	}

	// 4. Task 数上限
	testCount := len(m.Tests)
	if kind == "task" {
		testCount = 1
	}
	if testCount > s.MaxTasks {
		return nil, domain.NewFailureError(domain.CodeRunTooLarge,
			fmt.Sprintf("Task 数 %d 超过上限 %d", testCount, s.MaxTasks),
			map[string]any{"tasks": testCount, "max": s.MaxTasks})
	}

	// 5. tester 解析 + 6. profile 校验
	specs := make([]domain.TesterSpec, testCount)
	for i := 0; i < testCount; i++ {
		var name, version string
		if kind == "task" {
			name, version = m.Task.Tester, m.Task.Version
		} else {
			name, version = m.Tests[i].Tester, m.Tests[i].Version
		}
		spec, err := s.resolver.Resolve(ctx, name, version)
		if err != nil {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path:    m.field("%s", testerPath(kind, i)),
				Code:    domain.CodeTesterNotFound,
				Message: fmt.Sprintf("Tester %q (version=%q) 未注册", name, version),
			}}}
		}
		specs[i] = spec
	}

	pm, err := normalizePrecheck(m.Precheck)
	if err != nil {
		return nil, err
	}

	// 10. precheck auto：注册表恰好一个 role=pre_check
	var precheckSpec *domain.TesterSpec
	switch pm.mode {
	case "auto":
		list, err := s.resolver.List(ctx, domain.RolePreCheck, false)
		if err != nil {
			return nil, err
		}
		if len(list) != 1 {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("precheck"), Code: domain.IssueCodePrecheckCount,
				Message: fmt.Sprintf("precheck: auto 需要注册表里恰好一个 role=pre_check，实际 %d 个", len(list)),
			}}}
		}
		precheckSpec = &list[0]
	case "explicit":
		spec, err := s.resolver.Resolve(ctx, pm.spec.Tester, pm.spec.Version)
		if err != nil {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("precheck.tester"), Code: domain.CodeTesterNotFound,
				Message: fmt.Sprintf("Pre-Check Tester %q 未注册", pm.spec.Tester),
			}}}
		}
		if spec.Role != domain.RolePreCheck {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("precheck.tester"), Code: domain.IssueCodeEnum,
				Message: fmt.Sprintf("显式 precheck 必须使用 role=pre_check 的 Tester，%q 是 %q",
					spec.Name, spec.Role),
			}}}
		}
		precheckSpec = &spec
	}

	// 7. args 词法 + 公共参数冲突；8. on_failure 不弱化
	parsedArgs := make([][]string, testCount)
	for i := 0; i < testCount; i++ {
		var raw string
		var onFailure, testerOnFailure string
		if kind == "task" {
			raw, onFailure = m.Task.Args, m.Task.OnFailure
		} else {
			raw, onFailure = m.Tests[i].Args, m.Tests[i].OnFailure
		}
		argv, err := domain.SplitArgs(raw)
		if err != nil {
			var le *domain.ArgLexError
			code := domain.IssueCodeArgsLex
			if e, ok := err.(*domain.ArgLexError); ok {
				le = e
				code = e.Code
			}
			msg := "args 词法切分失败"
			if le != nil {
				msg = le.Message
			}
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("%s", argsPath(kind, i)), Code: code, Message: msg,
			}}}
		}
		if hits := domain.PublicParamConflict(argv); len(hits) > 0 {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("%s", argsPath(kind, i)), Code: domain.IssueCodePublicParam,
				Message: fmt.Sprintf("--%s 由平台注入，args 中不得出现", strings.Join(hits, ", --")),
			}}}
		}
		testerOnFailure = specs[i].OnFailure
		if onFailure == "" {
			onFailure = testerOnFailure
		} else if onFailure != domain.OnFailureAbortRun && onFailure != domain.OnFailureContinue {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("%s", onFailurePath(kind, i)), Code: domain.IssueCodeEnum,
				Message: fmt.Sprintf("on_failure 只能是 continue|abort_run，得到 %q", onFailure),
			}}}
		}
		if testerOnFailure == domain.OnFailureAbortRun && onFailure != domain.OnFailureAbortRun {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("%s", onFailurePath(kind, i)), Code: domain.IssueCodeOnFailure,
				Message: fmt.Sprintf("Tester %q 声明 abort_run，提交不得弱化为 %q", specs[i].Name, onFailure),
			}}}
		}
		if specs[i].Role == domain.RolePreCheck && onFailure != domain.OnFailureAbortRun {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: m.field("%s", onFailurePath(kind, i)), Code: domain.IssueCodeOnFailure,
				Message: "pre_check Tester 的 on_failure 必须是 abort_run",
			}}}
		}
		parsedArgs[i] = argv
	}
	_ = pm.source

	// 11. service 模式：三个公共参数的 required 检查
	var endpoint domain.Endpoint
	if m.Service != nil {
		if strings.TrimSpace(m.Service.URL) == "" {
			return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: "service.url", Code: domain.IssueCodeRequired, Message: "service.url 必填",
			}}}
		}
		endpoint = domain.EndpointFromManifest(m.Service.URL, m.Service.ModelName, m.Service.ModelPath)
		if ve := validatePublicParams(m, endpoint, specs); !ve.Empty() {
			return nil, ve
		}
	}

	// 6. execution.profile 已登记
	if ve := s.validateProfiles(ctx, m, kind); !ve.Empty() {
		return nil, ve
	}

	// 12. cleanup.policy + 超时区间
	cleanupPolicy := "always"
	if m.Cleanup != nil && m.Cleanup.Policy != "" {
		cleanupPolicy = m.Cleanup.Policy
	}
	switch cleanupPolicy {
	case "always", "on_failure", "never":
	default:
		return nil, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "cleanup.policy", Code: domain.IssueCodeCleanupPolicy,
			Message: fmt.Sprintf("cleanup.policy 只能是 always|on_failure|never，得到 %q", cleanupPolicy),
		}}}
	}

	// 9. env 来源与 secrets 引用
	if ve := validateEnv(m, specs, kind); !ve.Empty() {
		return nil, ve
	}

	// ---- 通过：构造冻结快照 ----
	return s.buildPrepared(m, kind, specs, parsedArgs, pm, precheckSpec, endpoint, cleanupPolicy, submittedBy)
}

// precheckModeOf 只取模式，不报错（结构校验阶段用）。
func precheckModeOf(m *Manifest) string {
	pm, err := normalizePrecheck(m.Precheck)
	if err != nil {
		return "auto"
	}
	return pm.mode
}

// ---- 结构校验 -------------------------------------------------------------'

func (s *Submitter) validateStructure(m *Manifest, kind string) *domain.ValidationError {
	ve := &domain.ValidationError{}
	if m.Run != nil && m.Task != nil {
		ve.Add("", domain.IssueCodeConflict, "run 与 task 不能同时出现")
	}
	if kind == "run" {
		if m.Run == nil || strings.TrimSpace(m.Run.Name) == "" {
			ve.Add(m.field("run.name"), domain.IssueCodeRequired, "name 必填")
		}
		// tests 为空只在 precheck:auto（平台注入 Pre-Check）时可接受；
		// precheck:off 且没有 tests 等于什么都不跑，按 C-5.1 拒绝。
		if len(m.Tests) == 0 && precheckModeOf(m) == "off" {
			ve.Add(m.field("tests"), domain.IssueCodeRequired,
				"tests 不能为空（precheck: off 时没有可执行的 Task）")
		}
		seen := map[string]bool{}
		for i, t := range m.Tests {
			path := m.field("tests[%d]", i)
			if strings.TrimSpace(t.Name) == "" {
				ve.Add(path+".name", domain.IssueCodeRequired, "name 必填")
			} else if seen[t.Name] {
				ve.Add(path+".name", domain.IssueCodeNameConflict, "%s", "同一 Run 内 name 重复: "+t.Name)
			}
			seen[t.Name] = true
			if strings.TrimSpace(t.Tester) == "" {
				ve.Add(path+".tester", domain.IssueCodeRequired, "tester 必填")
			}
			if strings.TrimSpace(t.Version) == "" {
				ve.Add(path+".version", domain.IssueCodeRequired, "version 必填（与 tester 一起构成注册表主键）")
			}
			if strings.TrimSpace(t.Execution.Profile) == "" {
				ve.Add(path+".execution.profile", domain.IssueCodeRequired, "execution.profile 必填")
			}
			s.validateTestCommon(ve, path, t.Args, t.OnFailure, t.TimeoutSeconds, t.Retry)
		}
		if len(m.Tests) > 0 && m.Tests[0].Name == "precheck" {
			ve.Add(m.field("tests[0].name"), domain.IssueCodeNameConflict, "name=precheck 由平台注入保留")
		}
	} else {
		if strings.TrimSpace(m.Task.Name) == "" {
			ve.Add("task.name", domain.IssueCodeRequired, "task.name 必填")
		}
		if strings.TrimSpace(m.Task.Tester) == "" {
			ve.Add("task.tester", domain.IssueCodeRequired, "task.tester 必填")
		}
		s.validateTestCommon(ve, "task", m.Task.Args, m.Task.OnFailure, m.Task.TimeoutSeconds, m.Task.Retry)
	}
	if m.Deploy != nil {
		if strings.TrimSpace(m.Deploy.Provider) == "" {
			ve.Add(m.field("deploy.provider"), domain.IssueCodeRequired, "deploy.provider 必填")
		}
	}
	if m.Service != nil {
		if strings.TrimSpace(m.Service.URL) == "" {
			ve.Add(m.field("service.url"), domain.IssueCodeRequired, "service.url 必填")
		}
	}
	return ve
}

func (s *Submitter) validateTestCommon(ve *domain.ValidationError, path, args, onFailure string,
	timeout int, retry *RetrySpec) {
	if onFailure != "" && onFailure != domain.OnFailureAbortRun && onFailure != domain.OnFailureContinue {
		ve.Add(path+".on_failure", domain.IssueCodeEnum,
			"%s", fmt.Sprintf("on_failure 只能是 continue|abort_run，得到 %q", onFailure))
	}
	if timeout < 0 {
		ve.Add(path+".timeout_seconds", domain.IssueCodeTimeoutRange, "timeout_seconds 不能为负")
	}
	if timeout > int(s.MaxTimeout.Seconds()) {
		ve.Add(path+".timeout_seconds", domain.IssueCodeTimeoutRange,
			"%s", fmt.Sprintf("timeout_seconds 超过上限 %s", s.MaxTimeout))
	}
	if retry != nil {
		if retry.MaxAttempts < 0 {
			ve.Add(path+".retry.max_attempts", domain.IssueCodeTimeoutRange, "max_attempts 不能为负")
		}
		if retry.BackoffSeconds < 0 {
			ve.Add(path+".retry.backoff_seconds", domain.IssueCodeTimeoutRange, "backoff_seconds 不能为负")
		}
		for _, code := range retry.RetryOn {
			if !domain.Retryable(code) {
				ve.Add(path+".retry.retry_on", domain.IssueCodeEnum,
					"%s", fmt.Sprintf("retry_on 只能取 retryable:true 的错误码，%q 不合法", code))
			}
		}
	}
}

func validateDeployService(m *Manifest) bool {
	return (m.Deploy == nil) != (m.Service == nil)
}

func validatePublicParams(m *Manifest, ep domain.Endpoint, specs []domain.TesterSpec) *domain.ValidationError {
	ve := &domain.ValidationError{}
	values := map[string]string{
		domain.ParamURL:       ep.BaseURL,
		domain.ParamModelName: ep.ModelName,
		domain.ParamModelPath: ep.ModelPath,
	}
	for i, spec := range specs {
		for name, p := range spec.Parameters {
			if !p.Required {
				continue
			}
			if _, isPublic := values[name]; !isPublic {
				continue // 非公共参数一律不校验（§5.4）
			}
			if strings.TrimSpace(values[name]) == "" {
				ve.Add(m.field("tests[%d]", i), domain.IssueCodePublicRequired,
					"%s", fmt.Sprintf("Tester %q 声明 %s 为 required，但 service 未提供", spec.Name, name))
			}
		}
	}
	return ve
}

func validateEnv(m *Manifest, specs []domain.TesterSpec, kind string) *domain.ValidationError {
	ve := &domain.ValidationError{}
	var taskEnv map[string]string
	if kind == "task" {
		taskEnv = m.Task.Env
	}
	for i, spec := range specs {
		env := taskEnv
		if kind == "run" {
			env = m.Tests[i].Env
		}
		for name, decl := range spec.Env {
			path := m.field("tests[%d].env.%s", i, name)
			if kind == "task" {
				path = "task.env." + name
			}
			// secret 型：来源只能是 run.secrets 的引用；required 且没给 → SECRET_REQUIRED。
			if decl.Secret {
				if _, ok := m.Secrets[name]; ok {
					continue
				}
				if _, ok := m.Env[name]; ok {
					continue
				}
				if _, ok := env[name]; ok {
					continue
				}
				if decl.Required {
					ve.Add(path, domain.IssueCodeSecretRequired,
						"%s", fmt.Sprintf("Tester %q 声明 secret %s 为必需，但 secrets 里没有引用", spec.Name, name))
				}
				continue
			}
			if !decl.Required {
				continue
			}
			if _, ok := m.Env[name]; ok {
				continue
			}
			if _, ok := env[name]; ok {
				continue
			}
			if _, ok := m.Secrets[name]; ok {
				continue
			}
			ve.Add(path, domain.IssueCodeEnvMissing,
				"%s", fmt.Sprintf("Tester %q 声明 %s 为必需环境变量，但 env/secrets 都没有来源", spec.Name, name))
		}
	}
	return ve
}

// validateProfiles 校验 execution.profile 已登记。
//
// MVP 引导期例外：execution_profiles 表为空（尚未 seed）时放行并告警，
// 否则全新部署无法提交任何 Run。表非空后恢复严格校验。
func (s *Submitter) validateProfiles(ctx context.Context, m *Manifest, kind string) *domain.ValidationError {
	names := map[string]string{} // profile -> path
	if kind == "task" {
		names[profileOrDefault(m.Task.Execution.Profile)] = "task.execution.profile"
	} else {
		for i, t := range m.Tests {
			names[profileOrDefault(t.Execution.Profile)] = m.field("tests[%d].execution.profile", i)
		}
	}
	for name, path := range names {
		ok, err := s.profileExists(ctx, name)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("profile 校验失败，跳过", "err", err, "profile", name)
			}
			continue
		}
		if !ok {
			return &domain.ValidationError{Issues: []domain.ValidationIssue{{
				Path: path, Code: domain.CodeExecutionProfileMissing,
				Message: fmt.Sprintf("Execution Profile %q 未登记", name),
			}}}
		}
	}
	return &domain.ValidationError{}
}

func profileOrDefault(p string) string {
	if strings.TrimSpace(p) == "" {
		return "default"
	}
	return p
}

// profileExists 检查 Profile 是否登记；引导期（表为空）放行。
func (s *Submitter) profileExists(ctx context.Context, name string) (bool, error) {
	if s.profiles == nil {
		return true, nil
	}
	list, err := s.profiles.List(ctx, nil)
	if err != nil {
		return true, err
	}
	if len(list) == 0 {
		return true, nil
	}
	return s.profiles.Exists(ctx, nil, name)
}

// ---- 路径助手（detail.issues[].path 用 JSON Pointer 风格）------------------

func testerPath(kind string, i int) string {
	if kind == "task" {
		return "task.tester"
	}
	return fmt.Sprintf("tests[%d].tester", i)
}

func argsPath(kind string, i int) string {
	if kind == "task" {
		return "task.args"
	}
	return fmt.Sprintf("tests[%d].args", i)
}

func onFailurePath(kind string, i int) string {
	if kind == "task" {
		return "task.on_failure"
	}
	return fmt.Sprintf("tests[%d].on_failure", i)
}

// argsSnapshotSHA 计算全部 argv 的快照指纹（resolved.args_snapshot_sha）。
func argsSnapshotSHA(argv [][]string) string {
	h := sha256.New()
	for _, a := range argv {
		for _, tok := range a {
			h.Write([]byte(tok))
			h.Write([]byte{0})
		}
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// defaultRetry 返回 tasks.retry_policy 的落库 JSON。
func retryJSON(r *RetrySpec) []byte {
	if r == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return []byte("{}")
	}
	return b
}

