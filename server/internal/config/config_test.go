package config

import (
	"os"
	"testing"
	"time"
)

// clearArgusEnv 清掉所有 AG_*，避免开发机环境污染测试。
func clearArgusEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				name := kv[:i]
				if len(name) > len("AG_") && name[:len("AG_")] == "AG_" {
					t.Setenv(name, "")
					os.Unsetenv(name)
				}
				break
			}
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearArgusEnv(t)
	t.Setenv("AG_DB_DSN", "postgres://u:p@localhost:5432/argus")
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
	if c.MaxGlobalConcurrency != 20 {
		t.Errorf("MaxGlobalConcurrency = %d, 期望 20（§12.7）", c.MaxGlobalConcurrency)
	}
	if c.TaskMaxRunners != 200 {
		t.Errorf("TaskMaxRunners = %d, 期望 200（§12.7）", c.TaskMaxRunners)
	}
	if c.AttemptMax != 3 {
		t.Errorf("AttemptMax = %d, 期望 3（§12.7）", c.AttemptMax)
	}
	if c.DeployTimeout != 259200*time.Second {
		t.Errorf("DeployTimeout = %s, 期望 72h", c.DeployTimeout)
	}
	if c.RunnerTimeoutDefault != 4*time.Hour || c.RunnerTimeoutMax != 24*time.Hour {
		t.Errorf("RunnerTimeout = %s / %s, 期望 4h / 24h", c.RunnerTimeoutDefault, c.RunnerTimeoutMax)
	}
	if c.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, 期望 30（D-70：保留期只由配置决定）", c.RetentionDays)
	}
	if c.StoragePVC != "argus-storage" {
		t.Errorf("StoragePVC = %q, 期望 argus-storage（D-41）", c.StoragePVC)
	}
	if c.Namespace != "argus" {
		t.Errorf("Namespace = %q, 期望 argus（D-66）", c.Namespace)
	}
	if c.APIPrefix != "/api/v1" {
		t.Errorf("APIPrefix = %q", c.APIPrefix)
	}
	if c.AuthMode != "dev" {
		t.Errorf("AuthMode = %q, 期望 dev", c.AuthMode)
	}
	if c.SessionCookie != "ag_session" {
		t.Errorf("SessionCookie = %q, 期望 ag_session", c.SessionCookie)
	}
}

func TestLoad_Overrides(t *testing.T) {
	clearArgusEnv(t)
	t.Setenv("AG_DB_DSN", "postgres://u:p@localhost:5432/argus")
	t.Setenv("AG_LISTEN", ":9090")
	t.Setenv("AG_STORAGE_ROOT", `D:\tmp\storage`)
	t.Setenv("AG_ROLE", "controller")
	t.Setenv("AG_LOG_LEVEL", "debug")
	t.Setenv("AG_RUNNERS_DIR", `D:\tmp\runners`)
	t.Setenv("AG_CONCURRENCY_MAX", "12")
	t.Setenv("AG_ATTEMPT_MAX", "5")
	t.Setenv("AG_DEPLOY_TIMEOUT", "48h")
	t.Setenv("AG_RUNNER_TIMEOUT", "8h")
	t.Setenv("AG_KUBECONFIG", `D:\kube.yaml`)

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
	if c.AttemptMax != 5 {
		t.Errorf("AttemptMax = %d", c.AttemptMax)
	}
	if c.DeployTimeout != 48*time.Hour {
		t.Errorf("DeployTimeout = %s", c.DeployTimeout)
	}
	if c.RunnerTimeoutDefault != 8*time.Hour {
		t.Errorf("RunnerTimeoutDefault = %s", c.RunnerTimeoutDefault)
	}
	if c.Kubeconfig != `D:\kube.yaml` {
		t.Errorf("Kubeconfig = %q", c.Kubeconfig)
	}
}

