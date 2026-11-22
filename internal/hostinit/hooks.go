package hostinit

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// RunHooks 用 `sh -c` 执行用户钩子，空条目被忽略。
// 同一套语义用于 preHostInit（在 plan 中走 journal）与 postRuntime/postInstall。
// 钩子失败必须让 phase 失败：静默吞掉会让“安装成功”与用户预期不一致。
func RunHooks(ctx context.Context, hooks []string, report func(string, ...any)) error {
	for i, hook := range hooks {
		command := strings.TrimSpace(hook)
		if command == "" {
			continue
		}
		if report != nil {
			report("执行钩子 %d：%s", i+1, command)
		}
		hookCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		out, err := exec.CommandContext(hookCtx, "sh", "-c", command).CombinedOutput()
		cancel()
		if err != nil {
			message := strings.TrimSpace(string(out))
			if message != "" {
				return fmt.Errorf("钩子 %q 失败: %w: %s", command, err, message)
			}
			return fmt.Errorf("钩子 %q 失败: %w", command, err)
		}
	}
	return nil
}
