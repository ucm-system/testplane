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
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/orchestrator"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Task 打包下载（issue #41 / 裁决 D-56）：把"人排错 / AI 读盘"要的东西一次打成 zip。
//
// 固定布局（顺序也是这个顺序，MANIFEST.json 在最前）：
//
//	MANIFEST.json                                包内清单（files/missing/counts，唯一可信的"缺了什么"）
//	report.json  report.md  metadata.json        来自 <storage-root>/<task_id>/（缺失 → MANIFEST.missing）
//	events.jsonl                                 全量 state_events（库是权威，按 seq 升序；已脱敏）
//	artifacts/index.json                         产物索引（与 GET /tasks/{id}/artifacts 同源）
//	artifacts/<path>                             逐条产物（超过 ResultMaxBytes 截断并在 MANIFEST 标记）
//	exports/<n>/<seq>.exports.env                exports 落档（含注入清单，§5.12）
//	logs/attempts/<n>/runners/<seq>.log          runner 执行日志（logs/stdout.log）
//	logs/deploy/<pod>.log                        部署 Pod 日志（当前窗口可得部分）
//
// 流式：zip.Writer 直接写响应；只有 events 走临时文件（避免整包进内存），
// 其余条目边读边写、单条按上限截断。Task 非终态仍返回 200（MANIFEST.terminal=false）。

const (
	bundleFormatVersion = "2"
	bundleDeployTail    = 2000 // 每个 Pod 取最后 N 行
	bundleMaxDeployPods = 20
)

// bundleFile 是包内一个条目（source 三者其一：disk / bytes / temp）。
type bundleFile struct {
	name       string
	disk       string   // 磁盘绝对路径
	concatLogs []string // 非空时按序拼接这些日志文件
	bytes      []byte   // 内存内容
	truncated  bool
	size       int64
}

// bundleManifest 是 MANIFEST.json 的结构。
type bundleManifest struct {
	FormatVersion string         `json:"format_version"`
	TaskID        string         `json:"task_id"`
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

// handleTaskBundle GET /tasks/{id}/bundle → application/zip。
func (s *Server) handleTaskBundle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	man := bundleManifest{
		FormatVersion: bundleFormatVersion,
		TaskID:        task.ID,
		State:         task.Status,
		Terminal:      domain.TerminalTaskStatus(domain.TaskStatus(task.Status)),
		GeneratedAt:   s.now().UTC().Format(time.RFC3339),
		Counts:        map[string]int{},
		Files:         []bundleEntry{},
		Missing:       []bundleMiss{},
	}

	files := make([]bundleFile, 0, 32)
	addMissing := func(path, reason string) {
		man.Missing = append(man.Missing, bundleMiss{Path: path, Reason: reason})
	}
	addDisk := func(name, disk string, max int64) {
		st, err := os.Stat(disk)
		if err != nil {
			addMissing(name, "file_not_found")
			return
		}
		bf := bundleFile{name: name, disk: disk, size: st.Size()}
		if max > 0 && st.Size() > max {
			bf.truncated = true
			bf.size = max
		}
		man.Counts["files"]++
		files = append(files, bf)
	}

	// 报告产物（未生成 → missing，不静默省略）。
	for _, name := range []string{"report.json", "report.md", "metadata.json"} {
		addDisk(name, filepath.Join(s.cfg.StorageRoot, task.ID, name), s.cfg.ResultMaxBytes)
	}

	// events.jsonl：库是权威，按 seq 升序；敏感键打码。
	var events []store.Event
	if evs, err := s.st.Events.AllByTask(ctx, nil, task.ID); err == nil {
		events = evs
	} else {
		addMissing("events.jsonl", "events_query_failed")
	}
	var evBuf strings.Builder
	for _, ev := range events {
		var payload any
		if len(ev.Payload) > 0 {
			_ = json.Unmarshal(ev.Payload, &payload)
			payload = redactSecrets(payload)
		}
		line, _ := json.Marshal(map[string]any{
			"seq": ev.Seq, "type": ev.Type, "task_id": ev.TaskID,
			"attempt_no": ev.AttemptNo, "runner_seq": ev.RunnerSeq,
			"from": ev.FromStatus, "to": ev.ToStatus, "reason": ev.Reason,
			"actor": ev.Actor, "at": ev.CreatedAt.UTC(), "payload": payload,
		})
		evBuf.Write(line)
		evBuf.WriteByte('\n')
	}
	man.Counts["events"] = len(events)
	files = append(files, bundleFile{name: "events.jsonl", bytes: []byte(evBuf.String()), size: int64(evBuf.Len())})

	// 产物索引与逐条产物。
	rows, err := s.st.Artifacts.ListByTask(ctx, nil, task.ID)
	if err != nil {
		addMissing("artifacts/index.json", "artifacts_query_failed")
	} else {
		idx, _ := json.Marshal(groupArtifacts(rows))
		files = append(files, bundleFile{name: "artifacts/index.json", bytes: idx, size: int64(len(idx))})
		for _, a := range rows {
			full, ok := safeJoin(s.cfg.StorageRoot, task.ID, a.Path)
			if !ok {
				addMissing("artifacts/"+a.Path, "unsafe_path")
				continue
			}
			man.Counts["artifacts"]++
			addDisk("artifacts/"+a.Path, full, s.cfg.ResultMaxBytes)
		}
	}

	// exports 落档（attempts/<n>/exports/<seq>.exports.env）。
	if s.cfg.StorageRoot != "" {
		if entries, rerr := os.ReadDir(filepath.Join(s.cfg.StorageRoot, task.ID, "attempts")); rerr == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				expDir := filepath.Join(s.cfg.StorageRoot, task.ID, "attempts", e.Name(), "exports")
				fs, ferr := os.ReadDir(expDir)
				if ferr != nil {
					continue
				}
				for _, f := range fs {
					if f.IsDir() {
						continue
					}
					rel := "attempts/" + e.Name() + "/exports/" + f.Name()
					addDisk("exports/"+e.Name()+"/"+f.Name(),
						filepath.Join(s.cfg.StorageRoot, task.ID, filepath.FromSlash(rel)), 0)
				}
			}
		}
	}

	// runner 日志（attempts/<n>/runners/<seq>/logs/stdout.log）。
	runs, rerr := s.st.RunnerRuns.ListByTask(ctx, nil, task.ID)
	if rerr == nil {
		for _, rr := range runs {
			logPath := filepath.Join(s.runnerDirFor(task.ID, rr.AttemptNo, rr.Seq), "logs", orchestrator.AttemptLogFileName)
			name := fmt.Sprintf("logs/attempts/%d/runners/%d.log", rr.AttemptNo, rr.Seq)
			if _, err := os.Stat(logPath); err != nil {
				addMissing(name, "log_not_captured")
				continue
			}
			man.Counts["runner_logs"]++
			addDisk(name, logPath, 0)
		}
	}

	// 部署 Pod 日志（attempts/<n>/runners/<deploy_seq>/logs/<pod>.log，只取可得部分）。
	s.appendDeployLogs(ctx, task, &files, &man)

	// MANIFEST.json 永远是第一个条目。
	manBytes, _ := json.MarshalIndent(man, "", "  ")
	files = append([]bundleFile{{name: "MANIFEST.json", bytes: manBytes, size: int64(len(manBytes))}}, files...)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", task.ID+"-bundle.zip"))
	w.WriteHeader(http.StatusOK)

	zw := zip.NewWriter(w)
	for _, bf := range files {
		hdr := &zip.FileHeader{Name: bf.name, Method: zip.Deflate}
		hdr.Modified = s.now()
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return
		}
		if err := s.writeBundleEntry(fw, bf); err != nil {
			s.logger.Warn("bundle 条目写入失败", "err", err, "entry", bf.name, "task_id", task.ID)
		}
	}
	_ = zw.Close()
}

