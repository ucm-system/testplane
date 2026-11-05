package api

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Run 打包下载（issue #41 / 裁决 D-56）：把"人排错 / AI 读盘"要的东西一次打成 zip。
//
// 固定布局（顺序也是这个顺序，MANIFEST.json 在最前）：
//
//	MANIFEST.json                       包内清单（files/missing/counts，唯一可信的"缺了什么"）
//	report.json  report.md  metadata.json   来自 <storage-root>/<run>/（缺失 → MANIFEST.missing）
//	events.jsonl                        全量 state_events（库是权威，按 seq 升序；已脱敏）
//	artifacts/index.json                产物索引（与 GET /runs/{id}/artifacts 同源）
//	artifacts/<path>                    逐条产物（超过 ResultMaxBytes 截断并在 MANIFEST 标记）
//	logs/tasks/<task>/attempts/<n>.log  Task/Attempt 日志（stdout+stderr 拼接）
//	logs/deploy/<pod>.log               Deploy 侧 Pod 日志（当前窗口可得部分）
//
// 流式：zip.Writer 直接写响应；只有 events 走临时文件（避免整包进内存），
// 其余条目边读边写、单条按上限截断。响应头：application/zip。

const (
	bundleFormatVersion = "1"
	bundleDeployTail    = 2000 // 每个 Pod 取最后 N 行
	bundleMaxDeployPods = 20
)

// bundleFile 是包内一个条目（source 三者其一：disk / bytes / temp）。
type bundleFile struct {
	name       string
	disk       string   // 磁盘绝对路径
	concatLogs []string // 非空时按序拼接这些日志文件（stdout+stderr）
	bytes      []byte   // 内存内容（deploy 日志）
	truncated  bool
	size       int64
}

// bundleManifest 是 MANIFEST.json 的结构。
type bundleManifest struct {
	FormatVersion string         `json:"format_version"`
	RunID         string         `json:"run_id"`
	State         string         `json:"state"`
	Terminal      bool           `json:"terminal"`
	GeneratedAt   string         `json:"generated_at"`
	Counts        map[string]int `json:"counts"`
	Files         []bundleEntry  `json:"files"`
	Missing       []bundleMiss   `json:"missing"`
}

type bundleEntry struct {
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	Truncated bool    `json:"truncated,omitempty"`
	SHA256    *string `json:"sha256,omitempty"`
}

type bundleMiss struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// secretKeyRe 匹配"疑似敏感"的 JSON 键（事件 payload 里不得出现 Secret 值）。
var secretKeyRe = regexp.MustCompile(`(?i)(secret|token|password|passwd|credential|api[_-]?key|authorization|bearer)`)

// redactSecrets 递归把敏感键的值替换成 "***"（结构不变，便于审计"这里本来有值"）。
func redactSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKeyRe.MatchString(k) {
				out[k] = "***"
				continue
			}
			out[k] = redactSecrets(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = redactSecrets(t[i])
		}
		return out
	default:
		return v
	}
}

