package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ucm-system/testplane/server/internal/api"
)

// ---- 测试脚手架 ----------------------------------------------------------

// fakeVolcanoClient 造一个能 List Volcano 两个资源的假 dynamic client。
// listKinds 必须显式给：fake 的 List 对未登记的资源会 panic（client-go v0.31）。
func fakeVolcanoClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{
		VolcanoQueueGVR:    "QueueList",
		VolcanoPodGroupGVR: "PodGroupList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
}

// queueObject 造一个 Queue CR（capability 在 spec、allocated 在 status）。
func queueObject(name, state string, capability, allocated map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": VolcanoAPIGroup + "/" + VolcanoAPIVersion,
		"kind":       "Queue",
		"metadata":   map[string]any{"name": name},
	}
	if len(capability) > 0 {
		obj["spec"] = map[string]any{"capability": capability}
	}
	status := map[string]any{}
	if state != "" {
		status["state"] = state
	}
	if len(allocated) > 0 {
		status["allocated"] = allocated
	}
	if len(status) > 0 {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

// podGroupObject 造一个 PodGroup CR。queue 为空时不写 spec.queue（Volcano 会归到 default）。
func podGroupObject(name, ns, queue, phase string, created time.Time, message, reason string) *unstructured.Unstructured {
	metaObj := map[string]any{"name": name, "namespace": ns}
	if !created.IsZero() {
		metaObj["creationTimestamp"] = created.UTC().Format(time.RFC3339)
	}
	obj := map[string]any{
		"apiVersion": VolcanoAPIGroup + "/" + VolcanoAPIVersion,
		"kind":       "PodGroup",
		"metadata":   metaObj,
		"status":     map[string]any{"phase": phase},
	}
	if queue != "" {
		obj["spec"] = map[string]any{"queue": queue}
	}
	if message != "" || reason != "" {
		cond := map[string]any{"type": "Unschedulable", "status": "True"}
		if message != "" {
			cond["message"] = message
		}
		if reason != "" {
			cond["reason"] = reason
		}
		obj["status"].(map[string]any)["conditions"] = []any{cond}
	}
	return &unstructured.Unstructured{Object: obj}
}

// listCalls 统计 List 次数（TTL / 单飞断言用）。
type listCalls struct {
	mu        sync.Mutex
	queues    int
	podgroups int
}

func (c *listCalls) set() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queues, c.podgroups
}

func countListCalls(dyn *dynamicfake.FakeDynamicClient) *listCalls {
	c := &listCalls{}
	dyn.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch action.GetResource().Resource {
		case "queues":
			c.queues++
		case "podgroups":
			c.podgroups++
		}
		return false, nil, nil
	})
	return c
}

