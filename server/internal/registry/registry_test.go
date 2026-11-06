package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// validManifest 是一份最小可用的 tester.yaml（test 角色）。
const validManifest = `name: my-tester
version: 1.2.3
display_name: 我的测试
description: |
  用于测试。
role: test
on_failure: continue
entry: /tester/run.sh
image: registry.example.com/testplane/my-tester:1.2.3
resources:
  cpu: "1"
  memory: "256Mi"
  gpu: { resource: nvidia.com/gpu, count: 0 }
concurrency:
  endpoint_exclusive: false
mounts: []
parameters:
  url:
    type: string
    required: true
    description: OpenAI 兼容 base URL
env:
  API_KEY:
    required: false
    secret: true
    description: Bearer token
`

// writeTesterDir 在临时目录里造一个合法的 Tester 目录。
func writeTesterDir(t *testing.T, root, dir, manifest string) string {
	t.Helper()
	sub := filepath.Join(root, dir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, TesterFileName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, RunScriptName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestParseAcceptsRealEndpointProbe(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testers", "endpoint-probe", "tester.yaml"))
	if err != nil {
		t.Skipf("real tester.yaml not available: %v", err)
	}
	spec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(endpoint-probe): %v", err)
	}
	if spec.Name != "endpoint-probe" || spec.Version != "1.0.0" {
		t.Errorf("spec = %s", spec.Key())
	}
	if spec.Role != domain.RolePreCheck || spec.OnFailure != domain.OnFailureAbortRun {
		t.Errorf("role/on_failure = %s/%s", spec.Role, spec.OnFailure)
	}
	if !spec.Concurrency.EndpointExclusive {
		t.Errorf("pre_check must be endpoint_exclusive")
	}
	if spec.Parameters["url"].Required != true {
		t.Errorf("url must be a required public parameter")
	}
	if len(spec.Env) != 1 || !spec.Env["ENDPOINT_API_KEY"].Secret {
		t.Errorf("env declarations = %+v", spec.Env)
	}
	if spec.Resources.CPU != "1" || spec.Resources.Memory != "256Mi" {
		t.Errorf("resources = %+v", spec.Resources)
	}
	// 真实文件里 gpu.count = 0，必须不误判为需要 GPU。
	if spec.Resources.GPU.Count != 0 {
		t.Errorf("gpu count = %d", spec.Resources.GPU.Count)
	}
}

func TestParseRejectsBrokenManifests(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*testing.T, string) string
		wantErr error
		path    string
	}{
		{name: "pre_check without abort_run", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "role: test", "role: pre_check", 1)
		}, wantErr: ErrInvalid, path: "on_failure"},
		{name: "wrong entry", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "/tester/run.sh", "/bin/bash", 1)
		}, wantErr: ErrInvalid, path: "entry"},
		{name: "bad version", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "version: 1.2.3", "version: v1", 1)
		}, wantErr: ErrInvalid, path: "version"},
		{name: "missing image", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "image: registry.example.com/testplane/my-tester:1.2.3", "image: \"\"", 1)
		}, wantErr: ErrInvalid, path: "image"},
		{name: "forbidden field", mutate: func(t *testing.T, m string) string {
			return m + "timeout_seconds: 600\n"
		}, wantErr: ErrInvalid, path: "timeout_seconds"},
		{name: "relative mount", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "mounts: []", "mounts:\n  - name: data\n    mount_path: data/dir\n", 1)
		}, wantErr: ErrInvalid, path: "mounts[0].mount_path"},
		{name: "platform mount conflict", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "mounts: []", "mounts:\n  - name: out\n    path: /workspace/output\n", 1)
		}, wantErr: ErrInvalid, path: "mounts[0].mount_path"},
		{name: "missing parameter description", mutate: func(t *testing.T, m string) string {
			return strings.Replace(m, "    description: OpenAI 兼容 base URL\n", "", 1)
		}, wantErr: ErrInvalid, path: "parameters.url.description"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(t, validManifest)))
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			var verr *ValidationError
			if errors.As(err, &verr) {
				var hit bool
				for _, fe := range verr.Errors {
					if strings.Contains(fe.Path, tc.path) {
						hit = true
					}
				}
				if !hit {
					t.Errorf("field errors %+v do not mention %q", verr.Errors, tc.path)
				}
			}
		})
	}
}

