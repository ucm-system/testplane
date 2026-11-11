package api

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/mcp"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 运维与元信息端点：/healthz /readyz /meta /me /clusters /queue-status（07 §5.5、§5.3）。

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

// agentCaps 是 /meta 的 agent 能力静态描述（agent-integration.md 的机器可读出口）。
// 全部是编译期事实，不做动态探测；mcp.tools 引用 mcp.ToolCount()（避免双写）。
func agentCaps() map[string]any {
	return map[string]any{
		"llms_txt": "/llms.txt",
		"openapi":  "/openapi.yaml",
		"mcp":      map[string]any{"transport": "stdio", "tools": mcp.ToolCount()},
		"cli":      "argus",
		"pagination": map[string]any{
			"ui":    "offset",
			"agent": "cursor",
		},
		"error_envelope": "flat:{code,message,detail,retryable,trace_id}",
	}
}

// handleMeta 是 GET /api/v1/meta（openapi Meta）：{version, api_version, features, agent_caps}。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     s.version,
		"api_version": "v1",
		"features": map[string]bool{
			"dry_run":        true,
			"events_history": true,
			"report_md":      true,
			"deploy_state":   true,
			"queue_status":   true,
		},
		"agent_caps": agentCaps(),
	})
}

// handleMe 是 GET /api/v1/me（openapi Me）：返回身份与（oidc 模式下的）CSRF token。
// 前端把 csrf_token 存进内存，写操作时作为 `X-CSRF-Token` 回填（双提交，07 §2.1）。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentIdentity(r))
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
	writeJSON(w, http.StatusOK, paged[clusterDTO]{Items: out, HasMore: false})
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

// handleQueueStatus 是 GET /api/v1/queue-status（openapi QueueStatus）：
// 平台槽位 + 等待原因分桶 + Volcano 只读快照（三层互不换算，C-4.8）。
//
// v2 口径：active 是 runner 执行记录的 preparing|running 计数（C-4.4）；
// waiting 按 waiting_reason 四值分桶（C-4.7：queued 必带原因）；
// waiting.runners 是等待中执行记录的 DB 明细（store.RunnerRuns.ListWaiting，
// 与 by_reason 同 WHERE 口径、FIFO 排序）。等待中的记录还没有 Job（Claim 时才创建），
// 天然没有 Task→Job→PodGroup 关联——明细只取真实存在的列，不造数（C-4.8.4 #4）。
func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	active, err := s.st.RunnerRuns.CountActive(ctx, nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	leases, err := s.st.Leases.List(ctx, nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	byReason, err := s.st.RunnerRuns.WaitingBreakdown(ctx, nil)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	// 明细多取 1 条探测截断；返回上限 waitingRunnersLimit（total 仍以 by_reason 为准）。
	waitingRows, err := s.st.RunnerRuns.ListWaiting(ctx, nil, waitingRunnersLimit+1)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	waitingTruncated := len(waitingRows) > waitingRunnersLimit
	if waitingTruncated {
		waitingRows = waitingRows[:waitingRunnersLimit]
	}

	maxSlots := s.cfg.MaxGlobalConcurrency
	available := maxSlots - active
	if available < 0 {
		available = 0
	}

	// 端点槽位按 endpoint_key 聚合（openapi EndpointSlot：holders 是 (task, attempt, seq)）。
	type holder struct {
		TaskID    string `json:"task_id"`
		AttemptNo int    `json:"attempt_no"`
		Seq       int    `json:"seq"`
	}
	type endpointSlot struct {
		EndpointKey string   `json:"endpoint_key"`
		Exclusive   bool     `json:"exclusive"`
		Holders     []holder `json:"holders"`
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
		slot.Holders = append(slot.Holders, holder{TaskID: l.TaskID, AttemptNo: l.AttemptNo, Seq: l.RunnerSeq})
		if l.Exclusive {
			slot.Exclusive = true
		}
	}
	endpoints := make([]endpointSlot, 0, len(order))
	for _, k := range order {
		endpoints = append(endpoints, *slotIndex[k])
	}

	// 四个键必须齐全（openapi WaitingReasonCounts additionalProperties: false）。
	reasonCounts := map[string]int{
		domain.WaitSlotWaiting:           0,
		domain.WaitQueueFull:             0,
		domain.WaitInsufficientResources: 0,
		domain.WaitGangPending:           0,
	}
	total := 0
	for k, v := range byReason {
		total += v
		if _, known := reasonCounts[k]; known {
			reasonCounts[k] = v
		}
	}

	waitingDetail := "waiting.runners 是等待中（pending/queued）执行记录的 DB 明细（最多 " +
		itoa(waitingRunnersLimit) + " 条）；这些记录还没有 Job（Claim 时才创建），因此没有 PodGroup 关联——不造数（C-4.8.4 #4）"
	if waitingTruncated {
		waitingDetail += "；明细已截断，total 以 by_reason 为准"
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
			"runners":   waitingRows,
			"detail":    waitingDetail,
		},
		"volcano": s.volcanoBlock(ctx),
	})
}

// waitingRunnersLimit 是 /queue-status 的 waiting.runners 明细返回上限。
const waitingRunnersLimit = 50

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
