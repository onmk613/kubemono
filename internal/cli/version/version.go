// Package version 实现 `kubemono version` 命令。
package version

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"kubemono/internal/bom"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/version"
)

func New(opts *options.GlobalOptions) *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show kubemono version and the pinned component versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if short && cmd.Flags().Changed("output") {
				return fmt.Errorf("--short 与 --output 不能同时使用")
			}
			b := bom.MustLoad()
			info := version.Info{
				Release:    b.Release,
				Kubernetes: b.Kubernetes,
				GoVersion:  runtime.Version(),
				Commit:     version.Commit,
				BuildDate:  version.BuildDate,
				Arch:       runtime.GOARCH,
				OS:         runtime.GOOS,
			}
			out := cmd.OutOrStdout()
			if short {
				_, _ = fmt.Fprintln(out, b.Release)
				return nil
			}
			// 组件顺序只在文本输出里有意义；JSON 对象的键无序，encoding/json 自行排序。
			names := b.Names()
			components := make(map[string]string, len(names))
			pairs := make([]term.KeyValue, 0, len(names))
			for _, name := range names {
				v := b.ComponentVersion(name)
				components[name] = v
				pairs = append(pairs, term.KeyValue{Key: name, Value: v})
			}
			payload := struct {
				version.Info
				Components map[string]string `json:"components"`
			}{info, components}
			return utils.Print(out, opts.Output, payload, func(w io.Writer) error {
				_, _ = fmt.Fprintf(w, "kubemono %s\n", info.Release)
				_, _ = fmt.Fprintf(w, "Kubernetes  %s\n", info.Kubernetes)
				_, _ = fmt.Fprintf(w, "Go          %s (%s/%s)\n", info.GoVersion, info.OS, info.Arch)
				if info.Commit != "" {
					_, _ = fmt.Fprintf(w, "Commit      %s\n", info.Commit)
				}
				if info.BuildDate != "" {
					_, _ = fmt.Fprintf(w, "Built       %s\n", info.BuildDate)
				}
				_, _ = fmt.Fprintln(w)
				_, _ = fmt.Fprintln(w, "Pinned component versions:")
				_, _ = fmt.Fprint(w, term.KeyValuesOrdered(pairs))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "Only output the version number")
	return options.Add(cmd, opts, options.OutputFlag)
}
