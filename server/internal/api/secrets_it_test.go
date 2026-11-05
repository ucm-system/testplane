package api

// Secret 引用的集成测试（真 PG）：
//   - GET /secrets（operator）与 GET /admin/secrets 只回引用信息，**任何 Secret 值都搜不到**；
//   - POST 拒绝值字段；
//   - rotate 用注入的假探针覆盖 200 / 404 / 400 / 503 与审计。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// fakeSecretProbe 是 rotate 的假校验器：记录参数，按预设返回。
type fakeSecretProbe struct {
	mu          sync.Mutex
	exists      bool
	keyNonEmpty bool
	err         error
	calls       int
	lastNS      string
	lastSecret  string
	lastKey     string
}

func (p *fakeSecretProbe) CheckSecretKey(ctx context.Context, namespace, secretName, key string) (bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastNS, p.lastSecret, p.lastKey = namespace, secretName, key
	return p.exists, p.keyNonEmpty, p.err
}

// withSecretProbe 注入假探针，测试结束恢复（生产路径永远不设置）。
func withSecretProbe(t *testing.T, p SecretProbe) {
	t.Helper()
	old := secretProbeOverride
	secretProbeOverride = p
	t.Cleanup(func() { secretProbeOverride = old })
}

func TestAdminSecrets_ListCreateNoValue(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-secret")
	actor := "user:" + subject
	name := uniqueAdminName("it-secret-ref")
	const sentinel = "IT-SENTINEL-SECRET-VALUE-DO-NOT-LEAK"

	// 1) 传值 → 400，且明确说明平台不接收 Secret 值。
	payload := fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"tp-secret","key":"token","value":%q}`,
		uniqueAdminName("it-secret-bad"), sentinel)
	rec := env.do(http.MethodPost, "/api/v1/admin/secrets", payload, subject)
	body := requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if !strings.Contains(fmt.Sprint(body["message"]), "不接收 Secret 值") {
		t.Fatalf("400 message 应说明不接收 Secret 值: %v", body["message"])
	}
	if strings.Contains(rec.Body.String(), sentinel) {
		t.Fatalf("错误响应不应回显提交的值: %s", rec.Body.String())
	}

	// 2) 传 data 对象也拒绝。
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"s","key":"k","data":{"k":%q}}`,
			uniqueAdminName("it-secret-bad"), sentinel), subject)
	requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)

	// 3) 正常登记 → 201，回显定位信息（不是值）。
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"tp-secret-endpoint-mock","key":"token","description":"it 引用"}`,
			name), subject)
	created := requireStatus(t, rec, http.StatusCreated)
	if created["name"] != name || created["namespace"] != "testplane" ||
		created["secret_name"] != "tp-secret-endpoint-mock" || created["key"] != "token" {
		t.Fatalf("创建响应异常: %v", created)
	}
	if created["created_by"] != actor {
		t.Fatalf("created_by = %v，期望 %s", created["created_by"], actor)
	}
	if _, exists := created["value"]; exists {
		t.Fatalf("响应绝不能有 value 字段: %v", created)
	}

	// 4) 重名 → 409。
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"s","key":"k"}`, name), subject)
	requireError(t, rec, http.StatusConflict, domain.CodeConflict)

	// 5) 缺 key → 400。
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"s"}`, uniqueAdminName("it-secret-nokey")), subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if detailOf(t, body)["field"] != "key" {
		t.Fatalf("detail.field 应为 key: %v", body["detail"])
	}

	// 6) 再登记一条，验证 limit 分页有 next_cursor；坏游标 → 400。
	second := uniqueAdminName("it-secret-ref")
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"tp-secret-2","key":"token"}`, second), subject)
	requireStatus(t, rec, http.StatusCreated)
	rec = env.do(http.MethodGet, "/api/v1/admin/secrets?limit=1", "", subject)
	page := requireStatus(t, rec, http.StatusOK)
	items, _ := page["items"].([]any)
	if len(items) != 1 || page["has_more"] != true || page["next_cursor"] == nil {
		t.Fatalf("limit=1 分页异常: %v", page)
	}
	rec = env.do(http.MethodGet, "/api/v1/admin/secrets?cursor=not-a-cursor", "", subject)
	requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)

	// 7) admin 列表：有定位信息，但**搜不到任何值**。
	rec = env.do(http.MethodGet, "/api/v1/admin/secrets?limit=200", "", subject)
	requireStatus(t, rec, http.StatusOK)
	raw := rec.Body.String()
	if !strings.Contains(raw, name) || !strings.Contains(raw, "tp-secret-endpoint-mock") {
		t.Fatalf("admin 列表缺少引用: %s", raw)
	}
	if strings.Contains(raw, sentinel) {
		t.Fatalf("admin 列表泄露了 Secret 值: %s", raw)
	}
	if strings.Contains(raw, `"value"`) {
		t.Fatalf("admin 列表出现了 value 字段: %s", raw)
	}

	// 8) GET /secrets（operator 及以上，07 §2.3）：只有 name/description。
	rec = env.do(http.MethodGet, "/api/v1/secrets", "", subject)
	viewerList := requireStatus(t, rec, http.StatusOK)
	raw = rec.Body.String()
	if !strings.Contains(raw, name) {
		t.Fatalf("GET /secrets 缺少引用名: %s", raw)
	}
	if strings.Contains(raw, sentinel) || strings.Contains(raw, "tp-secret-endpoint-mock") {
		t.Fatalf("GET /secrets 泄露了 Secret 定位/值: %s", raw)
	}
	if strings.Contains(raw, `"namespace"`) || strings.Contains(raw, `"secret_name"`) {
		t.Fatalf("GET /secrets 不应暴露 namespace/secret_name: %s", raw)
	}
	items, _ = viewerList["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("GET /secrets 为空: %v", viewerList)
	}
	first, _ := items[0].(map[string]any)
	for k := range first {
		if k != "name" && k != "description" {
			t.Fatalf("GET /secrets 出现了多余字段 %q: %v", k, first)
		}
	}
}

