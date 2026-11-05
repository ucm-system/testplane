package mocks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

func newTestGenet(t *testing.T, opts Options) (*Genet, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	opts.Now = clock.Now
	return New(opts), clock
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestGenetLifecycle(t *testing.T) {
	g, clock := newTestGenet(t, Options{
		Duration: 9 * time.Second,
		Endpoint: domain.Endpoint{BaseURL: "http://10.0.0.10:8000/v1", ModelName: "Qwen3-32B"},
	})
	ctx := context.Background()
	req := ports.DeployRequest{RunID: "r-1", DeployID: DeployID("r-1"), Timeout: time.Hour}

	if err := g.Start(ctx, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 幂等：重复 Start 不重置计时。
	if err := g.Start(ctx, req); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	state, err := g.Poll(ctx, "r-1")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if state.Phase != "queued" || state.Ready {
		t.Fatalf("state = %+v, want queued", state)
	}

	clock.Advance(4 * time.Second)
	state, _ = g.Poll(ctx, "r-1")
	if state.Phase != "deploying" {
		t.Fatalf("state = %+v, want deploying", state)
	}

	clock.Advance(6 * time.Second)
	state, err = g.Poll(ctx, "r-1")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !state.Ready || state.Phase != "ready" {
		t.Fatalf("state = %+v, want ready", state)
	}
	if state.Endpoint.BaseURL != "http://10.0.0.10:8000/v1" || state.Endpoint.ModelName != "Qwen3-32B" {
		t.Errorf("endpoint = %+v", state.Endpoint)
	}
	if state.Endpoint.Key != "http://10.0.0.10:8000" {
		t.Errorf("endpoint key = %q", state.Endpoint.Key)
	}
}

func TestGenetFailPath(t *testing.T) {
	g, _ := newTestGenet(t, Options{Fail: true})
	ctx := context.Background()
	if err := g.Start(ctx, ports.DeployRequest{RunID: "r-1", DeployID: "d-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Poll(ctx, "r-1"); err == nil {
		t.Fatal("expected simulated failure")
	}
}

func TestGenetBlockedPath(t *testing.T) {
	g, _ := newTestGenet(t, Options{Blocked: true})
	ctx := context.Background()
	if err := g.Start(ctx, ports.DeployRequest{RunID: "r-2", DeployID: "d-2"}); err != nil {
		t.Fatal(err)
	}
	state, err := g.Poll(ctx, "r-2")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "blocked" || state.Ready {
		t.Fatalf("state = %+v", state)
	}
	if state.Failure == nil || state.Failure.Code == "" {
		t.Errorf("blocked must carry a failure: %+v", state.Failure)
	}
}

func TestGenetCancel(t *testing.T) {
	g, _ := newTestGenet(t, Options{Duration: time.Second})
	ctx := context.Background()
	if err := g.Start(ctx, ports.DeployRequest{RunID: "r-3", DeployID: "d-3"}); err != nil {
		t.Fatal(err)
	}
	if err := g.Cancel(ctx, "r-3"); err != nil {
		t.Fatal(err)
	}
	state, err := g.Poll(ctx, "r-3")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "blocked" {
		t.Fatalf("state = %+v, want blocked after cancel", state)
	}
	// 取消未知 run 也幂等。
	if err := g.Cancel(ctx, "nope"); err != nil {
		t.Errorf("Cancel unknown run = %v", err)
	}
}

func TestGenetPollUnknownRun(t *testing.T) {
	g, _ := newTestGenet(t, Options{})
	if _, err := g.Poll(context.Background(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
}

func TestGenetRequiresIDs(t *testing.T) {
	g, _ := newTestGenet(t, Options{})
	if err := g.Start(context.Background(), ports.DeployRequest{}); err == nil {
		t.Fatal("expected error for empty run_id")
	}
	if err := g.Start(context.Background(), ports.DeployRequest{RunID: "r"}); err == nil {
		t.Fatal("expected error for empty deploy_id")
	}
}
