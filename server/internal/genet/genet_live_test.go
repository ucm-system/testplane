package genet

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// TestLiveAgainstFakeGenet 用**真实客户端**打一个真的 HTTP 服务（假 Genet 或真 Genet）。
//
// 默认跳过；手工联调：
//
//	go run ./tools/fake-genet -addr :18090            # 另一个终端
//	$env:TESTPLANE_GENET_LIVE_URL="http://127.0.0.1:18090"
//	$env:TESTPLANE_GENET_API_KEY="tp-local-2026"
//	go test -run TestLiveAgainstFakeGenet -v ./internal/genet
func TestLiveAgainstFakeGenet(t *testing.T) {
	base := os.Getenv("TESTPLANE_GENET_LIVE_URL")
	if base == "" || testing.Short() {
		t.Skip("需要运行中的 Genet 兼容服务（TESTPLANE_GENET_LIVE_URL + TESTPLANE_GENET_API_KEY）")
	}
	key := os.Getenv("TESTPLANE_GENET_API_KEY")
	if key == "" {
		key = "tp-local-2026"
	}

	client, err := New(Options{
		BaseURL:             base,
		APIKey:              key,
		PollInitialInterval: time.Second,
		PollMaxInterval:     5 * time.Second,
		NotFoundGrace:       180 * time.Second,
		DeployTimeout:       10 * time.Minute,
		RequestTimeout:      10 * time.Second,
		Logf:                func(format string, args ...any) { t.Logf("client: "+format, args...) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runID := "r-live-0001"
	deployID := "d-live-0001"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 幂等：先清掉同名 release（上一次失败遗留）。
	_ = client.Cancel(ctx, runID)

	man := DeployManifest{PackageType: "stack", NoWait: boolPtr(true)}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("== Start（release=%s）==", ReleaseName(runID, deployID))
	if err := client.Start(ctx, ports.DeployRequest{RunID: runID, DeployID: deployID, Manifest: raw}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var lastPhase string
	var readyState ports.DeployState
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		st, perr := client.Poll(ctx, runID)
		if perr != nil {
			t.Fatalf("Poll: %v", perr)
		}
		if st.Phase != lastPhase {
			t.Logf("== Poll → phase=%s reason=%q ready=%v ==", st.Phase, st.Reason, st.Ready)
			lastPhase = st.Phase
		}
		if st.Phase == PhaseReady {
			readyState = st
			break
		}
		if st.Phase == PhaseBlocked {
			t.Fatalf("deploy blocked: reason=%q failure=%+v", st.Reason, st.Failure)
		}
		time.Sleep(3 * time.Second)
	}
	if readyState.Phase != PhaseReady {
		t.Fatalf("deploy never became ready (last phase=%s)", lastPhase)
	}
	t.Logf("== Ready: url=%s model=%s key=%s ==",
		readyState.Endpoint.BaseURL, readyState.Endpoint.ModelName, readyState.Endpoint.Key)

	// 取消 = DELETE；release 存在 → 应当一次成功。
	if err := client.Cancel(ctx, runID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	t.Logf("== Cancel ok（release 已卸载）==")

	// 卸载后立刻 Poll：应回到 accepted（404 在 180s 宽限内），不是 blocked。
	st, err := client.Poll(ctx, runID)
	if err != nil {
		t.Fatalf("Poll after cancel: %v", err)
	}
	t.Logf("== Poll after cancel → phase=%s reason=%q ==", st.Phase, st.Reason)
	if st.Phase == PhaseReady {
		t.Errorf("after uninstall the release must not report ready")
	}
}

func boolPtr(b bool) *bool { return &b }
