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
	"github.com/richhaase/repoman/internal/progress"
	"github.com/richhaase/repoman/internal/repository"
	"github.com/richhaase/repoman/internal/syncer"
)

// ExitError preserves structured output while communicating a partial failure.
type ExitError struct {
	Code     int
	Err      error
	Reported bool
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
	SyncPolicies    []syncPolicy    `json:"sync_policies,omitempty"`
}

type syncPolicy struct {
	Root       string `json:"root"`
	FetchScope string `json:"fetch_scope"`
	Prune      bool   `json:"prune"`
	Force      bool   `json:"force"`
	Cleanup    bool   `json:"cleanup"`
}

type cleanupPolicy struct {
	Root                string        `json:"root"`
	Level               cleanup.Level `json:"level"`
	DiscardLocalChanges bool          `json:"discard_local_changes"`
}

type options struct {
	root, configPath string
	json             bool
	report           *humanReport
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
func output(cmd *cobra.Command, o options, env envelope) error {
	if o.json {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(env); err != nil {
			return err
		}
		for _, warning := range env.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", humanText(warning))
		}
	} else {
		o.report.finish(env)
	}
	if err := cmd.Context().Err(); err != nil {
		if !o.json {
			return &ExitError{Code: 130, Err: err, Reported: true}
		}
		return err
	}
	if len(env.Errors) > 0 {
		return &ExitError{Code: 3, Err: errors.New("completed with errors; see reported details"), Reported: !o.json}
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
	cmd := &cobra.Command{Use: "status [DIR]", Short: "See clones, branches, and worktrees without network access", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ts, err := selection(cmd, o, args)
		if err != nil {
			return err
		}
		o.report = newHumanReport(cmd, o.json)
		states := []repository.State{}
		errs := []string{}
		seen := map[string]bool{}
		for _, t := range ts {
			o.report.target(t.Dir, "Status · read only", o.root != "")
			o.report.line("  · Checking local repositories")
			ss, err := repository.Discover(cmd.Context(), t.Dir)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.Dir, err))
			}
			unique := []repository.State{}
			for _, state := range ss {
				if seen[state.Path] {
					continue
				}
				seen[state.Path] = true
				states = append(states, state)
				unique = append(unique, state)
				if len(state.Problems) > 0 {
					errs = append(errs, fmt.Sprintf("%s: %s", state.Path, strings.Join(state.Problems, "; ")))
				}
			}
			o.report.statusStates(unique)
		}
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "status", DryRun: true, Items: states, Errors: errs})
	}}
	bind(cmd, &o)
	cmd.Example = "  repoman status\n  repoman status --root ~/src\n  repoman status --json"
	return cmd
}
func newSyncCmd() *cobra.Command {
	return newSyncCmdWithRunner(syncer.RunWithOptions)
}

func validateFetchScopeFlag(scope string) error {
	if scope == "" {
		return fmt.Errorf("--fetch-scope cannot be empty; use origin or all")
	}
	_, err := syncer.ParseFetchScope(scope)
	return err
}

