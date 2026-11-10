package report

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 本文件是 Writer 的 DB 集成测试。需要 PostgreSQL：设置 AG_TEST_DATABASE_URL。
// 未设置时跳过——**不伪造通过**。

func newITStoreReport(t *testing.T) *store.Store {
	t.Helper()
	dsn, err := dbtest.URL("report")
	if err != nil {
		t.Fatalf("dbtest.URL: %v", err)
	}
	if dsn == "" {
		t.Skip("AG_TEST_DATABASE_URL 未设置，跳过需要 Postgres 的报告生成集成测试")
	}
	if _, err := dbtest.ResetSchema(context.Background(), dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	s, err := store.New(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestIT_WriteTaskReport(t *testing.T) {
	s := newITStoreReport(t)
	ctx := context.Background()
	now := time.Now()

	task := &store.Task{
		ID:          store.NewTaskID(now),
		Name:        "report-it",
		Manifest:    []byte(`{"task":{"name":"report-it","service":{"url":"http://x/v1"}}}`),
		Resolved:    []byte(`{"precheck":{"injected":false},"runners":[{"seq":1,"name":"acc","runner":"aisbench-acc","version":"1.0.0","role":"test"}],"schema_version":"2.0"}`),
		Env:         []byte(`{}`),
		SecretEnv:   []byte(`[]`),
		SubmittedBy: "it",
	}
	runners := []*store.RunnerRun{
		{Seq: 1, Name: "acc", RunnerName: "aisbench-acc", RunnerVersion: "1.0.0",
			Role: domain.RoleTest, ImageRef: "img:1", Profile: "k8s-npu",
			InputRef: "attempts/1/input/1", OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 600, Status: string(domain.RunnerPending)},
	}
	if _, _, err := s.SubmitTask(ctx, store.SubmitParams{Task: task, Runners: runners, Actor: "it"}); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	m := store.NewMachines(s, nil)
	_, _ = m.Tasks.Enqueue(ctx, task.ID, "it")
	if _, err := m.Tasks.BeginPipeline(ctx, task.ID, "it"); err != nil {
		t.Fatalf("BeginPipeline: %v", err)
	}
	k := domain.RunnerKey{TaskID: task.ID, AttemptNo: 1, Seq: 1}
	if _, err := m.Runners.Queue(ctx, k); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if _, err := m.Runners.Prepare(ctx, k, store.PrepareParams{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := m.Runners.Start(ctx, k); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.FinishRunnerRun(ctx, store.RunnerFinish{
		Key: k, Succeeded: true, ExitCode: intPtr(0), Phase: "succeeded",
		ResultRef:  "attempts/1/runners/1/result.json",
		LogRef:     "attempts/1/runners/1/logs/stdout.log",
		FinishedAt: time.Now(),
	}); err != nil {
		t.Fatalf("FinishRunnerRun: %v", err)
	}

	root := t.TempDir()
	// 造一份产物文件供索引。
	outDir := root + "/" + task.ID + "/attempts/1/runners/1"
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outDir+"/result.json", []byte(validResult2), 0o640); err != nil {
		t.Fatal(err)
	}

	w := &Writer{Store: s, Root: root, MaxReportBytes: 20 << 20}
	path, err := w.WriteTaskReport(ctx, task.ID)
	if err != nil {
		t.Fatalf("WriteTaskReport: %v", err)
	}
	if path == "" {
		t.Fatal("report.json 路径为空")
	}
	for _, name := range []string{"report.json", "report.md", "metadata.json", "events.jsonl"} {
		if _, err := os.Stat(root + "/" + task.ID + "/" + name); err != nil {
			t.Errorf("缺 %s: %v", name, err)
		}
	}
	// 幂等：再写一遍不报错。
	if _, err := w.WriteTaskReport(ctx, task.ID); err != nil {
		t.Fatalf("WriteTaskReport#2: %v", err)
	}

	// 产物索引已重建。
	arts, err := s.Artifacts.ListByRunner(ctx, nil, task.ID, 1, 1)
	if err != nil {
		t.Fatalf("ListByRunner: %v", err)
	}
	found := false
	for _, a := range arts {
		if a.Path == "attempts/1/runners/1/result.json" && a.Kind == "result" {
			found = true
		}
	}
	if !found {
		t.Fatalf("产物索引缺 result.json: %+v", arts)
	}
}

func intPtr(i int) *int { return &i }
