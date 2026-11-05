package api

// 管理面（07 §5.4 admin）的公共零件：JSON 请求体、名字校验、审计写入。
//
// 每个端点都遵守同一套约定：
//   - 成功：业务变更与 audit_log 在**同一个事务**里提交（store.InAuditedTx），
//     审计失败 → 整体回滚 + 5xx（PR #33 #6：不允许"改了没记"）；
//   - 失败（校验 / 冲突 / 不存在 / 轮换校验不通过 / 中间件拦截）：按 C-10
//     「失败也记录尝试」写一条 result=rejected 的审计，写入上下文与客户端连接解耦；
//     畸形 JSON 不记（没有可归属的对象）；
//   - **接入层拒绝（401/403/CSRF）走异步有界管道**（issue #40-6）：401 按
//     (remote IP, 1min 窗口) 聚合成一条，队列满丢弃并计数，绝不让 DB 拖慢拒绝路径；
//   - 响应体绝不包含 Secret 值或 kubeconfig 内容（06 §4.4、C-10）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// ---- 请求体 --------------------------------------------------------------

// decodeAdminJSON 读并解 JSON 请求体（上限与提交端点一致：1 MiB）。
// 失败时已经写好错误响应，返回 false。
func (s *Server) decodeAdminJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	_, ok := s.decodeAdminJSONRaw(w, r, v)
	return ok
}

// decodeAdminJSONRaw 与 decodeAdminJSON 相同，但额外返回原始 body，
// 供"禁止字段"检查（例如 POST /admin/clusters 里混进 kubeconfig 内容）。
func (s *Server) decodeAdminJSONRaw(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, r, http.StatusRequestEntityTooLarge, domain.CodeValidationError,
				"请求体超过 1 MiB", map[string]any{"limit_bytes": maxBodyBytes}, false)
			return nil, false
		}
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "读取请求体失败", nil, false)
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "请求体不能为空", nil, false)
		return nil, false
	}
	if err := json.Unmarshal(body, v); err != nil {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"请求体不是合法 JSON", map[string]any{"reason": err.Error()}, false)
		return nil, false
	}
	return body, true
}

// jsonHasAnyKey 返回 body 顶层命中的第一个 key（没有则空串）。
// 用于显式拒绝"平台不接收的东西"：kubeconfig 内容、Secret 值。
func jsonHasAnyKey(body []byte, keys ...string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return k
		}
	}
	return ""
}

// ---- 校验 -----------------------------------------------------------------

// validAdminName 校验管理面对象名：去首尾空白后非空、≤max 字节、不含控制字符。
// 名字大小写敏感（与 seed 的 k8s-npu 一致），不做 lower。
func validAdminName(v string, max int) (string, bool) {
	name := strings.TrimSpace(v)
	if name == "" || len(name) > max {
		return name, false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return name, false
		}
	}
	return name, true
}

// ---- 审计 -----------------------------------------------------------------
//
// 两条路径（PR #33 #6）：
//   1) **成功写操作**：业务变更与审计放进**同一个事务**（store.InAuditedTx），
//      审计失败 → 整体回滚 + 5xx。绝不能"先提交再补记"。
//   2) **拒绝 / 中间件拦截**：没有业务变更可回滚，用 detachAuditCtx 异步补记——
//      上下文与客户端连接解耦（context.WithoutCancel + 2s 超时），
//      客户端断连也会留下"谁被拒了"。

// auditTimeout 是拒绝类审计的写入上限（不依赖请求生命周期，也不无限阻塞）。
const auditTimeout = 2 * time.Second

// detachAuditCtx 返回与客户端断连解耦的审计上下文（保留 ctx 里的身份/请求号）。
func detachAuditCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
}

// auditEntry 组装"成功写操作"的审计条目（detail 里统一带 result=ok）。
// 由 handler 交给 store.InAuditedTx，与业务变更同事务提交。
func (s *Server) auditEntry(r *http.Request, action, objectType, objectID string, detail map[string]any) store.AuditEntry {
	d := map[string]any{"result": "ok"}
	for k, v := range detail {
		d[k] = v
	}
	return store.AuditEntry{
		Actor:      Actor(r.Context()),
		Role:       Role(r.Context()),
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		Detail:     d,
	}
}

// appendAuditDetached 写一条审计并返回错误：用于"审计本身是唯一副作用"的写操作
// （例如 rotate：没有业务状态可回滚，但审计写不进去就不能报成功）。
func (s *Server) appendAuditDetached(ctx context.Context, e store.AuditEntry) error {
	actx, cancel := detachAuditCtx(ctx)
	defer cancel()
	return s.st.AppendAudit(actx, e)
}