func TestAdminSecrets_Rotate(t *testing.T) {
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-rotate")
	actor := "user:" + subject
	name := uniqueAdminName("it-rotate-ref")

	rec := env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":"testplane","secret_name":"tp-secret-endpoint-mock","key":"token"}`, name), subject)
	requireStatus(t, rec, http.StatusCreated)
	rotatePath := "/api/v1/admin/secrets/" + name + "/rotate"

	// 1) 引用不存在 → 404。
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets/"+uniqueAdminName("it-no-such-ref")+"/rotate", "", subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)

	// 2) 目标 Secret 不存在 → 404 NOT_FOUND。
	probe := &fakeSecretProbe{exists: false}
	withSecretProbe(t, probe)
	rec = env.do(http.MethodPost, rotatePath, "", subject)
	body := requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)
	if detailOf(t, body)["secret_name"] != "tp-secret-endpoint-mock" {
		t.Fatalf("404 detail 应带 secret_name: %v", body["detail"])
	}

	// 3) Secret 存在但 key 为空 → 400 VALIDATION_ERROR。
	withSecretProbe(t, &fakeSecretProbe{exists: true, keyNonEmpty: false})
	rec = env.do(http.MethodPost, rotatePath, "", subject)
	body = requireError(t, rec, http.StatusBadRequest, domain.CodeValidationError)
	if detailOf(t, body)["key"] != "token" {
		t.Fatalf("400 detail 应带 key: %v", body["detail"])
	}

	// 4) 读集群失败 → 503 EXECUTOR_UNAVAILABLE（可重试）。
	withSecretProbe(t, &fakeSecretProbe{err: errors.New("dial tcp: connection refused")})
	rec = env.do(http.MethodPost, rotatePath, "", subject)
	body = requireError(t, rec, http.StatusServiceUnavailable, domain.CodeExecutorUnavailable)
	if detailOf(t, body)["reason"] != "k8s_unavailable" {
		t.Fatalf("不可达时 reason 应为 k8s_unavailable: %v", body["detail"])
	}

	// 4b) 无权限（D-55 的 resourceNames Role 没 apply）→ 503 + detail.reason=forbidden。
	//     评审 #5：必须让运维一眼看出是"授权没落"，而不是笼统的集群不可达。
	withSecretProbe(t, &fakeSecretProbe{err: errSecretForbidden})
	rec = env.do(http.MethodPost, rotatePath, "", subject)
	body = requireError(t, rec, http.StatusServiceUnavailable, domain.CodeExecutorUnavailable)
	if detailOf(t, body)["reason"] != "forbidden" {
		t.Fatalf("无权限时 reason 应为 forbidden: %v", body["detail"])
	}

	// 5) 成功 → 200 {rotated:true, verified:true, checked_at}，并写 rotate 审计。
	okProbe := &fakeSecretProbe{exists: true, keyNonEmpty: true}
	withSecretProbe(t, okProbe)
	rec = env.do(http.MethodPost, rotatePath, "", subject)
	rotated := requireStatus(t, rec, http.StatusOK)
	if rotated["rotated"] != true {
		t.Fatalf("rotated 应为 true: %v", rotated)
	}
	if rotated["verified"] != true {
		t.Fatalf("响应应带 verified=true（e2e 依赖它）：%v", rotated)
	}
	checkedAt, err := time.Parse(time.RFC3339, fmt.Sprint(rotated["checked_at"]))
	if err != nil {
		t.Fatalf("checked_at 不是 RFC3339: %v (%v)", rotated["checked_at"], err)
	}
	if time.Since(checkedAt) > 5*time.Minute {
		t.Fatalf("checked_at 不像当前时间: %v", checkedAt)
	}
	if okProbe.calls != 1 || okProbe.lastNS != "testplane" ||
		okProbe.lastSecret != "tp-secret-endpoint-mock" || okProbe.lastKey != "token" {
		t.Fatalf("探针参数异常: calls=%d ns=%s secret=%s key=%s",
			okProbe.calls, okProbe.lastNS, okProbe.lastSecret, okProbe.lastKey)
	}
	rows := env.auditRows(store.AuditFilter{
		Actor: actor, Action: "secret_rotated", ObjectType: "secret_ref", ObjectID: name, Limit: 20,
	})
	// 前面的 404/400/503 是失败尝试（也写审计），这里只取成功那条。
	okRows := make([]store.AuditRecord, 0, 1)
	for _, row := range rows {
		var d map[string]any
		if err := json.Unmarshal(row.Detail, &d); err == nil && d["result"] == "ok" {
			okRows = append(okRows, row)
		}
	}
	if len(okRows) != 1 {
		t.Fatalf("rotate 应写 1 条成功审计，得到 %d（总行数 %d）", len(okRows), len(rows))
	}
	var detail map[string]any
	if err := json.Unmarshal(okRows[0].Detail, &detail); err != nil {
		t.Fatalf("审计 detail 不是 JSON: %v", err)
	}
	if detail["result"] != "ok" || detail["secret_name"] != "tp-secret-endpoint-mock" {
		t.Fatalf("审计 detail 异常: %v", detail)
	}
	if detail["verified"] != true {
		t.Fatalf("成功审计应带 verified=true: %v", detail)
	}
	if _, ok := detail["checked_at"].(string); !ok {
		t.Fatalf("成功审计应带 checked_at: %v", detail)
	}
	// 审计里不允许出现任何值形态的字段。
	for _, forbidden := range []string{"value", "secret_value", "data"} {
		if _, exists := detail[forbidden]; exists {
			t.Fatalf("审计 detail 不应包含 %q: %v", forbidden, detail)
		}
	}

	// 6) 登记行本身不被 rotate 改动（轮换失败/成功都不回滚登记）。
	ref, err := env.st.GetSecretRef(context.Background(), name)
	if err != nil {
		t.Fatalf("读登记行: %v", err)
	}
	if ref.Key != "token" || ref.SecretName != "tp-secret-endpoint-mock" {
		t.Fatalf("rotate 不应修改登记行: %+v", ref)
	}
}
