package api

// 审计一致性集成测试（真 PG；没设 AG_TEST_DATABASE_URL 就 skip）。
//
// 从 v1 harness 的 audit_it_test.go / audit_async_it_test.go 平移（控制面波次删除），
// 断言改到 v2 路径/表名/造数方式：
//   - 验收①：admin 写的审计失败 → 业务变更不生效 + 5xx（store.InAuditedTx 同事务）；
//   - v2 差异：cancel 的审计是 best-effort（st.AppendAudit 在 CancelTask 之后单独调，
//     不同事务）——v1 的"审计失败回滚取消"断言不再成立，改为验证 v2 语义；
//   - 验收②：非 admin 调 admin 端点 → audit_log 有 result=rejected（#40-6 异步，先 drain）；
//   - 401 按 (remote IP, 1min) 聚合；队列满丢弃并计数；产物目录删除失败补 rejected 审计。
//
// 失败注入方式：把测试库的表改名，让 INSERT 必然失败；用例结束恢复。
// 这是测试库（dbtest 强制 *_test 结尾），不会碰 dev 数据。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// httptestDo 发一个带 Bearer 的请求（oidc token 场景）。
func httptestDo(srv *Server, method, path, body, bearer string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// httptestDoCookie 发一个带会话 Cookie（无 CSRF 头）的写请求。
func httptestDoCookie(srv *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// breakTable 让当前测试库的指定表不可写，返回恢复函数（t.Cleanup 注册）。
//
// 恢复后 Reset 连接池：DDL 改名会让 pgx 的 prepared statement 缓存指向旧 relation，
// 不重置的话同一进程里后续用例可能撞上"cached plan"类错误（本文件是全包唯一做 DDL 的地方）。
func breakTable(t *testing.T, st *store.Store, table, broken string) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(),
		fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, table, broken)); err != nil {
		t.Fatalf("制造表写失败（%s）: %v", table, err)
	}
	t.Cleanup(func() { restoreTable(t, st, fmt.Sprintf(`ALTER TABLE IF EXISTS %s RENAME TO %s`, broken, table)) })
}

// restoreTable 执行一次 DDL 恢复并重置连接池（见 breakTable 的说明）。
func restoreTable(t *testing.T, st *store.Store, ddl string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := st.Pool().Exec(ctx, ddl); err != nil {
		t.Logf("恢复表结构失败（需人工检查）: %v", err)
	}
	st.Pool().Reset()
}

// auditRowsWithResult 按 detail.result 过滤审计行（成功/拒绝分开断言）。
func auditRowsWithResult(t *testing.T, st *store.Store, f store.AuditFilter, result string) []store.AuditRecord {
	t.Helper()
	rows, _, err := st.Audits.List(context.Background(), nil, f)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	out := make([]store.AuditRecord, 0, len(rows))
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			continue
		}
		if detail["result"] == result {
			out = append(out, row)
		}
	}
	return out
}

// drainAuditHub 等异步拒绝审计落库（测试用）。
func drainAuditHub(t *testing.T, st *store.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := auditHub.drain(ctx, st); err != nil {
		t.Fatalf("排空拒绝审计队列: %v", err)
	}
}

// containsLinePrefix 检查文本里是否有以 prefix 开头的非注释行（/metrics 断言用）。
func containsLinePrefix(text, prefix string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) && !strings.HasPrefix(line, "#") {
			return true
		}
	}
	return false
}

