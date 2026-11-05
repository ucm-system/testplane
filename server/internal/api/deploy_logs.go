package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/domain"
)

// Deploy 日志端点（`GET /runs/{run_id}/deploy/logs`，07 §5.1、web-and-delivery §53）。
//
// 数据源：K8s 日志 API（`pods/log`，client-go `GetLogs`，**只读**）——平台不授予
// `pods/exec`（execution §8）。Deploy 的 Pod 属于 Genet 的 namespace
// （`runs.deploy.namespace`），不一定是本平台 namespace。
//
// 分页语义（与 openapi `DeployLogs` 一致）：
//   - `offset`：在该 Pod 日志流里的**行偏移**（对所有 Pod 同时生效，多 Pod 并行翻页）；
//   - `limit`：本次返回的总行数上限（默认 200，上限 2000），按 Pod 顺序填充；
//   - `tail_lines`：只取每个 Pod 日志末尾 N 行（传给 K8s，用于"先看最新"）；
//   - `next_offset`：下一次调用的 `offset`（`0` 表示没有更多）；`truncated` 表示被截断。
const (
	deployLogDefaultLimit = 200
	deployLogMaxLimit     = 2000
	deployLogMaxTailLines = 5000
	// deployLogContainer 默认容器名；空则取 Pod 的第一个容器。
	deployLogContainer = ""
)

