package api

// 集群登记（07 §5.4：POST/PUT/DELETE /admin/clusters）。
//
// 安全边界（06 §4.4、C-10）：
//   - `clusters.kubeconfig_key` 只是**挂载目录里的文件名**，凭据内容永不入库；
//     请求里若出现内容形态（`kubeconfig` 字段、多行 YAML、路径分隔符）一律 400；
//   - 响应只回显文件名，不回显任何凭据内容；
//   - 删除仍有 Profile 引用的集群 → 409 CONFLICT + `detail.profiles[]`（03 §3.2 外键）。

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

const (
	// clusterNameMaxLen 是集群名上限。
	clusterNameMaxLen = 128
	// kubeconfigKeyMaxLen 是文件名长度上限（Linux NAME_MAX=255）。
	kubeconfigKeyMaxLen = 255
)

// clusterUpsertRequest 是 POST/PUT /admin/clusters 的请求体。
//
// Kubeconfig / KubeconfigContent / Content 三个字段**只为显式拒绝而存在**：
// 平台不接收凭据内容，收到就 400 并说明原因（而不是静默忽略）。
type clusterUpsertRequest struct {
	Name              string  `json:"name"`
	APIServer         *string `json:"api_server"`
	KubeconfigKey     *string `json:"kubeconfig_key"`
	Description       *string `json:"description"`
	Kubeconfig        *string `json:"kubeconfig"`
	KubeconfigContent *string `json:"kubeconfig_content"`
	Content           *string `json:"content"`
}

// adminClusterDTO 是 admin 写端点的响应（openapi ClusterAdmin）。
type adminClusterDTO struct {
	Name            string     `json:"name"`
	APIServer       string     `json:"api_server"`
	KubeconfigKey   string     `json:"kubeconfig_key"`
	Description     *string    `json:"description"`
	LastCheckAt     *time.Time `json:"last_check_at"`
	LastCheckResult *string    `json:"last_check_result"`
	CreatedAt       time.Time  `json:"created_at"`
}

func toAdminClusterDTO(c *store.Cluster) adminClusterDTO {
	out := adminClusterDTO{
		Name:            c.Name,
		APIServer:       c.APIServer,
		KubeconfigKey:   c.KubeconfigKey,
		Description:     c.Description,
		LastCheckResult: c.LastCheckResult,
		CreatedAt:       c.CreatedAt.UTC(),
	}
	if c.LastCheckAt != nil {
		t := c.LastCheckAt.UTC()
		out.LastCheckAt = &t
	}
	return out
}