func TestLoad_Invalid(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"角色非法", map[string]string{"AG_ROLE": "boss"}},
		{"日志级别非法", map[string]string{"AG_LOG_LEVEL": "trace"}},
		{"并发上限为 0", map[string]string{"AG_CONCURRENCY_MAX": "0"}},
		{"部署窗口过小", map[string]string{"AG_DEPLOY_TIMEOUT": "60"}},
		{"部署窗口过大", map[string]string{"AG_DEPLOY_TIMEOUT": "1000000"}},
		{"runner 超时过大", map[string]string{"AG_RUNNER_TIMEOUT": "48h"}},
		{"attempt 上限为 0", map[string]string{"AG_ATTEMPT_MAX": "0"}},
		{"鉴权模式非法", map[string]string{"AG_AUTH_MODE": "jwt"}},
		{"整数非法", map[string]string{"AG_CONCURRENCY_MAX": "abc"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearArgusEnv(t)
			t.Setenv("AG_DB_DSN", "postgres://u:p@localhost:5432/argus")
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
	clearArgusEnv(t)
	if _, err := Load(); err == nil {
		t.Fatal("缺少 AG_DB_DSN 时应报错")
	}
}

// Volcano 队列快照（C-4.8）：默认接入 + 15s TTL。
func TestLoad_VolcanoDefaults(t *testing.T) {
	clearArgusEnv(t)
	t.Setenv("AG_DB_DSN", "postgres://u:p@localhost:5432/argus")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.VolcanoEnabled {
		t.Fatal("VolcanoEnabled 默认应为 true")
	}
	if c.VolcanoTTL != 15*time.Second {
		t.Fatalf("VolcanoTTL = %s，期望 15s", c.VolcanoTTL)
	}
	if len(c.VolcanoNamespaces) != 1 || c.VolcanoNamespaces[0] != "argus" {
		t.Fatalf("VolcanoNamespaces = %v，期望 [argus]", c.VolcanoNamespaces)
	}
}

func TestLoad_VolcanoOverrides(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, c *Config)
	}{
		{
			name: "关闭 + 多 ns + TTL",
			env: map[string]string{
				"AG_VOLCANO_ENABLED":    "false",
				"AG_VOLCANO_NAMESPACES": "ns-a, ns-b",
				"AG_VOLCANO_TTL":        "30s",
			},
			check: func(t *testing.T, c *Config) {
				if c.VolcanoEnabled {
					t.Fatal("VolcanoEnabled 应为 false")
				}
				if c.VolcanoTTL != 30*time.Second {
					t.Fatalf("VolcanoTTL = %s", c.VolcanoTTL)
				}
				if len(c.VolcanoNamespaces) != 2 || c.VolcanoNamespaces[0] != "ns-a" || c.VolcanoNamespaces[1] != "ns-b" {
					t.Fatalf("VolcanoNamespaces = %v", c.VolcanoNamespaces)
				}
			},
		},
		{
			name: "全部 namespace",
			env:  map[string]string{"AG_VOLCANO_NAMESPACES": "*"},
			check: func(t *testing.T, c *Config) {
				if c.VolcanoNamespaces != nil {
					t.Fatalf("`*` 应解析为 nil（全部 ns），得到 %v", c.VolcanoNamespaces)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearArgusEnv(t)
			t.Setenv("AG_DB_DSN", "postgres://u:p@localhost:5432/argus")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, c)
		})
	}

	// TTL 非法直接 fail fast。
	clearArgusEnv(t)
	t.Setenv("AG_DB_DSN", "postgres://u:p@localhost:5432/argus")
	t.Setenv("AG_VOLCANO_TTL", "abc")
	if _, err := Load(); err == nil {
		t.Fatal("AG_VOLCANO_TTL=abc 应报错")
	}
}

func TestRedactedDSN(t *testing.T) {
	c := &Config{DatabaseURL: "postgres://user:secret@db:5432/argus?sslmode=disable"}
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

// D-63：AG_SHARED_MOUNTS 解析与校验。
func TestParseSharedMounts(t *testing.T) {
	t.Run("默认 readOnly=true", func(t *testing.T) {
		out, err := ParseSharedMounts(`[{"name":"models","mountPath":"/models","claimName":"c1"}]`)
		if err != nil {
			t.Fatalf("ParseSharedMounts: %v", err)
		}
		if len(out) != 1 || !out[0].ReadOnly {
			t.Fatalf("readOnly 默认应为 true: %+v", out)
		}
	})
	t.Run("显式 false 可写", func(t *testing.T) {
		out, err := ParseSharedMounts(`[{"name":"rw","mountPath":"/models","claimName":"c1","readOnly":false}]`)
		if err != nil {
			t.Fatalf("ParseSharedMounts: %v", err)
		}
		if out[0].ReadOnly {
			t.Fatal("显式 false 不应被改成 true")
		}
	})
	t.Run("空 = 不注入", func(t *testing.T) {
		for _, raw := range []string{"", "[]", "  "} {
			out, err := ParseSharedMounts(raw)
			if err != nil || len(out) != 0 {
				t.Errorf("raw=%q → %v, %v", raw, out, err)
			}
		}
	})
	t.Run("非法输入拒绝", func(t *testing.T) {
		cases := []string{
			`not-json`,
			`[{"mountPath":"/models","claimName":""}]`,            // claimName 必填
			`[{"mountPath":"relative","claimName":"c"}]`,          // 绝对路径
			`[{"mountPath":"/workspace/output","claimName":"c"}]`, // 保留路径
			`[{"name":"a","mountPath":"/m1","claimName":"c"},{"name":"a","mountPath":"/m2","claimName":"c"}]`,
		}
		for _, raw := range cases {
			if _, err := ParseSharedMounts(raw); err == nil {
				t.Errorf("raw=%s 应被拒绝", raw)
			}
		}
	})
}

func TestTaskStorageDir(t *testing.T) {
	c := &Config{StorageRoot: "/storage-root"}
	if got := c.TaskStorageDir("t-20261110-0001"); got != "/storage-root/t-20261110-0001" {
		t.Errorf("TaskStorageDir = %q", got)
	}
}
