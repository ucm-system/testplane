package api

// 管理面集成测试（真 PG + dev 鉴权；没设 AG_TEST_DATABASE_URL 就 skip）。
//
// 从 v1 harness 的 admin_it_test.go 平移（控制面波次删除），断言改到 v2 路径/错误码/表名：
//   - Profile / Cluster CRUD 的 happy path 与错误路径（v2：POST /admin/profiles 必填
//     namespace；mounts 项必填 name+pvc）；
//   - 审计查询（GET /admin/audit 过滤 + 游标分页）；
//   - 人工清理产物（v2 路径 DELETE /admin/tasks/{task_id}/artifacts，造数用 store.SubmitTask）；
//   - RBAC（非 admin → 403；登录流复用 oidc_it_test.go 的 fakeIdP/browserLogin）。
// Secret 相关用例在 secrets_it_test.go（已随 v2 harness 保留）。

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
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// itSubmitTask 用 store 直插一个最小 Task（runners 1 条，挂指定 Profile），供
// Profile 引用计数 / 产物清理用例造数。
func itSubmitTask(t *testing.T, env *adminTestEnv, name, profile string) *store.Task {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		ID:          store.NewTaskID(time.Now()),
		Name:        name,
		Manifest:    []byte(fmt.Sprintf(`{"task":{"name":%q}}`, name)),
		Resolved:    []byte(`{"tasks":[]}`),
		SubmittedBy: "it",
	}
	rr := &store.RunnerRun{
		Seq:            1,
		Name:           "acc",
		RunnerName:     "aisbench-acc",
		RunnerVersion:  "1.0.0",
		Role:           string(domain.RoleTest),
		ImageRef:       "registry/argus/aisbench-acc:1.0.0",
		Profile:        profile,
		InputRef:       "attempts/1/input/runners/1",
		OnFailure:      domain.OnFailureContinue,
		TimeoutSeconds: 3600,
	}
	created, _, err := env.st.SubmitTask(ctx, store.SubmitParams{
		Task: task, Runners: []*store.RunnerRun{rr},
		Actor: "user:it", Role: "admin", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("造 Task: %v", err)
	}
	return created
}

// ============================================================================
// Execution Profile
// ============================================================================