// handleAdminCreateCluster 是 POST /api/v1/admin/clusters（201）。
func (s *Server) handleAdminCreateCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "register", "cluster"

	var req clusterUpsertRequest
	body, ok := s.decodeAdminJSONRaw(w, r, &req)
	if !ok {
		return
	}
	if key := jsonHasAnyKey(body, "kubeconfig", "kubeconfig_content", "content"); key != "" ||
		req.Kubeconfig != nil || req.KubeconfigContent != nil || req.Content != nil {
		s.rejectValidation(w, r, action, objectType, strings.TrimSpace(req.Name),
			"平台不接收 kubeconfig 内容：请把 kubeconfig 挂进 CLUSTERS_DIR，这里只登记文件名（kubeconfig_key）",
			map[string]any{"field": "kubeconfig_key", "rejected_field": key})
		return
	}
	name, ok := validAdminName(req.Name, clusterNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"name 不能为空、不得超过 128 字节且不能含控制字符", map[string]any{"field": "name"})
		return
	}
	apiServer, errDetail, detail := validateAPIServer(req.APIServer, true)
	if errDetail != "" {
		s.rejectValidation(w, r, action, objectType, name, errDetail, detail)
		return
	}
	kubeconfigKey, errDetail, detail := validateKubeconfigKey(req.KubeconfigKey, true)
	if errDetail != "" {
		s.rejectValidation(w, r, action, objectType, name, errDetail, detail)
		return
	}

	c := &store.Cluster{
		Name:          name,
		APIServer:     apiServer,
		KubeconfigKey: kubeconfigKey,
		Description:   optionalText(req.Description),
	}
	// 审计记文件名，不记内容（内容本来就不接收）；与登记同事务。
	err := s.st.InAuditedTx(ctx, s.auditEntry(r, action, objectType, name, map[string]any{
		"api_server": c.APIServer, "kubeconfig_key": c.KubeconfigKey,
	}), func(tx pgx.Tx) error {
		return s.st.Clusters.Create(ctx, tx, c)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusConflict, domain.CodeConflict,
				"集群名字已存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, toAdminClusterDTO(c))
}

// handleAdminUpdateCluster 是 PUT /api/v1/admin/clusters（200）。
// kubeconfig_key 变更必须留审计（07 §5.4、web-and-delivery §11 的轮换流程）。
func (s *Server) handleAdminUpdateCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "update", "cluster"

	var req clusterUpsertRequest
	body, ok := s.decodeAdminJSONRaw(w, r, &req)
	if !ok {
		return
	}
	if key := jsonHasAnyKey(body, "kubeconfig", "kubeconfig_content", "content"); key != "" ||
		req.Kubeconfig != nil || req.KubeconfigContent != nil || req.Content != nil {
		s.rejectValidation(w, r, action, objectType, strings.TrimSpace(req.Name),
			"平台不接收 kubeconfig 内容：请把 kubeconfig 挂进 CLUSTERS_DIR，这里只登记文件名（kubeconfig_key）",
			map[string]any{"field": "kubeconfig_key", "rejected_field": key})
		return
	}
	name, ok := validAdminName(req.Name, clusterNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"name 不能为空、不得超过 128 字节且不能含控制字符", map[string]any{"field": "name"})
		return
	}
	cur, err := s.st.Clusters.Get(ctx, nil, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"集群不存在", map[string]any{"name": name}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}

	merged := *cur
	if req.APIServer != nil {
		apiServer, errDetail, detail := validateAPIServer(req.APIServer, false)
		if errDetail != "" {
			s.rejectValidation(w, r, action, objectType, name, errDetail, detail)
			return
		}
		merged.APIServer = apiServer
	}
	if req.KubeconfigKey != nil {
		key, errDetail, detail := validateKubeconfigKey(req.KubeconfigKey, false)
		if errDetail != "" {
			s.rejectValidation(w, r, action, objectType, name, errDetail, detail)
			return
		}
		merged.KubeconfigKey = key
	}
	if req.Description != nil {
		merged.Description = optionalText(req.Description)
	}

	kubeconfigChanged := merged.KubeconfigKey != cur.KubeconfigKey
	// kubeconfig_key 变更必须留审计；与更新同事务。
	err = s.st.InAuditedTx(ctx, s.auditEntry(r, action, objectType, name, map[string]any{
		"api_server":             merged.APIServer,
		"kubeconfig_key_changed": kubeconfigChanged,
		"kubeconfig_key":         merged.KubeconfigKey,
	}), func(tx pgx.Tx) error {
		return s.st.Clusters.Update(ctx, tx, &merged)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"集群不存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, toAdminClusterDTO(&merged))
}

// handleAdminDeleteCluster 是 DELETE /api/v1/admin/clusters?name=<name>（200）。
// 仍有 Profile 引用 → 409 CONFLICT + detail.profiles[]（不是 500，也不是静默级联）。
func (s *Server) handleAdminDeleteCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "delete_cluster", "cluster"

	name, ok := validAdminName(r.URL.Query().Get("name"), clusterNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, "",
			"缺少或非法的 name 查询参数", map[string]any{"param": "name"})
		return
	}
	if _, err := s.st.Clusters.Get(ctx, nil, name); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"集群不存在", map[string]any{"name": name}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	profiles, err := s.st.Clusters.ProfilesByCluster(ctx, nil, name)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if len(profiles) > 0 {
		s.rejectWrite(w, r, action, objectType, name,
			http.StatusConflict, domain.CodeConflict,
			"仍有 Execution Profile 引用该集群，先删除或改指向这些 Profile",
			map[string]any{"cluster": name, "profiles": profiles}, false)
		return
	}
	err = s.st.InAuditedTx(ctx, s.auditEntry(r, action, objectType, name, map[string]any{}),
		func(tx pgx.Tx) error {
			return s.st.Clusters.Delete(ctx, tx, name)
		})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"集群不存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrPrecondition):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusConflict, domain.CodeConflict,
				"仍有 Execution Profile 引用该集群", map[string]any{"cluster": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}

// ---- 校验小工具 -----------------------------------------------------------

// validateAPIServer 校验 api_server：必须是 http(s) URL。
// required=false 时允许空串（PUT 未传该字段；调用方只在传了的时候进来）。
func validateAPIServer(p *string, required bool) (string, string, map[string]any) {
	if p == nil || strings.TrimSpace(*p) == "" {
		if required {
			return "", "api_server 不能为空", map[string]any{"field": "api_server"}
		}
		return "", "api_server 不能为空", map[string]any{"field": "api_server"}
	}
	v := strings.TrimSpace(*p)
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "api_server 必须是 http(s) URL（如 https://10.0.0.2:6443）",
			map[string]any{"field": "api_server"}
	}
	return v, "", nil
}

// validateKubeconfigKey 只接受**文件名**：拒绝路径分隔符、控制字符与内容形态。
// 内容形态的判据是最小集合（多行 YAML 的典型关键字 / BEGIN 块），够挡住误传。
func validateKubeconfigKey(p *string, required bool) (string, string, map[string]any) {
	v := ""
	if p != nil {
		v = strings.TrimSpace(*p)
	}
	if v == "" {
		if required {
			return "", "kubeconfig_key 不能为空：它是挂载目录里的文件名（如 cluster-01.kubeconfig）",
				map[string]any{"field": "kubeconfig_key"}
		}
		return "", "kubeconfig_key 不能为空", map[string]any{"field": "kubeconfig_key"}
	}
	if len(v) > kubeconfigKeyMaxLen {
		return "", "kubeconfig_key 过长（最多 255 字节）", map[string]any{"field": "kubeconfig_key"}
	}
	if strings.ContainsAny(v, `/\`) || v == "." || v == ".." || strings.Contains(v, "..") {
		return "", "kubeconfig_key 只能是文件名：不能包含路径分隔符或 `..`",
			map[string]any{"field": "kubeconfig_key"}
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", "kubeconfig_key 只能是文件名：不能包含换行/控制字符（拒绝 kubeconfig 内容）",
				map[string]any{"field": "kubeconfig_key"}
		}
	}
	lower := strings.ToLower(v)
	for _, marker := range []string{"begin ", "apiversion:", "clusters:", "users:", "contexts:", "kind:"} {
		if strings.Contains(lower, marker) {
			return "", "kubeconfig_key 只能是文件名：检测到 kubeconfig 内容（平台不存凭据内容）",
				map[string]any{"field": "kubeconfig_key"}
		}
	}
	return v, "", nil
}
