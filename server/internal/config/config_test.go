package config

import (
	"os"
	"testing"
	"time"
)

// clearTestplaneEnv 清掉所有 TESTPLANE_*，避免开发机环境污染测试。
func clearTestplaneEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				name := kv[:i]
				if len(name) > len("TESTPLANE_") && name[:len("TESTPLANE_")] == "TESTPLANE_" {
					t.Setenv(name, "")
					os.Unsetenv(name)
				}
				break
			}
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearTestplaneEnv(t)
	t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/testplane")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, 期望 :8080", c.HTTPAddr)
	}
	if c.StorageRoot != "/storage-root" {
		t.Errorf("StorageRoot = %q", c.StorageRoot)
	}
	if c.Role != RoleAll {
		t.Errorf("Role = %q, 期望 all", c.Role)
	}
	if c.TestersDir != "./testers" {
		t.Errorf("TestersDir = %q", c.TestersDir)
	}
	if c.MaxGlobalConcurrency != 4 {
		t.Errorf("MaxGlobalConcurrency = %d, 期望 4", c.MaxGlobalConcurrency)
	}
	if c.DeployTimeout != 259200*time.Second {
		t.Errorf("DeployTimeout = %s, 期望 72h", c.DeployTimeout)
	}
	if c.APIPrefix != "/api/v1" {
		t.Errorf("APIPrefix = %q", c.APIPrefix)
	}
	if c.AuthMode != "dev" {
		t.Errorf("AuthMode = %q, 期望 dev", c.AuthMode)
	}
}

func TestLoad_Overrides(t *testing.T) {
	clearTestplaneEnv(t)
	t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/tp")
	t.Setenv("TESTPLANE_HTTP_ADDR", ":9090")
	t.Setenv("TESTPLANE_STORAGE_ROOT", `D:\tmp\storage`)
	t.Setenv("TESTPLANE_ROLE", "controller")
	t.Setenv("TESTPLANE_LOG_LEVEL", "debug")
	t.Setenv("TESTPLANE_TESTERS_DIR", `D:\tmp\testers`)
	t.Setenv("TESTPLANE_MAX_GLOBAL_CONCURRENCY", "12")
	t.Setenv("TESTPLANE_DEPLOY_TIMEOUT_SECONDS", "3600")
	t.Setenv("TESTPLANE_KUBECONFIG", `D:\kube.yaml`)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":9090" || c.Role != RoleController || c.LogLevel != "debug" {
		t.Fatalf("基础覆盖失败: %+v", c)
	}
	if !c.Role.RunsController() || c.Role.RunsAPI() {
		t.Fatalf("角色分支错误: %+v", c.Role)
	}
	if c.MaxGlobalConcurrency != 12 {
		t.Errorf("MaxGlobalConcurrency = %d", c.MaxGlobalConcurrency)
	}
	if c.DeployTimeout != time.Hour {
		t.Errorf("DeployTimeout = %s", c.DeployTimeout)
	}
	if c.Kubeconfig != `D:\kube.yaml` {
		t.Errorf("Kubeconfig = %q", c.Kubeconfig)
	}
}

func TestLoad_DesignAliases(t *testing.T) {
	clearTestplaneEnv(t)
	t.Setenv("TESTPLANE_DB_DSN", "postgres://u:p@localhost:5432/tp")
	t.Setenv("TESTPLANE_LISTEN", ":7070")
	t.Setenv("TESTPLANE_CONCURRENCY_MAX", "7")
	t.Setenv("TESTPLANE_DEPLOY_TIMEOUT", "48h")
	t.Setenv("TESTPLANE_RESULT_MAX_BYTES", "10MiB")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":7070" || c.MaxGlobalConcurrency != 7 {
		t.Fatalf("C-11 别名未生效: %+v", c)
	}
	if c.DeployTimeout != 48*time.Hour {
		t.Errorf("DeployTimeout = %s", c.DeployTimeout)
	}
	if c.ResultMaxBytes != 10<<20 {
		t.Errorf("ResultMaxBytes = %d", c.ResultMaxBytes)
	}
}

func TestLoad_Invalid(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"角色非法", map[string]string{"TESTPLANE_ROLE": "boss"}},
		{"日志级别非法", map[string]string{"TESTPLANE_LOG_LEVEL": "trace"}},
		{"并发上限为 0", map[string]string{"TESTPLANE_MAX_GLOBAL_CONCURRENCY": "0"}},
		{"部署窗口过小", map[string]string{"TESTPLANE_DEPLOY_TIMEOUT_SECONDS": "60"}},
		{"部署窗口过大", map[string]string{"TESTPLANE_DEPLOY_TIMEOUT_SECONDS": "1000000"}},
		{"Task 超时过大", map[string]string{"TESTPLANE_TASK_TIMEOUT": "48h"}},
		{"鉴权模式非法", map[string]string{"TESTPLANE_AUTH_MODE": "jwt"}},
		{"整数非法", map[string]string{"TESTPLANE_MAX_GLOBAL_CONCURRENCY": "abc"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearTestplaneEnv(t)
			t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/tp")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("期望报错，实际通过")
			}
		})
	}
}

func TestLoad_MissingDSN(t *testing.T) {
	clearTestplaneEnv(t)
	if _, err := Load(); err == nil {
		t.Fatal("缺少 TESTPLANE_DATABASE_URL 时应报错")
	}
}

func TestRedactedDSN(t *testing.T) {
	c := &Config{DatabaseURL: "postgres://user:secret@db:5432/testplane?sslmode=disable"}
	got := c.RedactedDSN()
	if got == "" || contains(got, "secret") {
		t.Fatalf("DSN 未脱敏: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
