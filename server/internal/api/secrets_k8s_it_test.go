package api

// 真实集群的 rotate 冒烟测试（默认 skip，CI 不开）。
//
// 为什么单独放一个 gated 文件：单测用假探针覆盖分支，这里验证**生产路径**——
// 真 kubeconfig → client-go → `get Secret`（D-55 的唯一例外）→ 200/404 与审计。
//
// 运行（本地 kind，Secret 由 deploy 预置）：
//
//	$env:TESTPLANE_TEST_K8S_REAL='1'
//	$env:TESTPLANE_TEST_DATABASE_URL='postgres://testplane:testplane@127.0.0.1:5432/testplane?sslmode=disable'
//	go test ./internal/api/ -run TestSecretRotate_RealCluster -count=1 -v
//
// 不改集群、不写 Secret、不打印值：只读地确认"存在且 key 非空"。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

const (
	realClusterNamespace  = "testplane"
	realClusterSecretName = "tp-secret-endpoint-mock"
	realClusterSecretKey  = "api_key"
)

func TestSecretRotate_RealCluster(t *testing.T) {
	if os.Getenv("TESTPLANE_TEST_K8S_REAL") != "1" {
		t.Skip("未设置 TESTPLANE_TEST_K8S_REAL=1（需要真实 kind 集群）")
	}
	env := newAdminTestEnv(t)
	subject := uniqueAdminName("it-admin-real")
	actor := "user:" + subject

	// 1) 已登记的真实 Secret → 200（生产探针：kubeconfig → client-go get）。
	name := uniqueAdminName("it-real-secret-ref")
	rec := env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":%q,"secret_name":%q,"key":%q}`,
			name, realClusterNamespace, realClusterSecretName, realClusterSecretKey), subject)
	requireStatus(t, rec, http.StatusCreated)
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets/"+name+"/rotate", "", subject)
	rotated := requireStatus(t, rec, http.StatusOK)
	if rotated["rotated"] != true {
		t.Fatalf("真实 Secret 的 rotate 应成功: %v", rotated)
	}

	// 2) 登记一个不存在的 Secret → 404（不是 200、也不是 503）。
	missing := uniqueAdminName("it-real-missing-ref")
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets",
		fmt.Sprintf(`{"name":%q,"namespace":%q,"secret_name":"tp-does-not-exist-%d","key":"token"}`,
			missing, realClusterNamespace, os.Getpid()), subject)
	requireStatus(t, rec, http.StatusCreated)
	rec = env.do(http.MethodPost, "/api/v1/admin/secrets/"+missing+"/rotate", "", subject)
	requireError(t, rec, http.StatusNotFound, domain.CodeNotFound)

	// 3) 成功那条的审计确实落了 verified=true（且不含量）。
	rows := env.auditRows(store.AuditFilter{
		Actor: actor, Action: "secret_rotated", ObjectType: "secret_ref", ObjectID: name, Limit: 10,
	})
	okRows := 0
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err == nil && detail["verified"] == true {
			okRows++
		}
	}
	if okRows != 1 {
		t.Fatalf("真实 rotate 应写 1 条 verified=true 审计，得到 %d（总行数 %d）", okRows, len(rows))
	}
}
