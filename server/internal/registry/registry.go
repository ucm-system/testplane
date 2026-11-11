// Package registry 实现 Runner 注册表（ARCHITECTURE §5.6、design/impl/execution.md）。
//
// 职责：启动时扫描 runners/*/runner.yaml → 解析成 domain.RunnerSpec → 校验（v2 规则的
// 结构性部分）→ 与库同步（同 name+version 同内容 no-op，不同内容拒绝，拒绝启动）。
// 另外对外提供 Resolve / List 给 API 层，以及输入校验与 endpoint_key（C-8.4/C-8.5）。
//
// 边界：注册表不构建镜像、不渲染表单、不做输入语义校验（§5.4：范围/枚举/跨字段归 Runner）。
//
// v2 裁决（附录 C / D-66～D-71）：
//   - entry 是 **exec form 数组**（不经 shell、不追加 argv，§5.2）；
//   - input_format: jsonl|yaml|text 声明校验深度（D-67）；
//   - role 四值 test|pre_check|deploy|clean，**deploy/clean 仅内置**（§5.1，用户注册拒绝）；
//   - on_failure 枚举 continue|abort_task；pre_check 与内置 deploy/clean 必须 abort_task（§7.3）；
//   - parameters 仅 jsonl（yaml/text 必须 []），平台校验 type/required（D-67）；
//   - exports/consumes 声明下游/上游 key（§5.12）。
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
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 校验错误与哨兵错误。对外 code 用 domain.Code*（附录 D）。
var (
	// ErrInvalid 表示 runner.yaml 没通过校验（RUNNER_INVALID）。
	ErrInvalid = errors.New("registry: invalid runner")
	// ErrVersionPinned 表示同 name+version 内容不同（RUNNER_VERSION_PINNED，K-3）。
	ErrVersionPinned = errors.New("registry: same runner version has different content")
	// ErrForbiddenField 表示出现了附录 C 明令禁止的字段。
	ErrForbiddenField = errors.New("registry: forbidden field")
	// ErrForbiddenRole 表示用户目录声明了 role: deploy / role: clean（§5.1 仅内置）。
	ErrForbiddenRole = errors.New("registry: deploy/clean role is built-in only")
)

// FieldError 是字段级校验错误（§5.6 的"字段级错误"）。
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
	// RunnerFileName 是注册表唯一识别的文件名（§5.5：只认 Runner 根目录下的 runner.yaml）。
	RunnerFileName = "runner.yaml"
	// BuiltinDeployName 是内置 deploy runner（Genet/fake-genet 接入点）。
	BuiltinDeployName = "genet-deploy"
	// BuiltinMockDeployName 是 mock 薄壳（同 genet-deploy 脚本；AG_GENET_BASE_URL 的
	// runner.yaml env default 指向 fake-genet，Task env 可覆盖——平台不再按 provider 特判）。
	BuiltinMockDeployName = "mock-deploy"
	// BuiltinCleanName 是内置 clean runner（genet-deploy 的 clean_runner 配对）。
	BuiltinCleanName = "genet-clean"
	// BuiltinServiceDeployName / BuiltinServiceCleanName 是改名期的内置 deploy/clean runner
	//（runners/service-*；与 genet-* 同一契约，迁移期两套名字并存）。
	BuiltinServiceDeployName = "service-deploy"
	BuiltinServiceCleanName  = "service-clean"
	// existing-deploy 同为内置 deploy 变体（已有服务登记，不声明 clean_runner → 无 clean 阶段）。
	// BuiltinPreCheckName 是内置探活 Runner（§5.1）。
	BuiltinPreCheckName = "endpoint-probe"
)

// BuiltinRunnerNames 返回内置 Runner 名集合（随平台发版，§12.1）。
// deploy 变体：genet-deploy、mock-deploy（薄壳）、service-deploy（改名期并存）与
// existing-deploy（已有服务登记）；clean 变体：genet-clean、service-clean（由 deploy
// runner 的 clean_runner 声明配对）。
func BuiltinRunnerNames() map[string]bool {
	return map[string]bool{
		BuiltinDeployName:        true, // genet-deploy
		BuiltinMockDeployName:    true, // mock-deploy
		BuiltinCleanName:         true, // genet-clean
		BuiltinServiceDeployName: true, // service-deploy
		BuiltinServiceCleanName:  true, // service-clean
		BuiltinPreCheckName:      true,
		"existing-deploy":        true,
	}
}

