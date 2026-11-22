package bundle

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/artifact"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
)

// readDigestSidecar 读取同目录的 .sha256（sha256sum 格式：<hex>  <file>）；
// 不存在或格式不对都返回空，由调用方决定是否 warning。
func readDigestSidecar(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if sum, err := artifact.NormalizeDigest(fields[0]); err == nil {
			return sum
		}
	}
	return ""
}

// printLoadResult 渲染 load 的结果；dryRun 只追加一行模式说明。
// 人类可读字段只维护一份，机器可读 payload 在 dry-run 与正式导入间二选一。
func printLoadResult(cmd *cobra.Command, opts *options.GlobalOptions, meta *artifact.Meta, dest, archive string, dryRun bool) error {
	var payload any = meta
	if dryRun {
		payload = map[string]any{
			"dryRun":     true,
			"release":    meta.Release,
			"kubernetes": meta.Kubernetes,
			"os":         meta.OS,
			"arch":       meta.Arch,
			"dest":       dest,
			"archive":    archive,
			"files":      len(meta.Files),
			"images":     len(meta.Images),
			"size":       meta.TotalSize(),
		}
	}
	return utils.Print(cmd.OutOrStdout(), opts.Output, payload, func(w io.Writer) error {
		fields := map[string]string{
			"release":    meta.Release,
			"kubernetes": meta.Kubernetes,
			"平台":         meta.OS + "/" + meta.Arch,
			"物料目录":       dest,
			"归档":         archive,
			"文件数":        fmt.Sprintf("%d", len(meta.Files)),
			"镜像数":        fmt.Sprintf("%d", len(meta.Images)),
			"物料大小":       fmt.Sprintf("%.1f MiB", float64(meta.TotalSize())/(1<<20)),
		}
		if dryRun {
			fields["模式"] = "dry-run（未写入物料库）"
		}
		_, _ = fmt.Fprint(w, term.KeyValuesAligned(fields))
		return nil
	})
}