// handleRunBundle GET /runs/{id}/bundle → application/zip。
func (s *Server) handleRunBundle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	run, err := s.st.Runs.Get(ctx, nil, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	man := bundleManifest{
		FormatVersion: bundleFormatVersion,
		RunID:         run.ID,
		State:         run.Status,
		Terminal:      store.TerminalRunStatus(domain.RunStatus(run.Status)),
		GeneratedAt:   s.now().UTC().Format(time.RFC3339),
		Counts:        map[string]int{},
		Files:         []bundleEntry{},
		Missing:       []bundleMiss{},
	}

	files := make([]bundleFile, 0, 32)
	addMissing := func(path, reason string) {
		man.Missing = append(man.Missing, bundleMiss{Path: path, Reason: reason})
	}

	// ---- 1) 报告三件套（磁盘；未生成 → missing，不静默省略）----
	runDir := s.runDir(run.ID)
	for _, name := range []string{"report.json", "report.md", "metadata.json"} {
		if runDir == "" {
			addMissing(name, "storage_root 未配置")
			continue
		}
		p := filepath.Join(runDir, name)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			reason := "文件不存在（Run 未终态时报告尚未生成）"
			if err != nil && !os.IsNotExist(err) {
				reason = "读取失败: " + err.Error()
			}
			addMissing(name, reason)
			continue
		}
		files = append(files, bundleFile{name: name, disk: p})
	}

	// ---- 2) events.jsonl：永远从库里重建（权威、按 seq，且已脱敏）----
	eventsTmp, nEvents, err := s.writeEventsTempFile(ctx, run.ID)
	if err != nil {
		addMissing("events.jsonl", "读取事件失败: "+err.Error())
	} else {
		defer os.Remove(eventsTmp)
		man.Counts["events"] = nEvents
		files = append(files, bundleFile{name: "events.jsonl", disk: eventsTmp})
	}

	// ---- 3) artifacts/index.json + artifacts/<path> ----
	arts, err := s.st.Artifacts.ListByRun(ctx, nil, run.ID)
	if err != nil {
		addMissing("artifacts/index.json", "读取产物索引失败: "+err.Error())
	} else {
		man.Counts["artifacts"] = len(arts)
		index := make([]map[string]any, 0, len(arts))
		for _, a := range arts {
			index = append(index, map[string]any{
				"path": a.Path, "size_bytes": a.SizeBytes, "sha256": a.SHA256,
				"kind": a.Kind, "task_id": a.TaskID, "attempt_id": a.AttemptID,
			})
			abs := filepath.FromSlash(a.Path)
			if !filepath.IsAbs(abs) && runDir != "" {
				abs = filepath.Join(s.cfg.StorageRoot, abs)
			}
			if st, statErr := os.Stat(abs); statErr != nil || st.IsDir() {
				addMissing("artifacts/"+a.Path, "产物文件不存在")
				continue
			}
			files = append(files, bundleFile{name: "artifacts/" + filepath.ToSlash(a.Path), disk: abs})
		}
		if raw, mErr := json.MarshalIndent(index, "", "  "); mErr == nil {
			files = append(files, bundleFile{name: "artifacts/index.json", bytes: raw})
		} else {
			addMissing("artifacts/index.json", "序列化失败: "+mErr.Error())
		}
	}

	// ---- 4) Task / Attempt 日志 ----
	tasks, listErr := s.listRunTasks(ctx, run.ID)
	if listErr != nil {
		addMissing("logs/tasks", "读取 Task 列表失败: "+listErr.Error())
	}
	for _, t := range tasks {
		if t.CurrentAttempt <= 0 {
			continue
		}
		entry := fmt.Sprintf("logs/tasks/%s/attempts/%d.log", t.ID, t.CurrentAttempt)
		dir := filepath.Join(runDir, "tasks", t.ID, "attempts", fmt.Sprint(t.CurrentAttempt), "logs")
		if runDir == "" {
			addMissing(entry, "storage_root 未配置")
			continue
		}
		names, globErr := filepath.Glob(filepath.Join(dir, "*.log"))
		if globErr != nil || len(names) == 0 {
			addMissing(entry, "attempt 日志不存在")
			continue
		}
		sort.Strings(names)
		files = append(files, bundleFile{name: entry, disk: names[0], concatLogs: names})
	}

	// ---- 5) Deploy 侧 Pod 日志（实时窗口；不可得 → missing）----
	s.appendDeployLogs(ctx, run, &files, addMissing)

	// ---- 6) 落 MANIFEST（先算好 Entry，再写 zip）----
	for i := range files {
		f := &files[i]
		size, trunc, sha, sizeErr := s.bundleEntryStats(ctx, *f)
		if sizeErr != nil {
			addMissing(f.name, "读取失败: "+sizeErr.Error())
			f.name = "" // 标记跳过
			continue
		}
		f.size, f.truncated = size, trunc
		man.Files = append(man.Files, bundleEntry{Path: f.name, SizeBytes: size, Truncated: trunc, SHA256: sha})
	}
	man.Counts["files"] = len(man.Files)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-bundle.zip"`, run.ID))
	w.WriteHeader(http.StatusOK)

	zw := zip.NewWriter(w)
	defer zw.Close()

	manRaw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return
	}
	if err := writeZipBytes(zw, "MANIFEST.json", manRaw); err != nil {
		return
	}
	for _, f := range files {
		if f.name == "" {
			continue
		}
		if err := s.writeBundleEntry(ctx, zw, f); err != nil {
			s.logger.Warn("bundle: 写条目失败", "run_id", run.ID, "entry", f.name, "err", err)
			return
		}
	}
}

// runDir 是某个 Run 的存储根目录（storage_root 未配置时返回空串）。
func (s *Server) runDir(runID string) string {
	if s.cfg == nil || strings.TrimSpace(s.cfg.StorageRoot) == "" {
		return ""
	}
	return filepath.Join(s.cfg.StorageRoot, runID)
}

// listRunTasks 取 Run 的全部 Task（bundle 用；失败不致命）。
func (s *Server) listRunTasks(ctx context.Context, runID string) ([]*store.Task, error) {
	var out []*store.Task
	cursor := ""
	for {
		batch, next, err := s.st.Tasks.ListByRun(ctx, nil, runID, cursor, 200)
		if err != nil {
			return out, err
		}
		for i := range batch {
			out = append(out, &batch[i])
		}
		if next == "" || len(batch) == 0 {
			return out, nil
		}
		cursor = next
	}
}

