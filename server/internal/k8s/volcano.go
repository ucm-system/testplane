// Volcano 队列快照的生产者：`GET /api/v1/queue-status` 的 `volcano` 块。
//
// 依据（三份规格，改这里之前先读）：
//   - design/views/06-physical-view.md §2.1（两类排队必须分开、采样约束、降级四态）
//   - design/impl/control-plane.md §C-4.8（字段路径、采样方式、四态）
//   - design/views/07-api.md §5.3（`volcano` 块语义、`"<cluster>/<queue>"` 命名）
//
// 实现形状：dynamic.Interface + unstructured，**不引** volcano.sh/apis（避免被 Volcano
// 版本绑死，同 Genet 的选型）；只 List `scheduling.volcano.sh/v1beta1` 的 queues
// （cluster-scoped）与 podgroups（namespaced）；轮询 + 15s TTL 缓存 + 单飞，失败退避 60s，
// **不用 informer**（不需要 watch 语义，RBAC 也就可以只给 get/list）。
//
// 依赖方向（v2 调整）：快照 DTO（VolcanoQueue / VolcanoSnapshot）与降级四态常量定义在
// **本包**，`internal/api` 反向消费（api 层用类型别名对齐即可）——底层包不 import 上层
// 包，否则上层（api/orchestrator）改造期会连带打断底层包的独立编译。
//
// 红线（D-46）：不可达 ≠ 0 排队。任何降级都只产出 `available=false` + 四态 detail +
// 空 queues，绝不渲染成"队列空闲 / 无排队"；也绝不阻塞 `/queue-status` 的其它两块。
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Volcano 快照的四个降级态（openapi `VolcanoSnapshot.detail` 枚举）。
// 红线：**不可达 ≠ 0 排队**——任何一态都不得渲染成"空闲/无排队"。
const (
	VolcanoCRDMissing           = "crd_missing"           // 集群没装 Volcano
	VolcanoRBACForbidden        = "rbac_forbidden"        // 有 CRD 但无权读
	VolcanoSchedulerUnavailable = "scheduler_unavailable" // 超时/连不上/未接入
	VolcanoDisabled             = "disabled"              // 装了但调度未启用
)

// VolcanoQueue 是 openapi `VolcanoQueue` 的投影（只读快照，不做单位换算）。
type VolcanoQueue struct {
	Name               string            `json:"name"`
	State              *string           `json:"state"`
	Capability         map[string]string `json:"capability"`
	Allocated          map[string]string `json:"allocated"`
	PendingJobs        int               `json:"pending_jobs"`
	LongestWaitSeconds int               `json:"longest_wait_seconds"`
	Reason             *string           `json:"reason"`
}

// VolcanoSnapshot 是 `queue-status.volcano` 块。Provider 为 nil 时默认降级为
// scheduler_unavailable（未接入调度器探针 ≠ 没有排队）。
type VolcanoSnapshot struct {
	Available bool           `json:"available"`
	Detail    *string        `json:"detail"`
	Queues    []VolcanoQueue `json:"queues"`
	Stale     bool           `json:"stale,omitempty"`
}

// VolcanoProvider 是本包对外提供的只读快照能力（api 层按此消费；dynamic client
// 读 Volcano CRD，15s TTL + 单飞）。只读、不得阻塞 `/queue-status`：实现方必须
// 自己带超时并在失败时返回降级快照。
type VolcanoProvider interface {
	VolcanoSnapshot(ctx context.Context) VolcanoSnapshot
}

const (
	// VolcanoAPIGroup 是 Volcano 的 API 组（只读 queues/podgroups）。
	VolcanoAPIGroup = "scheduling.volcano.sh"
	// VolcanoAPIVersion 是唯一的版本口径（installer 与 Genet 都按 v1beta1）。
	VolcanoAPIVersion = "v1beta1"

	// DefaultVolcanoTTL 是快照新鲜度上限（06 §2.1（3）：15s，UI 30s 轮询该端点）。
	DefaultVolcanoTTL = 15 * time.Second
	// DefaultVolcanoTimeout 是单次 List 的超时（execution §11：5s，失败即降级不等待）。
	DefaultVolcanoTimeout = 5 * time.Second
	// DefaultVolcanoBackoff 是连续失败后的重试间隔（06 §2.1（3）：60s）。
	DefaultVolcanoBackoff = 60 * time.Second

	// defaultVolcanoQueue 是 PodGroup 没写 `spec.queue` 时的归属队列名
	// （Volcano 的默认队列就叫 `default`，不给它归组就会漏计）。
	defaultVolcanoQueue = "default"
)