// failListOn 让指定资源的 List 直接返回 err（降级路径注入）。
func failListOn(dyn *dynamicfake.FakeDynamicClient, resource string, err error) {
	dyn.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

// ---- 正常路径 ------------------------------------------------------------

// TestVolcano_TwoQueues 覆盖 C-4.8.1 的全部字段映射：state / capability / allocated /
// pending_jobs（只数 phase=Pending）/ longest_wait_seconds（pending∪inqueue 里最早的那个）/
// reason（conditions[-1].message，空则退到 reason）/ 名字带 `<cluster>/` 前缀。
func TestVolcano_TwoQueues(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dyn := fakeVolcanoClient(
		queueObject("queue-a", "Open",
			map[string]any{"nvidia.com/gpu": "8", "cpu": "64"},
			map[string]any{"nvidia.com/gpu": "3"}),
		queueObject("queue-b", "Closed", nil, nil),
		podGroupObject("pg-a1", "testplane", "queue-a", "Pending", now.Add(-540*time.Second),
			"0/3 nodes are available: insufficient nvidia.com/gpu", ""),
		podGroupObject("pg-a2", "testplane", "queue-a", "Pending", now.Add(-100*time.Second),
			"", "Gated by gang"),
		podGroupObject("pg-a3", "testplane", "queue-a", "Inqueue", now.Add(-300*time.Second),
			"inqueue by queue-a", ""),
		podGroupObject("pg-b1", "testplane", "queue-b", "Pending", now.Add(-30*time.Second),
			"queue queue-b is not open", ""),
		podGroupObject("pg-b2", "testplane", "queue-b", "Running", now.Add(-1000*time.Second),
			"running", ""),
	)
	calls := countListCalls(dyn)

	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled:    true,
		Cluster:    "cluster-01",
		Namespaces: []string{"testplane"},
		Now:        func() time.Time { return now },
	})

	snap := p.VolcanoSnapshot(context.Background())
	if !snap.Available {
		t.Fatalf("available = false, detail=%v", detailOf(snap))
	}
	if snap.Detail != nil {
		t.Fatalf("available=true 时 detail 必须为 nil，得到 %q", *snap.Detail)
	}
	if len(snap.Queues) != 2 {
		t.Fatalf("queues = %d 条，期望 2: %+v", len(snap.Queues), snap.Queues)
	}
	if q, pg := calls.set(); q != 1 || pg != 1 {
		t.Fatalf("List 次数 queues=%d podgroups=%d，期望各 1 次", q, pg)
	}

	a, b := snap.Queues[0], snap.Queues[1]
	if a.Name != "cluster-01/queue-a" || b.Name != "cluster-01/queue-b" {
		t.Fatalf("队列名未按 <cluster>/<queue> 生成: %q %q", a.Name, b.Name)
	}

	// queue-a：2 条 Pending（Inqueue 不算 pending_jobs）+ 最早在 540s 前。
	if a.State == nil || *a.State != "Open" {
		t.Fatalf("queue-a.state = %v", a.State)
	}
	if a.Capability["nvidia.com/gpu"] != "8" || a.Capability["cpu"] != "64" {
		t.Fatalf("queue-a.capability = %+v", a.Capability)
	}
	if a.Allocated["nvidia.com/gpu"] != "3" {
		t.Fatalf("queue-a.allocated = %+v", a.Allocated)
	}
	if a.PendingJobs != 2 {
		t.Fatalf("queue-a.pending_jobs = %d，期望 2（只数 phase=Pending）", a.PendingJobs)
	}
	if a.LongestWaitSeconds != 540 {
		t.Fatalf("queue-a.longest_wait_seconds = %d，期望 540（取最早的那条）", a.LongestWaitSeconds)
	}
	if a.Reason == nil || *a.Reason != "0/3 nodes are available: insufficient nvidia.com/gpu" {
		t.Fatalf("queue-a.reason = %v（期望最早那条的 condition 原文）", a.Reason)
	}

	// queue-b：1 条 Pending，等 30s；capability/allocated 缺字段时是空 map，不是 nil。
	if b.State == nil || *b.State != "Closed" {
		t.Fatalf("queue-b.state = %v", b.State)
	}
	if b.PendingJobs != 1 || b.LongestWaitSeconds != 30 {
		t.Fatalf("queue-b pending=%d wait=%d，期望 1/30", b.PendingJobs, b.LongestWaitSeconds)
	}
	if b.Reason == nil || *b.Reason != "queue queue-b is not open" {
		t.Fatalf("queue-b.reason = %v", b.Reason)
	}
	if b.Capability == nil || len(b.Capability) != 0 {
		t.Fatalf("queue-b.capability 应为空 map，得到 %+v", b.Capability)
	}
	if b.Allocated == nil || len(b.Allocated) != 0 {
		t.Fatalf("queue-b.allocated 应为空 map，得到 %+v", b.Allocated)
	}
}

// TestVolcano_NamePrefix 单集群也能带 cluster 前缀；没配 cluster 就保持原名（零契约变更）。
func TestVolcano_NamePrefix(t *testing.T) {
	now := time.Now()
	dyn := fakeVolcanoClient(queueObject("default", "Open", nil, nil))

	withCluster := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Cluster: "cluster-01", Now: func() time.Time { return now },
	}).VolcanoSnapshot(context.Background())
	if len(withCluster.Queues) != 1 || withCluster.Queues[0].Name != "cluster-01/default" {
		t.Fatalf("带前缀的队列名异常: %+v", withCluster.Queues)
	}

	noCluster := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Now: func() time.Time { return now },
	}).VolcanoSnapshot(context.Background())
	if len(noCluster.Queues) != 1 || noCluster.Queues[0].Name != "default" {
		t.Fatalf("不带前缀的队列名异常: %+v", noCluster.Queues)
	}
}

