// Package clitest 提供命令包集成测试共用的 root command 运行器。
// 它位于 internal/cli/clitest 下，子包的测试以外部 _test 包导入它，
// 避免与 cli 根包形成导入环。
package clitest

import (
	"bytes"
	"strings"
	"testing"

	"kubemono/internal/cli"
)

// ExecuteRoot 运行完整的 kubemono 命令树并返回合并后的 stdout/stderr。
func ExecuteRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return ExecuteWithInput(t, "", args...)
}

// ExecuteWithInput 是带确定性 stdin 的 ExecuteRoot。
func ExecuteWithInput(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}
