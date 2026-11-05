package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// 本文件是 CLI 与 MCP 共用的「等待 Run」实现：
//   - 默认轮询 GET /runs/{id}；
//   - Follow=true 时先订阅 SSE（GET /runs/{id}/events），按 id: 记 Last-Event-ID 断线续传；
//   - SSE 不可用（404 / 连接失败 / Content-Type 不对 / 重连 3 次仍失败）时**降级回轮询**，
//     而不是让命令失败（Agent 友好：服务端没开 SSE 也能用）。
//
// 这里只做传输与状态事实的读取，不新增状态语义；终态集合与 outcome 映射和 tools.go 共用。

// RunReader 是 wait/follow 需要的最小接口。*Client（MCP）与 tpctl 的 client 都实现它。
type RunReader interface {
	GetRun(ctx context.Context, runID string) ([]byte, error)
	OpenRunEvents(ctx context.Context, runID string, lastEventID int64) (io.ReadCloser, error)
}

// SSEUnavailableError 表示事件流不可用；Wait 收到它会降级为轮询。
// 401/403 等鉴权错误不属于它——那类错误应原样上抛，因为轮询也会失败。
type SSEUnavailableError struct {
	Reason string
	Err    error
}

func (e *SSEUnavailableError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *SSEUnavailableError) Unwrap() error { return e.Err }

// SSEFrame 是一条 SSE 帧（空行分隔）。
type SSEFrame struct {
	Event string
	Data  []byte
	ID    string
	HasID bool
}