// TestVolcano_LongestWaitTakesEarliest 明确构造 2 条 PodGroup：取创建最早的那条，
// 且 phase 不在 {Pending,Inqueue} 的（Running/Completed）不参与。
func TestVolcano_LongestWaitTakesEarliest(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dyn := fakeVolcanoClient(
		queueObject("q", "Open", nil, nil),
		podGroupObject("pg-running", "testplane", "q", "Running", now.Add(-9000*time.Second), "running", ""),
		podGroupObject("pg-young", "testplane", "q", "Pending", now.Add(-60*time.Second), "young", ""),
		podGroupObject("pg-old", "testplane", "q", "Inqueue", now.Add(-600*time.Second), "", ""),
	)
	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Namespaces: []string{"testplane"}, Now: func() time.Time { return now },
	})
	snap := p.VolcanoSnapshot(context.Background())
	if !snap.Available || len(snap.Queues) != 1 {
		t.Fatalf("快照异常: %+v", snap)
	}
	q := snap.Queues[0]
	if q.PendingJobs != 1 {
		t.Fatalf("pending_jobs = %d，期望 1（只有 phase=Pending 的那条）", q.PendingJobs)
	}
	if q.LongestWaitSeconds != 600 {
		t.Fatalf("longest_wait_seconds = %d，期望 600（Inqueue 那条最早，Running 不算）", q.LongestWaitSeconds)
	}
	// 等得最久的那条没有 condition，退到第二条有原文的。
	if q.Reason == nil || *q.Reason != "young" {
		t.Fatalf("reason = %v，期望 young", q.Reason)
	}
}

// TestVolcano_QueueWithoutPodGroups 队列存在但没有 PodGroup：可用、计数 0、等待 0、原因 null。
// UI 显示 `—`，不是"0 个排队"的绝对断言。
func TestVolcano_QueueWithoutPodGroups(t *testing.T) {
	dyn := fakeVolcanoClient(queueObject("q", "Open", nil, nil))
	snap := NewVolcanoWithDynamic(dyn, VolcanoOptions{Enabled: true}).VolcanoSnapshot(context.Background())
	if !snap.Available || len(snap.Queues) != 1 {
		t.Fatalf("快照异常: %+v", snap)
	}
	q := snap.Queues[0]
	if q.PendingJobs != 0 || q.LongestWaitSeconds != 0 || q.Reason != nil {
		t.Fatalf("空队列字段异常: %+v", q)
	}
}

// TestVolcano_NamespacesListedPerNamespace 配置了命名空间就逐个 List（不跨 ns 串数据）。
func TestVolcano_NamespacesListedPerNamespace(t *testing.T) {
	now := time.Now()
	dyn := fakeVolcanoClient(
		queueObject("q", "Open", nil, nil),
		podGroupObject("pg-a", "testplane", "q", "Pending", now.Add(-10*time.Second), "a", ""),
		podGroupObject("pg-b", "tp-qa", "q", "Pending", now.Add(-10*time.Second), "b", ""),
		podGroupObject("pg-c", "tp-other", "q", "Pending", now.Add(-10*time.Second), "c", ""),
	)
	calls := countListCalls(dyn)
	snap := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled:    true,
		Namespaces: []string{"testplane", "tp-qa", "testplane"},
		Now:        func() time.Time { return now },
	}).VolcanoSnapshot(context.Background())
	if _, pg := calls.set(); pg != 2 {
		t.Fatalf("podgroups List 次数 = %d，期望 2（重复的 ns 去重）", pg)
	}
	if len(snap.Queues) != 1 || snap.Queues[0].PendingJobs != 2 {
		t.Fatalf("只应统计 testplane + tp-qa 的 2 条，得到 %+v", snap.Queues)
	}
}

// TestVolcano_DefaultQueueWhenSpecQueueMissing PodGroup 没写 spec.queue 时归到 `default`，
// 否则会漏计（Volcano 的默认队列名就是 default）。
func TestVolcano_DefaultQueueWhenSpecQueueMissing(t *testing.T) {
	now := time.Now()
	dyn := fakeVolcanoClient(
		queueObject("default", "Open", nil, nil),
		podGroupObject("pg", "testplane", "", "Pending", now.Add(-10*time.Second), "boom", ""),
	)
	snap := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Namespaces: []string{"testplane"}, Now: func() time.Time { return now },
	}).VolcanoSnapshot(context.Background())
	if len(snap.Queues) != 1 || snap.Queues[0].PendingJobs != 1 {
		t.Fatalf("未归到 default 队列: %+v", snap.Queues)
	}
}

// ---- 降级四态（红线：不可达 ≠ 0 排队） -----------------------------------

