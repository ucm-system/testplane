package api

// issue #40 的回归用例：
//   - #40-6：401 按 (remote IP, 1min) 聚合 + 异步有界队列（刷 100 次不等比增长；队列满丢弃并计数）；
//   - #40-7：产物目录删除失败时必须补一条 result=rejected 结果审计（含 directory/error）。
//
// 说明：接入层拒绝审计是异步的，断言前用 drainAuditHub 排空；handler 内的业务拒绝仍同步。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// drainAuditHub 等异步拒绝审计落库（测试用）。
func drainAuditHub(t *testing.T, st *store.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := auditHub.drain(ctx, st); err != nil {
		t.Fatalf("排空拒绝审计队列: %v", err)
	}
}

// TestAudit401_AggregatedByIPWindow 是 #40-6 的验收：刷 100 次 401，
// audit_log 行数**不按请求数线性增长**（同 IP + 同窗口合并成一条，suppressed_count 记被压掉的条数）。
func TestAudit401_AggregatedByIPWindow(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, st, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	ctx := context.Background()
	// 每次测试用独立来源 IP，避免历史运行留下的聚合成干扰断言。
	ip := fmt.Sprintf("198.51.100.%d", 1+int(time.Now().UnixNano()%250))
	if _, err := st.Pool().Exec(ctx, `DELETE FROM audit_log WHERE object_id = $1`, ip); err != nil {
		t.Fatalf("清理历史审计: %v", err)
	}

	// 用时间窗圈定本次请求产生的审计行（测试库跨轮累积，不能只靠 object_id）。
	windowStart := time.Now().UTC().Add(-2 * time.Second)
	const attempts = 100
	for i := 0; i < attempts; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.RemoteAddr = ip + ":12345"
		req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次坏 token 应 401，得到 %d", i, rec.Code)
		}
	}
	drainAuditHub(t, st)
	windowEnd := time.Now().UTC().Add(2 * time.Second)

	allRows, _, err := st.Audits.List(ctx, nil, store.AuditFilter{
		Actor: "anonymous", Action: actionAuthRejected, Limit: 500,
	})
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	rows := make([]store.AuditRecord, 0, 4)
	for _, row := range allRows {
		if row.At.Before(windowStart) || row.At.After(windowEnd) {
			continue
		}
		rows = append(rows, row)
	}
	// 关键断言：100 次请求不能写 100 行（同 IP、同 1min 窗口 → 1 行）。
	if len(rows) > 2 {
		t.Fatalf("%d 次 401 写了 %d 行审计（未聚合；期望 ≤2）", attempts, len(rows))
	}
	if len(rows) == 0 {
		t.Fatal("401 聚合审计没有落库")
	}
	var detail map[string]any
	if err := json.Unmarshal(rows[len(rows)-1].Detail, &detail); err != nil {
		t.Fatalf("审计 detail 不是 JSON: %v", err)
	}
	if detail["result"] != "rejected" {
		t.Fatalf("detail.result 应为 rejected: %v", detail)
	}
	if detail["code"] != domain.CodeUnauthenticated {
		t.Fatalf("401 的 code 应为 %s: %v", domain.CodeUnauthenticated, detail)
	}
	if detail["source_ip"] != ip {
		t.Fatalf("detail.source_ip 应为 %s: %v", ip, detail)
	}
	if detail["suppressed_count"] != float64(attempts-1) {
		t.Fatalf("suppressed_count 应为 %d（被压掉的条数）: %v", attempts-1, detail)
	}
	if rows[0].ObjectType != objectHTTPRequest {
		t.Fatalf("object_type 应为 http_request: %q", rows[0].ObjectType)
	}
	if rows[0].ObjectID == nil || *rows[0].ObjectID != ip {
		t.Fatalf("聚合行的 object_id 应为来源 IP %s: %v", ip, rows[0].ObjectID)
	}

	// /metrics 必须暴露管道计数器（#40-6 要求可观测）。
	// 名字引用 metrics.go 里的常量（不手写字符串），改名后本用例会跟着红。
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		metricAuditRejectedWritten,
		metricAuditRejectedDropped,
		metricAuditRejectedSuppressed,
	} {
		if !containsLinePrefix(body, want) {
			t.Fatalf("/metrics 缺少 %s:\n%s", want, body)
		}
	}
}

