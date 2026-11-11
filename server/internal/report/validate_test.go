package report

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
)

func writeOut(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const validResult2 = `{
  "schema_version": "2.0",
  "task_id": "t-20261110-0001",
  "attempt_no": 1,
  "runner_seq": 3,
  "runner": {"name": "aisbench-perf", "version": "1.0.0"},
  "status": "succeeded",
  "started_at": "2026-11-10T10:00:00+08:00",
  "finished_at": "2026-11-10T10:14:20+08:00",
  "metrics": [
    {"name": "ttft", "value": 125.6, "unit": "ms", "statistic": "mean",
     "labels": {"input_len": 1024}, "sample_size": 400}
  ]
}`

func key() domain.RunnerKey {
	return domain.RunnerKey{TaskID: "t-20261110-0001", AttemptNo: 1, Seq: 3}
}

// D-74 第①段：三选一存在性。
func TestValidate_MissingThreeChoice(t *testing.T) {
	v := &Validator{MaxBytes: 1 << 20}
	dir := filepath.Join(t.TempDir(), "nope")
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil {
		t.Fatal("目录不存在应返回失败")
	} else if f.Code != domain.CodeResultMissing {
		t.Fatalf("code = %s, 期望 RESULT_MISSING", f.Code)
	}

	// 三选一全缺（空目录）。
	dir = t.TempDir()
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultMissing {
		t.Fatalf("全缺应 RESULT_MISSING，得到 %+v", f)
	}

	// 0 字节 = 缺失。
	writeOut(t, dir, "result.json", "")
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultMissing {
		t.Fatalf("0 字节应 RESULT_MISSING，得到 %+v", f)
	}

	// 只有 result.html 也满足三选一（D-74：不强求 result.json）。
	writeOut(t, dir, "result.json", "")
	writeOut(t, dir, "result.html", "<html>ok</html>")
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f != nil {
		t.Fatalf("仅 html 应通过第①段（不校验内容），得到 %+v", f)
	}

	// deploy/clean 无产物契约。
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleDeploy, t.TempDir(), nil); f != nil {
		t.Fatalf("deploy 角色不应要求产物: %+v", f)
	}
}

// D-74 第②段：result.json 存在时的 schema 2.0 校验。
func TestValidate_InvalidResultJSON(t *testing.T) {
	v := &Validator{MaxBytes: 1 << 20}
	dir := t.TempDir()

	// JSON 非法。
	writeOut(t, dir, "result.json", "{broken")
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("非法 JSON 应 RESULT_INVALID，得到 %+v", f)
	}

	// schema_version 不是 2.0。
	writeOut(t, dir, "result.json", strings.Replace(validResult2, `"schema_version": "2.0"`, `"schema_version": "1.0"`, 1))
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("schema_version=1.0 应 RESULT_INVALID，得到 %+v", f)
	}

	// 缺 runner{}。
	writeOut(t, dir, "result.json", strings.Replace(validResult2, `"runner": {"name": "aisbench-perf", "version": "1.0.0"},`, "", 1))
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("缺 runner 应 RESULT_INVALID，得到 %+v", f)
	}

	// 缺 metrics。
	writeOut(t, dir, "result.json", strings.Replace(validResult2, `"metrics": [`, `"metrics_x": [`, 1))
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("缺 metrics 应 RESULT_INVALID，得到 %+v", f)
	}

	// 标识与执行记录不一致。
	writeOut(t, dir, "result.json", strings.Replace(validResult2, `"runner_seq": 3`, `"runner_seq": 4`, 1))
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("runner_seq 不一致应 RESULT_INVALID，得到 %+v", f)
	}

	// metric 唯一性：name+statistic+labels 重复。
	dup := `{
		"schema_version": "2.0", "task_id": "t-20261110-0001", "attempt_no": 1, "runner_seq": 3,
		"runner": {"name": "p", "version": "1"}, "status": "succeeded",
		"started_at": "2026-11-10T10:00:00Z", "finished_at": "2026-11-10T10:14:20Z",
		"metrics": [
			{"name": "ttft", "value": 1, "unit": "ms", "statistic": "mean", "labels": {"a": 1}},
			{"name": "ttft", "value": 2, "unit": "ms", "statistic": "mean", "labels": {"a": 1}}
		]
	}`
	writeOut(t, dir, "result.json", dup)
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, nil); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("重复 metric 应 RESULT_INVALID，得到 %+v", f)
	}
}

