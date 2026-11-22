package bundle

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/artifact"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/util/fsutil"
)

const verifyLong = `校验物料是否齐备：文件在不在、大小/权限/内容哈希对不对、二进制架构对不对。

带 .tar.zst 参数时只读归档、不解包，是 verify 的主用法；
不带参数时检查本机物料库（需要 -f cluster.yaml 或 --release），属于导入后的便利检查。
完整性由归档摘要 + meta 里的逐文件 sha256 保证；没有 sha256 的文件只校验大小和权限。`

func newVerifyCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		release string
		sha256  string
		noHash  bool
		noArch  bool
	)
	cmd := &cobra.Command{
		Use:   "verify [<archive>]",
		Short: "校验归档或本机物料库的齐备性",
		Long:  verifyLong,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				for _, name := range []string{"release", "config"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("归档模式不支持 --%s（只读归档 meta；库模式才需要它）", name)
					}
				}
			}
			return runVerify(cmd, opts, args, release, sha256, noHash, noArch)
		},
	}
	cmd.Flags().StringVar(&release, "release", "", "要检查的 release（默认取 cluster.yaml 或物料库中唯一的 release）")
	cmd.Flags().StringVar(&sha256, "sha256", "", "归档的期望 sha256（只对 <archive> 模式有效）")
	cmd.Flags().BoolVar(&noHash, "no-hash", false, "跳过逐文件 sha256（只查存在、大小、权限、架构）")
	cmd.Flags().BoolVar(&noArch, "no-arch", false, "跳过可执行文件架构检查")
	return options.Add(cmd, opts, options.ConfigFlag|options.OutputFlag|options.DataDirFlag)
}

func runVerify(cmd *cobra.Command, opts *options.GlobalOptions, args []string, releaseFlag, shaFlag string, noHash, noArch bool) error {
	vopts := artifact.VerifyOptions{NoHash: noHash, NoArch: noArch}
	var (
		report *artifact.Report
		err    error
	)
	if len(args) == 1 {
		if strings.TrimSpace(shaFlag) == "" {
			shaFlag = readDigestSidecar(args[0] + ".sha256")
		}
		if strings.TrimSpace(shaFlag) != "" {
			if err := checkArchiveDigest(args[0], shaFlag); err != nil {
				return err
			}
		}
		report, err = artifact.VerifyArchive(cmd.Context(), args[0], vopts)
		if err != nil {
			return err
		}
	} else {
		release, err := resolveRelease(opts, releaseFlag)
		if err != nil {
			return err
		}
		dir := releaseDir(opts, release)
		report, err = artifact.VerifyDir(dir, vopts)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("物料目录 %s 不存在或缺少 %s：先运行 kubemono bundle load <archive>", dir, artifact.MetaName)
			}
			return err
		}
	}
	if err := printVerifyReport(cmd.OutOrStdout(), opts.Output, report); err != nil {
		return err
	}
	return report.Error()
}

func checkArchiveDigest(path, want string) error {
	norm, err := artifact.NormalizeDigest(want)
	if err != nil {
		return err
	}
	got, err := fsutil.SHA256File(path)
	if err != nil {
		return fmt.Errorf("artifact: 计算 %s 的摘要失败: %w", path, err)
	}
	if !strings.EqualFold(got, norm) {
		return fmt.Errorf("artifact: 归档 %s 的 sha256 是 %s，期望 %s", path, got, norm)
	}
	return nil
}

func printVerifyReport(out io.Writer, output string, report *artifact.Report) error {
	return utils.Print(out, output, report, func(w io.Writer) error {
		fields := map[string]string{
			"来源":      report.Source,
			"release": report.Release,
			"平台":      report.OS + "/" + report.Arch,
			"文件数":     fmt.Sprintf("%d", report.FilesChecked),
			"结果":      verifySummary(report),
		}
		_, _ = fmt.Fprint(w, term.KeyValuesAligned(fields))
		for _, p := range report.Missing {
			_, _ = fmt.Fprintf(w, "  - 缺失 %s\n", p)
		}
		for _, p := range report.Unknown {
			_, _ = fmt.Fprintf(w, "  - 不在 meta 里 %s\n", p)
		}
		for _, m := range report.HashMismatches {
			_, _ = fmt.Fprintf(w, "  - 哈希不匹配 %s：实际 %s，meta %s\n", m.Path, m.Got, m.Want)
		}
		for _, m := range report.SizeMismatches {
			_, _ = fmt.Fprintf(w, "  - 大小不匹配 %s：实际 %d，meta %d\n", m.Path, m.Got, m.Want)
		}
		for _, m := range report.ModeMismatches {
			_, _ = fmt.Fprintf(w, "  - %s\n", m.String())
		}
		for _, m := range report.ArchMismatches {
			_, _ = fmt.Fprintf(w, "  - 架构不匹配：%s\n", m)
		}
		return nil
	})
}

func verifySummary(report *artifact.Report) string {
	if report.OK() {
		return "通过"
	}
	parts := make([]string, 0, 6)
	for _, issue := range report.Issues() {
		parts = append(parts, fmt.Sprintf("%d %s", issue.Count, issue.Short))
	}
	return "未通过（" + strings.Join(parts, "，") + "）"
}
