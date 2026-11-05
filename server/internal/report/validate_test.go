package report

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
)

func writeResult(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "result.json")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("write result.json: %v", err)
	}
	return dir
}

const validResult = `{
  "schema_version": "1.0",
  "run_id": "r-1",
  "task_id": "t-1",
  "attempt_id": "t-1-a1",
  "tester": {"name": "probe", "version": "1.0.0"},
  "status": "succeeded",
  "started_at": "2026-11-05T01:00:00Z",
  "finished_at": "2026-11-05T01:00:10Z",
  "metrics": [{"name": "ttft", "value": 12.5, "unit": "ms", "statistic": "mean"}]
}`

func TestValidate_Missing(t *testing.T) {
	v := &Validator{MaxBytes: 1 << 20}
	// 目录不存在
	dir := filepath.Join(t.TempDir(), "nope")
	if f := v.Validate(context.Background(), "r-1", "t-1", "t-1-a1", dir); f == nil {
		t.Fatal("目录不存在应返回失败")
	} else if f.Code != domain.CodeResultMissing {
		t.Fatalf("code = %s, 期望 RESULT_MISSING", f.Code)
	}

	// 文件缺失
	dir = t.TempDir()
	f := v.Validate(context.Background(), "r-1", "t-1", "t-1-a1", dir)
	if f == nil || f.Code != domain.CodeResultMissing {
		t.Fatalf("缺 result.json 应 RESULT_MISSING，得到 %+v", f)
	}

	// 0 字节
	writeResult(t, dir, "")
	if f := v.Validate(context.Background(), "r-1", "t-1", "t-1-a1", dir); f == nil || f.Code != domain.CodeResultMissing {
		t.Fatalf("0 字节应 RESULT_MISSING，得到 %+v", f)
	}
}

func TestValidate_Invalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{not json`},
		{"缺 schema_version", `{"run_id":"r","task_id":"t","attempt_id":"a","tester":{"name":"n","version":"1"},"status":"succeeded","started_at":"x","finished_at":"y","metrics":[]}`},
		{"schema_version 不是 1.0", `{"schema_version":"2.0","run_id":"r","task_id":"t","attempt_id":"a","tester":{"name":"n","version":"1"},"status":"succeeded","started_at":"x","finished_at":"y","metrics":[]}`},
		{"缺 metrics", `{"schema_version":"1.0","run_id":"r","task_id":"t","attempt_id":"a","tester":{"name":"n","version":"1"},"status":"succeeded","started_at":"x","finished_at":"y"}`},
		{"status 非枚举", `{"schema_version":"1.0","run_id":"r","task_id":"t","attempt_id":"a","tester":{"name":"n","version":"1"},"status":"weird","started_at":"x","finished_at":"y","metrics":[]}`},
		{"metric 重复", `{"schema_version":"1.0","run_id":"r","task_id":"t","attempt_id":"a","tester":{"name":"n","version":"1"},"status":"succeeded","started_at":"x","finished_at":"y","metrics":[{"name":"m","value":1,"unit":"ms","statistic":"mean"},{"name":"m","value":2,"unit":"ms","statistic":"mean"}]}`},
		{"statistic 非法", `{"schema_version":"1.0","run_id":"r","task_id":"t","attempt_id":"a","tester":{"name":"n","version":"1"},"status":"succeeded","started_at":"x","finished_at":"y","metrics":[{"name":"m","value":1,"unit":"ms","statistic":"median"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeResult(t, t.TempDir(), c.body)
			v := &Validator{MaxBytes: 1 << 20}
			f := v.Validate(context.Background(), "r-1", "t-1", "t-1-a1", dir)
			if f == nil {
				t.Fatal("应返回失败")
			}
			if f.Code != domain.CodeResultInvalid {
				t.Fatalf("code = %s, 期望 RESULT_INVALID（%s）", f.Code, f.Message)
			}
		})
	}
}