// DeployLogPod 是某个 Deploy Pod 的元信息（前端"按 Pod 分页签"）。
type DeployLogPod struct {
	Name       string     `json:"name"`
	Ready      bool       `json:"ready"`
	Restarts   int32      `json:"restarts"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// DeployLogEntry 是一段日志（一个 Pod 的一段连续行）。
type DeployLogEntry struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Offset    int    `json:"offset"`
	LineCount int    `json:"line_count"`
	Text      string `json:"text"`
}

// DeployLogGap 解释"这里断过"：重启导致的缺口，平台不假装连续。
type DeployLogGap struct {
	Pod    string    `json:"pod"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

// DeployLogsResponse 是 `GET /runs/{run_id}/deploy/logs` 的响应体。
type DeployLogsResponse struct {
	RunID      string           `json:"run_id"`
	Pods       []DeployLogPod   `json:"pods"`
	Entries    []DeployLogEntry `json:"entries"`
	Gaps       []DeployLogGap   `json:"gaps"`
	Truncated  bool             `json:"truncated"`
	NextOffset int              `json:"next_offset"`
	Message    string           `json:"message,omitempty"`
}

// deployLogClient 是本端点需要的**最小** K8s 能力（只读）：
// 列 Pod + 取日志。收窄成接口是为了单测能直接伪造（含 Forbidden），
// 也保证这里永远不会碰 pods/exec（execution §8）。
type deployLogClient interface {
	ListPods(ctx context.Context, namespace string) ([]corev1.Pod, error)
	Logs(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error)
}

// kubeDeployLogClient 是 deployLogClient 的 client-go 实现。
type kubeDeployLogClient struct{ k kubernetes.Interface }

func (c *kubeDeployLogClient) ListPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	list, err := c.k.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *kubeDeployLogClient) Logs(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error) {
	opts := &corev1.PodLogOptions{Container: container}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	raw, err := c.k.CoreV1().Pods(namespace).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// deployLogsClient 返回本端点的 K8s 客户端；测试可替换（同一进程内只建一次）。
func (s *Server) deployLogsClient() (deployLogClient, error) {
	if s.deployLogsKube != nil {
		return s.deployLogsKube, nil
	}
	client, err := newDeployLogsKubeClient(s.cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	wrapped := &kubeDeployLogClient{k: client}
	s.deployLogsKube = wrapped
	return wrapped, nil
}

// newDeployLogsKubeClient 构造只读客户端：集群内用 ServiceAccount，否则用 kubeconfig。
//
// 为什么不复用 `internal/k8s`：那个包反过来 import 了 `api`（`VolcanoProvider`），
// 直接 import 会成环。这里的逻辑与 `k8s.NewClientset` 等价且刻意保持最小。
func newDeployLogsKubeClient(kubeconfig string) (kubernetes.Interface, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	path := kubeconfig
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		return nil, errors.New("既不在集群内，也没有 kubeconfig（TESTPLANE_KUBECONFIG / KUBECONFIG）")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// handleRunDeployLogs 处理 `GET /api/v1/runs/{run_id}/deploy/logs`。
func (s *Server) handleRunDeployLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")

	run, err := s.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	// runs.deploy 里同时有"有没有 deploy""在哪个 namespace""release 叫什么"。
	var deploy struct {
		Namespace   string `json:"namespace"`
		ReleaseName string `json:"release_name"`
		State       string `json:"state"`
		AcceptedAt  string `json:"accepted_at"`
	}
	if len(run.Deploy) > 0 {
		_ = json.Unmarshal(run.Deploy, &deploy)
	}
	resp := DeployLogsResponse{
		RunID:   runID,
		Pods:    []DeployLogPod{},
		Entries: []DeployLogEntry{},
		Gaps:    []DeployLogGap{},
	}
	if deploy.Namespace == "" && deploy.ReleaseName == "" {
		// 非 deploy 形态的 Run：没有部署日志可看，但也不是 404。
		resp.Message = "该 Run 没有 Deploy 阶段（service 模式）"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ns := deploy.Namespace
	if ns == "" {
		ns = k8sDefaultNamespace(s.cfg)
	}

	// ---- 参数校验（`?pod=` 必选：游标只在单个 Pod 内有意义，见下）----
	q := r.URL.Query()
	podName := strings.TrimSpace(q.Get("pod"))
	if podName == "" {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"缺少必选参数 pod：日志游标只在单个 Pod 内有意义（多 Pod 共用一个 offset 会丢短 Pod 的日志）",
			map[string]any{"param": "pod"}, false)
		return
	}
	offset := atoiDefault(q.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	limit := atoiDefault(q.Get("limit"), deployLogDefaultLimit)
	if limit <= 0 || limit > deployLogMaxLimit {
		limit = deployLogDefaultLimit
	}
	tailLines := atoiDefault(q.Get("tail_lines"), 0)
	if tailLines < 0 {
		tailLines = 0
	}
	if tailLines > deployLogMaxTailLines {
		tailLines = deployLogMaxTailLines
	}

	client, err := s.deployLogsClient()
	if err != nil {
		// 拿不到集群凭据 = 平台侧不可用，不是"没有日志"。
		fail(w, r, http.StatusBadGateway, domain.CodeExecutorUnavailable,
			"无法连接执行集群读取 Deploy 日志", map[string]any{"reason": "kube_client", "error": err.Error()}, true)
		return
	}

	pods, err := client.ListPods(ctx, ns)
	if err != nil {
		s.failDeployLogKubeErr(w, r, "list", err)
		return
	}
	// Genet 的 Pod 名以 release 名开头（`<release>-unified-cache-stack-xyz` 等）；
	// release 未知时退化成"该 namespace 的所有 Pod"。
	if deploy.ReleaseName != "" {
		filtered := make([]corev1.Pod, 0, len(pods))
		for _, pod := range pods {
			if strings.HasPrefix(pod.Name, deploy.ReleaseName) {
				filtered = append(filtered, pod)
			}
		}
		pods = filtered
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })

	var acceptedAt *time.Time
	if deploy.AcceptedAt != "" {
		if ts, perr := time.Parse(time.RFC3339, deploy.AcceptedAt); perr == nil {
			utc := ts.UTC()
			acceptedAt = &utc
		}
	}

	// `pods[]` 永远返回**全部** Pod（前端页签要），gap 也按全部 Pod 算。
	var target *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		runningSince := podStartedAt(*pod)
		restarts := podRestarts(*pod, deployLogContainer)
		lastSeen := podLastSeen(*pod)
		resp.Pods = append(resp.Pods, DeployLogPod{
			Name:       pod.Name,
			Ready:      podReady(*pod),
			Restarts:   restarts,
			StartedAt:  runningSince,
			LastSeenAt: lastSeen,
		})
		if pod.Name == podName {
			target = pod
		}
		if restarts > 0 {
			from := pod.CreationTimestamp.Time.UTC()
			to := from
			if lastSeen != nil {
				to = *lastSeen
			}
			resp.Gaps = append(resp.Gaps, DeployLogGap{
				Pod: pod.Name, From: from, To: to,
				Reason: "pod restarted",
			})
		}
		// Pod 比 Run 受理更晚才起来：中间那段没有日志可读。
		if acceptedAt != nil && runningSince != nil && runningSince.After(*acceptedAt) {
			resp.Gaps = append(resp.Gaps, DeployLogGap{
				Pod: pod.Name, From: *acceptedAt, To: *runningSince,
				Reason: "pod started after deploy accepted",
			})
		}
	}

	// ---- 排队期 / Pod 已回收：显式降级，绝不静默空数组 ----
	//
	// MVP 直读 `pods/log`，**没有**设计要求里 `deploy/logs/<pod>.log` 的落盘副本
	// （见 IMPLEMENTATION §15.2 偏差），所以 Pod 一旦被回收，日志就读不到了——
	// 端点必须诚实地说明这一点，而不是返回一个看起来"没有日志"的空列表。
	if len(resp.Pods) == 0 {
		switch deploy.State {
		case "queued", "accepted", "":
			resp.Message = "尚未产生 Pod（排队中）"
		default:
			resp.Message = "Pod 已回收，日志不可读：MVP 直读 pods/log，尚未落盘 deploy/logs/<pod>.log（IMPLEMENTATION §15.2）"
			from := time.Now().UTC()
			if acceptedAt != nil {
				from = *acceptedAt
			}
			resp.Gaps = append(resp.Gaps, DeployLogGap{
				Pod: podName, From: from, To: time.Now().UTC(),
				Reason: "pod recycled: log no longer readable (reading pods/log directly)",
			})
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 请求的 Pod 不在当前 Pod 列表里：明确 404（打错名字或已被回收），
	// 明细里给出当前可选的 Pod，前端能直接纠正。
	if target == nil {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound,
			"没有这个 Deploy Pod（可能已回收）",
			map[string]any{
				"pod":            podName,
				"reason":         "pod_not_found",
				"available_pods": podNames(resp.Pods),
			}, false)
		return
	}

	// ---- 单 Pod 分页（offset 只在这个 Pod 的日志流里有意义）----
	//
	// 为什么 `?pod=` 必选：多 Pod 共用一个 offset 时，"每页按 Pod 顺序填充 + next_offset 取最大值"
	// 会让短 Pod 永远轮不到（评审 #4 的 P0 反例：p1=5 行、p2=2 行、limit=2 → p2 一行都不出现）。
	// 改成"每次只服务一个 Pod"后，offset/limit/tail_lines 的语义唯一，也与前端 Pod 页签一一对应。
	//
	// tail_lines 与 offset 互斥：tail_lines 是"取窗口末尾 N 行"，窗口一变 offset 就没法稳定翻页，
	// 所以两者同时给出直接 400，而不是让人猜。
	if tailLines > 0 && offset > 0 {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"tail_lines 与 offset 不能同时使用（tail_lines 是窗口末尾语义，无法稳定翻页）",
			map[string]any{"tail_lines": tailLines, "offset": offset}, false)
		return
	}

	container := firstContainer(target.Name, pods)
	raw, err := client.Logs(ctx, ns, target.Name, container, int64(tailLines))
	if err != nil {
		s.failDeployLogKubeErr(w, r, "logs", err)
		return
	}
	lines := splitLogLines(raw)
	if offset < len(lines) {
		rest := lines[offset:]
		take := rest
		if len(take) > limit {
			take = take[:limit]
			resp.Truncated = true
		}
		if len(take) > 0 {
			resp.Entries = append(resp.Entries, DeployLogEntry{
				Pod: target.Name, Container: container,
				Offset: offset, LineCount: len(take),
				Text: strings.Join(take, "\n"),
			})
			if resp.Truncated {
				resp.NextOffset = offset + len(take)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// podNames 返回 Pod 名列表（404 明细里给前端纠正用）。
func podNames(pods []DeployLogPod) []string {
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Name)
	}
	return out
}

// failDeployLogKubeErr 把 K8s 侧错误映射成契约错误：
// 403/401（没有 pods/log 权限）→ 502 EXECUTOR_UNAVAILABLE，绝不映射成 500。
func (s *Server) failDeployLogKubeErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		fail(w, r, http.StatusBadGateway, domain.CodeExecutorUnavailable,
			"读取 Deploy 日志被集群拒绝（缺少 pods/log 权限）",
			map[string]any{"reason": "forbidden", "op": op, "error": err.Error()}, false)
	case apierrors.IsNotFound(err):
		fail(w, r, http.StatusNotFound, domain.CodeNotFound, "对象不存在", nil, false)
	default:
		fail(w, r, http.StatusBadGateway, domain.CodeExecutorUnavailable,
			"读取 Deploy 日志失败",
			map[string]any{"reason": "kube_error", "op": op, "error": err.Error()}, true)
	}
}

func firstContainer(podName string, pods []corev1.Pod) string {
	for _, p := range pods {
		if p.Name != podName {
			continue
		}
		if len(p.Spec.Containers) > 0 {
			return p.Spec.Containers[0].Name
		}
	}
	return ""
}

func podReady(pod corev1.Pod) bool {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Ready {
			return true
		}
	}
	return false
}

