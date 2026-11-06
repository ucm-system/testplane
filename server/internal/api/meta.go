package api

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// handleHealthz 是进程存活探针（07 §5.5 / openapi）：纯文本，不碰 DB。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz 是就绪探针：DB ping + 存储可写 + 迁移版本匹配；失败 503 + 纯文本原因。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var failures []string
	if err := s.st.Ping(ctx); err != nil {
		failures = append(failures, "database unavailable: "+err.Error())
	}
	if err := probeWritable(s.cfg.StorageRoot); err != nil {
		failures = append(failures, "storage not writable: "+err.Error())
	}
	dbVersion, err := store.CheckSchemaVersion(ctx, s.st.Pool())
	if err != nil {
		failures = append(failures, "schema version unreadable: "+err.Error())
	} else if expected := store.ExpectedSchemaVersion(); dbVersion < expected {
		failures = append(failures, "schema version mismatch: db="+
			itoa64(dbVersion)+" expected="+itoa64(expected))
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(failures) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		for i, f := range failures {
			if i > 0 {
				_, _ = w.Write([]byte("; "))
			}
			_, _ = w.Write([]byte(f))
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// probeWritable 在存储根写一个临时文件验证可写（NFS 可写性）。
func probeWritable(root string) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".readyz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// handleMeta 是 GET /api/v1/meta（openapi Meta）：{version, api_version, features}。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     s.version,
		"api_version": "v1",
		"features": map[string]bool{
			"dry_run":         true,
			"events_history":  true,
			"report_md":       true,
			"deploy":          true,
			"queue_status":    true,
			"events_sse":      true,
			"standalone_task": false, // MVP 不做 POST /tasks（C-5.9 仅设计保留）
		},
	})
}

// handleMe 是 GET /api/v1/me（openapi Me）：返回身份与（oidc 模式下的）CSRF token。
// 前端把 csrf_token 存进内存，写操作时作为 `X-CSRF-Token` 回填（双提交，07 §2.1）。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentIdentity(r))
}

// handleListTesters 是 GET /api/v1/testers（只读目录，K-3）。
// handleListTesters 是 GET /api/v1/testers（票 #60 / D-60）：
//
// 默认**每个 name 只回最新版本**——库里同名多版本是正常现象（历史版本不删，见 §5.6），
// 但目录页按"一个 Tester 一条"呈现才不会让人误以为重复入库。
// `include_retired=true` 时返回全部版本（含已退役的历史版本），每条带 `latest`/`retired`。
// 旧的 `orphaned=true` 参数保留为 `include_retired` 的别名（CLI 兼容）。
func (s *Server) handleListTesters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	role := q.Get("role")
	if role != "" && role != domain.RoleTest && role != domain.RolePreCheck {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError,
			"role 只能是 test|pre_check", nil, false)
		return
	}
	includeRetired := q.Get("include_retired") == "true" || q.Get("orphaned") == "true"

	specs, err := s.resolver.List(ctx, role, includeRetired)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	// latest/retired 来自 testers 表（权威：含退役行，registry 内存快照默认隐藏退役）。
	// trow 按 name@version 索引；latestByName 的名字集合也来自库，避免"目录里没有版本"时误判。
	rows, err := s.st.Testers.List(ctx, nil, store.TesterFilter{Role: role, IncludeOrphaned: true})
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	byKey := make(map[string]*store.Tester, len(rows))
	latestByName := make(map[string]string, len(rows))
	latestPresentByName := make(map[string]string, len(rows))
	for i := range rows {
		row := &rows[i]
		byKey[row.Name+"@"+row.Version] = row
		if cur, ok := latestByName[row.Name]; !ok || domain.VersionLess(cur, row.Version) {
			latestByName[row.Name] = row.Version
		}
		if !row.Orphaned {
			if cur, ok := latestPresentByName[row.Name]; !ok || domain.VersionLess(cur, row.Version) {
				latestPresentByName[row.Name] = row.Version
			}
		}
	}

	out := make([]testerDTO, 0, len(specs))
	for _, sp := range specs {
		row := byKey[sp.Name+"@"+sp.Version]
		if row == nil {
			// 注册表内存里比库里新（刚扫描到、尚未落库）：按"在目录里"处理。
			dto := toTesterDTO(sp)
			dto.Latest = latestByName[sp.Name] == "" || !domain.VersionLess(sp.Version, latestByName[sp.Name])
			out = append(out, dto)
			continue
		}
		if !includeRetired && row.Version != latestPresentByName[sp.Name] {
			// 默认视图：每个 name 只留"最新且未退役"的那条。
			continue
		}
		dto := toTesterDTO(sp)
		dto.ContentSHA = row.ContentSHA
		dto.SourceFile = row.SourceFile
		dto.Orphaned = row.Orphaned
		dto.RegisteredAt = utcPtr(&row.RegisteredAt)
		dto.RegisteredBy = row.RegisteredBy
		dto.Retired = row.Orphaned
		dto.Latest = latestByName[sp.Name] == row.Version
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, paged[testerDTO]{Items: out})
}