// TestAuditFailure_RollsBackAdminWrite 是评审点名的验收①（admin 写）。
// 用独立测试库（api_audit）：本用例会临时改名 audit_log，不能影响同包其它用例。
func TestAuditFailure_RollsBackAdminWrite(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_audit")
	ctx := context.Background()
	subject := uniqueAdminName("it-admin-auditfail")
	name := uniqueAdminName("it-auditfail-prof")

	breakTable(t, env.st, "audit_log", "audit_log_it_broken")

	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`, name), subject)
	// 审计失败 → 整体回滚 + 5xx（不是 201，也不是 200）。
	if rec.Code < 500 {
		t.Fatalf("审计写入失败时必须 5xx，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	if code, _ := body["code"].(string); code != domain.CodeInternalError {
		t.Fatalf("5xx 的 code 应为 INTERNAL_ERROR，得到 %q", code)
	}
	// 业务变更不生效：Profile 不存在。
	if _, err := env.st.Profiles.Get(ctx, nil, name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("审计失败时 Profile 不应落库（err=%v）", err)
	}

	// 恢复后确认同一个请求能成功，证明失败确实来自审计而不是其它原因。
	restoreTable(t, env.st, "ALTER TABLE audit_log_it_broken RENAME TO audit_log")

	// 防止 t.Cleanup 再次改名（表已恢复，IF EXISTS 兜底即可）。
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)
}

// TestAudit_CancelAuditIsBestEffort 是 v2 的取消审计语义（与 v1 断言的差异，见文件头）：
// v2 的 cancel 审计在 CancelTask 事务**之外** best-effort 写（api/tasks.go handleCancelTask），
// 审计表不可写时取消照常生效（202 + cancelling），失败只进日志——不回滚业务。
// 同事务审计（C-10 的强保证）只覆盖 admin 写路径（见 TestAuditFailure_RollsBackAdminWrite）。
func TestAudit_CancelAuditIsBestEffort(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_audit")
	subject := uniqueAdminName("it-admin-cancelfail")

	task := itSubmitTask(t, env, "cancel-audit-fail", "k8s-npu")

	breakTable(t, env.st, "audit_log", "audit_log_it_broken")

	rec := env.do(http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", "", subject)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("v2 取消审计为 best-effort：审计失败不应影响取消，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
	t2, err := env.st.Tasks.Get(context.Background(), nil, task.ID)
	if err != nil {
		t.Fatalf("读 Task: %v", err)
	}
	if t2.Status != string(domain.TaskCancelling) {
		t.Fatalf("取消应已生效，Task 状态 = %s", t2.Status)
	}
}

// TestAudit_MiddlewareRejectionRecorded 是评审点名的验收②（中间件拒绝也留痕）。
func TestAudit_MiddlewareRejectionRecorded(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, st, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	operatorToken := idp.signToken(map[string]any{
		"iss": idp.base(), "sub": "ops-bob", "aud": idp.client,
		"groups": []string{"tp-ops"},
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
	})
	rec := httptestDo(srv, http.MethodPost, "/api/v1/admin/profiles",
		`{"name":"x","cluster":"cluster-01","namespace":"argus"}`, operatorToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator 调 admin 端点应 403，得到 %d", rec.Code)
	}
	// issue #40-6：接入层拒绝审计是异步的，断言前先排空队列。
	drainAuditHub(t, st)

	rows, _, err := st.Audits.List(context.Background(), nil, store.AuditFilter{
		Actor: "user:ops-bob", Limit: 20,
	})
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	found := 0
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			continue
		}
		if detail["result"] != "rejected" {
			continue
		}
		if !strings.Contains(fmt.Sprint(detail["path"]), "/api/v1/admin/profiles") {
			continue
		}
		if row.ObjectType != "http_request" {
			t.Fatalf("拒绝审计的 object_type 应为 http_request，得到 %q", row.ObjectType)
		}
		found++
	}
	if found == 0 {
		t.Fatalf("非 admin 调 admin 端点没有在 audit_log 留 rejected 记录（rows=%d）", len(rows))
	}

	// CSRF 拒绝也留痕：alice 是 operator（tp-ops），Cookie 写操作缺 X-CSRF-Token → 403。
	session, _ := browserLogin(t, srv, idp)
	rec = httptestDoCookie(srv, http.MethodPost, "/api/v1/admin/profiles",
		`{"name":"x","cluster":"cluster-01","namespace":"argus"}`, session)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403，得到 %d", rec.Code)
	}
	drainAuditHub(t, st)
	csrfRows, _, err := st.Audits.List(context.Background(), nil, store.AuditFilter{
		Actor: "user:alice", Action: "csrf_rejected", Limit: 10,
	})
	if err != nil {
		t.Fatalf("读 CSRF 审计: %v", err)
	}
	if len(csrfRows) == 0 {
		t.Fatalf("CSRF 拒绝没有写审计（action=csrf_rejected）")
	}
}

// TestAudit_DBErrorAlsoRecorded 覆盖评审 #6 的"DB 报错路径走 failErr 也不审计"：
// 业务表写失败（不是审计表）时，也必须留一条 result=rejected 的尝试记录。
func TestAudit_DBErrorAlsoRecorded(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_audit")
	ctx := context.Background()
	subject := uniqueAdminName("it-admin-dbfail")
	actor := "user:" + subject
	name := uniqueAdminName("it-dbfail-prof")

	// 让业务表写失败（审计表本身正常）。
	breakTable(t, env.st, "execution_profiles", "execution_profiles_it_broken")

	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`, name), subject)
	if rec.Code < 500 {
		t.Fatalf("业务表写失败应 5xx，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}

	rows, _, err := env.st.Audits.List(ctx, nil, store.AuditFilter{
		Actor: actor, Limit: 20,
	})
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	found := 0
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			continue
		}
		if detail["result"] == "rejected" && detail["reason"] == "db_error" {
			found++
		}
	}
	if found == 0 {
		t.Fatalf("DB 报错路径没有留 rejected 审计（rows=%d）", len(rows))
	}

	// 恢复后确认同样的请求能成功（证明 5xx 来自业务表而不是别的原因）。
	restoreTable(t, env.st, "ALTER TABLE execution_profiles_it_broken RENAME TO execution_profiles")

	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)
}

