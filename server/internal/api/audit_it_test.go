package api

// 审计一致性集成测试（真 PG）：
//   - #6 验收①：审计写入失败 → 业务变更**不生效** + 5xx（同事务）；
//   - #6 验收②：非 admin 调 admin 端点 → audit_log 里有 result=rejected；
//   - cancel（Run/Task）与 submit 的审计确实落库。
//
// 失败注入方式：把测试库的 audit_log 改名，让审计 INSERT 必然失败；用例结束恢复。
// 这是测试库（dbtest 强制 *_test 结尾），不会碰 dev 数据。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// breakAuditTable 让当前测试库的 audit_log 不可写，返回恢复函数（t.Cleanup 注册）。
//
// 恢复后 Reset 连接池：DDL 改名会让 pgx 的 prepared statement 缓存指向旧 relation，
// 不重置的话同一进程里后续用例可能撞上"cached plan"类错误（本文件是全包唯一做 DDL 的地方）。
func breakAuditTable(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `ALTER TABLE audit_log RENAME TO audit_log_it_broken`); err != nil {
		t.Fatalf("制造审计写入失败: %v", err)
	}
	t.Cleanup(func() { restoreTable(t, st, `ALTER TABLE IF EXISTS audit_log_it_broken RENAME TO audit_log`) })
}

// restoreTable 执行一次 DDL 恢复并重置连接池（见 breakAuditTable 的说明）。
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

// TestAuditFailure_RollsBackAdminWrite 是评审点名的验收①（admin 写）。
// 用独立测试库（api_audit）：本用例会临时改名 audit_log，不能影响同包其它用例。
func TestAuditFailure_RollsBackAdminWrite(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_audit")
	ctx := context.Background()
	subject := uniqueAdminName("it-admin-auditfail")
	name := uniqueAdminName("it-auditfail-prof")

	breakAuditTable(t, env.st)

	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`, name), subject)
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
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)
}

// TestAuditFailure_RollsBackCancel 是验收①的取消路径（#6.4 补齐 cancel 审计）。
func TestAuditFailure_RollsBackCancel(t *testing.T) {
	env := newAdminTestEnvFor(t, "api_audit")
	ctx := context.Background()
	subject := uniqueAdminName("it-admin-cancelfail")

	runID := store.NewRunID(time.Now())
	if _, _, err := env.st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "cancel-audit-fail", Kind: "run",
			Manifest: []byte(`{"run":{"name":"cancel-audit-fail"}}`), Resolved: []byte(`{"tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "it",
		},
		Actor: "user:it", Role: "admin", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("造 Run: %v", err)
	}

	breakAuditTable(t, env.st)

	rec := env.do(http.MethodPost, "/api/v1/runs/"+runID+"/cancel", "", subject)
	if rec.Code < 500 {
		t.Fatalf("取消的审计写入失败时必须 5xx，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
	run, err := env.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		t.Fatalf("读 Run: %v", err)
	}
	if run.Status == string(domain.RunCancelling) || run.Status == string(domain.RunCanceled) {
		t.Fatalf("审计失败时取消不应生效，Run 仍是 %s", run.Status)
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
		`{"name":"x","cluster":"cluster-01","namespace":"testplane"}`, operatorToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator 调 admin 端点应 403，得到 %d", rec.Code)
	}

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
		`{"name":"x","cluster":"cluster-01","namespace":"testplane"}`, session)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403，得到 %d", rec.Code)
	}
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
	if _, err := env.st.Pool().Exec(ctx, `ALTER TABLE execution_profiles RENAME TO execution_profiles_it_broken`); err != nil {
		t.Fatalf("制造业务表写失败: %v", err)
	}
	t.Cleanup(func() {
		restoreTable(t, env.st, "ALTER TABLE IF EXISTS execution_profiles_it_broken RENAME TO execution_profiles")
	})

	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`, name), subject)
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
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)
}

// TestAudit_CancelRecorded 验证 cancel（Run/Task）与 submit 的审计确实落库（#6.4 补齐）。
func TestAudit_CancelRecorded(t *testing.T) {
	env := newAdminTestEnv(t)
	ctx := context.Background()
	subject := uniqueAdminName("it-admin-cancel")
	actor := "user:" + subject

	// submit（store.CreateRun 在事务里写 action=submit）。
	runID := store.NewRunID(time.Now())
	taskID := store.NewTaskID()
	if _, _, err := env.st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "audit-cancel", Kind: "run",
			Manifest: []byte(`{"run":{"name":"audit-cancel"}}`), Resolved: []byte(`{"tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "it",
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "t1",
			TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: "k8s-npu", ArgsRaw: "", OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 60, Status: string(domain.TaskPending),
		}},
		Actor: "user:it", Role: "admin", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("造 Run: %v", err)
	}
	if rows := auditRowsWithResult(t, env.st, store.AuditFilter{
		Action: "submit", ObjectType: "run", ObjectID: runID, Limit: 5,
	}, "ok"); len(rows) != 1 {
		t.Fatalf("submit 应写 1 条审计，得到 %d", len(rows))
	}

	// Task 取消（pending → skipped）+ 审计。
	rec := env.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/cancel", "", subject)
	requireStatus(t, rec, http.StatusAccepted)
	taskRows := auditRowsWithResult(t, env.st, store.AuditFilter{
		Actor: actor, Action: "cancel", ObjectType: "task", ObjectID: taskID, Limit: 5,
	}, "ok")
	if len(taskRows) != 1 {
		t.Fatalf("Task 取消应写 1 条审计，得到 %d", len(taskRows))
	}

	// Run 取消（submitted → cancelling）+ 审计。
	rec = env.do(http.MethodPost, "/api/v1/runs/"+runID+"/cancel", "", subject)
	requireStatus(t, rec, http.StatusAccepted)
	runRows := auditRowsWithResult(t, env.st, store.AuditFilter{
		Actor: actor, Action: "cancel", ObjectType: "run", ObjectID: runID, Limit: 5,
	}, "ok")
	if len(runRows) != 1 {
		t.Fatalf("Run 取消应写 1 条审计，得到 %d", len(runRows))
	}
}