// writeEventsTempFile 把全量事件（脱敏后，按 seq）写成临时 JSONL，返回路径与条数。
// 走临时文件而不是内存：事件数量与 payload 都可能很大，整包进内存会 OOM。
func (s *Server) writeEventsTempFile(ctx context.Context, runID string) (string, int, error) {
	events, err := s.st.Events.AllByRun(ctx, nil, runID)
	if err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp("", "tp-bundle-events-*.jsonl")
	if err != nil {
		return "", 0, err
	}
	enc := json.NewEncoder(f)
	n := 0
	for _, e := range events {
		var payload any
		if len(e.Payload) > 0 {
			if err := json.Unmarshal(e.Payload, &payload); err == nil {
				payload = redactSecrets(payload)
			}
		}
		row := map[string]any{
			"seq": e.Seq, "type": e.Type, "to_status": e.ToStatus,
			"actor": e.Actor, "created_at": e.CreatedAt.UTC().Format(time.RFC3339),
			"payload": payload,
		}
		if err := enc.Encode(row); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", 0, err
		}
		n++
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), n, nil
}

// appendDeployLogs 收集 Deploy 侧 Pod 日志（只做"当前可得部分"）。
func (s *Server) appendDeployLogs(ctx context.Context, run *store.Run, files *[]bundleFile,
	addMissing func(path, reason string)) {

	var deploy struct {
		Namespace string `json:"namespace"`
		State     string `json:"state"`
	}
	if len(run.Deploy) > 0 {
		_ = json.Unmarshal(run.Deploy, &deploy)
	}
	if deploy.Namespace == "" {
		addMissing("logs/deploy", "该 Run 没有 Deploy（或 deploy 快照缺 namespace）")
		return
	}
	client, err := s.deployLogsClient()
	if err != nil {
		addMissing("logs/deploy", "K8s 客户端不可用: "+err.Error())
		return
	}
	pods, err := client.ListPods(ctx, deploy.Namespace)
	if err != nil {
		addMissing("logs/deploy", "列 Pod 失败: "+err.Error())
		return
	}
	listed := 0
	for _, pod := range pods {
		name := pod.Name
		if !strings.HasPrefix(name, "tp-") && !strings.Contains(name, run.ID) {
			continue // 只取与本 Run 相关的 Pod（Job 名带 task id，不强行关联时跳过）
		}
		if listed >= bundleMaxDeployPods {
			addMissing("logs/deploy", fmt.Sprintf("Pod 数超过上限 %d，其余未打包", bundleMaxDeployPods))
			return
		}
		listed++
		container := firstContainer(name, pods)
		text, logErr := client.Logs(ctx, deploy.Namespace, name, container, bundleDeployTail)
		if logErr != nil {
			addMissing("logs/deploy/"+name+".log", "拉取失败: "+logErr.Error())
			continue
		}
		*files = append(*files, bundleFile{
			name: "logs/deploy/" + name + ".log", bytes: []byte(text),
		})
	}
	if listed == 0 {
		addMissing("logs/deploy", "当前窗口没有与本 Run 相关的 Pod")
	}
}

// bundleEntryStats 计算单条目大小 / 是否截断 / sha256（截断按 ResultMaxBytes 上限）。
func (s *Server) bundleEntryStats(ctx context.Context, f bundleFile) (int64, bool, *string, error) {
	limit := s.resultMaxBytes()
	if f.bytes != nil {
		data := f.bytes
		trunc := false
		if int64(len(data)) > limit {
			data = data[:limit]
			trunc = true
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		return int64(len(data)), trunc, &sha, nil
	}
	// 磁盘条目：大小取所有拼接文件之和。
	var total int64
	paths := f.concatLogs
	if len(paths) == 0 {
		paths = []string{f.disk}
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return 0, false, nil, err
		}
		total += st.Size()
	}
	trunc := total > limit
	size := total
	if trunc {
		size = limit
	}
	sha, err := hashFiles(f.disk, paths, size)
	if err != nil {
		return 0, false, nil, err
	}
	return size, trunc, sha, nil
}

// hashFiles 对（前 size 字节的）内容算 sha256。
func hashFiles(primary string, paths []string, size int64) (*string, error) {
	h := sha256.New()
	var written int64
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		remain := size - written
		if remain <= 0 {
			f.Close()
			break
		}
		n, err := io.CopyN(h, f, remain)
		f.Close()
		if err != nil {
			return nil, err
		}
		written += n
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return &sum, nil
}

// writeBundleEntry 把一条内容流式写进 zip（截断到 limit）。
func (s *Server) writeBundleEntry(ctx context.Context, zw *zip.Writer, f bundleFile) error {
	w, err := zw.Create(f.name)
	if err != nil {
		return err
	}
	// f.size 已是"截断后"的大小（bundleEntryStats 算过上限），直接按它拷贝；
	// 不能用 resultMaxBytes() 当剩余量——否则小文件会因 CopyN 未填满而不必要地报 EOF。
	remain := f.size
	if f.bytes != nil {
		data := f.bytes
		if int64(len(data)) > remain {
			data = data[:remain]
		}
		_, err = w.Write(data)
		return err
	}
	paths := f.concatLogs
	if len(paths) == 0 {
		paths = []string{f.disk}
	}
	for _, p := range paths {
		if remain <= 0 {
			break
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		n, copyErr := io.CopyN(w, src, remain)
		src.Close()
		remain -= n
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func writeZipBytes(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