// handleGetTester 是 GET /api/v1/testers/{name}：最新版本（version="" → 最新）。
// openapi 的 Tester 还要求注册表元数据（content_sha/source_file/orphaned/registered_at/by），
// 这些不在 TesterSpec 里，从 testers 表补。
func (s *Server) handleGetTester(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	spec, err := s.resolver.Resolve(ctx, name, "")
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	dto := toTesterDTO(spec)
	if row, err := s.st.Testers.Get(ctx, nil, spec.Name, spec.Version); err == nil {
		dto.ContentSHA = row.ContentSHA
		dto.SourceFile = row.SourceFile
		dto.Orphaned = row.Orphaned
		dto.RegisteredAt = utcPtr(&row.RegisteredAt)
		dto.RegisteredBy = row.RegisteredBy
		dto.Retired = row.Orphaned
		s.markTesterLatest(ctx, &dto, spec.Name)
	} else {
		s.logger.Warn("Tester 元数据缺失", "err", err, "tester", spec.Name, "version", spec.Version)
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleListClusters 是 GET /api/v1/clusters：概要，不返回凭据内容。
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.Clusters.List(r.Context(), nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	out := make([]clusterDTO, 0, len(items))
	for _, c := range items {
		out = append(out, toClusterDTO(c))
	}
	writeJSON(w, http.StatusOK, paged[clusterDTO]{Items: out})
}

// handleQueueStatus 是 GET /api/v1/queue-status（openapi QueueStatus）：
// 平台槽位 + 等待原因分桶 + 排队明细 + Volcano 只读快照（MVP 不可达 → available:false）。
func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	active, err := s.st.Attempts.CountActive(ctx, nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	leases, err := s.st.Leases.List(ctx, nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	byReason, err := s.st.Tasks.WaitingBreakdown(ctx, nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	waitingTasks, err := s.st.Tasks.WaitingTasks(ctx, nil, 100)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	maxSlots := s.cfg.MaxGlobalConcurrency
	available := maxSlots - active
	if available < 0 {
		available = 0
	}

	// 端点槽位按 endpoint_key 聚合。
	type endpointSlot struct {
		EndpointKey string   `json:"endpoint_key"`
		Exclusive   bool     `json:"exclusive"`
		Holders     []string `json:"holders"`
	}
	slotIndex := map[string]*endpointSlot{}
	order := make([]string, 0, len(leases))
	for _, l := range leases {
		slot, ok := slotIndex[l.EndpointKey]
		if !ok {
			slot = &endpointSlot{EndpointKey: l.EndpointKey}
			slotIndex[l.EndpointKey] = slot
			order = append(order, l.EndpointKey)
		}
		slot.Holders = append(slot.Holders, l.TaskID)
		if l.Exclusive {
			slot.Exclusive = true
		}
	}
	endpoints := make([]endpointSlot, 0, len(order))
	for _, k := range order {
		endpoints = append(endpoints, *slotIndex[k])
	}

	// by_reason 六个键必须齐全（openapi additionalProperties: false）。
	reasonCounts := map[string]int{
		domain.WaitPrecheckGate: 0, domain.WaitSlotWaiting: 0, domain.WaitGlobalLimit: 0,
		domain.WaitQueueFull: 0, domain.WaitInsufficientResources: 0, domain.WaitGangPending: 0,
	}
	total := 0
	for k, v := range byReason {
		total += v
		if _, known := reasonCounts[k]; known {
			reasonCounts[k] = v
		}
	}

	tasks := make([]map[string]any, 0, len(waitingTasks))
	for _, t := range waitingTasks {
		reason := domain.WaitPrecheckGate
		if t.WaitingReason != nil && *t.WaitingReason != "" {
			reason = *t.WaitingReason
		}
		item := map[string]any{
			"task_id":        t.TaskID,
			"run_id":         t.RunID,
			"name":           t.Name,
			"seq":            t.Seq,
			"status":         t.Status,
			"waiting_reason": reason,
			"endpoint_key":   t.EndpointKey,
			"since":          nil,
		}
		if t.Since != nil {
			item["since"] = t.Since.UTC()
		}
		tasks = append(tasks, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": s.now(),
		"slots": map[string]any{
			"concurrency_max": maxSlots,
			"active":          active,
			"available":       available,
			"endpoints":       endpoints,
		},
		"waiting": map[string]any{
			"total":     total,
			"by_reason": reasonCounts,
			"tasks":     tasks,
		},
		"volcano": s.volcanoBlock(ctx),
	})
}

// clusterDTO 是 openapi Cluster 的投影（不暴露 kubeconfig_key）。
type clusterDTO struct {
	Name            string     `json:"name"`
	APIServer       string     `json:"api_server"`
	Description     *string    `json:"description"`
	LastCheckAt     *time.Time `json:"last_check_at"`
	LastCheckResult *string    `json:"last_check_result"`
}

func toClusterDTO(c store.Cluster) clusterDTO {
	return clusterDTO{
		Name: c.Name, APIServer: c.APIServer,
		Description: c.Description, LastCheckAt: utcPtr(c.LastCheckAt),
		LastCheckResult: c.LastCheckResult,
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [20]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