func newSyncCmdWithRunner(run func(context.Context, syncer.Target, syncer.Options) ([]syncer.Result, error)) *cobra.Command {
	var o options
	var owner, excludesFile, fetchScope string
	var days int
	var noEvents, dryRun, force, cleanupInactive, prune bool
	var includes, excludes []string
	cmd := &cobra.Command{Use: "sync [DIR]", Short: "Clone and update your active GitHub repositories", Long: "Clone missing active repositories and fetch existing clones. Clean default branches fast-forward.\n\nFetch scope and pruning are independent. --force resets active checkouts;\n--cleanup removes eligible inactive clones. Neither is enabled by fetching.\nUse --dry-run to see the effective settings and planned work first.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("fetch-scope") {
			if err := validateFetchScopeFlag(fetchScope); err != nil {
				return &ExitError{Code: 2, Err: err}
			}
		}
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
		o.report = newHumanReport(cmd, o.json)
		policies := make([]syncPolicy, 0, len(ts))
		syncOptions := syncer.Options{DryRun: dryRun, Force: force, Cleanup: cleanupInactive, FetchScope: fetchScope}
		if cmd.Flags().Changed("prune") {
			syncOptions.Prune = &prune
		}
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
			scope := t.FetchScope
			if fetchScope != "" {
				scope = fetchScope
			}
			scope, _ = syncer.ParseFetchScope(scope) // Config and explicit flags were validated before any target runs.
			effectivePrune := t.Prune
			if syncOptions.Prune != nil {
				effectivePrune = *syncOptions.Prune
			}
			policies = append(policies, syncPolicy{Root: t.Dir, FetchScope: scope, Prune: effectivePrune, Force: force, Cleanup: cleanupInactive})
			phase := "apply"
			if dryRun {
				phase = "preview"
			}
			o.report.target(t.Dir, "Sync · "+phase, o.root != "")
			o.report.syncHeader(t, policies[len(policies)-1], dryRun)
			slog.DebugContext(cmd.Context(), "syncing target", "root", t.Dir, "owner", t.Owner, "dry_run", dryRun)
			ctx := cmd.Context()
			if o.report != nil {
				ctx = progress.WithReporter(ctx, o.report.event)
			}
			rs, e := run(ctx, t, syncOptions)
			results = append(results, rs...)
			if e != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.Dir, e))
			}
			for _, r := range rs {
				o.report.syncResult(r)
			}
			o.report.syncError(t.Dir, e)
		}
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "sync", DryRun: dryRun, Items: results, Errors: errs, SyncPolicies: policies})
	}}
	bind(cmd, &o)
	cmd.Flags().StringVarP(&owner, "owner", "o", "", "GitHub user or organization (defaults to authenticated gh user)")
	cmd.Flags().IntVarP(&days, "days", "d", 45, "positive push-activity window in days")
	cmd.Flags().BoolVar(&noEvents, "no-events", false, "explicitly use push-only activity for legacy events=true configs")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview clone, fetch, checkout, and inactive cleanup without changes")
	cmd.Flags().StringArrayVar(&includes, "include", nil, "repository-name glob to include (repeatable)")
	cmd.Flags().StringArrayVarP(&excludes, "exclude", "e", nil, "repository-name glob to exclude (repeatable; wins over includes)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "discard local changes and force the default branch to origin (destructive)")
	cmd.Flags().StringVar(&fetchScope, "fetch-scope", "", "fetch origin (default) or all configured remotes; overrides target config")
	cmd.Flags().BoolVar(&prune, "prune", false, "prune stale remote-tracking refs when fetching; overrides target config (use --prune=false to disable)")
	cmd.Flags().BoolVarP(&cleanupInactive, "cleanup", "c", false, "remove inactive primary clones without uncommitted or untracked changes (destructive)")
	cmd.Flags().StringVar(&excludesFile, "excludes-file", "", "read exclude patterns from FILE (blank lines and # comments ignored)")
	cmd.Example = "  repoman sync --dry-run\n  repoman sync\n  repoman sync --fetch-scope all --prune\n  repoman sync --force --dry-run"
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
	cmd := &cobra.Command{Use: "clean [DIR]", Short: "Remove eligible linked worktrees (applies by default)", Long: "Remove eligible linked worktrees; primary clones are always kept.\n\nThis command applies immediately unless --dry-run is used. The default\naggressive policy can discard local checkout files, including local changes;\nbranch refs are retained. Choose balanced or conservative to retain more work.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		o.report = newHumanReport(cmd, o.json)
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

		}
		for i, t := range ts {
			phase := "apply"
			if !effectiveApply {
				phase = "preview"
			}
			o.report.target(t.Dir, "Clean · "+phase, o.root != "")
			o.report.line("policy:   %s", policies[i].Level)
			o.report.filters(t.Includes, t.Excludes)
			if policies[i].Level == cleanup.Aggressive {
				o.report.line("! Removes checkout files, including local changes; branch refs are retained")
			}
			if !effectiveApply {
				o.report.line("Preview only · nothing will be changed")
			}
		}
		ctx := cmd.Context()
		if o.report != nil {
			ctx = progress.WithReporter(ctx, o.report.event)
		}
		results, err := run(ctx, targets, effectiveApply)
		errs := []string{}
		if err != nil {
			errs = append(errs, err.Error())
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
		}
		o.report.cleanResults(results, false)
		return output(cmd, o, envelope{SchemaVersion: 1, Command: "clean", DryRun: !effectiveApply, Items: results, Errors: errs, CleanupPolicies: policies, Warnings: warnings})
	}}
	bind(cmd, &o)
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "preview eligible removals and metadata pruning without changing anything")
	cmd.Flags().BoolVar(&apply, "apply", true, "compatibility alias: true applies; false previews")
	cmd.Flags().StringVar(&level, "level", "", "cleanup level: aggressive (default), balanced, or conservative; overrides config")
	cmd.Flags().BoolVar(&discard, "discard-local-changes", false, "deprecated compatibility alias for aggressive cleanup; no acknowledgement is required")
	cmd.Flags().IntVarP(&days, "days", "d", 45, "positive compatibility option; does not affect cleanup retention")
	cmd.Example = "  repoman clean --dry-run\n  repoman clean\n  repoman clean --level conservative --dry-run"
	for _, name := range []string{"apply", "discard-local-changes", "days"} {
		_ = cmd.Flags().MarkHidden(name)
	}
	return cmd
}