// auditRejected 记一条**拒绝/拦截**审计（best-effort；失败只记日志，不再影响响应）。
// 只用于 handler 内的"写了但被业务拒绝"（一次请求一条，且调用方/测试期望立即可查）；
// 接入层拒绝（401/403/CSRF）走 auditRejectedAsync / auditAuthRejectedAsync。
func (s *Server) auditRejected(ctx context.Context, actor, role, action, objectType, objectID string, detail map[string]any) {
	if s.st == nil {
		return
	}
	if actor == "" {
		actor = "anonymous"
	}
	if role == "" {
		role = "anonymous"
	}
	if detail == nil {
		detail = map[string]any{}
	}
	actx, cancel := detachAuditCtx(ctx)
	defer cancel()
	if err := s.st.AppendAudit(actx, store.AuditEntry{
		Actor: actor, Role: role, Action: action,
		ObjectType: objectType, ObjectID: objectID, Detail: detail,
	}); err != nil {
		s.logger.Warn("写拒绝审计失败", "err", err, "action", action,
			"object_type", objectType, "object_id", objectID, "request_id", RequestID(ctx))
	}
}

// ============================================================================
// 接入层拒绝审计：异步 + 有界 + 401 聚合（issue #40-6）
// ============================================================================
//
// 旧实现的问题：任何无效凭据都会在**响应前**同步写一条 audit_log——
//   - 写放大：`Authorization: Bearer x` 刷接口就能让审计表无限增长（无上限/无去重）；
//   - 拒绝路径被 DB 拖慢：DB 抖动时最慢要等 2s 才返回 401。
//
// 现在：
//   - 一个容量固定的 channel（1024）+ **单 goroutine** 消费；满了**丢弃并计数**
//     （`testplane_audit_rejected_dropped_total`），响应路径永不阻塞；
//   - 401 按 **(remote IP, 1min 窗口)** 聚合成一条，`detail.suppressed_count` 记被压掉的条数；
//   - 聚合表本身也有上限（1024 个来源 IP），超了同样丢弃+计数；
//   - handler 内的业务拒绝仍走同步 `auditRejected`（见上面的说明）。
//
// 参数（issue #40-6 裁决）：队列 1024、窗口 1min、聚合来源上限 1024、窗口每 10s 扫一次。

const (
	// auditRejectQueueCap 是拒绝审计队列容量；满了丢弃（绝不阻塞请求）。
	auditRejectQueueCap = 1024
	// authAggregateWindow 是 401 聚合窗口（issue #40-6：来源 IP + 1min）。
	authAggregateWindow = time.Minute
	// authAggregateMaxSources 是聚合表最多跟踪的来源 IP 数（防止按 IP 打爆内存）。
	authAggregateMaxSources = 1024
	// authAggregateFlushEvery 是过期窗口的扫描间隔（不必等于窗口，只影响聚合行的落库延迟）。
	authAggregateFlushEvery = 10 * time.Second
	// auditRejectWriteTimeout 是单条异步审计写入的 DB 上限（消费者自己不无限挂起）。
	auditRejectWriteTimeout = 2 * time.Second
)

type auditRejectJob struct {
	st    *store.Store
	entry store.AuditEntry
	// barrier 非空表示"排空屏障"：消费者处理到它时关闭，供测试/优雅退出等待。
	barrier chan struct{}
}

type authAggKey struct {
	st *store.Store
	ip string
}

type authAggBucket struct {
	start time.Time
	count int
	entry store.AuditEntry
}

// auditRejectHub 是进程级的拒绝审计管道。
//
// 为什么是进程级：生产一个进程一个控制面；测试里多个 Server 并存时，用 `st` 指针做
// 聚合键与投递目标，互不串库。计数器是进程累计（与 /metrics 的语义一致）。
type auditRejectHub struct {
	queue chan auditRejectJob

	written    atomic.Int64
	dropped    atomic.Int64
	failed     atomic.Int64
	suppressed atomic.Int64

	mu  sync.Mutex
	agg map[authAggKey]*authAggBucket

	// gate 只给测试用：非空时消费者先等它，便于把队列填满验证"丢弃计数"。
	gate atomic.Pointer[chan struct{}]

	startOnce sync.Once
}

var auditHub = &auditRejectHub{
	queue: make(chan auditRejectJob, auditRejectQueueCap),
	agg:   make(map[authAggKey]*authAggBucket, 16),
}

// stats 返回管道计数：(written, dropped, failed, suppressed)。
func (h *auditRejectHub) stats() (written, dropped, failed, suppressed int64) {
	return h.written.Load(), h.dropped.Load(), h.failed.Load(), h.suppressed.Load()
}

func (h *auditRejectHub) start() {
	h.startOnce.Do(func() { go h.loop() })
}