// TestVolcano_DegradeFourStates 四态各自命中，且一律 available=false + 空 queues。
func TestVolcano_DegradeFourStates(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: VolcanoAPIGroup, Resource: "queues"}, "queues", errors.New("no"))
	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: VolcanoAPIGroup, Resource: "queues"}, "queues")
	noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: VolcanoAPIGroup, Kind: "Queue"}}
	timeout := fmt.Errorf("list queues: %w", context.DeadlineExceeded)
	connRefused := &url.Error{Op: "Get", URL: "https://kubernetes.default.svc", Err: errors.New("connection refused")}

	cases := []struct {
		name    string
		enabled bool
		err     error
		want    string
	}{
		{"crd_missing_notfound", true, notFound, api.VolcanoCRDMissing},
		{"crd_missing_nomatch", true, noMatch, api.VolcanoCRDMissing},
		{"rbac_forbidden", true, forbidden, api.VolcanoRBACForbidden},
		{"timeout", true, timeout, api.VolcanoSchedulerUnavailable},
		{"connection_refused", true, connRefused, api.VolcanoSchedulerUnavailable},
		{"disabled", false, nil, api.VolcanoDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fakeVolcanoClient(queueObject("default", "Open", nil, nil))
			if tc.err != nil {
				failListOn(dyn, "queues", tc.err)
			}
			p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
				Enabled: tc.enabled, Namespaces: []string{"testplane"},
			})
			snap := p.VolcanoSnapshot(context.Background())
			if snap.Available {
				t.Fatalf("available = true，降级态必须为 false")
			}
			if snap.Detail == nil || *snap.Detail != tc.want {
				t.Fatalf("detail = %v，期望 %s", snap.Detail, tc.want)
			}
			if snap.Queues == nil || len(snap.Queues) != 0 {
				t.Fatalf("降级时 queues 必须是空数组（不可达 ≠ 0 排队），得到 %+v", snap.Queues)
			}
		})
	}
}

// TestVolcano_PodGroupListFailureDegrades podgroups 读不到时整体降级（openapi 不认"半可用"）。
func TestVolcano_PodGroupListFailureDegrades(t *testing.T) {
	dyn := fakeVolcanoClient(queueObject("default", "Open", nil, nil))
	failListOn(dyn, "podgroups", apierrors.NewForbidden(
		schema.GroupResource{Group: VolcanoAPIGroup, Resource: "podgroups"}, "podgroups", errors.New("no")))
	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{Enabled: true, Namespaces: []string{"testplane"}})
	snap := p.VolcanoSnapshot(context.Background())
	if snap.Available || snap.Detail == nil || *snap.Detail != api.VolcanoRBACForbidden {
		t.Fatalf("podgroups 无权限应降级 rbac_forbidden，得到 %+v", snap)
	}
	if len(snap.Queues) != 0 {
		t.Fatalf("降级时 queues 必须为空，得到 %+v", snap.Queues)
	}
}

// TestVolcano_NilClientDegrades 没配 dynamic client 也不能 panic，只降级。
func TestVolcano_NilClientDegrades(t *testing.T) {
	snap := NewVolcanoWithDynamic(nil, VolcanoOptions{Enabled: true}).
		VolcanoSnapshot(context.Background())
	if snap.Available || snap.Detail == nil || *snap.Detail != api.VolcanoSchedulerUnavailable {
		t.Fatalf("nil client 应降级 scheduler_unavailable，得到 %+v", snap)
	}
}

// TestVolcano_StaleAfterSuccess 上一次成功、这次超时：标 stale + 空 queues（不冒充旧数据）。
func TestVolcano_StaleAfterSuccess(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var (
		mu      sync.Mutex
		failErr error
	)
	dyn := fakeVolcanoClient(queueObject("q", "Open", nil, nil))
	dyn.PrependReactor("list", "queues", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if failErr != nil {
			return true, nil, failErr
		}
		return false, nil, nil
	})
	clock := now
	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Namespaces: []string{"testplane"},
		TTL: time.Second, Backoff: time.Minute,
		Now: func() time.Time { return clock },
	})

	if snap := p.VolcanoSnapshot(context.Background()); !snap.Available || snap.Stale {
		t.Fatalf("第一次应可用且非 stale: %+v", snap)
	}

	mu.Lock()
	failErr = fmt.Errorf("list queues: %w", context.DeadlineExceeded)
	mu.Unlock()
	clock = now.Add(2 * time.Second) // 越过 TTL，强制刷新

	snap := p.VolcanoSnapshot(context.Background())
	if snap.Available {
		t.Fatalf("超时后 available 必须为 false")
	}
	if !snap.Stale {
		t.Fatalf("上一次成功过，这次失败应标 stale")
	}
	if snap.Detail == nil || *snap.Detail != api.VolcanoSchedulerUnavailable {
		t.Fatalf("detail = %v，期望 scheduler_unavailable", snap.Detail)
	}
	if len(snap.Queues) != 0 {
		t.Fatalf("降级时不得回放旧队列数据，得到 %+v", snap.Queues)
	}
}

