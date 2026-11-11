package api

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 产物端点族（07 §5.1/§5.2、C-7.5）：Task 级 / runner 级清单、runner result（D-74）、
// 文件下载（白名单 + Range，不越出 Task 目录）。

// runner 级产物清单与 result 的两条路由共用同一实现。

// handleTaskRunnerArtifacts 是 GET /tasks/{task_id}/runners/{seq}/artifacts。
func (s *Server) handleTaskRunnerArtifacts(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.serveRunnerArtifacts(w, r, rr)
}

// handleAttemptRunnerArtifacts 是 GET /tasks/{task_id}/attempts/{attempt_no}/runners/{seq}/artifacts。
func (s *Server) handleAttemptRunnerArtifacts(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.serveRunnerArtifacts(w, r, rr)
}

// handleTaskRunnerResult 是 GET .../runners/{seq}/result（D-74：只服务 result.json；
// 只产出 html/csv 时 404 + detail.produced[]）。
func (s *Server) handleTaskRunnerResult(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.serveRunnerResult(w, r, rr)
}

// handleAttemptRunnerResult 是 attempt 级权威定位的 result。
func (s *Server) handleAttemptRunnerResult(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.serveRunnerResult(w, r, rr)
}

// serveRunnerResult 返回该 runner 的 result.json 原文（平台只校验不收窄字段）。
func (s *Server) serveRunnerResult(w http.ResponseWriter, r *http.Request, rr *store.RunnerRun) {
	if rr.Role == domain.RoleDeploy || rr.Role == domain.RoleClean {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound,
			"deploy/clean 角色无 result 产物", map[string]any{"seq": rr.Seq, "role": rr.Role}, false)
		return
	}
	dir := s.runnerDirFor(rr.TaskID, rr.AttemptNo, rr.Seq)
	produced := producedResults(dir)
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		if os.IsNotExist(err) {
			fail(w, r, http.StatusNotFound, domain.CodeNotFound,
				"该 runner 未产出 result.json", map[string]any{
					"task_id": rr.TaskID, "attempt_no": rr.AttemptNo, "seq": rr.Seq,
					"produced": produced,
				}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// producedResults 列出该 runner 实际产出的三选一产物（detail.produced[] 用）。
func producedResults(dir string) []string {
	out := []string{}
	for _, name := range []string{"result.json", "result.html", "result.csv"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() && st.Size() > 0 {
			out = append(out, name)
		}
	}
	return out
}

// serveRunnerArtifacts 返回该 runner 的产物清单（索引表 + exports 落档）。
func (s *Server) serveRunnerArtifacts(w http.ResponseWriter, r *http.Request, rr *store.RunnerRun) {
	ctx := r.Context()
	rows, err := s.st.Artifacts.ListByRunner(ctx, nil, rr.TaskID, rr.AttemptNo, rr.Seq)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := groupArtifacts(rows)
	// exports 落档不进索引表：直接从盘上列（attempts/<n>/exports/<seq>.exports.env）。
	if rr.ExportsRef != nil && *rr.ExportsRef != "" && s.cfg.StorageRoot != "" {
		full := filepath.Join(s.cfg.StorageRoot, rr.TaskID, filepath.FromSlash(*rr.ExportsRef))
		if st, serr := os.Stat(full); serr == nil && !st.IsDir() {
			dto := artifactDTO{Path: *rr.ExportsRef, Kind: "export", SizeBytes: st.Size(),
				AttemptNo: &rr.AttemptNo, RunnerSeq: &rr.Seq}
			out.Items = append(out.Items, dto)
			out.ByKind[dto.Kind] = append(out.ByKind[dto.Kind], dto)
			out.TotalSizeBytes += dto.SizeBytes
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Path < out.Items[j].Path })
	writeJSON(w, http.StatusOK, out)
}

// handleTaskArtifacts 是 GET /api/v1/tasks/{task_id}/artifacts（Task 级产物清单，含 exports 落档）。
func (s *Server) handleTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	if _, err := s.st.Tasks.Get(ctx, nil, taskID); err != nil {
		s.failErr(w, r, err)
		return
	}
	rows, err := s.st.Artifacts.ListByTask(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := groupArtifacts(rows)
	// exports 落档（attempts/<n>/exports/*.exports.env）从盘上列（D-70：索引可重建）。
	if s.cfg.StorageRoot != "" {
		if entries, rerr := os.ReadDir(filepath.Join(s.cfg.StorageRoot, taskID, "attempts")); rerr == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				no, perr := strconv.Atoi(e.Name())
				if perr != nil {
					continue
				}
				expDir := filepath.Join(s.cfg.StorageRoot, taskID, "attempts", e.Name(), "exports")
				files, ferr := os.ReadDir(expDir)
				if ferr != nil {
					continue
				}
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					st, serr := f.Info()
					if serr != nil {
						continue
					}
					rel := "attempts/" + e.Name() + "/exports/" + f.Name()
					noCopy := no
					seq := seqOfExportFile(f.Name())
					dto := artifactDTO{Path: rel, Kind: "export", SizeBytes: st.Size(),
						AttemptNo: &noCopy, RunnerSeq: seq}
					out.Items = append(out.Items, dto)
					out.ByKind[dto.Kind] = append(out.ByKind[dto.Kind], dto)
					out.TotalSizeBytes += dto.SizeBytes
				}
			}
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Path < out.Items[j].Path })
	writeJSON(w, http.StatusOK, out)
}

