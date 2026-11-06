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

// artifactDTO 是 openapi `Artifact` 的投影。sha256 惰性：没算过就是 null（不现算，C-7.5）。
type artifactDTO struct {
	Path      string  `json:"path"`
	Kind      string  `json:"kind"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    *string `json:"sha256"`
	TaskID    *string `json:"task_id"`
	AttemptID *string `json:"attempt_id"`
}

// artifactListDTO 是 Run / Attempt 两级产物清单的共同信封（07 §5.1、§5.2）。
type artifactListDTO struct {
	Items          []artifactDTO            `json:"items"`
	ByKind         map[string][]artifactDTO `json:"by_kind,omitempty"`
	TotalSizeBytes int64                    `json:"total_size_bytes"`
}

func toArtifactDTO(a store.Artifact) artifactDTO {
	return artifactDTO{
		Path: a.Path, Kind: a.Kind, SizeBytes: a.SizeBytes,
		SHA256: a.SHA256, TaskID: a.TaskID, AttemptID: a.AttemptID,
	}
}

func groupArtifacts(rows []store.Artifact) artifactListDTO {
	out := artifactListDTO{
		Items:  make([]artifactDTO, 0, len(rows)),
		ByKind: map[string][]artifactDTO{},
	}
	for _, a := range rows {
		dto := toArtifactDTO(a)
		out.Items = append(out.Items, dto)
		out.ByKind[dto.Kind] = append(out.ByKind[dto.Kind], dto)
		out.TotalSizeBytes += dto.SizeBytes
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Path < out.Items[j].Path })
	return out
}

// handleRunArtifacts 是 GET /api/v1/runs/{run_id}/artifacts：Run 级产物清单，按 kind 分组。
func (s *Server) handleRunArtifacts(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	if _, err := s.st.Runs.Get(r.Context(), nil, runID); err != nil {
		s.failErr(w, r, err)
		return
	}
	rows, err := s.st.Artifacts.ListByRun(r.Context(), nil, runID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := groupArtifacts(rows)
	// D-59：Deploy 的落盘副本也要能下载（deploy/logs/*.log + deploy/pods.json）。
	// 这些文件不进 artifacts 索引表，直接从盘上列，按 "deploy_logs" / "deploy_meta" 分组。
	for _, rel := range deployLogFiles(s.cfg.StorageRoot, runID) {
		full := filepath.Join(s.cfg.StorageRoot, runID, filepath.FromSlash(rel))
		st, serr := os.Stat(full)
		if serr != nil {
			continue
		}
		dto := artifactDTO{Path: rel, Kind: "deploy_logs", SizeBytes: st.Size()}
		out.Items = append(out.Items, dto)
		out.ByKind[dto.Kind] = append(out.ByKind[dto.Kind], dto)
		out.TotalSizeBytes += dto.SizeBytes
	}
	if st, serr := os.Stat(filepath.Join(s.cfg.StorageRoot, runID, deployPodsJSONRel)); serr == nil && !st.IsDir() {
		dto := artifactDTO{Path: deployPodsJSONRel, Kind: "deploy_meta", SizeBytes: st.Size()}
		out.Items = append(out.Items, dto)
		out.ByKind[dto.Kind] = append(out.ByKind[dto.Kind], dto)
		out.TotalSizeBytes += dto.SizeBytes
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Path < out.Items[j].Path })
	writeJSON(w, http.StatusOK, out)
}

// handleAttemptArtifacts 是 GET /api/v1/tasks/{task_id}/attempts/{idx}/artifacts。
func (s *Server) handleAttemptArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID := chi.URLParam(r, "id")
	idx, err := strconv.Atoi(chi.URLParam(r, "idx"))
	if err != nil || idx < 1 {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"attempt idx 必须是 ≥1 的整数", nil, false)
		return
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	// 找到该 idx 的 Attempt（没有就退化成"该 Task 全部 Attempt 的产物"）。
	attemptID := ""
	attempts, err := s.st.Attempts.ListByTask(ctx, nil, taskID)
	if err == nil {
		for _, a := range attempts {
			if a.Idx == idx {
				attemptID = a.ID
				break
			}
		}
	}
	rows, err := s.st.Artifacts.ListByAttempt(ctx, nil, task.RunID, taskID, attemptID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, groupArtifacts(rows))
}

// maxFileDownloadBytes 是不带 Range 时允许整文件下载的上限（1 GiB）。
// 超过则 413，提示改用 Range 分段（07 §5.1「files 下载」）。
const maxFileDownloadBytes = 1 << 30

// handleRunFile 是 GET /api/v1/runs/{run_id}/files?path=<相对路径>：
// 产物下载，支持 HTTP Range（206/416）。路径只由 run 目录 + 校验后的相对路径拼出。
func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	run, err := s.st.Runs.Get(ctx, nil, runID)
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
	// 容忍两种形态（契约见 openapi `/runs/{id}/files`）：
	//   - run 相对：   tasks/<task>/attempts/1/result.json
	//   - storage-root 相对（= /runs/{id}/artifacts 返回的 path）：<run_id>/tasks/...
	// 剥前缀**不豁免校验**：剥完仍走 safeJoin 的全部穿越检查（`..`/绝对路径/盘符/空段）。
	rel = stripRunPrefix(run.ID, rel)
	full, ok := safeJoin(s.cfg.StorageRoot, run.ID, rel)
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

// stripRunPrefix 剥掉 `<run_id>/` 前缀（`/runs/{id}/artifacts` 返回的 path 是 storage-root
// 相对的，直接透传给 /files 会重复拼一次 run 目录）。只剥一层、只在整段匹配时剥。
func stripRunPrefix(runID, rel string) string {
	norm := strings.ReplaceAll(rel, "\\", "/")
	prefix := runID + "/"
	if runID != "" && strings.HasPrefix(norm, prefix) {
		return strings.TrimPrefix(norm, prefix)
	}
	return rel
}

// safeJoin 把 <storage-root>/<run_id> 与用户给的相对路径安全拼起来。
// 拒绝：绝对路径、盘符、任何 ".." 段、空段、NUL；clean 之后再校验一次前缀，
// 保证结果一定落在该 Run 的目录内（不接受用户直接给绝对路径）。
func safeJoin(storageRoot, runID, rel string) (string, bool) {
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
	if strings.Contains(norm, "//") { // 双斜杠 = 空段，直接拒绝（Clean 会把它们吃掉）
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
	base := filepath.Join(storageRoot, runID)
	full := filepath.Join(base, filepath.FromSlash(clean))
	// 双保险：结果必须在 base 之下。
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
	default:
		return ""
	}
}
