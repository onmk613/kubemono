package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/bundle"
	"kubemono/internal/cli/cert"
	"kubemono/internal/cli/configcmd"
	"kubemono/internal/cli/initcmd"
	"kubemono/internal/cli/join"
	"kubemono/internal/cli/node"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/pki"
	"kubemono/internal/cli/reset"
	"kubemono/internal/cli/status"
	"kubemono/internal/cli/token"
	"kubemono/internal/cli/version"
	"kubemono/internal/cli/whoami"
	_ "kubemono/internal/phase/builtin"
)

func NewRootCmd() *cobra.Command {
	opts := options.Default()
	root := &cobra.Command{
		Use:               "kubemono",
		Short:             "kubemono - Install Kubernetes Deployment Tool",
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Verbose {
				opts.Reporter.SetVerbose(true)
			}
			switch opts.Output {
			case "", "table", "json", "yaml":
			default:
				return fmt.Errorf("unsupported --output %q: expected table, json or yaml", opts.Output)
			}

			opts.Reporter.SetProgressToStderr(opts.Output == "json" || opts.Output == "yaml")
			if opts.DataDir != "" {
				abs, err := filepath.Abs(opts.DataDir)
				if err != nil {
					return err
				}
				if abs == string(filepath.Separator) {
					return fmt.Errorf("--data-dir 不能是 %s", abs)
				}
				opts.DataDir = abs
			}
			return nil
		},
	}

	options.AddPersistent(root, opts, options.VerboseFlag)

	root.AddCommand(
		version.New(opts),
		initcmd.New(opts),
		token.New(opts),
		join.New(opts),
		reset.New(opts),
		bundle.New(opts),
		node.New(opts),
		configcmd.New(opts),
		cert.New(opts),
		pki.New(opts),
		status.New(opts),
		whoami.New(opts),
	)

	options.SetupCommandGroups(root)
	return root
}
