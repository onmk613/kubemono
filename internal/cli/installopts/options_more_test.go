package installopts

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	clusterconfig "kubemono/internal/config"
	installpkg "kubemono/internal/install"
	"kubemono/internal/util/output"
)

func TestOptionsForCarriesJoinSecret(t *testing.T) {
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	cluster := clusterconfig.DefaultCluster()

	join := &clusterconfig.JoinSecret{
		Role:  clusterconfig.RoleWorker,
		Token: "abcdef.0123456789abcdef",
	}
	io, err := OptionsFor(opts, cluster, join, false)
	if err != nil {
		t.Fatalf("OptionsFor: %v", err)
	}
	if io.JoinSecret != join {
		t.Fatalf("JoinSecret 未原样传递: %+v", io.JoinSecret)
	}
}

func TestRunInstallDryRunDoesNotExecute(t *testing.T) {
	cluster := clusterconfig.DefaultCluster()
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	opts.Node = cluster.Cluster.Hosts[0].Name
	opts.AllowAddressMismatch = true
	opts.DryRun = true

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	result, ran, err := RunInstall(cmd, opts, cluster, nil, false, installpkg.ActionInit, nil, nil)
	if err != nil {
		t.Fatalf("RunInstall dry-run: %v", err)
	}
	if ran {
		t.Fatal("dry-run 不应执行 phase")
	}
	if result != nil {
		t.Fatalf("dry-run 不应返回执行结果: %+v", result)
	}
}

// TestRunInstallPlanFollowsProgressStream 锁定“stdout 只留正文”的前提：
// 进度改到 stderr 时，安装计划也不能再写到 stdout，否则 join 命令会被污染。
func TestRunInstallPlanFollowsProgressStream(t *testing.T) {
	cluster := clusterconfig.DefaultCluster()
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	opts.Node = cluster.Cluster.Hosts[0].Name
	opts.AllowAddressMismatch = true
	opts.DryRun = true
	opts.Reporter = &output.Console{Out: io.Discard, Err: io.Discard}
	opts.Reporter.SetProgressToStderr(true)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if _, _, err := RunInstall(cmd, opts, cluster, nil, false, installpkg.ActionInit, nil, nil); err != nil {
		t.Fatalf("RunInstall: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("计划不应写到 stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "执行顺序") {
		t.Fatalf("计划应写到 stderr:\n%s", stderr.String())
	}
}
