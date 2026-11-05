package domain

import (
	"sync"
	"time"
)

// Clock 是**业务时间**的唯一来源（05-development-view §6.5：禁止在业务代码里直调 time.Now()）。
//
// 划分标准：
//   - 业务时间 = 会写进库、参与超窗/退避/有效期判定的时间戳 → 必须走注入的 Clock；
//   - 运行时机制 = ticker/AfterFunc/context.WithTimeout 的超时基准 → 保留 time.Now()，
//     它们是调度器/网络栈的语义点，替换成可拨时钟没有意义（也无法真正"快进"等待）。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// SystemClock 是默认时钟（UTC）。
var SystemClock Clock = realClock{}

// FixedClock 是测试用的可拨时钟（不导出到生产装配路径，仅在 _test 里 new）。
type FixedClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFixedClock 构造一个起始时刻固定的时钟。
func NewFixedClock(at time.Time) *FixedClock { return &FixedClock{now: at.UTC()} }

// Now 返回当前（可控）时间。
func (c *FixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 把时钟向前拨。
func (c *FixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set 直接把时钟设到某个时刻。
func (c *FixedClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at.UTC()
}
