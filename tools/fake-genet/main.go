// Command fake-genet 是一个可独立运行的假 Genet（Helm OpenAPI 子集），
// 供 TestPlane 在本地/kind 上验证"真实 Genet 客户端"（TESTPLANE_DEPLOY_PROVIDER=genet）。
//
// 语义按 design/impl/execution.md §9 与调研（.data/research/genet-deploy-plan.md §6.2）：
//   - POST /api/open/helm/uc-packages/{stack|pd}/releases  → helm upgrade --install（noWait，不 --wait）
//   - GET  /api/open/helm/releases/{name}[?light=true]     → status + callInfo.endpoint + pods
//   - DELETE /api/open/helm/releases/{name}?force=&keepHistory= → helm uninstall
//   - GET  /api/open/helm/releases[?light=true]            → 列表
//   - Bearer 鉴权：Authorization: Bearer <FAKE_GENET_API_KEY>
//   - namespace 由"API Key 绑定"（本实现用 -namespace 固定），请求体不能指定
//
// 两种驱动：
//   - 默认 -simulate：纯内存（不碰集群），用于快速联调客户端；
//   - -helm：真跑 helm/kubectl（宿主上执行，KUBECONFIG 指向 kind）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		addr       = flag.String("addr", ":18090", "监听地址")
		namespace  = flag.String("namespace", "testplane-mock", "API Key 绑定的 namespace（不由请求指定）")
		chartDir   = flag.String("chart", "deploy/charts/mock-llm", "mock-llm chart 目录（-helm 模式用）")
		helmBin    = flag.String("helm-bin", ".tools/helm.exe", "helm 可执行文件")
		kubectlBin = flag.String("kubectl", "kubectl", "kubectl 可执行文件")
		kubeconfig = flag.String("kubeconfig", "", "KUBECONFIG 路径（空则用环境变量）")
		apiKeyEnv  = flag.String("api-key-env", "FAKE_GENET_API_KEY", "存放 API Key 的环境变量名")
		apiKeyDef  = flag.String("api-key", "", "直接指定 API Key（优先于环境变量）")
		model      = flag.String("model", "mock-llm", "callInfo.model 的取值")
		helmMode   = flag.Bool("helm", false, "用真实 helm/kubectl 驱动（默认 false = 内存模拟）")
		readyAfter = flag.Duration("sim-ready-after", 3*time.Second, "-simulate 模式下安装到 ready 的耗时")
	)
	flag.Parse()

	apiKey := *apiKeyDef
	if apiKey == "" {
		apiKey = os.Getenv(*apiKeyEnv)
	}
	if apiKey == "" {
		apiKey = "tp-local-2026"
	}

	var store Store
	if *helmMode {
		if _, err := os.Stat(*helmBin); err != nil {
			log.Fatalf("fake-genet: helm 不存在 %s: %v（去掉 -helm 用内存模拟）", *helmBin, err)
		}
		store = &HelmStore{
			Helm: *helmBin, Kubectl: *kubectlBin, Kubeconfig: *kubeconfig,
			Namespace: *namespace, ChartDir: *chartDir, ChartName: "mock-llm",
		}
		log.Printf("fake-genet: Helm 模式（chart=%s, ns=%s）", *chartDir, *namespace)
	} else {
		store = NewSimStore(time.Now, *readyAfter)
		log.Printf("fake-genet: 模拟模式（ready-after=%s）", *readyAfter)
	}

	srv := NewServer(store, *namespace, apiKey, *model, time.Now)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("fake-genet: listening on %s（Bearer %s...）", *addr, mask(apiKey))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("fake-genet: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	fmt.Println("fake-genet: stopped")
}

func mask(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return s[:4] + "***"
}
