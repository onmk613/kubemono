package phase

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrPollTimeout 表示等待在给定超时内没有就绪。
// 用 errors.Is 判断，调用方再补面向用户的建议文案。
var ErrPollTimeout = errors.New("等待超时")

// PollOptions 控制一次轮询等待。
type PollOptions struct {
	// Timeout 是总时长上限；<=0 表示只受次数上限与 ctx 约束。
	Timeout time.Duration
	// Interval 是两次尝试之间的等待；<=0 时取 1s。
	Interval time.Duration
	// Clock 可注入（测试用假时钟）；nil 时用 time.Now。
	Clock func() time.Time
	// Sleep 可注入（测试里通常不真正阻塞）；nil 时用 time.Sleep。
	Sleep func(time.Duration)
}

// Poll 反复执行 attempt，直到它报告完成、ctx 被取消、或超过 Timeout/次数上限。
//
// 三条语义是踩过坑的：
//   - 每轮先看 ctx：不检查时按 Ctrl-C 后仍会跑满超时（etcd 默认 5 分钟），
//     CI 取消同样无效；
//   - 次数上限同时生效：注入的 Sleep 往往不推进时钟（测试），只看时钟会死循环；
//   - attempt 的错误只记录不中止：调用方通常要"重试到超时"再带着最后一次错误
//     给出可执行的建议（journalctl/systemctl ...）。
func Poll(ctx context.Context, opts PollOptions, attempt func() (bool, error)) error {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = time.Second
	}
	var deadline time.Time
	maxAttempts := 0
	if opts.Timeout > 0 {
		deadline = clock().Add(opts.Timeout)
		if maxAttempts = int(opts.Timeout/interval) + 1; maxAttempts < 1 {
			maxAttempts = 1
		}
	}

	var lastErr error
	for attempts := 1; ; attempts++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := attempt()
		if done {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if maxAttempts > 0 && attempts >= maxAttempts {
			break
		}
		if !deadline.IsZero() && !clock().Before(deadline) {
			break
		}
		sleep(interval)
	}
	return fmt.Errorf("%w（最后错误：%v）", ErrPollTimeout, lastErr)
}
