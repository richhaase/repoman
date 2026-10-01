package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		Use:   "repoman",
		Short: "Keep your repositories up to date and tidy",
		Long: `Keep your repositories up to date and tidy.

  status   See local clones, branches, and worktrees
  sync     Clone and update active GitHub repositories
  clean    Remove eligible linked worktrees
  config   Choose the folders and GitHub owners to manage

Start with sync --dry-run or clean --dry-run to preview changes.
Clean applies by default; its aggressive policy can discard local files.`,
		Example: "  repoman config add ~/src\n  repoman status\n  repoman sync --dry-run\n  repoman clean --dry-run",

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

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "show routine skips and extra repository details (plus debug logs)")

	root.AddCommand(
		newConfigCmd(),
		newStatusCmd(),
		newSyncCmd(),
		newCleanCmd(),
		newVersionCmd(build),
	)

	return root
}

// Execute runs the root command and returns an exit code.
func Execute(ctx context.Context, version, commit, date string) int {
	root := NewRootCmd(BuildInfo{Version: version, Commit: commit, Date: date})

	return reportExecution(root.ExecuteContext(ctx), os.Stderr)
}

func reportExecution(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var reported *ExitError
	if !errors.As(err, &reported) || !reported.Reported {
		fmt.Fprintf(stderr, "Error: %s\n", humanText(err.Error()))
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	if errors.As(err, &reported) {
		return reported.Code
	}
	return 1
}
