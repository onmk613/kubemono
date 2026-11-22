package phase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPollStopsOnContextCancel 守住"Ctrl-C 立刻生效"：
// 三个等待循环（etcd 健康、etcd promote、runtime 就绪）都必须检查 ctx；
// 否则取消后仍会跑满超时（etcd 默认 5 分钟）。
func TestPollStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := Poll(ctx, PollOptions{
		Timeout:  time.Hour,
		Interval: time.Millisecond,
		Sleep:    func(time.Duration) {},
	}, func() (bool, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v，期望 context.Canceled", err)
	}
	if calls > 4 {
		t.Fatalf("取消后不应继续轮询，实际 attempt %d 次", calls)
	}
}

// TestPollTimeoutCarriesLastError 超时错误必须带上最后一次失败原因，
// 否则运维只看到"没就绪"，没有 journalctl 的线索。
func TestPollTimeoutCarriesLastError(t *testing.T) {
	now := time.Unix(0, 0)
	err := Poll(context.Background(), PollOptions{
		Timeout:  5 * time.Second,
		Interval: time.Second,
		Clock:    func() time.Time { return now },
		Sleep:    func(d time.Duration) { now = now.Add(d) },
	}, func() (bool, error) {
		return false, errors.New("dial unix /run/containerd/containerd.sock: no such file")
	})
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v，期望 ErrPollTimeout", err)
	}
	if !strings.Contains(err.Error(), "dial unix") {
		t.Fatalf("超时错误应带上最后一次错误，实际: %v", err)
	}
}

// TestPollIsBoundedWhenSleepDoesNotAdvanceClock 注入的 Sleep 不推进时钟时
// （测试替身常见），必须靠次数上限停下，否则就是死循环。
func TestPollIsBoundedWhenSleepDoesNotAdvanceClock(t *testing.T) {
	calls := 0
	err := Poll(context.Background(), PollOptions{
		Timeout:  5 * time.Millisecond,
		Interval: time.Millisecond,
		Sleep:    func(time.Duration) {},
	}, func() (bool, error) {
		calls++
		return false, nil
	})
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v，期望 ErrPollTimeout", err)
	}
	if calls > 10 {
		t.Fatalf("轮询必须有界，实际 %d 次", calls)
	}
}

// TestPollReturnsOnDone 成功后立刻返回，不多轮询。
func TestPollReturnsOnDone(t *testing.T) {
	calls := 0
	err := Poll(context.Background(), PollOptions{
		Timeout:  time.Second,
		Interval: time.Millisecond,
		Sleep:    func(time.Duration) {},
	}, func() (bool, error) {
		calls++
		return calls == 2, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 2 {
		t.Fatalf("成功应立即返回，实际 attempt %d 次", calls)
	}
}
