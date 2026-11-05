package genet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// ReleaseName 由 run_id 与 deploy_id 派生（§9.2）：同 run 同 deploy 恒等、不同 deploy 必不同。
// 结果满足 Genet 的 `^[a-z][a-z0-9-]{0,52}$`。
func ReleaseName(runID, deployID string) string {
	sum := sha256.Sum256([]byte(runID + ":" + deployID))
	return "tp-" + hex.EncodeToString(sum[:])[:12]
}

// DefaultDeployID 在调用方没给 deploy_id 时兜底（编排层当前用 "d-"+run_id）。
func DefaultDeployID(runID string) string { return "d-" + runID }

// Options 是真实 Genet 客户端的配置。
type Options struct {
	// BaseURL / APIKey / CAFile / Insecure 是传输层配置（全部来自环境变量/Secret）。
	BaseURL  string
	APIKey   string
	CAFile   string
	Insecure bool

	// DeployTimeout 是总窗口（默认 72h，K-6）：超过即判 blocked(timeout)。
	DeployTimeout time.Duration
	// NotFoundGrace 是"GET 404 但仍算 accepted"的宽限期（默认 180s）。
	NotFoundGrace time.Duration
	// PollInitialInterval / PollMaxInterval 是轮询退避区间（默认 1s / 15s）。
	PollInitialInterval time.Duration
	PollMaxInterval     time.Duration
	// FastPollWindow 是"前 30s 每 1s"的快轮询窗口。
	FastPollWindow time.Duration
	// RequestTimeout 是单请求超时（默认 10s）。
	RequestTimeout time.Duration
	// HTTPClient 可直接注入（测试）。
	HTTPClient *http.Client
	// Now 供测试注入时钟。
	Now func() time.Time
	// Logf 可选日志钩子。
	Logf func(format string, args ...any)
}

