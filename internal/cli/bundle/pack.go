package bundle

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kubemono/internal/artifact/release"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/pack"
)

const packLong = `按 BOM 获取物料并写出可直接 load 的归档（唯一造包入口）。

默认生成轻量包（不含 OCI 镜像）；加 --images 生成完整包。
归档文件名：kubemono-<release>-<os>-<arch>.tar.zst（轻量包带 -lite）。
默认目标 linux/amd64；组件物料缺失时自动从 BOM 的 source 下载（需要网络）。
-f cluster.yaml 控制版本覆盖，--bom 替换组件列表与获取 URL。

目录边界：--cache-dir 是下载缓存根目录，实际按 release 隔离；
--out-dir 只接收最终归档和 .sha256；输出目录不能与该 release 的缓存目录互相包含。
缓存按“组件/架构/source 哈希”分目录：改 BOM URL 或组件版本会自然失效；
--refresh 可强制忽略已有组件缓存，--offline 只允许使用缓存、缺失直接报错。

示例：
  kubemono bundle pack                                  # 轻量包（默认）
  kubemono bundle pack --images                         # 完整包（缺镜像时联网拉取）
  kubemono bundle pack --images --fetch-images=false    # 只用本地镜像缓存
  kubemono bundle pack --arch linux/arm64
  kubemono bundle pack --images -f cluster.yaml --out-dir /mnt/usb
  kubemono bundle pack -f cluster.yaml --bom ./bom.yaml --refresh
  kubemono bundle pack -f cluster.yaml --bom ./bom.yaml --offline

打包后校验并导入：
  kubemono bundle verify dist/kubemono-...tar.zst
  kubemono bundle load dist/kubemono-...tar.zst`

func newPackCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		arch       string
		outDir     string
		cacheDir   string
		images     bool
		fetchImage bool
		bomPath    string
		refresh    bool
		offline    bool
		dryRun     bool
		created    string
	)
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "按 BOM 获取物料并写出归档（可直接 load）",
		Long:  packLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// cluster.yaml 只用于版本覆盖与按组件镜像覆盖：没有也能打包。
			// 显式给了 -f 却读不出来时必须报错；默认路径不存在时只用内嵌 BOM。
			cfgPath, cfgExists := opts.ClusterConfigPath()
			if !cfgExists {
				cfgPath = ""
				opts.ReporterOrDiscard().Warnf("未找到 cluster.yaml，按内嵌 BOM 的默认版本打包")
			}
			b, err := pack.EffectiveBOM(bomPath, cfgPath)
			if err != nil {
				return err
			}
			// 与 bundle list 共用同一套 --arch 解析/校验（只允许单架构、linux）。
			archName, err := resolveArch(b, arch)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("fetch-images") && !images {
				return fmt.Errorf("--fetch-images 只配合 --images 使用")
			}
			if offline && cmd.Flags().Changed("fetch-images") && fetchImage {
				return fmt.Errorf("--offline 与 --fetch-images=true 冲突：离线模式不会拉取镜像")
			}
			createdAt, err := packCreated(created)
			if err != nil {
				return err
			}
			packOpts := pack.Options{
				BOM:           b,
				Created:       createdAt,
				OutDir:        outDir,
				Archs:         []string{archName},
				IncludeImages: images,
				FetchImages:   fetchImage,
				CacheRoot:     cacheDir,
				Refresh:       refresh,
				Offline:       offline,
				Reporter:      opts.ReporterOrDiscard().Infof,
			}
			if dryRun {
				plan, err := pack.PlanPack(packOpts)
				if err != nil {
					return err
				}
				return printPackPlan(cmd, opts, plan)
			}
			results, err := packFor(cmd.Context(), packOpts)
			if err != nil {
				return err
			}
			return printPackResult(cmd, opts, results)
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "", "目标架构（默认 linux/amd64；一次一个，多架构请分多次执行）")
	cmd.Flags().StringVar(&outDir, "out-dir", "dist", "归档输出目录（只写最终归档与 .sha256）")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "下载缓存根目录（按 release 隔离；默认用户缓存）")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "忽略已有组件缓存，重新下载（镜像仍按 OCI layout 判断）")
	cmd.Flags().BoolVar(&offline, "offline", false, "只用本地缓存；缺失组件/镜像直接报错，不联网")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只打印将使用的组件/镜像与输出路径，不联网、不写盘")
	cmd.Flags().BoolVar(&images, "images", false, "生成含 OCI 镜像的完整包（默认轻量包）")
	cmd.Flags().BoolVar(&fetchImage, "fetch-images", true, "配合 --images：缺失镜像时联网拉取（--fetch-images=false 只用本地缓存）")
	cmd.Flags().StringVar(&bomPath, "bom", "", "覆盖内置 BOM（自定义发行版/测试）")
	// 造包没有确认环节，也没有覆盖语义：不挂 --assume-yes/--overwrite 这类假开关。
	return options.Add(cmd, opts, options.ConfigFlag|options.OutputFlag|options.DataDirFlag)
}

