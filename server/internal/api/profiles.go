package api

import (
	"encoding/json"
	"net/http"
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
	// ExecutionProfileList 契约：{items}（数量个位数，不分页）。
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
