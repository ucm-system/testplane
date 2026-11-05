package api

// 管理面集成测试（真 PG；没设 TESTPLANE_TEST_DATABASE_URL 就 skip）。
//
// 覆盖：Profile / Cluster / Audit / 人工清理产物的 happy path 与错误路径，
// 以及 RBAC（非 admin → 403）。Secret 相关用例在 secrets_it_test.go。

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// adminTestEnv 是管理面测试的最小环境：真 PG + dev 鉴权（Bearer <subject> = admin）。
type adminTestEnv struct {
	t   *testing.T
	srv *Server
	st  *store.Store
	cfg *config.Config
}

var adminNameSeq atomic.Int64

// uniqueAdminName 生成每次测试唯一的对象名（同一测试库跨用例复用，避免撞主键）。
func uniqueAdminName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e9, adminNameSeq.Add(1))
}

func newAdminTestEnv(t *testing.T) *adminTestEnv {
	t.Helper()
	return newAdminTestEnvFor(t, "api")
}

// newAdminTestEnvFor 建在独立测试库上（purpose 派生库名）。
// 需要做 DDL 破坏注入的用例用独立库（如 "api_audit"），避免影响同包其它用例。
func newAdminTestEnvFor(t *testing.T, purpose string) *adminTestEnv {
	t.Helper()
	dsn, err := dbtest.URL(purpose)
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
	if err := st.SeedDefaults(ctx); err != nil {
		st.Close()
		t.Fatalf("seed: %v", err)
	}
	cfg := &config.Config{
		APIPrefix: "/api/v1", StorageRoot: t.TempDir(),
		RunMaxTasks: 200, TaskTimeoutDefault: time.Hour, TaskTimeoutMax: 24 * time.Hour,
		MaxGlobalConcurrency: 4, DeployTimeout: 72 * time.Hour,
	}
	srv := New(Deps{Config: cfg, Store: st, Machines: store.NewMachines(st, nil),
		Resolver: &fakeResolver{specs: map[string]domain.TesterSpec{}}})
	t.Cleanup(func() { st.Close() })
	return &adminTestEnv{t: t, srv: srv, st: st, cfg: cfg}
}

// do 发一个请求：Bearer <subject> 在 dev 鉴权下即身份（角色 admin）。
func (e *adminTestEnv) do(method, path, body, subject string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if subject == "" {
		subject = "dev"
	}
	req.Header.Set("Authorization", "Bearer "+subject)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON 对象（status=%d）: %v\nbody=%s", rec.Code, err, rec.Body.String())
	}
	return out
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d，期望 %d\nbody=%s", rec.Code, want, rec.Body.String())
	}
	return decodeJSONBody(t, rec)
}

func requireError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	body := requireStatus(t, rec, wantStatus)
	if got, _ := body["code"].(string); got != wantCode {
		t.Fatalf("code = %q，期望 %q\nbody=%s", got, wantCode, rec.Body.String())
	}
	return body
}

func detailOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	d, ok := body["detail"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 detail 对象: %v", body)
	}
	return d
}

// auditRows 读审计行（直接走 store，避免依赖被其它用例污染的列表分页）。
func (e *adminTestEnv) auditRows(filter store.AuditFilter) []store.AuditRecord {
	e.t.Helper()
	rows, _, err := e.st.Audits.List(context.Background(), nil, filter)
	if err != nil {
		e.t.Fatalf("读审计: %v", err)
	}
	return rows
}

// ============================================================================
// Execution Profile
// ============================================================================

