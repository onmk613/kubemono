package pack

import (
	"os"
	"path/filepath"
	"testing"

	"kubemono/internal/bom"
)

// TestPlanPackDoesNotWrite：--dry-run 只出计划，不建输出目录、不下载。
func TestPlanPackDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "dist")
	plan, err := PlanPack(Options{
		BOM:       bom.MustLoad(),
		OutDir:    out,
		Archs:     []string{"amd64"},
		CacheRoot: filepath.Join(dir, "cache"),
	})
	if err != nil {
		t.Fatalf("PlanPack: %v", err)
	}
	if len(plan.Components) == 0 {
		t.Fatal("计划里没有组件")
	}
	if len(plan.Archives) != 1 || filepath.Dir(plan.Archives[0]) != out {
		t.Fatalf("归档路径不正确: %v", plan.Archives)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应创建输出目录（stat err=%v）", err)
	}
	if plan.Components[0].Action != "download" {
		t.Fatalf("空缓存时 action 应为 download，got %q", plan.Components[0].Action)
	}
}

// TestPlanPackOfflineRejectsMissing：离线 + 空缓存必须在计划阶段报错，
// 而不是等真正执行到下载才失败。
func TestPlanPackOfflineRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := PlanPack(Options{
		BOM:       bom.MustLoad(),
		OutDir:    filepath.Join(dir, "dist"),
		Archs:     []string{"amd64"},
		Offline:   true,
		CacheRoot: filepath.Join(dir, "cache"),
	})
	if err == nil {
		t.Fatal("离线 + 空缓存必须报错")
	}
}
