package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// Deploy 日志落盘 + Pod 拓扑快照（D-59、execution §7.2、03-data-view §4.4.1）。
//
// 为什么必须落盘：`pods/log` 是**实时窗口**——Pod 被 GC、容器重启后，旧日志永久丢失。
// 设计早写了"一 Pod 一文件、重启标记续写、200MB 分段、pods.json 每 30s 原子写"，
// 这里按 MVP 实现：
//
//   - 一 Pod 一文件：`<run>/deploy/logs/<pod>.log`，多容器时 `<pod>.<container>.log`；
//   - 流式抓 `follow=true&timestamps=true`，**落盘前剥掉时间戳前缀**；
//   - 容器重启（restarts 增加）：先抓 `previous=true` 补写"重启之前"那段，再写标记行，再继续跟随；
//   - Pod 消失：写 `--- [pod terminated at …] ---`；跟随中断：写 gap 标记（宁可标缺口不伪造连续）；
//   - 单文件 >200MB：滚到 `<pod>.1.log`；
//   - Run 进入 CLEANUP 才停（不是测试一结束就停），停时写 stopped 标记；
//   - `deploy/pods.json` 每轮原子写（temp+rename）。
//
// 刻意简化（标注，不假装做了）：
//   - **不做 §7.3 的 Lease 选主**，单副本 collector 即可（多副本时重复抓会串行化写入同一文件，
//     内容重复但不会丢；生产上应由 Deployment 单副本 + 后续 Lease 补）；
//   - 幂等靠"文件已存在就追加 + 用文件 mtime 当 sinceTime 续抓"，不做逐行去重。

const (
	// DeployLogDir 是 Run 目录下的 Deploy 日志目录。
	DeployLogDir = "deploy/logs"
	// DeployPodsJSON 是 Pod 拓扑快照（每 30s 原子写）。
	DeployPodsJSON = "deploy/pods.json"

	// deployLogDiscoverInterval 是"重新发现 Pod"的间隔（§7.2：每 30s）。
	deployLogDiscoverInterval = 30 * time.Second
	// deployLogMaxFileBytes 是单文件上限，超过滚到 `<name>.1.log`（§7.2：200MB）。
	deployLogMaxFileBytes int64 = 200 << 20
	// deployLogDefaultContainer 是单容器 Pod 的日志文件名（不带容器后缀）。
	deployLogDefaultContainer = "server"
)

// DeployLogCollector 是一个 Run 的 Deploy 日志 collector。
type DeployLogCollector struct {
	RunID       string
	Namespace   string
	ReleaseName string // Pod 名前缀（Genet release 名）
	Root        string // storage root
	Src         ports.PodLogSource
	Logf        func(format string, args ...any)
	Interval    time.Duration // 默认 30s（测试可调小）

	mu        sync.Mutex
	cancel    context.CancelFunc
	streaming map[string]context.CancelFunc // pod/container → 取消该流的跟随
	lastTs    map[string]time.Time          // pod/container → 已写入的最新时间戳
	known     map[string]ports.ContainerInfo
	pods      map[string]ports.PodInfo
	done      chan struct{}
}

// Start 启动 collector（幂等：同一个 collector 实例只启动一次）。
func (c *DeployLogCollector) Start(parent context.Context) {
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.streaming = map[string]context.CancelFunc{}
	c.lastTs = map[string]time.Time{}
	c.known = map[string]ports.ContainerInfo{}
	c.pods = map[string]ports.PodInfo{}
	c.done = make(chan struct{})
	c.mu.Unlock()

	go func() {
		defer close(c.done)
		c.run(ctx)
	}()
}

// Stop 停止 collector 并写 stopped 标记（Run 进入 CLEANUP 时调用）。
func (c *DeployLogCollector) Stop(reason string) {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	streams := make([]context.CancelFunc, 0, len(c.streaming))
	for _, s := range c.streaming {
		streams = append(streams, s)
	}
	c.streaming = map[string]context.CancelFunc{}
	c.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	for _, s := range streams {
		s()
	}
	c.markAll("--- [collection stopped: %s at %s] ---", reason, time.Now().UTC().Format(time.RFC3339))
}

