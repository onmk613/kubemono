package bundle

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/pack"
)

// listRow 是 `kubemono bundle list` 的一行。
type listRow struct {
	Name     string `json:"name"`
	Category string `json:"category,omitempty"`
	Version  string `json:"version,omitempty"`
	Path     string `json:"path,omitempty"`
	Status   string `json:"status"`
	Size     int64  `json:"size,omitempty"`
}

// imageRow 是镜像视图的一行。
type imageRow struct {
	Component string `json:"component,omitempty"`
	Reference string `json:"reference"`
	Platform  string `json:"platform,omitempty"`
	Status    string `json:"status"`
}

// listView 是 `list` 的机器可读输出。
type listView struct {
	Release    string     `json:"release"`
	Kubernetes string     `json:"kubernetes,omitempty"`
	OS         string     `json:"os"`
	Arch       string     `json:"arch"`
	Dir        string     `json:"dir,omitempty"`
	Installed  bool       `json:"installed"`
	Components []listRow  `json:"components"`
	Images     []imageRow `json:"images,omitempty"`
}

// newListCmd 是 `kubemono bundle list`。
func newListCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		category string
		images   bool
		arch     string
		bomPath  string
	)
	cmd := &cobra.Command{
		Use:   "list [<archive>]",
		Short: "列出物料计划或归档内容（--images 切换到镜像视图）",
		Long: `无参数时按 BOM/cluster.yaml 列出组件与版本，并标记本地物料库是否已具备；
带归档参数时只读 meta.json，不解包、不写文件。

计划模式和 pack 使用同一套来源：-f cluster.yaml 控制版本覆盖，
--bom 替换组件列表与获取 URL。
默认目标 linux/amd64（与 pack 一致）；一次只列一个架构。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if category != "" && !bom.CategoryValid(category) {
				return fmt.Errorf("未知类别 %q：可用 %s", category, strings.Join(bom.CategoryNames(), ", "))
			}
			if len(args) == 1 {
				for _, name := range []string{"arch", "bom"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("归档模式不支持 --%s（只读 meta.json；计划模式才需要它）", name)
					}
				}
				if cmd.Flags().Changed("config") {
					return fmt.Errorf("归档模式不支持 -f/--config（只读 meta.json）")
				}
				return runListArchive(cmd, opts, args[0], category, images)
			}
			return runListPlan(cmd, opts, category, images, arch, bomPath)
		},
	}
	cmd.Flags().StringVar(&category, "category", "", "只列出某个类别（k8s/etcd/runtime/cni/network/addon/lb/image）")
	cmd.Flags().BoolVar(&images, "images", false, "列出镜像而不是组件")
	cmd.Flags().StringVar(&arch, "arch", "", "目标架构（默认 linux/amd64，与 pack 一致；一次一个）")
	cmd.Flags().StringVar(&bomPath, "bom", "", "覆盖内置 BOM（自定义发行版/测试）")
	return options.Add(cmd, opts, options.ConfigFlag|options.OutputFlag|options.DataDirFlag)
}

func runListArchive(cmd *cobra.Command, opts *options.GlobalOptions, archivePath, category string, images bool) error {
	meta, err := artifact.ReadMetaFromArchive(archivePath)
	if err != nil {
		return err
	}
	if category != "" && images {
		return fmt.Errorf("归档模式下的 --images 不支持 --category（归档里没有 BOM 分类）")
	}
	view := &listView{
		Release:    meta.Release,
		Kubernetes: meta.Kubernetes,
		OS:         meta.OS,
		Arch:       meta.Arch,
		Dir:        archivePath,
		Installed:  true,
	}
	if images {
		for _, img := range meta.Images {
			view.Images = append(view.Images, imageRow{
				Component: img.Component, Reference: img.Reference, Platform: img.Platform, Status: "ok",
			})
		}
	} else {
		for _, c := range meta.Components {
			if category != "" && c.Category != category {
				continue
			}
			row := listRow{Name: c.Name, Category: c.Category, Version: c.Version, Status: "ok"}
			for _, f := range meta.Files {
				if f.Component != c.Name {
					continue
				}
				if row.Path == "" {
					row.Path = f.Path
				}
				row.Size += f.Size
			}
			view.Components = append(view.Components, row)
		}
	}
	return printListView(cmd.OutOrStdout(), opts.Output, view, images)
}

func runListPlan(cmd *cobra.Command, opts *options.GlobalOptions, category string, images bool, archFlag, bomPath string) error {
	cfgPath, cfgExists := opts.ClusterConfigPath()
	if !cfgExists {
		cfgPath = ""
	}
	b, err := pack.EffectiveBOM(bomPath, cfgPath)
	if err != nil {
		return err
	}
	if !cfgExists {
		opts.ReporterOrDiscard().Warnf("未找到 cluster.yaml，按 BOM 的默认版本列出")
	}
	arch, err := resolveArch(b, archFlag)
	if err != nil {
		return err
	}
	rel := b.Release
	dir := releaseDir(opts, rel)
	local, err := artifact.LoadMeta(dir)
	if err != nil {
		local = nil
	}
	view := &listView{
		Release:    rel,
		Kubernetes: b.Kubernetes,
		OS:         "linux",
		Arch:       arch,
		Dir:        dir,
		Installed:  local != nil,
	}
	if images {
		have := map[string]bool{}
		if refs, refsErr := artifact.LayoutReferences(filepath.Join(dir, artifact.DirImages)); refsErr == nil {
			for _, ref := range refs {
				have[ref] = true
			}
		}
		for _, img := range b.Images() {
			if category != "" && b.Components[img.Component].Category != category {
				continue
			}
			status := "missing"
			if have[img.Reference()] {
				status = "ok"
			}
			view.Images = append(view.Images, imageRow{
				Component: img.Component, Reference: img.Reference(), Status: status,
			})
		}
	} else {
		for _, name := range b.Names() {
			c := b.Components[name]
			if category != "" && c.Category != category {
				continue
			}
			if len(c.Binaries) == 0 {
				view.Components = append(view.Components, listRow{
					Name: name, Category: c.Category, Version: c.Version, Status: "-",
				})
				continue
			}
			for _, bin := range c.Binaries {
				bundleRel := artifact.BundlePath(c.Category, bin)
				row := listRow{Name: name, Category: c.Category, Version: c.Version, Path: bundleRel}
				if entry, ok := local.Find(bundleRel); ok {
					row.Status = "ok"
					row.Size = entry.Size
				} else if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(bundleRel))); err == nil && info.Mode().IsRegular() {
					row.Status = "unlisted"
					row.Size = info.Size()
				} else {
					row.Status = "missing"
				}
				view.Components = append(view.Components, row)
			}
		}
	}
	return printListView(cmd.OutOrStdout(), opts.Output, view, images)
}

func printListView(out io.Writer, output string, view *listView, images bool) error {
	return utils.Print(out, output, view, func(w io.Writer) error {
		fields := map[string]string{
			"release":    view.Release,
			"kubernetes": view.Kubernetes,
			"平台":         view.OS + "/" + view.Arch,
			"位置":         view.Dir,
			"可用":         term.YesNo(view.Installed),
		}
		_, _ = fmt.Fprint(w, term.KeyValuesAligned(fields))
		if images {
			rows := make([][]string, 0, len(view.Images))
			for _, r := range view.Images {
				rows = append(rows, []string{r.Component, r.Reference, r.Platform, statusLabel(r.Status)})
			}
			_, _ = fmt.Fprint(w, term.Table([]string{"COMPONENT", "IMAGE", "PLATFORM", "LOCAL"}, rows))
			return nil
		}
		rows := make([][]string, 0, len(view.Components))
		for _, r := range view.Components {
			path := r.Path
			if path == "" {
				path = "-"
			}
			rows = append(rows, []string{r.Name, r.Category, r.Version, path, statusLabel(r.Status)})
		}
		_, _ = fmt.Fprint(w, term.Table([]string{"COMPONENT", "CATEGORY", "VERSION", "PATH", "LOCAL"}, rows))
		return nil
	})
}

func statusLabel(s string) string {
	switch s {
	case "ok":
		return "✓"
	case "missing":
		return "缺失"
	case "unlisted":
		return "不在 meta"
	default:
		return "-"
	}
}
