package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 管道组装与提交校验（C-5.1 / C-5.2 / §10）。
//
// 提交校验 12 项按序执行（遇到第一个错误即返回；同一层可聚合多个 issue）：
//
//	 1. Manifest 可解析（YAML/JSON 同一结构；请求体 ≤1 MiB 由 API 层限制）
//	 2. 结构校验（顶层 task:、必填、类型）
//	 3. deploy XOR service（两者都缺/都给 → 拒绝，§10.1）
//	 4. runners[] 数量 ∈ [0, 200]（D-77：可为 0，只跑 deploy/precheck/clean；超限 TASK_TOO_LARGE）
//	 5. runner+version 命中注册表；deploy/clean 角色不可手动引用（deploy 由提交体的
//	    deploy.runner 引用注入、clean 由该 runner 的 clean_runner 声明注入）
//	 6. execution.profile 已登记
//	 7. input 按 input_format 校验（jsonl 逐行带行号 + 字段名，D-67）
//	 8. exports/consumes 闭合；任何两个 runner 的 exports 不允许同名 key（§5.12）
//	 9. on_failure 不得弱化 Runner 声明；pre_check 必须 abort_task
//	10. required 的 env 都有来源；secrets 引用存在
//	11. precheck: auto 时注册表恰好一个 role=pre_check
//	12. timeout_seconds ≤ 24h；task.name ≤ 200（D-58）
//
// 通过后生成 resolved 快照（管道序列：注入的 deploy / precheck / clean、冻结渲染输入、
// 输入落档路径）与 task_runners 行。

// 平台注入的 runner 行名（与 03 §3.1 一致；泄漏重放为 service-clean-replay）。
//
// deploy/clean 解耦裁决：平台不感知 "genet"/"mock"/"existing" 这些名字——deploy 行按
// 提交体的 deploy.runner 引用注入（行名 = 解析出的 runner 名），clean 行按该 runner
// runner.yaml 的 clean_runner 声明注入；唯一固定的注入行名是 precheck。
const (
	InjectedPrecheckName = "precheck"
)

// Manifest 是提交体（openapi TaskManifest；顶层嵌套 task:，D-40）。
type Manifest struct {
	Task TaskBody `json:"task"`
}

// TaskBody 是 Manifest 的 task: 内容（deploy XOR service，D-27）。
type TaskBody struct {
	Name     string            `json:"name"`
	Deploy   *DeployManifest   `json:"deploy,omitempty"`
	Service  *ServiceManifest  `json:"service,omitempty"`
	Precheck json.RawMessage   `json:"precheck,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Secrets  map[string]string `json:"secrets,omitempty"`
	Runners  []RunnerInput     `json:"runners"`
}

// DeployManifest 是部署模式的提交：**runner 引用**（与 tests[] 同构，deploy/clean 解耦
// 裁决）。平台按 runner+version 从注册表解析出 role=deploy 的 Runner 注入为 seq=1；
// clean 是否注入由该 runner runner.yaml 的 clean_runner 声明决定，提交体不出现 clean。
type DeployManifest struct {
	Runner  string `json:"runner"`          // deploy 角色的 runner 名（如 genet-deploy / mock-deploy / existing-deploy）
	Version string `json:"version"`         // 语义化版本（与 tests[] 同构，必填）
	Input   string `json:"input,omitempty"` // 该 runner 的输入原文（yaml/jsonl 按 runner.yaml input_format）
}

// ServiceManifest 是外接模式（"序号 0 的导出"，§10.3）。
type ServiceManifest struct {
	URL       string `json:"url"`
	ModelName string `json:"model_name,omitempty"`
	ModelPath string `json:"model_path,omitempty"`
}

// RunnerInput 是 runners[] 一项（没有 args / retry / cleanup.policy，D-66/D-69/D-70）。
type RunnerInput struct {
	Name           string        `json:"name"`
	Runner         string        `json:"runner"`
	Version        string        `json:"version"`
	Execution      ExecutionSpec `json:"execution"`
	Input          string        `json:"input,omitempty"`
	OnFailure      string        `json:"on_failure,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
}

// ExecutionSpec 是执行环境引用（只能引用已登记 Profile）。
type ExecutionSpec struct {
	Profile string `json:"profile"`
}

