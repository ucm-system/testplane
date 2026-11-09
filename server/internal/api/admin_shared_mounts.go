package api

// 平台级统一共享挂载的管理面（D-65；07 §5.4 admin + §5.3 只读视图）。
//
// 语义要点：
//   - `shared_mounts` 表是**运行时真源**；TESTPLANE_SHARED_MOUNTS 只在表为空时做一次性种子；
//   - 平台只存 **PVC 名**（claim_name），**不创建** PV/PVC（§10.4 边界）；
//   - 校验：mount_path 绝对路径、不得命中保留路径、name/path 唯一、claim_name 非空；
//     违反 → 400 `VALIDATION_ERROR`，`detail.issues[].code` 用 `CONFLICT`；
//   - 写入即审计（`InAuditedTx`，审计失败整体回滚）；
//   - 删除只影响**之后**的渲染：已提交 Run 的 Task 快照已冻结，不受影响。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

const (
	sharedMountNameMaxLen = 63 // volume 名要能当 K8s 资源名用
	actionMountRegister   = "register_shared_mount"
	actionMountUpdate     = "update_shared_mount"
	actionMountDelete     = "delete_shared_mount"
	objectTypeSharedMount = "shared_mount"
)

// sharedMountDTO 是 4 个 admin 端点与只读视图共用的投影（openapi AdminSharedMount / SharedMount）。
type sharedMountDTO struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only"`
	ClaimName string `json:"claim_name"`
	Enabled   bool   `json:"enabled"`
}

func toSharedMountDTO(m *store.SharedMount) sharedMountDTO {
	return sharedMountDTO{
		Name: m.Name, MountPath: m.MountPath, ReadOnly: m.ReadOnly,
		ClaimName: m.ClaimName, Enabled: m.Enabled,
	}
}

// sharedMountUpsertRequest 是 POST/PUT 的请求体；可选字段用指针区分"没传"与"传了 false"。
type sharedMountUpsertRequest struct {
	Name      string  `json:"name"`
	MountPath *string `json:"mount_path"`
	ReadOnly  *bool   `json:"read_only"`
	ClaimName *string `json:"claim_name"`
	Enabled   *bool   `json:"enabled"`
}

// cleanSharedMountPath 归一化（去尾斜杠 + path.Clean），保留原始绝对路径语义。
func cleanSharedMountPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if !path.IsAbs(p) {
		return p
	}
	return path.Clean(p)
}

// validateSharedMount 校验一条候选记录（self 为空表示新建）。
//
// 返回 (issues, ok)：issues 里带 path/code/message，直接放进 400 的 detail.issues。
func (s *Server) validateSharedMount(ctx context.Context, name, mountPath, claimName, self string) []map[string]any {
	var issues []map[string]any
	add := func(p, code, msg string) {
		issues = append(issues, map[string]any{"path": p, "code": code, "message": msg})
	}
	mountPath = cleanSharedMountPath(mountPath)
	claimName = strings.TrimSpace(claimName)

	if mountPath == "" || !path.IsAbs(mountPath) {
		add("mount_path", domain.IssueCodeRequired, "mount_path 必须是绝对路径（如 /models）")
	}
	if claimName == "" {
		add("claim_name", domain.IssueCodeRequired,
			"claim_name 必填：平台只引用已存在的 PVC，内联 volumes[].nfs 会被 PodSecurity restricted 拒绝")
	}
	// 保留路径（/workspace/output、/workspace/input、/tmp、/tester）不许被统一挂载占用。
	if mountPath != "" {
		if reason, reserved := domain.ReservedMountPaths(nil)[mountPath]; reserved {
			add("mount_path", domain.IssueCodeConflict,
				fmt.Sprintf("mount_path %q 与平台保留路径冲突（%s）", mountPath, reason))
		}
	}
	if len(issues) > 0 {
		return issues
	}
	// name / path 唯一：与**其它行**比较（self 表示更新时排除自己）。
	rows, err := s.st.SharedMounts.List(ctx, nil, false)
	if err != nil {
		// 查库失败交给上层 failErr（这里不伪造校验结论）。
		return nil
	}
	for i := range rows {
		row := &rows[i]
		if row.Name == self {
			// 更新自己：path 可以变，但只有名字相同才跳过整行比较。
			continue
		}
		if row.Name == name {
			add("name", domain.IssueCodeConflict, fmt.Sprintf("name %q 已存在（同名挂载点）", name))
		}
		if cleanSharedMountPath(row.MountPath) == mountPath {
			add("mount_path", domain.IssueCodeConflict,
				fmt.Sprintf("mount_path %q 已被挂载点 %q 占用", mountPath, row.Name))
		}
	}
	return issues
}