// loop 是唯一的消费者：写 DB + 定期把过期聚合窗口落库。
func (h *auditRejectHub) loop() {
	ticker := time.NewTicker(authAggregateFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case job := <-h.queue:
			if gate := h.gate.Load(); gate != nil {
				<-*gate
			}
			if job.barrier != nil {
				close(job.barrier)
				continue
			}
			h.write(job)
		case now := <-ticker.C:
			h.flushAggregates(now.UTC(), false)
		}
	}
}

func (h *auditRejectHub) write(job auditRejectJob) {
	ctx, cancel := context.WithTimeout(context.Background(), auditRejectWriteTimeout)
	defer cancel()
	if err := job.st.AppendAudit(ctx, job.entry); err != nil {
		h.failed.Add(1)
		slog.Default().Warn("异步拒绝审计写入失败",
			"err", err, "action", job.entry.Action, "object_type", job.entry.ObjectType)
		return
	}
	h.written.Add(1)
}

// noteDrop 记录丢弃并**采样**告警（首次 + 每 100 条），避免刷日志。
// 日志固定带 `dropped=true`（issue #40-6），便于告警规则直接匹配。
func (h *auditRejectHub) noteDrop(reason string, n int64) {
	total := h.dropped.Add(n)
	if total == n || total%100 < n {
		slog.Default().Warn("拒绝审计被丢弃（队列或聚合表已满）",
			"dropped", true, "reason", reason, "dropped_count", n, "dropped_total", total)
	}
}

// enqueue 投递一条非聚合拒绝审计；队列满就丢弃（绝不阻塞调用方）。
func (h *auditRejectHub) enqueue(st *store.Store, entry store.AuditEntry) {
	h.start()
	select {
	case h.queue <- auditRejectJob{st: st, entry: entry}:
	default:
		h.noteDrop("queue_full", 1)
	}
}

// aggregateAuth 把一次 401 合并进 (来源 IP, 窗口) 桶；桶满就丢弃并计数。
func (h *auditRejectHub) aggregateAuth(st *store.Store, ip string, entry store.AuditEntry) {
	h.start()
	key := authAggKey{st: st, ip: ip}
	h.mu.Lock()
	if b, ok := h.agg[key]; ok {
		b.count++
		h.mu.Unlock()
		return
	}
	if len(h.agg) >= authAggregateMaxSources {
		h.mu.Unlock()
		h.noteDrop("auth_aggregate_full", 1)
		return
	}
	h.agg[key] = &authAggBucket{start: time.Now().UTC(), count: 1, entry: entry}
	h.mu.Unlock()
}

// flushAggregates 把到期（all=true 时全部）桶合成一行投进队列。
// 聚合行的 detail 以第一条为模板，叠加 suppressed_count 与窗口信息。
func (h *auditRejectHub) flushAggregates(now time.Time, all bool) {
	h.mu.Lock()
	jobs := make([]auditRejectJob, 0, 4)
	for key, b := range h.agg {
		if !all && now.Sub(b.start) < authAggregateWindow {
			continue
		}
		detail := make(map[string]any, len(b.entry.Detail)+3)
		for k, v := range b.entry.Detail {
			detail[k] = v
		}
		detail["suppressed_count"] = b.count - 1
		detail["window_seconds"] = int(authAggregateWindow.Seconds())
		detail["source_ip"] = key.ip
		entry := b.entry
		entry.Detail = detail
		jobs = append(jobs, auditRejectJob{st: key.st, entry: entry})
		h.suppressed.Add(int64(b.count - 1))
		delete(h.agg, key)
	}
	h.mu.Unlock()
	for _, job := range jobs {
		h.enqueue(job.st, job.entry)
	}
}

