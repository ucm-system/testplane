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

// TestDeploySnapshotMergesAcrossPolls 是 §4.4 回归：runs.deploy 必须是**增量合并**，
// 每次 Poll 只带变化的键，冻结字段（release_name / provider / package_type）不能被冲掉。
func TestDeploySnapshotMergesAcrossPolls(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	runID := NewRunID(time.Now())
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "deploy-merge", Kind: "run",
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

	accepted := time.Now().UTC().Truncate(time.Second)
	// 1) BeginDeploy：deployment_id / state / queued_at。
	// 1) 冻结字段（只写一次）。
	if err := st.RecordDeployState(ctx, runID, domain.DeploySnapshot{
		DeployID: "d-" + runID,
		ReleaseName: "tp-abc123456789", Provider: "genet", PackageType: "stack",
		Chart: "mock-llm", ChartVersion: "0.1.0", ValuesDigest: "sha256:deadbeef",
		AcceptedAt: &accepted, State: "accepted",
	}); err != nil {
		t.Fatalf("RecordDeployState(frozen): %v", err)
	}
	// 3) 轮询：只带变化的键。
	if err := st.RecordDeployState(ctx, runID, domain.DeploySnapshot{
		State: "queued", QueueReason: "PodGroup Inqueue: insufficient Ascend910",
		ReleaseStatus: "pending-install", EndpointStatus: "pending",
		Queue:        &domain.DeployQueue{Phase: "Inqueue", Pending: 3, Inqueue: 2, Running: 7},
		LastPolledAt: &accepted,
	}); err != nil {
		t.Fatalf("RecordDeployState(poll): %v", err)
	}
	// 4) ready：endpoint + ready_at。
	ep := domain.Endpoint{Key: "http://x:18080", BaseURL: "http://x:18080/v1", ModelName: "mock-llm"}
	if err := st.RecordDeployState(ctx, runID, domain.DeploySnapshot{
		State: "ready", Endpoint: &ep, ReadyAt: &accepted, EndpointStatus: "ready", ReleaseStatus: "deployed",
	}); err != nil {
		t.Fatalf("RecordDeployState(ready): %v", err)
	}

	run, err := st.Runs.Get(ctx, nil, runID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(run.Deploy, &doc); err != nil {
		t.Fatalf("decode deploy: %v (%s)", err, string(run.Deploy))
	}
	// 冻结字段必须还在（这就是"合并而不是覆盖"的证据）。
	for key, want := range map[string]any{
		"release_name": "tp-abc123456789",
		"provider":     "genet",
		"package_type": "stack",
		"chart":        "mock-llm",
		"chart_version": "0.1.0",
		"values_digest": "sha256:deadbeef",
		"deployment_id": "d-" + runID,
	} {
		if doc[key] != want {
			t.Errorf("%s = %v, want %v（增量合并丢了冻结字段）", key, doc[key], want)
		}
	}
	// 两根轴原值 + 队列 + 时间线 + endpoint 都在。
	for _, key := range []string{
		"genet_release_status", "endpoint_status", "queue",
		"accepted_at", "last_polled_at", "ready_at", "endpoint", "state",
	} {
		if _, ok := doc[key]; !ok {
			t.Errorf("%s missing in runs.deploy: %v", key, doc)
		}
	}
	// queue_position 恒 null。
	if v, ok := doc["queue_position"]; !ok || v != nil {
		t.Errorf("queue_position = %v (present=%v), want null", v, ok)
	}
}