// handleAdminListSharedMounts 是 GET /api/v1/admin/shared-mounts（admin）。
func (s *Server) handleAdminListSharedMounts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.SharedMounts.List(r.Context(), nil, false)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	items := make([]sharedMountDTO, 0, len(rows))
	for i := range rows {
		items = append(items, toSharedMountDTO(&rows[i]))
	}
	writeJSON(w, http.StatusOK, paged[sharedMountDTO]{Items: items})
}

// handleAdminCreateSharedMount 是 POST /api/v1/admin/shared-mounts（201）。
func (s *Server) handleAdminCreateSharedMount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req sharedMountUpsertRequest
	if !s.decodeAdminJSON(w, r, &req) {
		return
	}
	name, ok := validAdminName(req.Name, sharedMountNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, actionMountRegister, objectTypeSharedMount, "",
			"name 不能为空、不得超过 63 字节且不能含控制字符", map[string]any{"field": "name"})
		return
	}
	mountPath, claimName := "", ""
	if req.MountPath != nil {
		mountPath = *req.MountPath
	}
	if req.ClaimName != nil {
		claimName = *req.ClaimName
	}
	if issues := s.validateSharedMount(ctx, name, mountPath, claimName, ""); len(issues) > 0 {
		s.rejectValidation(w, r, actionMountRegister, objectTypeSharedMount, name,
			"共享挂载校验失败", map[string]any{"issues": issues})
		return
	}
	readOnly := true // 默认只读（D-65）
	if req.ReadOnly != nil {
		readOnly = *req.ReadOnly
	}
	enabled := true // 默认启用
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if _, err := s.st.SharedMounts.Get(ctx, nil, name); err == nil {
		s.rejectWrite(w, r, actionMountRegister, objectTypeSharedMount, name,
			http.StatusConflict, domain.CodeConflict,
			"共享挂载点已存在（用 PUT 修改）", map[string]any{"name": name}, false)
		return
	} else if !errors.Is(err, domain.ErrNotFound) {
		s.failErr(w, r, err)
		return
	}

	m := &store.SharedMount{
		Name: name, MountPath: cleanSharedMountPath(mountPath), ReadOnly: readOnly,
		ClaimName: strings.TrimSpace(claimName), Enabled: enabled,
		CreatedBy: Actor(ctx), UpdatedBy: Actor(ctx),
	}
	err := s.st.InAuditedTx(ctx, s.auditEntry(r, actionMountRegister, objectTypeSharedMount, name,
		map[string]any{"mount_path": m.MountPath, "claim_name": m.ClaimName,
			"read_only": m.ReadOnly, "enabled": m.Enabled}), func(tx pgx.Tx) error {
		return s.st.SharedMounts.Upsert(ctx, tx, m)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, actionMountRegister, objectTypeSharedMount, name, err)
		case errors.Is(err, domain.ErrConflict):
			s.rejectValidation(w, r, actionMountRegister, objectTypeSharedMount, name,
				"mount_path 已被占用", map[string]any{"issues": []map[string]any{{
					"path": "mount_path", "code": domain.IssueCodeConflict,
					"message": "mount_path 已被占用",
				}}})
		default:
			s.failWriteErr(w, r, actionMountRegister, objectTypeSharedMount, name, err)
		}
		return
	}
	s.logger.Info("shared mount registered", "name", m.Name, "mount_path", m.MountPath,
		"claim_name", m.ClaimName, "read_only", m.ReadOnly, "enabled", m.Enabled)
	writeJSON(w, http.StatusCreated, toSharedMountDTO(m))
}