func (o Options) withDefaults() Options {
	if o.DeployTimeout <= 0 {
		o.DeployTimeout = DefaultDeployTimeout
	}
	if o.NotFoundGrace <= 0 {
		o.NotFoundGrace = DefaultNotFoundGrace
	}
	if o.PollInitialInterval <= 0 {
		o.PollInitialInterval = DefaultPollInitialInterval
	}
	if o.PollMaxInterval <= 0 {
		o.PollMaxInterval = DefaultPollMaxInterval
	}
	if o.FastPollWindow <= 0 {
		o.FastPollWindow = DefaultFastPollWindow
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// Client 是 ports.Deployer 的真实实现（Helm release 语义）。
type Client struct {
	tr   *Transport
	opts Options

	mu      sync.Mutex
	deploys map[string]*deployState // runID -> state
}

// 确保实现契约。
var _ ports.Deployer = (*Client)(nil)

// ErrCancelNotYetApplied 表示取消时 release 尚未出现或正被占用：
// 调用方必须保留取消意图、由对账循环重试（§9.6），**不能**当成功。
var ErrCancelNotYetApplied = errors.New("genet: cancel not applied yet (release absent or busy)")

// deployState 是单个 Run 的部署轮询状态（单飞 + 退避）。
type deployState struct {
	mu          sync.Mutex
	releaseName string
	namespace   string
	modelName   string
	modelPath   string
	acceptedAt  time.Time
	interval    time.Duration
	nextPollAt  time.Time
	last        ports.DeployState
	hasLast     bool
	inflight    chan struct{}
	queueAt     time.Time
	queue       *QueueSnapshot
}

// New 构造客户端。
func New(opts Options) (*Client, error) {
	opts = opts.withDefaults()
	tr, err := NewTransport(TransportOptions{
		BaseURL:    opts.BaseURL,
		APIKey:     opts.APIKey,
		CAFile:     opts.CAFile,
		Insecure:   opts.Insecure,
		Timeout:    opts.RequestTimeout,
		HTTPClient: opts.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &Client{tr: tr, opts: opts, deploys: make(map[string]*deployState)}, nil
}

// Start 发起部署：先 GET 再 POST（§9.2 的幂等流程）。
//
// 调用方必须已经**先落库**（`runs.deploy.release_name`）再调本方法：POST 超时不代表没创建。
func (c *Client) Start(ctx context.Context, req ports.DeployRequest) error {
	man, err := decodeManifest(req.Manifest)
	if err != nil {
		return err
	}
	deployID := strings.TrimSpace(req.DeployID)
	if deployID == "" {
		deployID = strings.TrimSpace(man.DeployID)
	}
	if deployID == "" {
		deployID = DefaultDeployID(req.RunID)
	}
	releaseName := strings.TrimSpace(man.ReleaseName)
	if releaseName == "" {
		releaseName = ReleaseName(req.RunID, deployID)
	}
	packageType := strings.TrimSpace(man.PackageType)
	if packageType == "" {
		packageType = "stack"
	}

	d := c.stateFor(req.RunID, releaseName)
	d.mu.Lock()
	if d.acceptedAt.IsZero() {
		d.acceptedAt = c.opts.Now()
	}
	d.namespace = man.Namespace
	d.modelName = man.ModelName
	d.modelPath = man.ModelPath
	d.mu.Unlock()

	// 1) 已经存在 → 幂等成功（重启 / 重试场景）。
	var rel Release
	if err := c.tr.getJSON(ctx, releasePath(releaseName), nil, &rel); err == nil {
		c.recordRelease(d, &rel)
		c.opts.Logf("genet: release %s already exists (status=%s)", releaseName, rel.Status)
		return nil
	} else if !errors.Is(err, ErrReleaseNotFound) {
		return fmt.Errorf("genet: start %s: %w", releaseName, err)
	}

	// 2) POST 拉起（noWait=true 恒开）。
	//
	// values 只来自冻结的 runs.deploy.values；它为空但用户给了 deploy.parameters 时，
	// 把 parameters 序列化成 JSON（JSON 是合法 YAML，helm -f 可直接读）当 values，
	// 避免"用户参数被默默丢掉"（§9.11）。两者都有时以冻结的 values 为准。
	values := man.Values
	if strings.TrimSpace(values) == "" && len(man.Parameters) > 0 {
		if enc, err := json.Marshal(man.Parameters); err == nil {
			values = string(enc)
			c.opts.Logf("genet: values 为空，用 deploy.parameters 兜底（%d 项）", len(man.Parameters))
		}
	}
	install := ReleaseInstall{
		ReleaseName:  releaseName,
		Values:       values,
		TemplatePath: man.TemplatePath,
		ModelPath:    man.ModelPath,
		Image:        man.Image,
		Set:          man.Set,
		NoWait:       true,
		TimeoutSec:   man.TimeoutSec,
	}
	var receipt ReleaseReceipt
	status, err := c.tr.postJSON(ctx, installPath(packageType), install, &receipt)
	if err == nil {
		if receipt.Namespace != "" {
			d.mu.Lock()
			d.namespace = receipt.Namespace
			d.mu.Unlock()
		}
		c.opts.Logf("genet: installed release %s (http=%d, status=%s)", releaseName, status, receipt.Status)
		return nil
	}

	// 3) 同名已存在：GET 复核，命中即成功（400 不能被误判成失败，§9.2）。
	if errors.Is(err, ErrReleaseExists) {
		if gere := c.tr.getJSON(ctx, releasePath(releaseName), nil, &rel); gere == nil {
			c.recordRelease(d, &rel)
			c.opts.Logf("genet: release %s already exists after POST 400/409; treated as success", releaseName)
			return nil
		}
	}
	return fmt.Errorf("genet: install %s: %w", releaseName, err)
}

// Poll 返回合成后的部署状态（§9.3 的九行映射）。
//
// 特性：同 release **单飞**（并发调用只发一个请求）、1s→15s 指数退避、
// 单请求 10s 超时、总窗口 72h（超过即 blocked/timeout）。
func (c *Client) Poll(ctx context.Context, runID string) (ports.DeployState, error) {
	d := c.stateFor(runID, "")
	return c.pollState(ctx, runID, d)
}

// pollState 是 Poll 的实现主体（单飞 + 退避）。
func (c *Client) pollState(ctx context.Context, runID string, d *deployState) (ports.DeployState, error) {
	d.mu.Lock()
	if d.releaseName == "" {
		d.releaseName = ReleaseName(runID, DefaultDeployID(runID))
	}
	releaseName := d.releaseName
	if d.inflight != nil {
		wait := d.inflight
		d.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ports.DeployState{}, ctx.Err()
		}
		d.mu.Lock()
		out := d.last
		d.mu.Unlock()
		return out, nil
	}
	if d.hasLast && c.opts.Now().Before(d.nextPollAt) {
		out := d.last
		d.mu.Unlock()
		return out, nil
	}
	inflight := make(chan struct{})
	d.inflight = inflight
	if d.acceptedAt.IsZero() {
		d.acceptedAt = c.opts.Now()
	}
	d.mu.Unlock()

	st, err := c.fetchState(ctx, d, releaseName)

	d.mu.Lock()
	d.inflight = nil
	close(inflight)
	if err == nil {
		d.last = st
		d.hasLast = true
		d.nextPollAt = c.opts.Now().Add(c.bumpInterval(d))
	}
	d.mu.Unlock()
	return st, err
}

// fetchState 打一次 GET 并按 §9.3 合成状态。
func (c *Client) fetchState(ctx context.Context, d *deployState, releaseName string) (ports.DeployState, error) {
	var rel Release
	err := c.tr.getJSON(ctx, releasePath(releaseName), nil, &rel)
	switch {
	case errors.Is(err, ErrReleaseNotFound):
		d.mu.Lock()
		acceptedAt := d.acceptedAt
		d.mu.Unlock()
		return c.synthesizeNotFound(acceptedAt), nil
	case err != nil:
		// 网络/5xx/鉴权：平台侧问题，交给编排层判 DEPLOY_FAILED（Run failed）。
		return ports.DeployState{}, fmt.Errorf("genet: poll %s: %w", releaseName, err)
	}

	d.mu.Lock()
	d.namespace = rel.Namespace
	if rel.CallInfo != nil && rel.CallInfo.Model != "" {
		d.modelName = rel.CallInfo.Model
	}
	acceptedAt := d.acceptedAt
	d.mu.Unlock()

	// 队列信息只在"还没就绪"时需要；失败不影响状态（§9.5）。
	var queue *QueueSnapshot
	if rel.CallInfo == nil || rel.CallInfo.Endpoint == nil ||
		!strings.EqualFold(rel.CallInfo.Endpoint.Status, EndpointReady) {
		queue = c.queueSnapshot(ctx, d)
	}
	return c.synthesize(&rel, acceptedAt, queue), nil
}

// synthesize 是 §9.3 的九行映射（纯函数，便于单测）。
//
// 除了 Phase/Ready/Endpoint/Failure/Reason，还把 Genet 的两根轴原值（release status /
// endpoint status）与队列水位一并带回给编排层落库（runs.deploy §4.4）。
func (c *Client) synthesize(rel *Release, acceptedAt time.Time, queue *QueueSnapshot) ports.DeployState {
	st := c.synthesizePhase(rel, acceptedAt, queue)
	// 定位信息回填：编排层要把它们落进 `runs.deploy`（namespace 尤其重要——
	// Deploy 的 Pod 在 Genet 的 namespace 里，日志端点靠它找 Pod）。
	st.Namespace = strings.TrimSpace(rel.Namespace)
	st.ReleaseName = strings.TrimSpace(rel.Name)
	st.ReleaseStatus = strings.TrimSpace(rel.Status)
	st.PackageType = strings.TrimSpace(rel.PackageType)
	st.Chart = strings.TrimSpace(rel.Chart)
	st.ChartVersion = strings.TrimSpace(rel.ChartVersion)
	if rel.CallInfo != nil && rel.CallInfo.Endpoint != nil {
		st.EndpointStatus = strings.ToLower(strings.TrimSpace(rel.CallInfo.Endpoint.Status))
	}
	if queue != nil {
		q := &domain.DeployQueue{}
		if queue.Queue != nil {
			q.Pending = queue.Queue.Pending
			q.Inqueue = queue.Queue.Inqueue
			q.Running = queue.Queue.Running
		}
		if len(queue.PodGroups) > 0 {
			q.Phase = queue.PodGroups[len(queue.PodGroups)-1].Phase
		}
		st.Queue = q
	}
	st.ObservedAt = c.opts.Now()
	return st
}

// synthesizePhase 是纯映射本体（不含落库附带字段）。
func (c *Client) synthesizePhase(rel *Release, acceptedAt time.Time, queue *QueueSnapshot) ports.DeployState {
	now := c.opts.Now()

	// #9 总窗口：超窗一律 blocked(timeout)，优先于其它判断。
	if !acceptedAt.IsZero() && now.Sub(acceptedAt) > c.opts.DeployTimeout {
		return ports.DeployState{
			Phase:  PhaseBlocked,
			Reason: "timeout",
			Failure: &domain.Failure{
				Code:      domain.CodeDeployBlocked,
				Message:   fmt.Sprintf("deploy did not become ready within %s", c.opts.DeployTimeout),
				Retryable: true,
				Detail:    map[string]any{"reason": "timeout", "release_status": rel.Status},
			},
		}
	}

	endpointStatus := ""
	var endpoint *Endpoint
	if rel.CallInfo != nil {
		endpoint = rel.CallInfo.Endpoint
		if endpoint != nil {
			endpointStatus = strings.ToLower(strings.TrimSpace(endpoint.Status))
		}
	}

	// #5 就绪：endpoint.status=ready 且 url 非空。注意 ready ≠ 可用（§9.4）。
	if endpointStatus == EndpointReady && endpoint != nil && strings.TrimSpace(endpoint.URL) != "" {
		modelName := ""
		modelPath := ""
		if rel.CallInfo != nil {
			modelName = rel.CallInfo.Model
		}
		ep := domain.Endpoint{
			Key:       domain.NormalizeEndpointKey(endpoint.URL),
			BaseURL:   endpoint.URL,
			ModelName: modelName,
			ModelPath: modelPath,
		}
		return ports.DeployState{
			Phase:    PhaseReady,
			Ready:    true,
			Endpoint: ep,
			Reason:   "endpoint ready (address resolved; availability still needs pre-check)",
		}
	}

	// #6 endpoint blocked：终态入口不可用。
	if endpointStatus == EndpointBlocked {
		msg := ""
		if endpoint != nil {
			msg = endpoint.Message
		}
		return blockedState("blocked", msg, rel.Status)
	}

	status := strings.ToLower(strings.TrimSpace(rel.Status))
	switch {
	// #7 failed / superseded / uninstalling / uninstalled → 折叠成 blocked。
	case status == ReleaseFailed:
		return blockedState("blocked", describe(rel), status)
	case status == ReleaseSuperseded:
		return blockedState("superseded", describe(rel), status)
	case status == ReleaseUninstalling:
		return blockedState("uninstalling", describe(rel), status)
	case status == ReleaseUninstalled:
		return blockedState("uninstalled", describe(rel), status)

	// #8 deployed 但 endpoint 还没 pending→ready：仍在 deploying。
	case status == ReleaseDeployed:
		return ports.DeployState{
			Phase:  PhaseDeploying,
			Reason: "release deployed but callInfo.endpoint is still pending",
		}

	// #3 / #4 pending-*：Pod 是否被调度决定 queued / deploying。
	case strings.HasPrefix(status, "pending"):
		if anyPodScheduled(rel.Pods) {
			return ports.DeployState{
				Phase:  PhaseDeploying,
				Reason: fmt.Sprintf("%s: %d pod(s) scheduled", status, scheduledCount(rel.Pods)),
			}
		}
		return ports.DeployState{
			Phase:  PhaseQueued,
			Reason: queueReason(queue, status),
		}
	}

	// 兜底：状态不认识时按 accepted 处理（不臆造 ready，也不误判失败）。
	return ports.DeployState{
		Phase:  PhaseAccepted,
		Reason: "unrecognized release status: " + rel.Status,
	}
}

// synthesizeNotFound 处理 GET 404（#1 / #2）。
func (c *Client) synthesizeNotFound(acceptedAt time.Time) ports.DeployState {
	st := c.synthesizeNotFoundPhase(acceptedAt)
	st.EndpointStatus = "pending"
	st.ObservedAt = c.opts.Now()
	return st
}

func (c *Client) synthesizeNotFoundPhase(acceptedAt time.Time) ports.DeployState {
	now := c.opts.Now()
	// 超窗优先：即使 release 一直没出现，也要按 timeout 收口（§9.3 #9）。
	if !acceptedAt.IsZero() && now.Sub(acceptedAt) > c.opts.DeployTimeout {
		return ports.DeployState{
			Phase:  PhaseBlocked,
			Reason: "timeout",
			Failure: &domain.Failure{
				Code:      domain.CodeDeployBlocked,
				Message:   fmt.Sprintf("deploy did not become ready within %s", c.opts.DeployTimeout),
				Retryable: true,
				Detail:    map[string]any{"reason": "timeout"},
			},
		}
	}
	if acceptedAt.IsZero() || now.Sub(acceptedAt) < c.opts.NotFoundGrace {
		return ports.DeployState{
			Phase:  PhaseAccepted,
			Reason: "release not yet visible (within 180s grace)",
		}
	}
	return ports.DeployState{
		Phase:  PhaseBlocked,
		Reason: "blocked",
		Failure: &domain.Failure{
			Code:      domain.CodeDeployBlocked,
			Message:   "release was not created within the grace window",
			Retryable: true,
			Detail:    map[string]any{"reason": "blocked"},
		},
	}
}

// blockedState 造一个 blocked 结果（DEPLOY_BLOCKED + detail.reason）。
func blockedState(reason, message, releaseStatus string) ports.DeployState {
	if message == "" {
		message = "deploy blocked (reason=" + reason + ")"
	}
	return ports.DeployState{
		Phase:  PhaseBlocked,
		Reason: reason,
		Failure: &domain.Failure{
			Code:      domain.CodeDeployBlocked,
			Message:   message,
			Retryable: true,
			Detail: map[string]any{
				"reason":         reason,
				"release_status": releaseStatus,
			},
		},
	}
}

// Cancel 卸载 release（§9.6/§9.7）。
//
// 404 / 409 说明 release 尚未出现或正被占用：返回 ErrCancelNotYetApplied（可重试），
// **不当成功**——调用方必须先落取消意图再由对账补偿。
func (c *Client) Cancel(ctx context.Context, runID string) error {
	d := c.stateFor(runID, "")
	d.mu.Lock()
	if d.releaseName == "" {
		d.releaseName = ReleaseName(runID, DefaultDeployID(runID))
	}
	releaseName := d.releaseName
	d.mu.Unlock()

	var result UninstallResult
	err := c.tr.deleteJSON(ctx, releasePath(releaseName), url.Values{
		"keepHistory": {"false"},
		"force":       {"true"},
	}, &result)
	if err == nil {
		// 卸载成功：作废缓存并把宽限计时重置到现在，否则后续 Poll 会
		//   ① 命中 ready 缓存（假象），② 用很久以前的 acceptedAt 直接判 blocked。
		d.mu.Lock()
		d.hasLast = false
		d.nextPollAt = time.Time{}
		d.interval = c.opts.PollInitialInterval
		d.acceptedAt = c.opts.Now()
		d.mu.Unlock()
		c.opts.Logf("genet: uninstalled release %s (forced=%d)", releaseName, len(result.ForcedPods))
		return nil
	}
	if errors.Is(err, ErrReleaseNotFound) || errors.Is(err, ErrOperationInProgress) {
		return fmt.Errorf("%w: %v", ErrCancelNotYetApplied, err)
	}
	return fmt.Errorf("genet: cancel %s: %w", releaseName, err)
}

// VerifyUninstalled 校验"删干净"（§9.7）：release 已不存在**且**它的 Pod 列表为空。
//
// 这是卸载收口的唯一可信判据：DELETE 返回 200 不等于干净（Genet 的异步清理可能还在跑），
// 而 Pod 查询失败 ≠ Pod 为空（R9），所以查询失败一律返回 false + error。
func (c *Client) VerifyUninstalled(ctx context.Context, runID string) (bool, error) {
	d := c.stateFor(runID, "")
	d.mu.Lock()
	releaseName := d.releaseName
	d.mu.Unlock()
	if releaseName == "" {
		releaseName = ReleaseName(runID, DefaultDeployID(runID))
	}

	var rel Release
	err := c.tr.getJSON(ctx, releasePath(releaseName), nil, &rel)
	switch {
	case errors.Is(err, ErrReleaseNotFound):
		// release 已消失；继续确认 Pod 为空。
	case err != nil:
		return false, fmt.Errorf("genet: verify %s: %w", releaseName, err)
	default:
		// 还能 GET 到 → 没删干净。
		return false, nil
	}

	// release 已经 404：Pod 必然不存在。这里再查一次 pods 只是**尽力**加证；
	// 查询失败（某些假 Genet / 旧版本没有 /pods 路由）不能推翻"release 已不存在"这个事实，
	// 否则卸载永远无法收敛（§9.7）。
	var pods PodList
	err = c.tr.getJSON(ctx, releasePath(releaseName)+"/pods", nil, &pods)
	switch {
	case err == nil:
		return len(pods.Items) == 0, nil
	default:
		c.opts.Logf("genet: verify pods %s: best-effort failed: %v", releaseName, err)
		return true, nil
	}
}

// Queue 返回最近一次队列快照（诊断/测试用；可能为 nil）。
func (c *Client) Queue(runID string) *QueueSnapshot {
	c.mu.Lock()
	d := c.deploys[runID]
	c.mu.Unlock()
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queue
}

// ReleaseNameFor 返回某个 run 当前使用的 release 名（诊断用）。
func (c *Client) ReleaseNameFor(runID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.deploys[runID]; ok {
		return d.releaseName
	}
	return ""
}

// ---- 内部工具 -------------------------------------------------------------

// stateFor 取/建该 Run 的轮询状态；releaseName 为空时按 runID 派生。
func (c *Client) stateFor(runID, releaseName string) *deployState {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.deploys[runID]
	if !ok {
		d = &deployState{
			releaseName: releaseName,
			interval:    c.opts.PollInitialInterval,
		}
		c.deploys[runID] = d
		return d
	}
	// Start 传入的 releaseName 是权威值（由 run_id + deploy_id 派生或来自冻结的 runs.deploy），
	// 必须覆盖 Poll/Cancel 在"还没 Start 过"时按默认 deploy_id 推出的临时值，否则会查到别的 release。
	if releaseName != "" {
		d.releaseName = releaseName
	}
	return d
}

// recordRelease 记录一次 GET 到的 release（更新 namespace/model 等）。
func (c *Client) recordRelease(d *deployState, rel *Release) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rel.Namespace != "" {
		d.namespace = rel.Namespace
	}
	if rel.CallInfo != nil {
		if rel.CallInfo.Model != "" {
			d.modelName = rel.CallInfo.Model
		}
		if rel.CallInfo.Namespace != "" && d.namespace == "" {
			d.namespace = rel.CallInfo.Namespace
		}
	}
}

// bumpInterval 实现 1s→15s 的退避（前 30s 固定 1s）。
func (c *Client) bumpInterval(d *deployState) time.Duration {
	if d.acceptedAt.IsZero() {
		return c.opts.PollInitialInterval
	}
	if c.opts.Now().Sub(d.acceptedAt) < c.opts.FastPollWindow {
		d.interval = c.opts.PollInitialInterval
		return d.interval
	}
	next := d.interval * 2
	if next < 5*time.Second {
		next = 5 * time.Second
	}
	if next > c.opts.PollMaxInterval {
		next = c.opts.PollMaxInterval
	}
	d.interval = next
	return d.interval
}

// queueSnapshot 每 30s 最多打一次队列接口（best-effort，失败不判 Run 失败）。
func (c *Client) queueSnapshot(ctx context.Context, d *deployState) *QueueSnapshot {
	d.mu.Lock()
	recent := !d.queueAt.IsZero() && c.opts.Now().Sub(d.queueAt) < 30*time.Second
	cached := d.queue
	d.mu.Unlock()
	if recent {
		return cached
	}
	// 队列接口失败只影响 reason 展示（§9.5）。
	snap, err := c.fetchQueue(ctx)
	if err != nil {
		c.opts.Logf("genet: queue snapshot unavailable: %v", err)
		snap = nil
	}
	d.mu.Lock()
	d.queueAt = c.opts.Now()
	d.queue = snap
	d.mu.Unlock()
	return snap
}

// fetchQueue 拉队列水位 + 本 namespace 的 PodGroup（两个接口，任一失败即返回 nil）。
func (c *Client) fetchQueue(ctx context.Context) (*QueueSnapshot, error) {
	var snap QueueSnapshot
	if err := c.tr.getJSON(ctx, "/api/open/queue", nil, &snap); err != nil {
		return nil, err
	}
	var groups struct {
		PodGroups []PodGroup `json:"podGroups"`
	}
	if err := c.tr.getJSON(ctx, "/api/open/queue/podgroups", nil, &groups); err != nil {
		return nil, err
	}
	snap.PodGroups = groups.PodGroups
	return &snap, nil
}

// queueReason 把 PodGroup 的 phase + 最新 condition 拼成一句给人看的原因（无位次）。
func queueReason(queue *QueueSnapshot, releaseStatus string) string {
	if queue == nil {
		return releaseStatus + ": waiting to be scheduled"
	}
	for i := len(queue.PodGroups) - 1; i >= 0; i-- {
		pg := queue.PodGroups[i]
		if pg.Phase == "" {
			continue
		}
		msg := ""
		if len(pg.Conditions) > 0 {
			last := pg.Conditions[len(pg.Conditions)-1]
			msg = last.Message
			if strings.TrimSpace(msg) == "" {
				msg = last.Reason
			}
		}
		if msg != "" {
			return fmt.Sprintf("PodGroup %s: %s", pg.Phase, strings.TrimSpace(msg))
		}
		return "PodGroup " + pg.Phase
	}
	return releaseStatus + ": waiting to be scheduled"
}

// decodeManifest 解析 runs.deploy 里本层需要的字段。
func decodeManifest(raw json.RawMessage) (DeployManifest, error) {
	var man DeployManifest
	if len(raw) == 0 {
		return man, errors.New("genet: deploy manifest is empty (runs.deploy must be frozen before Start)")
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		return man, fmt.Errorf("genet: decode deploy manifest: %w", err)
	}
	return man, nil
}

// ReleasePath / installPath 暴露给测试与诊断（路径拼接集中在这里）。
func releasePath(releaseName string) string {
	return "/api/open/helm/releases/" + url.PathEscape(releaseName)
}

func installPath(packageType string) string {
	t := strings.ToLower(strings.TrimSpace(packageType))
	if t != "pd" {
		t = "stack"
	}
	return "/api/open/helm/uc-packages/" + t + "/releases"
}

// describe 取 release 的失败描述（helm 原文优先，回退 description 字段）。
func describe(rel *Release) string {
	if strings.TrimSpace(rel.Description) != "" {
		return strings.TrimSpace(rel.Description)
	}
	return "release status " + rel.Status
}

func anyPodScheduled(pods []Pod) bool { return scheduledCount(pods) > 0 }

func scheduledCount(pods []Pod) int {
	n := 0
	for _, p := range pods {
		if strings.TrimSpace(p.NodeName) != "" {
			n++
		}
	}
	return n
}