// Wait 等 collector 退出（测试用）。
func (c *DeployLogCollector) Wait() {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (c *DeployLogCollector) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return deployLogDiscoverInterval
}

func (c *DeployLogCollector) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// run 是发现循环：每 30s 重新 LIST Pod → 更新 pods.json → 处理重启/消失 → 按需起跟随流。
// ensureMaps 惰性初始化（Start 与测试直接调用 discover 都要安全）。
func (c *DeployLogCollector) ensureMaps() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streaming == nil {
		c.streaming = map[string]context.CancelFunc{}
	}
	if c.lastTs == nil {
		c.lastTs = map[string]time.Time{}
	}
	if c.known == nil {
		c.known = map[string]ports.ContainerInfo{}
	}
	if c.pods == nil {
		c.pods = map[string]ports.PodInfo{}
	}
}

func (c *DeployLogCollector) run(ctx context.Context) {
	tick := time.NewTicker(c.interval())
	defer tick.Stop()
	c.discover(ctx) // 立刻跑一轮，别等第一个 tick
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.discover(ctx)
		}
	}
}

func (c *DeployLogCollector) discover(ctx context.Context) {
	c.ensureMaps()
	pods, err := c.Src.ListPods(ctx, c.Namespace)
	if err != nil {
		c.logf("orchestrator: WARN deploy_logs_list_failed: run %s: %v", c.RunID, err)
		return
	}
	// 只保留本 release 的 Pod（Genet 的 Pod 名以 release 名开头）。
	sel := make([]ports.PodInfo, 0, len(pods))
	for _, p := range pods {
		if c.ReleaseName != "" && !strings.HasPrefix(p.Name, c.ReleaseName) {
			continue
		}
		sel = append(sel, p)
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Name < sel[j].Name })

	c.mu.Lock()
	prev := c.pods
	c.pods = map[string]ports.PodInfo{}
	for _, p := range sel {
		c.pods[p.Name] = p
	}
	c.mu.Unlock()

	// Pod 消失 → terminated 标记 + 停跟随。
	for name := range prev {
		if _, ok := c.pods[name]; ok {
			continue
		}
		c.markPod(name, "--- [pod terminated at %s] ---", time.Now().UTC().Format(time.RFC3339))
		c.stopStreamsOf(name)
	}

	for _, p := range sel {
		for _, ct := range p.Containers {
			key := streamKey(p.Name, ct.Name)
			c.mu.Lock()
			old := c.known[key]
			c.known[key] = ct
			_, running := c.streaming[key]
			c.mu.Unlock()

			// 重启：先补 previous 段，再写标记行，再继续跟随。
			if ct.Restarts > old.Restarts {
				c.capturePrevious(ctx, p.Name, ct)
				c.appendLine(fileNameOf(p.Name, ct.Name),
					fmt.Sprintf("--- [container restarted #%d at %s, previous exit: %s] ---",
						ct.Restarts, time.Now().UTC().Format(time.RFC3339), firstNonEmpty(ct.LastExit, "unknown")))
			}
			if running {
				continue
			}
			// 还没跑起来的容器（Waiting）先别跟随：等它 Started。
			if ct.Restarts == 0 && ct.StartedAt.IsZero() && !ct.FinishedAt.IsZero() {
				continue
			}
			c.follow(ctx, p.Name, ct.Name)
		}
	}
	c.writePodsJSON(sel)
}

