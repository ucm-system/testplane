package dbtest

import (
	"context"
	"strings"
	"testing"
)

// 安全闸门：测试库名必须含 `_test`，不同 purpose 派生不同库（避免互清数据）。
// v2 更名（D-66）：测试库命名空间从 testplane_test* 迁到 argus_test*。
func TestDeriveName(t *testing.T) {
	cases := []struct{ db, purpose, want string }{
		{"argus", "store", "argus_test_store"},
		{"argus_test", "store", "argus_test_store"},
		{"argus_test_store", "store", "argus_test_store"},
		{"argus", "", "argus_test"},
		{"argus_test", "", "argus_test"},
		{"ag_dev", "api", "ag_dev_test_api"},
	}
	for _, c := range cases {
		if got := deriveName(c.db, c.purpose); got != c.want {
			t.Errorf("deriveName(%q,%q) = %q, 期望 %q", c.db, c.purpose, got, c.want)
		}
	}
}

func TestDeriveNameAlwaysContainsTest(t *testing.T) {
	for _, db := range []string{"argus", "myprod", "ag"} {
		for _, p := range []string{"", "store", "api"} {
			if got := deriveName(db, p); !strings.Contains(got, "_test") {
				t.Fatalf("deriveName(%q,%q) = %q 不含 _test（安全闸门会放行 DDL）", db, p, got)
			}
		}
	}
}

func TestResetSchema_RefusesNonTestDatabase(t *testing.T) {
	// DSN 指向 dev 库名时必须直接拒绝，且不尝试连库。
	_, err := ResetSchema(context.Background(),
		"postgres://u:p@127.0.0.1:5432/argus?sslmode=disable")
	if err == nil {
		t.Fatal("非测试库必须被拒绝（否则会 drop dev 数据）")
	}
	if !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("错误信息应说明拒绝原因: %v", err)
	}
}

func TestIsTestDatabase(t *testing.T) {
	if IsTestDatabase("postgres://u:p@h:5432/argus") {
		t.Fatal("dev 库不应被判为测试库")
	}
	if !IsTestDatabase("postgres://u:p@h:5432/argus_test_store") {
		t.Fatal("测试库应被判为测试库")
	}
}
