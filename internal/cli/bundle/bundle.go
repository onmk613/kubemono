package bundle

import (
	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
)

func New(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Offline Assets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPackCmd(opts), newListCmd(opts), newLoadCmd(opts), newVerifyCmd(opts))
	return cmd
}
