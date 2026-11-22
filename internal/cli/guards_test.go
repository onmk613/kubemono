package cli

import (
	"strings"
	"testing"
)

// TestModeSpecificFlagsRejected 固定“参数只在某个模式下有意义”时的行为：
// 不能静默忽略，必须在开始操作前报错。
func TestModeSpecificFlagsRejected(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"version short+output", []string{"version", "--short", "--output", "json"}, "--short 与 --output"},
		{"config default table", []string{"config", "default", "--output", "table"}, "只支持 --output json|yaml"},
		{"list archive arch", []string{"bundle", "list", "/nonexistent.tar.zst", "--arch", "amd64"}, "归档模式不支持 --arch"},
		{"list archive config", []string{"bundle", "list", "/nonexistent.tar.zst", "-f", "c.yaml"}, "归档模式不支持 -f/--config"},
		{"verify archive release", []string{"bundle", "verify", "/nonexistent.tar.zst", "--release", "v1"}, "归档模式不支持 --release"},
		{"reset kubeconfig without leave", []string{"reset", "--kubeconfig", "/tmp/x", "-y"}, "--kubeconfig 只在 --leave"},
		{"reset force-leave without leave", []string{"reset", "--force-leave", "-y"}, "--force-leave 只在 --leave"},
		{"token invalid role", []string{"token", "create", "--role", "etcd"}, "只支持 worker 或 master"},
		{"repair diff + only", []string{"node", "repair", "--diff", "--only", "local-proxy"}, "--diff 与 --only"},
		{"pack fetch-images without images", []string{"bundle", "pack", "--fetch-images", "--offline"}, "--fetch-images 只配合 --images"},
		{"pack offline e fetch", []string{"bundle", "pack", "--images", "--offline", "--fetch-images"}, "冲突"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeRoot(t, tc.args...)
			if err == nil {
				t.Fatalf("kubemono %s 应该失败，实际输出:\n%s", strings.Join(tc.args, " "), out)
			}
			if !strings.Contains(err.Error(), tc.want) && !strings.Contains(out, tc.want) {
				t.Fatalf("错误信息应包含 %q，got err=%v out=%s", tc.want, err, out)
			}
		})
	}
}

// TestRequiredFlags 固定“实际必填”的参数必须由 cobra 在进入 RunE 前拦截。
func TestRequiredFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"join credentials", []string{"join"}, "token"},
		{"pki ca", []string{"pki", "issue", "--cn", "x", "--san", "x"}, "ca"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeRoot(t, tc.args...)
			if err == nil {
				t.Fatalf("kubemono %s 缺少必填参数也必须失败:\n%s", strings.Join(tc.args, " "), out)
			}
			if !strings.Contains(err.Error(), tc.want) && !strings.Contains(out, tc.want) {
				t.Fatalf("错误信息应提到 %q，got err=%v out=%s", tc.want, err, out)
			}
		})
	}
}
