package api

// Execution Profile 维护（07 §5.4：POST/PUT/DELETE /admin/profiles）。
//
// 语义要点：
//   - `name` 是主键：POST 冲突 409 CONFLICT；PUT/DELETE 按 name 定位，不存在 404；
//   - `cluster` 必须已登记（clusters 表），否则 400 VALIDATION_ERROR；
//   - DELETE **允许删除仍被 Run/Task 引用的 Profile**（设计没禁止），
//     引用数放在 detail 里回报（tasks.profile 计数）；
//   - PUT 是整体更新，但请求里省略的可选字段保留旧值（避免误清空 mounts 等）；
//     `created_by` 不变，`updated_at` 由库刷新。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

const (
	// profileNameMaxLen 是 Profile 名字上限（够用即可，防止异常长的主键）。
	profileNameMaxLen = 128
	// namespaceMaxLen 是 K8s namespace 的 DNS-1123 上限。
	namespaceMaxLen = 63
	// deployTimeoutDefault / Min / Max 与 03 §3.2 的 CHECK 约束逐字一致。
	deployTimeoutDefault = 259200
	deployTimeoutMin     = 3600
	deployTimeoutMax     = 604800
)

// profileUpsertRequest 是 POST/PUT /admin/profiles 的请求体。
// 可选字段用指针区分"没传"（PUT 保留旧值）与"传了空值"。
type profileUpsertRequest struct {
	Name                 string             `json:"name"`
	Cluster              *string            `json:"cluster"`
	Namespace            *string            `json:"namespace"`
	Mounts               *[]json.RawMessage `json:"mounts"`
	Scheduler            *string            `json:"scheduler"`
	Queue                *string            `json:"queue"`
	ImagePullSecrets     *[]string          `json:"image_pull_secrets"`
	ResourceDefaults     *map[string]any    `json:"resource_defaults"`
	DeployTimeoutSeconds *int               `json:"deploy_timeout_seconds"`
	NetworkPolicy        *map[string]any    `json:"network_policy"`
}

