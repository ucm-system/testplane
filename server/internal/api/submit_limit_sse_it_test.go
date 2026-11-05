package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// submitBody 用真实 HTTP 打一次提交（返回 recorder）。
func submitBody(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestSubmitRun_ZeroTestsRejected 是 D-57 的验收①：
// tests=0 + 默认 precheck(auto) 必须 400，`detail.issues` 里给出 `path=tests` 与
// "至少需要 1 个测试 Task（Pre-Check 由平台注入，不计入测试）"——旧实现只挡 precheck:off，
// 这条在旧实现下会是 202（"成功地什么都没测"）。
func TestSubmitRun_ZeroTestsRejected(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	// 嵌套写法：issues.path 前缀是 run.（与其它字段一致；平级写法下就是 tests）。
	body := `
run:
  name: zero-tests
  service:
    url: http://127.0.0.1:8000/v1
  tests: []
`
	rec := submitBody(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	var e struct {
		Code   string `json:"code"`
		Detail struct {
			Issues []struct {
				Code    string `json:"code"`
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"issues"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("解析错误体: %v", err)
	}
	if e.Code != domain.CodeValidationError {
		t.Fatalf("code = %q, 期望 VALIDATION_ERROR（body=%s）", e.Code, rec.Body.String())
	}
	var hit bool
	for _, is := range e.Detail.Issues {
		if strings.HasSuffix(is.Path, "tests") && is.Code == "REQUIRED" {
			hit = true
			if !strings.Contains(is.Message, "至少需要 1 个测试 Task") ||
				!strings.Contains(is.Message, "Pre-Check") {
				t.Fatalf("message = %q，期望包含下限说明与 Pre-Check 不计入", is.Message)
			}
		}
	}
	if !hit {
		t.Fatalf("issues 里没有 path=tests code=REQUIRED: %+v", e.Detail.Issues)
	}
}

// TestSubmitRun_ZeroTestsPrecheckOffStillRejected 验收②：precheck:off 只是子集，
// 0 条 tests 依旧 400（旧实现也 400，但不能因此放松；这里锁住"与 precheck 无关"）。
func TestSubmitRun_ZeroTestsPrecheckOffStillRejected(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: zero-tests-off
  precheck: "off"
  service:
    url: http://127.0.0.1:8000/v1
  tests: []
`
	rec := submitBody(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	var e struct {
		Detail struct {
			Issues []struct {
				Path string `json:"path"`
			} `json:"issues"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if len(e.Detail.Issues) == 0 || e.Detail.Issues[0].Path != "run.tests" {
		t.Fatalf("嵌套写法 issue path 期望 run.tests: %+v", e.Detail.Issues)
	}
}

// TestSubmitRun_OneTestWithPrecheckAuto 验收③：1 条 tests + precheck:auto → 202，
// 且 tasks 里 Pre-Check(seq=0) + 用户 Task 共 2 条——证明 Pre-Check 不计入下限。
func TestSubmitRun_OneTestWithPrecheckAuto(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: one-test-auto
  precheck: auto
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: only-test
      tester: endpoint-probe
      version: "1.0.0"
      execution:
        profile: k8s-npu
`
	rec := submitBody(t, srv, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, 期望 202（body=%s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		Tasks []struct {
			Name string `json:"name"`
			Seq  int    `json:"seq"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(resp.Tasks) != 2 {
		t.Fatalf("tasks = %d 条，期望 2（Pre-Check + 用户测试）: %+v", len(resp.Tasks), resp.Tasks)
	}
	if resp.Tasks[0].Seq != 0 || resp.Tasks[0].Name != "precheck" {
		t.Fatalf("首条应是注入的 Pre-Check（seq=0）: %+v", resp.Tasks[0])
	}
	if resp.Tasks[1].Name != "only-test" {
		t.Fatalf("第二条应是用户 Task: %+v", resp.Tasks[1])
	}
}

// TestSSE_FirstByteWithinOneSecond 是 P0-4 的"改回去会红"用例：
// 连上 /runs/{id}/events 后 **1 秒内**必须收到响应头 + 首字节 `: connected`。
//
// 关键：把 Last-Event-ID 设成库内**最新** seq（没有可回放事件）——旧实现
// `WriteHeader` 之后不 Flush，响应头要等到 15s 心跳才发出，客户端这段时间显示"未连接"。
// 随后再插一条新事件，断言按 seq 增量推送（不只验证"头到了"）。
func TestSSE_FirstByteWithinOneSecond(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	runID := store.NewRunID(srv.now())
	taskID := store.NewTaskID()
	epKey := "http://sse-ep:8000"
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "sse-first-byte", Kind: "run",
			Manifest:      []byte(`{"run":{"name":"sse-first-byte"}}`),
			Resolved:      []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev", SubmittedAt: srv.now(),
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "t1", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			Status: string(domain.TaskRunning), CurrentAttempt: 1, EndpointKey: &epKey,
			TimeoutSeconds: 60, OnFailure: domain.OnFailureContinue,
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE runs SET status='testing' WHERE id=$1`, runID); err != nil {
		t.Fatalf("置 testing: %v", err)
	}
	// 现有事件里的最大 seq（客户端"已看到全部"的位置）。
	var latest int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT COALESCE(max(seq), 0) FROM state_events WHERE run_id = $1`, runID).Scan(&latest); err != nil {
		t.Fatalf("读最新 seq: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", fmt.Sprint(latest))
	client := &http.Client{Timeout: 20 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("连接 SSE 失败（响应头没有立即发出？）：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("响应头用了 %.2fs（>1s）：WriteHeader 后没有立即 Flush（旧实现要等 15s 心跳）", elapsed.Seconds())
	}

	br := bufio.NewReader(resp.Body)
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := br.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		if got := strings.TrimSpace(line); got != ": connected" {
			t.Fatalf("首行 = %q, 期望 \": connected\"", got)
		}
	case err := <-errCh:
		t.Fatalf("读首行失败: %v", err)
	case <-time.After(time.Second):
		t.Fatal("1 秒内没有收到首字节：响应头/首字节没有立即 Flush（旧实现要等 15s 心跳）")
	}

	// 再插一条新事件：必须按 seq 增量推过来（不依赖 15s 心跳）。
	if _, err := st.Pool().Exec(ctx, `
		INSERT INTO state_events (run_id, seq, type, to_status, payload, actor, created_at)
		VALUES ($1, $2, 'run.state_changed', 'testing', '{}'::jsonb, 'system:test', now())`,
		runID, latest+1); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	sawID := make(chan struct{}, 1)
	go func() {
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(l) == "id: "+fmt.Sprint(latest+1) {
				sawID <- struct{}{}
				return
			}
		}
	}()
	select {
	case <-sawID:
	case <-time.After(6 * time.Second):
		t.Fatalf("6 秒内没有收到 seq=%d 的增量事件（轮询/推送没生效）", latest+1)
	}
}

// TestSubmitRun_NameTooLongRejected 是 D-58 的验收①：
// 201 字符的 run.name → 400，issue 的 path 以 run.name 结尾，message 带实际长度（201）。
func TestSubmitRun_NameTooLongRejected(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	long := strings.Repeat("a", 201)
	body := `
run:
  name: "` + long + `"
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: t1
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: k8s-npu
`
	rec := submitBody(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	var e struct {
		Code   string `json:"code"`
		Detail struct {
			Issues []struct {
				Code    string `json:"code"`
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"issues"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if e.Code != domain.CodeValidationError {
		t.Fatalf("code = %q", e.Code)
	}
	var hit bool
	for _, is := range e.Detail.Issues {
		if strings.HasSuffix(is.Path, "run.name") {
			hit = true
			if is.Code != domain.IssueCodeTooLong {
				t.Fatalf("issue code = %q, 期望 %q", is.Code, domain.IssueCodeTooLong)
			}
			if !strings.Contains(is.Message, "200") || !strings.Contains(is.Message, "201") {
				t.Fatalf("message = %q，期望带上限 200 与实际 201", is.Message)
			}
		}
	}
	if !hit {
		t.Fatalf("没有 run.name 的 issue: %+v", e.Detail.Issues)
	}
}

// TestSubmitRun_NameExactly200CJKAccepted 是 D-58 的验收②：
// **200 个中文字符**必须 202——证明按 rune 计数，不是按字节（UTF-8 下 200 个汉字是 600 字节）。
func TestSubmitRun_NameExactly200CJKAccepted(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	name := strings.Repeat("测", 200)
	if got := len([]rune(name)); got != 200 {
		t.Fatalf("构造错误: %d runes", got)
	}
	if got := len(name); got <= 200 {
		t.Fatalf("构造错误: %d bytes，应当 >200（否则测不出按字节还是按 rune）", got)
	}
	body := `
run:
  name: "` + name + `"
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: t1
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: k8s-npu
`
	rec := submitBody(t, srv, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("200 个中文字符应 202（按 rune 计数），实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSubmitRun_NameBoundary199And201 是 D-58 的验收③：199 通过、200 通过、201 拒绝。
func TestSubmitRun_NameBoundary199And201(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := func(n int) string {
		return `
run:
  name: "` + strings.Repeat("n", n) + `"
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: t1
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: k8s-npu
`
	}
	for _, n := range []int{199, 200} {
		if rec := submitBody(t, srv, body(n)); rec.Code != http.StatusAccepted {
			t.Fatalf("name=%d 字符应 202，实际 %d: %s", n, rec.Code, rec.Body.String())
		}
	}
	if rec := submitBody(t, srv, body(201)); rec.Code != http.StatusBadRequest {
		t.Fatalf("name=201 字符应 400，实际 %d", rec.Code)
	}
}

// TestSubmitRun_TestsNameTooLongRejected 是 D-58 对 tests[].name 的验收（同一上限）。
func TestSubmitRun_TestsNameTooLongRejected(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	body := `
run:
  name: ok-name
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: "` + strings.Repeat("b", 201) + `"
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: k8s-npu
`
	rec := submitBody(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	var e struct {
		Detail struct {
			Issues []struct {
				Path string `json:"path"`
			} `json:"issues"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	hit := false
	for _, is := range e.Detail.Issues {
		if strings.HasSuffix(is.Path, "tests[0].name") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("没有 tests[0].name 的 issue: %+v", e.Detail.Issues)
	}
}
