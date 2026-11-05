package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// buildPrepared 把校验通过的提交冻结成落库快照（C-5.1 收尾、C-5.2、C-5.9）。
func (s *Submitter) buildPrepared(m *Manifest, kind string, specs []domain.TesterSpec,
	argv [][]string, pm precheckMode, precheckSpec *domain.TesterSpec,
	endpoint domain.Endpoint, cleanupPolicy, submittedBy string) (*Prepared, error) {

	now := time.Now().UTC()
	runID := store.NewRunID(now)
	name := ""
	if kind == "task" {
		name = m.Task.Name
	} else {
		name = m.Run.Name
	}

	// 公共参数：service 模式提交时即可注入；deploy 模式等 Endpoint ready 后回填（C-5.3）。
	serviceMode := m.Service != nil
	var endpointKey *string
	if serviceMode {
		k := endpoint.Key
		endpointKey = &k
	}

	var (
		tasks    []SubmittedTask
		resolved []struct {
			ID             string `json:"id"`
			Seq            int    `json:"seq"`
			Name           string `json:"name"`
			Profile        string `json:"profile"`
			TimeoutSeconds int    `json:"timeout_seconds"`
			Retry          any    `json:"retry,omitempty"`
		}
		precheckInjected bool
		precheckSeq      = -1
	)

	// 1) Pre-Check（seq=0）
	if precheckSpec != nil {
		p := precheckParams(m, kind, pm, precheckSpec, argv)
		p.secrets = m.Secrets
		t, err := makeTask(runID, 0, "precheck", *precheckSpec, p, endpoint, endpointKey, serviceMode)
		if err == nil {
			s.applyTimeout(t, p.timeout)
		}
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, SubmittedTask{Task: t, Spec: *precheckSpec})
		precheckInjected = true
		precheckSeq = 0
		resolved = append(resolved, resolutionOf(t, p.retry))
	}

	// 2) 业务 Task
	baseSeq := 0
	if precheckInjected {
		baseSeq = 1
	}
	if kind == "task" {
		t := m.Task
		p := taskParams{
			name: t.Name, profile: t.Execution.Profile, argsRaw: t.Args,
			onFailure: t.OnFailure, timeout: t.TimeoutSeconds, retry: t.Retry, env: t.Env,
		}
		p.secrets = m.Secrets
		sp := specs[0]
		task, err := makeTask(runID, baseSeq, t.Name, sp, p, endpoint, endpointKey, serviceMode)
		if err == nil {
			s.applyTimeout(task, p.timeout)
		}
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, SubmittedTask{Task: task, Spec: sp})
		resolved = append(resolved, resolutionOf(task, p.retry))
	} else {
		for i, t := range m.Tests {
			p := taskParams{
				name: t.Name, profile: t.Execution.Profile, argsRaw: t.Args,
				onFailure: t.OnFailure, timeout: t.TimeoutSeconds, retry: t.Retry, env: t.Env,
			}
			p.secrets = m.Secrets
			sp := specs[i]
			task, err := makeTask(runID, baseSeq+i, t.Name, sp, p, endpoint, endpointKey, serviceMode)
			if err == nil {
				s.applyTimeout(task, p.timeout)
			}
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, SubmittedTask{Task: task, Spec: sp})
			resolved = append(resolved, resolutionOf(task, p.retry))
		}
	}

	// resolved 快照（03 §4.2）
	snap := map[string]any{
		"kind":              kind,
		"tasks":             resolved,
		"args_snapshot_sha": argsSnapshotSHA(argv),
		"schema_version":    "1.0",
	}
	if precheckInjected {
		snap["precheck"] = map[string]any{
			"injected": true,
			"tester":   precheckSpec.Name,
			"version":  precheckSpec.Version,
			"seq":      precheckSeq,
		}
	} else {
		snap["precheck"] = map[string]any{"injected": false}
	}
	resolvedJSON, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("api: marshal resolved: %w", err)
	}

	manifestJSON, err := json.Marshal(manifestSnapshot(m, kind, name))
	if err != nil {
		return nil, fmt.Errorf("api: marshal manifest: %w", err)
	}

	return &Prepared{
		RunID:         runID,
		Kind:          kind,
		Name:          name,
		Manifest:      manifestJSON,
		Resolved:      resolvedJSON,
		CleanupPolicy: cleanupPolicy,
		Endpoint:      endpoint,
		Tasks:         tasks,
	}, nil
}

// taskParams 是构造 Task 行的统一输入。
type taskParams struct {
	name      string
	profile   string
	argsRaw   string
	onFailure string
	timeout   int
	retry     *RetrySpec
	env       map[string]string
	secrets   map[string]string
}

// applyTimeout 用平台默认值与上限修正 Task 超时（提交没给值时）。
func (s *Submitter) applyTimeout(t *store.Task, requested int) {
	if requested <= 0 {
		t.TimeoutSeconds = int(s.DefaultTimeout.Seconds())
	} else {
		t.TimeoutSeconds = requested
	}
	if max := int(s.MaxTimeout.Seconds()); max > 0 && t.TimeoutSeconds > max {
		t.TimeoutSeconds = max
	}
}

