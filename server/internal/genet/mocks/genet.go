// Package mocks 提供 Deploy 阶段的假实现（MVP：不联网，按配置模拟耗时与失败）。
//
// 接口按真实 Genet 适配层的形状定义（ports.Deployer，design/impl/execution.md §9），
// 因此后续替换成真实 Genet 客户端时只换实现，不动编排层。
//
// 配置（环境变量）：
//   - DEPLOY_MOCK_DURATION_SECONDS：默认 5，从受理到 ready 的总时长；
//   - DEPLOY_MOCK_FAIL=1：走失败路径（Poll 返回 error，平台据此把 Run 判 failed）；
//   - DEPLOY_MOCK_BLOCKED=1：走 blocked 路径（Run 判 service_unavailable）；
//   - TESTPLANE_MOCK_ENDPOINT_URL：默认 http://mock-llm.testplane.svc:8080/v1；
//   - DEPLOY_MOCK_MODEL_NAME / MODEL_NAME：默认 mock-model。
package mocks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// 默认值。
const (
	DefaultDuration    = 5 * time.Second
	DefaultEndpointURL = "http://mock-llm.testplane.svc:8080/v1"
	DefaultModelName   = "mock-model"
)

// 环境变量名。
const (
	EnvDuration   = "DEPLOY_MOCK_DURATION_SECONDS"
	EnvFail       = "DEPLOY_MOCK_FAIL"
	EnvBlocked    = "DEPLOY_MOCK_BLOCKED"
	EnvEndpoint   = "TESTPLANE_MOCK_ENDPOINT_URL"
	EnvModelName  = "MODEL_NAME"
	EnvModelName2 = "DEPLOY_MOCK_MODEL_NAME"
)

// Options 是假 Genet 的配置。
type Options struct {
	// Duration 是从受理到 ready 的模拟耗时。
	Duration time.Duration
	// Fail 为 true 时 Poll 返回错误（模拟平台调不通 → Run failed）。
	Fail bool
	// Blocked 为 true 时 Poll 返回 blocked（模拟后端拒绝 → Run service_unavailable）。
	Blocked bool
	// Endpoint 是 ready 时返回的接入点。
	Endpoint domain.Endpoint
	// Now 供测试注入时钟。
	Now func() time.Time
}

// Genet 是内存版 Genet（每个 run 一个部署状态）。
type Genet struct {
	mu      sync.Mutex
	opts    Options
	deploys map[string]*deployment
}

type deployment struct {
	id        string
	startedAt time.Time
	canceled  bool
}

// 确保实现契约。
var _ ports.Deployer = (*Genet)(nil)

// New 用给定配置构造。
func New(opts Options) *Genet {
	if opts.Duration <= 0 {
		opts.Duration = DefaultDuration
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Endpoint.BaseURL == "" {
		opts.Endpoint.BaseURL = DefaultEndpointURL
	}
	if opts.Endpoint.ModelName == "" {
		opts.Endpoint.ModelName = DefaultModelName
	}
	return &Genet{opts: opts, deploys: make(map[string]*deployment)}
}

// FromEnv 按环境变量构造（不带错误返回时使用默认值；非法数字退化为默认）。
func FromEnv() *Genet {
	opts := Options{
		Duration: DefaultDuration,
		Endpoint: domain.Endpoint{
			BaseURL:   envOr(EnvEndpoint, DefaultEndpointURL),
			ModelName: envFirst(EnvModelName2, EnvModelName, DefaultModelName),
		},
	}
	if raw := strings.TrimSpace(os.Getenv(EnvDuration)); raw != "" {
		if secs, err := strconv.ParseFloat(raw, 64); err == nil && secs > 0 {
			opts.Duration = time.Duration(secs * float64(time.Second))
		}
	}
	opts.Fail = envBool(EnvFail)
	opts.Blocked = envBool(EnvBlocked)
	return New(opts)
}

// Start 受理一次部署。同一 runID 重复调用是幂等的（内部幂等键 = run_id，§9）。
func (g *Genet) Start(ctx context.Context, req ports.DeployRequest) error {
	if req.RunID == "" {
		return errors.New("genet-mock: run_id is required")
	}
	if req.DeployID == "" {
		return errors.New("genet-mock: deploy_id is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.deploys[req.RunID]; ok {
		return nil
	}
	g.deploys[req.RunID] = &deployment{id: req.DeployID, startedAt: g.opts.Now()}
	return nil
}

// Poll 返回当前部署状态。状态序列 queued → deploying → ready（§C-5.3 的枚举）。
func (g *Genet) Poll(ctx context.Context, runID string) (ports.DeployState, error) {
	g.mu.Lock()
	d, ok := g.deploys[runID]
	g.mu.Unlock()
	if !ok {
		// 未 Start 就 Poll：按未受理处理，由调用方决定是否重试 Start。
		return ports.DeployState{}, fmt.Errorf("genet-mock: deployment for run %q not found: %w", runID, domain.ErrNotFound)
	}
	if g.opts.Fail {
		return ports.DeployState{}, fmt.Errorf("genet-mock: simulated Genet call failure for run %s", runID)
	}
	if d.canceled {
		return ports.DeployState{
			Phase:  "blocked",
			Reason: "canceled by platform",
		}, nil
	}
	if g.opts.Blocked {
		return ports.DeployState{
			Phase:  "blocked",
			Ready:  false,
			Reason: "backend reported blocked (simulated)",
			Failure: &domain.Failure{
				Code:      domain.CodeInternalError,
				Message:   "deploy blocked by Genet (simulated)",
				Retryable: false,
			},
		}, nil
	}

	elapsed := g.opts.Now().Sub(d.startedAt)
	switch {
	case elapsed >= g.opts.Duration:
		ep := g.opts.Endpoint
		ep.Key = endpointKey(ep)
		return ports.DeployState{Phase: "ready", Ready: true, Endpoint: ep}, nil
	}
	// 模拟的三段式进度：前 1/3 queued、中 1/3 deploying、之后 ready。
	third := g.opts.Duration / 3
	if third <= 0 {
		// 时长极短（测试里常用 1ns）：直接 ready，否则会被时钟粒度卡在 deploying。
		ep := g.opts.Endpoint
		ep.Key = endpointKey(ep)
		return ports.DeployState{Phase: "ready", Ready: true, Endpoint: ep}, nil
	}
	if elapsed >= third {
		return ports.DeployState{
			Phase:  "deploying",
			Reason: fmt.Sprintf("deploying for %s", elapsed.Truncate(time.Second)),
		}, nil
	}
	return ports.DeployState{
		Phase:  "queued",
		Reason: fmt.Sprintf("queued at position 1 for %s", elapsed.Truncate(time.Second)),
	}, nil
}

// Cancel 取消排队/部署（幂等）。
func (g *Genet) Cancel(ctx context.Context, runID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d, ok := g.deploys[runID]; ok {
		d.canceled = true
	}
	return nil
}

// DeployID 由 runID 派生（演示幂等键的用法；真实实现取 Genet 返回值）。
func DeployID(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "d-" + hex.EncodeToString(sum[:])[:8]
}

func endpointKey(ep domain.Endpoint) string {
	if ep.Key != "" {
		return ep.Key
	}
	return domain.NormalizeEndpointKey(ep.BaseURL)
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envFirst(primary, alias, def string) string {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(alias)); v != "" {
		return v
	}
	return def
}

func envBool(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}
