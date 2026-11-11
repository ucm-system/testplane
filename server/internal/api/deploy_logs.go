package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Deploy Pod 日志（07 §5.1 / §8.6 / D-59）：日志在 deploy runner 的产物目录
// attempts/<n>/runners/<deploy_seq>/logs/<pod>.log（按 Pod 一份、持续追加），
// 本端点是它的读取口。分页规则（openapi getTaskDeployLogs）：
//
//   - 排队期没有 Pod → 200 + pods:[] + entries:[] + message（不是 404）；
//     外接模式（无部署）同理；
//   - ?pod= 缺省时按 Pod 数量决定：0 Pod → 200+message；1 Pod → 直读该 Pod；
//     ≥2 Pod → 409 CONFLICT detail.reason=pod_ambiguous + available_pods[]（不默默取第一个）；
//   - 给了 pod 但不在当前列表 → 404 NOT_FOUND detail.reason=pod_not_found；
//   - offset 是该 Pod 全文窗口的行偏移；limit 默认 200、上限 2000；
//     tail_lines 只取末尾 N 行（tail 不可续页），与 offset 同时给出 → 400。
func (s *Server) handleTaskDeployLogs(w http.ResponseWriter, r *http.Request) {
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
	attemptNo, err := strconv.Atoi(r.URL.Query().Get("attempt"))
	if err != nil || attemptNo < 1 {
		attemptNo = task.CurrentAttempt
		if attemptNo < 1 {
			attemptNo = 1
		}
	}

	// 定位 service-deploy 执行记录（该 attempt 内 role=deploy 的那一行）。
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, taskID, attemptNo)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	var deployRow *store.RunnerRun
	for i := range runs {
		if runs[i].Role == domain.RoleDeploy {
			deployRow = &runs[i]
			break
		}
	}
	// 外接模式（无部署）：200 + 空列表 + message（不是 404，openapi 明确）。
	if deployRow == nil {
		writeJSON(w, http.StatusOK, deployLogsEmpty(taskID, attemptNo,
			"该 Task 是外接模式（无部署），没有 Deploy Pod 日志"))
		return
	}

	logsDir := filepath.Join(s.runnerDirFor(taskID, attemptNo, deployRow.Seq), "logs")
	pods := listDeployPods(logsDir)

	q := r.URL.Query()
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	limit := atoiDefault(q.Get("limit"), 200)
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	tailLines := atoiDefault(q.Get("tail_lines"), 0)
	if tailLines < 0 {
		tailLines = 0
	}
	if tailLines > 5000 {
		tailLines = 5000
	}
	if tailLines > 0 && q.Get("offset") != "" {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"offset 与 tail_lines 不可同时给出（两个窗口的行号基准不同）", nil, false)
		return
	}

	podParam := q.Get("pod")
	if podParam != "" && !podInList(pods, podParam) {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound,
			"指定的 Pod 不在该部署的 Pod 列表里", map[string]any{
				"reason": "pod_not_found", "pod": podParam, "available_pods": pods,
			}, false)
		return
	}
	if podParam == "" && len(pods) >= 2 {
		// ≥2 Pod 且未指定 pod：端点不会替你挑一个（openapi 409 语义）。
		fail(w, r, http.StatusConflict, domain.CodeConflict,
			"该部署有多个 Pod，请用 ?pod= 指定要读哪个",
			map[string]any{"reason": "pod_ambiguous", "available_pods": pods}, false)
		return
	}
	if podParam == "" && len(pods) == 1 {
		podParam = pods[0]
	}
	if podParam == "" {
		// 0 Pod：排队中 / Pod 已回收 → 200 + message。
		writeJSON(w, http.StatusOK, deployLogsEmpty(taskID, attemptNo,
			"当前没有 Deploy Pod（排队中或 Pod 已回收）"))
		return
	}

	files := deployPodLogSegments(logsDir, podParam)
	if len(files) == 0 {
		writeJSON(w, http.StatusOK, deployLogsEmpty(taskID, attemptNo,
			"Pod "+podParam+" 的日志文件尚未落盘"))
		return
	}

	var lines []string
	if tailLines > 0 {
		lines = tailLinesOf(files, tailLines)
	} else {
		lines = linesFromOffset(files, offset, limit)
	}
	truncated := false
	next := 0
	if tailLines == 0 {
		total := countLines(files)
		next = offset + len(lines)
		truncated = next < total
		if !truncated {
			next = 0
		}
	}
	window := "full"
	if tailLines > 0 {
		window = "tail"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":     taskID,
		"attempt_no":  attemptNo,
		"pods":        deployLogPods(pods, logsDir),
		"entries":     []map[string]any{{"pod": podParam, "container": "", "offset": offset, "line_count": len(lines), "text": strings.Join(lines, "\n")}},
		"gaps":        []any{},
		"window":      window,
		"truncated":   truncated,
		"next_offset": next,
	})
}

