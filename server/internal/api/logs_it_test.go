package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// seedLogAttempt 造一个 Run + Task（seq=1），并在
// <storage-root>/<run>/tasks/<task>/attempts/<idx>/logs/ 下写日志文件。
func seedLogAttempt(t *testing.T, st *store.Store, root string, files map[string]string) (runID, taskID string) {
	t.Helper()
	runID = store.NewRunID(time.Now())
	taskID = store.NewTaskID()
	if _, _, err := st.CreateRun(context.Background(), store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "log-demo", Kind: "run",
			Manifest: []byte(`{}`), Resolved: []byte(`{}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 1, Name: "t1",
			TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 600, Status: string(domain.TaskPending),
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	dir := filepath.Join(root, runID, "tasks", taskID, "attempts", "1", "logs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return runID, taskID
}

func getLogs(t *testing.T, srv *Server, taskID, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/tasks/"+taskID+"/attempts/1/logs"+query, nil)
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAttemptLogs_ReadsStdout(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	_, taskID := seedLogAttempt(t, st, srv.cfg.StorageRoot, map[string]string{
		"stdout.log": "line-1\nline-2\nline-3\n",
		"probe.log":  "probe-only\n",
	})

	rec := getLogs(t, srv, taskID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.HasPrefix(got, "line-1") {
		t.Fatalf("应优先读 stdout.log，得到 %q", got)
	}
	if rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-Log-Offset") != "0" {
		t.Fatalf("X-Log-Offset = %q, 期望 0", rec.Header().Get("X-Log-Offset"))
	}
	if _, ok := rec.Header()["X-Log-Coverage"]; !ok {
		t.Fatal("缺少 X-Log-Coverage 头（可为空值）")
	}
}

func TestAttemptLogs_OffsetAndLimit(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	body := "0123456789ABCDEFGHIJ"
	_, taskID := seedLogAttempt(t, st, srv.cfg.StorageRoot, map[string]string{"stdout.log": body})

	rec := getLogs(t, srv, taskID, "?offset=5&limit=4")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "5678" {
		t.Fatalf("body = %q, 期望 5678", rec.Body.String())
	}
	if rec.Header().Get("X-Log-Offset") != "5" {
		t.Fatalf("X-Log-Offset = %q, 期望 5（本次起点）", rec.Header().Get("X-Log-Offset"))
	}

	// limit 超过上限被截到 64KiB（这里只断言不报错且长度受控）。
	rec = getLogs(t, srv, taskID, "?limit=999999")
	if rec.Code != http.StatusOK || len(rec.Body.String()) != len(body) {
		t.Fatalf("limit 上限处理异常: status=%d len=%d", rec.Code, len(rec.Body.String()))
	}
}

func TestAttemptLogs_Tail(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	_, taskID := seedLogAttempt(t, st, srv.cfg.StorageRoot, map[string]string{
		"stdout.log": "a\nb\nc\nd\ne\n",
	})
	rec := getLogs(t, srv, taskID, "?tail=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "d\ne\n" {
		t.Fatalf("tail=2 得到 %q, 期望 \"d\\ne\\n\"", got)
	}
	if rec.Header().Get("X-Log-Offset") == "0" {
		t.Fatalf("tail 应返回非 0 起点，得到 %q", rec.Header().Get("X-Log-Offset"))
	}
}

func TestAttemptLogs_FallsBackToSortedLogs(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	_, taskID := seedLogAttempt(t, st, srv.cfg.StorageRoot, map[string]string{
		"b.log": "BBB\n",
		"a.log": "AAA\n",
	})
	rec := getLogs(t, srv, taskID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// 按文件名排序拼接：a.log 在前。
	if got := rec.Body.String(); got != "AAA\nBBB\n" {
		t.Fatalf("拼接顺序 = %q, 期望 a.log 在前", got)
	}
}

func TestAttemptLogs_NotReady404(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	_, taskID := seedLogAttempt(t, st, srv.cfg.StorageRoot, map[string]string{})

	rec := getLogs(t, srv, taskID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, 期望 404（不是 202/500）", rec.Code)
	}
	var body struct {
		Code   string `json:"code"`
		Detail struct {
			Reason string `json:"reason"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误体: %v", err)
	}
	if body.Code != domain.CodeNotFound || body.Detail.Reason != "log_not_ready" {
		t.Fatalf("错误体 = %+v, 期望 NOT_FOUND + log_not_ready", body)
	}
}
