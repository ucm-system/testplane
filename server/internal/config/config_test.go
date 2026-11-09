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

// Volcano 队列快照（E-11、C-4.8）：默认接入 + 15s TTL + podgroups namespace 口径。
func TestLoad_VolcanoDefaults(t *testing.T) {
	clearTestplaneEnv(t)
	t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/tp")

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
	// 未配置 TESTPLANE_VOLCANO_NAMESPACES 时默认取 profile 的 namespace
	//（= seed 的 execution_profiles.namespace，与控制面 namespace 同值）。
	if len(c.VolcanoNamespaces) != 1 || c.VolcanoNamespaces[0] != "testplane" {
		t.Fatalf("VolcanoNamespaces = %v，期望 [testplane]", c.VolcanoNamespaces)
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
				"TESTPLANE_VOLCANO_ENABLED":    "false",
				"TESTPLANE_VOLCANO_NAMESPACES": "tp-a, tp-b",
				"TESTPLANE_VOLCANO_TTL":        "30s",
			},
			check: func(t *testing.T, c *Config) {
				if c.VolcanoEnabled {
					t.Fatal("VolcanoEnabled 应为 false")
				}
				if c.VolcanoTTL != 30*time.Second {
					t.Fatalf("VolcanoTTL = %s", c.VolcanoTTL)
				}
				if len(c.VolcanoNamespaces) != 2 || c.VolcanoNamespaces[0] != "tp-a" || c.VolcanoNamespaces[1] != "tp-b" {
					t.Fatalf("VolcanoNamespaces = %v", c.VolcanoNamespaces)
				}
			},
		},
		{
			name: "全部 namespace",
			env:  map[string]string{"TESTPLANE_VOLCANO_NAMESPACES": "*"},
			check: func(t *testing.T, c *Config) {
				if c.VolcanoNamespaces != nil {
					t.Fatalf("`*` 应解析为 nil（全部 ns），得到 %v", c.VolcanoNamespaces)
				}
			},
		},
		{
			name: "默认 ns 跟随 profile 的 namespace",
			env:  map[string]string{"TESTPLANE_NAMESPACE": "tp-custom"},
			check: func(t *testing.T, c *Config) {
				if len(c.VolcanoNamespaces) != 1 || c.VolcanoNamespaces[0] != "tp-custom" {
					t.Fatalf("VolcanoNamespaces = %v，期望 [tp-custom]", c.VolcanoNamespaces)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTestplaneEnv(t)
			t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/tp")
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
	clearTestplaneEnv(t)
	t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/tp")
	t.Setenv("TESTPLANE_VOLCANO_TTL", "abc")
	if _, err := Load(); err == nil {
		t.Fatal("TESTPLANE_VOLCANO_TTL=abc 应报错")
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

// Deploy 阶段配置：provider=mock 不校验 Genet 细节；provider=genet 必须给全。
func TestLoadDeployProvider(t *testing.T) {
	t.Run("mock 默认", func(t *testing.T) {
		t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/db")
		t.Setenv("TESTPLANE_DEPLOY_PROVIDER", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DeployProvider != "mock" {
			t.Errorf("provider = %q, want mock", c.DeployProvider)
		}
	})
	t.Run("genet 缺 base url", func(t *testing.T) {
		t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/db")
		t.Setenv("TESTPLANE_DEPLOY_PROVIDER", "genet")
		t.Setenv("TESTPLANE_GENET_BASE_URL", "")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for missing TESTPLANE_GENET_BASE_URL")
		}
	})
	t.Run("genet 缺 api key", func(t *testing.T) {
		t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/db")
		t.Setenv("TESTPLANE_DEPLOY_PROVIDER", "genet")
		t.Setenv("TESTPLANE_GENET_BASE_URL", "https://genet.example")
		t.Setenv("TESTPLANE_GENET_API_KEY", "")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for missing TESTPLANE_GENET_API_KEY")
		}
	})
	t.Run("genet 配置齐全", func(t *testing.T) {
		t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/db")
		t.Setenv("TESTPLANE_DEPLOY_PROVIDER", "genet")
		t.Setenv("TESTPLANE_GENET_BASE_URL", "https://genet.example/")
		t.Setenv("TESTPLANE_GENET_API_KEY", "k")
		t.Setenv("TESTPLANE_GENET_INSECURE", "false")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DeployProvider != "genet" || c.GenetBaseURL != "https://genet.example" || c.GenetAPIKey != "k" {
			t.Errorf("config = %+v", c)
		}
		if c.GenetInsecure {
			t.Errorf("insecure must default to false")
		}
	})
	t.Run("非法 provider", func(t *testing.T) {
		t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/db")
		t.Setenv("TESTPLANE_DEPLOY_PROVIDER", "bogus")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for unknown provider")
		}
	})
}

// D-63：TESTPLANE_SHARED_MOUNTS 解析与校验。
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
