package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// SSE（07 §7、C-9.2）：
//   - 回放：从 Last-Event-ID+1 开始读库，窗口 24h、单次 ≤1000 条；越窗先发 resync_required；
//   - 实时：本进程 LISTEN/NOTIFY 的 hub 立即唤醒；另有 500ms 轮询兜底（漏通知只影响延迟）；
//   - 心跳：15s 一行 `: ping`；
//   - 断开：客户端断开 / ctx 取消 / Run 已终态且事件发完。
//
// 事件只描述事实，不带凭据、不带日志正文。

const (
	sseReplayLimit = 1000
	ssePollEvery   = 500 * time.Millisecond
	ssePingEvery   = 15 * time.Second
)

// handleRunEvents 是 GET /api/v1/runs/{id}/events（SSE）。
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := chi.URLParam(r, "id")
	if _, err := s.st.Runs.Get(ctx, nil, runID); err != nil {
		s.failErr(w, r, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, r, http.StatusInternalServerError, domain.CodeInternalError,
			"响应不支持流式", nil, false)
		return
	}

	lastSeq := sseLastEventID(r)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 回放窗口：起点早于窗口下界 → 先让客户端整页重建（不静默截断）。
	if lastSeq > 0 {
		oldest, err := s.st.Events.OldestSeqSince(ctx, nil, runID, s.now().Add(-s.cfg.SSEReplayWindow))
		if err == nil && oldest > 0 && lastSeq < oldest-1 {
			writeSSE(w, store.Event{Seq: lastSeq, RunID: runID, Type: domain.EventResyncRequired,
				CreatedAt: s.now()}, domain.EventResyncRequired)
			flusher.Flush()
			lastSeq = oldest - 1
		}
	}

	var hubCh <-chan store.Notification
	if n := s.st.NotifierRef(); n != nil {
		var cancel func()
		hubCh, cancel = n.Hub().Subscribe(runID)
		defer cancel()
	}

	ping := time.NewTicker(ssePingEvery)
	defer ping.Stop()
	poll := time.NewTicker(ssePollEvery)
	defer poll.Stop()

	for {
		// 1) 把库里的新事件发出去：回放与实时共用一条路径，天然幂等。
		events, err := s.st.Events.ListByRun(ctx, nil, runID, lastSeq, sseReplayLimit)
		if err != nil {
			s.logger.Warn("SSE 读事件失败", "err", err, "run_id", runID)
		}
		for i := range events {
			ev := events[i]
			writeSSE(w, ev, ev.Type)
			lastSeq = ev.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			if len(events) >= sseReplayLimit {
				// 单次读满：先提示可能需要整页重建，随后继续补齐（不静默截断）。
				writeSSE(w, store.Event{Seq: lastSeq, RunID: runID,
					Type: domain.EventResyncRequired, CreatedAt: s.now()}, domain.EventResyncRequired)
				flusher.Flush()
			}
		}

		// 2) Run 已终态且事件发完 → 收尾断开（客户端以 SSE 为最终结果，07 §8）。
		if cur, err := s.st.Runs.Get(ctx, nil, runID); err == nil {
			if domain.RunStatus(cur.Status).IsTerminal() {
				latest, err := s.st.Events.LatestSeq(ctx, nil, runID)
				if err == nil && lastSeq >= latest {
					return
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case _, ok := <-hubCh:
			if !ok {
				hubCh = nil
			}
		case <-poll.C:
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

// sseLastEventID 从 Last-Event-ID 头或 ?last_event_id= 取起点 seq。
func sseLastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return n
	}
	return 0
}

// writeSSE 按 07 §7 的格式写一条事件：id / event / data 三行 + 空行。
func writeSSE(w http.ResponseWriter, ev store.Event, eventType string) {
	if eventType == "" {
		eventType = domain.EventRunStateChanged
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(map[string]any{
		"seq":        ev.Seq,
		"type":       eventType,
		"run_id":     ev.RunID,
		"task_id":    ev.TaskID,
		"attempt_id": ev.AttemptID,
		"from":       ev.FromStatus,
		"to":         ev.ToStatus,
		"reason":     ev.Reason,
		"actor":      ev.Actor,
		"at":         ev.CreatedAt.UTC(),
		"payload":    payload,
	})
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("id: " + strconv.FormatInt(ev.Seq, 10) + "\n"))
	_, _ = w.Write([]byte("event: " + eventType + "\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n\n"))
}
