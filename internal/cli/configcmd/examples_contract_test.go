package configcmd_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/cli/clitest"
)

// captureStdout 临时把进程 os.Stdout 接到管道上并返回还原函数。
// 为什么不用 cobra 的 out 缓冲：reporter（output.NewConsole）在命令构造时
// 捕获的是当时的 os.Stdout，进度与 Result 文案直写进程标准输出，
// 只有替换 os.Stdout 才能断言“校验通过”这类文案。
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	return func() string {
		os.Stdout = old
		_ = w.Close()
		data, _ := io.ReadAll(r)
		_ = r.Close()
		return string(data)
	}
}

// TestExamplesClusterYAMLValidates 把 examples/cluster.yaml 当作发布契约：
// 任何让示例校验失败的改动都必须同步维护示例，否则这里立即失败。
// 纯校验不依赖 root、不写 /etc：-f 显式指定文件，options.Default 也不读默认路径。
func TestExamplesClusterYAMLValidates(t *testing.T) {
	// go test 的工作目录是本包目录 internal/cli/configcmd，相对路径向上三级回到仓库根。
	example := filepath.Join("..", "..", "..", "examples", "cluster.yaml")
	if _, err := os.Stat(example); err != nil {
		t.Fatalf("缺少示例 %s: %v", example, err)
	}

	restore := captureStdout(t)
	out, err := clitest.ExecuteRoot(t, "config", "validate", "-f", example)
	stdout := restore()

	if err != nil {
		t.Fatalf("examples/cluster.yaml 未通过校验（示例已腐烂，请同步修复示例）: %v\nstdout:\n%s\ncmd out:\n%s", err, stdout, out)
	}
	if !strings.Contains(stdout, "校验通过") {
		t.Fatalf("输出缺少“校验通过”文案：\nstdout:\n%s\ncmd out:\n%s", stdout, out)
	}
	// 表格摘要应能看到示例集群的关键信息，防止输出被静默改成空壳。
	for _, want := range []string{"masters", "kube-router"} {
		if !strings.Contains(out, want) {
			t.Errorf("校验摘要缺少 %q：\n%s", want, out)
		}
	}
}
