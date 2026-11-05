package api

// 真实集群的 rotate / RBAC 冒烟测试（默认 skip，CI 不开）。
//
// 两个门控用例，**身份不同**（评审 #5 的要点：不能只用本机 admin kubeconfig 证明部署可用）：
//
//	TestSecretRotate_RealCluster        本机 admin kubeconfig → 只验证 client-go get 路径
//	TestSecretProbe_ServiceAccountRBAC  部署 SA 的 token（TESTPLANE_TEST_SA_TOKEN）→ 验证
//	                                    D-55 的 resourceNames 窄授权：登记项可读、其它 Secret 必须 forbidden
//
// 运行（本地 kind）：
//
//	$env:TESTPLANE_TEST_K8S_REAL='1'
//	$env:TESTPLANE_TEST_DATABASE_URL='postgres://testplane:testplane@127.0.0.1:5432/testplane?sslmode=disable'
//	$env:TESTPLANE_TEST_SA_TOKEN = (kubectl -n testplane create token testplane)
//	go test ./internal/api/ -run 'TestSecretRotate_RealCluster|TestSecretProbe_ServiceAccountRBAC' -count=1 -v
//
// 不改集群、不写 Secret、不打印值：只读地确认"存在且 key 非空"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

const (
	realClusterNamespace  = "testplane"
	realClusterSecretName = "tp-secret-endpoint-mock"
	realClusterSecretKey  = "api_key"
	// realClusterControlPlaneSA 是部署的 ServiceAccount（deploy/k8s/serviceaccount.yaml）。
	realClusterControlPlaneSA = "testplane"
)

// TestSecretRotate_RealCluster 用**本机 kubeconfig**（admin 身份）走一遍 rotate HTTP 链路。
// 注意：它证明的是 handler + client-go 能读；**不能**证明部署的 SA 有权限——
// 后者由 TestSecretProbe_ServiceAccountRBAC（SA token）与 admin-check.ps1 覆盖。
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
	if rotated["rotated"] != true || rotated["verified"] != true {
		t.Fatalf("真实 Secret 的 rotate 应 200 + rotated/verified=true: %v", rotated)
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

// TestSecretProbe_ServiceAccountRBAC 用**部署 SA 的 token** 验证 D-55 的窄授权：
//   - 已登记的那条 Secret：get 成功（rotate 才可能返回 verified）；
//   - 未登记的另一条 Secret：必须 forbidden（证明 resourceNames 生效，不是通配授权）。
//
// Role 来自 deploy/k8s/gen-secret-rotate-role.ps1 生成的清单；没 apply 时本用例红。
func TestSecretProbe_ServiceAccountRBAC(t *testing.T) {
	if os.Getenv("TESTPLANE_TEST_K8S_REAL") != "1" {
		t.Skip("未设置 TESTPLANE_TEST_K8S_REAL=1（需要真实 kind 集群）")
	}
	token := strings.TrimSpace(os.Getenv("TESTPLANE_TEST_SA_TOKEN"))
	if token == "" {
		t.Skip("未设置 TESTPLANE_TEST_SA_TOKEN（kubectl -n testplane create token " + realClusterControlPlaneSA + "）")
	}
	path := firstProbeKubeconfig()
	if path == "" {
		t.Skip("找不到 kubeconfig（TESTPLANE_KUBECONFIG / KUBECONFIG / ~/.kube/config）")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("加载 kubeconfig: %v", err)
	}
	// 用 SA token 覆盖凭据，但保留 kubeconfig 里的 server/CA。
	// **必须清掉客户端证书**：kind 的 kubeconfig 带 client-certificate-data，
	// 与 Bearer token 同时存在时 apiserver 会按证书认证，测的就不是 SA 了。
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	cfg.CertData = nil
	cfg.KeyData = nil
	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.ExecProvider = nil
	cfg.AuthProvider = nil
	cfg.Username = ""
	cfg.Password = ""
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("构造 clientset: %v", err)
	}
	probe := &k8sSecretProbe{cs: cs}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exists, keyNonEmpty, err := probe.CheckSecretKey(ctx, realClusterNamespace, realClusterSecretName, realClusterSecretKey)
	if err != nil {
		t.Fatalf("部署 SA 读已登记 Secret 应成功（Role 没 apply？跑 deploy/k8s/gen-secret-rotate-role.ps1）: %v", err)
	}
	if !exists || !keyNonEmpty {
		t.Fatalf("已登记 Secret 应存在且 key 非空: exists=%v keyNonEmpty=%v", exists, keyNonEmpty)
	}

	// 未登记的另一条 Secret：resourceNames 之外 → 必须 forbidden。
	if _, _, err := probe.CheckSecretKey(ctx, realClusterNamespace, "testplane-db", "dsn"); !errors.Is(err, errSecretForbidden) {
		t.Fatalf("未登记的 Secret 必须被 resourceNames 拒绝（errSecretForbidden），得到 err=%v", err)
	}
}
