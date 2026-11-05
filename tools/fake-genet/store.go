package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotInstalled 表示 release 不存在（对应真实 Genet 的 404）。
var ErrNotInstalled = errors.New("fake-genet: release not installed")

// Pod 是 release 下的一个 Pod（只保留客户端会用到的字段）。
type Pod struct {
	Name     string
	Phase    string
	NodeName string
	Ready    bool
}

// ReleaseInfo 是一次 release 查询的结果。
type ReleaseInfo struct {
	Name         string
	Namespace    string
	Status       string // helm 原值：pending-install | deployed | failed | uninstalled …
	Description  string
	Revision     int
	Chart        string
	ChartVersion string
	Pods         []Pod
}

// Store 是 helm 操作的抽象：真实实现走 helm/kubectl CLI，测试用内存实现。
type Store interface {
	// Install 执行 `helm upgrade --install`（noWait：不 --wait）。
	Install(ctx context.Context, release, valuesYAML string, set []string) error
	// Status 返回 release 状态；不存在返回 ErrNotInstalled。
	Status(ctx context.Context, release string) (ReleaseInfo, error)
	// Uninstall 执行 `helm uninstall`；不存在返回 ErrNotInstalled。
	Uninstall(ctx context.Context, release string) error
	// List 列出本 namespace 的 release。
	List(ctx context.Context) ([]ReleaseInfo, error)
}

// ---- 真实 helm 实现 -------------------------------------------------------

// HelmStore 用 helm/kubectl CLI 驱动真实集群（宿主上跑，kubeconfig 指向 kind）。
type HelmStore struct {
	Helm       string // helm 可执行文件路径
	Kubectl    string // kubectl 可执行文件路径
	Kubeconfig string // KUBECONFIG 路径（空则用环境变量）
	Namespace  string
	ChartDir   string // deploy/charts/mock-llm
	ChartName  string // mock-llm（用于拼 Service 名）
}

