package api

// 管理面（07 §5.4 admin）的公共零件：JSON 请求体、名字校验、审计写入。
//
// 每个端点都遵守同一套约定：
//   - 成功：先落库，再写一条 audit_log（action/object_type/object_id/detail），
//     审计写失败只记日志，不把已生效的变更报成失败；
//   - 失败（校验 / 冲突 / 不存在 / 轮换校验不通过）：按 C-10「失败也记录尝试」
//     写一条 result=rejected 的审计；畸形 JSON 不记（没有可归属的对象）；
//   - 响应体绝不包含 Secret 值或 kubeconfig 内容（06 §4.4、C-10）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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

// appendAudit 写一条审计；失败只记 warn（不阻断已生效的变更）。
func (s *Server) appendAudit(r *http.Request, action, objectType, objectID string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	err := s.st.AppendAudit(r.Context(), store.AuditEntry{
		Actor:      Actor(r.Context()),
		Role:       Role(r.Context()),
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		Detail:     detail,
	})
	if err != nil {
		s.logger.Warn("写审计失败", "err", err, "action", action,
			"object_type", objectType, "object_id", objectID,
			"request_id", RequestID(r.Context()))
	}
}

// auditOK 记一次成功的写操作。
func (s *Server) auditOK(r *http.Request, action, objectType, objectID string, detail map[string]any) {
	d := map[string]any{"result": "ok"}
	for k, v := range detail {
		d[k] = v
	}
	s.appendAudit(r, action, objectType, objectID, d)
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
	s.appendAudit(r, action, objectType, objectID, d)
	fail(w, r, status, code, message, detail, retryable)
}

// rejectValidation 是校验失败（400 VALIDATION_ERROR）的快捷方式。
func (s *Server) rejectValidation(w http.ResponseWriter, r *http.Request,
	action, objectType, objectID, message string, detail map[string]any) {
	s.rejectWrite(w, r, action, objectType, objectID,
		http.StatusBadRequest, domain.CodeValidationError, message, detail, false)
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
