package cli

import (
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
				if s.InUse {
					flags = append(flags, "in use")
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
	var owner string
	var days int
	var noEvents, dryRun bool
	var includes, excludes []string
	cmd := &cobra.Command{Use: "sync [DIR]", Short: "Clone recently pushed repositories and safely fast-forward default branches", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ts, e := selection(cmd, o, args)
		if e != nil {
			return e
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
			t.Excludes = append(t.Excludes, excludes...)
			slog.DebugContext(cmd.Context(), "syncing target", "root", t.Dir, "owner", t.Owner, "dry_run", dryRun)
			rs, e := syncer.Run(cmd.Context(), t, dryRun)
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
	cmd.Flags().StringVar(&owner, "owner", "", "GitHub user or organization (required for ad hoc sync)")
	cmd.Flags().IntVar(&days, "days", 45, "positive push-activity window in days")
	cmd.Flags().BoolVar(&noEvents, "no-events", false, "explicitly use push-only activity for legacy events=true configs")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview; do not clone, fetch, or update")
	cmd.Flags().StringArrayVar(&includes, "include", nil, "repository-name glob to include (repeatable)")
	cmd.Flags().StringArrayVarP(&excludes, "exclude", "e", nil, "repository-name glob to exclude (repeatable; wins over includes)")
	return cmd
}
func newCleanCmd() *cobra.Command {
	var o options
	var apply, discard bool
	var level string
	cmd := &cobra.Command{Use: "clean [DIR]", Short: "Preview cleanup under a configurable policy; apply explicitly", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ts, e := selection(cmd, o, args)
		if e != nil {
			return e
		}
		if apply && len(ts) > 1 {
			return &ExitError{Code: 2, Err: fmt.Errorf("--apply requires one target; select DIR or --root")}
		}
		policies := make([]cleanupPolicy, 0, len(ts))
		// Validate every selected policy before inspecting or changing any target.
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
				return &ExitError{Code: 2, Err: fmt.Errorf("--discard-local-changes requires --level aggressive (or configured aggressive level)")}
			}
			if apply && effective == cleanup.Aggressive && !discard {
				return &ExitError{Code: 2, Err: fmt.Errorf("aggressive apply can permanently discard tracked changes, untracked files, and ignored files; preview first, then explicitly pass --discard-local-changes")}
			}
			policies = append(policies, cleanupPolicy{Root: t.Dir, Level: effective, DiscardLocalChanges: discard})
		}
		results := []cleanup.Result{}
		errs := []string{}
		lines := []string{}
		for i, t := range ts {
			policy := policies[i]
			phase := "preview"
			if apply {
				phase = "apply"
			}
			lines = append(lines, fmt.Sprintf("cleanup %s | level=%s | root=%q", phase, policy.Level, t.Dir))
			if policy.Level == cleanup.Aggressive {
				lines = append(lines, "WARNING: aggressive cleanup can permanently discard tracked changes, untracked files, and ignored files")
				if !apply {
					lines = append(lines, "Preview only. Applying aggressive cleanup requires --apply --discard-local-changes")
				}
			}
			slog.DebugContext(cmd.Context(), "planning cleanup", "root", t.Dir, "apply", apply, "level", policy.Level)
			rs, e := cleanup.RunWithOptions(cmd.Context(), filepath.Clean(t.Dir), apply, t.Includes, t.Excludes, cleanup.Options{Level: policy.Level, DiscardLocalChanges: discard})
			results = append(results, rs...)
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.Dir, e))
			} else if len(rs) == 0 {
				lines = append(lines, "No repositories found")
			}
			for _, r := range rs {
				destructive := ""
				if r.Destructive {
					destructive = " [DISCARDS LOCAL FILES]"
				}
				lines = append(lines, fmt.Sprintf("%-14s %q%s: %s", r.Action, r.Path, destructive, r.Reason))
			}
		}
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "clean", DryRun: !apply, Items: results, Errors: errs, CleanupPolicies: policies}, lines)
	}}
	bind(cmd, &o)
	cmd.Flags().BoolVar(&apply, "apply", false, "revalidate the selected policy and remove eligible worktrees")
	cmd.Flags().StringVar(&level, "level", "", "cleanup level: conservative (default), balanced, or aggressive; overrides config")
	cmd.Flags().BoolVar(&discard, "discard-local-changes", false, "authorize permanent loss of tracked changes, untracked files, and ignored files; required for aggressive apply")
	return cmd
}
