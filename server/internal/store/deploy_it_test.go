package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestRecordDeployState_QueuePositionAlwaysNull 是 D-46 回归：
// `queue_position` 键必须保留（前端/openapi 兼容），但值恒为 null ——
// Genet 只给队列水位计数，没有位次。
func TestRecordDeployState_QueuePositionAlwaysNull(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	runID := NewRunID(time.Now())
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "deploy-null", Kind: "run",
			Manifest: []byte(`{}`), Resolved: []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: []*Task{{
			ID: NewTaskID(), Seq: 0, Name: "t", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			OnFailure: domain.OnFailureContinue, TimeoutSeconds: 60, Status: string(domain.TaskPending),
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// 即使调用方传了位次，也不能落库。
	if err := st.RecordDeployState(ctx, runID, domain.DeploySnapshot{
		State:       "queued",
		QueueReason: "queue genet-shared: Insufficient Ascend910",
	}); err != nil {
		t.Fatalf("RecordDeployState: %v", err)
	}

	run, err := st.Runs.Get(ctx, nil, runID)
	if err != nil {
		t.Fatalf("读 run: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(run.Deploy, &d); err != nil {
		t.Fatalf("解析 deploy: %v (%s)", err, string(run.Deploy))
	}
	if v, ok := d["queue_position"]; !ok {
		t.Fatal("queue_position 键必须保留（D-46：删键会让前端与 openapi 不一致）")
	} else if v != nil {
		t.Fatalf("queue_position 必须恒为 null，得到 %v", v)
	}
	if d["state"] != "queued" {
		t.Fatalf("state = %v", d["state"])
	}
	if got, _ := d["queue_reason"].(string); got == "" {
		t.Fatalf("queue_reason 应保留（只展示水位/原因，不代表位次）: %+v", d)
	}
}
