package api

import (
	"errors"
	"io"
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

// runner 日志端点（07 §5.2、C-7.5）：日志明文（不做脱敏，K-5），单次 ≤64 KiB；
// 翻页用 offset + limit，同一坐标系由 X-Log-Offset / X-Log-Coverage 交代。
// 日志不走 SSE（C-9.2）。
//
// 两条路由共用同一实现：
//   - GET /tasks/{task_id}/runners/{seq}/logs           （缺省最新 attempt，?attempt=）
//   - GET /tasks/{task_id}/attempts/{attempt_no}/runners/{seq}/logs

// maxLogChunk 是单次日志返回上限（07 §5.2：≤64 KiB）。
const maxLogChunk = 64 << 10

// handleTaskRunnerLogs 是 Task 级快捷定位（?attempt= 缺省最新）。
func (s *Server) handleTaskRunnerLogs(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.serveRunnerLogs(w, r, rr)
}

// handleAttemptRunnerLogs 是 attempt 级权威定位。
func (s *Server) handleAttemptRunnerLogs(w http.ResponseWriter, r *http.Request) {
	_, rr, err := s.runnerKeyFromURL(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.serveRunnerLogs(w, r, rr)
}

// serveRunnerLogs 读 <storage-root>/<task_id>/attempts/<n>/runners/<seq>/logs/ 下的日志。
func (s *Server) serveRunnerLogs(w http.ResponseWriter, r *http.Request, rr *store.RunnerRun) {
	q := r.URL.Query()

	stream := q.Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	if stream != "stdout" && stream != "stderr" {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"stream 只能是 stdout|stderr", nil, false)
		return
	}

	offset := int64(atoiDefault(q.Get("offset"), 0))
	if offset < 0 {
		offset = 0
	}
	limit := atoiDefault(q.Get("limit"), maxLogChunk)
	if limit <= 0 || limit > maxLogChunk {
		limit = maxLogChunk
	}

	dir := filepath.Join(s.runnerDirFor(rr.TaskID, rr.AttemptNo, rr.Seq), "logs")
	files := pickLogFiles(dir, stream)
	if len(files) == 0 {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound, "日志尚未产出", map[string]any{
			"reason":     "log_not_ready",
			"task_id":    rr.TaskID,
			"attempt_no": rr.AttemptNo,
			"runner_seq": rr.Seq,
			"stream":     stream,
		}, true)
		return
	}

	data, start, err := readLogFiles(files, offset, limit, 0)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// X-Log-Offset = 本次返回的**结束处**字节偏移（openapi headers 定义）：
	// 客户端下次直接用 offset= 这个值续读，不重读也不跳过。
	w.Header().Set("X-Log-Offset", strconv.FormatInt(start+int64(len(data)), 10))
	// 日志区间需要采集时间；拿不到就不编，留空。
	w.Header().Set("X-Log-Coverage", "")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// pickLogFiles 按优先级选日志文件：
// stdout 模式：stdout.log 存在就只用它；否则按文件名排序返回全部 *.log；
// stderr 模式：只认 stderr.log。
func pickLogFiles(dir, stream string) []string {
	if stream == "stderr" {
		p := filepath.Join(dir, "stderr.log")
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return []string{p}
		}
		return nil
	}
	preferred := filepath.Join(dir, "stdout.log")
	if info, err := os.Stat(preferred); err == nil && !info.IsDir() && info.Size() > 0 {
		return []string{preferred}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(dir, n))
	}
	return out
}

// readLogFiles 把一组文件当成一条连续的字节流来读。
// tail>0 时先从尾部找最后 N 行的起点，再从该起点读 limit 字节。
func readLogFiles(files []string, offset int64, limit, tail int) ([]byte, int64, error) {
	sizes := make([]int64, len(files))
	var total int64
	for i, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			sizes[i] = 0
			continue
		}
		sizes[i] = info.Size()
		total += info.Size()
	}
	if total == 0 {
		return nil, 0, nil
	}

	start := offset
	if tail > 0 {
		start = tailStartOffset(files, sizes, tail)
		if start < 0 {
			start = 0
		}
	}
	if start > total {
		start = total
	}
	want := int64(limit)
	if start+want > total {
		want = total - start
	}
	if want <= 0 {
		return nil, start, nil
	}

	buf := make([]byte, want)
	var read int64
	remain := start + want // 绝对终点
	var pos int64
	for i, f := range files {
		fileSize := sizes[i]
		if fileSize == 0 {
			continue
		}
		fileStart, fileEnd := pos, pos+fileSize
		pos = fileEnd
		if fileEnd <= start || fileStart >= remain {
			continue
		}
		from := int64(0)
		if start > fileStart {
			from = start - fileStart
		}
		to := fileSize
		if remain < fileEnd {
			to = remain - fileStart
		}
		fh, err := os.Open(f)
		if err != nil {
			return nil, start, err
		}
		if _, err := fh.Seek(from, io.SeekStart); err != nil {
			_ = fh.Close()
			return nil, start, err
		}
		n, err := io.ReadFull(fh, buf[read:read+(to-from)])
		_ = fh.Close()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, start, err
		}
		read += int64(n)
		if read >= want {
			break
		}
	}
	return buf[:read], start, nil
}

// tailStartOffset 从尾部往前扫，找到最后 n 行的起点（跨文件连续字节流）。
func tailStartOffset(files []string, sizes []int64, n int) int64 {
	var total int64
	for _, s := range sizes {
		total += s
	}
	if total == 0 {
		return 0
	}
	// 只按行数找起点：从尾部按 8KiB 块回读，数到 n 个换行即停。
	const chunk = 8 << 10
	newlines := 0
	var offset int64 = total
	for offset > 0 {
		size := int64(chunk)
		if offset < size {
			size = offset
		}
		start := offset - size
		buf := make([]byte, size)
		if _, err := readAtVirtual(files, sizes, start, buf); err != nil {
			return 0
		}
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				newlines++
				if newlines >= n+1 {
					return start + int64(i) + 1
				}
			}
		}
		offset = start
	}
	return 0
}

// readAtVirtual 从虚拟拼接流里读 len(buf) 字节到 buf。
func readAtVirtual(files []string, sizes []int64, offset int64, buf []byte) (int, error) {
	var read int
	var pos int64
	remain := offset + int64(len(buf))
	for i, f := range files {
		size := sizes[i]
		if size == 0 {
			continue
		}
		fileStart, fileEnd := pos, pos+size
		pos = fileEnd
		if fileEnd <= offset || fileStart >= remain {
			continue
		}
		from := int64(0)
		if offset > fileStart {
			from = offset - fileStart
		}
		to := size
		if remain < fileEnd {
			to = remain - fileStart
		}
		fh, err := os.Open(f)
		if err != nil {
			return read, err
		}
		if _, err := fh.Seek(from, io.SeekStart); err != nil {
			_ = fh.Close()
			return read, err
		}
		n, err := io.ReadFull(fh, buf[read:read+int(to-from)])
		_ = fh.Close()
		read += n
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return read, err
		}
		if int64(read) >= int64(len(buf)) {
			break
		}
	}
	return read, nil
}

var _ = chi.URLParam