// handleAdminUpdateSharedMount 是 PUT /api/v1/admin/shared-mounts/{name}（200）。
// name 不可改；省略的字段保留旧值。
func (s *Server) handleAdminUpdateSharedMount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, ok := validAdminName(chi.URLParam(r, "name"), sharedMountNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, actionMountUpdate, objectTypeSharedMount, "",
			"路径里的 name 非法", map[string]any{"param": "name"})
		return
	}
	var req sharedMountUpsertRequest
	if !s.decodeAdminJSON(w, r, &req) {
		return
	}
	if req.Name != "" && req.Name != name {
		s.rejectValidation(w, r, actionMountUpdate, objectTypeSharedMount, name,
			"name 不可修改（要改名请删除后重建）", map[string]any{"field": "name"})
		return
	}
	cur, err := s.st.SharedMounts.Get(ctx, nil, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, actionMountUpdate, objectTypeSharedMount, name,
				http.StatusNotFound, domain.CodeNotFound, "共享挂载点不存在",
				map[string]any{"name": name}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	mountPath := cur.MountPath
	if req.MountPath != nil {
		mountPath = *req.MountPath
	}
	claimName := cur.ClaimName
	if req.ClaimName != nil {
		claimName = *req.ClaimName
	}
	if issues := s.validateSharedMount(ctx, name, mountPath, claimName, name); len(issues) > 0 {
		s.rejectValidation(w, r, actionMountUpdate, objectTypeSharedMount, name,
			"共享挂载校验失败", map[string]any{"issues": issues})
		return
	}
	readOnly := cur.ReadOnly
	if req.ReadOnly != nil {
		readOnly = *req.ReadOnly
	}
	enabled := cur.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	m := &store.SharedMount{
		Name: name, MountPath: cleanSharedMountPath(mountPath), ReadOnly: readOnly,
		ClaimName: strings.TrimSpace(claimName), Enabled: enabled,
		CreatedBy: cur.CreatedBy, UpdatedBy: Actor(ctx),
	}
	err = s.st.InAuditedTx(ctx, s.auditEntry(r, actionMountUpdate, objectTypeSharedMount, name,
		map[string]any{"mount_path": m.MountPath, "claim_name": m.ClaimName,
			"read_only": m.ReadOnly, "enabled": m.Enabled}), func(tx pgx.Tx) error {
		return s.st.SharedMounts.Upsert(ctx, tx, m)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, actionMountUpdate, objectTypeSharedMount, name, err)
		case errors.Is(err, domain.ErrConflict):
			s.rejectValidation(w, r, actionMountUpdate, objectTypeSharedMount, name,
				"mount_path 已被占用", map[string]any{"issues": []map[string]any{{
					"path": "mount_path", "code": domain.IssueCodeConflict,
					"message": "mount_path 已被占用",
				}}})
		default:
			s.failWriteErr(w, r, actionMountUpdate, objectTypeSharedMount, name, err)
		}
		return
	}
	s.logger.Info("shared mount updated", "name", m.Name, "mount_path", m.MountPath,
		"claim_name", m.ClaimName, "read_only", m.ReadOnly, "enabled", m.Enabled)
	m.CreatedAt = cur.CreatedAt
	writeJSON(w, http.StatusOK, toSharedMountDTO(m))
}

// handleAdminDeleteSharedMount 是 DELETE /api/v1/admin/shared-mounts/{name}（204）。
func (s *Server) handleAdminDeleteSharedMount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, ok := validAdminName(chi.URLParam(r, "name"), sharedMountNameMaxLen)
	if !ok {
		s.rejectValidation(w, r, actionMountDelete, objectTypeSharedMount, "",
			"路径里的 name 非法", map[string]any{"param": "name"})
		return
	}
	if _, err := s.st.SharedMounts.Get(ctx, nil, name); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.rejectWrite(w, r, actionMountDelete, objectTypeSharedMount, name,
				http.StatusNotFound, domain.CodeNotFound, "共享挂载点不存在",
				map[string]any{"name": name}, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	err := s.st.InAuditedTx(ctx, s.auditEntry(r, actionMountDelete, objectTypeSharedMount, name,
		map[string]any{"name": name}), func(tx pgx.Tx) error {
		return s.st.SharedMounts.Delete(ctx, tx, name)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			s.rejectWrite(w, r, actionMountDelete, objectTypeSharedMount, name,
				http.StatusNotFound, domain.CodeNotFound, "共享挂载点不存在",
				map[string]any{"name": name}, false)
		case errors.Is(err, domain.ErrTx):
			s.failAuditTx(w, r, actionMountDelete, objectTypeSharedMount, name, err)
		default:
			s.failWriteErr(w, r, actionMountDelete, objectTypeSharedMount, name, err)
		}
		return
	}
	s.logger.Info("shared mount deleted", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// handleListSharedMounts 是 GET /api/v1/shared-mounts（viewer+，只读）：
// 回答"我这次提交的 Run 会挂哪些共享挂载"——只回**启用中**的。
func (s *Server) handleListSharedMounts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.SharedMounts.List(r.Context(), nil, true)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	items := make([]sharedMountDTO, 0, len(rows))
	for i := range rows {
		items = append(items, toSharedMountDTO(&rows[i]))
	}
	writeJSON(w, http.StatusOK, paged[sharedMountDTO]{Items: items})
}

// sharedMountsSpecs 是"渲染/提交校验用"的统一入口（D-65）：
// 优先 DB（启用中的行）；DB 读失败或为空时回落启动种子（config.SharedMounts）。
//
// 注意（设计上的取舍）：这里**不缓存**，所以管理面写入立即生效、不存在失效问题；
// 每次提交多一次 small query，与"改完立刻能看到效果"相比可以接受。
func (s *Server) sharedMountsSpecs(ctx context.Context) []domain.SharedMountSpec {
	if s.sharedMounts == nil {
		return s.cfg.SharedMounts
	}
	specs, err := s.sharedMounts(ctx)
	if err != nil {
		s.logger.Warn("读取 shared_mounts 失败，回落到启动种子", "err", err)
		return s.cfg.SharedMounts
	}
	return specs
}