// adminProfileDTO 是 admin 写端点的响应（openapi ExecutionProfileAdmin）。
// 与 GET /profiles 的投影相比多出 network_policy / created_by / updated_at。
type adminProfileDTO struct {
	Name                 string            `json:"name"`
	Cluster              string            `json:"cluster"`
	Namespace            string            `json:"namespace"`
	Mounts               []json.RawMessage `json:"mounts"`
	Scheduler            string            `json:"scheduler"`
	Queue                *string           `json:"queue"`
	ImagePullSecrets     []string          `json:"image_pull_secrets"`
	ResourceDefaults     map[string]any    `json:"resource_defaults"`
	DeployTimeoutSeconds int               `json:"deploy_timeout_seconds"`
	NetworkPolicy        map[string]any    `json:"network_policy"`
	CreatedBy            string            `json:"created_by"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

func toAdminProfileDTO(p *store.AdminProfile) adminProfileDTO {
	return adminProfileDTO{
		Name:                 p.Name,
		Cluster:              p.Cluster,
		Namespace:            p.Namespace,
		Mounts:               decodeRawArray(p.Mounts),
		Scheduler:            p.Scheduler,
		Queue:                p.Queue,
		ImagePullSecrets:     decodeStringArray(p.ImagePullSecrets),
		ResourceDefaults:     decodeObject(p.ResourceDefaults),
		DeployTimeoutSeconds: p.DeployTimeoutSeconds,
		NetworkPolicy:        decodeObject(p.NetworkPolicy),
		CreatedBy:            p.CreatedBy,
		UpdatedAt:            p.UpdatedAt.UTC(),
	}
}

// handleAdminCreateProfile 是 POST /api/v1/admin/profiles（201）。
func (s *Server) handleAdminCreateProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "register", "execution_profile"

	var req profileUpsertRequest
	if !s.decodeAdminJSON(w, r, &req) {
		return
	}
	name, ok := validAdminName(req.Name, profileNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"name 不能为空、不得超过 128 字节且不能含控制字符", map[string]any{"field": "name"})
		return
	}
	cluster, ok := requiredString(req.Cluster)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"cluster 不能为空", map[string]any{"field": "cluster"})
		return
	}
	if _, err := s.st.Clusters.Get(ctx, nil, cluster); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectValidation(w, r, action, objectType, name,
				"cluster 未登记（先 POST /admin/clusters）",
				map[string]any{"field": "cluster", "cluster": cluster})
			return
		}
		s.failErr(w, r, err)
		return
	}
	namespace, ok := requiredString(req.Namespace)
	if !ok || len(namespace) > namespaceMaxLen {
		s.rejectValidation(w, r, action, objectType, name,
			"namespace 不能为空且不得超过 63 字节", map[string]any{"field": "namespace"})
		return
	}
	scheduler, errDetail := schedulerOrDefault(req.Scheduler)
	if errDetail != "" {
		s.rejectValidation(w, r, action, objectType, name, errDetail, map[string]any{"field": "scheduler"})
		return
	}
	timeout, errDetail := deployTimeoutOrDefault(req.DeployTimeoutSeconds)
	if errDetail != "" {
		s.rejectValidation(w, r, action, objectType, name, errDetail,
			map[string]any{"field": "deploy_timeout_seconds"})
		return
	}
	mounts := []json.RawMessage{}
	if req.Mounts != nil {
		mounts = *req.Mounts
	}
	if bad, detail := validateMounts(mounts); bad != "" {
		s.rejectValidation(w, r, action, objectType, name, bad, detail)
		return
	}

	p := &store.AdminProfile{
		Name:                 name,
		Cluster:              cluster,
		Namespace:            namespace,
		Mounts:               jsonRaw(nonNilRaw(mounts)),
		Scheduler:            scheduler,
		Queue:                optionalText(req.Queue),
		ImagePullSecrets:     jsonRaw(stringSlice(req.ImagePullSecrets)),
		ResourceDefaults:     jsonRaw(mapOrEmpty(req.ResourceDefaults)),
		DeployTimeoutSeconds: timeout,
		NetworkPolicy:        jsonRaw(mapOrEmpty(req.NetworkPolicy)),
		CreatedBy:            Actor(ctx),
	}
	// 业务变更 + 审计同事务：审计失败整体回滚（PR #33 #6）。
	err := s.st.InAuditedTx(ctx, s.auditEntry(r, action, objectType, name, map[string]any{
		"cluster": cluster, "namespace": namespace,
	}), func(tx pgx.Tx) error {
		return s.st.Profiles.Create(ctx, tx, p)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusConflict, domain.CodeConflict,
				"Execution Profile 名字已存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, toAdminProfileDTO(p))
}

// handleAdminUpdateProfile 是 PUT /api/v1/admin/profiles（200）。
func (s *Server) handleAdminUpdateProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "update", "execution_profile"

	var req profileUpsertRequest
	if !s.decodeAdminJSON(w, r, &req) {
		return
	}
	name, ok := validAdminName(req.Name, profileNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, name,
			"name 不能为空、不得超过 128 字节且不能含控制字符", map[string]any{"field": "name"})
		return
	}
	cur, err := s.st.Profiles.Get(ctx, nil, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"Execution Profile 不存在", map[string]any{"name": name}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}

	merged := *cur
	if req.Cluster != nil {
		cluster, ok := requiredString(req.Cluster)
		if !ok {
			s.rejectValidation(w, r, action, objectType, name,
				"cluster 不能为空", map[string]any{"field": "cluster"})
			return
		}
		if _, err := s.st.Clusters.Get(ctx, nil, cluster); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				s.rejectValidation(w, r, action, objectType, name,
					"cluster 未登记（先 POST /admin/clusters）",
					map[string]any{"field": "cluster", "cluster": cluster})
				return
			}
			s.failErr(w, r, err)
			return
		}
		merged.Cluster = cluster
	}
	if req.Namespace != nil {
		ns, ok := requiredString(req.Namespace)
		if !ok || len(ns) > namespaceMaxLen {
			s.rejectValidation(w, r, action, objectType, name,
				"namespace 不能为空且不得超过 63 字节", map[string]any{"field": "namespace"})
			return
		}
		merged.Namespace = ns
	}
	if req.Scheduler != nil {
		scheduler, errDetail := schedulerOrDefault(req.Scheduler)
		if errDetail != "" {
			s.rejectValidation(w, r, action, objectType, name, errDetail, map[string]any{"field": "scheduler"})
			return
		}
		merged.Scheduler = scheduler
	}
	if req.DeployTimeoutSeconds != nil {
		timeout, errDetail := deployTimeoutOrDefault(req.DeployTimeoutSeconds)
		if errDetail != "" {
			s.rejectValidation(w, r, action, objectType, name, errDetail,
				map[string]any{"field": "deploy_timeout_seconds"})
			return
		}
		merged.DeployTimeoutSeconds = timeout
	}
	if req.Queue != nil {
		merged.Queue = optionalText(req.Queue)
	}
	if req.Mounts != nil {
		if bad, detail := validateMounts(*req.Mounts); bad != "" {
			s.rejectValidation(w, r, action, objectType, name, bad, detail)
			return
		}
		merged.Mounts = jsonRaw(nonNilRaw(*req.Mounts))
	}
	if req.ImagePullSecrets != nil {
		merged.ImagePullSecrets = jsonRaw(stringSlice(req.ImagePullSecrets))
	}
	if req.ResourceDefaults != nil {
		merged.ResourceDefaults = jsonRaw(mapOrEmpty(req.ResourceDefaults))
	}
	if req.NetworkPolicy != nil {
		merged.NetworkPolicy = jsonRaw(mapOrEmpty(req.NetworkPolicy))
	}

	entry := s.auditEntry(r, action, objectType, name, map[string]any{
		"cluster": merged.Cluster, "namespace": merged.Namespace,
		"scheduler": merged.Scheduler, "deploy_timeout_seconds": merged.DeployTimeoutSeconds,
	})
	err = s.st.InAuditedTx(ctx, entry, func(tx pgx.Tx) error {
		return s.st.Profiles.Update(ctx, tx, &merged)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"Execution Profile 不存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, toAdminProfileDTO(&merged))
}

// handleAdminDeleteProfile 是 DELETE /api/v1/admin/profiles?name=<name>（200）。
//
// 被引用也允许删除：历史 Task 已冻结 profile 名，删掉登记不影响它们；
// 引用数只作为 detail 回报，方便调用方判断影响面。
func (s *Server) handleAdminDeleteProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const action, objectType = "delete_profile", "execution_profile"

	name, ok := validAdminName(r.URL.Query().Get("name"), profileNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, action, objectType, "",
			"缺少或非法的 name 查询参数", map[string]any{"param": "name"})
		return
	}
	if _, err := s.st.Profiles.Get(ctx, nil, name); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"Execution Profile 不存在", map[string]any{"name": name}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	refs := 0
	detail := map[string]any{"referencing_tasks": 0}
	entry := s.auditEntry(r, action, objectType, name, detail)
	// 计数 + 删除 + 审计同事务：删除生效就一定有审计，反之亦然。
	err := s.st.InAuditedTx(ctx, entry, func(tx pgx.Tx) error {
		n, err := s.st.Profiles.CountTaskRefs(ctx, tx, name)
		if err != nil {
			return err
		}
		refs = n
		detail["referencing_tasks"] = n
		entry.Detail["referencing_tasks"] = n
		return s.st.Profiles.Delete(ctx, tx, name)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			s.rejectWrite(w, r, action, objectType, name,
				http.StatusNotFound, domain.CodeNotFound,
				"Execution Profile 不存在", map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, action, objectType, name, err)
		default:
			s.failWriteErr(w, r, action, objectType, name, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "deleted": true, "referencing_tasks": refs,
	})
}

// ---- Profile 请求的校验小工具 --------------------------------------------

// requiredString 取值并去空白；空串返回 false。
func requiredString(p *string) (string, bool) {
	if p == nil {
		return "", false
	}
	v := strings.TrimSpace(*p)
	return v, v != ""
}

// optionalText 把空串收敛成 nil（jsonb/text 列里"没队列"用 NULL 表示更干净）。
func optionalText(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

func schedulerOrDefault(p *string) (string, string) {
	if p == nil {
		return "default", ""
	}
	v := strings.TrimSpace(*p)
	switch v {
	case "":
		return "default", ""
	case "default", "volcano":
		return v, ""
	default:
		return "", "scheduler 只能是 default 或 volcano"
	}
}

func deployTimeoutOrDefault(p *int) (int, string) {
	if p == nil || *p == 0 {
		return deployTimeoutDefault, ""
	}
	if *p < deployTimeoutMin || *p > deployTimeoutMax {
		return 0, fmt.Sprintf("deploy_timeout_seconds 必须在 %d..%d 秒之间", deployTimeoutMin, deployTimeoutMax)
	}
	return *p, ""
}

func stringSlice(p *[]string) []string {
	if p == nil {
		return []string{}
	}
	return *p
}

func mapOrEmpty(p *map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return *p
}

// nonNilRaw 保证 mounts 序列化成 [] 而不是 null。
func nonNilRaw(items []json.RawMessage) []json.RawMessage {
	if items == nil {
		return []json.RawMessage{}
	}
	return items
}

// validateMounts 校验 mounts 项的形状：每项必须是对象且 name/pvc 非空（06 §3）。
func validateMounts(items []json.RawMessage) (string, map[string]any) {
	for i, item := range items {
		var m map[string]any
		if err := json.Unmarshal(item, &m); err != nil {
			return fmt.Sprintf("mounts[%d] 必须是对象", i),
				map[string]any{"field": fmt.Sprintf("mounts[%d]", i)}
		}
		for _, key := range []string{"name", "pvc"} {
			v, _ := m[key].(string)
			if strings.TrimSpace(v) == "" {
				return fmt.Sprintf("mounts[%d].%s 不能为空", i, key),
					map[string]any{"field": fmt.Sprintf("mounts[%d].%s", i, key)}
			}
		}
	}
	return "", nil
}