// precheckParams 按 C-5.2 决定 Pre-Check 的参数。
func precheckParams(m *Manifest, kind string, pm precheckMode, spec *domain.TesterSpec,
	argv [][]string) taskParams {

	if pm.mode == "explicit" {
		raw := pm.spec.Args
		return taskParams{
			name:      "precheck",
			profile:   profileOrDefault(pm.spec.Execution.Profile),
			argsRaw:   raw,
			onFailure: domain.OnFailureAbortRun,
			timeout:   pm.spec.TimeoutSeconds,
			retry:     pm.spec.Retry,
		}
	}

	// auto：args=""，profile 复用 tests[0]；无 tests 时取 precheck.execution.profile（此处无对象，落 default）
	profile := "default"
	if kind == "run" && len(m.Tests) > 0 {
		profile = profileOrDefault(m.Tests[0].Execution.Profile)
	}
	_ = argv
	return taskParams{
		name:      "precheck",
		profile:   profile,
		argsRaw:   "",
		onFailure: domain.OnFailureAbortRun,
	}
}

// makeTask 构造一行 tasks（字段与 03 §3.1 对齐）。
func makeTask(runID string, seq int, name string, spec domain.TesterSpec, p taskParams,
	endpoint domain.Endpoint, endpointKey *string, serviceMode bool) (*store.Task, error) {

	argRaw := p.argsRaw
	parsed, err := domain.SplitArgs(argRaw)
	if err != nil {
		return nil, err
	}
	injected := []string{}
	if serviceMode {
		injected = domain.InjectPublicParams(parsed, endpoint)
	}

	timeout := p.timeout

	onFailure := p.onFailure
	if onFailure == "" {
		onFailure = spec.OnFailure
	}
	if onFailure == "" {
		onFailure = domain.OnFailureContinue
	}

	env := map[string]string{}
	if p.env != nil {
		for k, v := range p.env {
			env[k] = v
		}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("api: marshal env: %w", err)
	}
	argvJSON, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("api: marshal argv: %w", err)
	}
	injectedJSON, err := json.Marshal(injected)
	if err != nil {
		return nil, fmt.Errorf("api: marshal injected argv: %w", err)
	}
	secretEnv := secretEnvFor(spec, p.secrets)

	return &store.Task{
		ID:                store.NewTaskID(),
		RunID:             runID,
		Seq:               seq,
		Name:              name,
		TesterName:        spec.Name,
		TesterVersion:     spec.Version,
		ImageRef:          spec.Image,
		Profile:           profileOrDefault(p.profile),
		ArgsRaw:           argRaw,
		ArgsArgv:          argvJSON,
		InjectedArgv:      injectedJSON,
		Env:               envJSON,
		SecretEnv:         secretEnv,
		OnFailure:         onFailure,
		TimeoutSeconds:    timeout,
		RetryPolicy:       retryJSON(p.retry),
		Status:            string(domain.TaskPending),
		EndpointKey:       endpointKey,
		EndpointExclusive: spec.Concurrency.EndpointExclusive,
	}, nil
}

// secretEnvFor 把 tester.yaml 声明的 secret 环境变量转成 tasks.secret_env（03 §4.6）。
// secret_ref 取 Manifest 的 secrets 映射；没有映射时只保留变量名（值为空表示由 Profile/Secret 提供）。
func secretEnvFor(spec domain.TesterSpec, secrets map[string]string) []byte {
	type ref struct {
		Name      string `json:"name"`
		SecretRef string `json:"secret_ref,omitempty"`
	}
	var refs []ref
	for name, decl := range spec.Env {
		if !decl.Secret {
			continue
		}
		// 03 §4.6：只落"有绑定"的项。可选凭据没给就不写（不是写一条空 ref）；
		// required=true 且没给的情况已在提交校验阶段 400（SECRET_REQUIRED）。
		if secretRef := secrets[name]; secretRef != "" {
			refs = append(refs, ref{Name: name, SecretRef: secretRef})
		}
	}
	if len(refs) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// resolutionOf 生成 resolved.tasks[] 的一项。
func resolutionOf(t *store.Task, retry *RetrySpec) struct {
	ID             string `json:"id"`
	Seq            int    `json:"seq"`
	Name           string `json:"name"`
	Profile        string `json:"profile"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Retry          any    `json:"retry,omitempty"`
} {
	var r any
	if retry != nil {
		r = retry
	}
	return struct {
		ID             string `json:"id"`
		Seq            int    `json:"seq"`
		Name           string `json:"name"`
		Profile        string `json:"profile"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Retry          any    `json:"retry,omitempty"`
	}{ID: t.ID, Seq: t.Seq, Name: t.Name, Profile: t.Profile, TimeoutSeconds: t.TimeoutSeconds, Retry: r}
}

// manifestSnapshot 生成 runs.manifest 的冻结快照（03 §4.1；secrets 只存引用）。
// 形状按 07 §4.1 的嵌套写法：run 下承载 deploy/service/precheck/env/secrets/tests/cleanup。
func manifestSnapshot(m *Manifest, kind, name string) map[string]any {
	if kind == "task" {
		return map[string]any{
			"task": map[string]any{
				"name":   name,
				"tester": m.Task.Tester,
			},
			"precheck": m.Precheck,
			"env":      redactEnv(m.Env),
			"secrets":  m.Secrets,
		}
	}
	body := map[string]any{
		"name":    name,
		"tests":   m.Tests,
		"env":     redactEnv(m.Env),
		"secrets": m.Secrets,
	}
	if m.Deploy != nil {
		body["deploy"] = m.Deploy
	}
	if m.Service != nil {
		body["service"] = m.Service
	}
	if m.Precheck != nil {
		body["precheck"] = m.Precheck
	}
	if m.Cleanup != nil {
		body["cleanup"] = m.Cleanup
	}
	return map[string]any{"run": body}
}

func redactEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	return env
}
