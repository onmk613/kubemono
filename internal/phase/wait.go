package phase

import "time"

// WaitKnobs 统一各 phase 等待循环的可注入参数。
// "timeout/interval/sleep/clock"这组回退统一在这里解析，etcd、runtime、kubelet、
// markcontrolplane 四个 phase 共用，避免新增 phase 再抄一份。
type WaitKnobs struct {
	// Timeout 是等待上限；<=0 时用调用方给的默认值（各 phase 不同）。
	Timeout time.Duration
	// Interval 是两次尝试之间的间隔；<=0 时用调用方给的默认值。
	Interval time.Duration
	// Sleep 等待（可注入，测试里通常不真正阻塞）；nil 用 time.Sleep。
	Sleep func(time.Duration)
	// Clock 返回当前时间（可注入）；nil 时依次回退 Context.Now、time.Now。
	Clock func() time.Time
}

// PollOptions 把 knobs 解析成 Poll 需要的形态：填默认值、回退注入。
func (w WaitKnobs) PollOptions(c *Context, defaultTimeout, defaultInterval time.Duration) PollOptions {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	return PollOptions{
		Timeout:  timeout,
		Interval: interval,
		Sleep:    w.Sleep,
		Clock:    w.ResolveClock(c),
	}
}

// ResolveClock 返回生效的时钟。
func (w WaitKnobs) ResolveClock(c *Context) func() time.Time {
	switch {
	case w.Clock != nil:
		return w.Clock
	case c != nil && c.Now != nil:
		return c.Now
	default:
		return time.Now
	}
}

// TimeoutOrDefault 返回生效的超时（<=0 时取默认）。
func (w WaitKnobs) TimeoutOrDefault(defaultTimeout time.Duration) time.Duration {
	if w.Timeout > 0 {
		return w.Timeout
	}
	return defaultTimeout
}

// IntervalOrDefault 返回生效的间隔（<=0 时取默认）。
func (w WaitKnobs) IntervalOrDefault(defaultInterval time.Duration) time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultInterval
}

// ResolveSleep 返回生效的 Sleep。
func (w WaitKnobs) ResolveSleep() func(time.Duration) {
	if w.Sleep != nil {
		return w.Sleep
	}
	return time.Sleep
}