// packFor 是打包的唯一入口，测试可替换。
var packFor = pack.Run

func printPackResult(cmd *cobra.Command, opts *options.GlobalOptions, results []release.Result) error {
	out := cmd.OutOrStdout()
	payload := make([]map[string]any, 0, len(results))
	for _, r := range results {
		payload = append(payload, map[string]any{
			"release": r.Meta.Release,
			"arch":    r.Meta.Arch,
			"archive": r.Archive,
			"sha256":  r.SHA256,
			"size":    r.Size,
		})
	}
	return utils.Print(out, opts.Output, payload, func(w io.Writer) error {
		for _, r := range results {
			_, _ = fmt.Fprintf(w, "归档：%s\n  架构：%s  大小：%.1f MiB\n  sha256：%s\n",
				r.Archive, r.Meta.Arch, float64(r.Size)/(1<<20), r.SHA256)
		}
		if len(results) > 0 {
			_, _ = fmt.Fprintln(w, "\n下一步：")
			for _, r := range results {
				_, _ = fmt.Fprintf(w, "  校验：kubemono bundle verify %s\n", r.Archive)
				_, _ = fmt.Fprintf(w, "  导入：kubemono bundle load %s\n", r.Archive)
			}
		}
		return nil
	})
}

// printPackPlan 输出 --dry-run 的计划：组件缓存状态、镜像状态与归档路径。
func printPackPlan(cmd *cobra.Command, opts *options.GlobalOptions, plan *pack.Plan) error {
	return utils.Print(cmd.OutOrStdout(), opts.Output, plan, func(w io.Writer) error {
		_, _ = fmt.Fprint(w, term.KeyValuesAligned(map[string]string{
			"release": plan.Release,
			"平台":      plan.OS + "/" + strings.Join(plan.Archs, ","),
			"输出目录":    plan.OutDir,
			"缓存根目录":   plan.CacheRoot,
			"含镜像":     term.YesNo(plan.IncludeImages),
			"离线":      term.YesNo(plan.Offline),
		}))
		rows := make([][]string, 0, len(plan.Components))
		for _, c := range plan.Components {
			rows = append(rows, []string{c.Arch, c.Name, c.Category, c.Version, c.Action, c.Source})
		}
		_, _ = fmt.Fprint(w, term.Table([]string{"ARCH", "COMPONENT", "CATEGORY", "VERSION", "LOCAL", "SOURCE"}, rows))
		if len(plan.Images) > 0 {
			imgRows := make([][]string, 0, len(plan.Images))
			for _, img := range plan.Images {
				imgRows = append(imgRows, []string{img.Arch, img.Reference, img.Action})
			}
			_, _ = fmt.Fprint(w, term.Table([]string{"ARCH", "IMAGE", "LOCAL"}, imgRows))
		}
		for _, archive := range plan.Archives {
			_, _ = fmt.Fprintf(w, "将写出：%s（及 .sha256）\n", archive)
		}
		return nil
	})
}

// packCreated 解析 --created / SOURCE_DATE_EPOCH，让同一份物料得到稳定的
// meta.json 与 tar mtime；未设置时保持 time.Now（普通用户不需要关心）。
func packCreated(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH"))
		if raw == "" {
			return time.Time{}, nil
		}
		sec, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH 不是 Unix 秒: %w", err)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("--created 必须是 RFC3339 时间: %w", err)
	}
	return t.UTC(), nil
}
