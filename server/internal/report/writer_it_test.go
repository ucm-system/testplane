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

// TestWriteRunReport_Files 生成 report.json / report.md / metadata.json / events.jsonl，
// 并重建 artifacts 索引（C-7.2、C-7.3、C-7.4、C-7.5）。需要真 PG。
func TestWriteRunReport_Files(t *testing.T) {
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
	w := &Writer{Store: st, Root: root, Logger: nil}

	runID := store.NewRunID(time.Now())
	okTask := &store.Task{
		ID: store.NewTaskID(), Seq: 1, Name: "accuracy",
		TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
		Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
		TimeoutSeconds: 600, Status: string(domain.TaskSucceeded),
	}
	badTask := &store.Task{
		ID: store.NewTaskID(), Seq: 2, Name: "perf",
		TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
		Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
		TimeoutSeconds: 600, Status: string(domain.TaskFailed),
		Error: []byte(`{"code":"TESTER_NONZERO_EXIT","message":"boom","retryable":false}`),
	}
	run := &store.Run{
		ID: runID, Name: "report-demo", Kind: "run",
		Manifest:      []byte(`{"run":{"name":"report-demo"}}`),
		Resolved:      []byte(`{"kind":"run","tasks":[]}`),
		CleanupPolicy: "always", SubmittedBy: "dev",
	}
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: run, Tasks: []*store.Task{okTask, badTask}, Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// 落一个 attempt，让 report.json 的 attempts/duration 有内容。
	if _, err := st.Attempts.Create(ctx, nil, store.CreateAttemptParams{TaskID: okTask.ID}); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// 造一个 result.json 让产物索引有 result 类型。
	resultDir := filepath.Join(root, runID, "tasks", okTask.ID, "attempts", "1")
	if err := os.MkdirAll(resultDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "result.json"),
		[]byte(`{"schema_version":"1.0"}`), 0o640); err != nil {
		t.Fatalf("write result: %v", err)
	}

	path, err := w.WriteRunReport(ctx, runID)
	if err != nil {
		t.Fatalf("WriteRunReport: %v", err)
	}
	if path == "" {
		t.Fatal("返回路径为空")
	}
	for _, name := range []string{"report.json", "report.md", "metadata.json", "events.jsonl"} {
		full := filepath.Join(root, runID, name)
		if info, err := os.Stat(full); err != nil || info.Size() == 0 {
			t.Fatalf("%s 未生成或为空: err=%v", name, err)
		}
	}

	// report.json 的形状（ARCHITECTURE §8.4）
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 report.json: %v", err)
	}
	var rep struct {
		RunID  string         `json:"run_id"`
		Status string         `json:"status"`
		Counts map[string]int `json:"counts"`
		Tasks  []struct {
			TaskID   string `json:"task_id"`
			Status   string `json:"status"`
			Attempts int    `json:"attempts"`
		} `json:"tasks"`
		Failures []struct {
			TaskID    string `json:"task_id"`
			ErrorCode string `json:"error_code"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("解析 report.json: %v", err)
	}
	if rep.RunID != runID {
		t.Fatalf("report.run_id = %s", rep.RunID)
	}
	if rep.Counts["total"] != 2 || rep.Counts["succeeded"] != 1 || rep.Counts["failed"] != 1 {
		t.Fatalf("counts 异常: %+v", rep.Counts)
	}
	if len(rep.Failures) != 1 || rep.Failures[0].TaskID != badTask.ID ||
		rep.Failures[0].ErrorCode != "TESTER_NONZERO_EXIT" {
		t.Fatalf("failures 异常: %+v", rep.Failures)
	}

	// report.md 模板要点（C-7.3）
	md, err := os.ReadFile(filepath.Join(root, runID, "report.md"))
	if err != nil {
		t.Fatalf("读 report.md: %v", err)
	}
	for _, want := range []string{"# Run " + runID, "## Task", "## 失败与跳过", "## 产物与报告"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("report.md 缺少 %q", want)
		}
	}

	// events.jsonl 必须按 seq 升序，且第一条是 run.submitted。
	lines := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(root, runID, "events.jsonl"))), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("events.jsonl 为空")
	}
	var first struct {
		Seq  int64  `json:"seq"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("解析 events.jsonl 首行: %v", err)
	}
	if first.Type != domain.EventRunSubmitted || first.Seq != 1 {
		t.Fatalf("首行事件 = %+v", first)
	}

	// 产物索引（artifacts 表）
	arts, err := st.Artifacts.ListByRun(ctx, nil, runID)
	if err != nil {
		t.Fatalf("列产物: %v", err)
	}
	var foundResult bool
	for _, a := range arts {
		if strings.HasSuffix(a.Path, "result.json") && a.Kind == "result" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Fatalf("artifacts 索引缺少 result 条目: %+v", arts)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}