func TestValidate_TooLarge(t *testing.T) {
	dir := writeResult(t, t.TempDir(), validResult)
	v := &Validator{MaxBytes: 10}
	f := v.Validate(context.Background(), "r-1", "t-1", "t-1-a1", dir)
	if f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("超限应 RESULT_INVALID，得到 %+v", f)
	}
	if f.Detail["too_large"] != true {
		t.Fatalf("缺少 detail.too_large: %+v", f.Detail)
	}
}

func TestValidate_ExitCodeConsistency(t *testing.T) {
	dir := writeResult(t, t.TempDir(), validResult)
	v := &Validator{MaxBytes: 1 << 20}
	zero := int32(0)
	if f := v.ValidateExit(context.Background(), "r-1", "t-1", "t-1-a1", dir, &zero); f != nil {
		t.Fatalf("合法结果不应失败: %+v", f)
	}

	// 退出码 0 但 status=failed → RESULT_INVALID（契约不一致）。
	failedBody := `{"schema_version":"1.0","run_id":"r","task_id":"t","attempt_id":"a",
	  "tester":{"name":"n","version":"1"},"status":"failed","started_at":"x","finished_at":"y",
	  "metrics":[],"error":{"code":"PRECHECK_FAILED","message":"boom","retryable":true}}`
	dir = writeResult(t, t.TempDir(), failedBody)
	if f := v.ValidateExit(context.Background(), "r-1", "t-1", "t-1-a1", dir, &zero); f == nil ||
		f.Code != domain.CodeResultInvalid {
		t.Fatalf("exit=0 但 status=failed 应 RESULT_INVALID，得到 %+v", f)
	}

	// 非零退出：优先用 result.error.code。
	dir = writeResult(t, t.TempDir(), failedBody)
	one := int32(1)
	f := v.ValidateExit(context.Background(), "r-1", "t-1", "t-1-a1", dir, &one)
	if f == nil || f.Code != domain.CodePrecheckFailed {
		t.Fatalf("应使用 result.error.code，得到 %+v", f)
	}
	if !f.Retryable {
		t.Fatalf("实例级 retryable 应保留")
	}

	// 非零退出且无 error → TESTER_NONZERO_EXIT。
	dir = writeResult(t, t.TempDir(), validResult)
	f = v.ValidateExit(context.Background(), "r-1", "t-1", "t-1-a1", dir, &one)
	if f == nil || f.Code != domain.CodeTesterNonzeroExit {
		t.Fatalf("应兜底 TESTER_NONZERO_EXIT，得到 %+v", f)
	}

	// 非零退出且 error.code 不在附录 D → 仍兜底 TESTER_NONZERO_EXIT。
	dir = writeResult(t, t.TempDir(), `{"schema_version":"1.0","run_id":"r","task_id":"t","attempt_id":"a",
	  "tester":{"name":"n","version":"1"},"status":"failed","started_at":"x","finished_at":"y",
	  "metrics":[],"error":{"code":"MY_CUSTOM","message":"x","retryable":true}}`)
	f = v.ValidateExit(context.Background(), "r-1", "t-1", "t-1-a1", dir, &one)
	if f == nil || f.Code != domain.CodeTesterNonzeroExit {
		t.Fatalf("未知码应兜底 TESTER_NONZERO_EXIT，得到 %+v", f)
	}
}

func TestValidate_Valid(t *testing.T) {
	dir := writeResult(t, t.TempDir(), validResult)
	v := &Validator{MaxBytes: 1 << 20}
	if f := v.Validate(context.Background(), "r-1", "t-1", "t-1-a1", dir); f != nil {
		t.Fatalf("合法结果不应失败: %+v", f)
	}
}

func TestLabelsKeyOrderInsensitive(t *testing.T) {
	a := labelsKey(map[string]any{"b": 1, "a": "x"})
	b := labelsKey(map[string]any{"a": "x", "b": 1})
	if a != b {
		t.Fatalf("labels key 应顺序无关: %s != %s", a, b)
	}
}