func podRestarts(pod corev1.Pod, container string) int32 {
	var max int32
	for _, c := range pod.Status.ContainerStatuses {
		if container != "" && c.Name != container {
			continue
		}
		if c.RestartCount > max {
			max = c.RestartCount
		}
	}
	return max
}

func podStartedAt(pod corev1.Pod) *time.Time {
	if pod.Status.StartTime != nil {
		ts := pod.Status.StartTime.Time.UTC()
		return &ts
	}
	return nil
}

func podLastSeen(pod corev1.Pod) *time.Time {
	for _, c := range pod.Status.ContainerStatuses {
		if c.State.Running != nil {
			ts := c.State.Running.StartedAt.Time.UTC()
			return &ts
		}
		if c.State.Terminated != nil {
			ts := c.State.Terminated.FinishedAt.Time.UTC()
			return &ts
		}
	}
	return podStartedAt(pod)
}

// splitLogLines 按行切日志；去掉末尾空行（K8s 日志总是以 \n 结尾）。
func splitLogLines(raw string) []string {
	raw = strings.TrimRight(raw, "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// k8sDefaultNamespace 在 runs.deploy.namespace 缺失时的兜底（与执行器同口径）。
func k8sDefaultNamespace(cfg *config.Config) string {
	if ns := os.Getenv("TESTPLANE_EXECUTOR_NAMESPACE"); ns != "" {
		return ns
	}
	if ns := os.Getenv("TESTPLANE_NAMESPACE"); ns != "" {
		return ns
	}
	return "testplane"
}