func TestValidate_ValidResultAndExitConsistency(t *testing.T) {
	v := &Validator{MaxBytes: 1 << 20}
	dir := t.TempDir()

	// 合法 result.json + exit 0 → 通过。
	writeOut(t, dir, "result.json", validResult2)
	zero := int32(0)
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, &zero); f != nil {
		t.Fatalf("合法结果应通过: %+v", f)
	}

	// exit 0 但 status=failed → RESULT_INVALID（契约不一致，§5.8）。
	writeOut(t, dir, "result.json", strings.Replace(validResult2, `"status": "succeeded"`, `"status": "failed"`, 1))
	if f := v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, &zero); f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("exit0+failed 应 RESULT_INVALID，得到 %+v", f)
	}

	// 非零退出 + result.error.code 属附录 D → 用该码。
	writeOut(t, dir, "result.json", strings.Replace(validResult2,
		`"metrics": [`, `"error": {"code": "ENDPOINT_UNREACHABLE", "message": "refused", "retryable": true}, "metrics": [`, 1))
	one := int32(1)
	f := v.ValidateRunnerOutput(context.Background(), key(), domain.RolePreCheck, dir, &one)
	if f == nil || f.Code != domain.CodeEndpointUnreachable || !f.Retryable {
		t.Fatalf("非零退出应按 result.error.code 归类，得到 %+v", f)
	}

	// 非零退出、无 error.code → 回退 RUNNER_NONZERO_EXIT。
	writeOut(t, dir, "result.json", validResult2)
	f = v.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, &one)
	if f == nil || f.Code != domain.CodeRunnerNonzeroExit {
		t.Fatalf("应回退 RUNNER_NONZERO_EXIT，得到 %+v", f)
	}

	// 超技术上限 → RESULT_INVALID（detail=too_large）。
	v2 := &Validator{MaxBytes: 4}
	f = v2.ValidateRunnerOutput(context.Background(), key(), domain.RoleTest, dir, &zero)
	if f == nil || f.Code != domain.CodeResultInvalid {
		t.Fatalf("超限应 RESULT_INVALID，得到 %+v", f)
	}
}

// D-74 / §5.12：exports.env 词法解析。
func TestParseExportsEnv(t *testing.T) {
	kvs, err := ParseExportsEnv("# 注释\nAG_SERVED_URL=http://10.0.0.10:8000/v1\r\n\nAG_SERVED_MODEL_NAME=Qwen3-32B\nBAD KEY=x\n")
	if err == nil {
		t.Fatalf("非法 KEY 应整体拒绝，得到 %v", kvs)
	}

	kvs, err = ParseExportsEnv(
		"# 平台收割示例\n" +
			"AG_SERVED_URL=http://141.111.32.68:23345\n" +
			"AG_SERVED_MODEL_NAME=Qwen3-32B\n" +
			"AG_MODEL_PATH=/models/Qwen3-32B\n" +
			"AG_DEPLOY_REF=arg-3f9c1a2b7d40\n")
	if err != nil {
		t.Fatalf("ParseExportsEnv: %v", err)
	}
	m := KVMap(kvs)
	if m["AG_SERVED_URL"] != "http://141.111.32.68:23345" || len(m) != 4 {
		t.Fatalf("map = %v", m)
	}
	if len(KVKeys(kvs)) != 4 || KVKeys(kvs)[0] != "AG_SERVED_URL" {
		t.Fatalf("keys = %v（保序）", KVKeys(kvs))
	}
	if kvs[0].Line != 2 {
		t.Fatalf("line = %d, want 2（跳过注释行）", kvs[0].Line)
	}
}

func TestParseExportsEnv_Rejects(t *testing.T) {
	cases := map[string]string{
		"无等号":       "JUST_A_KEY\n",
		"重复 key":    "A=1\nA=2\n",
		"值含换行":      "A=1\n2\n",
		"KEY 数字开头":  "1BAD=x\n",
		"KEY 含非法字符": "A-B=x\n",
		"空 KEY":     "=value\n",
		"值含 NUL":    "A=x\x00y\n",
	}
	for name, raw := range cases {
		if _, err := ParseExportsEnv(raw); err == nil {
			t.Errorf("%s 应被拒绝（EXPORT_INVALID）", name)
		}
	}
}

func TestParseExportsEnv_EmptyAndComments(t *testing.T) {
	for _, raw := range []string{"", "\n", "# only\n\n# another\n"} {
		kvs, err := ParseExportsEnv(raw)
		if err != nil || len(kvs) != 0 {
			t.Errorf("raw=%q → %v, %v", raw, kvs, err)
		}
	}
}
