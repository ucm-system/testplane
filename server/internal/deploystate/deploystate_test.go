package deploystate

import (
	"strings"
	"testing"

	"github.com/ucm-system/testplane/server/internal/deploystate/fixtures"
	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestParsePhaseSpectrum 用 fixtures 的全谱样例验证 Parse 的白名单提取
// （execution §9.5：解析 + 校验 + 投影；不发 HTTP）。
func TestParsePhaseSpectrum(t *testing.T) {
	cases := []struct {
		name        string
		raw         []byte
		wantPhase   string
		wantGenet   string
		wantPods    int64
		wantExports int // exports 键数量（ready 四件套）
	}{
		{"queued", fixtures.Queued(), PhaseQueued, "pending-install", 1, 0},
		{"accepted", fixtures.Accepted(), PhaseAccepted, "pending-install", 0, 0},
		{"deploying", fixtures.Deploying(), PhaseDeploying, "deployed", 2, 0},
		{"ready", fixtures.Ready(), PhaseReady, "deployed", 2, 4},
		{"blocked", fixtures.Blocked(), PhaseBlocked, "failed", 1, 0},
		{"blocked_timeout", fixtures.BlockedTimeout(), PhaseBlocked, "pending-install", 0, 0},
		{"call_failed", fixtures.CallFailed(), PhaseBlocked, "", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if s.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", s.Phase, tc.wantPhase)
			}
			if s.GenetStatus != tc.wantGenet {
				t.Errorf("genet_status = %q, want %q", s.GenetStatus, tc.wantGenet)
			}
			if got := s.PodCount(); got != tc.wantPods {
				t.Errorf("pod_count = %d, want %d", got, tc.wantPods)
			}
			if len(s.Exports) != tc.wantExports {
				t.Errorf("exports = %d keys, want %d", len(s.Exports), tc.wantExports)
			}
			if s.ReleaseName != "" && !strings.HasPrefix(s.ReleaseName, "arg-") {
				t.Errorf("release_name %q must keep the arg- prefix (D-44)", s.ReleaseName)
			}
			if q := s.Queue; q != nil && q.Position != nil {
				t.Errorf("queue.position must be null (D-46)")
			}
		})
	}
}

// TestParseReadyExports 验证 ready 样例的四件套（§9.3 第 5 步的内置契约）。
func TestParseReadyExports(t *testing.T) {
	s, err := Parse(fixtures.Ready())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AG_SERVED_URL", "AG_SERVED_MODEL_NAME", "AG_MODEL_PATH", "AG_DEPLOY_REF"} {
		if v, ok := s.Exports[key]; !ok || strings.TrimSpace(v) == "" {
			t.Errorf("ready exports missing %s", key)
		}
	}
	if s.Exports["AG_DEPLOY_REF"] != "genet-ns-01/arg-9f3a1c2b7d40" {
		t.Errorf("AG_DEPLOY_REF = %q, want namespace/release_name", s.Exports["AG_DEPLOY_REF"])
	}
}

// TestErrorKindClassification 验证三值归类（genet_call_failed | blocked | timeout）。
func TestErrorKindClassification(t *testing.T) {
	cases := []struct {
		name      string
		raw       []byte
		wantKind  string
		wantCode  string
		wantRetry bool
	}{
		{"ready has no failure", fixtures.Ready(), "", "", false},
		{"deploying has no failure", fixtures.Deploying(), "", "", false},
		{"blocked → DEPLOY_BLOCKED", fixtures.Blocked(), ErrorKindBlocked, "DEPLOY_BLOCKED", true},
		{"timeout → DEPLOY_BLOCKED", fixtures.BlockedTimeout(), ErrorKindTimeout, "DEPLOY_BLOCKED", true},
		{"call failed → DEPLOY_FAILED", fixtures.CallFailed(), ErrorKindGenetCallFailed, "DEPLOY_FAILED", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.ErrorKind(); got != tc.wantKind {
				t.Errorf("ErrorKind = %q, want %q", got, tc.wantKind)
			}
			if got := s.FailureCode(); got != tc.wantCode {
				t.Errorf("FailureCode = %q, want %q", got, tc.wantCode)
			}
			if tc.wantCode != "" {
				if got := domain.Retryable(tc.wantCode); got != tc.wantRetry {
					t.Errorf("domain.Retryable(%s) = %v, want %v", tc.wantCode, got, tc.wantRetry)
				}
			}
		})
	}
}

