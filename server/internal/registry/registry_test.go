package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// validManifest 是一份最小可用的 runner.yaml（test 角色，附录 C v2 形态）。
const validManifest = `name: my-runner
version: 1.2.3
display_name: 我的测试
description: |
  用于测试。
role: test
on_failure: continue
entry: ["/runner/run.sh"]
input_format: jsonl
image: registry.example.com/argus/my-runner:1.2.3
resources:
  cpu: "1"
  memory: "256Mi"
  gpu: { resource: nvidia.com/gpu, count: 0 }
concurrency:
  endpoint_exclusive: false
mounts: []
exports: []
consumes: [AG_SERVED_URL, AG_SERVED_MODEL_NAME]
parameters:
  - name: dataset
    type: string
    required: true
    description: 数据集路径
env:
  API_KEY:
    required: false
    secret: true
    description: Bearer token
placeholder: |
  {"dataset":"/data/a"}
`

// writeRunnerDir 在临时目录里造一个合法的 Runner 目录（entry 文件 = run.sh）。
func writeRunnerDir(t *testing.T, root, dir, manifest string) string {
	t.Helper()
	sub := filepath.Join(root, dir)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, RunnerFileName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestParseAcceptsRealEndpointProbe(t *testing.T) {
	// 仓库里的全部真实 runner.yaml 都必须能通过 v2 注册校验（扫描目录仍是 testers/，
	// 目录改名随 Makefile/部署波次）。
	entries, err := os.ReadDir(filepath.Join("..", "..", "..", "testers"))
	if err != nil {
		t.Skipf("testers dir not available: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testers", e.Name(), "runner.yaml"))
		if err != nil {
			t.Skipf("%s: runner.yaml not available: %v", e.Name(), err)
		}
		if _, err := Parse(raw); err != nil {
			t.Errorf("Parse(%s): %v", e.Name(), err)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testers", "endpoint-probe", "runner.yaml"))
	if err != nil {
		t.Skipf("real runner.yaml not available: %v", err)
	}
	spec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(endpoint-probe): %v", err)
	}
	if spec.Name != "endpoint-probe" || spec.Version != "1.0.0" {
		t.Errorf("spec = %s", spec.Key())
	}
	if spec.Role != domain.RolePreCheck || spec.OnFailure != domain.OnFailureAbortTask {
		t.Errorf("role/on_failure = %s/%s", spec.Role, spec.OnFailure)
	}
	if len(spec.Entry) != 1 || spec.Entry[0] != "/runner/run.sh" {
		t.Errorf("entry = %v（exec form 数组，D-66）", spec.Entry)
	}
	if spec.InputFormat != domain.InputFormatJSONL {
		t.Errorf("input_format = %s", spec.InputFormat)
	}
	if !spec.Concurrency.EndpointExclusive {
		t.Errorf("pre_check must be endpoint_exclusive")
	}
	if spec.Exports != nil {
		t.Errorf("pre_check exports 应为空: %v", spec.Exports)
	}
	foundConsumes := map[string]bool{}
	for _, k := range spec.Consumes {
		foundConsumes[k] = true
	}
	if !foundConsumes["AG_SERVED_URL"] || !foundConsumes["AG_SERVED_MODEL_NAME"] {
		t.Errorf("consumes = %v", spec.Consumes)
	}
	if len(spec.Env) != 1 || !spec.Env["ENDPOINT_API_KEY"].Secret {
		t.Errorf("env declarations = %+v", spec.Env)
	}
	if spec.Resources.CPU != "1" || spec.Resources.Memory != "256Mi" {
		t.Errorf("resources = %+v", spec.Resources)
	}
	if spec.Resources.GPU.Count != 0 {
		t.Errorf("gpu count = %d", spec.Resources.GPU.Count)
	}
	// parameters 是 jsonl 结构 schema（name/type/required，D-67）。
	var hasWait bool
	for _, p := range spec.Parameters {
		if p.Name == "wait_timeout_seconds" {
			hasWait = true
			if p.Type != "integer" || p.Required {
				t.Errorf("wait_timeout_seconds = %+v", p)
			}
		}
	}
	if !hasWait {
		t.Error("parameters 缺 wait_timeout_seconds")
	}
}

func TestParseRejectsBrokenManifests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		path   string
	}{
		{name: "pre_check without abort_task", mutate: func(m string) string {
			return strings.Replace(m, "role: test", "role: pre_check", 1)
		}, path: "on_failure"},
		{name: "empty entry", mutate: func(m string) string {
			return strings.Replace(m, `entry: ["/runner/run.sh"]`, "entry: []", 1)
		}, path: "entry"},
		{name: "scalar entry not exec form", mutate: func(m string) string {
			return strings.Replace(m, `entry: ["/runner/run.sh"]`, `entry: "/runner/run.sh"`, 1)
		}, path: "yaml"},
		{name: "bad version", mutate: func(m string) string {
			return strings.Replace(m, "version: 1.2.3", "version: v1", 1)
		}, path: "version"},
		{name: "missing image", mutate: func(m string) string {
			return strings.Replace(m, "image: registry.example.com/argus/my-runner:1.2.3", `image: ""`, 1)
		}, path: "image"},
		{name: "forbidden field timeout_seconds", mutate: func(m string) string {
			return m + "timeout_seconds: 600\n"
		}, path: "timeout_seconds"},
		{name: "forbidden field requires", mutate: func(m string) string {
			return m + "requires: url\n"
		}, path: "requires"},
		{name: "relative mount", mutate: func(m string) string {
			return strings.Replace(m, "mounts: []", "mounts:\n  - name: data\n    mount_path: data/dir\n", 1)
		}, path: "mounts[0].mount_path"},
		{name: "platform mount conflict", mutate: func(m string) string {
			return strings.Replace(m, "mounts: []", "mounts:\n  - name: out\n    path: /workspace/output\n", 1)
		}, path: "mounts[0].mount_path"},
		{name: "bad input_format", mutate: func(m string) string {
			return strings.Replace(m, "input_format: jsonl", "input_format: ini", 1)
		}, path: "input_format"},
		{name: "parameters with yaml format", mutate: func(m string) string {
			m = strings.Replace(m, "input_format: jsonl", "input_format: yaml", 1)
			return m
		}, path: "parameters"},
		{name: "bad param type", mutate: func(m string) string {
			return strings.Replace(m, "type: string", "type: float128", 1)
		}, path: "parameters[0].type"},
		{name: "duplicate export key", mutate: func(m string) string {
			return strings.Replace(m, "exports: []", "exports: [A, A]", 1)
		}, path: "exports"},
		{name: "role deploy rejected for users", mutate: func(m string) string {
			return strings.Replace(m, "role: test", "role: deploy", 1)
		}, path: "role"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.mutate(validManifest)))
			if err == nil {
				t.Fatalf("期望被拒绝")
			}
			var ve *ValidationError
			if !errorAs(err, &ve) {
				t.Fatalf("err = %v, want ValidationError", err)
			}
			found := c.path == "yaml" // yaml 反序列化错误的 path 为空
			for _, fe := range ve.Errors {
				if strings.HasPrefix(fe.Path, c.path) {
					found = true
				}
			}
			if !found {
				t.Fatalf("字段路径 %q 未出现在错误里: %v", c.path, ve.Errors)
			}
		})
	}
}

