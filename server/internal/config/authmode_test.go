package config

import (
	"strings"
	"testing"
)

// TestLoad_DevAuthRejectedOutsideDev 是票 #27-2 的验收：
// dev 鉴权 + staging/prod/production 必须**启动即失败**（06 §6.1）；缺省/dev 仍可加载。
func TestLoad_DevAuthRejectedOutsideDev(t *testing.T) {
	for _, envName := range []string{"staging", "prod", "production"} {
		t.Run("reject_"+envName, func(t *testing.T) {
			clearTestplaneEnv(t)
			t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/testplane")
			t.Setenv("TESTPLANE_AUTH_MODE", "dev")
			t.Setenv("TESTPLANE_ENV", envName)
			_, err := Load()
			if err == nil {
				t.Fatalf("TESTPLANE_ENV=%s + AUTH_MODE=dev 必须报错（不能只 WARN 继续跑）", envName)
			}
			if !strings.Contains(err.Error(), "TESTPLANE_AUTH_MODE=dev") {
				t.Fatalf("错误信息应指明 dev 模式不可用: %v", err)
			}
		})
	}

	// dev/local 仍可加载（保持现状）。
	for _, envName := range []string{"", "dev", "local"} {
		t.Run("allow_"+orDefault(envName, "empty"), func(t *testing.T) {
			clearTestplaneEnv(t)
			t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/testplane")
			t.Setenv("TESTPLANE_AUTH_MODE", "dev")
			t.Setenv("TESTPLANE_ENV", envName)
			if _, err := Load(); err != nil {
				t.Fatalf("TESTPLANE_ENV=%q 下 dev 鉴权应允许: %v", envName, err)
			}
		})
	}
}

// TestLoad_OIDCInProdIsAllowed oidc 在 prod 允许（只要有必需配置）。
func TestLoad_OIDCInProdIsAllowed(t *testing.T) {
	clearTestplaneEnv(t)
	t.Setenv("TESTPLANE_DATABASE_URL", "postgres://u:p@localhost:5432/testplane")
	t.Setenv("TESTPLANE_AUTH_MODE", "oidc")
	t.Setenv("TESTPLANE_ENV", "prod")
	t.Setenv("TESTPLANE_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("TESTPLANE_OIDC_CLIENT_ID", "testplane")
	t.Setenv("TESTPLANE_OIDC_REDIRECT_URL", "https://testplane.example.com/auth/callback")
	t.Setenv("TESTPLANE_SESSION_SECRET", strings.Repeat("a", 32))
	c, err := Load()
	if err != nil {
		t.Fatalf("oidc + prod 应可加载: %v", err)
	}
	if c.AuthMode != "oidc" {
		t.Fatalf("AuthMode = %q", c.AuthMode)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
