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

	raw, err := os.ReadFile(filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if err != nil {
		t.Fatalf("日志未落盘: %v", err)
	}
	got := string(raw)
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

	raw, err := os.ReadFile(filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if err != nil {
		t.Fatalf("读日志: %v", err)
	}
	body := string(raw)
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

	raw, _ := os.ReadFile(filepath.Join(root, "r-1", DeployLogDir, "tp-rel-stack-1.log"))
	if !strings.Contains(string(raw), "pod terminated at") {
		t.Fatalf("没有 Pod 终止标记: %q", string(raw))
	}
}

type podsDocForTest struct {
	GeneratedAt string `json:"generated_at"`
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
	raw, err := os.ReadFile(filepath.Join(root, "r-1", "tasks", "t-1", "attempts", "1", "logs", "stdout.log"))
	if err != nil {
		t.Fatalf("读 stdout.log: %v", err)
	}
	body := string(raw)
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
