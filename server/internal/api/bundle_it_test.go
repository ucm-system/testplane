package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// bundleTestEnv 造一个终态 Run：报告三件套中的 report.json / metadata.json 落盘，
// report.md 故意不落（验证 MANIFEST.missing），并写两条产物 + 一条带 Secret 的事件。
func bundleTestEnv(t *testing.T) (srv *Server, st *store.Store, runID string, cleanup func()) {
	t.Helper()
	var cl func()
	srv, st, cl = integrationServer(t)
	cleanup = cl
	ctx := context.Background()

	runID = store.NewRunID(srv.now())
	taskID := store.NewTaskID()
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "bundle-demo", Kind: "run",
			Manifest:      []byte(`{"run":{"name":"bundle-demo"}}`),
			Resolved:      []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev", SubmittedAt: srv.now(),
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "t1", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			Status: string(domain.TaskSucceeded), CurrentAttempt: 1,
			EndpointKey: strPtr("http://ep:8000"), TimeoutSeconds: 60,
			OnFailure: domain.OnFailureContinue,
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status='succeeded', finished_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("置终态: %v", err)
	}

	dir := filepath.Join(srv.cfg.StorageRoot, runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"run_id":"`+runID+`"}`), 0o640); err != nil {
		t.Fatalf("write report.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"schema_version":"1.0"}`), 0o640); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	// 两条产物：一条文件真实存在，一条索引有但文件缺失（验证 missing）。
	artDir := filepath.Join(dir, "tasks", taskID, "attempts", "1")
	if err := os.MkdirAll(artDir, 0o750); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "result.json"), []byte(`{"schema_version":"1.0"}`), 0o640); err != nil {
		t.Fatalf("write result: %v", err)
	}
	tid := taskID
	if err := st.InTx(ctx, func(tx pgx.Tx) error {
		ok := &store.Artifact{
			RunID: runID, TaskID: &tid,
			Path:      runID + "/tasks/" + taskID + "/attempts/1/result.json",
			SizeBytes: 25, Kind: "result",
		}
		gone := &store.Artifact{
			RunID: runID, TaskID: &tid,
			Path:      runID + "/tasks/" + taskID + "/attempts/1/ghost.log",
			SizeBytes: 9, Kind: "log",
		}
		return st.Artifacts.UpsertBatch(ctx, tx, runID, []store.Artifact{*ok, *gone})
	}); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}

	// 一条带 Secret 值的事件（payload 里的 secret_env 必须被打码）。
	if _, err := st.Pool().Exec(ctx, `
		INSERT INTO state_events (run_id, seq, type, to_status, payload, actor, created_at)
		VALUES ($1, 999, 'task.state_changed', 'running',
		        '{"secret_env":{"TOKEN":"s3cr3t-value"},"note":"ok"}'::jsonb, 'system:test', now())`,
		runID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return srv, st, runID, cleanup
}

func strPtr(s string) *string { return &s }

// readBundle 请求 /runs/{id}/bundle 并把 zip 解开成 map[path][]byte。
func readBundle(t *testing.T, srv *Server, runID string) (map[string][]byte, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, 期望 application/zip", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("解析 zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读 %s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out, rec.Header()
}

