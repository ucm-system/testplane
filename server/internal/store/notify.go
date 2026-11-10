package store

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 事件发布（C-9.2、§3.7）：
//   - state_events 是唯一事件源；提交后调用 Notify(task_id, seq)；
//   - 本进程内先 fan-out 给 SSE 订阅者（低延迟）；
//   - 同时 `pg_notify` 到统一频道，多副本各自 LISTEN 后按需查库；
//   - NOTIFY 载荷只有 task_id 与 seq（避免 PG 8KB 载荷限制）。
//
// 只做"提示"，不保证不丢：SSE 侧另有 500ms 轮询兜底，丢失只影响延迟。

// 频道名常量只允许在这里定义（C-4.4 同类约束：锁名/频道名一处定义）。
const (
	notifyChannel      = "argus_task_events"
	notifyPayloadSep   = "|"
	hubQueuePerSub     = 64
	notifyQueueDepth   = 256
	notifyEmitDeadline = 2 * time.Second
)

// Notification 是一次状态变化提示。
type Notification struct {
	TaskID string
	Seq    int64
}

// Hub 是本进程内的事件广播（每个 Run 一组订阅者）。
type Hub struct {
	mu     sync.RWMutex
	nextID uint64
	subs   map[uint64]*hubSub
}

type hubSub struct {
	taskID string
	ch     chan Notification
}

// NewHub 构造广播中心。
func NewHub() *Hub { return &Hub{subs: make(map[uint64]*hubSub)} }

// Subscribe 订阅某个 Task 的通知；返回取消函数（必须调用，避免泄漏）。
func (h *Hub) Subscribe(taskID string) (<-chan Notification, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan Notification, hubQueuePerSub)
	h.subs[id] = &hubSub{taskID: taskID, ch: ch}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs, id)
			close(ch)
		})
	}
}

// Publish 把通知投递给该 Task 的订阅者；队列满时丢弃（由轮询兜底补上）。
func (h *Hub) Publish(n Notification) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subs {
		if s.taskID != n.TaskID {
			continue
		}
		select {
		case s.ch <- n:
		default:
		}
	}
}

// Notifier 实现 ports.Notifier：本进程 fan-out + 跨副本 pg_notify。
type Notifier struct {
	pool *pgxpool.Pool
	hub  *Hub
	out  chan Notification
	log  *slog.Logger
	once sync.Once
}

// NewNotifier 构造 Notifier 并启动发送 worker。
func NewNotifier(pool *pgxpool.Pool) *Notifier {
	n := &Notifier{
		pool: pool,
		hub:  NewHub(),
		out:  make(chan Notification, notifyQueueDepth),
		log:  slog.Default(),
	}
	go n.emitLoop()
	return n
}

// Hub 暴露给 API 的订阅入口。
func (n *Notifier) Hub() *Hub { return n.hub }

// Notify 在事务提交后调用（存活期内不阻塞调用方）。
func (n *Notifier) Notify(taskID string, seq int64) {
	if taskID == "" {
		return
	}
	note := Notification{TaskID: taskID, Seq: seq}
	n.hub.Publish(note)
	select {
	case n.out <- note:
	default:
		// 队列满：跨副本通知交给轮询兜底，不阻塞状态推进。
	}
}

func (n *Notifier) emitLoop() {
	for note := range n.out {
		if n.pool == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), notifyEmitDeadline)
		payload := note.TaskID + notifyPayloadSep + strconv.FormatInt(note.Seq, 10)
		_, err := n.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, notifyChannel, payload)
		cancel()
		if err != nil {
			n.log.Debug("pg_notify 失败（SSE 轮询兜底）", "err", err, "task_id", note.TaskID, "seq", note.Seq)
		}
	}
}

// StartListener 启动 LISTEN 循环（多副本各一份），ctx 取消即退出。
// 连接断开时按 1s 退避重连；事件仍可从库里补齐，不丢事实。
func (n *Notifier) StartListener(ctx context.Context) {
	go n.listenLoop(ctx)
}

func (n *Notifier) listenLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := n.listenOnce(ctx); err != nil && ctx.Err() == nil {
			n.log.Warn("LISTEN 断开，1s 后重连", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (n *Notifier) listenOnce(ctx context.Context) error {
	if n.pool == nil {
		return fmt.Errorf("store: listener 无连接池")
	}
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: listener acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return fmt.Errorf("store: listen: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "UNLISTEN "+notifyChannel)
	}()

	for {
		raw, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("store: wait notification: %w", err)
		}
		taskID, seq, ok := parseNotifyPayload(raw.Payload)
		if !ok {
			continue
		}
		n.hub.Publish(Notification{TaskID: taskID, Seq: seq})
	}
}

func parseNotifyPayload(p string) (taskID string, seq int64, ok bool) {
	idx := strings.Index(p, notifyPayloadSep)
	if idx <= 0 {
		return "", 0, false
	}
	taskID = p[:idx]
	seq, err := strconv.ParseInt(p[idx+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return taskID, seq, true
}
