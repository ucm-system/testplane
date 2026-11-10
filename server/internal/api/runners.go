package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Runner 目录端点（07 §5.3、ARCHITECTURE 附录 C v2）：只读（注册靠目录扫描，K-3）。
// 数据源是 runners 表（注册真源，含 orphaned 标记与注册表元数据）。
//
// 默认每个 name 只回最新版本（票 #60 / D-60）；include_retired=true 回全部版本，
// 条目带 latest / retired。旧的 orphaned=true 参数保留为别名（兼容）。

// handleListRunners 是 GET /api/v1/runners。
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	role := q.Get("role")
	if role != "" && !domain.ValidRole(role) {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"role 只能是 test|pre_check|deploy|clean", nil, false)
		return
	}
	includeRetired := q.Get("include_retired") == "true" || q.Get("orphaned") == "true"

	rows, err := s.st.Runners.List(ctx, nil, runnerFilterOf(role, includeRetired))
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	// latest 判定按 name 维度（domain.VersionLess 唯一真源），与 include_retired 无关。
	latestByName := map[string]string{}
	presentByName := map[string]string{}
	for i := range rows {
		row := &rows[i]
		if cur, ok := latestByName[row.Name]; !ok || domain.VersionLess(cur, row.Version) {
			latestByName[row.Name] = row.Version
		}
		if !row.Orphaned {
			if cur, ok := presentByName[row.Name]; !ok || domain.VersionLess(cur, row.Version) {
				presentByName[row.Name] = row.Version
			}
		}
	}

	out := make([]runnerDTO, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if !includeRetired && row.Version != presentByName[row.Name] {
			// 默认视图：每个 name 只留"最新且未退役"的那条。
			continue
		}
		dto := toRunnerDTO(row)
		dto.Latest = latestByName[row.Name] == row.Version
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, paged[runnerDTO]{Items: out, HasMore: false})
}

// runnerFilterOf 构造目录过滤（v2：orphaned 行也入库，include_retired 控制可见性）。
func runnerFilterOf(role string, includeRetired bool) store.RunnerFilter {
	return store.RunnerFilter{Role: role, IncludeOrphaned: includeRetired}
}

// handleGetRunner 是 GET /api/v1/runners/{name}：最新版本详情。
func (s *Server) handleGetRunner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	row, err := s.st.Runners.GetLatest(ctx, nil, name)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dto := toRunnerDTO(row)
	dto.Latest = true
	writeJSON(w, http.StatusOK, dto)
}

// handleGetRunnerVersion 是 GET /api/v1/runners/{name}/versions/{version}：指定版本。
func (s *Server) handleGetRunnerVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	version := chi.URLParam(r, "version")
	row, err := s.st.Runners.Get(ctx, nil, name, version)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dto := toRunnerDTO(row)
	dto.Latest = false
	writeJSON(w, http.StatusOK, dto)
}