// TestAuditRejectedQueue_DropCounted 是 #40-6 的另一半：队列满时必须丢弃并计数（不能无限阻塞/无限增长）。
func TestAuditRejectedQueue_DropCounted(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_issue40")

	// 暂停消费者，把队列压满。
	gate := make(chan struct{})
	var closeGate sync.Once
	t.Cleanup(func() {
		closeGate.Do(func() { close(gate) })
		auditHub.gate.Store(nil)
	})
	auditHub.gate.Store(&gate)

	beforeWritten, beforeDropped, _, _ := auditHub.stats()
	const attempts = auditRejectQueueCap + 64
	for i := 0; i < attempts; i++ {
		env.srv.auditRejectedAsync("user:flood", "viewer", actionRBACRejected, objectHTTPRequest, "/api/v1/admin/profiles",
			map[string]any{"result": "rejected", "reason": "rbac", "n": i})
	}
	_, afterDropped, _, _ := auditHub.stats()
	droppedDelta := afterDropped - beforeDropped
	if droppedDelta < 32 {
		t.Fatalf("队列满时应丢弃并计数：dropped delta=%d（期望 ≥32）", droppedDelta)
	}

	// 恢消费并排空：接受数 + 丢弃数 == 投递数（允许 1 条正在处理）。
	closeGate.Do(func() { close(gate) })
	auditHub.gate.Store(nil)
	drainAuditHub(t, env.st)
	afterWritten, finalDropped, _, _ := auditHub.stats()
	accepted := afterWritten - beforeWritten
	if diff := int64(attempts) - (accepted + (finalDropped - beforeDropped)); diff > 2 || diff < -2 {
		t.Fatalf("投递 %d != 接受 %d + 丢弃 %d（差值 %d，允许 ±2 在飞）",
			attempts, accepted, finalDropped-beforeDropped, diff)
	}
}

// TestAuditCleanup_DirectoryRemoveFailureRecordsRejected 是 #40-7 的验收：
// 目录删除失败时 audit_log 里必须有一条 result=rejected（带 directory 与 error），
// 而不是只有提前写好的 result=ok。
func TestAuditCleanup_DirectoryRemoveFailureRecordsRejected(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_issue40")
	ctx := context.Background()
	subject := uniqueAdminName("it-admin-cleanup-fail")

	runID := store.NewRunID(time.Now())
	if _, _, err := env.st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "cleanup-fail", Kind: "run",
			Manifest: []byte(`{"run":{"name":"cleanup-fail"}}`), Resolved: []byte(`{"tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "it",
		},
		Actor: "user:it", Role: "admin", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("造 Run: %v", err)
	}
	if _, err := env.st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'succeeded', finished_at = now() WHERE id = $1`, runID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	dir := filepath.Join(env.cfg.StorageRoot, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建产物目录: %v", err)
	}
	if err := env.st.Artifacts.UpsertBatch(ctx, nil, runID, []store.Artifact{
		{RunID: runID, Path: runID + "/tasks/a.json", SizeBytes: 7, Kind: "result"},
	}); err != nil {
		t.Fatalf("写产物索引: %v", err)
	}

	// 注入 fs 失败（评审要求"只影响测试、不改生产行为"）。
	oldRemove := removeAllForCleanup
	removeAllForCleanup = func(string) error { return errors.New("fake fs failure: permission denied") }
	t.Cleanup(func() { removeAllForCleanup = oldRemove })

	rec := env.do(http.MethodDelete, "/api/v1/admin/runs/"+runID+"/artifacts", "", subject)
	body := requireError(t, rec, http.StatusInternalServerError, domain.CodeCleanupFailed)
	if detailOf(t, body)["directory"] != dir {
		t.Fatalf("5xx detail 应带 directory: %v", body["detail"])
	}

	rows := env.auditRows(store.AuditFilter{
		Action: "cleanup_artifacts", ObjectType: "run", ObjectID: runID, Limit: 10,
	})
	var (
		okRow       map[string]any
		rejectedRow map[string]any
	)
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			continue
		}
		switch detail["result"] {
		case "ok":
			okRow = detail
		case "rejected":
			rejectedRow = detail
		}
	}
	if okRow == nil {
		t.Fatalf("应保留一条 result=ok 的清理审计: %v", rows)
	}
	if okRow["directory"] != dir {
		t.Fatalf("ok 审计的 detail.directory 应为 %s: %v", dir, okRow)
	}
	if rejectedRow == nil {
		t.Fatalf("#40-7：目录删除失败没有补 rejected 审计（rows=%d）", len(rows))
	}
	if rejectedRow["directory"] != dir {
		t.Fatalf("rejected 审计的 detail.directory 应为 %s: %v", dir, rejectedRow)
	}
	if errMsg, _ := rejectedRow["error"].(string); errMsg == "" {
		t.Fatalf("rejected 审计应带 detail.error: %v", rejectedRow)
	}
}

// containsLinePrefix 判断 metrics exposition 里是否有以 name 开头的行。
func containsLinePrefix(body, name string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return true
		}
	}
	return false
}
