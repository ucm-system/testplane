package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ucm-system/testplane/server/internal/store"
)

// Deploy 盘的读取侧（D-59：落盘副本是唯一真源，盘上缺失才降级实时读）。
//
// 目录布局（03-data-view §4.4.1）：
//   <run>/deploy/logs/<pod>.log             单容器 Pod
//   <run>/deploy/logs/<pod>.<container>.log 多容器 Pod
//   <run>/deploy/logs/<pod>.1.log           200MB 滚档后的上一代（读的时候拼在前面）
//   <run>/deploy/pods.json                  拓扑快照（每 30s 原子写）

const (
	deployPodsJSONRel = "deploy/pods.json"
	deployLogDirRel   = "deploy/logs"
)

// deployLogPodJSON / deployLogContainerJSON / deployLogFileJSON 是 pods.json 的结构
// （03-data-view §4.4.1）。
type deployLogFileJSON struct {
	File      string `json:"file"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	Truncated bool   `json:"truncated"`
}

type deployLogContainerJSON struct {
	Name     string `json:"name"`
	Restarts int32  `json:"restarts"`
	LastExit string `json:"last_exit,omitempty"`
}

type deployLogPodJSON struct {
	Name       string                   `json:"name"`
	Phase      string                   `json:"phase"`
	Node       string                   `json:"node,omitempty"`
	Ready      bool                     `json:"ready"`
	Containers []deployLogContainerJSON `json:"containers"`
	Logs       []deployLogFileJSON      `json:"logs"`
}

type deployPodsDoc struct {
	GeneratedAt string             `json:"generated_at"`
	Pods        []deployLogPodJSON `json:"pods"`
}

// readDeployPodsDoc 读 `deploy/pods.json`。ok=false 表示盘上没有（→ 调用方降级并说明原因）。
func readDeployPodsDoc(root, runID string) (deployPodsDoc, bool) {
	var doc deployPodsDoc
	if root == "" || runID == "" {
		return doc, false
	}
	raw, err := os.ReadFile(filepath.Join(root, runID, deployPodsJSONRel))
	if err != nil {
		return doc, false
	}
	// 兼容早期"只写数组"的形态。
	if err := json.Unmarshal(raw, &doc); err == nil && doc.Pods != nil {
		return doc, true
	}
	var arr []deployLogPodJSON
	if err := json.Unmarshal(raw, &arr); err == nil {
		return deployPodsDoc{Pods: arr}, true
	}
	return doc, false
}

// deployLogFileOf 返回某个 Pod 的日志文件（相对 <run>/deploy/logs），优先单容器命名。
// 找不到返回空串。
func deployLogFileOf(root, runID, pod string) string {
	dir := filepath.Join(root, runID, deployLogDirRel)
	if ok, _ := fileExistsAt(filepath.Join(dir, pod+".log")); ok {
		return pod + ".log"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, 4)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".1.log") {
			continue
		}
		// `<pod>.<container>.log`：前缀必须是 `<pod>.`，避免把别的 Pod 抢过来。
		if strings.HasPrefix(name, pod+".") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// deployLogLinesFromDisk 读落盘日志（含滚档的上一代），返回按行切好的内容。
// ok=false 表示该 Pod 盘上没有日志（→ 降级实时读并标注"实时窗口"）。
func deployLogLinesFromDisk(root, runID, pod string) ([]string, string, bool) {
	file := deployLogFileOf(root, runID, pod)
	if file == "" {
		return nil, "", false
	}
	dir := filepath.Join(root, runID, deployLogDirRel)
	var lines []string
	// 上一代（滚档）在前：`<pod>.1.log`。
	rotated := strings.TrimSuffix(file, ".log") + ".1.log"
	if raw, err := os.ReadFile(filepath.Join(dir, rotated)); err == nil {
		lines = append(lines, splitLogLines(string(raw))...)
	}
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return nil, "", false
	}
	lines = append(lines, splitLogLines(string(raw))...)
	return lines, deployLogDirRel + "/" + file, true
}

// deployLogFiles 列出该 Run 的 Deploy 日志文件（相对 Run 目录，按名排序）。
func deployLogFiles(root, runID string) []string {
	dir := filepath.Join(root, runID, deployLogDirRel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		out = append(out, deployLogDirRel+"/"+e.Name())
	}
	sort.Strings(out)
	return out
}

func fileExistsAt(path string) (bool, int64) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false, 0
	}
	return true, st.Size()
}

// podBelongsToRun 是"这个 Pod 属不属于本 Run"的**唯一**判据（#45-2）。
//
// 之前两处各写一套：bundle 用 `strings.HasPrefix(name,"tp-") || contains(run.ID)`，
// /deploy 用 `HasPrefix(name, release_name)` —— 真 Genet release 的 Pod 名既不是 tp- 开头
// 也不含 run id，于是打包时全被跳过，只在 MANIFEST.missing 里留一句"列 Pod 失败"。
//
// 统一规则（release_name 已知时以它为准，未知时退化到宽松匹配）：
//  1. release_name 已知 → Pod 名以 `<release>` 开头（Genet 的命名约定）；
//  2. 否则 → Pod 名以 `tp-` 开头（平台自己的 Job Pod），或包含 run id（本地 mock/调试）。
func podBelongsToRun(pod corev1.Pod, run *store.Run, releaseName string) bool {
	name := pod.Name
	if releaseName != "" {
		return strings.HasPrefix(name, releaseName)
	}
	if run != nil && run.ID != "" && strings.Contains(name, run.ID) {
		return true
	}
	return strings.HasPrefix(name, "tp-")
}
