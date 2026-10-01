package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richhaase/repoman/internal/cleanup"
	"github.com/richhaase/repoman/internal/config"
	"github.com/richhaase/repoman/internal/repository"
	"github.com/richhaase/repoman/internal/syncer"
)

// ExitError preserves structured output while communicating a partial failure.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

type envelope struct {
	SchemaVersion   int             `json:"schema_version"`
	Command         string          `json:"command"`
	DryRun          bool            `json:"dry_run"`
	Items           any             `json:"items"`
	Errors          []string        `json:"errors"`
	Warnings        []string        `json:"warnings,omitempty"`
	CleanupPolicies []cleanupPolicy `json:"cleanup_policies,omitempty"`
}

type cleanupPolicy struct {
	Root                string        `json:"root"`
	Level               cleanup.Level `json:"level"`
	DiscardLocalChanges bool          `json:"discard_local_changes"`
}

type options struct {
	root, configPath string
	json             bool
}

func (o options) targets(args []string) ([]syncer.Target, error) {
	if o.root != "" {
		if len(args) > 0 {
			return nil, fmt.Errorf("use DIR or --root, not both")
		}
		p, e := config.Normalize(o.root)
		return []syncer.Target{{Dir: p, Days: 45}}, e
	}
	c, e := config.Load(o.configPath)
	if e != nil {
		return nil, e
	}
	if len(args) == 0 {
		return c.Targets, nil
	}
	p, e := config.Normalize(args[0])
	if e != nil {
		return nil, e
	}
	for _, t := range c.Targets {
		if t.Dir == p {
			return []syncer.Target{t}, nil
		}
	}
	return nil, fmt.Errorf("target %q is not configured; use --root for ad hoc access", p)
}
func bind(cmd *cobra.Command, o *options) {
	cmd.Flags().StringVar(&o.root, "root", "", "scan an ad hoc parent directory (no config lookup)")
	cmd.Flags().StringVarP(&o.root, "target", "t", "", "alias for --root")
	cmd.MarkFlagsMutuallyExclusive("root", "target")
	cmd.Flags().StringVar(&o.configPath, "config", config.DefaultPath(), "JSON target config (or REPOMAN_CONFIG)")
	cmd.Flags().BoolVar(&o.json, "json", false, "emit schema-versioned JSON on stdout")
}
func output(cmd *cobra.Command, o options, env envelope, lines []string) error {
	if o.json {
		e := json.NewEncoder(cmd.OutOrStdout())
		e.SetEscapeHTML(false)
		e.SetIndent("", "  ")
		if err := e.Encode(env); err != nil {
			return err
		}
	} else {
		for _, s := range lines {
			fmt.Fprintln(cmd.OutOrStdout(), s)
		}
		if len(lines) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No repositories found")
		}
		for _, s := range env.Errors {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", s)
		}
	}
	for _, warning := range env.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	if len(env.Errors) > 0 {
		return &ExitError{Code: 3, Err: errors.New("completed with errors; see reported details")}
	}
	return nil
}
func selection(cmd *cobra.Command, o options, args []string) ([]syncer.Target, error) {
	ts, e := o.targets(args)
	if e != nil {
		return nil, &ExitError{Code: 2, Err: e}
	}
	if err := cmd.Context().Err(); err != nil {
		return nil, err
	}
	return ts, nil
}
func newStatusCmd() *cobra.Command {
	var o options
	cmd := &cobra.Command{Use: "status [DIR]", Short: "Inventory primary clones and linked worktrees without network access", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ts, e := selection(cmd, o, args)
		if e != nil {
			return e
		}
		states := []repository.State{}
		errs := []string{}
		lines := []string{}
		seen := map[string]bool{}
		for _, t := range ts {
			slog.DebugContext(cmd.Context(), "inspecting target", "root", t.Dir)
			ss, e := repository.Discover(cmd.Context(), t.Dir)
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.Dir, e))
			}
			for _, s := range ss {
				if seen[s.Path] {
					continue
				}
				seen[s.Path] = true
				states = append(states, s)
				kind := "worktree"
				if s.Primary {
					kind = "primary"
				}
				flags := []string{}
				if s.Bare {
					flags = append(flags, "bare")
				}
				if s.Missing {
					flags = append(flags, "missing")
				}
				if s.Prunable {
					flags = append(flags, "prunable registration")
				}
				if s.Dirty {
					flags = append(flags, "dirty")
				}
				if s.Untracked {
					flags = append(flags, "untracked")
				}
				if s.Ignored {
					flags = append(flags, "ignored")
				}
				if s.Locked {
					flags = append(flags, "locked")
				}
				if s.Unique {
					flags = append(flags, "local-only commits")
				}
				if s.Stash {
					flags = append(flags, "stash")
				}
				if s.CwdInUse {
					flags = append(flags, "cwd in use")
				} else if s.InUse {
					flags = append(flags, "open file/process reference")
				}
				if !s.CwdInUseKnown {
					flags = append(flags, "cwd use unknown")
				}
				if !s.InUseKnown {
					flags = append(flags, "process use unknown")
				}
				if len(s.Problems) > 0 {
					flags = append(flags, "unknown")
					errs = append(errs, fmt.Sprintf("%s: %s", s.Path, strings.Join(s.Problems, "; ")))
				}
				if len(flags) == 0 {
					flags = append(flags, "clean")
				}
				branch := s.Branch
				if branch == "" {
					branch = "detached"
				}
				distance := "upstream unknown"
				if s.Ahead >= 0 && s.Behind >= 0 {
					distance = fmt.Sprintf("+%d/-%d", s.Ahead, s.Behind)
				}
				head := s.Head
				if len(head) > 12 {
					head = head[:12]
				}
				if head == "" {
					head = "unknown HEAD"
				}
				lines = append(lines, fmt.Sprintf("%-8s %q\n         %s @ %s | %s | %s", kind, s.Path, branch, head, distance, strings.Join(flags, ", ")))
				if len(s.SafetyProblems) > 0 {
					lines = append(lines, fmt.Sprintf("         process observation: %s", s.SafetyProblems[0]))
				}
			}
		}
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "status", DryRun: true, Items: states, Errors: errs}, lines)
	}}
	bind(cmd, &o)
	return cmd
}
func newSyncCmd() *cobra.Command {
	var o options
	var owner, excludesFile string
	var days int
	var noEvents, dryRun, force, cleanupInactive bool
	var includes, excludes []string
	cmd := &cobra.Command{Use: "sync [DIR]", Short: "Clone and update active repositories; optionally force checkout or clean inactive clones", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ts, e := selection(cmd, o, args)
		if e != nil {
			return e
		}
		fileExcludes := []string{}
		if excludesFile != "" {
			var err error
			fileExcludes, err = syncer.ReadExcludesFile(excludesFile)
			if err != nil {
				return &ExitError{Code: 2, Err: err}
			}
		}
		if !noEvents {
			for _, target := range ts {
				if target.Events != nil && *target.Events {
					fmt.Fprintln(cmd.ErrOrStderr(), "notice: legacy events=true uses push activity; event fallback is no longer used (pass --no-events to suppress this notice)")
					break
				}
			}
		}
		results := []syncer.Result{}
		errs := []string{}
		lines := []string{}
		for _, t := range ts {
			if cmd.Flags().Changed("owner") {
				t.Owner = owner
			}
			if cmd.Flags().Changed("days") {
				t.Days = days
			}
			if noEvents {
				v := false
				t.Events = &v
			}
			if cmd.Flags().Changed("include") {
				t.Includes = includes
			}
			t.Excludes = append(append(append([]string{}, t.Excludes...), excludes...), fileExcludes...)
			slog.DebugContext(cmd.Context(), "syncing target", "root", t.Dir, "owner", t.Owner, "dry_run", dryRun)
			rs, e := syncer.RunWithOptions(cmd.Context(), t, syncer.Options{DryRun: dryRun, Force: force, Cleanup: cleanupInactive})
			results = append(results, rs...)
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.Dir, e))
			}
			for _, r := range rs {
				lines = append(lines, fmt.Sprintf("%-14s %s: %s", r.Action, r.Name, r.Reason))
			}
		}
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "sync", DryRun: dryRun, Items: results, Errors: errs}, lines)
	}}
	bind(cmd, &o)
	cmd.Flags().StringVarP(&owner, "owner", "o", "", "GitHub user or organization (defaults to authenticated gh user)")
	cmd.Flags().IntVarP(&days, "days", "d", 45, "positive push-activity window in days")
	cmd.Flags().BoolVar(&noEvents, "no-events", false, "explicitly use push-only activity for legacy events=true configs")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview clone, fetch, checkout, and inactive cleanup without changes")
	cmd.Flags().StringArrayVar(&includes, "include", nil, "repository-name glob to include (repeatable)")
	cmd.Flags().StringArrayVarP(&excludes, "exclude", "e", nil, "repository-name glob to exclude (repeatable; wins over includes)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "discard local changes and force the default branch to origin (destructive)")
	cmd.Flags().BoolVarP(&cleanupInactive, "cleanup", "c", false, "remove inactive primary clones without uncommitted or untracked changes (destructive)")
	cmd.Flags().StringVar(&excludesFile, "excludes-file", "", "read exclude patterns from FILE (blank lines and # comments ignored)")
	return cmd
}
func newCleanCmd() *cobra.Command {
	return newCleanCmdWithRunner(cleanup.RunBatch)
}

