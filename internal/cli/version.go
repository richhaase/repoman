package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(build BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "mycli %s\n", build.Version)
			fmt.Fprintf(out, "  commit: %s\n", build.Commit)
			fmt.Fprintf(out, "  built:  %s\n", build.Date)
		},
	}
}
