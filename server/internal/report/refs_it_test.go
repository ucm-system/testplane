package report

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// TestWriteRunReport_TaskResultRef 是票 UX-2026-11 的端到端验收：
// `tasks.result_ref` 落库后，report.json 的 `tasks[].result_ref` 必须非空、
// 指向**真实存在**的 result.json，report.md 的产物链接也不是死链。
//
// 改回去（不写 refs）这条必红：report.json 里 result_ref 缺失，指标表整块空。
func TestWriteRunReport_TaskResultRef(t *testing.T) {
	dsn, err := dbtest.URL("report")
	if err != nil {
		t.Fatalf("准备测试库: %v", err)
	}
	if dsn == "" {
		t.Skip("未设置 TESTPLANE_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	defer st.Close()

	root := t.TempDir()
	w := &Writer{Store: st, Root: root}

	runID := store.NewRunID(time.Now())
	taskID := store.NewTaskID()
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "report-refs", Kind: "run",
			Manifest: []byte(`{"run":{"name":"report-refs"}}`), Resolved: []byte(`{"kind":"run","tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 1, Name: "accuracy",
			TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 600, Status: string(domain.TaskSucceeded),
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.Attempts.Create(ctx, nil, store.CreateAttemptParams{TaskID: taskID}); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	// 产物真的落盘（否则就是"报告引用不存在的文件"）。
	attemptDir := filepath.Join(root, runID, "tasks", taskID, "attempts", "1")
	if err := os.MkdirAll(attemptDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(attemptDir, "result.json"),
		[]byte(`{"schema_version":"1.0","metrics":[{"name":"acc","value":0.9}]}`), 0o640); err != nil {
		t.Fatalf("write result: %v", err)
	}
	// 模拟编排层写 Task 级 ref（相对 Run 目录，03-data-view §3.1）。
	wantRef := "tasks/" + taskID + "/attempts/1/result.json"
	if err := st.SetTaskReuseRef(ctx, taskID, wantRef); err != nil {
		t.Fatalf("set task result_ref: %v", err)
	}

	path, err := w.WriteRunReport(ctx, runID)
	if err != nil {
		t.Fatalf("WriteRunReport: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 report.json: %v", err)
	}
	var rep struct {
		Tasks []struct {
			TaskID    string `json:"task_id"`
			ResultRef string `json:"result_ref"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("解析 report.json: %v", err)
	}
	if len(rep.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(rep.Tasks))
	}
	if rep.Tasks[0].ResultRef != wantRef {
		t.Fatalf("report.json tasks[0].result_ref = %q, want %q", rep.Tasks[0].ResultRef, wantRef)
	}
	// 不是死链：按 ref 相对 Run 目录能找到文件。
	if _, err := os.Stat(filepath.Join(root, runID, rep.Tasks[0].ResultRef)); err != nil {
		t.Fatalf("result_ref 指向的文件不存在（死链）: %v", err)
	}

	// report.md 里也要出现这条产物链接，且指向同一路径。
	md, err := os.ReadFile(filepath.Join(root, runID, "report.md"))
	if err != nil {
		t.Fatalf("读 report.md: %v", err)
	}
	if !strings.Contains(string(md), wantRef) {
		t.Fatalf("report.md 缺产物链接 %q", wantRef)
	}
}