// seqOfExportFile 从 `<seq>.exports.env` 提取 seq（非该命名返回 nil）。
func seqOfExportFile(name string) *int {
	i := strings.IndexByte(name, '.')
	if i <= 0 {
		return nil
	}
	n, err := strconv.Atoi(name[:i])
	if err != nil {
		return nil
	}
	return &n
}

// maxFileDownloadBytes 是不带 Range 时允许整文件下载的上限（1 GiB）。
// 超过则 413，提示改用 Range 分段（07 §5.1「files 下载」）。
const maxFileDownloadBytes = 1 << 30

// handleTaskFile 是 GET /api/v1/tasks/{task_id}/files?path=<Task 目录相对路径>：
// 产物下载，支持 HTTP Range（206/416）。路径只由 task 目录 + 校验后的相对路径拼出。
func (s *Server) handleTaskFile(w http.ResponseWriter, r *http.Request) {
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
	rel := r.URL.Query().Get("path")
	if rel == "" {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"缺少 path 参数", map[string]any{"param": "path"}, false)
		return
	}
	// 容忍两种形态：task 相对（契约）与 storage-root 相对（/artifacts 返回的 path）。
	rel = stripTaskPrefix(task.ID, rel)
	full, ok := safeJoin(s.cfg.StorageRoot, task.ID, rel)
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"非法路径（不允许绝对路径、盘符、上级目录或空段）",
			map[string]any{"path": rel}, false)
		return
	}

	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			fail(w, r, http.StatusNotFound, domain.CodeNotFound, "产物不存在",
				map[string]any{"path": rel}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if info.IsDir() {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound, "目标是目录，不是文件",
			map[string]any{"path": rel}, false)
		return
	}
	if info.Size() > maxFileDownloadBytes && r.Header.Get("Range") == "" {
		fail(w, r, http.StatusRequestEntityTooLarge, domain.CodeValidationError,
			"文件超过 1 GiB，请使用 Range 头分段下载",
			map[string]any{"size_bytes": info.Size(), "max_bytes": maxFileDownloadBytes}, false)
		return
	}

	if ct := contentTypeFor(rel); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", "inline; filename=\""+path.Base(full)+"\"")
	// ServeContent 负责 200/206/416 与 If-Range 等完整语义。
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// stripTaskPrefix 剥掉 `<task_id>/` 前缀（/artifacts 返回的 path 是 storage-root 相对）。
// 只剥一层、只在整段匹配时剥。
func stripTaskPrefix(taskID, rel string) string {
	norm := strings.ReplaceAll(rel, "\\", "/")
	prefix := taskID + "/"
	if taskID != "" && strings.HasPrefix(norm, prefix) {
		return strings.TrimPrefix(norm, prefix)
	}
	return rel
}

// safeJoin 把 <storage-root>/<task_id> 与用户给的相对路径安全拼起来。
// 拒绝：绝对路径、盘符、任何 ".." 段、空段、NUL；clean 之后再校验一次前缀。
func safeJoin(storageRoot, taskID, rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", false
	}
	norm := strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(norm, "/") {
		return "", false
	}
	if len(norm) >= 2 && norm[1] == ':' { // Windows 盘符
		return "", false
	}
	if strings.Contains(norm, "//") { // 双斜杠 = 空段，直接拒绝
		return "", false
	}
	clean := path.Clean(norm)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	base := filepath.Join(storageRoot, taskID)
	full := filepath.Join(base, filepath.FromSlash(clean))
	relToBase, err := filepath.Rel(base, full)
	if err != nil || strings.HasPrefix(relToBase, "..") || filepath.IsAbs(relToBase) {
		return "", false
	}
	return full, true
}

// contentTypeFor 给常见产物补 Content-Type（ServeContent 亦会按扩展名猜，这里显式兜底）。
func contentTypeFor(rel string) string {
	switch strings.ToLower(path.Ext(rel)) {
	case ".json", ".jsonl":
		return "application/json"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".log", ".txt":
		return "text/plain; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".env":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
