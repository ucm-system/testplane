// Package registry 实现 Tester 注册表（design/impl/execution.md §10）。
//
// 职责：启动时扫描 testers/*/tester.yaml → 解析成 domain.TesterSpec → 校验（15 条规则的
// 结构性部分）→ 与库同步（同 name+version 同内容 no-op，不同内容拒绝，拒绝启动）。
// 另外对外提供 Resolve / List / Register 给 API 层，以及公共参数注入（C-8.5）。
//
// 边界：注册表不构建镜像、不渲染表单、不做运行时参数语义校验（§1、§5.4）。
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 校验错误与哨兵错误。对外 code 用 domain.Code*（附录 D）。
var (
	// ErrInvalid 表示 tester.yaml 没通过校验（TESTER_INVALID）。
	ErrInvalid = errors.New("registry: invalid tester")
	// ErrVersionPinned 表示同 name+version 内容不同（TESTER_VERSION_PINNED，K-3）。
	ErrVersionPinned = errors.New("registry: same tester version has different content")
	// ErrForbiddenField 表示出现了附录 C 明令禁止的字段。
	ErrForbiddenField = errors.New("registry: forbidden field")
)

// FieldError 是字段级校验错误（§10 的"带字段路径"）。
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e FieldError) Error() string { return e.Path + ": " + e.Message }

// ValidationError 聚合一次校验的全部字段错误。
type ValidationError struct {
	Name   string       `json:"name"`
	Errors []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Error())
	}
	return fmt.Sprintf("%v: %s: %s", ErrInvalid, e.Name, strings.Join(parts, "; "))
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

// 校验规则常量。
const (
	// RequiredEntry 是 Tester 契约规定的容器内入口（§10 校验项 4）。
	RequiredEntry = "/tester/run.sh"
	// TesterFileName 是注册表唯一识别的文件名。
	TesterFileName = "tester.yaml"
	// RunScriptName 是必须存在且带可执行位的脚本（§10 校验项 14）。
	RunScriptName = "run.sh"
)

var (
	// namePattern：小写 [a-z0-9-]，≤63（§10 校验项 1–2）。
	namePattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	// versionPattern：语义化版本（§10 校验项 2）。
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	// 平台保留的挂载点（§10 校验项 11）。
	reservedMountPaths = map[string]string{
		"/workspace/output": "platform output directory",
		"/tmp":              "platform tmp directory",
		"/tester":           "tester image directory",
	}
	// 禁止出现的 K8s 清单（§6.8、§10 校验项 15）。
	forbiddenManifests = map[string]struct{}{
		"job.yaml":        {},
		"job.yml":         {},
		"deployment.yaml": {},
		"deployment.yml":  {},
		"pod.yaml":        {},
		"pod.yml":         {},
		"service.yaml":    {},
		"service.yml":     {},
		"ingress.yaml":    {},
		"configmap.yaml":  {},
		"pvc.yaml":        {},
		"networkpolicy.yaml": {},
	}
	// 禁止出现的 tester.yaml 顶层字段（§10 校验项 7）。
	forbiddenFields = []string{"requires", "timeout_seconds", "outputs"}
)

// K8sSection 是 tester.yaml 里允许的 k8s 白名单（§10 校验项 8）。
type K8sSection struct {
	NodeSelector    map[string]string `yaml:"node_selector" json:"node_selector"`
	Tolerations     []map[string]any  `yaml:"tolerations" json:"tolerations"`
	Annotations     map[string]string `yaml:"annotations" json:"annotations"`
	SecurityContext map[string]any    `yaml:"security_context" json:"security_context"`
}

// Record 是注册表里的一行（spec + 指纹 + 来源）。
type Record struct {
	Spec      domain.TesterSpec `json:"spec"`
	ContentSHA string           `json:"content_sha"`
	SourceFile string           `json:"source_file"`
	Orphaned  bool              `json:"orphaned"`
}

