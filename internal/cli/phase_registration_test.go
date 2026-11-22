package cli

import (
	"testing"

	"kubemono/internal/phase"
)

// TestEveryCanonicalPhaseIsRegistered 验证真实 CLI 的组合根：
// internal/cli/root.go blank import internal/phase/builtin，
// builtin 再 blank import 所有 phase 实现包，init() 完成注册。
//
// 这个测试同时覆盖两件事：
//   - builtin 的 import 清单没有漏掉 CanonicalOrder 中的 phase；
//   - 生产入口（CLI）确实链接了 builtin。
//
// 不要把它放回 internal/phase：那是注册表核心，不能反向依赖实现包，
// 否则测试会形成 import cycle；也不要在 cmd/kubemono 和 builtin 各放一份。
func TestEveryCanonicalPhaseIsRegistered(t *testing.T) {
	reg := phase.Default()
	var missing []string
	for _, name := range phase.CanonicalOrder {
		if !reg.Has(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("以下 phase 未注册：%v；检查 internal/phase/builtin 的 blank import 和 internal/cli/root.go 的 builtin 导入", missing)
	}
}
