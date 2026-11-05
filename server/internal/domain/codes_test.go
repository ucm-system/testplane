package domain

import "testing"

// 部署失败只折叠成两个码（附录 D，D-46/D-47）：DEPLOY_FAILED 不可重试、DEPLOY_BLOCKED 可重试。
func TestDeployFailureCodes(t *testing.T) {
	cases := []struct {
		code      string
		retryable bool
	}{
		{CodeDeployFailed, false},
		{CodeDeployBlocked, true},
	}
	for _, c := range cases {
		if !KnownCode(c.code) {
			t.Fatalf("%s 必须是附录 D 的枚举值（KnownCode=false 会导致 Failure 校验失败）", c.code)
		}
		if got := Retryable(c.code); got != c.retryable {
			t.Fatalf("Retryable(%s) = %v, 期望 %v", c.code, got, c.retryable)
		}
		if f := NewFailure(c.code, "x", nil); f.Retryable != c.retryable {
			t.Fatalf("NewFailure(%s).Retryable = %v", c.code, f.Retryable)
		}
	}
	// 不要有第三个部署码：超窗归 DEPLOY_BLOCKED + detail.reason=timeout。
	if KnownCode("DEPLOY_TIMEOUT") {
		t.Fatal("DEPLOY_TIMEOUT 不应存在（超窗用 DEPLOY_BLOCKED + detail.reason=timeout）")
	}
}