func TestAdminProfiles_CRUD(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-prof")
	actor := "user:" + subject
	name := uniqueAdminName("it-prof")

	// 1) 创建一个挂在种子集群上的 Profile。
	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane",
			"mounts":[{"name":"data","pvc":"pvc-data","sub_path":"/data","kind":"dataset"}]}`, name), subject)
	created := requireStatus(t, rec, http.StatusCreated)
	if created["name"] != name || created["cluster"] != "cluster-01" {
		t.Fatalf("创建响应异常: %v", created)
	}
	if created["scheduler"] != "default" {
		t.Fatalf("scheduler 默认值应为 default，得到 %v", created["scheduler"])
	}
	if created["created_by"] != actor {
		t.Fatalf("created_by = %v，期望 %s", created["created_by"], actor)
	}
	if _, ok := created["updated_at"].(string); !ok {
		t.Fatalf("响应缺少 updated_at: %v", created)
	}
	mounts, _ := created["mounts"].([]any)
	if len(mounts) != 1 {
		t.Fatalf("mounts 未回显: %v", created["mounts"])
	}

	// 2) 名字冲突 → 409 CONFLICT。
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`, name), subject)
	body := requireError(t, rec, http.StatusConflict, domain.CodeConflict)
	if detailOf(t, body)["name"] != name {
		t.Fatalf("409 detail 应带 name: %v", body["detail"])
	}

	// 3) cluster 未登记 → 400 VALIDATION_ERROR + detail.field=cluster。
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"no-such-cluster","namespace":"testplane"}`, uniqueAdminName("it-bad")), subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if detailOf(t, body)["field"] != "cluster" {
		t.Fatalf("400 detail.field 应为 cluster: %v", body["detail"])
	}

	// 4) mounts 缺 pvc → 400。
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane","mounts":[{"name":"x"}]}`,
			uniqueAdminName("it-bad-mount")), subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if !strings.Contains(fmt.Sprint(detailOf(t, body)["field"]), "mounts[0]") {
		t.Fatalf("400 detail.field 应指向 mounts[0]: %v", body["detail"])
	}

	// 5) 只读端点能看到它（GET /profiles 是既有契约）。
	rec = env.do(http.MethodGet, "/api/v1/profiles", "", subject)
	list := requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), name) {
		t.Fatalf("GET /profiles 里找不到新建的 profile %s", name)
	}
	_ = list

	// 6) PUT：namespace/scheduler 更新，created_by 不变。
	rec = env.do(http.MethodPut, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane-2","scheduler":"volcano","queue":"default"}`, name), subject)
	updated := requireStatus(t, rec, http.StatusOK)
	if updated["namespace"] != "testplane-2" || updated["scheduler"] != "volcano" {
		t.Fatalf("更新未生效: %v", updated)
	}
	if updated["queue"] != "default" {
		t.Fatalf("queue 未回显: %v", updated["queue"])
	}
	if updated["created_by"] != actor {
		t.Fatalf("created_by 不应变化: %v", updated["created_by"])
	}

	// 7) PUT 不存在的名字 → 404 NOT_FOUND。
	rec = env.do(http.MethodPut, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"namespace":"x"}`, uniqueAdminName("it-missing-prof")), subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)

	// 8) DELETE ?name= → 200；重复删除 → 404；缺参数 → 400。
	rec = env.do(http.MethodDelete, "/api/v1/admin/profiles?name="+name, "", subject)
	deleted := requireStatus(t, rec, http.StatusOK)
	if deleted["deleted"] != true {
		t.Fatalf("delete 响应异常: %v", deleted)
	}
	if deleted["referencing_tasks"] != float64(0) {
		t.Fatalf("referencing_tasks 应为 0，得到 %v", deleted["referencing_tasks"])
	}
	rec = env.do(http.MethodDelete, "/api/v1/admin/profiles?name="+name, "", subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)
	rec = env.do(http.MethodDelete, "/api/v1/admin/profiles", "", subject)
	requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)

	// 9) 审计：register / update / delete 三条都在。
	rows := env.auditRows(store.AuditFilter{Actor: actor, ObjectType: "execution_profile", Limit: 50})
	actions := map[string]bool{}
	for _, row := range rows {
		if row.ObjectID != nil && *row.ObjectID == name {
			actions[row.Action] = true
		}
	}
	for _, want := range []string{"register", "update", "delete_profile"} {
		if !actions[want] {
			t.Fatalf("缺少 action=%s 的审计行；已有 %v", want, actions)
		}
	}
}

// TestAdminProfiles_DeleteReferencedAllowed 验证"被引用也允许删，但 detail 报引用数"。
func TestAdminProfiles_DeleteReferencedAllowed(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-ref")
	ctx := context.Background()

	name := uniqueAdminName("it-prof-ref")
	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)

	// 造一个引用该 Profile 的 Run + Task（历史 Task 冻结 profile 名）。
	runID := store.NewRunID(time.Now())
	_, _, err := env.st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "ref-run", Kind: "run",
			Manifest: []byte(`{"run":{"name":"ref-run"}}`), Resolved: []byte(`{"tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "it",
		},
		Tasks: []*store.Task{{
			ID: store.NewTaskID(), Seq: 0, Name: "t1",
			TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: name, ArgsRaw: "", OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 60, Status: string(domain.TaskPending),
		}},
		Actor: "user:it", Role: "admin", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("造引用 Run: %v", err)
	}

	rec = env.do(http.MethodDelete, "/api/v1/admin/profiles?name="+name, "", subject)
	deleted := requireStatus(t, rec, http.StatusOK)
	if deleted["referencing_tasks"] != float64(1) {
		t.Fatalf("referencing_tasks 应为 1，得到 %v", deleted["referencing_tasks"])
	}
}

