package api

// Secret 引用（07 §5.3 `GET /secrets` + §5.4 `/admin/secrets*`）。
//
// 红线（06 §4.4、C-10）：
//   - 平台只保存"从哪个 namespace 的哪个 Secret 的哪个 key 取"；
//   - **绝不**接收、读取、回显 Secret 值：请求里带 value 一律 400，
//     响应里没有任何值字段（GET /secrets 连 namespace/secret_name/key 都不给，只回名称/说明）；
//   - `GET /secrets` 是 operator 及以上（07 §2.3 / C-8.2）；
//   - rotate 只做一次**存在性校验**（get 已登记的那一个 Secret 对象、看 `data[key]` 非空）
//     + 审计「verified / checked_at」。这是 D-55 给 06 §4.4 的窄例外：只允许对
//     `secret_refs` 里登记的具体 (namespace, secret_name) 做 get（RBAC 用 resourceNames 限定），
//     **不读值当数据使用、不打印、不落库、不回显**；无权限/不可达一律 503，绝不假装成功。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

const (
	// secretRefNameMaxLen 是引用名上限。
	secretRefNameMaxLen = 128
	// secretObjectNameMaxLen 是 K8s Secret 名上限（DNS subdomain 253）。
	secretObjectNameMaxLen = 253
	// secretKeyMaxLen 是 Secret data key 的实际上限。
	secretKeyMaxLen = 253
	// rotateCheckTimeout 是 rotate 校验的 K8s API 超时（不阻塞管理面太久）。
	rotateCheckTimeout = 10 * time.Second
)