// deployLogsEmpty 是"没有日志可读"的 200 响应（排队期 / 外接模式 / Pod 已回收）。
func deployLogsEmpty(taskID string, attemptNo int, message string) map[string]any {
	return map[string]any{
		"task_id":     taskID,
		"attempt_no":  attemptNo,
		"pods":        []any{},
		"entries":     []any{},
		"gaps":        []any{},
		"window":      "full",
		"truncated":   false,
		"next_offset": 0,
		"message":     message,
	}
}

// listDeployPods 列出该部署已有的 Pod 日志文件名（<pod>.log / <pod>.N.log 归并成 pod 名）。
func listDeployPods(logsDir string) []string {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		name := e.Name()
		// 滚档段（<pod>.N.log）归并回 pod 名。
		if i := strings.LastIndexByte(name, '.'); i > 0 && isSegmentSuffix(name[i+1:]) {
			name = name[:i]
		}
		seen[strings.TrimSuffix(name, ".log")] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isSegmentSuffix 报告是不是滚档的数字段（"1"、"2"…）。
func isSegmentSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// podInList 报告 pod 是否在当前 Pod 列表里。
func podInList(pods []string, pod string) bool {
	for _, p := range pods {
		if p == pod {
			return true
		}
	}
	return false
}

// deployPodLogSegments 返回该 Pod 的日志分段文件（当前段在前：*.N.log 降序 + <pod>.log）。
func deployPodLogSegments(logsDir, pod string) []string {
	var segs []string
	for i := 99; i >= 1; i-- {
		p := filepath.Join(logsDir, pod+"."+strconv.Itoa(i)+".log")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			segs = append(segs, p)
		}
	}
	base := filepath.Join(logsDir, pod+".log")
	if st, err := os.Stat(base); err == nil && !st.IsDir() {
		segs = append(segs, base)
	}
	return segs
}

// deployLogPods 投影 Pod 页签数据（openapi DeployLogPod）。
func deployLogPods(pods []string, logsDir string) []map[string]any {
	out := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		size := int64(0)
		for _, f := range deployPodLogSegments(logsDir, p) {
			if st, err := os.Stat(f); err == nil {
				size += st.Size()
			}
		}
		out = append(out, map[string]any{
			"name": p, "ready": false, "restarts": 0,
			"size_bytes": size,
		})
	}
	return out
}

// linesFromOffset 从分段拼接流读 offset 起的 limit 行。
func linesFromOffset(files []string, offset, limit int) []string {
	lines := readAllLines(files)
	if offset >= len(lines) {
		return []string{}
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	return lines[offset:end]
}

// tailLinesOf 取末尾 N 行。
func tailLinesOf(files []string, n int) []string {
	lines := readAllLines(files)
	if n >= len(lines) {
		return lines
	}
	return lines[len(lines)-n:]
}

// countLines 统计分段拼接流的总行数。
func countLines(files []string) int { return len(readAllLines(files)) }

// readAllLines 把分段文件按顺序拼成行数组（部署日志按行分页，行号坐标系就是它）。
func readAllLines(files []string) []string {
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" && len(out) > 0 && out[len(out)-1] == "" {
				continue
			}
			out = append(out, line)
		}
	}
	return out
}

var _ = json.Marshal
