package bundle

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/artifact"
	"kubemono/internal/cli/options"
)

const loadLong = `从本地 .tar.zst 导入物料库。

归档必须包含 meta.json；导入按 release 落到 /usr/local/kubemono/artifacts/<release>。
load 只做导入：release/arch/images 全部以归档 meta 为准，不联网、不构建、不选镜像。
如果本机存在 cluster.yaml，会只读对比 release 并提示差异，但不会拒绝导入。
--dry-run 只报告将写入的位置，不落盘。

归档由 kubemono bundle pack（或 make dist）生成；多个 release 可以在同一物料库并存。`

func newLoadCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		sha256    string
		noArch    bool
		overwrite bool
	)
	cmd := &cobra.Command{
		Use:   "load <archive>",
		Short: "从本地归档导入物料库",
		Long:  loadLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLoad(cmd, opts, args[0], sha256, noArch, overwrite)
		},
	}
	cmd.Flags().StringVar(&sha256, "sha256", "", "归档的期望 sha256（默认读取同目录 .sha256）")
	cmd.Flags().BoolVar(&noArch, "no-arch", false, "跳过归档目标架构检查")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "重新解包已存在的 release 物料目录")
	return options.Add(cmd, opts,
		options.DryRunFlag|options.OutputFlag|options.DataDirFlag)
}

func runLoad(cmd *cobra.Command, opts *options.GlobalOptions, archivePath, shaFlag string, noArch, overwrite bool) error {
	meta, err := artifact.ReadMetaFromArchive(archivePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("artifact: 归档 %s 不存在；先在构建机执行 `kubemono bundle pack`（或 make dist）生成并拷贝到本机", archivePath)
		}
		return err
	}
	if !noArch {
		if err := meta.CheckPlatform(runtime.GOOS, runtime.GOARCH); err != nil {
			return err
		}
	}
	warnReleaseMismatch(opts, meta)

	expected := strings.TrimSpace(shaFlag)
	if expected == "" {
		expected = readDigestSidecar(archivePath + ".sha256")
		if expected == "" {
			opts.ReporterOrDiscard().Warnf("未找到 %s.sha256，跳过归档摘要校验", archivePath)
		}
	}
	dest := releaseDir(opts, meta.Release)
	if opts.DryRun {
		if expected != "" {
			if err := checkArchiveDigest(archivePath, expected); err != nil {
				return err
			}
		}
		return printLoadResult(cmd, opts, meta, dest, archivePath, true)
	}
	installed, err := artifact.Install(cmd.Context(), archivePath, dest, artifact.InstallOptions{
		ExpectedSHA256: expected,
		Force:          overwrite,
		Reporter:       opts.ReporterOrDiscard().Infof,
	})
	if err != nil {
		return fmt.Errorf("artifact: 导入 %s 失败: %w", archivePath, err)
	}
	return printLoadResult(cmd, opts, installed, dest, archivePath, false)
}

// warnReleaseMismatch 只读对比 cluster.yaml 的生效 release，不改变导入决策。
// 没有 cluster.yaml（构建机、临时目录）时静默跳过。
func warnReleaseMismatch(opts *options.GlobalOptions, meta *artifact.Meta) {
	cfg, exists := opts.ClusterConfigPath()
	if !exists {
		return
	}
	cluster, err := opts.LoadCluster()
	if err != nil {
		opts.ReporterOrDiscard().Warnf("读取 %s 失败，跳过 release 一致性检查: %v", cfg, err)
		return
	}
	b, err := opts.EffectiveBOM(cluster)
	if err != nil {
		opts.ReporterOrDiscard().Warnf("解析 %s 的生效 BOM 失败，跳过 release 一致性检查: %v", cfg, err)
		return
	}
	if meta.Release != b.Release {
		opts.ReporterOrDiscard().Warnf(
			"物料 release=%s，cluster.yaml 生效 release=%s；init/join 仍会使用后者，请确认这是预置而非误装",
			meta.Release, b.Release)
	}
}