// secretRefDTO 是 admin 视角的 Secret 引用投影（openapi SecretRef）。
// **没有 value 字段**，而且永远不会有。
type secretRefDTO struct {
	Name        string    `json:"name"`
	Namespace   string    `json:"namespace"`
	SecretName  string    `json:"secret_name"`
	Key         string    `json:"key"`
	Description *string   `json:"description"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

func toSecretRefDTO(r store.SecretRef) secretRefDTO {
	return secretRefDTO{
		Name: r.Name, Namespace: r.Namespace, SecretName: r.SecretName, Key: r.Key,
		Description: r.Description, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.UTC(),
	}
}

// secretNameDTO 是 GET /api/v1/secrets 的 operator 投影（openapi SecretName）：
// 只有引用名与说明——哪个 namespace / Secret / key 属于内部拓扑，不给调用方（07 §2.3）。
type secretNameDTO struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

// secretRefCreateRequest 是 POST /admin/secrets 的请求体。
// Value / SecretValue / Data 只为显式拒绝而存在（平台不接收 Secret 值）。
type secretRefCreateRequest struct {
	Name        string         `json:"name"`
	Namespace   string         `json:"namespace"`
	SecretName  string         `json:"secret_name"`
	Key         string         `json:"key"`
	Description *string        `json:"description"`
	Value       *string        `json:"value"`
	SecretValue *string        `json:"secret_value"`
	Data        map[string]any `json:"data"`
}

// handleListSecretRefs 是 GET /api/v1/secrets（operator 及以上）：只返回引用名与说明。
func (s *Server) handleListSecretRefs(w http.ResponseWriter, r *http.Request) {
	refs, err := s.listAllSecretRefs(r.Context())
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]secretNameDTO, 0, len(refs))
	for _, ref := range refs {
		out = append(out, secretNameDTO{Name: ref.Name, Description: ref.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleAdminListSecretRefs 是 GET /api/v1/admin/secrets：游标分页的引用列表（不含值）。
func (s *Server) handleAdminListSecretRefs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, next, err := s.st.ListSecretRefs(r.Context(), q.Get("cursor"), atoiDefault(q.Get("limit"), 50))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]secretRefDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toSecretRefDTO(item))
	}
	writeJSON(w, http.StatusOK, paged[secretRefDTO]{
		Items: out, NextCursor: cursorPtr(next), HasMore: next != "",
	})
}

// handleAdminCreateSecretRef 是 POST /api/v1/admin/secrets（201）。
func (s *Server) handleAdminCreateSecretRef(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "register", "secret_ref"

	var req secretRefCreateRequest
	body, ok := s.decodeAdminJSONRaw(w, r, &req)
	if !ok {
		return
	}
	if key := jsonHasAnyKey(body, "value", "secret_value", "data"); key != "" ||
		req.Value != nil || req.SecretValue != nil || req.Data != nil {
		s.rejectValidation(w, r, action, objectType, strings.TrimSpace(req.Name),
			"平台不接收 Secret 值：只登记「namespace / secret_name / key」三要素，值只在 Secret Store 侧维护",
			map[string]any{"rejected_field": key})
		return
	}
	name, ok := validAdminName(req.Name, secretRefNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"name 不能为空、不得超过 128 字节且不能含控制字符", map[string]any{"field": "name"})
		return
	}
	namespace, ok := validAdminName(req.Namespace, namespaceMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"namespace 不能为空且不得超过 63 字节", map[string]any{"field": "namespace"})
		return
	}
	secretName, ok := validAdminName(req.SecretName, secretObjectNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"secret_name 不能为空且不得超过 253 字节", map[string]any{"field": "secret_name"})
		return
	}
	key, ok := validAdminName(req.Key, secretKeyMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"key 不能为空且不得超过 253 字节", map[string]any{"field": "key"})
		return
	}

	ref := &store.SecretRef{
		Name: name, Namespace: namespace, SecretName: secretName, Key: key,
		Description: optionalText(req.Description), CreatedBy: Actor(ctx),
	}
	// 审计只记定位信息（这是设计允许保存的内容），不记任何值；与登记同事务。
	err := s.st.InAuditedTx(ctx, s.auditEntry(r, action, objectType, name, map[string]any{
		"namespace": namespace, "secret_name": secretName, "key": key,
	}), func(tx pgx.Tx) error {
		return s.st.CreateSecretRefTx(ctx, tx, ref)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusConflict, domain.CodeConflict,
				"Secret 引用名已存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, toSecretRefDTO(*ref))
}

// handleAdminRotateSecret 是 POST /api/v1/admin/secrets/{name}/rotate（200）。
//
// 语义（C-10、UC-18、D-55）：值由 admin 在 Secret Store 侧更新；本端点对已登记的
// 那一个 (namespace, secret_name) 做一次**存在性校验**（Secret 存在、该 key 非空），
// 并把 `verified` / `checked_at` 写进审计（action=secret_rotated）。
// 值不读出来当数据用、不打印、不落库、不回显；无权限/不可达 → 503 可重试。
// 已运行 Job 的环境变量不可变，轮换只影响新 Attempt；失败不改登记行。
func (s *Server) handleAdminRotateSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "secret_rotated", "secret_ref"

	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if name == "" {
		s.rejectValidation(w, r, action, objectType, "",
			"缺少引用名", map[string]any{"param": "name"})
		return
	}
	ref, err := s.st.GetSecretRef(ctx, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"Secret 引用不存在", map[string]any{"name": name}, false)
			return
		}
		s.failWriteErr(w, r, action, objectType, name, err)
		return
	}

	probe, err := s.secretProbe()
	if err != nil {
		s.logger.Warn("Secret 校验器不可用", "err", err, "request_id", RequestID(ctx))
		s.rejectWrite(w, r, action, objectType, name,
			http.StatusServiceUnavailable, domain.CodeExecutorUnavailable,
			"无法连接集群校验 Secret（平台可能没有 secrets get 权限）",
			map[string]any{"reason": "k8s_unavailable", "verified": false}, true)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, rotateCheckTimeout)
	defer cancel()
	exists, keyNonEmpty, err := probe.CheckSecretKey(pctx, ref.Namespace, ref.SecretName, ref.Key)
	if err != nil {
		// 不清空引用、不报成功：轮换校验失败只告警（C-10）。
		// 区分 forbidden 与不可达：前者说明 D-55 的 resourceNames Role 没 apply。
		reason := "k8s_unavailable"
		if errors.Is(err, errSecretForbidden) {
			reason = "forbidden"
		}
		s.logger.Warn("轮换校验读 Secret 失败", "err", err, "secret_ref", name,
			"namespace", ref.Namespace, "reason", reason, "request_id", RequestID(ctx))
		s.rejectWrite(w, r, action, objectType, name,
			http.StatusServiceUnavailable, domain.CodeExecutorUnavailable,
			"无法读取目标 Secret（集群不可达或无权限）",
			map[string]any{"reason": reason, "verified": false}, true)
		return
	}
	if !exists {
		s.rejectWrite(w, r, action, objectType, name,
			http.StatusNotFound, domain.CodeNotFound,
			"目标 Secret 在集群里不存在",
			map[string]any{"namespace": ref.Namespace, "secret_name": ref.SecretName, "verified": false}, false)
		return
	}
	if !keyNonEmpty {
		s.rejectValidation(w, r, action, objectType, name,
			"目标 Secret 存在，但该 key 不存在或为空",
			map[string]any{"namespace": ref.Namespace, "secret_name": ref.SecretName, "key": ref.Key, "verified": false})
		return
	}

	checkedAt := s.now().UTC()
	// rotate 没有业务状态可回滚，但审计写不进去就不能报成功：
	// 审计上下文与客户端连接解耦（断连也要留下 verified 记录）。
	err = s.appendAuditDetached(ctx, s.auditEntry(r, action, objectType, name, map[string]any{
		"namespace": ref.Namespace, "secret_name": ref.SecretName, "key": ref.Key,
		"verified": true, "checked_at": checkedAt.Format(time.RFC3339),
	}))
	if err != nil {
		s.failAuditTx(w, r, action, objectType, name, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rotated": true, "verified": true, "checked_at": checkedAt,
	})
}

// listAllSecretRefs 拉全量引用（引用表是配置级数据，量级很小）。
// GET /secrets 不分页，但仍走同一套 keyset 循环，避免以后量大时静默截断。
func (s *Server) listAllSecretRefs(ctx context.Context) ([]store.SecretRef, error) {
	out := make([]store.SecretRef, 0, 16)
	cursor := ""
	for {
		batch, next, err := s.st.ListSecretRefs(ctx, cursor, 200)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}
