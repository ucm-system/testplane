package api

// rotate 的 Secret 存在性校验（C-10：平台重读引用做一次可读性校验；D-55 的窄例外）。
//
// 边界（D-55，06 §4.4 的**唯一例外**）：
//   - **只对已登记 `secret_refs` 的那一个 (namespace, secret_name) 做 get**，
//     用来确认 Secret 存在且该 key 非空；RBAC 侧用 `resourceNames` 限定到具体 Secret，
//     不是通配 `secrets get/list/watch`；
//   - **不读值当数据使用**：只看 `len(data[key]) > 0`，值不打印、不返回、不落库、不进日志；
//   - 平台没有 Secret 写权限，rotate 不改任何 K8s 对象，也不改登记行；
//   - 无权限 / 不可达 → 向上返回 error，由 handler 映射 503 EXECUTOR_UNAVAILABLE，
//     **绝不**降级成"看起来成功"；
//   - 客户端按 in-cluster 优先、本地 kubeconfig 兜底的顺序构造。
//
// 为什么这里没有复用 internal/k8s.NewClientset：那个包为了 Volcano 快照的类型
// 反向 import 了本包（internal/k8s/volcano.go），api → k8s 会构成 import cycle。
// 这里只做**只读** Secret 探测，凭据解析规则与 internal/k8s 保持一致；等 k8s 包的
// 反向依赖拆掉（把 Volcano 投影类型挪到 ports）后应切回 k8s.NewClientset。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// errSecretForbidden 表示"能连上集群但没权限读这条 Secret"。
// 调用方据此把 detail.reason 报成 forbidden（说明 D-55 的 resourceNames Role 没 apply），
// 而不是笼统的 k8s_unavailable——评审 #5 要求运维能一眼看出是授权没落。
var errSecretForbidden = errors.New("k8s: 无权读取该 Secret")

// SecretProbe 校验 secret_refs 指向的 K8s Secret 与 key 是否可读。
type SecretProbe interface {
	// CheckSecretKey 返回 (Secret 是否存在, key 是否存在且非空, 错误)。
	// 不存在不是错误（由调用方映射 404）；错误表示集群不可达 / 无权限
	// （无权限包 errSecretForbidden）。
	CheckSecretKey(ctx context.Context, namespace, secretName, key string) (exists bool, keyNonEmpty bool, err error)
}

// secretProbeOverride 允许集成测试注入假探针；生产路径永远为 nil。
var secretProbeOverride SecretProbe

// secretProbe 返回当前可用的探针（测试注入优先）。
func (s *Server) secretProbe() (SecretProbe, error) {
	if secretProbeOverride != nil {
		return secretProbeOverride, nil
	}
	return defaultSecretProbe()
}

var (
	secretProbeOnce sync.Once
	secretProbeVal  SecretProbe
	secretProbeErr  error
)

// defaultSecretProbe 惰性构造一次 clientset（rotate 是低频端点，不为此拖慢启动）。
// 构造失败（不在集群内且没有 kubeconfig）时缓存错误，端点按 503 降级。
func defaultSecretProbe() (SecretProbe, error) {
	secretProbeOnce.Do(func() {
		cs, err := newProbeClientset()
		if err != nil {
			secretProbeErr = err
			return
		}
		secretProbeVal = &k8sSecretProbe{cs: cs}
	})
	return secretProbeVal, secretProbeErr
}

// newProbeClientset 构造只读 clientset：in-cluster → TESTPLANE_KUBECONFIG →
// KUBECONFIG → ~/.kube/config（与 internal/k8s 的凭据顺序一致）。
func newProbeClientset() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		path := firstProbeKubeconfig()
		if path == "" {
			return nil, err
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, err
		}
	}
	cfg.UserAgent = "testplane-controller"
	cfg.QPS = 5
	cfg.Burst = 10
	return kubernetes.NewForConfig(cfg)
}

// firstProbeKubeconfig 按约定顺序找 kubeconfig 文件（KUBECONFIG 允许列表，取第一个存在的）。
func firstProbeKubeconfig() string {
	for _, env := range []string{"TESTPLANE_KUBECONFIG", "KUBECONFIG"} {
		raw := strings.TrimSpace(os.Getenv(env))
		if raw == "" {
			continue
		}
		for _, p := range filepath.SplitList(raw) {
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

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// k8sSecretProbe 用 client-go 只读地取 Secret 对象。
type k8sSecretProbe struct {
	cs kubernetes.Interface
}

func (p *k8sSecretProbe) CheckSecretKey(ctx context.Context, namespace, secretName, key string) (bool, bool, error) {
	secret, err := p.cs.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if apierrors.IsForbidden(err) {
		// 能连上集群但没权限：D-55 的 resourceNames Role 没 apply（或 apply 到了别的 namespace）。
		// 只把"哪条 Secret 读不了"包进 sentinel，不回传 k8s 原文（可能含用户/角色信息）。
		return false, false, fmt.Errorf("%w（%s/%s）: %w", errSecretForbidden, namespace, secretName, err)
	}
	if err != nil {
		return false, false, err
	}
	v, ok := secret.Data[key]
	return true, ok && len(v) > 0, nil
}
