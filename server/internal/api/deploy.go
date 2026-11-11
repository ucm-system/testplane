package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 部署端点族（03 §4.4 / D-70）：平台不直连 Genet——部署由内置 service-deploy runner
// 执行，状态经 deploy-state.json 副本承载。本端点读 tasks.deploy（增量合并列）与
// attempts/<n>/deploy-state.json 归档副本，phase ∈ queued|accepted|deploying|ready|blocked；
// queue.position 恒 null（D-46）。外接模式（无部署）返回 deploy:null、pods:[]。

// handleTaskDeploy 是 GET /api/v1/tasks/{task_id}/deploy（openapi DeploySnapshot）。
func (s *Server) handleTaskDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskID, ok := safePathComponent(chi.URLParam(r, "task_id"))
	if !ok {
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "非法 task id", nil, false)
		return
	}
	task, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	attemptNo := task.CurrentAttempt
	if attemptNo < 1 {
		attemptNo = 1
	}

	resp := map[string]any{
		"task_id": taskID,
		"deploy":  nil,
		"pods":    []any{},
	}
	if len(task.Deploy) > 0 {
		resp["deploy"] = json.RawMessage(task.Deploy)
	}
	// Pod 拓扑来自归档副本 deploy-state.json 的 pods[]（service-deploy 周期改写，
	// 平台按 attempt 归档，C-5.3.2）。盘上没有就明确说明原因，不静默返回空数组。
	if pods, genAt, ok := readDeployStatePods(s.cfg.StorageRoot, taskID, attemptNo); ok {
		resp["pods"] = pods
		resp["pods_generated_at"] = genAt
	} else {
		resp["pods_message"] = "deploy-state.json 尚未产生 Pod 快照：service-deploy 还在排队或尚未写入（C-5.3.2）"
	}
	resp["deploy_timeout_seconds"] = int(s.cfg.DeployTimeout.Seconds())
	writeJSON(w, http.StatusOK, resp)
}

// readDeployStatePods 读归档副本里的 pods[]（结构由 runner 契约定义，平台只透传）。
// 解析失败/缺失返回 ok=false（UI 提示"尚未产生"，不判部署失败）。
func readDeployStatePods(root, taskID string, attemptNo int) ([]any, *time.Time, bool) {
	path := filepath.Join(root, taskID, "attempts", strconv.Itoa(attemptNo), "deploy-state.json")
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil, nil, false
	}
	var doc struct {
		Pods        []any      `json:"pods"`
		GeneratedAt *time.Time `json:"generated_at"`
		UpdatedAt   *time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, false
	}
	if doc.Pods == nil {
		doc.Pods = []any{}
	}
	at := doc.GeneratedAt
	if at == nil {
		at = doc.UpdatedAt
	}
	if at != nil {
		u := at.UTC()
		at = &u
	}
	return doc.Pods, at, true
}