// TestParseNegatives 验证结构校验拒绝坏文件（坏文件不判 runner 失败，下轮重读）。
func TestParseNegatives(t *testing.T) {
	for name, raw := range fixtures.Negatives() {
		t.Run(name, func(t *testing.T) {
			if s, err := Parse(raw); err == nil {
				t.Fatalf("Parse(%s) = %+v, want error", name, s)
			}
		})
	}
	t.Run("empty input", func(t *testing.T) {
		if _, err := Parse(nil); err == nil {
			t.Fatal("Parse(nil) should fail")
		}
	})
	t.Run("bad release name", func(t *testing.T) {
		raw := []byte(`{"phase":"ready","release_name":"Arg_Bad!"}`)
		if _, err := Parse(raw); err == nil {
			t.Fatal("release_name pattern must be enforced")
		}
	})
	t.Run("negative waited_seconds", func(t *testing.T) {
		raw := []byte(`{"phase":"queued","waited_seconds":-1}`)
		if _, err := Parse(raw); err == nil {
			t.Fatal("negative waited_seconds must be rejected")
		}
	})
	t.Run("export key not env name", func(t *testing.T) {
		raw := []byte(`{"phase":"ready","exports":{"not a key":"v"}}`)
		if _, err := Parse(raw); err == nil {
			t.Fatal("exports key must match env-name pattern")
		}
	})
	t.Run("export value with newline", func(t *testing.T) {
		raw := []byte(`{"phase":"ready","exports":{"AG_SERVED_URL":"http://x\ny"}}`)
		if _, err := Parse(raw); err == nil {
			t.Fatal("exports value with newline must be rejected (§5.12)")
		}
	})
	t.Run("unknown fields tolerated", func(t *testing.T) {
		// 前向兼容：runner 未来的新字段平台不认识也不能拒收。
		raw := []byte(`{"phase":"deploying","state_detail":"heuristic","future":{"a":1}}`)
		s, err := Parse(raw)
		if err != nil {
			t.Fatalf("unknown fields must be tolerated: %v", err)
		}
		if s.Phase != PhaseDeploying {
			t.Errorf("phase = %q", s.Phase)
		}
	})
}

// TestProject 验证 domain.DeployDoc 投影（store.RecordDeployState 的输入形状，
// 03-data-view §4.4）。
func TestProject(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		s, err := Parse(fixtures.Ready())
		if err != nil {
			t.Fatal(err)
		}
		doc := s.Project("attempts/1/deploy-state.json")
		if doc.Phase != PhaseReady || doc.GenetStatus != "deployed" {
			t.Errorf("doc phase/genet = %q/%q", doc.Phase, doc.GenetStatus)
		}
		if doc.DeployStateRef != "attempts/1/deploy-state.json" {
			t.Errorf("deploy_state_ref = %q", doc.DeployStateRef)
		}
		if doc.WaitedSeconds == nil || *doc.WaitedSeconds != s.WaitedSeconds {
			t.Errorf("waited_seconds = %v", doc.WaitedSeconds)
		}
		if doc.PodCount == nil || *doc.PodCount != 2 {
			t.Errorf("pod_count = %v, want 2", doc.PodCount)
		}
		if doc.LabelSelector != "helm-release-name=arg-9f3a1c2b7d40" {
			t.Errorf("label_selector = %q", doc.LabelSelector)
		}
		if doc.Queue == nil {
			t.Fatal("queue must be projected")
		}
		if doc.Queue.Position != nil {
			t.Error("queue.position must stay null (D-46)")
		}
		if doc.Failure != nil {
			t.Errorf("ready must not carry failure, got %+v", doc.Failure)
		}
	})
	t.Run("blocked timeout carries failure", func(t *testing.T) {
		s, err := Parse(fixtures.BlockedTimeout())
		if err != nil {
			t.Fatal(err)
		}
		doc := s.Project("")
		if doc.Failure == nil {
			t.Fatal("failure must be projected")
		}
		if doc.Failure.Code != "DEPLOY_BLOCKED" {
			t.Errorf("code = %q, want DEPLOY_BLOCKED", doc.Failure.Code)
		}
		if !doc.Failure.Retryable {
			t.Error("DEPLOY_BLOCKED must be retryable (附录 D)")
		}
		reason, _ := doc.Failure.Detail["reason"].(string)
		if reason != "timeout" {
			t.Errorf("detail.reason = %q, want timeout", reason)
		}
	})
	t.Run("call failed maps DEPLOY_FAILED", func(t *testing.T) {
		s, err := Parse(fixtures.CallFailed())
		if err != nil {
			t.Fatal(err)
		}
		doc := s.Project("")
		if doc.Failure == nil || doc.Failure.Code != "DEPLOY_FAILED" {
			t.Fatalf("failure = %+v, want DEPLOY_FAILED", doc.Failure)
		}
		if doc.Failure.Retryable {
			t.Error("DEPLOY_FAILED must not be retryable (附录 D)")
		}
	})
	t.Run("failure message keeps genet text", func(t *testing.T) {
		s, err := Parse(fixtures.Blocked())
		if err != nil {
			t.Fatal(err)
		}
		doc := s.Project("")
		if doc.Failure == nil || doc.Failure.Message != "release failed: install chart timeout" {
			t.Errorf("Genet 原文必须保留（D-47），got %+v", doc.Failure)
		}
	})
}

// TestProjectDoesNotTouchFrozenKeys 是防御性断言：投影结果只含 deploy-state
// 承载的键——冻结键（release_name/provider/spec/…）由 store 侧 jsonb `||` 保护，
// 这里保证 Project 不输出它们（否则会误导调用方做整段覆盖）。
func TestProjectDoesNotTouchFrozenKeys(t *testing.T) {
	s, err := Parse(fixtures.Ready())
	if err != nil {
		t.Fatal(err)
	}
	doc := s.Project("attempts/1/deploy-state.json")
	if doc.ReleaseName != "" || doc.Provider != "" || doc.Spec != "" || doc.SpecDigest != "" {
		t.Errorf("projection must not carry frozen keys: %+v", doc)
	}
}