var (
	// VolcanoQueueGVR 是 cluster-scoped 的队列资源。
	VolcanoQueueGVR = schema.GroupVersionResource{
		Group: VolcanoAPIGroup, Version: VolcanoAPIVersion, Resource: "queues",
	}
	// VolcanoPodGroupGVR 是 namespaced 的 PodGroup 资源（与 queue 的关联键是 spec.queue）。
	VolcanoPodGroupGVR = schema.GroupVersionResource{
		Group: VolcanoAPIGroup, Version: VolcanoAPIVersion, Resource: "podgroups",
	}
)

// VolcanoOptions 是快照生产者的构造参数。零值除 Enabled 外都有默认值。
type VolcanoOptions struct {
	// Enabled=false 时直接降级为 `disabled`（平台侧关闭队列调度），不打 API。
	Enabled bool
	// Cluster 是队列名的前缀来源（`"<cluster>/<queue>"`，零契约变更；P0 单集群也带前缀，
	// 取 AG_DEFAULT_CLUSTER）。为空则不带前缀。
	Cluster string
	// Namespaces 是 List podgroups 的 namespace 列表。空 = 全部 namespace（需要集群级
	// list 权限，RBAC 已经给了）；元素为 `*` 也表示全部。
	Namespaces []string
	// TTL 是缓存新鲜度上限，默认 15s。
	TTL time.Duration
	// Timeout 是单次 List 的超时，默认 5s。
	Timeout time.Duration
	// Backoff 是连续失败后的退避窗口，默认 60s。
	Backoff time.Duration
	// Now 是时钟（测试注入用），默认 time.Now。
	Now func() time.Time
	// Logger 可为空；降级时会在此打一条可见的日志（每轮连续失败只打第一条）。
	Logger *slog.Logger
}