// TestAudit_SubmitAndCancelRecorded 验证 submit 与 cancel 的审计确实落库（#6.4 补齐）。
func TestAudit_SubmitAndCancelRecorded(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-cancel")
	actor := "user:" + subject

	// submit（store.SubmitTask 在事务里写 action=submit / object_type=task）。
	task := itSubmitTask(t, env, "audit-cancel", "k8s-npu")
	if rows := auditRowsWithResult(t, env.st, store.AuditFilter{
		Action: "submit", ObjectType: "task", ObjectID: task.ID, Limit: 5,
	}, "ok"); len(rows) != 1 {
		t.Fatalf("submit 应写 1 条审计，得到 %d", len(rows))
	}

	// Task 取消（submitted → cancelling）+ 审计。
	rec := env.do(http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", "", subject)
	requireStatus(t, rec, http.StatusAccepted)
	taskRows := auditRowsWithResult(t, env.st, store.AuditFilter{
		Actor: actor, Action: "cancel", ObjectType: "task", ObjectID: task.ID, Limit: 5,
	}, "ok")
	if len(taskRows) != 1 {
		t.Fatalf("Task 取消应写 1 条审计，得到 %d", len(taskRows))
	}
}

// ============================================================================
// issue #40 的回归用例（v1 audit_async_it_test.go 平移）
// ============================================================================

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
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
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

	task := itSubmitTask(t, env, "cleanup-fail", "k8s-npu")
	taskID := task.ID
	if _, err := env.st.Pool().Exec(ctx,
		`UPDATE tasks SET status = 'failed', finished_at = GREATEST(now(), submitted_at) WHERE id = $1`, taskID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	dir := filepath.Join(env.cfg.StorageRoot, taskID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("建产物目录: %v", err)
	}
	if err := env.st.Artifacts.UpsertBatch(ctx, nil, taskID, []store.Artifact{
		{TaskID: taskID, Path: taskID + "/attempts/1/result.json", SizeBytes: 7, Kind: "result"},
	}); err != nil {
		t.Fatalf("写产物索引: %v", err)
	}

	// 注入 fs 失败（评审要求"只影响测试、不改生产行为"）。
	oldRemove := removeAllForCleanup
	removeAllForCleanup = func(string) error { return errors.New("fake fs failure: permission denied") }
	t.Cleanup(func() { removeAllForCleanup = oldRemove })

	rec := env.do(http.MethodDelete, "/api/v1/admin/tasks/"+taskID+"/artifacts", "", subject)
	body := requireError(t, rec, http.StatusInternalServerError, domain.CodeCleanupFailed)
	if detailOf(t, body)["directory"] != dir {
		t.Fatalf("5xx detail 应带 directory: %v", body["detail"])
	}

	rows := env.auditRows(store.AuditFilter{
		Action: "cleanup_artifacts", ObjectType: "task", ObjectID: taskID, Limit: 10,
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