// ReadSSE 逐帧解析 SSE（07 §7）：`id:` / `event:` / `data:`，`:` 开头是注释（心跳）。
// 多行 data 用 \n 连接；EOF 时若有未以空行收尾的帧也会派发一次。
func ReadSSE(r io.Reader, fn func(SSEFrame) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)

	var frame SSEFrame
	var hasField bool
	flush := func() error {
		if !hasField {
			return nil
		}
		f := frame
		frame = SSEFrame{}
		hasField = false
		return fn(f)
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // 心跳 / 注释
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			frame.ID = value
			frame.HasID = value != ""
			hasField = true
		case "event":
			frame.Event = value
			hasField = true
		case "data":
			if len(frame.Data) > 0 {
				frame.Data = append(frame.Data, '\n')
			}
			frame.Data = append(frame.Data, value...)
			hasField = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// WaitOptions 是 Wait 的输入。
type WaitOptions struct {
	// Timeout 是总等待时长（含首次 GET、SSE 重连与降级后的轮询）。
	Timeout time.Duration
	// Interval 是轮询间隔；Follow 的 SSE 阶段不使用它。
	Interval time.Duration
	// Follow=true 先尝试 SSE，不可用时自动降级。
	Follow bool
	// OnStatus 只在状态**变化**时回调（含首次 GET 的当前状态）。
	OnStatus func(status string)
	// OnEvent 收到每条 SSE 帧时回调（轮询阶段不触发）。
	OnEvent func(frame SSEFrame)
	// OnFallback 在决定从 SSE 降级到轮询时回调一次，reason 可直接给用户看。
	OnFallback func(reason string)
	// MaxReconnects 是 SSE 断线重连次数，默认 3。
	MaxReconnects int
	// ReconnectBackoff 是首次重连退避，默认 1s；之后 2x 递增。
	ReconnectBackoff time.Duration
}

// WaitResult 是 Wait 的结果。Outcome 与 CLI 退出码一一对应：
// success→0、failed→1、canceled→2、timeout→3（连不上是 error，不在这里）。
type WaitResult struct {
	RunJSON        []byte
	Status         string
	Outcome        string
	Mode           string // poll | sse
	FallbackReason string
	Elapsed        time.Duration
	TimedOut       bool
}

const (
	defaultWaitTimeout   = time.Hour
	defaultWaitInterval  = 3 * time.Second
	defaultMaxReconnects = 3
	defaultReconnectBase = time.Second
	finalFetchTimeout    = 3 * time.Second
)

// Wait 等到 Run 终态或超时。返回的 error 只表示**无法继续等待**（鉴权失败、连接失败、坏 JSON）；
// 超时不是 error，用 TimedOut 表示。
func Wait(ctx context.Context, api RunReader, runID string, opts WaitOptions) (WaitResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultWaitTimeout
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultWaitInterval
	}
	if opts.MaxReconnects <= 0 {
		opts.MaxReconnects = defaultMaxReconnects
	}
	if opts.ReconnectBackoff <= 0 {
		opts.ReconnectBackoff = defaultReconnectBase
	}

	started := time.Now()
	deadline := started.Add(opts.Timeout)
	res := WaitResult{Mode: "poll"}
	tracker := &statusTracker{fn: opts.OnStatus}

	state, err := fetchRun(ctx, api, runID, deadline)
	if err != nil {
		if time.Now().After(deadline) || (errors.Is(err, context.DeadlineExceeded) && !time.Now().Before(deadline)) {
			res.TimedOut = true
			res.Outcome = "timeout"
			res.Elapsed = time.Since(started)
			return res, nil
		}
		return res, err
	}
	res.RunJSON, res.Status = state.raw, state.status
	tracker.update(state.status)
	if isTerminalRunStatus(state.status) {
		res.Outcome = runOutcome(state.status)
		res.Elapsed = time.Since(started)
		return res, nil
	}

	if opts.Follow {
		sseRes, sseErr := waitSSE(ctx, api, runID, deadline, opts, state, tracker)
		if sseErr != nil {
			return res, sseErr
		}
		if sseRes.state != nil {
			state = *sseRes.state
			res.RunJSON, res.Status = state.raw, state.status
		}
		if sseRes.fallbackReason != "" {
			res.FallbackReason = sseRes.fallbackReason
			if opts.OnFallback != nil {
				opts.OnFallback(sseRes.fallbackReason)
			}
		} else {
			res.Mode = "sse"
			res.Elapsed = time.Since(started)
			if sseRes.timedOut {
				res.TimedOut = true
				res.Outcome = "timeout"
			} else {
				res.Outcome = runOutcome(res.Status)
			}
			return res, nil
		}
		if isTerminalRunStatus(res.Status) {
			res.Outcome = runOutcome(res.Status)
			res.Elapsed = time.Since(started)
			return res, nil
		}
	}

	// 轮询阶段（Follow=false，或 SSE 降级之后）。
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := opts.Interval
		if remaining < wait {
			wait = remaining
		}
		if err := sleepCtx(ctx, wait, deadline); err != nil {
			return res, err
		}
		if !time.Now().Before(deadline) {
			break
		}
		state, err = fetchRun(ctx, api, runID, deadline)
		if err != nil {
			if !time.Now().Before(deadline) {
				break
			}
			return res, err
		}
		res.RunJSON, res.Status = state.raw, state.status
		tracker.update(state.status)
		if isTerminalRunStatus(state.status) {
			res.Outcome = runOutcome(state.status)
			res.Elapsed = time.Since(started)
			return res, nil
		}
	}

	// 超时：再做一次 best-effort 读取，把最后状态带给调用方。
	if last, err := fetchRun(context.Background(), api, runID, time.Now().Add(finalFetchTimeout)); err == nil {
		state = last
		res.RunJSON, res.Status = last.raw, last.status
		tracker.update(last.status)
		if isTerminalRunStatus(last.status) {
			res.Outcome = runOutcome(last.status)
			res.Elapsed = time.Since(started)
			return res, nil
		}
	}
	res.TimedOut = true
	res.Outcome = "timeout"
	res.Elapsed = time.Since(started)
	return res, nil
}

type sseWaitResult struct {
	state          *runState
	fallbackReason string
	timedOut       bool
}