func (o VolcanoOptions) withDefaults() VolcanoOptions {
	if o.TTL <= 0 {
		o.TTL = DefaultVolcanoTTL
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultVolcanoTimeout
	}
	if o.Backoff <= 0 {
		o.Backoff = DefaultVolcanoBackoff
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Volcano 是 VolcanoProvider 的真实实现（只读快照 + TTL 缓存 + 单飞）。
//
// 并发模型：`mu` 保护缓存与在飞标记；同一时刻最多一个 goroutine 在打 API
// （`/queue-status` 是 30s 轮询的公共端点，看板一开就是多客户端同时打）。
type Volcano struct {
	dyn dynamic.Interface
	opt VolcanoOptions

	mu         sync.Mutex
	snap       VolcanoSnapshot // 最近一次刷新的结果（成功或降级）
	at         time.Time       // 最近一次刷新的时刻
	loaded     bool            // 是否已经刷过一次（区分"从没数据"与"上次数据")
	inflight   chan struct{}   // 非 nil 表示有刷新在飞；关闭即完成
	retryAfter time.Time       // 失败退避窗口：此刻之前不再刷新
	failStreak int             // 连续失败次数（只为"每轮只打一条日志"）
}

var _ VolcanoProvider = (*Volcano)(nil)

// NewVolcano 按与 executor 相同的凭据链（in-cluster / kubeconfig，见 client.go）
// 构造 dynamic client。配错了立刻报错，由调用方决定降级还是退出。
func NewVolcano(o Options, vo VolcanoOptions) (*Volcano, error) {
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
	return NewVolcanoFromConfig(cfg, vo)
}

// NewVolcanoFromConfig 用现成的 rest.Config 构造（多集群时每个集群一份）。
func NewVolcanoFromConfig(cfg *rest.Config, vo VolcanoOptions) (*Volcano, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: volcano dynamic client: %w", err)
	}
	return NewVolcanoWithDynamic(dyn, vo), nil
}

// NewVolcanoWithDynamic 注入现成的 dynamic client（单测用 fake，多集群可复用连接）。
// dyn 为 nil 时不会 panic：所有刷新都降级为 scheduler_unavailable。
func NewVolcanoWithDynamic(dyn dynamic.Interface, vo VolcanoOptions) *Volcano {
	vo = vo.withDefaults()
	if vo.Logger == nil {
		vo.Logger = slog.Default()
	}
	return &Volcano{dyn: dyn, opt: vo}
}

// VolcanoSnapshot 实现 VolcanoProvider：命中 TTL 直接回缓存，未命中走单飞刷新，
// 刷新失败只降级本块（永远返回，不阻塞、不 panic、不返回 0 排队）。
func (v *Volcano) VolcanoSnapshot(ctx context.Context) VolcanoSnapshot {
	if v == nil {
		return volcanoUnavailable(VolcanoSchedulerUnavailable, false)
	}
	// 配置关闭：不打 API，直接四态里的 disabled（"平台未启用队列调度"）。
	if !v.opt.Enabled {
		return volcanoUnavailable(VolcanoDisabled, false)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	v.mu.Lock()
	// 1) TTL 命中：直接回缓存（失败快照也在 TTL 内复用，避免风暴）。
	if v.loaded && v.opt.Now().Sub(v.at) < v.opt.TTL {
		snap := v.snap
		v.mu.Unlock()
		return snap
	}
	// 2) 有刷新在飞：等它（单飞），不重复打 API。
	if v.inflight != nil {
		done := v.inflight
		v.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		v.mu.Lock()
		snap := v.currentLocked()
		v.mu.Unlock()
		return snap
	}
	// 3) 失败退避窗口内：不再打 API，回上次结果（含降级快照）。
	if !v.retryAfter.IsZero() && v.opt.Now().Before(v.retryAfter) {
		snap := v.currentLocked()
		v.mu.Unlock()
		return snap
	}
	done := make(chan struct{})
	v.inflight = done
	v.mu.Unlock()

	snap, err := v.refresh(ctx)

	v.mu.Lock()
	v.inflight = nil
	if err != nil && v.loaded && v.snap.Available {
		// 上次是好的、这次读不到：标 stale，让 UI 能说"上次更新 HH:MM"。
		snap.Stale = true
	}
	v.snap = snap
	v.at = v.opt.Now()
	v.loaded = true
	if err != nil {
		v.retryAfter = v.at.Add(v.opt.Backoff)
		v.failStreak++
		if v.failStreak == 1 {
			v.opt.Logger.Warn("Volcano 队列快照降级",
				"detail", detailOf(snap), "err", err, "retry_after", v.opt.Backoff.String())
		}
	} else {
		v.retryAfter = time.Time{}
		v.failStreak = 0
	}
	close(done)
	v.mu.Unlock()
	return snap
}

// currentLocked 返回当前（可能过期的）快照；从没刷新成功过就报 scheduler_unavailable。
// 调用方必须持有 v.mu。
func (v *Volcano) currentLocked() VolcanoSnapshot {
	if v.loaded {
		return v.snap
	}
	return volcanoUnavailable(VolcanoSchedulerUnavailable, false)
}

// refresh 打一次 API：先 queues（小、稳定），再 podgroups（大）；任一失败即整体降级
// （openapi：不可达时 queues 为 `[]`，不做"半可用"）。
func (v *Volcano) refresh(parent context.Context) (VolcanoSnapshot, error) {
	ctx, cancel := context.WithTimeout(parent, v.opt.Timeout)
	defer cancel()

	if v.dyn == nil {
		return volcanoUnavailable(VolcanoSchedulerUnavailable, false),
			fmt.Errorf("k8s: volcano dynamic client 未配置")
	}

	queueList, err := v.dyn.Resource(VolcanoQueueGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return volcanoUnavailable(classifyVolcanoError(err), false), fmt.Errorf("list queues: %w", err)
	}
	groups, err := v.listPodGroups(ctx)
	if err != nil {
		return volcanoUnavailable(classifyVolcanoError(err), false), fmt.Errorf("list podgroups: %w", err)
	}

	return VolcanoSnapshot{
		Available: true,
		Queues:    v.buildQueues(queueList, groups, v.opt.Now()),
	}, nil
}

// listPodGroups 按配置的 namespace 列表逐个 List 后合并（空 = 全部 namespace 一次 List）。
func (v *Volcano) listPodGroups(ctx context.Context) ([]podGroup, error) {
	namespaces := v.podGroupNamespaces()
	if len(namespaces) == 0 {
		// 全部 namespace：一次 List，靠 ClusterRole 的集群级 list 权限。
		list, err := v.dyn.Resource(VolcanoPodGroupGVR).Namespace("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return parsePodGroups(list), nil
	}
	out := make([]podGroup, 0, 32)
	for _, ns := range namespaces {
		list, err := v.dyn.Resource(VolcanoPodGroupGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		out = append(out, parsePodGroups(list)...)
	}
	return out, nil
}

// podGroupNamespaces 把配置里的 namespace 归一：空 / `*` → 全部（返回 nil）。
func (v *Volcano) podGroupNamespaces() []string {
	seen := make(map[string]struct{}, len(v.opt.Namespaces))
	out := make([]string, 0, len(v.opt.Namespaces))
	for _, ns := range v.opt.Namespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" || ns == "*" {
			return nil
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}

// podGroup 是聚合用的 PodGroup 投影（只留排队判定要的字段）。
type podGroup struct {
	queue   string
	phase   string
	reason  string // conditions[-1].message ?: conditions[-1].reason（原文，不翻译）
	created time.Time
	hasTime bool
}

// buildQueues 把 Queue List + PodGroup 投影拼成 DTO 列表（字段映射见 C-4.8.1）。
func (v *Volcano) buildQueues(list *unstructured.UnstructuredList, groups []podGroup, now time.Time) []VolcanoQueue {
	byQueue := make(map[string][]podGroup, len(groups))
	for _, g := range groups {
		byQueue[g.queue] = append(byQueue[g.queue], g)
	}

	out := make([]VolcanoQueue, 0, len(list.Items))
	for i := range list.Items {
		u := &list.Items[i]
		name := u.GetName()
		q := VolcanoQueue{
			Name:       v.queueName(name),
			State:      nestedStringPtr(u, "status", "state"),
			Capability: nestedStringMap(u, "spec", "capability"),
			Allocated:  nestedStringMap(u, "status", "allocated"),
		}
		pending, wait, reason := summarize(byQueue[name], now)
		q.PendingJobs = pending
		q.LongestWaitSeconds = wait
		if reason != "" {
			r := reason
			q.Reason = &r
		}
		out = append(out, q)
	}
	// 顺序稳定（apiserver 一般按 name 回，但不保证；JSON 快照要可 diff）。
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// summarize 按 C-4.8.1 算一个队列的三个派生字段：
//   - pending_jobs：该 queue 下 `phase=Pending` 的 PodGroup 计数（**不用** queue.status.pending）
//   - longest_wait_seconds：`now - min(creationTimestamp)`，限定 `phase ∈ {Pending,Inqueue}`
//   - reason：同范围里等得最久的那条 PodGroup 的 conditions[-1].message ?: .reason（原文）
func summarize(groups []podGroup, now time.Time) (pending, waitSeconds int, reason string) {
	var (
		earliest    time.Time
		hasEarliest bool
		oldestWith  *podGroup // 等得最久且带 condition 原文的那条（不排序，就地挑）
	)
	for i := range groups {
		g := &groups[i]
		switch g.phase {
		case "Pending":
			pending++
		case "Inqueue":
			// Inqueue 参与等待时长与原因，但不算 pending_jobs（口径见 C-4.8.1）。
		default:
			continue
		}
		if g.hasTime && (!hasEarliest || g.created.Before(earliest)) {
			earliest = g.created
			hasEarliest = true
		}
		if g.reason == "" {
			continue
		}
		if oldestWith == nil {
			oldestWith = g
			continue
		}
		// 有时间的比没时间的更可靠；都有时间取更早的那条。
		switch {
		case g.hasTime && !oldestWith.hasTime:
			oldestWith = g
		case g.hasTime && oldestWith.hasTime && g.created.Before(oldestWith.created):
			oldestWith = g
		}
	}
	if hasEarliest {
		if d := now.Sub(earliest); d > 0 {
			waitSeconds = int(d / time.Second)
		}
	}
	if oldestWith != nil {
		reason = oldestWith.reason
	}
	return pending, waitSeconds, reason
}

// queueName 给队列名加集群前缀（`"<cluster>/<queue>"`，多集群零契约变更；P0 单集群也带，
// 这样 UI/契约在切多集群时不用改）。
func (v *Volcano) queueName(name string) string {
	cluster := strings.TrimSpace(v.opt.Cluster)
	if cluster == "" {
		return name
	}
	return cluster + "/" + name
}

// parsePodGroups 把 PodGroup List 投影成聚合用的结构（缺字段不是错误）。
func parsePodGroups(list *unstructured.UnstructuredList) []podGroup {
	out := make([]podGroup, 0, len(list.Items))
	for i := range list.Items {
		u := &list.Items[i]
		g := podGroup{
			phase:  nestedString(u, "status", "phase"),
			reason: podGroupReason(u),
			queue:  nestedString(u, "spec", "queue"),
		}
		if g.queue == "" {
			g.queue = defaultVolcanoQueue
		}
		if ts, ok := objectCreationTime(u); ok {
			g.created, g.hasTime = ts, true
		}
		out = append(out, g)
	}
	return out
}

// podGroupReason 取 `status.conditions[-1]` 的 message，空则退到 reason（Jenkins 的取法，
// Helm-Start-Service-MB.jenkinsfile:617）。**原文透传**：归一成 waiting_reason 需要
// Task→Job→Pod→PodGroup 关联链路，属 C-4.8.4 的 MVP 边界之外。
func podGroupReason(u *unstructured.Unstructured) string {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found || len(conds) == 0 {
		return ""
	}
	last, ok := conds[len(conds)-1].(map[string]any)
	if !ok {
		return ""
	}
	if msg, ok, _ := unstructured.NestedString(last, "message"); ok && strings.TrimSpace(msg) != "" {
		return msg
	}
	if reason, ok, _ := unstructured.NestedString(last, "reason"); ok {
		return reason
	}
	return ""
}

// classifyVolcanoError 把 List 错误收敛到降级四态（A 类错误绝不泄漏原文，避免泄露
// SA / 角色名；原始 err 只进日志）。
func classifyVolcanoError(err error) string {
	switch {
	case err == nil:
		return VolcanoSchedulerUnavailable
	case apierrors.IsForbidden(err):
		return VolcanoRBACForbidden
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		// CRD 没装：apiserver 对未知 group/version 回 404，discovery 缺失回 NoMatch。
		return VolcanoCRDMissing
	default:
		// 超时 / 连接失败 / 其余 —— 调度器不可达，不是"没有排队"。
		return VolcanoSchedulerUnavailable
	}
}

// volcanoUnavailable 是唯一的降级出口：available=false + 空 queues（红线：不可达 ≠ 0 排队）。
func volcanoUnavailable(detail string, stale bool) VolcanoSnapshot {
	d := detail
	return VolcanoSnapshot{
		Available: false,
		Detail:    &d,
		Queues:    []VolcanoQueue{},
		Stale:     stale,
	}
}

func detailOf(snap VolcanoSnapshot) string {
	if snap.Detail == nil {
		return ""
	}
	return *snap.Detail
}

func nestedString(u *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, fields...)
	return v
}

func nestedStringPtr(u *unstructured.Unstructured, fields ...string) *string {
	v, found, _ := unstructured.NestedString(u.Object, fields...)
	if !found || strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

// nestedStringMap 把 `map[string]any` 的资源量映射成 `map[string]string`：
// **原文透传，不做单位换算**（openapi：`spec.capability` 原文）。
func nestedStringMap(u *unstructured.Unstructured, fields ...string) map[string]string {
	raw, found, err := unstructured.NestedMap(u.Object, fields...)
	if err != nil || !found {
		return map[string]string{}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case nil:
			out[k] = ""
		case string:
			out[k] = t
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
}

// objectCreationTime 从 unstructured 的 `metadata.creationTimestamp` 取时间
// （apiserver 回的是 RFC3339 字符串；解析不了就当没有，不猜）。
func objectCreationTime(u *unstructured.Unstructured) (time.Time, bool) {
	raw, found, err := unstructured.NestedString(u.Object, "metadata", "creationTimestamp")
	if err != nil || !found || raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