var (
	// namePattern：小写 [a-z0-9-]，≤63（附录 C）。
	namePattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	// versionPattern：语义化版本（附录 C）。
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	// 平台保留的挂载点。唯一来源是 domain；统一共享挂载（D-63）的路径也在其中
	//（由调用方传入 shared 列表时合入）。
	reservedMountPaths = domain.ReservedMountPaths(nil)
	// 禁止出现的 K8s 清单（§6.8）。
	forbiddenManifests = map[string]struct{}{
		"job.yaml":           {},
		"job.yml":            {},
		"deployment.yaml":    {},
		"deployment.yml":     {},
		"pod.yaml":           {},
		"pod.yml":            {},
		"service.yaml":       {},
		"service.yml":        {},
		"ingress.yaml":       {},
		"configmap.yaml":     {},
		"pvc.yaml":           {},
		"networkpolicy.yaml": {},
	}
	// 禁止出现的 runner.yaml 顶层字段（附录 C：timeout_seconds/outputs/requires 均"禁止"）。
	forbiddenFields = []string{"timeout_seconds", "outputs", "requires", "executors", "retry"}
)

// K8sSection 是 runner.yaml 里允许的 k8s 白名单（§6.8 受限声明字段）。
type K8sSection struct {
	NodeSelector    map[string]string `yaml:"node_selector" json:"node_selector"`
	Tolerations     []map[string]any  `yaml:"tolerations" json:"tolerations"`
	Annotations     map[string]string `yaml:"annotations" json:"annotations"`
	SecurityContext map[string]any    `yaml:"security_context" json:"security_context"`
}

// Record 是注册表里的一行（spec + 指纹 + 来源）。
type Record struct {
	Spec       domain.RunnerSpec `json:"spec"`
	ContentSHA string            `json:"content_sha"`
	SourceFile string            `json:"source_file"`
	Orphaned   bool              `json:"orphaned"`
}

// Store 是注册表对持久化的唯一依赖（由 store.RunnerRegistry 适配）。
// 实现者按 (name, version) upsert；content_sha 不同视为"同版本不同内容"。
type Store interface {
	// UpsertRunner 在一个事务内写一行；created 表示是否新增。
	UpsertRunner(ctx context.Context, rec Record) (created bool, err error)
	// GetRunner 读一行；version 为空表示取最高版本。未找到返回包装 domain.ErrNotFound 的错误。
	GetRunner(ctx context.Context, name, version string) (Record, error)
	// ListRunners 列出全部；role 为空不过滤。
	ListRunners(ctx context.Context) ([]Record, error)
	// MarkRunnerOrphaned 把 (name, version) 标记为 retired/orphaned（**只标记，不删除**）。
	// 必须在 Scan 里落库：否则进程重启后从库里读回来的行 orphaned=false，
	// `retired` 会永远为 false（§5.6.1 / 票 #60 真机回归）。
	MarkRunnerOrphaned(ctx context.Context, name, version string) error
}

// Options 是注册表配置。
type Options struct {
	// Builtins 是内置 Runner 名集合（deploy/clean 仅内置，§5.1）；nil 视为全内置名单。
	Builtins map[string]bool
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
	if len(o.Builtins) == 0 {
		o.Builtins = BuiltinRunnerNames()
	}
	return o
}

// IsBuiltin 报告该 Runner 名是否内置（deploy/clean 角色只对内置名单放行）。
func (o Options) IsBuiltin(name string) bool { return o.withDefaults().Builtins[name] }

