package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"
)

func newExampleCmd() *cobra.Command {
	var (
		name  string
		count int
	)

	cmd := &cobra.Command{
		Use:   "example [message]",
		Short: "An example command to demonstrate patterns",
		Long: `This is an example command that demonstrates common CLI patterns:
- Positional arguments
- Flag parsing
- Context cancellation
- Error handling`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			message := "Hello"
			if len(args) > 0 {
				message = args[0]
			}

			slog.Debug("running example command", "message", message, "count", count)

			for i := 0; i < count; i++ {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				default:
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s, %s!\n", message, name)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "World", "name to greet")
	cmd.Flags().IntVarP(&count, "count", "c", 1, "number of times to repeat")

	return cmd
}