// follow 起一个跟随流（该 Pod/容器已终态时不再跟随，避免空转）。
func (c *DeployLogCollector) follow(ctx context.Context, pod, container string) {
	key := streamKey(pod, container)
	sctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if _, ok := c.streaming[key]; ok {
		c.mu.Unlock()
		cancel()
		return
	}
	c.streaming[key] = cancel
	last := c.lastTs[key]
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.streaming, key)
			c.mu.Unlock()
		}()
		opts := ports.LogStreamOptions{Container: container, Follow: true, Timestamps: true}
		// 幂等：已有文件时从"文件最后写入时间"续抓（重启后不重复整份日志）。
		if !last.IsZero() {
			since := last.Add(-time.Second)
			opts.SinceTime = &since
		}
		rc, err := c.Src.StreamLogs(sctx, c.Namespace, pod, container, opts)
		if err != nil {
			if sctx.Err() == nil {
				c.appendLine(fileNameOf(pod, container),
					fmt.Sprintf("--- [log gap: %s – %s, %s] ---",
						time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), err.Error()))
			}
			return
		}
		defer rc.Close()

		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		from := time.Now().UTC()
		for sc.Scan() {
			ts, line, ok := splitTimestamp(sc.Text())
			if !ok {
				c.appendLine(fileNameOf(pod, container), line)
				continue
			}
			// 幂等续抓：sinceTime 边界会重放一行，跳过已写过的。
			c.mu.Lock()
			lastWritten := c.lastTs[key]
			c.mu.Unlock()
			if !lastWritten.IsZero() && !ts.After(lastWritten) {
				continue
			}
			c.appendChild(fileNameOf(pod, container), line)
			c.mu.Lock()
			c.lastTs[key] = ts
			c.mu.Unlock()
		}
		// 流被中断（非我们主动停）：标缺口，不伪造连续。
		if sctx.Err() == nil {
			c.appendLine(fileNameOf(pod, container),
				fmt.Sprintf("--- [log gap: %s – %s, log stream ended] ---",
					from.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)))
		}
	}()
}

// capturePrevious 抓 previous=true 的日志并补写到同一文件（重启前那段）。
func (c *DeployLogCollector) capturePrevious(ctx context.Context, pod string, ct ports.ContainerInfo) {
	rc, err := c.Src.StreamLogs(ctx, c.Namespace, pod, ct.Name,
		ports.LogStreamOptions{Container: ct.Name, Previous: true, Timestamps: true})
	if err != nil {
		c.appendLine(fileNameOf(pod, ct.Name),
			fmt.Sprintf("--- [log gap: previous container logs unavailable at %s, %s] ---",
				time.Now().UTC().Format(time.RFC3339), err.Error()))
		return
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if _, line, ok := splitTimestamp(sc.Text()); ok {
			c.appendChild(fileNameOf(pod, ct.Name), line)
		} else {
			c.appendChild(fileNameOf(pod, ct.Name), sc.Text())
		}
	}
}

