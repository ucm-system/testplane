package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// profileDTO 是 openapi `ExecutionProfile` 的投影（06 §3、03 §3.2）。
// mounts 是挂载能力：[{name, pvc, sub_path, kind}]，前端提交页据此渲染可选挂载。
type profileDTO struct {
	Name                 string            `json:"name"`
	Cluster              string            `json:"cluster"`
	Namespace            string            `json:"namespace"`
	Mounts               []json.RawMessage `json:"mounts"`
	Scheduler            string            `json:"scheduler"`
	Queue                *string           `json:"queue"`
	ImagePullSecrets     []string          `json:"image_pull_secrets"`
	ResourceDefaults     map[string]any    `json:"resource_defaults"`
	DeployTimeoutSeconds int               `json:"deploy_timeout_seconds"`
}

// handleListProfiles 是 GET /api/v1/profiles（07 §5.3）：只读，返回已登记的
// Execution Profile 与挂载能力。Profile 数量是个位数，不分页。
func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.Profiles.List(r.Context(), nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]profileDTO, 0, len(rows))
	for _, p := range rows {
		dto := profileDTO{
			Name: p.Name, Cluster: p.Cluster, Namespace: p.Namespace,
			Scheduler: p.Scheduler, Queue: p.Queue,
			DeployTimeoutSeconds: p.DeployTimeoutSeconds,
			Mounts:               []json.RawMessage{},
			ImagePullSecrets:     []string{},
			ResourceDefaults:     map[string]any{},
		}
		if len(p.Mounts) > 0 {
			_ = json.Unmarshal(p.Mounts, &dto.Mounts)
		}
		if len(p.ImagePullSecrets) > 0 {
			_ = json.Unmarshal(p.ImagePullSecrets, &dto.ImagePullSecrets)
		}
		if len(p.ResourceDefaults) > 0 {
			_ = json.Unmarshal(p.ResourceDefaults, &dto.ResourceDefaults)
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, paged[profileDTO]{Items: out})
}

// handleGetTesterVersion 是 GET /api/v1/testers/{name}/versions/{version}：
// 指定版本的 Tester 详情（含 parameters / env 声明，07 §5.3）。
func (s *Server) handleGetTesterVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	version := chi.URLParam(r, "version")
	if name == "" || version == "" {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"name 与 version 都不能为空", nil, false)
		return
	}
	spec, err := s.resolver.Resolve(ctx, name, version)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dto := toTesterDTO(spec)
	if row, err := s.st.Testers.Get(ctx, nil, spec.Name, spec.Version); err == nil {
		fillTesterRegistryMeta(&dto, row)
		s.markTesterLatest(ctx, &dto, spec.Name)
	} else {
		s.logger.Warn("Tester 元数据缺失", "err", err, "tester", name, "version", version)
	}
	writeJSON(w, http.StatusOK, dto)
}

// fillTesterRegistryMeta 把注册表行里的审计字段补进 DTO（两个 Tester 端点共用）。
func fillTesterRegistryMeta(dto *testerDTO, row *store.Tester) {
	dto.ContentSHA = row.ContentSHA
	dto.SourceFile = row.SourceFile
	dto.Orphaned = row.Orphaned
	dto.RegisteredAt = utcPtr(&row.RegisteredAt)
	dto.RegisteredBy = row.RegisteredBy
	// retired 与 orphaned 同源（票 #60）：目录里已不存在、只剩库里的历史版本。
	dto.Retired = row.Orphaned
}

// markTesterLatest 按"该 name 的库里最高版本"设置 dto.Latest（domain.VersionLess 唯一真源）。
func (s *Server) markTesterLatest(ctx context.Context, dto *testerDTO, name string) {
	rows, err := s.st.Testers.List(ctx, nil, store.TesterFilter{IncludeOrphaned: true})
	if err != nil {
		return
	}
	var maxVer string
	for i := range rows {
		if rows[i].Name == name && domain.VersionLess(maxVer, rows[i].Version) {
			maxVer = rows[i].Version
		}
	}
	dto.Latest = maxVer == dto.Version
}
