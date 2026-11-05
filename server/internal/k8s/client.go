// Package k8s 负责构造 Kubernetes 客户端与解析执行层配置。
//
// 选型（design/impl/execution.md §1、§4）：MVP 用 client-go 直连 API（list+watch），
// 不上 shared informer；因此这里只提供 clientset，不引入 informer 工厂。
//
// 集群内运行时优先 in-cluster（Pod 的 ServiceAccount），本地开发退回 kubeconfig：
// TESTPLANE_KUBECONFIG → KUBECONFIG → ~/.kube/config。二者都没有时返回可读的错误，
// 由调用方决定是降级还是拒绝启动。
package k8s

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// DefaultNamespace 是 Job / PVC 所在 namespace（C-11 TESTPLANE_EXECUTOR_NAMESPACE 的默认值）。
	DefaultNamespace = "testplane"

	// EnvNamespace 是首选的环境变量名；EnvNamespaceLegacy 是 C-11 里的旧名，兼容读取。
	EnvNamespace       = "TESTPLANE_NAMESPACE"
	EnvNamespaceLegacy = "TESTPLANE_EXECUTOR_NAMESPACE"
	// EnvKubeconfig 显式指定 kubeconfig 路径（本地开发用）。
	EnvKubeconfig = "TESTPLANE_KUBECONFIG"
	// EnvInCluster 显式开关 in-cluster 探测（"false" 时直接走 kubeconfig）。
	EnvInCluster = "TESTPLANE_K8S_IN_CLUSTER"
)

var (
	// ErrInClusterMissing 表示显式要求 in-cluster 但拿不到凭据。
	ErrInClusterMissing = errors.New("k8s: in-cluster config unavailable")
	// ErrNoConfig 表示既不在集群内、也没有可用 kubeconfig。
	ErrNoConfig = errors.New("k8s: no in-cluster config and no kubeconfig found")
)

// Options 是客户端构造参数。零值即"按环境变量自动探测"。
type Options struct {
	// Namespace 为空时取 TESTPLANE_NAMESPACE / TESTPLANE_EXECUTOR_NAMESPACE / testplane。
	Namespace string
	// Kubeconfig 为空时按 TESTPLANE_KUBECONFIG → KUBECONFIG → ~/.kube/config 回退。
	Kubeconfig string
	// InCluster 为 nil 时自动探测；显式给出则按它走（测试与本地联调方便）。
	InCluster *bool
	// QPS/Burst 为 0 时使用 client-go 默认值。
	QPS   float32
	Burst int
	// UserAgent 供审计与 apiserver 侧排查使用，为空时取默认值。
	UserAgent string
}

// NamespaceFromEnv 解析 namespace：TESTPLANE_NAMESPACE 优先，其次
// TESTPLANE_EXECUTOR_NAMESPACE（C-11 的旧名），都没有则用 DefaultNamespace。
func NamespaceFromEnv() string {
	for _, key := range []string{EnvNamespace, EnvNamespaceLegacy} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return DefaultNamespace
}

// OptionsFromEnv 把环境变量读成 Options，供 cmd 层一行接入。
func OptionsFromEnv() Options {
	return Options{
		Namespace:  NamespaceFromEnv(),
		Kubeconfig: strings.TrimSpace(os.Getenv(EnvKubeconfig)),
	}
}

func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = NamespaceFromEnv()
	}
	if o.Kubeconfig == "" {
		o.Kubeconfig = strings.TrimSpace(os.Getenv(EnvKubeconfig))
	}
	if o.UserAgent == "" {
		o.UserAgent = "testplane-controller"
	}
	return o
}

// NewClientset 按 Options 构造 clientset。
//
// 顺序：显式 kubeconfig > in-cluster > 默认 kubeconfig。显式给了路径却读不出来是错误，
// 不静默回退——配错了应当立刻暴露。
func NewClientset(o Options) (kubernetes.Interface, error) {
	o = o.withDefaults()
	cfg, err := restConfig(o)
	if err != nil {
		return nil, err
	}
	if o.QPS > 0 {
		cfg.QPS = o.QPS
	}
	if o.Burst > 0 {
		cfg.Burst = o.Burst
	}
	cfg.UserAgent = o.UserAgent
	return kubernetes.NewForConfig(cfg)
}

// New 是 NewClientset 的便利封装，同时返回应使用的 namespace。
func New(o Options) (kubernetes.Interface, string, error) {
	o = o.withDefaults()
	cs, err := NewClientset(o)
	if err != nil {
		return nil, "", err
	}
	return cs, o.Namespace, nil
}

// restConfig 决定用哪套凭据。
func restConfig(o Options) (*rest.Config, error) {
	if o.Kubeconfig != "" {
		return configFromKubeconfig(o.Kubeconfig)
	}
	inCluster := true
	if o.InCluster != nil {
		inCluster = *o.InCluster
	} else if v := strings.TrimSpace(os.Getenv(EnvInCluster)); v != "" {
		// C-11: TESTPLANE_K8S_IN_CLUSTER（默认 true）。
		inCluster = !strings.EqualFold(v, "false") && v != "0"
	}
	if inCluster {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		} else if o.InCluster != nil && *o.InCluster {
			// 显式要求 in-cluster 却拿不到，直接报错，不要悄悄换凭据。
			return nil, fmt.Errorf("%w: %v", ErrInClusterMissing, err)
		}
	}
	path := firstKubeconfigPath()
	if path == "" {
		return nil, ErrNoConfig
	}
	return configFromKubeconfig(path)
}

// firstKubeconfigPath 按约定顺序找 kubeconfig 文件。
func firstKubeconfigPath() string {
	if v := strings.TrimSpace(os.Getenv("KUBECONFIG")); v != "" {
		// KUBECONFIG 允许用 filepath.ListSeparator 连接多个路径，取第一个存在的。
		for _, p := range filepath.SplitList(v) {
			if p != "" && fileExists(p) {
				return p
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".kube", "config")
	if fileExists(p) {
		return p
	}
	return ""
}

func configFromKubeconfig(path string) (*rest.Config, error) {
	if !fileExists(path) {
		return nil, fmt.Errorf("k8s: kubeconfig %q: %w", path, os.ErrNotExist)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("k8s: load kubeconfig %q: %w", path, err)
	}
	return cfg, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