func TestParseBuiltinAllowsDeployRole(t *testing.T) {
	m := strings.Replace(validManifest, "role: test", "role: deploy", 1)
	m = strings.Replace(m, "on_failure: continue", "on_failure: abort_task", 1)
	m = strings.Replace(m, "name: my-runner", "name: service-deploy", 1)
	spec, err := ParseBuiltin([]byte(m))
	if err != nil {
		t.Fatalf("内置 deploy 应放行: %v", err)
	}
	if spec.Role != domain.RoleDeploy {
		t.Errorf("role = %s", spec.Role)
	}
	// 用户路径拒绝 deploy（ErrForbiddenRole 经 FieldError 呈现）。
	if _, err := Parse([]byte(m)); err == nil || !strings.Contains(err.Error(), "built-in only") {
		t.Fatalf("用户注册 deploy 应拒绝: %v", err)
	}
}

func TestParseAcceptsPathAndMountPathKeys(t *testing.T) {
	m := strings.Replace(validManifest, "mounts: []",
		"mounts:\n  - name: data\n    path: /data\n    read_only: true\n", 1)
	spec, err := Parse([]byte(m))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].MountPath != "/data" {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}
}

func TestScanRegistersAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeRunnerDir(t, root, "my-runner", validManifest)
	reg := New(NewMemoryStore(), Options{})
	diff, err := reg.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(diff.Added) != 1 {
		t.Fatalf("diff = %+v", diff)
	}
	diff2, err := reg.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan#2: %v", err)
	}
	if len(diff2.Unchanged) != 1 || len(diff2.Added) != 0 {
		t.Fatalf("diff2 = %+v（应幂等）", diff2)
	}
	spec, err := reg.Resolve(context.Background(), "my-runner", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Key() != "my-runner@1.2.3" {
		t.Errorf("spec = %s", spec.Key())
	}
}

