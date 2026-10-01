package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// BuildInfo carries the values injected at build time via ldflags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// NewRootCmd builds a fresh command tree. Constructing the tree rather than
// sharing a package-level command keeps flag values out of globals, so tests
// can execute the CLI repeatedly in one process without state leaking between
// runs.
func NewRootCmd(build BuildInfo) *cobra.Command {
	var verbose bool

	root := &cobra.Command{
		Use:   "mycli",
		Short: "A brief description of your CLI",
		Long: `A longer description that spans multiple lines and likely contains
examples and usage of using your application.`,

		Version:       build.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
				Level: level,
			})))
		},
	}

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose (debug) logging")

	root.AddCommand(
		newExampleCmd(),
		newVersionCmd(build),
	)

	return root
}

// Execute runs the root command and returns an exit code.
func Execute(ctx context.Context, version, commit, date string) int {
	root := NewRootCmd(BuildInfo{Version: version, Commit: commit, Date: date})

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}