// SyncDiff 是一次扫描的结果。
type SyncDiff struct {
	Added     []string `json:"added"`
	Unchanged []string `json:"unchanged"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
}

// Registry 是 Runner 注册表。内存快照服务于热路径（Resolve/List），
// 持久化通过 Store 完成；启动扫描失败即拒绝启动（K-3：不做运行时更新）。
type Registry struct {
	mu      sync.RWMutex
	records map[string]Record // key = name@version
	latest  map[string]string // name -> 最高 version
	store   Store
	opts    Options
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

// Scan 扫描 dir 下所有 runners/*/runner.yaml，校验并同步进注册表。
//
// 任何一条不合法都返回错误（调用方应当拒绝启动）。同 name+version 同内容 no-op，
// 不同内容返回 ErrVersionPinned 并整批拒绝（§5.6）。role=deploy/clean 只对内置名单放行。
func (r *Registry) Scan(ctx context.Context, dir string) (SyncDiff, error) {
	diff := SyncDiff{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return diff, fmt.Errorf("registry: read runners dir %q: %w", dir, err)
	}
	found := make(map[string]Record)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		rec, err := loadDir(sub, r.opts.IsBuiltin(e.Name()))
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

	// clean_runner 配对校验（deploy/clean 解耦裁决）：声明了 clean_runner 的 runner，
	// 其指向必须是本批扫描（或已在注册表中）的 role=clean runner，否则整批拒绝启动。
	if err := r.checkCleanRunnerPairs(found); err != nil {
		return diff, err
	}

	for _, k := range keys {
		rec := found[k]
		if err := r.apply(ctx, rec, &diff); err != nil {
			return diff, err
		}
	}

	// 目录里消失的只标记 orphaned，不自动删（§5.6.1）。
	//
	// **必须落库**：只改内存的话，进程重启后从库里读回来的行 orphaned 仍是 false，
	// `retired` 标志退化成恒 false（票 #60 真机回归）。
	// 顺序：先写库，成功后再改内存 —— 写库失败时内存保持原样，天然不会与库分叉。
	r.mu.RLock()
	removed := make([]Record, 0, len(r.records))
	for k, existing := range r.records {
		if _, ok := found[k]; ok {
			continue
		}
		removed = append(removed, existing)
	}
	r.mu.RUnlock()
	sort.Slice(removed, func(i, j int) bool {
		if removed[i].Spec.Name != removed[j].Spec.Name {
			return removed[i].Spec.Name < removed[j].Spec.Name
		}
		return removed[i].Spec.Version < removed[j].Spec.Version
	})
	for _, rec := range removed {
		key := Key(rec.Spec.Name, rec.Spec.Version)
		if r.store != nil {
			if err := r.store.MarkRunnerOrphaned(ctx, rec.Spec.Name, rec.Spec.Version); err != nil {
				// 内存一个字节都没动 → 返回错误即可，不会出现"内存说 retired、库里不是"。
				return diff, fmt.Errorf("registry: mark orphaned %s: %w", key, err)
			}
		}
		r.mu.Lock()
		if cur, ok := r.records[key]; ok {
			cur.Orphaned = true
			r.records[key] = cur
		}
		r.mu.Unlock()
		diff.Removed = append(diff.Removed, key)
	}
	sort.Strings(diff.Removed)
	return diff, nil
}

// checkCleanRunnerPairs 校验 clean_runner 声明闭合：每个非空 CleanRunner 必须命中一个
// role=clean 的 runner（先看本批扫描结果，再看注册表内存快照——后者覆盖"clean runner
// 已在库、本次目录没有"的场景）。命中但角色不对 / 找不到 → ValidationError（注册拒绝）。
func (r *Registry) checkCleanRunnerPairs(found map[string]Record) error {
	isCleanRunner := func(name string) (bool, bool) { // (found, roleOK)
		for _, rec := range found {
			if rec.Spec.Name == name {
				return true, rec.Spec.Role == domain.RoleClean
			}
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
		for _, rec := range r.records {
			if rec.Spec.Name == name {
				return true, rec.Spec.Role == domain.RoleClean
			}
		}
		return false, false
	}
	names := make([]string, 0, len(found))
	for _, rec := range found {
		names = append(names, rec.Spec.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, rec := range found {
			if rec.Spec.Name != name || rec.Spec.CleanRunner == "" {
				continue
			}
			ok, roleOK := isCleanRunner(rec.Spec.CleanRunner)
			if !ok {
				return &ValidationError{
					Name: rec.Spec.Key(),
					Errors: []FieldError{{
						Path:    "clean_runner",
						Message: fmt.Sprintf("clean_runner %q is not a registered runner (clean pairing must resolve to a role=clean runner)", rec.Spec.CleanRunner),
					}},
				}
			}
			if !roleOK {
				return &ValidationError{
					Name: rec.Spec.Key(),
					Errors: []FieldError{{
						Path:    "clean_runner",
						Message: fmt.Sprintf("clean_runner %q has role != clean (must pair with a role=clean runner)", rec.Spec.CleanRunner),
					}},
				}
			}
		}
	}
	return nil
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
			stored, err := r.store.GetRunner(ctx, rec.Spec.Name, rec.Spec.Version)
			if err == nil && stored.ContentSHA != rec.ContentSHA && !r.opts.AllowContentChange {
				return fmt.Errorf("%w: %s (stored %s, on disk %s); bump the version",
					ErrVersionPinned, key, stored.ContentSHA, rec.ContentSHA)
			}
		}
		diff.Added = append(diff.Added, key)
	}

	if r.store != nil {
		if _, err := r.store.UpsertRunner(ctx, rec); err != nil {
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

// Register 校验并注册一份上传的 runner.yaml（管理面维护路径；deploy/clean 角色拒绝）。
// 上传内容没有 entry 文件，因此指纹只覆盖 yaml 本身。
func (r *Registry) Register(ctx context.Context, raw []byte) (domain.RunnerSpec, error) {
	return r.registerRaw(ctx, raw, false, "upload:")
}

// RegisterForTest 是测试用的注册入口（不走目录扫描）。
func (r *Registry) RegisterForTest(raw []byte) (domain.RunnerSpec, error) {
	return r.registerRaw(context.Background(), raw, false, "test:")
}

// registerRaw 解析、校验并写入注册表（幂等：同内容重复注册 no-op）。
func (r *Registry) registerRaw(ctx context.Context, raw []byte, builtin bool, srcPrefix string) (domain.RunnerSpec, error) {
	spec, err := parseWithBuiltin(raw, builtin)
	if err != nil {
		return domain.RunnerSpec{}, err
	}
	rec := Record{
		Spec:       spec,
		ContentSHA: contentSHA(raw, nil),
		SourceFile: srcPrefix + spec.Key(),
	}
	key := spec.Key()

	r.mu.RLock()
	existing, ok := r.records[key]
	r.mu.RUnlock()
	if ok && existing.ContentSHA == rec.ContentSHA {
		// 幂等：同内容重复注册 no-op。
		return existing.Spec, nil
	}
	if ok && existing.ContentSHA != rec.ContentSHA && !r.opts.AllowContentChange {
		return domain.RunnerSpec{}, fmt.Errorf("%w: %s; bump the version", ErrVersionPinned, key)
	}
	if r.store != nil {
		if _, err := r.store.UpsertRunner(ctx, rec); err != nil {
			return domain.RunnerSpec{}, fmt.Errorf("registry: persist %s: %w", key, err)
		}
	}
	r.put(rec)
	r.opts.Logf("registry: registered %s (source=%s)", key, rec.SourceFile)
	return spec, nil
}

// Resolve 按 name+version 取 RunnerSpec；version 为空取最高版本。
// 找不到返回包装 domain.ErrNotFound 的错误（api 侧据此映射 404）。
func (r *Registry) Resolve(ctx context.Context, name, version string) (domain.RunnerSpec, error) {
	if name == "" {
		return domain.RunnerSpec{}, fmt.Errorf("registry: resolve: empty name: %w", domain.ErrNotFound)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if version == "" {
		version = r.latest[name]
		if version == "" {
			return domain.RunnerSpec{}, fmt.Errorf("registry: runner %q: %w", name, domain.ErrNotFound)
		}
	}
	rec, ok := r.records[Key(name, version)]
	if !ok {
		return domain.RunnerSpec{}, fmt.Errorf("registry: runner %s@%s: %w", name, version, domain.ErrNotFound)
	}
	return rec.Spec, nil
}

// List 列出注册表；role 非空时按 role 过滤，includeOrphaned=false 时隐藏已下线版本。
func (r *Registry) List(ctx context.Context, role string, includeOrphaned bool) ([]domain.RunnerSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.RunnerSpec, 0, len(r.records))
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
	// **先水合、再扫描**：Scan 判定"目录里已消失的版本"依赖 r.records 里已有的行。
	// 先 Scan 再 ListRunners 的话 records 还是空的，removed 恒为空 —— 只存在于库里的
	// 旧版本永远不会被标 orphaned/retired（真机回归）。
	if r.store != nil {
		recs, err := r.store.ListRunners(ctx)
		if err != nil {
			return diff, fmt.Errorf("registry: load: %w", err)
		}
		for _, rec := range recs {
			r.put(rec)
		}
	}
	if dir == "" {
		return diff, nil
	}
	var err error
	diff, err = r.Scan(ctx, dir)
	if err != nil {
		return diff, err
	}
	return diff, nil
}

// RunRescan 周期重扫目录（运维兜底）。interval <= 0 时只扫一次即返回。
//
// 注意：K-3 的默认口径是"启动时扫描一次、不做运行时更新"；这里提供的是运维兜底，
// 由调用方决定是否开启。内容变化的判定仍走 Scan 的同版本拦截逻辑，不会静默覆盖。
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

// runnerYAML 是 runner.yaml 的解析 DTO。容忍未知字段（yaml.v3 默认忽略），
// 与附录 C 的字段差异不会导致注册失败。
type runnerYAML struct {
	Name        string             `yaml:"name"`
	Version     string             `yaml:"version"`
	DisplayName string             `yaml:"display_name"`
	Description string             `yaml:"description"`
	Role        string             `yaml:"role"`
	CleanRunner string             `yaml:"clean_runner"`
	OnFailure   string             `yaml:"on_failure"`
	Entry       []string           `yaml:"entry"`
	InputFormat string             `yaml:"input_format"`
	Image       string             `yaml:"image"`
	Resources   resourcesYAML      `yaml:"resources"`
	Concurrency concurrencyYAML    `yaml:"concurrency"`
	Mounts      []mountYAML        `yaml:"mounts"`
	Parameters  []paramYAML        `yaml:"parameters"`
	Exports     []string           `yaml:"exports"`
	Consumes    []string           `yaml:"consumes"`
	Placeholder string             `yaml:"placeholder"`
	Env         map[string]envYAML `yaml:"env"`
	K8s         *K8sSection        `yaml:"k8s"`
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

// mountYAML 同时接受 path 与 mount_path 两个键。
type mountYAML struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	MountPath string `yaml:"mount_path"`
	ReadOnly  bool   `yaml:"read_only"`
	Source    string `yaml:"source"`
}

type paramYAML struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

type envYAML struct {
	Required    bool   `yaml:"required"`
	Secret      bool   `yaml:"secret"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

// Parse 解析并校验一份 runner.yaml 字节内容（不含目录级检查：entry 文件存在性由 loadDir 做）。
func Parse(raw []byte) (domain.RunnerSpec, error) {
	return parseWithBuiltin(raw, false)
}

// ParseBuiltin 解析内置 Runner 的 runner.yaml（deploy/clean 角色只对内置放行）。
func ParseBuiltin(raw []byte) (domain.RunnerSpec, error) {
	return parseWithBuiltin(raw, true)
}

func parseWithBuiltin(raw []byte, builtin bool) (domain.RunnerSpec, error) {
	var doc runnerYAML
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return domain.RunnerSpec{}, &ValidationError{Errors: []FieldError{{Path: "", Message: "yaml: " + err.Error()}}}
	}
	// 明确拒绝附录 C 禁止字段：先按通用 map 检查一遍键。
	var generic map[string]any
	_ = yaml.Unmarshal(raw, &generic)
	var issues []FieldError
	for _, f := range forbiddenFields {
		if _, ok := generic[f]; ok {
			issues = append(issues, FieldError{Path: f, Message: "field is not allowed in runner.yaml (appendix C)"})
		}
	}

	spec := domain.RunnerSpec{
		Name:        strings.TrimSpace(doc.Name),
		Version:     strings.TrimSpace(doc.Version),
		DisplayName: strings.TrimSpace(doc.DisplayName),
		Description: doc.Description,
		Role:        strings.TrimSpace(doc.Role),
		CleanRunner: strings.TrimSpace(doc.CleanRunner),
		OnFailure:   strings.TrimSpace(doc.OnFailure),
		Entry:       doc.Entry,
		InputFormat: strings.TrimSpace(doc.InputFormat),
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
		Exports:     trimList(doc.Exports),
		Consumes:    trimList(doc.Consumes),
		Env:         make(map[string]domain.EnvVar, len(doc.Env)),
	}
	if spec.InputFormat == "" {
		spec.InputFormat = domain.InputFormatJSONL // 附录 C：jsonl（默认）
	}
	if len(spec.Entry) == 0 {
		if s, ok := generic["entry"]; ok && s == nil {
			// entry 显式为空：留给 ValidateSpec 报错。
		}
	} else {
		spec.Entry = trimList(doc.Entry)
	}
	for _, p := range doc.Parameters {
		spec.Parameters = append(spec.Parameters, domain.Param{
			Name: strings.TrimSpace(p.Name), Type: strings.TrimSpace(p.Type),
			Required: p.Required, Default: p.Default, Description: p.Description,
		})
	}
	for name, e := range doc.Env {
		spec.Env[name] = domain.EnvVar{Required: e.Required, Secret: e.Secret, Default: e.Default, Description: e.Description}
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

	issues = append(issues, ValidateSpec(spec, doc, builtin)...)
	if len(issues) > 0 {
		return domain.RunnerSpec{}, &ValidationError{Name: spec.Key(), Errors: issues}
	}
	return spec, nil
}

// trimList 去除列表项首尾空白并丢弃空项。
func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidateSpec 校验 §5.6 的业务规则（schema 之外的部分）。
// builtin=true 表示这份 yaml 来自内置 Runner 目录（deploy/clean 角色放行）。
func ValidateSpec(spec domain.RunnerSpec, raw runnerYAML, builtin bool) []FieldError {
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
	// entry：exec form 数组，非空、每项非空（§5.2 / §5.6 步骤 2）。
	if len(spec.Entry) == 0 {
		add("entry", "must be a non-empty exec form array, e.g. [\"/runner/run.sh\"]")
	} else {
		for i, e := range spec.Entry {
			if strings.TrimSpace(e) == "" {
				add(fmt.Sprintf("entry[%d]", i), "must not be empty")
			}
		}
		if strings.Contains(spec.EntryString(), " && ") || strings.Contains(spec.EntryString(), "|") {
			add("entry", "must be exec form (no shell operators)")
		}
	}
	if !domain.ValidInputFormat(spec.InputFormat) {
		add("input_format", "must be jsonl|yaml|text (D-67)")
	}
	switch spec.Role {
	case domain.RoleTest, domain.RolePreCheck, domain.RoleDeploy, domain.RoleClean:
	default:
		add("role", "must be test|pre_check|deploy|clean")
	}
	// role 边界（§5.1）：deploy / clean 仅内置，用户注册一律拒绝。
	if (spec.Role == domain.RoleDeploy || spec.Role == domain.RoleClean) && !builtin {
		add("role", "role deploy/clean is built-in only (service-deploy / service-clean); user registration is rejected")
	}
	// clean_runner：clean 配对由 deploy runner 自己声明（deploy/clean 解耦裁决）。
	// 仅 role=deploy 可声明；指向的 runner 必须是 role=clean（跨 runner 校验在 Scan /
	// registerRaw 做，这里只做角色合法性）。clean 角色声明 clean_runner 无意义，拒绝。
	if spec.CleanRunner != "" && spec.Role != domain.RoleDeploy {
		add("clean_runner", fmt.Sprintf("clean_runner is only valid on role=deploy runners (got role=%s)", spec.Role))
	}
	switch spec.OnFailure {
	case domain.OnFailureContinue, domain.OnFailureAbortTask:
	default:
		add("on_failure", "must be continue or abort_task")
	}
	// §7.3：pre_check 与内置 deploy/clean 必须 abort_task（否则拒绝注册）。
	needsAbort := spec.Role == domain.RolePreCheck || spec.Role == domain.RoleDeploy || spec.Role == domain.RoleClean
	if needsAbort && spec.OnFailure != domain.OnFailureAbortTask {
		add("on_failure", fmt.Sprintf("role=%s must declare on_failure: abort_task", spec.Role))
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

	// parameters：仅 jsonl（yaml/text 必须 []，附录 C）；平台校验 type/required 结构（D-67）。
	if spec.InputFormat != domain.InputFormatJSONL {
		if len(spec.Parameters) > 0 {
			add("parameters", fmt.Sprintf("input_format=%s requires parameters: [] (jsonl only, D-67)", spec.InputFormat))
		}
	} else {
		seen := map[string]bool{}
		for i, p := range spec.Parameters {
			path := fmt.Sprintf("parameters[%d]", i)
			if p.Name == "" {
				add(path+".name", "is required")
				continue
			}
			if seen[p.Name] {
				add(path+".name", fmt.Sprintf("duplicate parameter name %q", p.Name))
			}
			seen[p.Name] = true
			if !domain.ValidParamType(p.Type) {
				add(path+".type", fmt.Sprintf("must be one of %s", strings.Join(domain.ParamTypes, "|")))
			}
		}
	}

	// exports / consumes（§5.12）：字符串列表，条目非空；重复 key 拒绝。
	for _, list := range []struct {
		path string
		keys []string
	}{{"exports", spec.Exports}, {"consumes", spec.Consumes}} {
		seen := map[string]bool{}
		for _, k := range list.keys {
			if seen[k] {
				add(list.path, fmt.Sprintf("duplicate key %q (combination-time duplicate keys are rejected, §5.12)", k))
			}
			seen[k] = true
		}
	}

	for name, e := range spec.Env {
		if strings.TrimSpace(e.Description) == "" {
			add("env."+name+".description", "is required")
		}
		if e.Secret && e.Default != "" {
			add("env."+name+".default", "secret env must not carry a default value (§12.4: 敏感值只走 secrets)")
		}
	}

	// 挂载：绝对路径 + 不与平台保留路径冲突 + 互不前缀重叠（§3.4）。
	seen := map[string]struct{}{}
	for i, m := range spec.Mounts {
		path := "mounts[" + strconvItoa(i) + "]"
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

	// k8s 白名单（§6.8）：只允许四个键，且 security_context 只允许两个键。
	if raw.K8s != nil {
		for key := range raw.K8s.SecurityContext {
			switch key {
			case "run_as_user", "fs_group":
			default:
				add("k8s.security_context."+key, "is not allowed")
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
	}
	return issues
}

func strconvItoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// loadDir 读取一个 Runner 目录（runner.yaml + entry 文件），做目录级校验。
// entry 文件的存在性由本目录保证（§5.6 步骤 3：entry 文件存在且具备执行权限；
// 镜像内检查归 CI，平台不做）。
func loadDir(dir string, builtin bool) (Record, error) {
	manifestPath := filepath.Join(dir, RunnerFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Record{}, fmt.Errorf("registry: read %s: %w", manifestPath, err)
	}
	spec, err := parseWithBuiltin(raw, builtin)
	if err != nil {
		return Record{}, err
	}

	var entryErrs []FieldError
	if len(spec.Entry) > 0 {
		// entry[0] 的文件名必须存在于 Runner 根目录（exec form 数组的第一个元素是可执行文件）。
		entryBase := filepath.Base(spec.Entry[0])
		entryPath := filepath.Join(dir, entryBase)
		st, statErr := os.Stat(entryPath)
		if statErr != nil || st.IsDir() {
			entryErrs = append(entryErrs, FieldError{
				Path: "entry[0]", Message: fmt.Sprintf("entry file %q must exist in the runner directory", spec.Entry[0]),
			})
		} else if !runOnWindows() && st.Mode().Perm()&0o111 == 0 {
			entryErrs = append(entryErrs, FieldError{
				Path: "entry[0]", Message: fmt.Sprintf("entry file %q must be executable", spec.Entry[0]),
			})
		}
	}
	var entryBytes []byte
	if len(spec.Entry) > 0 {
		entryBytes, _ = os.ReadFile(filepath.Join(dir, filepath.Base(spec.Entry[0])))
	}
	if len(entryErrs) > 0 {
		return Record{}, &ValidationError{Name: spec.Key(), Errors: entryErrs}
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
				Errors: []FieldError{{Path: e.Name(), Message: "K8s manifests must not live in a runner directory"}},
			}
		}
	}

	return Record{
		Spec:       spec,
		ContentSHA: contentSHA(raw, entryBytes),
		SourceFile: manifestPath,
	}, nil
}

// contentSHA 是"规范化 runner.yaml + entry 文件字节"的指纹（§5.6.1）：行尾统一成 \n，
// 避免 Windows/Linux checkout 造成假变更。
func contentSHA(manifest, entry []byte) string {
	h := sha256.New()
	h.Write([]byte(normalizeText(manifest)))
	h.Write([]byte{0})
	h.Write([]byte(normalizeText(entry)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func normalizeText(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// runOnWindows 让"可执行位"校验在 Windows 上跳过：NTFS 不存 POSIX 执行位，
// 镜像内是 Linux，这条校验在 Linux/CI 上才有效。
func runOnWindows() bool { return runtime.GOOS == "windows" }

// versionLess 比较两个语义化版本（a < b）。
// versionLess 委托给 domain.VersionLess —— 版本比较只允许有一份实现
// （注册表排序、/runners 的 latest 判定、argus CLI 展示都用它）。
func versionLess(a, b string) bool { return domain.VersionLess(a, b) }