func TestScanRefusesSameVersionDifferentContent(t *testing.T) {
	root := t.TempDir()
	writeRunnerDir(t, root, "my-runner", validManifest)
	reg := New(NewMemoryStore(), Options{})
	if _, err := reg.Scan(context.Background(), root); err != nil {
		t.Fatalf("Scan#1: %v", err)
	}
	// 改内容不改版本 → 拒绝。
	writeRunnerDir(t, root, "my-runner", strings.Replace(validManifest, "cpu: \"1\"", "cpu: \"2\"", 1))
	_, err := reg.Scan(context.Background(), root)
	if err == nil || !errorIs(err, ErrVersionPinned) {
		t.Fatalf("err = %v, want ErrVersionPinned", err)
	}
}

func TestScanRejectsMissingEntryFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "my-runner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, RunnerFileName), []byte(validManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := reg0().Scan(context.Background(), root)
	if err == nil || !errorIs(err, ErrInvalid) {
		t.Fatalf("entry 文件缺失应拒绝: %v", err)
	}
}

func TestScanRejectsDirectoryNameMismatch(t *testing.T) {
	root := t.TempDir()
	writeRunnerDir(t, root, "wrong-dir", validManifest)
	_, err := reg0().Scan(context.Background(), root)
	if err == nil || !errorIs(err, ErrInvalid) {
		t.Fatalf("目录名不一致应拒绝: %v", err)
	}
}

func TestScanMarksOrphanedInsteadOfDeleting(t *testing.T) {
	root := t.TempDir()
	writeRunnerDir(t, root, "my-runner", validManifest)
	store := NewMemoryStore()
	reg := New(store, Options{})
	if _, err := reg.Scan(context.Background(), root); err != nil {
		t.Fatalf("Scan#1: %v", err)
	}
	// 目录清空 → orphaned。
	if err := os.RemoveAll(filepath.Join(root, "my-runner")); err != nil {
		t.Fatal(err)
	}
	diff, err := reg.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan#2: %v", err)
	}
	if len(diff.Removed) != 1 {
		t.Fatalf("diff = %+v", diff)
	}
	rec, err := store.GetRunner(context.Background(), "my-runner", "1.2.3")
	if err != nil || !rec.Orphaned {
		t.Fatalf("rec = %+v err=%v（orphaned 必须落库）", rec, err)
	}
}