// ---- 采样：TTL 缓存 + 单飞 + 失败退避 -----------------------------------

// TestVolcano_TTLCachesSnapshot TTL 内第二次不调 List；过期后重新 List。
func TestVolcano_TTLCachesSnapshot(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := base
	dyn := fakeVolcanoClient(
		queueObject("q", "Open", nil, nil),
		podGroupObject("pg", "testplane", "q", "Pending", base.Add(-5*time.Second), "wait", ""),
	)
	calls := countListCalls(dyn)
	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Namespaces: []string{"testplane"},
		TTL: 15 * time.Second, Now: func() time.Time { return clock },
	})

	first := p.VolcanoSnapshot(context.Background())
	second := p.VolcanoSnapshot(context.Background())
	if q, pg := calls.set(); q != 1 || pg != 1 {
		t.Fatalf("TTL 内应只 List 一次，得到 queues=%d podgroups=%d", q, pg)
	}
	if !second.Available || len(second.Queues) != 1 || second.Queues[0].Name != first.Queues[0].Name {
		t.Fatalf("命中缓存时快照应一致: %+v vs %+v", first, second)
	}

	clock = base.Add(16 * time.Second) // 越过 15s TTL
	_ = p.VolcanoSnapshot(context.Background())
	if q, _ := calls.set(); q != 2 {
		t.Fatalf("TTL 过期后应重新 List，queues List 次数 = %d，期望 2", q)
	}
}

// TestVolcano_SingleFlight 并发 10 个请求只打一次 List（单飞）。
func TestVolcano_SingleFlight(t *testing.T) {
	dyn := fakeVolcanoClient(
		queueObject("q", "Open", nil, nil),
		podGroupObject("pg", "testplane", "q", "Pending", time.Now().Add(-5*time.Second), "wait", ""),
	)
	calls := countListCalls(dyn)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	dyn.PrependReactor("list", "queues", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // 卡住第一次刷新，让其它 goroutine 全部挤到单飞等待
		return false, nil, nil
	})

	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Namespaces: []string{"testplane"},
	})

	const n = 10
	results := make([]api.VolcanoSnapshot, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = p.VolcanoSnapshot(context.Background())
		}(i)
	}

	<-started
	time.Sleep(100 * time.Millisecond) // 其余 9 个应当已进入 inflight 等待
	close(release)
	wg.Wait()

	if q, pg := calls.set(); q != 1 || pg != 1 {
		t.Fatalf("并发 %d 个请求应只 List 一次，得到 queues=%d podgroups=%d", n, q, pg)
	}
	for i, snap := range results {
		if !snap.Available || len(snap.Queues) != 1 {
			t.Fatalf("第 %d 个结果异常: %+v", i, snap)
		}
	}
}

// TestVolcano_BackoffAfterFailure 连续失败后退避窗口内不再打 API（但每次都要给出降级快照）。
func TestVolcano_BackoffAfterFailure(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := base
	dyn := fakeVolcanoClient(queueObject("q", "Open", nil, nil))
	failListOn(dyn, "queues", apierrors.NewNotFound(
		schema.GroupResource{Group: VolcanoAPIGroup, Resource: "queues"}, "queues"))
	calls := countListCalls(dyn)
	p := NewVolcanoWithDynamic(dyn, VolcanoOptions{
		Enabled: true, Namespaces: []string{"testplane"},
		TTL: 15 * time.Second, Backoff: 60 * time.Second,
		Now: func() time.Time { return clock },
	})

	if snap := p.VolcanoSnapshot(context.Background()); snap.Detail == nil || *snap.Detail != api.VolcanoCRDMissing {
		t.Fatalf("首次应 crd_missing，得到 %+v", snap)
	}
	// 越过 15s TTL 但在 60s 退避窗口内：不打 API。
	clock = base.Add(30 * time.Second)
	if snap := p.VolcanoSnapshot(context.Background()); snap.Available || snap.Detail == nil {
		t.Fatalf("退避窗口内仍要返回降级快照，得到 %+v", snap)
	}
	if q, _ := calls.set(); q != 1 {
		t.Fatalf("退避窗口内不得重新 List，queues List 次数 = %d", q)
	}
	// 越过退避窗口：允许重试一次。
	clock = base.Add(61 * time.Second)
	_ = p.VolcanoSnapshot(context.Background())
	if q, _ := calls.set(); q != 2 {
		t.Fatalf("退避结束后应重试，queues List 次数 = %d，期望 2", q)
	}
}
