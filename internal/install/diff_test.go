package install

import (
	"context"
	"strings"
	"testing"

	"kubemono/internal/util/fsutil"
)

// TestRunDiffDoesNotWrite：--diff 走完整渲染路径，但不落盘、不写快照。
func TestRunDiffDoesNotWrite(t *testing.T) {
	opts := repairOptions(t)
	opts.Diff = true
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run --diff: %v", err)
	}
	if fsutil.FileExists(opts.Paths.KubeletConfig()) {
		t.Fatal("--diff 不应写 kubelet 配置")
	}
	if fsutil.FileExists(opts.Paths.Desired()) {
		t.Fatal("--diff 不应写 desired 快照目录")
	}
}

func TestShortLineDiffShowsChangedRange(t *testing.T) {
	live := []byte("a\nb\nold\nc\n")
	want := []byte("a\nb\nnew\nc\n")
	lines := shortLineDiff(live, want, 6)
	joined := ""
	for _, line := range lines {
		joined += line + "\n"
	}
	for _, wantPart := range []string{"- old", "+ new", "@@ 第 3 行"} {
		if !strings.Contains(joined, wantPart) {
			t.Fatalf("diff 缺少 %q:\n%s", wantPart, joined)
		}
	}
}
