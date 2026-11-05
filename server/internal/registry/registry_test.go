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
