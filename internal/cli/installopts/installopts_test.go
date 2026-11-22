package installopts

import (
	"testing"
	"time"

	"kubemono/internal/bom"
	"kubemono/internal/cli/options"
	clusterconfig "kubemono/internal/config"
)

// TestOptionsForMapsGlobalOptions 守住 init/join/node repair 共用的选项映射：
// 这些字段一旦漏传，三条命令会各自漂移（例如不复用 BOM、丢 dry-run/force）。
func TestOptionsForMapsGlobalOptions(t *testing.T) {
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	opts.Node = "m1"
	opts.AllowAddressMismatch = true
	opts.DryRun = true
	opts.Wait = 90 * time.Second

	cluster := clusterconfig.DefaultCluster()
	io, err := OptionsFor(opts, cluster, nil, false)
	if err != nil {
		t.Fatalf("OptionsFor: %v", err)
	}
	if io.Cluster != cluster {
		t.Fatal("Cluster 未原样传递")
	}
	if io.Node != "m1" || !io.AllowAddressMismatch || !io.DryRun {
		t.Fatalf("基础选项未传递：%+v", io)
	}
	if io.ForceInit {
		t.Fatal("ForceInit 应使用参数值 false")
	}
	if io.Wait != 90*time.Second {
		t.Fatalf("Wait = %s，期望 90s", io.Wait)
	}
	if io.Paths != opts.Paths() {
		t.Fatalf("Paths = %+v，期望 %+v", io.Paths, opts.Paths())
	}
	if io.Release != bom.MustLoad().Release {
		t.Fatalf("Release = %s，期望 %s", io.Release, bom.MustLoad().Release)
	}
	if io.BOM == nil || io.BOM.Release != io.Release {
		t.Fatalf("BOM 未传递或 release 不一致: %+v", io.BOM)
	}
	if io.Report == nil {
		t.Fatal("Report 未传递")
	}
	if io.Resolver == nil {
		t.Fatal("Resolver 未构造")
	}
	if io.ClientFunc == nil {
		t.Fatal("ClientFunc 未构造")
	}
	if io.JoinSecret != nil {
		t.Fatal("空 joinSecretPath 不应产生 JoinSecret")
	}
}