// Store 是注册表对持久化的唯一依赖（由 backend-core 的 store 适配）。
// 实现者按 (name, version) upsert；content_sha 不同视为"同版本不同内容"。
type Store interface {
	// UpsertTester 在一个事务内写一行；created 表示是否新增。
	UpsertTester(ctx context.Context, rec Record) (created bool, err error)
	// GetTester 读一行；version 为空表示取最新。未找到返回包装 domain.ErrNotFound 的错误。
	GetTester(ctx context.Context, name, version string) (Record, error)
	// ListTesters 列出全部；role 为空不过滤。
	ListTesters(ctx context.Context) ([]Record, error)
}

// Options 是注册表配置。
type Options struct {
	// Now 便于测试注入时钟。
	Now func() time.Time
	// Logf 是可选的日志钩子（nil 即静默）。
	Logf func(format string, args ...any)
	// AllowContentChange 仅测试使用：允许同版本不同内容（生产必须 false）。
	AllowContentChange bool
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// SyncDiff 是一次扫描的结果（§10 的 SyncDiff）。
type SyncDiff struct {
	Added     []string `json:"added"`
	Unchanged []string `json:"unchanged"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
}

// Registry 是 Tester 注册表。内存快照服务于热路径（Resolve/List），
// 持久化通过 Store 完成；启动扫描失败即拒绝启动（K-3：不做运行时更新）。
type Registry struct {
	mu       sync.RWMutex
	records  map[string]Record // key = name@version
	latest   map[string]string // name -> 最新 version
	store    Store
	opts     Options
}

// New 构造注册表。store 可为 nil（纯内存模式，供测试与本地联调）。
func New(store Store, opts Options) *Registry {
	return &Registry{
		records: make(map[string]Record),
		latest:  make(map[string]string),
		store:   store,
		opts:    opts.withDefaults(),
	}
}

// Key 返回注册表主键。
func Key(name, version string) string { return name + "@" + version }

// Scan 扫描 dir 下所有 testers/*/tester.yaml，校验并同步进注册表。
//
// 任何一条不合法都返回错误（调用方应当拒绝启动）。同 name+version 同内容 no-op，
// 不同内容返回 ErrVersionPinned 并整批拒绝（§10 "拒绝整批"）。
func (r *Registry) Scan(ctx context.Context, dir string) (SyncDiff, error) {
	diff := SyncDiff{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return diff, fmt.Errorf("registry: read testers dir %q: %w", dir, err)
	}
	found := make(map[string]Record)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		rec, err := loadDir(sub)
		if err != nil {
			return diff, err
		}
		if rec.Spec.Name != e.Name() {
			return diff, &ValidationError{
				Name: e.Name(),
				Errors: []FieldError{{
					Path:    "name",
					Message: fmt.Sprintf("name %q must equal directory name %q", rec.Spec.Name, e.Name()),
				}},
			}
		}
		found[Key(rec.Spec.Name, rec.Spec.Version)] = rec
	}

	// 顺序遍历，保证 diff 稳定（便于 CI 断言）。
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		rec := found[k]
		if err := r.apply(ctx, rec, &diff); err != nil {
			return diff, err
		}
	}

	// 目录里消失的只标记 orphaned，不自动删（§10）。
	r.mu.Lock()
	for k, existing := range r.records {
		if _, ok := found[k]; ok {
			continue
		}
		existing.Orphaned = true
		r.records[k] = existing
		diff.Removed = append(diff.Removed, k)
	}
	r.mu.Unlock()
	sort.Strings(diff.Removed)
	return diff, nil
}

// apply 把一条扫描结果合并进内存与存储。
func (r *Registry) apply(ctx context.Context, rec Record, diff *SyncDiff) error {
	key := Key(rec.Spec.Name, rec.Spec.Version)

	r.mu.RLock()
	existing, ok := r.records[key]
	r.mu.RUnlock()
	switch {
	case ok && existing.ContentSHA == rec.ContentSHA:
		diff.Unchanged = append(diff.Unchanged, key)
	case ok && existing.ContentSHA != rec.ContentSHA:
		if !r.opts.AllowContentChange {
			return fmt.Errorf("%w: %s (content sha %s -> %s); bump the version",
				ErrVersionPinned, key, existing.ContentSHA, rec.ContentSHA)
		}
		diff.Changed = append(diff.Changed, key)
	default:
		if r.store != nil {
			stored, err := r.store.GetTester(ctx, rec.Spec.Name, rec.Spec.Version)
			if err == nil && stored.ContentSHA != rec.ContentSHA && !r.opts.AllowContentChange {
				return fmt.Errorf("%w: %s (stored %s, on disk %s); bump the version",
					ErrVersionPinned, key, stored.ContentSHA, rec.ContentSHA)
			}
		}
		diff.Added = append(diff.Added, key)
	}

	if r.store != nil {
		if _, err := r.store.UpsertTester(ctx, rec); err != nil {
			return fmt.Errorf("registry: persist %s: %w", key, err)
		}
	}
	r.put(rec)
	return nil
}

// put 更新内存快照与 latest 索引。
func (r *Registry) put(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[Key(rec.Spec.Name, rec.Spec.Version)] = rec
	if cur, ok := r.latest[rec.Spec.Name]; !ok || versionLess(cur, rec.Spec.Version) {
		r.latest[rec.Spec.Name] = rec.Spec.Version
	}
}

// Register 校验并注册一份上传的 tester.yaml（POST /v1/testers）。
// 上传内容没有 run.sh，因此指纹只覆盖 yaml 本身。
func (r *Registry) Register(ctx context.Context, raw []byte) (domain.TesterSpec, error) {
	spec, err := Parse(raw)
	if err != nil {
		return domain.TesterSpec{}, err
	}
	rec := Record{
		Spec:       spec,
		ContentSHA: contentSHA(raw, nil),
		SourceFile: "upload:" + spec.Key(),
	}
	key := spec.Key()

	r.mu.RLock()
	existing, ok := r.records[key]
	r.mu.RUnlock()
	if ok && existing.ContentSHA == rec.ContentSHA {
		// 幂等：同内容重复上传 no-op（§10 "同步幂等"）。
		return existing.Spec, nil
	}
	if ok && existing.ContentSHA != rec.ContentSHA && !r.opts.AllowContentChange {
		return domain.TesterSpec{}, fmt.Errorf("%w: %s; bump the version", ErrVersionPinned, key)
	}
	if r.store != nil {
		if _, err := r.store.UpsertTester(ctx, rec); err != nil {
			return domain.TesterSpec{}, fmt.Errorf("registry: persist %s: %w", key, err)
		}
	}
	r.put(rec)
	r.opts.Logf("registry: registered %s (source=%s)", key, rec.SourceFile)
	return spec, nil
}

// Resolve 按 name+version 取 TesterSpec；version 为空取最新。
// 找不到返回包装 domain.ErrNotFound 的错误（api 侧据此映射 404）。
func (r *Registry) Resolve(ctx context.Context, name, version string) (domain.TesterSpec, error) {
	if name == "" {
		return domain.TesterSpec{}, fmt.Errorf("registry: resolve: empty name: %w", domain.ErrNotFound)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if version == "" {
		version = r.latest[name]
		if version == "" {
			return domain.TesterSpec{}, fmt.Errorf("registry: tester %q: %w", name, domain.ErrNotFound)
		}
	}
	rec, ok := r.records[Key(name, version)]
	if !ok {
		return domain.TesterSpec{}, fmt.Errorf("registry: tester %s@%s: %w", name, version, domain.ErrNotFound)
	}
	return rec.Spec, nil
}

// List 列出注册表；role 非空时按 role 过滤，includeOrphaned=false 时隐藏已下线版本。
func (r *Registry) List(ctx context.Context, role string, includeOrphaned bool) ([]domain.TesterSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.TesterSpec, 0, len(r.records))
	for _, rec := range r.records {
		if rec.Orphaned && !includeOrphaned {
			continue
		}
		if role != "" && rec.Spec.Role != role {
			continue
		}
		out = append(out, rec.Spec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return versionLess(out[j].Version, out[i].Version)
	})
	return out, nil
}

// Len 返回内存快照的条目数（健康检查与测试用）。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.records)
}

// Load 预热内存快照：先扫描目录（自发现），再从 Store 补齐历史版本（重启后 Resolve 仍可解析旧版本）。
func (r *Registry) Load(ctx context.Context, dir string) (SyncDiff, error) {
	var diff SyncDiff
	if dir != "" {
		var err error
		diff, err = r.Scan(ctx, dir)
		if err != nil {
			return diff, err
		}
	}
	if r.store == nil {
		return diff, nil
	}
	recs, err := r.store.ListTesters(ctx)
	if err != nil {
		return diff, fmt.Errorf("registry: load: %w", err)
	}
	for _, rec := range recs {
		if _, ok := r.records[Key(rec.Spec.Name, rec.Spec.Version)]; ok {
			continue
		}
		r.put(rec)
	}
	return diff, nil
}

// RunRescan 周期重扫目录（自发现）。interval <= 0 时只扫一次即返回。
//
// 注意：K-3 的默认口径是"启动时扫描一次、不做运行时更新"；这里提供的是运维兜底，
// 由调用方决定是否开启（例如灰度环境热更新 tester.yaml）。内容变化的判定仍走
// Scan 的同版本拦截逻辑，不会静默覆盖。
func (r *Registry) RunRescan(ctx context.Context, dir string, interval time.Duration) error {
	if interval <= 0 {
		_, err := r.Scan(ctx, dir)
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			diff, err := r.Scan(ctx, dir)
			if err != nil {
				r.opts.Logf("registry: rescan %s failed: %v", dir, err)
				continue
			}
			if len(diff.Added)+len(diff.Changed)+len(diff.Removed) > 0 {
				r.opts.Logf("registry: rescan %s: +%d ~%d -%d",
					dir, len(diff.Added), len(diff.Changed), len(diff.Removed))
			}
		}
	}
}

// ---- 解析 ----------------------------------------------------------------

// testerYAML 是 tester.yaml 的解析 DTO。容忍未知字段（yaml.v3 默认忽略），
// 与附录 C 的字段差异不会导致注册失败。
type testerYAML struct {
	Name        string              `yaml:"name"`
	Version     string              `yaml:"version"`
	DisplayName string              `yaml:"display_name"`
	Description string              `yaml:"description"`
	Role        string              `yaml:"role"`
	OnFailure   string              `yaml:"on_failure"`
	Entry       string              `yaml:"entry"`
	Image       string              `yaml:"image"`
	Resources   resourcesYAML       `yaml:"resources"`
	Concurrency concurrencyYAML     `yaml:"concurrency"`
	Mounts      []mountYAML         `yaml:"mounts"`
	Parameters  map[string]paramYAML `yaml:"parameters"`
	Placeholder string              `yaml:"placeholder"`
	Env         map[string]envYAML  `yaml:"env"`
	K8s         *K8sSection         `yaml:"k8s"`
}

type resourcesYAML struct {
	CPU    string  `yaml:"cpu"`
	Memory string  `yaml:"memory"`
	GPU    gpuYAML `yaml:"gpu"`
}

type gpuYAML struct {
	Resource string `yaml:"resource"`
	Count    int    `yaml:"count"`
}

type concurrencyYAML struct {
	EndpointExclusive bool `yaml:"endpoint_exclusive"`
}

// mountYAML 同时接受 path 与 mount_path 两个键（lead 口径）。
type mountYAML struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	MountPath string `yaml:"mount_path"`
	ReadOnly  bool   `yaml:"read_only"`
	Source    string `yaml:"source"`
	Required  *bool  `yaml:"required"`
}

type paramYAML struct {
	Type        string `yaml:"type"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

type envYAML struct {
	Required    bool   `yaml:"required"`
	Secret      bool   `yaml:"secret"`
	Description string `yaml:"description"`
}

// Parse 解析并校验一份 tester.yaml 字节内容。
func Parse(raw []byte) (domain.TesterSpec, error) {
	var doc testerYAML
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return domain.TesterSpec{}, &ValidationError{Errors: []FieldError{{Path: "", Message: "yaml: " + err.Error()}}}
	}
	// 明确拒绝附录 C 禁止字段：先按通用 map 检查一遍键。
	var generic map[string]any
	_ = yaml.Unmarshal(raw, &generic)
	var issues []FieldError
	for _, f := range forbiddenFields {
		if _, ok := generic[f]; ok {
			issues = append(issues, FieldError{Path: f, Message: "field is not allowed in tester.yaml (appendix C)"})
		}
	}

	spec := domain.TesterSpec{
		Name:        strings.TrimSpace(doc.Name),
		Version:     strings.TrimSpace(doc.Version),
		DisplayName: strings.TrimSpace(doc.DisplayName),
		Description: doc.Description,
		Role:        strings.TrimSpace(doc.Role),
		OnFailure:   strings.TrimSpace(doc.OnFailure),
		Entry:       strings.TrimSpace(doc.Entry),
		Image:       strings.TrimSpace(doc.Image),
		Placeholder: doc.Placeholder,
		Resources: domain.ResourceRequest{
			CPU:    strings.TrimSpace(doc.Resources.CPU),
			Memory: strings.TrimSpace(doc.Resources.Memory),
			GPU: domain.GPURequest{
				Resource: strings.TrimSpace(doc.Resources.GPU.Resource),
				Count:    doc.Resources.GPU.Count,
			},
		},
		Concurrency: domain.ConcurrencyPolicy{EndpointExclusive: doc.Concurrency.EndpointExclusive},
		Parameters:  make(map[string]domain.Param, len(doc.Parameters)),
		Env:         make(map[string]domain.EnvVar, len(doc.Env)),
	}
	for name, p := range doc.Parameters {
		spec.Parameters[name] = domain.Param{
			Type: p.Type, Required: p.Required, Default: p.Default, Description: p.Description,
		}
	}
	for name, e := range doc.Env {
		spec.Env[name] = domain.EnvVar{Required: e.Required, Secret: e.Secret, Description: e.Description}
	}
	for _, m := range doc.Mounts {
		mountPath := m.MountPath
		if mountPath == "" {
			mountPath = m.Path
		}
		spec.Mounts = append(spec.Mounts, domain.MountSpec{
			Name: m.Name, MountPath: mountPath, ReadOnly: m.ReadOnly, Source: m.Source,
		})
	}

	issues = append(issues, ValidateSpec(spec, doc)...)
	if len(issues) > 0 {
		return domain.TesterSpec{}, &ValidationError{Name: spec.Key(), Errors: issues}
	}
	return spec, nil
}

// ValidateSpec 校验 §10 的业务规则（schema 之外的部分）。
func ValidateSpec(spec domain.TesterSpec, raw testerYAML) []FieldError {
	var issues []FieldError
	add := func(path, msg string) { issues = append(issues, FieldError{Path: path, Message: msg}) }

	if !namePattern.MatchString(spec.Name) {
		add("name", "must match ^[a-z0-9-]{1,63}$")
	}
	if !versionPattern.MatchString(spec.Version) {
		add("version", "must be a semantic version, e.g. 1.0.0")
	}
	if spec.Image == "" {
		add("image", "is required")
	}
	if spec.Entry != RequiredEntry {
		add("entry", "must be "+RequiredEntry)
	}
	switch spec.Role {
	case domain.RoleTest, domain.RolePreCheck:
	default:
		add("role", "must be test or pre_check")
	}
	switch spec.OnFailure {
	case domain.OnFailureContinue, domain.OnFailureAbortRun:
	default:
		add("on_failure", "must be continue or abort_run")
	}
	if spec.Role == domain.RolePreCheck && spec.OnFailure != domain.OnFailureAbortRun {
		add("on_failure", "role=pre_check must declare on_failure: abort_run")
	}
	if spec.Role == domain.RolePreCheck && !spec.Concurrency.EndpointExclusive {
		add("concurrency.endpoint_exclusive", "role=pre_check must be true")
	}
	if spec.Resources.CPU == "" {
		add("resources.cpu", "is required")
	}
	if spec.Resources.Memory == "" {
		add("resources.memory", "is required")
	}
	if spec.Resources.GPU.Count < 0 {
		add("resources.gpu.count", "must be >= 0")
	}
	if spec.Parameters == nil {
		add("parameters", "must be present (may be an empty object)")
	} else {
		for name, p := range spec.Parameters {
			if strings.TrimSpace(p.Description) == "" {
				add("parameters."+name+".description", "is required")
			}
		}
	}
	for name, e := range spec.Env {
		if strings.TrimSpace(e.Description) == "" {
			add("env."+name+".description", "is required")
		}
	}

	// 挂载：绝对路径 + 不与平台保留路径冲突 + 互不前缀重叠（§3.4、§10 校验项 11）。
	seen := map[string]struct{}{}
	for i, m := range spec.Mounts {
		path := "mounts[" + strconv.Itoa(i) + "]"
		if m.MountPath == "" {
			add(path+".mount_path", "is required")
			continue
		}
		if !strings.HasPrefix(m.MountPath, "/") {
			add(path+".mount_path", "must be an absolute path")
		}
		if why, reserved := reservedMountPaths[m.MountPath]; reserved {
			add(path+".mount_path", "conflicts with platform path ("+why+")")
		}
		if _, dup := seen[m.MountPath]; dup {
			add(path+".mount_path", "duplicate mount path")
		}
		seen[m.MountPath] = struct{}{}
		for other := range seen {
			if other == m.MountPath {
				continue
			}
			if strings.HasPrefix(strings.TrimRight(m.MountPath, "/")+"/", strings.TrimRight(other, "/")+"/") {
				add(path+".mount_path", "must not be nested under "+other)
			}
		}
	}

	// k8s 白名单（§10 校验项 8）：只允许四个键，且 security_context 只允许两个键。
	if raw.K8s != nil {
		for key := range raw.K8s.SecurityContext {
			switch key {
			case "run_as_user", "fs_group":
			default:
				add("k8s.security_context."+key, "is not allowed")
			}
		}
	}
	var genericK8s map[string]any
	if b, err := yaml.Marshal(raw.K8s); err == nil && len(b) > 0 {
		_ = yaml.Unmarshal(b, &genericK8s)
	}
	for key := range genericK8s {
		switch key {
		case "node_selector", "tolerations", "annotations", "security_context":
		default:
			add("k8s."+key, "is not allowed (only node_selector/tolerations/annotations/security_context)")
		}
	}
	return issues
}

// loadDir 读取一个 Tester 目录（tester.yaml + run.sh），做目录级校验。
func loadDir(dir string) (Record, error) {
	manifestPath := filepath.Join(dir, TesterFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Record{}, fmt.Errorf("registry: read %s: %w", manifestPath, err)
	}
	spec, err := Parse(raw)
	if err != nil {
		return Record{}, err
	}

	runPath := filepath.Join(dir, RunScriptName)
	runBytes, err := os.ReadFile(runPath)
	if err != nil {
		return Record{}, &ValidationError{
			Name:   spec.Key(),
			Errors: []FieldError{{Path: RunScriptName, Message: "run.sh is required"}},
		}
	}
	if st, statErr := os.Stat(runPath); statErr == nil && !runOnWindows() && st.Mode().Perm()&0o111 == 0 {
		return Record{}, &ValidationError{
			Name:   spec.Key(),
			Errors: []FieldError{{Path: RunScriptName, Message: "run.sh must be executable"}},
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return Record{}, fmt.Errorf("registry: read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, bad := forbiddenManifests[strings.ToLower(e.Name())]; bad {
			return Record{}, &ValidationError{
				Name:   spec.Key(),
				Errors: []FieldError{{Path: e.Name(), Message: "K8s manifests must not live in a tester directory"}},
			}
		}
	}

	return Record{
		Spec:       spec,
		ContentSHA: contentSHA(raw, runBytes),
		SourceFile: manifestPath,
	}, nil
}

// contentSHA 是"规范化 tester.yaml + run.sh 字节"的指纹（§10）：行尾统一成 \n，
// 避免 Windows/Linux checkout 造成假变更。
func contentSHA(manifest, runScript []byte) string {
	h := sha256.New()
	h.Write([]byte(normalizeText(manifest)))
	h.Write([]byte{0})
	h.Write([]byte(normalizeText(runScript)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func normalizeText(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// runOnWindows 让"可执行位"校验在 Windows 上跳过：NTFS 不存 POSIX 执行位，
// 镜像内是 Linux，这条校验在 Linux/CI 上才有效（§10 校验项 14）。
func runOnWindows() bool { return runtime.GOOS == "windows" }

// versionLess 比较两个语义化版本（a < b）。
func versionLess(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return a < b
}

func parseVersion(v string) [3]int {
	core := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(core, ".", 3) {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out
}