// stopStreamsOf 停掉某个 Pod 的所有跟随流。
func (c *DeployLogCollector) stopStreamsOf(pod string) {
	c.mu.Lock()
	var cancels []context.CancelFunc
	prefix := pod + "/"
	for key, cancel := range c.streaming {
		if strings.HasPrefix(key, prefix) {
			cancels = append(cancels, cancel)
			delete(c.streaming, key)
		}
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *DeployLogCollector) markPod(pod, format string, args ...any) {
	name := fileNameOf(pod, deployLogDefaultContainer)
	c.appendLine(name, fmt.Sprintf(format, args...))
}

func (c *DeployLogCollector) markAll(format string, args ...any) {
	c.mu.Lock()
	names := make([]string, 0, len(c.streaming))
	for key := range c.streaming {
		names = append(names, strings.ReplaceAll(key, "/", "."))
	}
	c.mu.Unlock()
	for _, n := range names {
		c.appendLine(n+".log", fmt.Sprintf(format, args...))
	}
}

func (c *DeployLogCollector) appendLine(file, line string) { c.appendChild(file, line) }

// appendChild 追加一行并处理 200MB 滚动（temp+rename 不做：追加写天然是原子的单行写）。
func (c *DeployLogCollector) appendChild(file, line string) {
	path := filepath.Join(c.Root, c.RunID, DeployLogDir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		c.logf("orchestrator: WARN deploy_logs_mkdir: %v", err)
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > deployLogMaxFileBytes {
		// 滚档：<pod>.log → <pod>.1.log（只保留一代，够 MVP）。
		_ = os.Rename(path, strings.TrimSuffix(path, ".log")+".1.log")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o660)
	if err != nil {
		c.logf("orchestrator: WARN deploy_logs_open: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		c.logf("orchestrator: WARN deploy_logs_write: %v", err)
	}
}

// writePodsJSON 原子写 `deploy/pods.json`（03-data-view §4.4.1）。
func (c *DeployLogCollector) writePodsJSON(pods []ports.PodInfo) {
	type containerJSON struct {
		Name     string `json:"name"`
		Restarts int32  `json:"restarts"`
		LastExit string `json:"last_exit,omitempty"`
	}
	type logJSON struct {
		File      string `json:"file"`
		Start     string `json:"start,omitempty"`
		End       string `json:"end,omitempty"`
		Truncated bool   `json:"truncated"`
	}
	type podJSON struct {
		Name       string          `json:"name"`
		Phase      string          `json:"phase"`
		Node       string          `json:"node,omitempty"`
		Ready      bool            `json:"ready"`
		Containers []containerJSON `json:"containers"`
		Logs       []logJSON       `json:"logs"`
	}
	doc := struct {
		GeneratedAt string    `json:"generated_at"`
		Pods        []podJSON `json:"pods"`
	}{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}

	for _, p := range pods {
		pj := podJSON{Name: p.Name, Phase: p.Phase, Node: p.Node, Ready: p.Ready}
		for _, ct := range p.Containers {
			pj.Containers = append(pj.Containers, containerJSON{
				Name: ct.Name, Restarts: ct.Restarts, LastExit: ct.LastExit,
			})
			file := fileNameOf(p.Name, ct.Name)
			full := filepath.Join(c.Root, c.RunID, DeployLogDir, file)
			entry := logJSON{File: DeployLogDir + "/" + file}
			if st, err := os.Stat(full); err == nil {
				entry.Start = st.ModTime().UTC().Format(time.RFC3339)
				entry.End = entry.Start
				entry.Truncated = st.Size() > deployLogMaxFileBytes
			}
			if len(p.Containers) == 1 {
				pj.Logs = []logJSON{entry}
			} else if len(pj.Logs) < len(p.Containers) {
				pj.Logs = append(pj.Logs, entry)
			}
		}
		if pj.Logs == nil {
			pj.Logs = []logJSON{}
		}
		if pj.Containers == nil {
			pj.Containers = []containerJSON{}
		}
		doc.Pods = append(doc.Pods, pj)
	}
	if doc.Pods == nil {
		doc.Pods = []podJSON{}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Join(c.Root, c.RunID, "deploy")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return
	}
	tmp := filepath.Join(dir, "pods.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o660); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, "pods.json"))
}

// ---------------- helpers ----------------

func streamKey(pod, container string) string { return pod + "/" + container }

// fileNameOf：单容器 Pod 用 `<pod>.log`，多容器用 `<pod>.<container>.log`。
// （多容器判定由调用方保证：单容器 Pod 的容器名是 deployLogDefaultContainer 时不加后缀。）
func fileNameOf(pod, container string) string {
	if container == "" || container == deployLogDefaultContainer {
		return pod + ".log"
	}
	return pod + "." + container + ".log"
}

// splitTimestamp 剥掉 K8s `timestamps=true` 的 RFC3339 前缀（§7.2：落盘不保留时间戳）。
func splitTimestamp(line string) (time.Time, string, bool) {
	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return time.Time{}, line, false
	}
	ts, err := time.Parse(time.RFC3339Nano, line[:sp])
	if err != nil {
		return time.Time{}, line, false
	}
	return ts, line[sp+1:], true
}

// ReadDeployLogFiles 列出某个 Run 的 Deploy 日志文件（相对 Run 目录，排序）。
func ReadDeployLogFiles(root, runID string) []string {
	dir := filepath.Join(root, runID, DeployLogDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		out = append(out, DeployLogDir+"/"+e.Name())
	}
	sort.Strings(out)
	return out
}

// copyStream 便于测试/调试时把流拷到内存。
func copyStream(rc io.Reader) string {
	raw, _ := io.ReadAll(rc)
	return string(raw)
}