func TestResolveLatestUsesVersionOrder(t *testing.T) {
	reg := New(NewMemoryStore(), Options{})
	if _, err := reg.RegisterForTest([]byte(validManifest)); err != nil {
		t.Fatalf("register 1.2.3: %v", err)
	}
	v2 := strings.Replace(validManifest, "version: 1.2.3", "version: 1.10.0", 1)
	if _, err := reg.RegisterForTest([]byte(v2)); err != nil {
		t.Fatalf("register 1.10.0: %v", err)
	}
	spec, err := reg.Resolve(context.Background(), "my-runner", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Version != "1.10.0" {
		t.Fatalf("latest = %s, want 1.10.0（VersionLess 排序，含两位小版本）", spec.Version)
	}
	list, err := reg.List(context.Background(), "", true)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v err=%v", list, err)
	}
	// 版本降序：1.10.0 在 1.2.3 前。
	if !versionLess(list[1].Version, list[0].Version) {
		t.Fatalf("排序错误: %v", list)
	}
}

func TestListFiltersByRole(t *testing.T) {
	reg := New(nil, Options{})
	if _, err := reg.RegisterForTest([]byte(validManifest)); err != nil {
		t.Fatalf("RegisterForTest: %v", err)
	}
	pre := strings.Replace(validManifest, "role: test", "role: pre_check", 1)
	pre = strings.Replace(pre, "on_failure: continue", "on_failure: abort_task", 1)
	pre = strings.Replace(pre, "name: my-runner", "name: probe-runner", 1)
	if _, err := reg.RegisterForTest([]byte(pre)); err != nil {
		t.Fatalf("RegisterForTest(pre): %v", err)
	}
	list, err := reg.List(context.Background(), domain.RolePreCheck, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", list, err)
	}
	if list[0].Role != domain.RolePreCheck {
		t.Errorf("role = %s", list[0].Role)
	}
}

func TestOnFailureMerge(t *testing.T) {
	if err := ValidateOnFailureMerge(domain.OnFailureAbortTask, domain.OnFailureContinue); err == nil {
		t.Fatal("弱化 abort_task 应拒绝（§7.3）")
	}
	for _, ok := range []struct{ r, m string }{
		{domain.OnFailureAbortTask, domain.OnFailureAbortTask},
		{domain.OnFailureContinue, domain.OnFailureContinue},
		{domain.OnFailureContinue, domain.OnFailureAbortTask},
		{domain.OnFailureContinue, ""},
	} {
		if err := ValidateOnFailureMerge(ok.r, ok.m); err != nil {
			t.Errorf("merge(%s,%s) = %v", ok.r, ok.m, err)
		}
	}
}

func TestEndpointKeyNormalization(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"http://10.0.0.10:8000/v1", "http://10.0.0.10:8000"},
		{"http://EXAMPLE.com", "http://example.com:80"},
	}
	for _, c := range cases {
		if got := EndpointKey(c.raw); got != c.want {
			t.Errorf("EndpointKey(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestValidateInput(t *testing.T) {
	spec := domain.RunnerSpec{
		InputFormat: domain.InputFormatJSONL,
		Parameters: []domain.Param{
			{Name: "dataset", Type: "string", Required: true},
			{Name: "input", Type: "integer"},
		},
	}
	if issues := ValidateInput(spec, "{\"dataset\":\"/a\",\"input\":512}\n"); len(issues) != 0 {
		t.Fatalf("合法输入应通过: %v", issues)
	}
	issues := ValidateInput(spec, "{broken\n")
	if len(issues) == 0 || issues[0].Line != 1 {
		t.Fatalf("issues = %v, want line=1", issues)
	}
	yamlSpec := domain.RunnerSpec{InputFormat: domain.InputFormatYAML}
	if issues := ValidateInput(yamlSpec, "a: 1\n"); len(issues) != 0 {
		t.Fatalf("yaml 输入应只做语法校验: %v", issues)
	}
}

// ---- 小工具 ---------------------------------------------------------------

func reg0() *Registry { return New(NewMemoryStore(), Options{}) }

func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func errorAs(err error, target any) bool { return errors.As(err, target) }