// ============================================================================
// Cluster
// ============================================================================

func TestAdminClusters_CRUD(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-cluster")
	actor := "user:" + subject
	name := uniqueAdminName("it-cluster")

	// 1) 登记成功（kubeconfig_key 只是文件名）。
	rec := env.do(http.MethodPost, "/api/v1/admin/clusters",
		fmt.Sprintf(`{"name":%q,"api_server":"https://10.0.0.9:6443","kubeconfig_key":%q,"description":"it"}`,
			name, name+".kubeconfig"), subject)
	created := requireStatus(t, rec, http.StatusCreated)
	if created["name"] != name || created["api_server"] != "https://10.0.0.9:6443" {
		t.Fatalf("创建响应异常: %v", created)
	}
	if created["kubeconfig_key"] != name+".kubeconfig" {
		t.Fatalf("kubeconfig_key 未回显: %v", created["kubeconfig_key"])
	}

	// 2) 重名 → 409。
	rec = env.do(http.MethodPost, "/api/v1/admin/clusters",
		fmt.Sprintf(`{"name":%q,"api_server":"https://10.0.0.9:6443","kubeconfig_key":"a.kubeconfig"}`, name), subject)
	requireError(t, rec, http.StatusConflict, domain.CodeConflict)

	// 3) 传 kubeconfig 内容 → 400（平台不接收凭据内容）。
	rec = env.do(http.MethodPost, "/api/v1/admin/clusters",
		fmt.Sprintf(`{"name":%q,"api_server":"https://10.0.0.9:6443","kubeconfig_key":"a.kubeconfig","kubeconfig":"apiVersion: v1\nclusters: []"}`,
			uniqueAdminName("it-cluster-bad")), subject)
	body := requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if !strings.Contains(fmt.Sprint(body["message"]), "kubeconfig") {
		t.Fatalf("400 message 应说明不接收 kubeconfig 内容: %v", body["message"])
	}

	// 4) kubeconfig_key 里塞内容形态 / 路径穿越 → 400。
	for _, badKey := range []string{"apiVersion: v1", "../escape.kubeconfig", "dir/sub.kubeconfig", "a\nb"} {
		payload, err := json.Marshal(map[string]any{
			"name": uniqueAdminName("it-cluster-bad"), "api_server": "https://10.0.0.9:6443",
			"kubeconfig_key": badKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec = env.do(http.MethodPost, "/api/v1/admin/clusters", string(payload), subject)
		body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
		if detailOf(t, body)["field"] != "kubeconfig_key" {
			t.Fatalf("非法 kubeconfig_key=%q 的 detail.field 应为 kubeconfig_key: %v", badKey, body["detail"])
		}
	}

	// 5) api_server 不是 URL → 400。
	rec = env.do(http.MethodPost, "/api/v1/admin/clusters",
		fmt.Sprintf(`{"name":%q,"api_server":"not-a-url","kubeconfig_key":"a.kubeconfig"}`,
			uniqueAdminName("it-cluster-bad")), subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if detailOf(t, body)["field"] != "api_server" {
		t.Fatalf("detail.field 应为 api_server: %v", body["detail"])
	}

	// 6) 只读端点不暴露 kubeconfig_key（07 §5.3「不返回凭据」）。
	rec = env.do(http.MethodGet, "/api/v1/clusters", "", subject)
	requireStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "kubeconfig_key") {
		t.Fatalf("GET /clusters 不得暴露 kubeconfig_key: %s", rec.Body.String())
	}

	// 7) PUT 更新（kubeconfig_key 变更必须留审计）。
	rec = env.do(http.MethodPut, "/api/v1/admin/clusters",
		fmt.Sprintf(`{"name":%q,"kubeconfig_key":%q,"description":"rotated"}`, name, name+".v2.kubeconfig"), subject)
	updated := requireStatus(t, rec, http.StatusOK)
	if updated["kubeconfig_key"] != name+".v2.kubeconfig" || updated["description"] != "rotated" {
		t.Fatalf("更新未生效: %v", updated)
	}
	rows := env.auditRows(store.AuditFilter{
		Actor: actor, Action: "update", ObjectType: "cluster", ObjectID: name, Limit: 10,
	})
	if len(rows) == 0 {
		t.Fatal("kubeconfig_key 变更没有写审计")
	}
	var detail map[string]any
	if err := json.Unmarshal(rows[0].Detail, &detail); err != nil {
		t.Fatalf("审计 detail 不是 JSON: %v", err)
	}
	if detail["kubeconfig_key_changed"] != true {
		t.Fatalf("审计应标记 kubeconfig_key_changed=true: %v", detail)
	}

	// 8) PUT 不存在的集群 → 404。
	rec = env.do(http.MethodPut, "/api/v1/admin/clusters",
		fmt.Sprintf(`{"name":%q,"description":"x"}`, uniqueAdminName("it-cluster-missing")), subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)

	// 9) 仍有 Profile 引用 → 409 + detail.profiles[]。
	prof := uniqueAdminName("it-prof-on-cluster")
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":%q,"namespace":"testplane"}`, prof, name), subject)
	requireStatus(t, rec, http.StatusCreated)

	rec = env.do(http.MethodDelete, "/api/v1/admin/clusters?name="+name, "", subject)
	body = requireError(t, rec, http.StatusConflict, domain.CodeConflict)
	profiles, _ := detailOf(t, body)["profiles"].([]any)
	if len(profiles) != 1 || profiles[0] != prof {
		t.Fatalf("409 detail.profiles 应列出引用者: %v", body["detail"])
	}

	// 10) 删掉 Profile 后可以删集群；再删 → 404。
	rec = env.do(http.MethodDelete, "/api/v1/admin/profiles?name="+prof, "", subject)
	requireStatus(t, rec, http.StatusOK)
	rec = env.do(http.MethodDelete, "/api/v1/admin/clusters?name="+name, "", subject)
	deleted := requireStatus(t, rec, http.StatusOK)
	if deleted["deleted"] != true {
		t.Fatalf("删除响应异常: %v", deleted)
	}
	rec = env.do(http.MethodDelete, "/api/v1/admin/clusters?name="+name, "", subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)
	rec = env.do(http.MethodDelete, "/api/v1/admin/clusters", "", subject)
	requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
}

// ============================================================================
// 审计查询
// ============================================================================

func TestAdminAudit_FiltersAndPaging(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-auditor")
	actor := "user:" + subject

	// 造成两条可区分的写：两条 Profile 注册。
	for i := 0; i < 2; i++ {
		rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
			fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"testplane"}`,
				uniqueAdminName(fmt.Sprintf("it-audit-prof-%d", i))), subject)
		requireStatus(t, rec, http.StatusCreated)
	}

	// 1) 过滤 + limit=1：第一页 1 条且有 next_cursor。
	rec := env.do(http.MethodGet,
		"/api/v1/admin/audit?actor="+actor+"&object_type=execution_profile&limit=1", "", subject)
	page1 := requireStatus(t, rec, http.StatusOK)
	items, _ := page1["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("limit=1 应返回 1 条，得到 %d", len(items))
	}
	if page1["has_more"] != true {
		t.Fatalf("还有下一页但 has_more=false: %v", page1)
	}
	cursor, _ := page1["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("缺少 next_cursor")
	}
	first, _ := items[0].(map[string]any)
	if first["actor"] != actor || first["object_type"] != "execution_profile" {
		t.Fatalf("审计行投影异常: %v", first)
	}

	// 2) 游标翻页：第二页与第一页不重复。
	rec = env.do(http.MethodGet,
		"/api/v1/admin/audit?actor="+actor+"&object_type=execution_profile&limit=1&cursor="+cursor, "", subject)
	page2 := requireStatus(t, rec, http.StatusOK)
	items2, _ := page2["items"].([]any)
	if len(items2) != 1 {
		t.Fatalf("第二页应返回 1 条，得到 %d", len(items2))
	}
	second, _ := items2[0].(map[string]any)
	if first["id"] == second["id"] {
		t.Fatalf("翻页返回了重复行: %v", first)
	}
	// ORDER BY at DESC：第一页的时间不早于第二页。
	t1, err1 := time.Parse(time.RFC3339, fmt.Sprint(first["at"]))
	t2, err2 := time.Parse(time.RFC3339, fmt.Sprint(second["at"]))
	if err1 != nil || err2 != nil || t1.Before(t2) {
		t.Fatalf("审计未按 at DESC 排序: %v vs %v (%v/%v)", first["at"], second["at"], err1, err2)
	}

	// 3) action 过滤：不存在的 action → 空页。
	rec = env.do(http.MethodGet, "/api/v1/admin/audit?actor="+actor+"&action=no_such_action", "", subject)
	empty := requireStatus(t, rec, http.StatusOK)
	if items, _ := empty["items"].([]any); len(items) != 0 {
		t.Fatalf("action 过滤失效: %v", empty)
	}

	// 4) 时间范围：未来时间 → 空页；非法时间 → 400。
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = env.do(http.MethodGet, "/api/v1/admin/audit?actor="+actor+"&from="+future, "", subject)
	futurePage := requireStatus(t, rec, http.StatusOK)
	if items, _ := futurePage["items"].([]any); len(items) != 0 {
		t.Fatalf("from 过滤失效: %v", futurePage)
	}
	rec = env.do(http.MethodGet, "/api/v1/admin/audit?from=not-a-time", "", subject)
	requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
}

// ============================================================================
// 人工清理产物
// ============================================================================

func TestAdminCleanupArtifacts(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-clean")
	actor := "user:" + subject
	ctx := context.Background()

	runID := store.NewRunID(time.Now())
	_, _, err := env.st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "cleanup-run", Kind: "run",
			Manifest: []byte(`{"run":{"name":"cleanup-run"}}`), Resolved: []byte(`{"tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "it",
		},
		Actor: "user:it", Role: "admin", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("造 Run: %v", err)
	}

	// 1) 非终态 → 409 CONFLICT。
	rec := env.do(http.MethodDelete, "/api/v1/admin/runs/"+runID+"/artifacts", "", subject)
	body := requireError(t, rec, http.StatusConflict, domain.CodeConflict)
	if detailOf(t, body)["status"] != string(domain.RunSubmitted) {
		t.Fatalf("409 detail.status 应为 submitted: %v", body["detail"])
	}

	// 2) 不存在的 Run → 404。
	rec = env.do(http.MethodDelete, "/api/v1/admin/runs/r-19700101-0000/artifacts", "", subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)

	// 3) 终态 Run：造目录 + 索引行 → 清理。
	if _, err := env.st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'succeeded', finished_at = now() WHERE id = $1`, runID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	dir := filepath.Join(env.cfg.StorageRoot, runID)
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatalf("建产物目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks", "a.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("写产物文件: %v", err)
	}
	if err := env.st.Artifacts.UpsertBatch(ctx, nil, runID, []store.Artifact{
		{RunID: runID, Path: runID + "/tasks/a.json", SizeBytes: 10, Kind: "result"},
		{RunID: runID, Path: runID + "/report.json", SizeBytes: 32, Kind: "report"},
	}); err != nil {
		t.Fatalf("写产物索引: %v", err)
	}

	rec = env.do(http.MethodDelete, "/api/v1/admin/runs/"+runID+"/artifacts", "", subject)
	cleaned := requireStatus(t, rec, http.StatusOK)
	if cleaned["run_id"] != runID {
		t.Fatalf("响应 run_id 异常: %v", cleaned)
	}
	if cleaned["deleted_entries"] != float64(2) || cleaned["freed_bytes"] != float64(42) {
		t.Fatalf("删除计数异常: %v", cleaned)
	}
	if cleaned["directory_removed"] != true {
		t.Fatalf("目录未被删除: %v", cleaned)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("产物目录仍存在: %v", err)
	}
	if rows, err := env.st.Artifacts.ListByRun(ctx, nil, runID); err != nil || len(rows) != 0 {
		t.Fatalf("artifacts 索引未清空: rows=%d err=%v", len(rows), err)
	}
	if _, err := env.st.Runs.Get(ctx, nil, runID); err != nil {
		t.Fatalf("清理产物不应删除 Run 本身: %v", err)
	}

	// 4) 审计：成败都留痕（C-10），这里断言成功那条（result=ok）。
	rows := env.auditRows(store.AuditFilter{
		Actor: actor, Action: "cleanup_artifacts", ObjectType: "run", ObjectID: runID, Limit: 10,
	})
	okRows := 0
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err == nil && detail["result"] == "ok" {
			okRows++
		}
	}
	if okRows != 1 {
		t.Fatalf("清理产物应写 1 条成功审计，得到 %d（总行数 %d）", okRows, len(rows))
	}
}

// ============================================================================
// RBAC：非 admin 一律 403
// ============================================================================

func TestAdminRBAC_NonAdminForbidden(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	token := func(groups []string, sub string) string {
		return idp.signToken(map[string]any{
			"iss": idp.base(), "sub": sub, "aud": idp.client,
			"groups": groups,
			"exp":    time.Now().Add(5 * time.Minute).Unix(),
		})
	}
	operator := token([]string{"tp-ops"}, "ops-bob")   // → operator
	viewer := token([]string{"unknown"}, "view-carol") // → viewer

	do := func(method, path, body, bearer string) *httptest.ResponseRecorder {
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

	cases := []struct {
		method, path, body string
		bearer             string
	}{
		{http.MethodPost, "/api/v1/admin/profiles", `{"name":"x","cluster":"cluster-01","namespace":"testplane"}`, operator},
		{http.MethodPut, "/api/v1/admin/profiles", `{"name":"x"}`, operator},
		{http.MethodDelete, "/api/v1/admin/profiles?name=x", "", operator},
		{http.MethodPost, "/api/v1/admin/clusters", `{"name":"x","api_server":"https://a:6443","kubeconfig_key":"a"}`, viewer},
		{http.MethodGet, "/api/v1/admin/secrets", "", operator},
		{http.MethodPost, "/api/v1/admin/secrets", `{"name":"x","namespace":"n","secret_name":"s","key":"k"}`, operator},
		{http.MethodPost, "/api/v1/admin/secrets/x/rotate", "", operator},
		{http.MethodGet, "/api/v1/admin/audit", "", operator},
		{http.MethodDelete, "/api/v1/admin/runs/r-x/artifacts", "", operator},
	}
	for _, c := range cases {
		rec := do(c.method, c.path, c.body, c.bearer)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 非 admin 应 403，得到 %d\nbody=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}

	// 反向：operator 可以读 /secrets（07 §2.3 / C-8.2），viewer 不行；admin 可以读 /admin/audit。
	rec := do(http.MethodGet, "/api/v1/secrets", "", operator)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator GET /secrets 应 200，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodGet, "/api/v1/secrets", "", viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer GET /secrets 应 403（07 §2.3），得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
	admin := token([]string{"tp-admins"}, "root-alice")
	rec = do(http.MethodGet, "/api/v1/admin/audit?limit=1", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /admin/audit 应 200，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
}