// Install 执行 helm upgrade --install（不带 --wait：对应 Genet 的 noWait=true）。
func (h *HelmStore) Install(ctx context.Context, release, valuesYAML string, set []string) error {
	args := []string{"upgrade", "--install", release, h.ChartDir, "-n", h.Namespace, "--create-namespace"}
	if valuesYAML != "" {
		args = append(args, "-f", "-")
	}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.CommandContext(ctx, h.Helm, args...)
	cmd.Env = h.env()
	if valuesYAML != "" {
		cmd.Stdin = strings.NewReader(valuesYAML)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm upgrade --install %s: %w: %s", release, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Status 用 `helm status -o json` 取状态，再用 kubectl 查 Pod Ready。
func (h *HelmStore) Status(ctx context.Context, release string) (ReleaseInfo, error) {
	out, err := h.run(ctx, h.Helm, "status", release, "-n", h.Namespace, "-o", "json")
	if err != nil {
		if isNotFound(err) {
			return ReleaseInfo{}, ErrNotInstalled
		}
		return ReleaseInfo{}, err
	}
	var raw struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status      string `json:"status"`
			Description string `json:"description"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return ReleaseInfo{}, fmt.Errorf("decode helm status: %w", err)
	}
	info := ReleaseInfo{
		Name:         raw.Name,
		Namespace:    raw.Namespace,
		Status:       raw.Info.Status,
		Description:  raw.Info.Description,
		Revision:     raw.Version,
		Chart:        raw.Chart.Metadata.Name,
		ChartVersion: raw.Chart.Metadata.Version,
	}
	pods, perr := h.pods(ctx, release)
	if perr == nil {
		info.Pods = pods
	}
	return info, nil
}

// Uninstall 执行 helm uninstall。
func (h *HelmStore) Uninstall(ctx context.Context, release string) error {
	out, err := h.run(ctx, h.Helm, "uninstall", release, "-n", h.Namespace)
	if err != nil {
		if isNotFound(err) {
			return ErrNotInstalled
		}
		return err
	}
	_ = out
	return nil
}

// List 列出本 namespace 的 release。
func (h *HelmStore) List(ctx context.Context) ([]ReleaseInfo, error) {
	out, err := h.run(ctx, h.Helm, "list", "-n", h.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Status     string `json:"status"`
		Chart      string `json:"chart"`
		Revision   string `json:"revision"`
		AppVersion string `json:"app_version"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode helm list: %w", err)
	}
	res := make([]ReleaseInfo, 0, len(raw))
	for _, r := range raw {
		chart := r.Chart
		version := ""
		if i := strings.LastIndex(chart, "-"); i > 0 {
			version = chart[i+1:]
			chart = chart[:i]
		}
		res = append(res, ReleaseInfo{Name: r.Name, Namespace: r.Namespace, Status: r.Status, Chart: chart, ChartVersion: version})
	}
	return res, nil
}

// pods 用 kubectl 查该 release 的 Pod（按名字前缀 `<release>-` 过滤，避免依赖 chart 的 label）。
func (h *HelmStore) pods(ctx context.Context, release string) ([]Pod, error) {
	out, err := h.run(ctx, h.Kubectl, "get", "pods", "-n", h.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Ready bool `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode kubectl pods: %w", err)
	}
	prefix := release + "-"
	res := make([]Pod, 0, 4)
	for _, it := range list.Items {
		if !strings.HasPrefix(it.Metadata.Name, prefix) {
			continue
		}
		ready := len(it.Status.ContainerStatuses) > 0
		for _, cs := range it.Status.ContainerStatuses {
			if !cs.Ready {
				ready = false
			}
		}
		res = append(res, Pod{Name: it.Metadata.Name, Phase: it.Status.Phase, NodeName: it.Spec.NodeName, Ready: ready})
	}
	return res, nil
}

func (h *HelmStore) run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = h.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (h *HelmStore) env() []string {
	env := os.Environ()
	if h.Kubeconfig != "" {
		env = append(env, "KUBECONFIG="+h.Kubeconfig)
	}
	return env
}

func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such release") ||
		strings.Contains(msg, "release: not found")
}

// ---- 内存实现（-simulate 与单测用） --------------------------------------

// SimStore 不碰集群：安装后过 readyAfter 变 deployed，Pod 视为 Ready。
type SimStore struct {
	mu         sync.Mutex
	rel        map[string]*simRelease
	Now        func() time.Time
	ReadyAfter time.Duration
}

type simRelease struct {
	name      string
	status    string
	desc      string
	chart     string
	version   string
	revision  int
	readyAt   time.Time
	uninstall bool
}

// NewSimStore 构造内存实现。
func NewSimStore(now func() time.Time, readyAfter time.Duration) *SimStore {
	return &SimStore{rel: make(map[string]*simRelease), Now: now, ReadyAfter: readyAfter}
}

func (s *SimStore) Install(ctx context.Context, release, valuesYAML string, set []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rel[release]; ok {
		return fmt.Errorf("release %s already has protected metadata; install cannot replace an existing release", release)
	}
	s.rel[release] = &simRelease{
		name: release, status: "pending-install", desc: "Initial install underway",
		chart: "mock-llm", version: "0.1.0", revision: 1,
		readyAt: s.Now().Add(s.ReadyAfter),
	}
	return nil
}

func (s *SimStore) Status(ctx context.Context, release string) (ReleaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rel[release]
	if !ok || r.uninstall {
		return ReleaseInfo{}, ErrNotInstalled
	}
	status := r.status
	if status == "pending-install" && !s.Now().Before(r.readyAt) {
		r.status = "deployed"
		r.desc = "Upgrade complete"
		status = r.status
	}
	info := ReleaseInfo{Name: r.name, Status: status, Description: r.desc, Chart: r.chart, ChartVersion: r.version, Revision: r.revision}
	if status == "deployed" {
		info.Pods = []Pod{{Name: r.name + "-mock-llm-0", Phase: "Running", NodeName: "testplane-control-plane", Ready: true}}
	} else {
		info.Pods = []Pod{{Name: r.name + "-mock-llm-0", Phase: "Pending"}}
	}
	return info, nil
}

func (s *SimStore) Uninstall(ctx context.Context, release string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rel[release]
	if !ok || r.uninstall {
		return ErrNotInstalled
	}
	r.uninstall = true
	delete(s.rel, release)
	return nil
}

func (s *SimStore) List(ctx context.Context) ([]ReleaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReleaseInfo, 0, len(s.rel))
	for _, r := range s.rel {
		out = append(out, ReleaseInfo{Name: r.name, Status: r.status, Chart: r.chart, ChartVersion: r.version, Revision: r.revision})
	}
	return out, nil
}
