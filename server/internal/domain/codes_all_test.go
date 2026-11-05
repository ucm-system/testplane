package domain

import (
	"sort"
	"testing"
)

// TestAllCodes 是票 #22-4 的验收前置：代码侧必须能**枚举**附录 D 全集，
// 供 tools/contract 与 openapi / 设计文档做三方集合相等校验。
func TestAllCodes(t *testing.T) {
	codes := AllCodes()
	if len(codes) == 0 {
		t.Fatal("AllCodes 不能为空")
	}
	if !sort.StringsAreSorted(codes) {
		t.Fatal("AllCodes 必须按字典序返回（差集比对与日志稳定）")
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if c == "" {
			t.Fatal("AllCodes 不能包含空码")
		}
		if seen[c] {
			t.Fatalf("AllCodes 有重复: %s", c)
		}
		seen[c] = true
		if !KnownCode(c) {
			t.Fatalf("AllCodes 里的 %s 不被 KnownCode 认可", c)
		}
	}
	// 票 #22-1：新增的执行层 Pod 错误码必须在集合里且可重试。
	if !seen[CodeExecutorPodError] {
		t.Fatalf("AllCodes 缺少 %s", CodeExecutorPodError)
	}
	if !Retryable(CodeExecutorPodError) {
		t.Fatalf("%s 必须是可重试（附录 D）", CodeExecutorPodError)
	}
	// 票 #22-2：部署两码必须被 KnownCode 认可（调用方已产出，代码侧备好）。
	for _, c := range []string{CodeDeployFailed, CodeDeployBlocked} {
		if !seen[c] {
			t.Fatalf("AllCodes 缺少 %s", c)
		}
	}
}
