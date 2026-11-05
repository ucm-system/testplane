package api

// 管理面（07 §5.4 admin）的公共零件：JSON 请求体、名字校验、审计写入。
//
// 每个端点都遵守同一套约定：
//   - 成功：业务变更与 audit_log 在**同一个事务**里提交（store.InAuditedTx），
//     审计失败 → 整体回滚 + 5xx（PR #33 #6：不允许"改了没记"）；
//   - 失败（校验 / 冲突 / 不存在 / 轮换校验不通过 / 中间件拦截）：按 C-10
//     「失败也记录尝试」写一条 result=rejected 的审计，写入上下文与客户端连接解耦；
//     畸形 JSON 不记（没有可归属的对象）；
//   - 响应体绝不包含 Secret 值或 kubeconfig 内容（06 §4.4、C-10）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