// drain 把聚合桶全部落库并等待队列排空（测试与优雅退出用；ctx 超时返回错误）。
func (h *auditRejectHub) drain(ctx context.Context, st *store.Store) error {
	h.start()
	h.flushAggregates(time.Now().UTC(), true)
	done := make(chan struct{})
	select {
	case h.queue <- auditRejectJob{st: st, barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// auditRejectedAsync 投递一条 403/CSRF 拒绝审计（异步、有界；失败不影响响应）。
func (s *Server) auditRejectedAsync(actor, role, action, objectType, objectID string, detail map[string]any) {
	if s.st == nil {
		return
	}
	if actor == "" {
		actor = "anonymous"
	}
	if role == "" {
		role = "anonymous"
	}
	if detail == nil {
		detail = map[string]any{}
	}
	auditHub.enqueue(s.st, store.AuditEntry{
		Actor: actor, Role: role, Action: action,
		ObjectType: objectType, ObjectID: objectID, Detail: detail,
	})
}

// auditAuthRejectedAsync 把 401 聚合进 (remote IP, 1min 窗口)。
// detail 里固定带 result=rejected / code（401 的码）/ source_ip / suppressed_count（flush 时补）。
//
// 注：401 的错误码用附录 D 的 `UNAUTHENTICATED`（`domain.CodeUnauthenticated`）；
// 评审文字里写的 `AUTH_REQUIRED` 不是本仓库的枚举值，故这里按代码既有码。
func (s *Server) auditAuthRejectedAsync(r *http.Request, detail map[string]any) {
	if s.st == nil {
		return
	}
	ip := clientIP(r)
	d := map[string]any{
		"result": "rejected", "code": domain.CodeUnauthenticated,
		"source_ip": ip, "reason": "unauthenticated",
	}
	for k, v := range detail {
		d[k] = v
	}
	auditHub.aggregateAuth(s.st, ip, store.AuditEntry{
		Actor: "anonymous", Role: "anonymous", Action: actionAuthRejected,
		ObjectType: objectHTTPRequest, ObjectID: ip, Detail: d,
	})
}

// clientIP 取来源 IP（聚合键）。只信 RemoteAddr：X-Forwarded-For 可伪造，
// 且 Ingress 场景下 RemoteAddr 是 Ingress 地址（聚合粒度变粗但不丢审计）。
func clientIP(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		return host
	}
	return addr
}

// rejectWrite 记一次失败的写尝试（C-10），然后按统一错误模型写响应。
// 审计 detail 只放可读原因与对象标识，不放请求原文（避免把敏感内容写进审计表）。
func (s *Server) rejectWrite(w http.ResponseWriter, r *http.Request,
	action, objectType, objectID string,
	status int, code, message string, detail map[string]any, retryable bool) {
	d := map[string]any{"result": "rejected", "code": code}
	for k, v := range detail {
		if _, exists := d[k]; exists {
			continue
		}
		d[k] = v
	}
	s.auditRejected(r.Context(), Actor(r.Context()), Role(r.Context()), action, objectType, objectID, d)
	fail(w, r, status, code, message, detail, retryable)
}

// rejectValidation 是校验失败（400 VALIDATION_ERROR）的快捷方式。
func (s *Server) rejectValidation(w http.ResponseWriter, r *http.Request,
	action, objectType, objectID, message string, detail map[string]any) {
	s.rejectWrite(w, r, action, objectType, objectID,
		http.StatusBadRequest, domain.CodeValidationError, message, detail, false)
}

// failAuditTx 把"业务或审计事务失败"映射成 5xx：变更已被回滚，调用方可以重试。
// 审计失败和 DB 失败走同一个响应，避免把内部细节泄露给客户端。
func (s *Server) failAuditTx(w http.ResponseWriter, r *http.Request,
	action, objectType, objectID string, err error) {
	// 尽力留一条 rejected（审计表本身坏掉时这里也会失败，只记日志）。
	// 复用**被尝试的那个 action**（register/cancel/…），失败语义落在 detail.result/reason 上。
	s.auditRejected(r.Context(), Actor(r.Context()), Role(r.Context()), action, objectType, objectID,
		map[string]any{"result": "rejected", "reason": "tx_failed", "code": domain.CodeInternalError})
	s.logger.Error("写操作事务失败（已回滚）", "err", err, "action", action,
		"object_type", objectType, "object_id", objectID, "request_id", RequestID(r.Context()))
	fail(w, r, http.StatusInternalServerError, domain.CodeInternalError,
		"写操作未提交：审计或数据库失败，请重试",
		map[string]any{"reason": "tx_failed"}, true)
}

// failWriteErr 记录一次"未知/DB 错误"的失败写尝试（C-10：失败也记录尝试），
// 再按统一错误模型返回。与 rejectWrite 的区别：这里没有具体业务码，reason 固定 db_error。
func (s *Server) failWriteErr(w http.ResponseWriter, r *http.Request,
	action, objectType, objectID string, err error) {
	s.auditRejected(r.Context(), Actor(r.Context()), Role(r.Context()), action, objectType, objectID,
		map[string]any{"result": "rejected", "reason": "db_error", "code": domain.CodeInternalError})
	s.logger.Error("写操作失败", "err", err, "action", action,
		"object_type", objectType, "object_id", objectID, "request_id", RequestID(r.Context()))
	s.failErr(w, r, err)
}

// ---- 小工具 ---------------------------------------------------------------

// jsonRaw 把任意值序列化成 jsonb 参数；nil 返回 nil。
func jsonRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// decodeRawArray 把库里的 jsonb 解成 []json.RawMessage（坏数据按空数组处理）。
func decodeRawArray(raw json.RawMessage) []json.RawMessage {
	out := []json.RawMessage{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// decodeStringArray 把库里的 jsonb 解成 []string（坏数据按空数组处理）。
func decodeStringArray(raw json.RawMessage) []string {
	out := []string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// decodeObject 把库里的 jsonb 解成 map（坏数据按空对象处理）。
func decodeObject(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}