func newCleanCmdWithRunner(run func(context.Context, []cleanup.Target, bool) ([]cleanup.Result, error)) *cobra.Command {
	var o options
	var apply, dryRun, discard bool
	var level string
	var days int
	cmd := &cobra.Command{Use: "clean [DIR]", Short: "Remove eligible linked worktrees; --dry-run previews and --level opts into stricter checks", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("days") && days < 1 {
			return &ExitError{Code: 2, Err: fmt.Errorf("--days must be positive (compatibility option; no retention effect)")}
		}
		if cmd.Flags().Changed("apply") && apply && dryRun {
			return &ExitError{Code: 2, Err: fmt.Errorf("--apply and --dry-run cannot both be true")}
		}
		effectiveApply := !dryRun
		if cmd.Flags().Changed("apply") && !apply {
			effectiveApply = false
		}
		ts, e := selection(cmd, o, args)
		if e != nil {
			return e
		}
		policies := make([]cleanupPolicy, 0, len(ts))
		targets := make([]cleanup.Target, 0, len(ts))
		lines := []string{}
		for _, t := range ts {
			raw := t.CleanupLevel
			if cmd.Flags().Changed("level") {
				raw = level
				if strings.TrimSpace(raw) == "" {
					return &ExitError{Code: 2, Err: fmt.Errorf("--level cannot be empty")}
				}
			}
			effective, err := cleanup.ParseLevel(raw)
			if err != nil {
				return &ExitError{Code: 2, Err: err}
			}
			if discard && effective != cleanup.Aggressive {
				return &ExitError{Code: 2, Err: fmt.Errorf("--discard-local-changes is a compatibility alias for aggressive cleanup")}
			}
			policies = append(policies, cleanupPolicy{Root: t.Dir, Level: effective, DiscardLocalChanges: discard})
			targets = append(targets, cleanup.Target{Root: filepath.Clean(t.Dir), Includes: t.Includes, Excludes: t.Excludes, Options: cleanup.Options{Level: effective, DiscardLocalChanges: discard}})
			phase := "apply"
			if !effectiveApply {
				phase = "preview"
			}
			lines = append(lines, fmt.Sprintf("cleanup %s | level=%s | root=%q", phase, effective, t.Dir))
			if effective == cleanup.Aggressive {
				lines = append(lines, "Aggressive cleanup force-removes checkout files, including local changes; branch refs are retained")
			}
		}
		results, err := run(cmd.Context(), targets, effectiveApply)
		errs := []string{}
		if err != nil {
			errs = append(errs, err.Error())
		}
		if len(results) == 0 && err == nil {
			lines = append(lines, "No repositories found")
		}
		warnings := []string{}
		seenWarnings := map[string]bool{}
		for _, r := range results {
			for _, warning := range r.Warnings {
				if !seenWarnings[warning] {
					warnings = append(warnings, warning)
					seenWarnings[warning] = true
				}
			}
			destructive := ""
			if r.Destructive {
				destructive = " [FORCE REMOVAL]"
			}
			lines = append(lines, fmt.Sprintf("%-14s %q%s: %s", r.Action, r.Path, destructive, r.Reason))
		}
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "clean", DryRun: !effectiveApply, Items: results, Errors: errs, CleanupPolicies: policies, Warnings: warnings}, lines)
	}}
	bind(cmd, &o)
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview eligible removals and metadata pruning without changing anything")
	cmd.Flags().BoolVar(&apply, "apply", true, "compatibility alias: true applies; false previews")
	cmd.Flags().StringVar(&level, "level", "", "cleanup level: aggressive (default), balanced, or conservative; overrides config")
	cmd.Flags().BoolVar(&discard, "discard-local-changes", false, "deprecated compatibility alias for aggressive cleanup; no acknowledgement is required")
	cmd.Flags().IntVarP(&days, "days", "d", 45, "positive compatibility option; does not affect cleanup retention")
	return cmd
}
