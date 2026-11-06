package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// metricsRegistry 是手写的 Prometheus 指标登记处（不引 client_golang）：
// 计数在进程内原子累加，采样时再拼文本 exposition（07 §5.5）。
type metricsRegistry struct {
	mu   sync.RWMutex
	http map[httpKey]*atomic.Int64
}

type httpKey struct {
	method string
	code   int
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{http: map[httpKey]*atomic.Int64{}}
}

// 拒绝审计管道的指标名（issue #40-6）。名字只在这里定义一次：
// exposition 与测试都引用同一组常量，改名后测试会跟着变（不会"改了名测试还绿"）。
const (
	metricAuditRejectedWritten    = "testplane_audit_rejected_written_total"
	metricAuditRejectedDropped    = "testplane_audit_rejected_dropped_total"
	metricAuditRejectedFailed     = "testplane_audit_rejected_failed_total"
	metricAuditRejectedSuppressed = "testplane_audit_rejected_suppressed_total"
)

func (m *metricsRegistry) observeHTTP(method string, code int) {
	k := httpKey{method, code}
	m.mu.RLock()
	c := m.http[k]
	m.mu.RUnlock()
	if c != nil {
		c.Add(1)
		return
	}
	m.mu.Lock()
	c = m.http[k]
	if c == nil {
		c = &atomic.Int64{}
		m.http[k] = c
	}
	m.mu.Unlock()
	c.Add(1)
}

func (m *metricsRegistry) httpSnapshot() map[httpKey]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[httpKey]int64, len(m.http))
	for k, v := range m.http {
		out[k] = v.Load()
	}
	return out
}

// middlewareMetrics 累计 method+status 维度的请求数（复用访问日志的 statusRecorder）。
func (s *Server) middlewareMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w}
		}
		next.ServeHTTP(rec, r)
		code := rec.status
		if code == 0 {
			code = http.StatusOK
		}
		s.metrics.observeHTTP(r.Method, code)
	})
}

// handleMetrics 是 GET /metrics（不带 /api/v1 前缀，也不参与 SPA fallback）。
// 文本格式手写：DB 采样失败只跳过对应指标，绝不让 /metrics 500。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	now := s.now()

	// ---- HTTP 请求计数 ----
	b.WriteString("# HELP testplane_http_requests_total HTTP 请求总数（按 method/status 分组）。\n")
	b.WriteString("# TYPE testplane_http_requests_total counter\n")
	snap := s.metrics.httpSnapshot()
	keys := make([]httpKey, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].code < keys[j].code
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "testplane_http_requests_total{method=%q,code=%q} %d\n",
			k.method, strconv.Itoa(k.code), snap[k])
	}

	// ---- DB 计数（采样失败只跳过这一组） ----
	if stats, err := s.st.MetricsSnapshot(r.Context()); err == nil {
		writeSeriesMetric(&b, "testplane_runs_total", "Run 数量（按状态）。", "status",
			stats.RunsByStatus, runStatusSeries)
		writeSeriesMetric(&b, "testplane_tasks_total", "Task 数量（按状态）。", "status",
			stats.TasksByStatus, taskStatusSeries)
		b.WriteString("# HELP testplane_attempts_active 进行中的 Attempt 数（preparing/running）。\n")
		b.WriteString("# TYPE testplane_attempts_active gauge\n")
		fmt.Fprintf(&b, "testplane_attempts_active %d\n", stats.AttemptsActive)
		b.WriteString("# HELP testplane_endpoint_leases 当前端点租约数（互斥槽位占用）。\n")
		b.WriteString("# TYPE testplane_endpoint_leases gauge\n")
		fmt.Fprintf(&b, "testplane_endpoint_leases %d\n", stats.Leases)
		writeSeriesMetric(&b, "testplane_queue_waiting", "排队中的 Task 数（按 waiting_reason）。",
			"reason", stats.WaitingByReason, waitingReasonSeries)
	} else {
		b.WriteString("# testplane_db_metrics_error 1（采样失败，只影响 DB 类指标）\n")
	}

	// ---- 接入层拒绝审计管道（issue #40-6：异步有界队列 + 401 聚合）----
	rejectedWritten, rejectedDropped, rejectedFailed, rejectedSuppressed := auditHub.stats()
	b.WriteString("# HELP " + metricAuditRejectedWritten + " 已写入 audit_log 的拒绝审计条数（异步队列消费成功）。\n")
	b.WriteString("# TYPE " + metricAuditRejectedWritten + " counter\n")
	fmt.Fprintf(&b, "%s %d\n", metricAuditRejectedWritten, rejectedWritten)
	b.WriteString("# HELP " + metricAuditRejectedDropped + " 因队列/聚合表满而被丢弃的拒绝审计条数（不阻塞请求）。\n")
	b.WriteString("# TYPE " + metricAuditRejectedDropped + " counter\n")
	fmt.Fprintf(&b, "%s %d\n", metricAuditRejectedDropped, rejectedDropped)
	b.WriteString("# HELP " + metricAuditRejectedFailed + " 异步写 audit_log 失败的条数。\n")
	b.WriteString("# TYPE " + metricAuditRejectedFailed + " counter\n")
	fmt.Fprintf(&b, "%s %d\n", metricAuditRejectedFailed, rejectedFailed)
	b.WriteString("# HELP " + metricAuditRejectedSuppressed + " 401 聚合压掉的重复条数（窗口内同 IP）。\n")
	b.WriteString("# TYPE " + metricAuditRejectedSuppressed + " counter\n")
	fmt.Fprintf(&b, "%s %d\n", metricAuditRejectedSuppressed, rejectedSuppressed)

	b.WriteString("# HELP testplane_up 进程存活（恒为 1）。\n")
	b.WriteString("# TYPE testplane_up gauge\n")
	b.WriteString("testplane_up 1\n")
	b.WriteString("# HELP testplane_metrics_scrape_timestamp_seconds 本次采样时间戳。\n")
	b.WriteString("# TYPE testplane_metrics_scrape_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "testplane_metrics_scrape_timestamp_seconds %d\n", now.Unix())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// 指标里的状态维度**全量预置**（即使计数为 0 也输出），让看板不必处理"序列时有时无"。
// 终态部分必须走 domain 的单一真源：手抄一份会在新增状态时静默漏掉（issue #45-5）。
var (
	runStatusSeries = append(
		[]string{"submitted", "queued", "deploying", "testing", "cleanup", "cancelling"},
		sortedCopy(domain.TerminalRunStatusStrings())...,
	)
	taskStatusSeries = append(
		[]string{"pending", "queued", "preparing", "running"},
		sortedCopy(domain.TerminalTaskStatusStrings())...,
	)
	waitingReasonSeries = []string{
		"precheck_gate", "slot_waiting", "global_limit",
		"queue_full", "insufficient_resources", "gang_pending",
	}
)

// sortedCopy 复制并排序（domain 返回的顺序不是契约，指标序列要稳定）。
func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// writeSeriesMetric 输出 `<name>{<label>="<v>"} n`。
// keys 非 nil 时先按给定顺序补齐（看板要求 6 个 waiting_reason 全在）。
func writeSeriesMetric(b *strings.Builder, name, help, label string, counts map[string]int, keys []string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	if keys == nil {
		names := make([]string, 0, len(counts))
		for k := range counts {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, counts[k])
		}
		return
	}
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, counts[k])
	}
}
