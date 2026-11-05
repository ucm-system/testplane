package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// finishSingleTask 跑完"唯一 Task 成功"的链路，返回 FinishAttempt 结果。
func finishSingleTask(t *testing.T, st *Store, runID, taskID, attemptID string) *AttemptFinishResult {
	t.Helper()
	out, err := st.FinishAttempt(context.Background(), AttemptFinish{
		AttemptID: attemptID, TaskID: taskID, RunID: runID,
		Succeeded: true, Phase: "succeeded", FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}
	return out
}

// TestPreCheckPassed_PrecheckOffWithPrecheckTester 票 #19 验收①：
// `precheck:"off"` + seq=0 且 Tester 是 role=pre_check 时，**不得**判成 Pre-Check 通过。
func TestPreCheckPassed_PrecheckOffWithPrecheckTester(t *testing.T) {
	st := requireStore(t)
	seedTesterRole(t, st, "endpoint-probe-off", "1.0.0", domain.RolePreCheck, domain.OnFailureAbortRun)
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "endpoint-probe-off", "1.0.0",
		domain.OnFailureAbortRun, false) // injected=false

	out := finishSingleTask(t, st, runID, taskID, attemptID)
	if out.PreCheckPassed {
		t.Fatal("precheck:\"off\" 时 seq=0 的测试 Task 不能被判成 Pre-Check 通过")
	}
}

// TestPreCheckPassed_InjectedWorksWithoutTesterRow 票 #19 验收②：
// 注入的 Pre-Check 完成后必须 PreCheckPassed=true，且**不依赖 testers 表里有该 Tester
// （历史静默 bug：注册表未落库时 Pre-Check 门永远开不了）。
func TestPreCheckPassed_InjectedWorksWithoutTesterRow(t *testing.T) {
	st := requireStore(t)
	// 故意**不** seed testers 行。
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "not-in-registry", "9.9.9",
		domain.OnFailureAbortRun, true) // injected=true

	out := finishSingleTask(t, st, runID, taskID, attemptID)
	if !out.PreCheckPassed {
		t.Fatal("注入的 Pre-Check 成功但 PreCheckPassed=false（判据还在看 testers 表？）")
	}
}

// TestPreCheckPassed_InjectedFalseWithRoleRow 反向对照：注入=true + 角色行存在 → true；
// 证明结论只由 resolved 快照决定，与角色行无关（有/无都同结论）。
func TestPreCheckPassed_InjectedWithRoleRow(t *testing.T) {
	st := requireStore(t)
	seedTesterRole(t, st, "endpoint-probe-on", "1.0.0", domain.RolePreCheck, domain.OnFailureAbortRun)
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "endpoint-probe-on", "1.0.0",
		domain.OnFailureAbortRun, true)

	out := finishSingleTask(t, st, runID, taskID, attemptID)
	if !out.PreCheckPassed {
		t.Fatal("注入的 Pre-Check 应 PreCheckPassed=true")
	}
	// 同时确认 resolved 快照确实是判据来源。
	var resolved []byte
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT resolved FROM runs WHERE id=$1`, runID).Scan(&resolved); err != nil {
		t.Fatalf("读 resolved: %v", err)
	}
	var snap struct {
		Precheck struct {
			Injected bool `json:"injected"`
		} `json:"precheck"`
	}
	if err := json.Unmarshal(resolved, &snap); err != nil || !snap.Precheck.Injected {
		t.Fatalf("resolved.precheck.injected 应为 true: %v (%s)", err, string(resolved))
	}
}