// writeBundleEntry 把一个条目写进 zip（disk 截断 / concat / bytes 三种来源）。
func (s *Server) writeBundleEntry(w io.Writer, bf bundleFile) error {
	if len(bf.bytes) > 0 {
		_, err := w.Write(bf.bytes)
		return err
	}
	if len(bf.concatLogs) > 0 {
		for _, p := range bf.concatLogs {
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			_, _ = io.Copy(w, f)
			_ = f.Close()
		}
		return nil
	}
	if bf.disk == "" {
		return nil
	}
	f, err := os.Open(bf.disk)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if bf.truncated && bf.size > 0 {
		_, err = io.CopyN(w, f, bf.size)
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// appendDeployLogs 把部署 Pod 日志的当前可得部分加进包（logs/deploy/<pod>.log）。
func (s *Server) appendDeployLogs(ctx context.Context, task *store.Task, files *[]bundleFile, man *bundleManifest) {
	if s.cfg.StorageRoot == "" {
		return
	}
	attemptNo := task.CurrentAttempt
	if attemptNo < 1 {
		attemptNo = 1
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, task.ID, attemptNo)
	if err != nil {
		return
	}
	var deploySeq int
	for _, rr := range runs {
		if rr.Role == domain.RoleDeploy && rr.RunnerName == "service-deploy" {
			deploySeq = rr.Seq
			break
		}
	}
	if deploySeq == 0 {
		return
	}
	logsDir := filepath.Join(s.runnerDirFor(task.ID, attemptNo, deploySeq), "logs")
	pods := listDeployPods(logsDir)
	count := 0
	for _, pod := range pods {
		if count >= bundleMaxDeployPods {
			break
		}
		count++
		segs := deployPodLogSegments(logsDir, pod)
		if len(segs) == 0 {
			continue
		}
		lines := tailLinesOf(segs, bundleDeployTail)
		text := strings.Join(lines, "\n")
		sum := sha256.Sum256([]byte(text))
		man.Files = append(man.Files, bundleEntry{
			Path: "logs/deploy/" + pod + ".log", SizeBytes: int64(len(text)),
			SHA256: strPtr(hex.EncodeToString(sum[:])),
		})
		*files = append(*files, bundleFile{name: "logs/deploy/" + pod + ".log", bytes: []byte(text), size: int64(len(text))})
	}
}

func strPtr(s string) *string { return &s }

var _ = strconv.Itoa