// PrecheckSpec 是 precheck 的显式对象形态（C-5.2）。
type PrecheckSpec struct {
	Runner         string        `json:"runner"`
	Version        string        `json:"version"`
	Execution      ExecutionSpec `json:"execution"`
	Input          string        `json:"input,omitempty"`
	OnFailure      string        `json:"on_failure,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
}

// Catalog 是组装对 Runner 注册表的只读依赖（registry.Registry 直接满足）。
type Catalog interface {
	Resolve(ctx context.Context, name, version string) (domain.RunnerSpec, error)
	List(ctx context.Context, role string, includeOrphaned bool) ([]domain.RunnerSpec, error)
}

// Assembler 把 Manifest 组装成线性管道（api submit / dry-run 共用）。
type Assembler struct {
	Catalog Catalog
	// ProfileExists / SecretExists 是引用校验依赖（store 提供；测试注入假实现）。
	ProfileExists func(ctx context.Context, name string) (bool, error)
	SecretExists  func(ctx context.Context, name string) (bool, error)
	// DefaultTimeoutSeconds 是未显式给 timeout 时的平台默认（AG_RUNNER_TIMEOUT）。
	DefaultTimeoutSeconds int
	// MaxTimeoutSeconds 是覆盖上限（AG_RUNNER_TIMEOUT_MAX，默认 24h；
	// 同时是 task_runners.timeout_seconds 的 CHECK 上限）。
	MaxTimeoutSeconds int
	// MaxRunners 是单 Task runner 数上限（AG_TASK_MAX_RUNNERS）。
	MaxRunners int
}

func (a *Assembler) defaults() {
	if a.DefaultTimeoutSeconds <= 0 {
		a.DefaultTimeoutSeconds = 4 * 3600
	}
	if a.MaxTimeoutSeconds <= 0 {
		a.MaxTimeoutSeconds = 24 * 3600
	}
	if a.MaxRunners <= 0 {
		a.MaxRunners = 200
	}
}

// ParseManifest 把请求体（YAML 或 JSON；同一结构）解析成 Manifest。
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("Manifest 不是合法的 YAML/JSON: %w", err)
	}
	return &m, nil
}

// Pipeline 是组装结果：store.SubmitTask 的直接输入。
type Pipeline struct {
	// Task 已填 ID / Name / Manifest / Resolved / Endpoint / Deploy / Env / SecretEnv。
	Task *store.Task
	// Runners 是含注入行的最终执行序（seq 从 1 连续递增）。
	Runners []*store.RunnerRun
	// Inputs 是输入落盘清单（API 层落库后写 <storage-root>/<task>/attempts/1/input/<seq>/）。
	Inputs []InputFile
	// Endpoint 是外接模式的静态端点（部署模式为 nil，收割后回填）。
	Endpoint *domain.Endpoint
	// DeployDoc 是部署模式提交时冻结的部署描述（外接模式为 nil）。
	DeployDoc *domain.DeployDoc
	// Resolved 是冻结快照（已 marshal 进 Task.Resolved，冗余保留给 dry-run 预览）。
	Resolved ResolvedSnapshot
}

// ResolvedSnapshot 是 tasks.resolved 的 jsonb 形状（03 §4.2）。
type ResolvedSnapshot struct {
	SchemaVersion string           `json:"schema_version"`
	Precheck      ResolvedPrecheck `json:"precheck"`
	Runners       []ResolvedRunner `json:"runners"`
}

// ResolvedPrecheck 记录注入判据（precheck:"off" 时只有 injected=false）。
type ResolvedPrecheck struct {
	Injected bool   `json:"injected"`
	Runner   string `json:"runner,omitempty"`
	Version  string `json:"version,omitempty"`
}

// ResolvedRunner 是冻结后的单个 runner 解析结果（含 Job 渲染输入）。
type ResolvedRunner struct {
	Seq            int                    `json:"seq"`
	Name           string                 `json:"name"`
	Runner         string                 `json:"runner"`
	Version        string                 `json:"version"`
	Role           string                 `json:"role"`
	Injected       bool                   `json:"injected"`
	Profile        string                 `json:"profile"`
	TimeoutSeconds int                    `json:"timeout_seconds"`
	OnFailure      string                 `json:"on_failure"`
	InputFile      string                 `json:"input_file,omitempty"`
	InputFormat    string                 `json:"input_format,omitempty"`
	Entry          []string               `json:"entry,omitempty"`
	Resources      domain.ResourceRequest `json:"resources,omitempty"`
	Mounts         []domain.MountSpec     `json:"mounts,omitempty"`
	Exports        []string               `json:"exports,omitempty"`
	Consumes       []string               `json:"consumes,omitempty"`
	InputRef       string                 `json:"input_ref,omitempty"`
	// EnvDefaults 是 runner.yaml env 声明的非敏感缺省值（key→default；§12.4）。
	// Job 渲染时在 Task env / 上游 exports 之后注入（先到者胜，不覆盖显式来源）。
	EnvDefaults map[string]string `json:"env_defaults,omitempty"`
}

// InputFile 是输入落盘清单的一项（内容原样落盘为只读挂载源，§5.7）。
type InputFile struct {
	Seq     int
	Name    string
	Format  string
	Content string
}

// resolvedUser 是校验阶段的中间产物（Runner 规格 + 提交意图）。
type resolvedUser struct {
	spec domain.RunnerSpec
	in   *RunnerInput
}

// Assemble 校验并组装线性管道。校验失败返回 *domain.ValidationError（API 层映射
// 400 VALIDATION_ERROR + detail.issues）；引用缺失返回 *domain.FailureError。
func (a *Assembler) Assemble(ctx context.Context, m *Manifest, taskID, actor string, now domain.Clock) (*Pipeline, error) {
	a.defaults()
	if now == nil {
		now = domain.SystemClock
	}
	ve := &domain.ValidationError{}

	// ---- 2/3/4. 结构与形态校验（C-5.1）----
	if strings.TrimSpace(m.Task.Name) == "" {
		ve.Add("task.name", domain.IssueCodeRequired, "task.name 必填")
	}
	if len(m.Task.Name) > 200 {
		ve.Add("task.name", domain.IssueCodeTooLong, "task.name 超过 200 字符")
	}
	if (m.Task.Deploy == nil) == (m.Task.Service == nil) {
		ve.Add("task", domain.IssueCodeDeployService, "deploy 与 service 必须二选一（两者都缺或都给都拒绝）")
	}
	if len(m.Task.Runners) > a.MaxRunners {
		return nil, domain.NewFailureError(domain.CodeTaskTooLarge,
			fmt.Sprintf("runners 超过单 Task 上限 %d", a.MaxRunners),
			map[string]any{"limit": a.MaxRunners, "actual": len(m.Task.Runners)})
	}
	if !ve.Empty() {
		return nil, ve
	}

	if !ve.Empty() {
		return nil, ve
	}

	// deploy 块校验与解析（runner 引用：必须命中注册表中 role=deploy 的 Runner）。
	// 放在用户 runner 名校验之前：注入行保留名校验需要 deploy/clean runner 的名字。
	var deploySpec *domain.RunnerSpec
	if m.Task.Deploy != nil {
		if m.Task.Deploy.Runner == "" || m.Task.Deploy.Version == "" {
			ve.Add("task.deploy", domain.IssueCodeRequired, "deploy.runner 与 deploy.version 必填（runner 引用与 tests[] 同构）")
		} else {
			spec, err := a.Catalog.Resolve(ctx, m.Task.Deploy.Runner, m.Task.Deploy.Version)
			if err != nil {
				return nil, domain.NewFailureError(domain.CodeRunnerNotFound,
					fmt.Sprintf("deploy.runner %q@%q 不在注册表中（或不是 role=deploy 的 Runner）",
						m.Task.Deploy.Runner, m.Task.Deploy.Version),
					map[string]any{"path": "task.deploy.runner", "runner": m.Task.Deploy.Runner, "version": m.Task.Deploy.Version})
			}
			if spec.Role != domain.RoleDeploy {
				return nil, domain.NewFailureError(domain.CodeValidationError,
					fmt.Sprintf("deploy.runner %q 的角色是 %s，必须是 role=deploy 的 Runner", spec.Name, spec.Role),
					map[string]any{"path": "task.deploy.runner", "runner": spec.Name, "role": spec.Role})
			}
			deploySpec = &spec
		}
	}
	if !ve.Empty() {
		return nil, ve
	}

	// runner 名/字段级结构校验。
	// 保留名 = 平台注入行的行名：precheck（固定）+ deploy 引用的 runner 名 +
	// 其 clean_runner 声明（上面 deploy 解析后已补进集合）。
	reservedNames := map[string]bool{InjectedPrecheckName: true}
	if deploySpec != nil {
		reservedNames[deploySpec.Name] = true
		if deploySpec.CleanRunner != "" {
			reservedNames[deploySpec.CleanRunner] = true
		}
	}
	usedNames := map[string]bool{}
	for i := range m.Task.Runners {
		r := &m.Task.Runners[i]
		if r.Name == "" {
			ve.Add(fmt.Sprintf("runners[%d].name", i), domain.IssueCodeRequired, "name 必填（Task 内唯一）")
			continue
		}
		if usedNames[r.Name] {
			ve.Add(fmt.Sprintf("runners[%d].name", i), domain.IssueCodeNameConflict, "name 与前面的 runner 重复")
		}
		usedNames[r.Name] = true
		if reservedNames[r.Name] {
			ve.Add(fmt.Sprintf("runners[%d].name", i), domain.IssueCodeNameConflict,
				"name %q 是平台注入行保留名，不可占用", r.Name)
		}
		if r.Runner == "" || r.Version == "" {
			ve.Add(fmt.Sprintf("runners[%d].runner", i), domain.IssueCodeRequired, "runner 与 version 必填")
		}
		if r.Execution.Profile == "" {
			ve.Add(fmt.Sprintf("runners[%d].execution.profile", i), domain.IssueCodeRequired, "execution.profile 必填")
		}
	}
	if !ve.Empty() {
		return nil, ve
	}

	// ---- 3. 注入判定（C-5.2：deploy XOR service 决定 service-deploy / service-clean）----
	precheck, perr := a.resolvePrecheck(ctx, m.Task.Precheck, ve)
	if perr != nil {
		return nil, perr
	}
	if !ve.Empty() {
		return nil, ve
	}

	// ---- 5/6/9/12. 逐 runner 校验与冻结 ----
	users := make([]resolvedUser, 0, len(m.Task.Runners))
	for i := range m.Task.Runners {
		in := &m.Task.Runners[i]
		if in.Runner == "" || in.Version == "" || in.Execution.Profile == "" || in.Name == "" {
			continue // 已由上面的 issue 覆盖
		}
		spec, err := a.Catalog.Resolve(ctx, in.Runner, in.Version)
		if err != nil {
			return nil, domain.NewFailureError(domain.CodeRunnerNotFound,
				fmt.Sprintf("runners[%d].runner %q@%q 不在注册表中", i, in.Runner, in.Version),
				map[string]any{"path": fmt.Sprintf("runners[%d].runner", i), "runner": in.Runner, "version": in.Version})
		}
		if spec.Role == domain.RoleDeploy || spec.Role == domain.RoleClean {
			return nil, domain.NewFailureError(domain.CodeRunnerNotFound,
				fmt.Sprintf("runners[%d].runner %q 的角色 %s 由平台自动注入，不可手动引用", i, in.Runner, spec.Role),
				map[string]any{"path": fmt.Sprintf("runners[%d].runner", i)})
		}
		// Profile（C-5.1 第 6 条）。
		if a.ProfileExists != nil {
			ok, perr := a.ProfileExists(ctx, in.Execution.Profile)
			if perr != nil {
				return nil, perr
			}
			if !ok {
				return nil, domain.NewFailureError(domain.CodeExecutionProfileMissing,
					fmt.Sprintf("runners[%d].execution.profile %q 未登记", i, in.Execution.Profile),
					map[string]any{"path": fmt.Sprintf("runners[%d].execution.profile", i), "profile": in.Execution.Profile})
			}
		}
		// 超时（C-5.1 第 12 条；CHECK 上限 86400）。
		timeout := in.TimeoutSeconds
		if timeout <= 0 {
			timeout = a.DefaultTimeoutSeconds
		}
		if timeout > a.MaxTimeoutSeconds {
			ve.Add(fmt.Sprintf("runners[%d].timeout_seconds", i), domain.IssueCodeTimeoutRange,
				"timeout_seconds 超过上限 %d 秒（§6.6）", a.MaxTimeoutSeconds)
		}
		// on_failure 合并取严（C-5.1 第 9 条）：不得弱化 Runner 声明。
		onFailure := firstNonEmpty(spec.OnFailure, domain.OnFailureContinue)
		if in.OnFailure != "" {
			if weaker(in.OnFailure, onFailure) {
				ve.Add(fmt.Sprintf("runners[%d].on_failure", i), domain.IssueCodeOnFailure,
					"on_failure=%s 会弱化 Runner 声明 %s（§7.3）", in.OnFailure, onFailure)
			} else {
				onFailure = in.OnFailure
			}
		}
		if spec.Role == domain.RolePreCheck && onFailure != domain.OnFailureAbortTask {
			ve.Add(fmt.Sprintf("runners[%d].on_failure", i), domain.IssueCodeOnFailure,
				"pre_check 角色必须 on_failure=abort_task（§7.3）")
		}
		users = append(users, resolvedUser{spec: spec, in: in})
	}
	if !ve.Empty() {
		return nil, ve
	}

	// ---- 7. 输入校验（D-67：jsonl 带行号 + 字段名）----
	for _, u := range users {
		issues := domain.ValidateInput(u.spec.InputFormat, u.in.Input, u.spec.Parameters)
		for _, is := range issues {
			is.Path = fmt.Sprintf("runners[%d].input", indexRunners(m, u.in))
			ve.Issues = append(ve.Issues, is)
		}
	}
	if !ve.Empty() {
		return nil, ve
	}

	// ---- 11. precheck: auto 的唯一性（C-5.1；显式对象形态在 resolvePrecheck 已校验）----
	if precheck.inject && precheck.fromManifest == nil {
		precheckers, err := a.Catalog.List(ctx, domain.RolePreCheck, false)
		if err != nil {
			return nil, err
		}
		if len(precheckers) != 1 {
			return nil, domain.NewFailureError(domain.CodeValidationError,
				fmt.Sprintf("precheck: auto 需要注册表恰好一个 role=pre_check 的 Runner，实际 %d 个", len(precheckers)),
				map[string]any{"issues": []any{map[string]any{
					"path": "task.precheck", "code": domain.IssueCodePrecheckCount, "message": "pre_check Runner 必须唯一",
				}}})
		}
		precheck.runner = &precheckers[0]
		precheck.profile = firstUserProfile(m)
	}

	// ---- 8/10. exports/consumes 闭合 + required env（§5.12 / §12.4）----
	provided := map[string]string{} // key -> 来源描述
	if m.Task.Service != nil {
		provided[EnvServedURL] = "service"
		if m.Task.Service.ModelName != "" {
			provided[EnvServedModelName] = "service"
		}
		if m.Task.Service.ModelPath != "" {
			provided[EnvModelPath] = "service"
		}
	}
	for k := range m.Task.Env {
		provided[k] = "env"
	}
	for k := range m.Task.Secrets {
		provided[k] = "secret"
	}
	// 部署模式：service-deploy 的四件套（AG_SERVED_* / AG_MODEL_PATH / AG_DEPLOY_REF）
	// 是内置契约的下游来源（§5.12）；consumes 对它们闭合合法。
	if m.Task.Deploy != nil {
		provided[EnvServedURL] = "deploy"
		provided[EnvServedModelName] = "deploy"
		provided[EnvModelPath] = "deploy"
		provided[EnvDeployRef] = "deploy"
	}
	// exports 不允许重复 key（§5.12）。
	seenExports := map[string]int{}
	for i := range m.Task.Runners {
		for _, key := range users[i].spec.Exports {
			if prev, dup := seenExports[key]; dup {
				ve.Add(fmt.Sprintf("runners[%d].exports", i), domain.IssueCodeExportDupKey,
					"exports key %q 与 runners[%d] 重复（§5.12 组合期禁止重复 key）", key, prev)
				continue
			}
			seenExports[key] = i
		}
	}
	if !ve.Empty() {
		return nil, ve
	}
	// consumes 按管道序闭合：前序 runner 的 exports 对后续 runner 可见（§5.12）。
	for i := range m.Task.Runners {
		u := users[i]
		for _, key := range u.spec.Consumes {
			if _, ok := provided[key]; !ok {
				ve.Add(fmt.Sprintf("runners[%d].consumes", i), domain.IssueCodeConsumeOpen,
					"consumes key %q 没有上游导出、静态 service 值或 Task 级来源（§5.12）", key)
			}
		}
		for envKey, decl := range u.spec.Env {
			if !decl.Required {
				continue
			}
			if _, ok := provided[envKey]; ok {
				continue
			}
			if isPublicParamEnv(envKey) {
				continue
			}
			ve.Add(fmt.Sprintf("runners[%d].env", i), domain.IssueCodeEnvMissing,
				"Runner 声明的必填环境变量 %s 没有来源（Task env / secrets / 上游 exports）", envKey)
		}
		// 本 runner 的 exports 进入下游可见集合（管道序即依赖方向）。
		for _, key := range u.spec.Exports {
			if _, exists := provided[key]; !exists {
				provided[key] = fmt.Sprintf("runners[%d]", i)
			}
		}
	}
	for name := range m.Task.Secrets {
		if a.SecretExists != nil {
			ok, serr := a.SecretExists(ctx, name)
			if serr != nil {
				return nil, serr
			}
			if !ok {
				ve.Add("task.secrets", domain.IssueCodeSecretMissing, "secret 引用 %q 不存在", name)
			}
		}
	}
	if !ve.Empty() {
		return nil, ve
	}

	// ---- 组装管道 ----
	return a.buildPipeline(ctx, m, users, precheck, deploySpec, taskID, actor, now)
}

// indexRunners 找 in 在 runners[] 里的下标（issue 定位用）。
func indexRunners(m *Manifest, in *RunnerInput) int {
	for i := range m.Task.Runners {
		if &m.Task.Runners[i] == in {
			return i
		}
	}
	return 0
}

// weaker 报告 manifest 的 on_failure 是否比 Runner 声明更弱（只能取严，§7.3）。
func weaker(in, declared string) bool {
	return declared == domain.OnFailureAbortTask && in == domain.OnFailureContinue
}

func firstUserProfile(m *Manifest) string {
	for i := range m.Task.Runners {
		if m.Task.Runners[i].Execution.Profile != "" {
			return m.Task.Runners[i].Execution.Profile
		}
	}
	return ""
}

// precheckDecision 是 precheck 的注入判定结果。
type precheckDecision struct {
	inject       bool
	off          bool
	fromManifest *PrecheckSpec
	runner       *domain.RunnerSpec
	profile      string
}

// resolvePrecheck 解析 precheck 字段：缺省 auto / "auto" / "off" / 显式对象（C-5.2）。
func (a *Assembler) resolvePrecheck(ctx context.Context, raw json.RawMessage, ve *domain.ValidationError) (precheckDecision, error) {
	dec := precheckDecision{inject: true} // 缺省 auto
	if len(raw) == 0 {
		return dec, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "", "auto":
			return dec, nil
		case "off":
			dec.inject = false
			dec.off = true
			return dec, nil
		default:
			ve.Add("task.precheck", domain.IssueCodeUnsupported, "precheck 只接受 auto | off | 显式对象")
			return dec, error(ve)
		}
	}
	var spec PrecheckSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		ve.Add("task.precheck", domain.IssueCodeUnsupported, "precheck 形态不合法（auto | off | 显式对象）")
		return dec, error(ve)
	}
	if spec.Runner == "" || spec.Version == "" || spec.Execution.Profile == "" {
		ve.Add("task.precheck", domain.IssueCodeRequired, "precheck 显式对象必须给 runner / version / execution.profile")
		return dec, error(ve)
	}
	rspec, err := a.Catalog.Resolve(ctx, spec.Runner, spec.Version)
	if err != nil {
		return dec, domain.NewFailureError(domain.CodeRunnerNotFound,
			fmt.Sprintf("task.precheck.runner %q@%q 不在注册表中", spec.Runner, spec.Version),
			map[string]any{"path": "task.precheck.runner"})
	}
	if rspec.Role != domain.RolePreCheck {
		ve.Add("task.precheck", domain.IssueCodeUnsupported, "precheck 显式对象必须引用 role=pre_check 的 Runner")
		return dec, error(ve)
	}
	dec.fromManifest = &spec
	dec.runner = &rspec
	dec.profile = spec.Execution.Profile
	return dec, nil
}

// buildPipeline 生成 Task / RunnerRun 行与 resolved 快照（C-5.2 的管道序列）：
//
//	[有 deploy] seq=1 <deploy.runner 解析出的 runner>（input = deploy.input 原文，格式按 runner.yaml）
//	[precheck≠off] seq=+1 <pre_check runner>（name=precheck，input 空、公共参数走 env）
//	seq=+1… 用户 runners[]（仅 test / 显式 pre_check）
//	[deploy runner 声明了 clean_runner] seq=+n <clean runner>（finally，C-5.7）
func (a *Assembler) buildPipeline(ctx context.Context, m *Manifest, users []resolvedUser, precheck precheckDecision,
	deploySpec *domain.RunnerSpec, taskID, actor string, now domain.Clock) (*Pipeline, error) {

	t := &store.Task{
		ID:          taskID,
		Name:        m.Task.Name,
		SubmittedBy: actor,
	}
	t.SubmittedAt = now.Now()
	if raw, err := json.Marshal(m); err == nil {
		t.Manifest = raw
	}
	if env, err := json.Marshal(m.Task.Env); err == nil {
		t.Env = env
	}
	var secretEnv []map[string]string
	for name, ref := range m.Task.Secrets {
		secretEnv = append(secretEnv, map[string]string{"name": name, "secret_ref": ref})
	}
	if raw, err := json.Marshal(secretEnv); err == nil {
		t.SecretEnv = raw
	}

	p := &Pipeline{Task: t}

	// 外接模式：静态端点（"序号 0 的导出"）+ 全部 runner 的 endpoint_key。
	if m.Task.Service != nil {
		ep := domain.EndpointFromManifest(m.Task.Service.URL, m.Task.Service.ModelName, m.Task.Service.ModelPath)
		p.Endpoint = &ep
		if raw, err := json.Marshal(ep); err == nil {
			t.Endpoint = raw
		}
	}

	// 部署模式：冻结 runner 引用（deploy 块与 tests[] 同构；release_name 由平台派生，D-44）。
	if m.Task.Deploy != nil && deploySpec != nil {
		sum := sha256.Sum256([]byte(m.Task.Deploy.Input))
		p.DeployDoc = &domain.DeployDoc{
			Runner:      deploySpec.Name,
			Version:     deploySpec.Version,
			Input:       m.Task.Deploy.Input,
			SpecDigest:  hex.EncodeToString(sum[:]),
			ReleaseName: domain.ReleaseName(taskID, 1),
		}
		if raw, err := json.Marshal(p.DeployDoc); err == nil {
			t.Deploy = raw
		}
	}

	// 注入行的执行时限：平台 CHECK 上限（86400）。72h 部署总窗口由 runner 内部按
	// AG_DEPLOY_TIMEOUT 自判（K-6），不是 runner 超时列的职责。
	injectedTimeout := a.MaxTimeoutSeconds

	rows := make([]ResolvedRunner, 0, len(users)+3)
	runners := make([]*store.RunnerRun, 0, len(users)+3)
	inputs := make([]InputFile, 0, len(users)+2)
	seq := 0
	addRow := func(name string, spec domain.RunnerSpec, profile string, injected bool, timeout int, onFailure string) {
		seq++
		format := firstNonEmpty(spec.InputFormat, domain.InputFormatJSONL)
		inputRef := fmt.Sprintf("attempts/1/input/%d/", seq)
		var envDefaults map[string]string
		for key, decl := range spec.Env {
			if decl.Default != "" {
				if envDefaults == nil {
					envDefaults = map[string]string{}
				}
				envDefaults[key] = decl.Default
			}
		}
		rows = append(rows, ResolvedRunner{
			Seq: seq, Name: name, Runner: spec.Name, Version: spec.Version,
			Role: spec.Role, Injected: injected, Profile: profile,
			TimeoutSeconds: timeout, OnFailure: onFailure,
			InputFile: domain.InputFileName(format), InputFormat: format,
			Entry: spec.Entry, Resources: spec.Resources, Mounts: spec.Mounts,
			Exports: spec.Exports, Consumes: spec.Consumes, InputRef: inputRef,
			EnvDefaults: envDefaults,
		})
		runners = append(runners, &store.RunnerRun{
			TaskID:            taskID,
			AttemptNo:         1,
			Seq:               seq,
			Name:              name,
			RunnerName:        spec.Name,
			RunnerVersion:     spec.Version,
			Role:              spec.Role,
			ImageRef:          spec.Image,
			Profile:           profile,
			InputRef:          inputRef,
			OnFailure:         onFailure,
			TimeoutSeconds:    timeout,
			Status:            string(domain.RunnerPending),
			EndpointKey:       endpointKeyOf(p.Endpoint),
			EndpointExclusive: spec.Concurrency.EndpointExclusive,
		})
	}

	// clean 注入判据：deploy runner 的 runner.yaml 声明了 clean_runner（deploy/clean
	// 解耦裁决——clean 配对由 runner 自己声明，平台不按名字特判；existing-deploy 等不
	// 声明的 deploy runner 无 clean 阶段）。
	cleanRunnerName := ""
	if deploySpec != nil {
		cleanRunnerName = deploySpec.CleanRunner
	}

	// 1. 有部署 → 按提交体的 runner 引用注入 deploy 行（C-5.3.1；seq=1）。
	//    行名 = 解析出的 runner 名（平台不感知具体名字）；输入 = deploy.input 原文，
	//    格式 = 该 runner 的 input_format（genet-deploy: yaml、existing-deploy: jsonl）。
	if m.Task.Deploy != nil && deploySpec != nil {
		addRow(deploySpec.Name, *deploySpec, firstUserProfile(m), true, injectedTimeout, domain.OnFailureAbortTask)
		inputs = append(inputs, InputFile{Seq: len(rows), Name: deploySpec.Name,
			Format:  firstNonEmpty(deploySpec.InputFormat, domain.InputFormatJSONL),
			Content: m.Task.Deploy.Input})
	}

	// 2. precheck 注入（auto：注册表唯一 pre_check；显式对象；off 不注入）。
	if precheck.inject && precheck.runner != nil {
		spec := *precheck.runner
		input := ""
		timeout := a.DefaultTimeoutSeconds
		if precheck.fromManifest != nil {
			input = precheck.fromManifest.Input
			if precheck.fromManifest.TimeoutSeconds > 0 {
				timeout = precheck.fromManifest.TimeoutSeconds
			}
		}
		profile := precheck.profile
		if profile == "" {
			profile = firstUserProfile(m)
		}
		addRow(InjectedPrecheckName, spec, profile, true, timeout, domain.OnFailureAbortTask)
		inputs = append(inputs, InputFile{Seq: len(rows), Name: InjectedPrecheckName,
			Format: firstNonEmpty(spec.InputFormat, domain.InputFormatJSONL), Content: input})
	}

	// 3. 用户 runners（仅 test / 显式 pre_check，C-5.2）。
	for i := range users {
		u := users[i]
		timeout := u.in.TimeoutSeconds
		if timeout <= 0 {
			timeout = a.DefaultTimeoutSeconds
		}
		addRow(u.in.Name, u.spec, u.in.Execution.Profile, false, timeout, runnerOnFailure(u))
		inputs = append(inputs, InputFile{Seq: len(rows), Name: u.in.Name,
			Format: firstNonEmpty(u.spec.InputFormat, domain.InputFormatJSONL), Content: u.in.Input})
	}

	// 4. deploy runner 声明了 clean_runner → 按 seq=last+1 注入 clean 行（finally，C-5.7）。
	//    未声明（如 existing-deploy）不注入：无可拆除对象。
	if cleanRunnerName != "" {
		spec, err := a.Catalog.Resolve(ctx, cleanRunnerName, "")
		if err != nil {
			return nil, domain.NewFailureError(domain.CodeRunnerInvalid,
				fmt.Sprintf("clean_runner %q 不在注册表中（检查 %s 的 runner.yaml 声明与 AG_RUNNERS_DIR 扫描）",
					cleanRunnerName, deploySpec.Name), nil)
		}
		if spec.Role != domain.RoleClean {
			return nil, domain.NewFailureError(domain.CodeRunnerInvalid,
				fmt.Sprintf("clean_runner %q 的角色是 %s，必须是 role=clean 的 Runner", spec.Name, spec.Role), nil)
		}
		addRow(spec.Name, spec, firstUserProfile(m), true, injectedTimeout, domain.OnFailureAbortTask)
	}

	p.Inputs = inputs
	p.Runners = runners
	p.Resolved = ResolvedSnapshot{
		SchemaVersion: "2.0",
		Precheck: ResolvedPrecheck{
			Injected: precheck.inject,
			Runner:   runnerNameOf(precheck.runner),
			Version:  runnerVersionOf(precheck.runner),
		},
		Runners: rows,
	}
	if raw, err := json.Marshal(p.Resolved); err == nil {
		t.Resolved = raw
	}
	return p, nil
}

// runnerOnFailure 返回用户 runner 的最终 on_failure（已通过"不得弱化"校验）。
func runnerOnFailure(u resolvedUser) string {
	if u.in.OnFailure != "" {
		return u.in.OnFailure
	}
	return firstNonEmpty(u.spec.OnFailure, domain.OnFailureContinue)
}

func endpointKeyOf(ep *domain.Endpoint) *string {
	if ep == nil || ep.Key == "" {
		return nil
	}
	v := ep.Key
	return &v
}

func runnerNameOf(spec *domain.RunnerSpec) string {
	if spec == nil {
		return ""
	}
	return spec.Name
}

func runnerVersionOf(spec *domain.RunnerSpec) string {
	if spec == nil {
		return ""
	}
	return spec.Version
}
