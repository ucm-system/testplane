package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// fakePodLogs 是 ports.PodLogSource 的测试替身：
// 可控 Pod 列表、可按 (pod, container, previous) 返回固定日志、记录调用次数。
type fakePodLogs struct {
	mu       sync.Mutex
	pods     []ports.PodInfo
	logs     map[string]string // key: pod|container|prev
	listCall int
	calls    []string
}

func (f *fakePodLogs) ListPods(ctx context.Context, namespace string) ([]ports.PodInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCall++
	return f.pods, nil
}

func (f *fakePodLogs) StreamLogs(ctx context.Context, namespace, pod, container string, opts ports.LogStreamOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	key := pod + "|" + container
	if opts.Previous {
		key += "|prev"
	}
	f.calls = append(f.calls, key)
	content := f.logs[key]
	f.mu.Unlock()
	if content == "" {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func newDeployCollector(t *testing.T, root string, src ports.PodLogSource) *DeployLogCollector {
	t.Helper()
	return &DeployLogCollector{
		RunID:       "r-1",
		Namespace:   "testplane-mock",
		ReleaseName: "tp-rel",
		Root:        root,
		Src:         src,
		Interval:    time.Hour, // 测试里手动调 discover，避免 ticker 干扰
	}
}

// ① 流式返回 → deploy/logs/<pod>.log 内容正确（**时间戳已剥**），pods.json 里出现该 Pod。
func TestDeployLogCollector_WritesLogAndPodsJSON(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{
		pods: []ports.PodInfo{{
			Name: "tp-rel-stack-1", Phase: "Running", Ready: true,
			Containers: []ports.ContainerInfo{{Name: "server"}},
		}},
		logs: map[string]string{
			"tp-rel-stack-1|server": "2026-11-06T01:02:03.123456789Z hello\n2026-11-06T01:02:04Z world\n",
		},
	}
	c := newDeployCollector(t, root, src)
	c.discover(context.Background())
	c.Wait()

	got := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if strings.Contains(got, "2026-11-06T01:02:03") {
		t.Errorf("时间戳没剥掉: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("日志内容不全: %q", got)
	}

	doc, ok := readPodsJSONForTest(t, root)
	if !ok {
		t.Fatal("pods.json 未生成")
	}
	if len(doc.Pods) != 1 || doc.Pods[0].Name != "tp-rel-stack-1" {
		t.Fatalf("pods.json = %+v", doc.Pods)
	}
	if doc.Pods[0].Phase != "Running" || !doc.Pods[0].Ready {
		t.Errorf("pods.json 拓扑不对: %+v", doc.Pods[0])
	}
}

// ② restartCount 0→1：先补 previous 段，再写重启标记 —— **顺序正确**（previous 在前）。
func TestDeployLogCollector_RestartWritesPreviousThenMarker(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{
		pods: []ports.PodInfo{{
			Name: "tp-rel-stack-1", Phase: "Running", Ready: true,
			Containers: []ports.ContainerInfo{{Name: "server", Restarts: 0}},
		}},
		logs: map[string]string{
			"tp-rel-stack-1|server":      "2026-11-06T01:00:00Z old-run-current\n",
			"tp-rel-stack-1|server|prev": "2026-11-06T00:59:00Z BEFORE-RESTART\n",
		},
	}
	c := newDeployCollector(t, root, src)
	c.discover(context.Background())
	c.Wait()

	// 模拟重启：restarts 0 → 1。
	src.mu.Lock()
	src.pods[0].Containers[0].Restarts = 1
	src.pods[0].Containers[0].LastExit = "OOMKilled"
	src.logs["tp-rel-stack-1|server"] = "2026-11-06T01:05:00Z new-run-current\n"
	src.mu.Unlock()

	c.discover(context.Background())
	c.Wait()

	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	prevIdx := strings.Index(body, "BEFORE-RESTART")
	markerIdx := strings.Index(body, "container restarted #1")
	if prevIdx < 0 {
		t.Fatalf("previous 段没补写: %q", body)
	}
	if markerIdx < 0 {
		t.Fatalf("重启标记缺失: %q", body)
	}
	if prevIdx > markerIdx {
		t.Fatalf("顺序错：previous 段必须在重启标记之前（prev=%d marker=%d）", prevIdx, markerIdx)
	}
	if !strings.Contains(body, "previous exit: OOMKilled") {
		t.Errorf("标记行没带退出原因: %q", body)
	}
}

// ③ Pod 消失 → 写 `--- [pod terminated at …] ---`。
func TestDeployLogCollector_TerminatedMarker(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{
		pods: []ports.PodInfo{{
			Name: "tp-rel-stack-1", Phase: "Running",
			Containers: []ports.ContainerInfo{{Name: "server"}},
		}},
		logs: map[string]string{"tp-rel-stack-1|server": "2026-11-06T01:00:00Z line\n"},
	}
	c := newDeployCollector(t, root, src)
	c.discover(context.Background())
	c.Wait()

	src.mu.Lock()
	src.pods = nil // Pod 消失（被收掉）
	src.mu.Unlock()
	c.discover(context.Background())
	c.Wait()

	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if !strings.Contains(body, "pod terminated at") {
		t.Fatalf("没有 Pod 终止标记: %q", body)
	}
}

type podsDocForTest struct {
	GeneratedAt string `json:"generated_at"`
	Collecting  bool   `json:"collecting"`
	StopReason  string `json:"stop_reason"`
	Pods        []struct {
		Name       string `json:"name"`
		Phase      string `json:"phase"`
		Ready      bool   `json:"ready"`
		Containers []struct {
			Name     string `json:"name"`
			Restarts int32  `json:"restarts"`
		} `json:"containers"`
		Logs []struct {
			File string `json:"file"`
		} `json:"logs"`
	} `json:"pods"`
}

// waitFileNonEmpty 轮询等文件出现且非空（落盘是异步的；CI 慢机器上不能赌调度）。
func waitFileNonEmpty(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return string(raw)
		}
		if time.Now().After(deadline) {
			t.Fatalf("等文件非空超时: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readPodsJSONForTest(t *testing.T, root string) (podsDocForTest, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "r-1", "deploy", "pods.json"))
	if err != nil {
		return podsDocForTest{}, false
	}
	var doc podsDocForTest
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("pods.json 解析失败: %v", err)
	}
	return doc, true
}

// fakePodLogExec 让 fakeExec 也实现 ports.PodLogSource（D-59 的 previous 段）。
type fakePodLogExec struct {
	*fakeExec
	pods     []ports.PodInfo
	previous string
	streamed int
}

func (f *fakePodLogExec) ListPods(ctx context.Context, namespace string) ([]ports.PodInfo, error) {
	return f.pods, nil
}

func (f *fakePodLogExec) StreamLogs(ctx context.Context, namespace, pod, container string,
	opts ports.LogStreamOptions) (io.ReadCloser, error) {
	f.streamed++
	if !opts.Previous {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return io.NopCloser(strings.NewReader(f.previous)), nil
}

// ⑦ attempt 容器重启：stdout.log 里要有 previous 段 + 重启标记（顺序：previous 在前）。
func TestWriteAttemptLogs_RestartAddsPreviousAndMarker(t *testing.T) {
	root := t.TempDir()
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	ex := &fakePodLogExec{
		fakeExec: &fakeExec{logContent: "current-run-line\n"},
		pods: []ports.PodInfo{{
			Name: "tp-r-1-t-1-a1-xyz",
			Containers: []ports.ContainerInfo{{
				Name: "tester", Restarts: 1, LastExit: "OOMKilled",
			}},
		}},
		previous: "BEFORE-RESTART-LINE\n",
	}
	c := newTestController(t, repos, ex.fakeExec, Config{}, nil, nil)
	c.cfg.StorageRoot = root
	c.exec = ex // 用带 PodLogSource 的版本

	if err := c.writeAttemptLogs(context.Background(), ref); err != nil {
		t.Fatalf("writeAttemptLogs: %v", err)
	}
	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", "tasks", "t-1", "attempts", "1", "logs", "stdout.log"))
	prevIdx := strings.Index(body, "BEFORE-RESTART-LINE")
	markerIdx := strings.Index(body, "container restarted #1")
	if prevIdx < 0 || markerIdx < 0 {
		t.Fatalf("缺 previous 段或重启标记: %q", body)
	}
	if prevIdx > markerIdx {
		t.Fatalf("顺序错：previous 段应在标记之前 (prev=%d marker=%d)", prevIdx, markerIdx)
	}
	if !strings.Contains(body, "previous exit: OOMKilled") {
		t.Errorf("标记行没带退出原因: %q", body)
	}
	if !strings.Contains(body, "current-run-line") {
		t.Errorf("当前流内容丢了: %q", body)
	}
}

// 孤儿 .tmp：写日志前要清掉（否则 /artifacts 里能看到 0 字节残留）。
func TestWriteAttemptLogs_CleansStaleTmp(t *testing.T) {
	root := t.TempDir()
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	logsDir := filepath.Join(root, "r-1", "tasks", "t-1", "attempts", "1", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(logsDir, "stdout.log.tmp")
	if err := os.WriteFile(orphan, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := newTestController(t, repos, &fakeExec{logContent: "x\n"}, Config{}, nil, nil)
	c.cfg.StorageRoot = root
	if err := c.writeAttemptLogs(context.Background(), ref); err != nil {
		t.Fatalf("writeAttemptLogs: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤儿 .tmp 没被清理: err=%v", err)
	}
}

// ---- D-59 补充条款：停止时机 ---------------------------------------------

// 采集已停止但 Pod 还活着（cleanup: never / skipped / 卸载失败）→ 继续跟，Pod 后创建也能落盘。
func TestDeployLogCollector_KeepsCollectingWhenUninstallSkipped(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{pods: nil}
	c := newDeployCollector(t, root, src)
	c.Start(context.Background())
	defer c.Stop("test cleanup")

	// 第一次发现：还没有 Pod（与真机现象一致：Run 终态时 Pod 还没起来）。
	c.discover(context.Background())
	c.Wait()
	if !c.IsCollecting() {
		t.Fatal("没有 Pod 时不该自我停止")
	}

	// Pod 后创建：应被发现并落盘。
	src.mu.Lock()
	src.pods = []ports.PodInfo{{
		Name: "tp-rel-stack-1", Phase: "Running", Ready: true,
		Containers: []ports.ContainerInfo{{Name: "server"}},
	}}
	src.logs = map[string]string{"tp-rel-stack-1|server": "2026-11-06T01:20:00Z late-pod-line\n"}
	src.mu.Unlock()

	c.discover(context.Background())
	c.Wait()
	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if !strings.Contains(body, "late-pod-line") {
		t.Fatalf("后创建的 Pod 日志没落盘: %q", body)
	}
	if !c.IsCollecting() {
		t.Fatal("Pod 还在时不该停")
	}
}

// uninstall=succeeded/verified → Run 终态即停，并写 stopped 标记 + pods.json collecting=false。
func TestDeployLogCollector_StopsWhenUninstalled(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{
		pods: []ports.PodInfo{{
			Name: "tp-rel-stack-1", Phase: "Running",
			Containers: []ports.ContainerInfo{{Name: "server"}},
		}},
		logs: map[string]string{"tp-rel-stack-1|server": "2026-11-06T01:00:00Z x\n"},
	}
	c := newDeployCollector(t, root, src)
	c.Start(context.Background())
	c.discover(context.Background())
	c.Wait()

	c.Stop("run terminal + deploy uninstalled")
	c.Wait()
	if c.IsCollecting() {
		t.Fatal("uninstall 之后应该停止采集")
	}
	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if !strings.Contains(body, "collection stopped: run terminal + deploy uninstalled") {
		t.Fatalf("缺 stopped 标记: %q", body)
	}
	doc, _ := readPodsJSONForTest(t, root)
	if doc.Collecting {
		t.Fatal("pods.json 的 collecting 应为 false")
	}
	if !strings.Contains(doc.StopReason, "uninstalled") {
		t.Errorf("pods.json stop_reason = %q", doc.StopReason)
	}
}

// 所有 Pod 消失 → 收尾写 stopped（原因写明），不静默停。
func TestDeployLogCollector_StopsWhenAllPodsGone(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{
		pods: []ports.PodInfo{{
			Name: "tp-rel-stack-1", Phase: "Running",
			Containers: []ports.ContainerInfo{{Name: "server"}},
		}},
		logs: map[string]string{"tp-rel-stack-1|server": "2026-11-06T01:00:00Z x\n"},
	}
	c := newDeployCollector(t, root, src)
	c.Start(context.Background())
	c.discover(context.Background())
	c.Wait()

	src.mu.Lock()
	src.pods = nil
	src.mu.Unlock()
	c.discover(context.Background())
	c.Wait()

	if c.IsCollecting() {
		t.Fatal("Pod 全消失后应该停止采集")
	}
	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if !strings.Contains(body, "pod terminated at") {
		t.Errorf("缺 pod terminated 标记: %q", body)
	}
	if !strings.Contains(body, "collection stopped: all pods gone") {
		t.Errorf("缺 collected stopped 收尾标记: %q", body)
	}
}

// MaxDuration 到达 → 停止并写 max duration 标记。
func TestDeployLogCollector_StopsAtMaxDuration(t *testing.T) {
	root := t.TempDir()
	src := &fakePodLogs{
		pods: []ports.PodInfo{{
			Name: "tp-rel-stack-1", Phase: "Running",
			Containers: []ports.ContainerInfo{{Name: "server"}},
		}},
		logs: map[string]string{"tp-rel-stack-1|server": "2026-11-06T01:00:00Z x\n"},
	}
	c := newDeployCollector(t, root, src)
	// 不启后台循环：直接把"已经开始且远超上限"的状态摆好，discover 一次即可判定。
	// （早先版本用 Start + 1ms 上限，在 CI/race 下会偶发绿红。）
	c.MaxDuration = time.Millisecond
	c.mu.Lock()
	c.startedAt = time.Now().Add(-time.Hour)
	c.collecting = true
	c.mu.Unlock()
	c.discover(context.Background())
	c.Wait()
	if c.IsCollecting() {
		t.Fatal("到上限应停止采集")
	}
	body := waitFileNonEmpty(t, filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if !strings.Contains(body, "max duration reached") {
		t.Fatalf("缺 max duration 标记: %q", body)
	}
}

// 控制器层：Run 终态 + `cleanup: never`（uninstall.state=skipped）→ **不停**采集；
// uninstall=verified → 立即停。这是真机 r-20261106-0D5J 的那个缺口。
func TestControllerKeepsCollectorWhenUninstallSkipped(t *testing.T) {
	root := t.TempDir()
	repos := NewMemoryRepos()
	for _, tc := range []struct {
		name      string
		uninstall string
		wantStop  bool
	}{
		{"skipped_keeps_collecting", "skipped", false},
		{"verified_stops", "verified", true},
		{"failed_keeps_collecting", "failed", false},
	} {
		repos.SeedRun(RunView{
			ID: "r-" + tc.name, Status: domain.RunSucceeded, HasDeploy: true,
			CleanupPolicy: "never", SubmittedAt: time.Now(),
			DeployJSON: json.RawMessage(`{"release_name":"tp-x","namespace":"testplane-mock","uninstall":{"state":"` + tc.uninstall + `"}}`),
		})
		r := repos.runs["r-"+tc.name]
		r.DeployJSON = json.RawMessage(`{"release_name":"tp-x","namespace":"testplane-mock","uninstall":{"state":"` + tc.uninstall + `"}}`)

		src := &fakePodLogs{
			pods: []ports.PodInfo{{
				Name: "tp-x-stack-1", Phase: "Running",
				Containers: []ports.ContainerInfo{{Name: "server"}},
			}},
			logs: map[string]string{"tp-x-stack-1|server": "2026-11-06T01:00:00Z line\n"},
		}
		ex := &fakePodLogExec{fakeExec: &fakeExec{logContent: "x"}, pods: src.pods}
		c := newTestController(t, repos, ex.fakeExec, Config{}, nil, nil)
		c.cfg.StorageRoot = root
		c.exec = &combinedExec{fakeExec: ex.fakeExec, src: src}

		c.ensureDeployLogCollector(context.Background(), RunView{ID: "r-" + tc.name, DeployJSON: r.DeployJSON})
		c.cleanupDeployIfTerminal(context.Background(), "r-"+tc.name)
		// 采集是后台 goroutine：测试结束前必须停掉并等它写完，否则 t.TempDir 清理会撞上并发写
		// （CI 上就是这么红的：TempDir RemoveAll cleanup: unlinkat …）。
		t.Cleanup(func() {
			c.mu.Lock()
			left := make([]*DeployLogCollector, 0, len(c.deployLogs))
			for _, col := range c.deployLogs {
				left = append(left, col)
			}
			c.mu.Unlock()
			for _, col := range left {
				col.StopQuiet()
			}
		})

		c.mu.Lock()
		collector := c.deployLogs["r-"+tc.name]
		c.mu.Unlock()
		if tc.wantStop && collector != nil {
			t.Errorf("%s: 卸载后应停止采集，但 collector 还在", tc.name)
		}
		if !tc.wantStop {
			if collector == nil {
				t.Fatalf("%s: 未卸载时不该停采集（真机缺口）", tc.name)
			}
			collector.Stop("test cleanup")
			collector.Wait()
		}
	}
}

// combinedExec 同时提供 Executor 与 PodLogSource 能力（控制器两步用不同接口）。
type combinedExec struct {
	fakeExec *fakeExec
	src      *fakePodLogs
}

func (c *combinedExec) Submit(ctx context.Context, spec ports.JobSpec) (ports.JobHandle, error) {
	return c.fakeExec.Submit(ctx, spec)
}
func (c *combinedExec) Watch(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) error {
	return c.fakeExec.Watch(ctx, h, obs)
}
func (c *combinedExec) Cancel(ctx context.Context, h ports.JobHandle) error {
	return c.fakeExec.Cancel(ctx, h)
}
func (c *combinedExec) Logs(ctx context.Context, h ports.JobHandle) (io.ReadCloser, error) {
	return c.fakeExec.Logs(ctx, h)
}
func (c *combinedExec) Delete(ctx context.Context, h ports.JobHandle, p ports.DeletePolicy) error {
	return c.fakeExec.Delete(ctx, h, p)
}
func (c *combinedExec) ListPods(ctx context.Context, ns string) ([]ports.PodInfo, error) {
	return c.src.ListPods(ctx, ns)
}
func (c *combinedExec) StreamLogs(ctx context.Context, ns, pod, ct string, o ports.LogStreamOptions) (io.ReadCloser, error) {
	return c.src.StreamLogs(ctx, ns, pod, ct, o)
}