// waitSSE 用事件流等终态。返回 fallbackReason 表示应继续轮询。
func waitSSE(ctx context.Context, api RunReader, runID string, deadline time.Time, opts WaitOptions, initial runState, tracker *statusTracker) (*sseWaitResult, error) {
	last := initial
	lastSeq := int64(0)
	reconnects := 0

	for {
		if !time.Now().Before(deadline) {
			return &sseWaitResult{state: &last, timedOut: true}, nil
		}

		reqCtx, cancel := context.WithDeadline(ctx, deadline)
		body, err := api.OpenRunEvents(reqCtx, runID, lastSeq)
		if err != nil {
			cancel()
			var unavailable *SSEUnavailableError
			if !errors.As(err, &unavailable) {
				return nil, err // 鉴权等致命错误：不要静默降级
			}
			if reconnects == 0 && lastSeq == 0 {
				return &sseWaitResult{state: &last, fallbackReason: unavailable.Reason}, nil
			}
			// 已经读到过事件或已重连：按断线处理。
			if reconnects >= opts.MaxReconnects {
				return &sseWaitResult{state: &last, fallbackReason: "SSE 重连 " + strconv.Itoa(reconnects) + " 次仍失败"}, nil
			}
			reconnects++
			if err := sleepCtx(ctx, reconnectDelay(opts.ReconnectBackoff, reconnects), deadline); err != nil {
				return &sseWaitResult{state: &last, timedOut: true}, nil
			}
			continue
		}

		finished := false
		// 只有从 seq=0 开始回放时才需要跳过历史；重连时 lastSeq>0，后续事件都是新的。
		seenInitial := lastSeq > 0
		initialStatus := initial.status
		frameErr := ReadSSE(body, func(frame SSEFrame) error {
			if frame.HasID {
				if n, err := strconv.ParseInt(strings.TrimSpace(frame.ID), 10, 64); err == nil {
					lastSeq = n
				}
			}
			if opts.OnEvent != nil {
				opts.OnEvent(frame)
			}
			switch frame.Event {
			case "run.state_changed":
				st := sseEventStatus(frame.Data)
				if st == "" {
					break
				}
				// 服务端从 seq=1 回放；在回放到"初始 GET 的状态"之前都是历史事件，
				// 不再重复打印（否则终端会出现 testing→queued→testing 的倒序噪声）。
				if !seenInitial {
					if st == initialStatus {
						seenInitial = true
					}
					break
				}
				last.status = st
				tracker.update(st)
			case "run.finished":
				finished = true
				if st := sseEventStatus(frame.Data); st != "" {
					last.status = st
					seenInitial = true
					tracker.update(st)
				}
			case "resync_required":
				// 服务端会从窗口起点继续补齐；这里只需保留 lastSeq，不做整页重建。
				// 窗口起点之后的事件都是新的，后续状态可以正常展示。
				seenInitial = true
			}
			return nil
		})
		_ = body.Close()
		cancel()

		if finished {
			if st, err := fetchRun(context.Background(), api, runID, time.Now().Add(finalFetchTimeout)); err == nil {
				last = st
			}
			return &sseWaitResult{state: &last}, nil
		}

		// 流结束 / 读失败：先用一次 GET 校准状态。
		if st, err := fetchRun(context.Background(), api, runID, time.Now().Add(finalFetchTimeout)); err == nil {
			last = st
			if isTerminalRunStatus(last.status) {
				return &sseWaitResult{state: &last}, nil
			}
		}
		if !time.Now().Before(deadline) {
			return &sseWaitResult{state: &last, timedOut: true}, nil
		}
		if reconnects >= opts.MaxReconnects {
			reason := fmt.Sprintf("SSE 断开（已重连 %d 次）", reconnects)
			if frameErr != nil {
				reason += ": " + frameErr.Error()
			}
			return &sseWaitResult{state: &last, fallbackReason: reason}, nil
		}
		reconnects++
		if err := sleepCtx(ctx, reconnectDelay(opts.ReconnectBackoff, reconnects), deadline); err != nil {
			return &sseWaitResult{state: &last, timedOut: true}, nil
		}
	}
}

type runState struct {
	raw    []byte
	status string
}

func fetchRun(ctx context.Context, api RunReader, runID string, deadline time.Time) (runState, error) {
	reqCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	raw, err := api.GetRun(reqCtx, runID)
	if err != nil {
		return runState{}, err
	}
	var v struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return runState{}, fmt.Errorf("解析 Run 失败: %w", err)
	}
	return runState{raw: raw, status: v.Status}, nil
}

type statusTracker struct {
	fn   func(string)
	last string
}

func (t *statusTracker) update(status string) {
	if status == "" || status == t.last {
		return
	}
	t.last = status
	if t.fn != nil {
		t.fn(status)
	}
}

// sseEventStatus 从事件 data 里取终态：优先信封的 to（状态转移目标），其次 payload.status。
func sseEventStatus(data []byte) string {
	var env struct {
		To      string `json:"to"`
		Payload struct {
			Status string `json:"status"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return ""
	}
	if env.To != "" {
		return env.To
	}
	return env.Payload.Status
}

func reconnectDelay(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= 30*time.Second {
			return 30 * time.Second
		}
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration, deadline time.Time) error {
	if d <= 0 {
		return nil
	}
	if remaining := time.Until(deadline); remaining < d {
		d = remaining
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