func TestAdminProfiles_CRUD(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-prof")
	actor := "user:" + subject
	name := uniqueAdminName("it-prof")

	// 1) 创建一个挂在种子集群上的 Profile（v2：namespace 必填）。
	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus",
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
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`, name), subject)
	body := requireError(t, rec, http.StatusConflict, domain.CodeConflict)
	if detailOf(t, body)["name"] != name {
		t.Fatalf("409 detail 应带 name: %v", body["detail"])
	}

	// 3) cluster 未登记 → 400 VALIDATION_ERROR + detail.field=cluster。
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"no-such-cluster","namespace":"argus"}`, uniqueAdminName("it-bad")), subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if detailOf(t, body)["field"] != "cluster" {
		t.Fatalf("400 detail.field 应为 cluster: %v", body["detail"])
	}

	// 4) mounts 缺 pvc → 400。
	rec = env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus","mounts":[{"name":"x"}]}`,
			uniqueAdminName("it-bad-mount")), subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if !strings.Contains(fmt.Sprint(detailOf(t, body)["field"]), "mounts[0]") {
		t.Fatalf("400 detail.field 应指向 mounts[0]: %v", body["detail"])
	}

	// 5) 只读端点能看到它（GET /profiles 是既有契约）。
	rec = env.do(http.MethodGet, "/api/v1/profiles", "", subject)
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), name) {
		t.Fatalf("GET /profiles 里找不到新建的 profile %s", name)
	}

	// 6) PUT：namespace/scheduler 更新，created_by 不变。
	rec = env.do(http.MethodPut, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"namespace":"argus-2","scheduler":"volcano","queue":"default"}`, name), subject)
	updated := requireStatus(t, rec, http.StatusOK)
	if updated["namespace"] != "argus-2" || updated["scheduler"] != "volcano" {
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

	// 9) 审计：register / update / delete_profile 三条都在（v2 action 名不变）。
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

// TestAdminProfiles_DeleteReferencedAllowed 验证"被引用也允许删，但 detail 报引用数"
// （v2：引用列在 task_runners.profile，按 Task 去重计数）。
func TestAdminProfiles_DeleteReferencedAllowed(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-ref")

	name := uniqueAdminName("it-prof-ref")
	rec := env.do(http.MethodPost, "/api/v1/admin/profiles",
		fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)

	// 造一个引用该 Profile 的 Task（runner 执行记录冻结 profile 名）。
	task := itSubmitTask(t, env, "ref-task", name)

	rec = env.do(http.MethodDelete, "/api/v1/admin/profiles?name="+name, "", subject)
	deleted := requireStatus(t, rec, http.StatusOK)
	if deleted["referencing_tasks"] != float64(1) {
		t.Fatalf("referencing_tasks 应为 1，得到 %v", deleted["referencing_tasks"])
	}
	if got, _ := deleted["name"].(string); got != name {
		t.Fatalf("delete 响应 name 异常: %v", deleted)
	}
	_ = task
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
		fmt.Sprintf(`{"name":%q,"cluster":%q,"namespace":"argus"}`, prof, name), subject)
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
			fmt.Sprintf(`{"name":%q,"cluster":"cluster-01","namespace":"argus"}`,
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
// 人工清理产物（v2：DELETE /admin/tasks/{task_id}/artifacts）
// ============================================================================

func TestAdminCleanupArtifacts(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-clean")
	actor := "user:" + subject
	ctx := context.Background()

	task := itSubmitTask(t, env, "cleanup-task", "k8s-npu")
	taskID := task.ID

	// 1) 非终态 → 409 CONFLICT（不让人工清理正在写入的目录）。
	rec := env.do(http.MethodDelete, "/api/v1/admin/tasks/"+taskID+"/artifacts", "", subject)
	body := requireError(t, rec, http.StatusConflict, domain.CodeConflict)
	if detailOf(t, body)["status"] != string(domain.TaskSubmitted) {
		t.Fatalf("409 detail.status 应为 submitted: %v", body["detail"])
	}

	// 2) 不存在的 Task → 404。
	rec = env.do(http.MethodDelete, "/api/v1/admin/tasks/t-19700101-0000/artifacts", "", subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)

	// 3) 终态 Task：造目录 + 索引行 → 清理。
	if _, err := env.st.Pool().Exec(ctx,
		`UPDATE tasks SET status = 'failed', finished_at = GREATEST(now(), submitted_at) WHERE id = $1`, taskID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	dir := filepath.Join(env.cfg.StorageRoot, taskID)
	if err := os.MkdirAll(filepath.Join(dir, "attempts", "1"), 0o750); err != nil {
		t.Fatalf("建产物目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempts", "1", "result.json"), []byte(`{}`), 0o640); err != nil {
		t.Fatalf("写产物文件: %v", err)
	}
	if err := env.st.Artifacts.UpsertBatch(ctx, nil, taskID, []store.Artifact{
		{TaskID: taskID, Path: taskID + "/attempts/1/result.json", SizeBytes: 10, Kind: "result"},
		{TaskID: taskID, Path: taskID + "/report.json", SizeBytes: 32, Kind: "report"},
	}); err != nil {
		t.Fatalf("写产物索引: %v", err)
	}

	rec = env.do(http.MethodDelete, "/api/v1/admin/tasks/"+taskID+"/artifacts", "", subject)
	cleaned := requireStatus(t, rec, http.StatusOK)
	if cleaned["task_id"] != taskID {
		t.Fatalf("响应 task_id 异常: %v", cleaned)
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
	if rows, err := env.st.Artifacts.ListByTask(ctx, nil, taskID); err != nil || len(rows) != 0 {
		t.Fatalf("artifacts 索引未清空: rows=%d err=%v", len(rows), err)
	}
	if _, err := env.st.Tasks.Get(ctx, nil, taskID); err != nil {
		t.Fatalf("清理产物不应删除 Task 本身: %v", err)
	}

	// 4) 审计：成败都留痕（C-10），这里断言成功那条（result=ok）。
	rows := env.auditRows(store.AuditFilter{
		Actor: actor, Action: "cleanup_artifacts", ObjectType: "task", ObjectID: taskID, Limit: 10,
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
		{http.MethodPost, "/api/v1/admin/profiles", `{"name":"x","cluster":"cluster-01","namespace":"argus"}`, operator},
		{http.MethodPut, "/api/v1/admin/profiles", `{"name":"x"}`, operator},
		{http.MethodDelete, "/api/v1/admin/profiles?name=x", "", operator},
		{http.MethodPost, "/api/v1/admin/clusters", `{"name":"x","api_server":"https://a:6443","kubeconfig_key":"a"}`, viewer},
		{http.MethodGet, "/api/v1/admin/secrets", "", operator},
		{http.MethodPost, "/api/v1/admin/secrets", `{"name":"x","namespace":"n","secret_name":"s","key":"k"}`, operator},
		{http.MethodPost, "/api/v1/admin/secrets/x/rotate", "", operator},
		{http.MethodGet, "/api/v1/admin/audit", "", operator},
		// v2：产物清理按 Task 定位。
		{http.MethodDelete, "/api/v1/admin/tasks/t-19700101-0000/artifacts", "", operator},
		// D-65：共享挂载管理面同样要求 admin。
		{http.MethodGet, "/api/v1/admin/shared-mounts", "", operator},
		{http.MethodPost, "/api/v1/admin/shared-mounts",
			`{"name":"rbac-sm","mount_path":"/rbac-sm","claim_name":"pvc"}`, operator},
		{http.MethodPut, "/api/v1/admin/shared-mounts/rbac-sm", `{"enabled":false}`, operator},
		{http.MethodDelete, "/api/v1/admin/shared-mounts/rbac-sm", "", operator},
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
	// D-65：只读视图 viewer 可读（"这次 Task 会挂什么"不该要 admin）。
	rec = do(http.MethodGet, "/api/v1/shared-mounts", "", viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET /shared-mounts 应 200，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
	admin := token([]string{"tp-admins"}, "root-alice")
	rec = do(http.MethodGet, "/api/v1/admin/audit?limit=1", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /admin/audit 应 200，得到 %d\nbody=%s", rec.Code, rec.Body.String())
	}
}
