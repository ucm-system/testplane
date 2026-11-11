package executor

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestDumpRenderedJobYAML 把渲染结果写成 YAML，供真集群手工验证
// （`kubectl apply --dry-run=client -f` / `kubectl apply -f`）。
//
// 只有设置了 AG_RENDER_OUT 才会执行，默认跳过，避免污染常规测试：
//
//	AG_RENDER_OUT=%TEMP%\argus-job.yaml go test ./internal/executor -run TestDumpRenderedJobYAML
func TestDumpRenderedJobYAML(t *testing.T) {
	out := os.Getenv("AG_RENDER_OUT")
	if out == "" {
		t.Skip("set AG_RENDER_OUT to dump the rendered Job YAML")
	}
	// 用真实集群里的名字：namespace=argus、PVC=argus-storage（infra 提供）。
	spec := sampleSpec()
	// 真集群验证不需要示例里的 Secret/额外 env（Secret 由 Task 级注入，验证时不建）。
	spec.SecretEnv = nil
	spec.Env = nil
	spec.Namespace = "argus"
	spec.StoragePVC = "argus-storage"
	spec.Image = "registry.dev.huawei.com/argus/endpoint-probe:1.0.0"

	job, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	raw, err := yaml.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := filepath.Dir(out)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("rendered Job YAML written to %s (name=%s)", out, job.Name)
}
