package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/store"
)

// TestAttemptByTask_FillsRunID 是 QA restart-drill（Run r-20261105-RGWX）的回归：
// 对账路径拿到的 AttemptRef 必须带 RunID，否则结算时拼出的产物 subPath 为空，
// validator 会去读 `<storage-root>/result.json` 而不是
// `<storage-root>/<run>/tasks/<task>/attempts/<n>/result.json`。
func TestAttemptByTask_FillsRunID(t *testing.T) {
	dsn, err := dbtest.URL("cmd")
	if err != nil {
		t.Fatalf("准备测试库: %v", err)
	}
	if dsn == "" {
		t.Skip("未设置 TESTPLANE_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	defer st.Close()

	root := t.TempDir()
	runID := store.NewRunID(time.Now())
	taskID := store.NewTaskID()
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "restart-drill", Kind: "run",
			Manifest: []byte(`{}`), Resolved: []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "t1", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			OnFailure: domain.OnFailureContinue, TimeoutSeconds: 60,
			Status: string(domain.TaskPending),
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// 造一个"进行中"的 Attempt（AttemptByTask 只认 preparing/running）。
	att, err := st.Attempts.Create(ctx, nil, store.CreateAttemptParams{TaskID: taskID})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	repos := &storeRepos{st: st}
	ref, err := repos.AttemptByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("AttemptByTask: %v", err)
	}
	if ref.AttemptID != att.ID || ref.AttemptNo != att.Idx {
		t.Fatalf("AttemptRef 身份不对: %+v (want id=%s idx=%d)", ref, att.ID, att.Idx)
	}
	if ref.RunID != runID {
		t.Fatalf("AttemptRef.RunID = %q, 期望 %q（漏填会让产物读成 <root>/result.json）", ref.RunID, runID)
	}
	if ref.TaskID != taskID {
		t.Fatalf("AttemptRef.TaskID = %q, 期望 %q", ref.TaskID, taskID)
	}

	// 模拟结算路径：用 ref 拼 subPath，写 result.json，再从拼出的目录读回来。
	sub, err := executor.OutputSubPath(ref.RunID, ref.TaskID, ref.AttemptNo)
	if err != nil {
		t.Fatalf("OutputSubPath(%q,%q,%d): %v", ref.RunID, ref.TaskID, ref.AttemptNo, err)
	}
	if sub != runID+"/tasks/"+taskID+"/attempts/1" {
		t.Fatalf("subPath = %q, 期望 %s/tasks/%s/attempts/1", sub, runID, taskID)
	}
	dir := filepath.Join(root, filepath.FromSlash(sub))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	result := map[string]any{
		"schema_version": "1.0", "run_id": runID, "task_id": taskID, "attempt_id": att.ID,
		"tester": map[string]any{"name": "probe", "version": "1.0.0"},
		"status": "succeeded", "started_at": "2026-11-05T01:00:00Z",
		"finished_at": "2026-11-05T01:00:10Z", "metrics": []any{},
	}
	b, _ := json.Marshal(result)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o640); err != nil {
		t.Fatalf("write result.json: %v", err)
	}

	// 断言"读到的是子目录那份，而不是 <root>/result.json"。
	got := filepath.Join(root, filepath.FromSlash(sub), "result.json")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("按 ref 拼出的 result.json 不存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "result.json")); err == nil {
		t.Fatal("不应在 <storage-root> 根目录出现 result.json（正是漏填 RunID 的症状）")
	}
}
