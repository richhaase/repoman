package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richhaase/repoman/internal/config"
	"github.com/richhaase/repoman/internal/syncer"
)

type configOptions struct {
	path string
	json bool
}

func newConfigCmd() *cobra.Command {
	var o configOptions
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Add, list, or remove configured targets",
		Long:  "Manage target settings in the JSON configuration. Removing an entry never deletes its directory or repositories.",
	}
	cmd.PersistentFlags().StringVar(&o.path, "config", config.DefaultPath(), "JSON target config (or REPOMAN_CONFIG)")
	cmd.PersistentFlags().BoolVar(&o.json, "json", false, "emit the plain JSON target configuration on stdout")
	cmd.AddCommand(newConfigAddCmd(&o), newConfigListCmd(&o), newConfigRemoveCmd(&o))
	return cmd
}

func newConfigAddCmd(o *configOptions) *cobra.Command {
	var owner, excludesFile, cleanupLevel string
	var days int
	var includes, excludes []string
	cmd := &cobra.Command{
		Use:   "add DIR",
		Short: "Register or replace a target, appending it in configuration order",
		Long:  "Register or replace a target's sync settings. Omitted sync flags use their defaults; an existing cleanup level is retained unless --cleanup-level is supplied. New registrations use push-only activity (events=false).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if days < 1 {
				return configError(fmt.Errorf("--days must be positive"))
			}
			if cmd.Flags().Changed("cleanup-level") && strings.TrimSpace(cleanupLevel) == "" {
				return configError(fmt.Errorf("--cleanup-level cannot be empty"))
			}
			dir, err := config.Normalize(args[0])
			if err != nil {
				return configError(err)
			}
			// Reject an invalid existing document before authentication or writes.
			if _, err = config.List(o.path); err != nil {
				return configError(err)
			}
			patterns := append([]string{}, excludes...)
			if cmd.Flags().Changed("excludes-file") {
				fromFile, readErr := syncer.ReadExcludesFile(excludesFile)
				if readErr != nil {
					return configError(readErr)
				}
				patterns = append(patterns, fromFile...)
			}
			if owner == "" {
				owner, err = syncer.AuthenticatedOwner(cmd.Context())
				if err != nil {
					return err
				}
			}
			if err = cmd.Context().Err(); err != nil {
				return err
			}
			target := syncer.Target{Dir: dir, Owner: owner, Days: days, Includes: includes, Excludes: patterns, CleanupLevel: cleanupLevel}
			updated, err := config.Register(o.path, target)
			if err != nil {
				return configError(err)
			}
			if o.json {
				return configJSON(cmd, updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Registered target %q in %s\n", dir, o.path)
			printConfigTarget(cmd, updated.Targets[len(updated.Targets)-1])
			return nil
		},
	}
	cmd.Flags().StringVarP(&owner, "owner", "o", "", "GitHub user or organization (default: authenticated gh user)")
	cmd.Flags().IntVarP(&days, "days", "d", 45, "positive push-activity window in days")
	cmd.Flags().StringArrayVar(&includes, "include", nil, "repository-name glob to include (repeatable)")
	cmd.Flags().StringArrayVarP(&excludes, "exclude", "e", nil, "repository-name glob to exclude (repeatable; wins over includes)")
	cmd.Flags().StringVar(&excludesFile, "excludes-file", "", "file of exclude patterns, one per line; blank and # comment lines ignored")
	cmd.Flags().StringVar(&cleanupLevel, "cleanup-level", "", "cleanup level: conservative, balanced, or aggressive (retains existing value if omitted)")
	cmd.Flags().Bool("no-events", false, "use push-only activity (already the default for registrations)")
	return cmd
}

func newConfigListCmd(o *configOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show configured targets in configuration order",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			c, err := config.List(o.path)
			if err != nil {
				return configError(err)
			}
			if o.json {
				return configJSON(cmd, c)
			}
			if len(c.Targets) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No configured targets in %s\n", o.path)
			}
			for _, target := range c.Targets {
				printConfigTarget(cmd, target)
			}
			return nil
		},
	}
}

func newConfigRemoveCmd(o *configOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "remove DIR",
		Short: "Remove a target's configuration entry without deleting any files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			dir, err := config.Normalize(args[0])
			if err != nil {
				return configError(err)
			}
			updated, err := config.Remove(o.path, dir)
			if err != nil {
				return configError(err)
			}
			if o.json {
				return configJSON(cmd, updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed target %q from %s\n", dir, o.path)
			return nil
		},
	}
}

func configError(err error) error {
	return &ExitError{Code: 2, Err: err}
}

func configJSON(cmd *cobra.Command, c config.Config) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(c)
}

func printConfigTarget(cmd *cobra.Command, target syncer.Target) {
	owner := target.Owner
	if owner == "" {
		owner = "(authenticated gh user)"
	}
	days := target.Days
	if days == 0 {
		days = 45
	}
	events := target.Events != nil && *target.Events
	includes, excludes := "(all)", "(none)"
	if len(target.Includes) > 0 {
		includes = strings.Join(target.Includes, ", ")
	}
	if len(target.Excludes) > 0 {
		excludes = strings.Join(target.Excludes, ", ")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n  owner: %s   days: %d   events: %t   includes: %s   excludes: %s", target.Dir, owner, days, events, includes, excludes)
	if target.CleanupLevel != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "   cleanup_level: %s", target.CleanupLevel)
	}
	fmt.Fprintln(cmd.OutOrStdout())
}