// TestRunBundle_ZipContents 验收：终态 Run 的包能解出 4 个必需文件、artifacts 条数与
// /artifacts 一致、缺失项进 MANIFEST（report.md 与 ghost.log）。
func TestRunBundle_ZipContents(t *testing.T) {
	srv, _, runID, cleanup := bundleTestEnv(t)
	defer cleanup()

	entries, _ := readBundle(t, srv, runID)
	for _, name := range []string{"MANIFEST.json", "report.json", "metadata.json", "events.jsonl", "artifacts/index.json"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("包内缺 %s（现有条目：%v）", name, keysOf(entries))
		}
	}
	if _, ok := entries["report.md"]; ok {
		t.Fatal("report.md 未落盘，不应出现在包里")
	}

	// artifacts 条目数必须与 /artifacts 一致（index 里的条数 = 索引行数）。
	var index []map[string]any
	if err := json.Unmarshal(entries["artifacts/index.json"], &index); err != nil {
		t.Fatalf("解析 index: %v", err)
	}
	listRec := doGet(t, srv, "/api/v1/runs/"+runID+"/artifacts")
	var list struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &list)
	if len(index) != len(list.Items) {
		t.Fatalf("index 条数 = %d，/artifacts = %d", len(index), len(list.Items))
	}

	var man struct {
		RunID    string `json:"run_id"`
		State    string `json:"state"`
		Terminal bool   `json:"terminal"`
		Files    []struct {
			Path      string `json:"path"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"files"`
		Missing []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"missing"`
	}
	if err := json.Unmarshal(entries["MANIFEST.json"], &man); err != nil {
		t.Fatalf("解析 MANIFEST: %v", err)
	}
	if man.RunID != runID || man.State != "succeeded" || !man.Terminal {
		t.Fatalf("MANIFEST 头 = %+v", man)
	}
	missing := map[string]bool{}
	for _, m := range man.Missing {
		missing[m.Path] = true
		if m.Reason == "" {
			t.Fatalf("missing 项 %s 没有 reason", m.Path)
		}
	}
	if !missing["report.md"] {
		t.Fatalf("MANIFEST.missing 应包含 report.md: %+v", man.Missing)
	}
	if !missing["artifacts/"+runID+"/tasks/"+firstArtifactTask(t, srv, runID)+"/attempts/1/ghost.log"] {
		// ghost.log 的文件不存在，必须进 missing（不静默省略）
		t.Fatalf("MANIFEST.missing 应包含缺失的 ghost.log: %+v", man.Missing)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstArtifactTask(t *testing.T, srv *Server, runID string) string {
	t.Helper()
	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/artifacts")
	var list struct {
		Items []struct {
			TaskID string `json:"task_id"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Items) == 0 {
		t.Fatal("没有产物")
	}
	return list.Items[0].TaskID
}

// TestRunBundle_NonTerminalHasState 非终态：200，MANIFEST.state 是当前状态（让人能看到进度）。
func TestRunBundle_NonTerminalHasState(t *testing.T) {
	srv, st, runID, cleanup := bundleTestEnv(t)
	defer cleanup()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE runs SET status='testing', finished_at=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("置非终态: %v", err)
	}
	entries, _ := readBundle(t, srv, runID)
	var man struct {
		State    string `json:"state"`
		Terminal bool   `json:"terminal"`
	}
	if err := json.Unmarshal(entries["MANIFEST.json"], &man); err != nil {
		t.Fatalf("解析 MANIFEST: %v", err)
	}
	if man.State != "testing" || man.Terminal {
		t.Fatalf("MANIFEST = %+v, 期望 state=testing terminal=false", man)
	}
}

// TestRunBundle_RedactsSecrets 事件里的 Secret 值不得出现在包里。
func TestRunBundle_RedactsSecrets(t *testing.T) {
	srv, _, runID, cleanup := bundleTestEnv(t)
	defer cleanup()
	entries, _ := readBundle(t, srv, runID)
	if bytes.Contains(entries["events.jsonl"], []byte("s3cr3t-value")) {
		t.Fatalf("events.jsonl 泄漏了 Secret 值：%s", entries["events.jsonl"])
	}
	if !bytes.Contains(entries["events.jsonl"], []byte(`"***"`)) {
		t.Fatalf("events.jsonl 应保留被打码的占位符: %s", entries["events.jsonl"])
	}
}

// TestRunBundle_NotFound Run 不存在 → 404。
func TestRunBundle_NotFound(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()
	rec := doGet(t, srv, "/api/v1/runs/r-19700101-ZZZZ/bundle")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, 期望 404", rec.Code)
	}
}