func TestParseAcceptsPathAndMountPathKeys(t *testing.T) {
	m := strings.Replace(validManifest, "mounts: []",
		"mounts:\n  - name: a\n    path: /opt/a\n  - name: b\n    mount_path: /opt/b\n", 1)
	spec, err := Parse([]byte(m))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(spec.Mounts) != 2 || spec.Mounts[0].MountPath != "/opt/a" || spec.Mounts[1].MountPath != "/opt/b" {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}
}

func TestScanRegistersAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeTesterDir(t, root, "my-tester", validManifest)
	store := NewMemoryStore()
	reg := New(store, Options{})

	diff, err := reg.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(diff.Added) != 1 || diff.Added[0] != "my-tester@1.2.3" {
		t.Fatalf("diff = %+v", diff)
	}
	if reg.Len() != 1 {
		t.Fatalf("registry len = %d", reg.Len())
	}

	// 再扫一次：同内容 no-op。
	diff, err = reg.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if len(diff.Added) != 0 || len(diff.Unchanged) != 1 {
		t.Fatalf("second diff = %+v", diff)
	}
}

func TestScanRefusesSameVersionDifferentContent(t *testing.T) {
	root := t.TempDir()
	writeTesterDir(t, root, "my-tester", validManifest)
	reg := New(NewMemoryStore(), Options{})
	if _, err := reg.Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// 改内容但不动版本 → 必须拒绝（TESTER_VERSION_PINNED）。
	changed := strings.Replace(validManifest, "description: |\n  用于测试。", "description: modified", 1)
	if err := os.WriteFile(filepath.Join(root, "my-tester", TesterFileName), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Scan(context.Background(), root)
	if !errors.Is(err, ErrVersionPinned) {
		t.Fatalf("err = %v, want ErrVersionPinned", err)
	}
}

func TestScanRejectsDirectoryNameMismatch(t *testing.T) {
	root := t.TempDir()
	writeTesterDir(t, root, "other-dir", validManifest)
	reg := New(NewMemoryStore(), Options{})
	if _, err := reg.Scan(context.Background(), root); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestScanMarksOrphanedInsteadOfDeleting(t *testing.T) {
	root := t.TempDir()
	writeTesterDir(t, root, "my-tester", validManifest)
	reg := New(NewMemoryStore(), Options{})
	if _, err := reg.Scan(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "my-tester")); err != nil {
		t.Fatal(err)
	}
	diff, err := reg.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(diff.Removed) != 1 {
		t.Fatalf("removed = %+v", diff.Removed)
	}
	if _, err := reg.Resolve(context.Background(), "my-tester", "1.2.3"); err != nil {
		t.Fatalf("orphaned tester must still resolve: %v", err)
	}
	list, err := reg.List(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("orphaned tester must be hidden by default: %+v", list)
	}
	list, _ = reg.List(context.Background(), "", true)
	if len(list) != 1 {
		t.Fatalf("includeOrphaned must show it: %+v", list)
	}
}

func TestRegisterAndResolve(t *testing.T) {
	store := NewMemoryStore()
	reg := New(store, Options{})
	ctx := context.Background()

	spec, err := reg.Register(ctx, []byte(validManifest))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if spec.Key() != "my-tester@1.2.3" {
		t.Fatalf("spec = %s", spec.Key())
	}
	// 幂等重复上传。
	if _, err := reg.Register(ctx, []byte(validManifest)); err != nil {
		t.Fatalf("idempotent Register: %v", err)
	}
	// 同版本不同内容 → 409 语义。
	changed := strings.Replace(validManifest, "display_name: 我的测试", "display_name: 改名了", 1)
	if _, err := reg.Register(ctx, []byte(changed)); !errors.Is(err, ErrVersionPinned) {
		t.Fatalf("err = %v, want ErrVersionPinned", err)
	}

	got, err := reg.Resolve(ctx, "my-tester", "")
	if err != nil || got.Version != "1.2.3" {
		t.Fatalf("Resolve latest = %+v, %v", got, err)
	}
	if _, err := reg.Resolve(ctx, "nope", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Resolve missing err = %v, want domain.ErrNotFound", err)
	}
	if _, err := reg.Resolve(ctx, "my-tester", "9.9.9"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Resolve missing version err = %v", err)
	}
}

func TestListFiltersByRoleAndSortsVersions(t *testing.T) {
	store := NewMemoryStore()
	reg := New(store, Options{})
	ctx := context.Background()
	for _, v := range []string{"1.0.0", "1.10.0", "1.2.0"} {
		m := strings.Replace(validManifest, "version: 1.2.3", "version: "+v, 1)
		if _, err := reg.Register(ctx, []byte(m)); err != nil {
			t.Fatalf("Register %s: %v", v, err)
		}
	}
	specs, err := reg.List(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("list = %d", len(specs))
	}
	// 最新版本应当是 1.10.0（语义化版本比较，不是字典序）。
	latest, err := reg.Resolve(ctx, "my-tester", "")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != "1.10.0" {
		t.Errorf("latest = %s, want 1.10.0", latest.Version)
	}
	if got, _ := reg.List(ctx, domain.RolePreCheck, false); len(got) != 0 {
		t.Errorf("role filter should exclude testers: %+v", got)
	}
}

func TestLoadScansThenHydratesFromStore(t *testing.T) {
	root := t.TempDir()
	writeTesterDir(t, root, "my-tester", validManifest)
	store := NewMemoryStore()
	// 预置一条历史版本（目录里已不存在）。
	old := strings.Replace(validManifest, "version: 1.2.3", "version: 0.9.0", 1)
	oldSpec, err := Parse([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTester(context.Background(), Record{Spec: oldSpec, ContentSHA: "deadbeef"}); err != nil {
		t.Fatal(err)
	}

	reg := New(store, Options{})
	diff, err := reg.Load(context.Background(), root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(diff.Added) != 1 {
		t.Fatalf("diff = %+v", diff)
	}
	if _, err := reg.Resolve(context.Background(), "my-tester", "0.9.0"); err != nil {
		t.Fatalf("historical version must resolve after Load: %v", err)
	}
}

func TestRunRescanPicksUpNewTester(t *testing.T) {
	root := t.TempDir()
	writeTesterDir(t, root, "my-tester", validManifest)
	reg := New(NewMemoryStore(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = reg.RunRescan(ctx, root, 20*time.Millisecond) }()

	second := strings.Replace(validManifest, "name: my-tester", "name: second-tester", 1)
	writeTesterDir(t, root, "second-tester", second)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := reg.Resolve(context.Background(), "second-tester", ""); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("periodic rescan never picked up the new tester")
}

func TestBuildArgvInjectsPublicParams(t *testing.T) {
	ep := domain.Endpoint{BaseURL: "http://10.0.0.10:8000/v1", ModelName: "Qwen3-32B"}
	argv, err := BuildArgv("--wait_timeout_seconds=600 --retry_interval_seconds=15", ep)
	if err != nil {
		t.Fatalf("BuildArgv: %v", err)
	}
	want := []string{
		"--wait_timeout_seconds=600", "--retry_interval_seconds=15",
		"--url=http://10.0.0.10:8000/v1", "--model_name=Qwen3-32B",
	}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
	// 空值跳过。
	argv, err = BuildArgv("", domain.Endpoint{BaseURL: "http://h:1/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "--url=http://h:1/v1" {
		t.Fatalf("argv = %#v", argv)
	}
	// 冲突：提交者自己写 --url → 拒绝。
	if _, err := BuildArgv("--url=http://x", ep); err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestCheckPublicParams(t *testing.T) {
	spec, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	if missing := CheckPublicParams(spec, domain.Endpoint{BaseURL: "http://h:1/v1"}); len(missing) != 0 {
		t.Errorf("missing = %v", missing)
	}
	if missing := CheckPublicParams(spec, domain.Endpoint{}); len(missing) != 1 || missing[0] != "url" {
		t.Errorf("missing = %v", missing)
	}
}

func TestEndpointKeyNormalization(t *testing.T) {
	if got := EndpointKey("http://10.0.0.10:8000/v1"); got != "http://10.0.0.10:8000" {
		t.Errorf("key = %q", got)
	}
	if EndpointKey("") != "" {
		t.Errorf("empty endpoint has no key; the scheduler guard must reject it (C-4.5)")
	}
}

// TestScanPersistsOrphanedSoRestartKeepsRetired 是票 #60 真机回归的核心用例：
// Scan 发现目录里的版本消失后，**必须把 orphaned 落库**；新建一个 Registry 只从同一个
// store 读取（模拟进程重启）时，旧版本仍应为 orphaned=true，并且默认 List 不返回它。
//
// 改前：Scan 只改内存 → 重启后从库里读回 orphaned=false → `retired` 恒 false。
// TestLoadMarksOrphanedForStoreOnlyVersions 锁住 Load 的顺序：必须**先水合再 Scan**。
//
// 真机回归（票 #60）：升级到 0.3.0 后，0.1.0/0.2.0 只存在于库里、目录里没有；
// 旧实现先 Scan（此时 records 为空，removed 恒空）再 ListTesters，于是永远不标
// orphaned → `retired` 恒 false。这里直接把"库里已有旧版本、目录只有新版本"喂给 Load。
func TestLoadMarksOrphanedForStoreOnlyVersions(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	root := t.TempDir()

	// ① 让库里先有 0.1.0（模拟上一个镜像注册过的旧版本）。
	old := strings.Replace(validManifest, "version: 1.2.3", "version: 0.1.0", 1)
	writeTesterDir(t, root, "my-tester", old)
	if _, err := New(store, Options{}).Scan(ctx, root); err != nil {
		t.Fatalf("seed Scan: %v", err)
	}

	// ② 目录换成 0.2.0（模拟新镜像只剩新版本），然后**只调用 Load**（进程重启的真实路径）。
	if err := os.RemoveAll(filepath.Join(root, "my-tester")); err != nil {
		t.Fatal(err)
	}
	writeTesterDir(t, root, "my-tester", strings.Replace(validManifest, "version: 1.2.3", "version: 0.2.0", 1))

	reg := New(store, Options{})
	diff, err := reg.Load(ctx, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(diff.Removed) != 1 || diff.Removed[0] != "my-tester@0.1.0" {
		t.Fatalf("diff.Removed = %+v, 期望 [my-tester@0.1.0]（Load 必须先水合再 Scan）", diff.Removed)
	}
	rec, err := store.GetTester(ctx, "my-tester", "0.1.0")
	if err != nil {
		t.Fatalf("store.GetTester: %v", err)
	}
	if !rec.Orphaned {
		t.Fatal("Load 之后库里的 0.1.0 仍是 orphaned=false（顺序错了：先 Scan 后水合）")
	}
	got, err := reg.List(ctx, "", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Version != "0.2.0" {
		t.Fatalf("List(includeOrphaned=false) = %+v, 期望只剩 0.2.0", got)
	}
}
func TestScanPersistsOrphanedSoRestartKeepsRetired(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	root := t.TempDir()

	// 第一次启动：目录里是 1.2.3（一个 tester 目录只有一份 tester.yaml，同名不同版本
	// 通过"改目录内容 + 重扫"来表达，正是真实升版本的样子）。
	writeTesterDir(t, root, "my-tester", validManifest)

	reg := New(store, Options{})
	if _, err := reg.Scan(ctx, root); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// 升级到 1.3.0：删掉旧目录，用同名目录写新版本，再扫一次。
	if err := os.RemoveAll(filepath.Join(root, "my-tester")); err != nil {
		t.Fatal(err)
	}
	writeTesterDir(t, root, "my-tester", strings.Replace(validManifest, "version: 1.2.3", "version: 1.3.0", 1))

	diff, err := reg.Scan(ctx, root)
	if err != nil {
		t.Fatalf("Scan(2): %v", err)
	}
	if len(diff.Removed) != 1 || diff.Removed[0] != "my-tester@1.2.3" {
		t.Fatalf("diff.Removed = %+v, 期望 [my-tester@1.2.3]", diff.Removed)
	}

	// 关键：库里必须已经落库（而不是只在内存）。
	rec, err := store.GetTester(ctx, "my-tester", "1.2.3")
	if err != nil {
		t.Fatalf("store.GetTester: %v", err)
	}
	if !rec.Orphaned {
		t.Fatal("Scan 后库里的 1.2.3 仍是 orphaned=false（只改了内存，没落库）")
	}

	// 模拟重启：新 Registry 只从同一个 store 读（Load 不带目录 = 纯水合）。
	restarted := New(store, Options{})
	if _, err := restarted.Load(ctx, ""); err != nil {
		t.Fatalf("Load: %v", err)
	}
	listAll, err := restarted.List(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, sp := range listAll {
		if sp.Name == "my-tester" && sp.Version == "1.2.3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("重启后 includeOrphaned=true 应仍能看到 1.2.3: %+v", listAll)
	}
	defaultList, err := restarted.List(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range defaultList {
		if sp.Version == "1.2.3" {
			t.Fatalf("重启后默认 List 不应返回已退役的 1.2.3: %+v", defaultList)
		}
	}
	// 再确认内存快照里的 orphaned 标记也回来了（不是靠 List 过滤掩盖）。
	rec2 := recordOf(t, restarted, "my-tester", "1.2.3")
	if rec2.Orphaned != rec.Orphaned {
		t.Fatalf("重启后 orphaned = %v，库内 = %v（内存与库分叉）", rec2.Orphaned, rec.Orphaned)
	}
}

// TestScanOrphanedFailureDoesNotDivergeMemory 落库失败时不得只改内存。
func TestScanOrphanedFailureDoesNotDivergeMemory(t *testing.T) {
	ctx := context.Background()
	store := &failingOrphanStore{MemoryStore: NewMemoryStore()}
	root := t.TempDir()
	writeTesterDir(t, root, "my-tester", validManifest)

	reg := New(store, Options{})
	if _, err := reg.Scan(ctx, root); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "my-tester")); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if _, err := reg.Scan(ctx, root); err == nil {
		t.Fatal("落库失败时 Scan 必须返回错误（不能让内存与库分叉）")
	}
	// 内存里的标记必须仍是 false（没有单方面置 true）。
	rec := recordOf(t, reg, "my-tester", "1.2.3")
	if rec.Orphaned {
		t.Fatal("落库失败后内存里却标了 orphaned（内存与库分叉）")
	}
}

// recordOf 读注册表内存快照里的一行（同包测试用，避免为测试新增导出 API）。
func recordOf(t *testing.T, r *Registry, name, version string) Record {
	t.Helper()
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[Key(name, version)]
	if !ok {
		t.Fatalf("内存快照里没有 %s@%s", name, version)
	}
	return rec
}

// failingOrphanStore 让 MarkTesterOrphaned 可控失败。
type failingOrphanStore struct {
	*MemoryStore
	fail bool
}

func (f *failingOrphanStore) MarkTesterOrphaned(ctx context.Context, name, version string) error {
	if f.fail {
		return errors.New("store unavailable")
	}
	return f.MemoryStore.MarkTesterOrphaned(ctx, name, version)
}
