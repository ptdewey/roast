// Package cli wires Roast's local review pipeline to Cobra.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/roast/internal/buildinfo"
	"github.com/bluesky-social/roast/internal/config"
	rcontext "github.com/bluesky-social/roast/internal/context"
	"github.com/bluesky-social/roast/internal/gate"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/prompts"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/render"
	"github.com/bluesky-social/roast/internal/review"
	"github.com/bluesky-social/roast/internal/runbundle"
	"github.com/bluesky-social/roast/internal/runupload"
	"github.com/bluesky-social/roast/internal/scope"
	"github.com/bluesky-social/roast/internal/snapshot"
	"github.com/bluesky-social/roast/internal/textsafe"
	"github.com/bluesky-social/roast/internal/updatecheck"
	"github.com/bluesky-social/roast/internal/verify"
)

const (
	ExitClean     = 0
	ExitFindings  = 1
	ExitUsage     = 2
	ExitAuth      = 3
	ExitPartial   = 4
	ExitCancelled = 5
)

// ErrorCategory is the stable machine-readable classification paired with an
// exit code. Multiple operational categories intentionally share ExitPartial.
type ErrorCategory string

const (
	ErrorNone      ErrorCategory = "none"
	ErrorFindings  ErrorCategory = "findings"
	ErrorUsage     ErrorCategory = "usage"
	ErrorAuth      ErrorCategory = "auth"
	ErrorEndpoint  ErrorCategory = "endpoint"
	ErrorResource  ErrorCategory = "resource_limit"
	ErrorPartial   ErrorCategory = "partial"
	ErrorCancelled ErrorCategory = "cancelled"
)

type commandError struct {
	err    error
	format string
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

// ErrorCategoryFor maps one CLI result to its stable category.
func ErrorCategoryFor(code int, err error) ErrorCategory {
	switch {
	case code == ExitClean:
		return ErrorNone
	case code == ExitFindings:
		return ErrorFindings
	case errors.Is(err, context.Canceled):
		return ErrorCancelled
	case errors.Is(err, llm.ErrAuth):
		return ErrorAuth
	case errors.Is(err, llm.ErrEndpoint):
		return ErrorEndpoint
	case errors.Is(err, llm.ErrBudgetExhausted), errors.Is(err, llm.ErrWorkDeadline), errors.Is(err, llm.ErrNoFinalCall), errors.Is(err, llm.ErrUsageLimit):
		return ErrorResource
	}
	var usage *usageError
	if errors.As(err, &usage) || code == ExitUsage {
		return ErrorUsage
	}
	if code == ExitCancelled {
		return ErrorCancelled
	}
	return ErrorPartial
}

// WriteError writes the user-selected text or JSON error representation.
func WriteError(w io.Writer, code int, err error) {
	if err == nil {
		return
	}
	format := "text"
	var classified *commandError
	if errors.As(err, &classified) {
		format = classified.format
	}
	category := ErrorCategoryFor(code, err)
	if format == "json" {
		_ = json.NewEncoder(w).Encode(struct {
			Category ErrorCategory `json:"error_category"`
			Message  string        `json:"message"`
			ExitCode int           `json:"exit_code"`
		}{category, err.Error(), code})
		return
	}
	// Errors can carry git stderr naming repository paths.
	fmt.Fprintf(w, "roast: %s\n", textsafe.Text(err.Error()))
}

type gateExit struct{ code int }

func (e *gateExit) Error() string { return fmt.Sprintf("exit %d", e.code) }

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

var gateBootstrapCommands = map[string]bool{"roast": true, "review": true}

type reviewFlags struct {
	configPath      string
	base            string
	uncommittedOnly bool
	staged          bool
	task            string
	format          string
	maxSeverity     string
	paths           []string
	excludes        []string
	includeVendored bool
	mode            string
	quick           bool
	noVerify        bool
	interactive     bool
	maxTurns        int
	deadline        time.Duration
	requestTimeout  time.Duration
	maxTokensTotal  int
	maxVerified     int
	maxAttempts     int
	showDropped     bool
	langs           []string
	scope           string
	authorModel     string
	finder          roleFlags
	verifier        roleFlags
}

const (
	defaultMaxTurns          = 16
	surveyMaxTurnsDefault    = 40
	defaultDeadline          = 10 * time.Minute
	surveyDeadlineDefault    = 25 * time.Minute
	defaultRequestTimeout    = 10 * time.Minute
	defaultMaxAttempts       = 4
	defaultMaxTotalTokens    = 3_000_000
	defaultMaxVerified       = 25
	defaultMaxDiffBytes      = 1_000_000
	defaultQuickDiffBytes    = 200_000
	defaultMaxFileCtxBytes   = 400_000
	defaultQuickFileCtxBytes = 100_000
	defaultMaxPromptBytes    = 8_000_000
)

type roleFlags struct {
	baseURL         string
	model           string
	protocol        string
	maxOutputTokens int
}

// Execute runs the process CLI.
func Execute() (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ExecuteContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

// ExecuteContext runs the CLI with explicit streams for tests and embedding.
func ExecuteContext(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	root := newRootCommand(stdout, stderr)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitClean, nil
	}
	var gate *gateExit
	if errors.As(err, &gate) {
		return gate.code, nil
	}
	code := exitCode(err)
	format, _ := root.PersistentFlags().GetString("error-format")
	return code, &commandError{err: err, format: format}
}

func exitCode(err error) int {
	switch {
	case err == nil:
		return ExitClean
	case errors.Is(err, context.Canceled):
		return ExitCancelled
	case errors.Is(err, llm.ErrAuth):
		return ExitAuth
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	return ExitPartial
}

func newRootCommand(stdout, stderr io.Writer) *cobra.Command {
	var rootFlags reviewFlags
	root := &cobra.Command{
		Use:           "roast",
		Short:         "Adversarial cross-model code review",
		Long:          "A finder proposes potential defects, then an independent verifier adjudicates each claim against the local checkout.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := scope.GitRoot("."); err != nil {
				fmt.Fprintln(stderr, "roast: not in a Git repository; cd into one to review, or try `roast doctor` / `roast --help`")
				if helpErr := cmd.Help(); helpErr != nil {
					return helpErr
				}
				return nil
			}
			return runReview(cmd, &rootFlags, stdout, stderr)
		},
	}
	root.PersistentFlags().String("error-format", "text", "error output: text|json")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		format, _ := cmd.Flags().GetString("error-format")
		if format != "text" && format != "json" {
			return &usageError{msg: fmt.Sprintf("invalid --error-format %q (text|json)", format)}
		}
		startUpdateCheck(cmd.Context(), cmd.Name(), format, stderr)
		if format != "json" && gateBootstrapCommands[cmd.Name()] {
			maybeBootstrapGate(stderr)
		}
		return nil
	}
	root.SetFlagErrorFunc(flagUsageError)
	addReviewFlags(root, &rootFlags)

	var subFlags reviewFlags
	reviewCmd := &cobra.Command{
		Use:           "review",
		Short:         "Review the current diff",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReview(cmd, &subFlags, stdout, stderr)
		},
	}
	reviewCmd.SetFlagErrorFunc(flagUsageError)
	addReviewFlags(reviewCmd, &subFlags)
	root.AddCommand(reviewCmd)
	root.AddCommand(newRunsCommand(stdout, stderr))
	root.AddCommand(newGateCommand(stdout))
	root.AddCommand(newConfigCommand(stdout, stderr))
	root.AddCommand(newSkillCommand(stdout))
	root.AddCommand(newDoctorCommand(stdout))
	root.AddCommand(newAuthCommand(stdout, stderr))
	return root
}

func maybeBootstrapGate(stderr io.Writer) {
	repo, err := scope.GitRoot(".")
	if err != nil {
		return
	}
	opts := gate.BootstrapOptions{Repo: repo, Out: stderr}
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) {
		opts.Prompt = func(question string) (bool, error) {
			return promptYesDefault(stderr, os.Stdin, question)
		}
	}
	if _, err := gate.Bootstrap(opts); err != nil {
		fmt.Fprintf(stderr, "roast: pre-commit gate setup skipped: %v\n", err)
	}
}

func startUpdateCheck(ctx context.Context, command, errorFormat string, stderr io.Writer) {
	if updateCheckDisabled() || command == "doctor" {
		return
	}
	checker, err := updatecheck.Default()
	if err != nil {
		return
	}
	cached, err := checker.Cached(buildinfo.Version)
	if err == nil && cached.UpdateAvailable() && errorFormat != "json" {
		fmt.Fprintf(stderr, "roast: %s is available (running %s); update with Homebrew or your installation method\n", cached.Latest, buildinfo.Version)
	}
	if err != nil || checker.NeedsRefresh(cached) {
		go func() {
			refreshContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = checker.Check(refreshContext, buildinfo.Version, true)
		}()
	}
}

func updateCheckDisabled() bool {
	value := strings.TrimSpace(os.Getenv("ROAST_NO_UPDATE_CHECK"))
	return value == "1" || strings.EqualFold(value, "true")
}

func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return &usageError{msg: err.Error()}
	}
	return nil
}

// usageArgs makes a Cobra positional-argument validator report usage errors.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return &usageError{msg: err.Error()}
		}
		return nil
	}
}

// showHelp makes a command group runnable, so its noArgs validator rejects an
// unknown subcommand as a usage error instead of Cobra printing help and
// exiting cleanly.
func showHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

func flagUsageError(_ *cobra.Command, err error) error {
	return &usageError{msg: err.Error()}
}

func addReviewFlags(cmd *cobra.Command, f *reviewFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.configPath, "config", "", "config file (default: user config directory/roast/config.yaml)")
	fl.StringVar(&f.base, "base", "", "diff base ref (default: origin/main with fallbacks)")
	fl.BoolVar(&f.uncommittedOnly, "uncommitted-only", false, "only review uncommitted changes")
	fl.BoolVar(&f.staged, "staged", false, "review exactly the staged set")
	fl.StringVar(&f.task, "task", "(not provided)", "what the builder was asked to do")
	fl.StringVar(&f.format, "format", "both", "json|markdown|both")
	fl.StringVar(&f.maxSeverity, "max-severity", "", "exit 1 when a finding meets INFO|LOW|MEDIUM|HIGH|CRITICAL")
	fl.StringSliceVar(&f.paths, "paths", nil, "limit review to these git pathspecs")
	fl.StringSliceVar(&f.excludes, "exclude", nil, "exclude these pathspecs")
	fl.BoolVar(&f.includeVendored, "include-vendored", false, "review vendored and generated files")
	fl.StringVar(&f.mode, "mode", "auto", "code|prose|mixed|auto")
	fl.BoolVar(&f.quick, "quick", false, "single-shot finder pass without verification")
	fl.BoolVar(&f.noVerify, "no-verify", false, "skip cross-model verification")
	fl.BoolVar(&f.interactive, "interactive", false, "let the finder navigate the local checkout")
	fl.IntVar(&f.maxTurns, "max-turns", defaultMaxTurns, fmt.Sprintf("maximum interactive finder turns (%d with --scope repo)", surveyMaxTurnsDefault))
	fl.DurationVar(&f.deadline, "deadline", 0, fmt.Sprintf("interactive finder work deadline (default %s, %s with --scope repo; 0 disables)", shortDuration(defaultDeadline), shortDuration(surveyDeadlineDefault)))
	fl.DurationVar(&f.requestTimeout, "request-timeout", 0, fmt.Sprintf("timeout for each inference request (default %s; 0 disables)", shortDuration(defaultRequestTimeout)))
	fl.IntVar(&f.maxTokensTotal, "max-tokens-total", 0, fmt.Sprintf("shared finder/verifier token ceiling (default %d; 0 disables)", defaultMaxTotalTokens))
	fl.IntVar(&f.maxVerified, "max-verified", 25, "maximum findings sent to the verifier (0 disables)")
	fl.IntVar(&f.maxAttempts, "max-attempts", 4, "maximum provider and structured-output attempts")
	fl.BoolVar(&f.showDropped, "show-dropped", false, "render malformed and refuted candidates")
	fl.StringSliceVar(&f.langs, "lang", nil, "language prompt packs (default: auto; use none to disable)")
	fl.StringVar(&f.scope, "scope", "diff", "review scope: diff|repo (repo performs prioritized survey triage)")
	fl.StringVar(&f.authorModel, "author-model", "", "author model or family; use roles.finder.author_routes to select an independent profile")
	addRoleFlags(fl, "finder", &f.finder)
	addRoleFlags(fl, "verifier", &f.verifier)
}

// shortDuration renders whole-minute durations as "10m" rather than "10m0s".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func addRoleFlags(fl interface {
	StringVar(*string, string, string, string)
	IntVar(*int, string, int, string)
}, name string, role *roleFlags) {
	fl.StringVar(&role.baseURL, name+"-base-url", "", name+" inference API base URL")
	fl.StringVar(&role.model, name+"-model", "", name+" model name")
	fl.StringVar(&role.protocol, name+"-protocol", "", name+" protocol: chat|responses|anthropic")
	fl.IntVar(&role.maxOutputTokens, name+"-max-output-tokens", 0, name+" output-token ceiling")
}

func runReview(cmd *cobra.Command, f *reviewFlags, stdout, stderr io.Writer) error {
	resolved := func() bool { return rolesResolve(cmd.Flags().Changed, f.finder, f.verifier) }
	if err := maybeProvision(cmd, f.configPath, stderr, resolved); err != nil {
		return err
	}
	return runReviewCommand(cmd.Context(), cmd.Flags().Changed, f, stdout, stderr)
}

func runReviewCommand(ctx context.Context, changed func(string) bool, f *reviewFlags, stdout, stderr io.Writer) error {
	if f.staged && f.uncommittedOnly {
		return &usageError{msg: "--staged and --uncommitted-only cannot be combined"}
	}
	if f.format != "json" && f.format != "markdown" && f.format != "both" {
		return &usageError{msg: fmt.Sprintf("invalid --format %q", f.format)}
	}
	if f.maxTurns < 1 {
		return &usageError{msg: "max-turns must be positive"}
	}
	if f.deadline < 0 {
		return &usageError{msg: "deadline cannot be negative"}
	}
	if f.requestTimeout < 0 {
		return &usageError{msg: "request-timeout cannot be negative"}
	}
	if f.maxTokensTotal < 0 {
		return &usageError{msg: "max-tokens-total cannot be negative"}
	}
	if f.maxVerified < 0 {
		return &usageError{msg: "max-verified cannot be negative"}
	}
	if f.maxAttempts < 1 {
		return &usageError{msg: "max-attempts must be positive"}
	}
	if f.scope != "diff" && f.scope != "repo" {
		return &usageError{msg: fmt.Sprintf("invalid --scope %q (diff|repo)", f.scope)}
	}
	survey := f.scope == "repo"
	if survey && f.quick {
		return &usageError{msg: "--quick cannot run a repository survey; drop --quick"}
	}
	if survey && (f.staged || f.uncommittedOnly || f.base != "") {
		return &usageError{msg: "--scope repo cannot be combined with --staged, --uncommitted-only, or --base"}
	}
	gateRank := -1
	if f.maxSeverity != "" {
		sev, ok := review.ParseSeverity(strings.ToUpper(f.maxSeverity))
		if !ok {
			return &usageError{msg: fmt.Sprintf("invalid --max-severity %q", f.maxSeverity)}
		}
		gateRank = sev.Rank()
	}

	cfg, err := config.Load(f.configPath)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	// Establish the opt-in boundary before this run creates its directory. A
	// later best-effort upload can then never mistake an older run for one
	// produced after consent, even when multiple reviews finish concurrently.
	if cfg.Upload.Enabled {
		if recordDir := record.DefaultDir(); recordDir != "" {
			_ = runupload.EnsureEnrolled(recordDir)
		}
	}
	finderRole, err := cfg.ResolveRole("finder")
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	verifierRole, err := cfg.ResolveRole("verifier")
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	finderRole = applyRoleFlags(changed, "finder", finderRole, f.finder)
	verifierRole = applyRoleFlags(changed, "verifier", verifierRole, f.verifier)
	authorModel := f.authorModel
	if !changed("author-model") {
		authorModel = config.AuthorModel()
	}
	flip, err := cfg.ResolveFinderFlip(finderRole, authorModel)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	if flip.Note != "" {
		logf(stderr, "%s", flip.Note)
	}
	if flip.Warn != "" {
		logf(stderr, "%s", flip.Warn)
	}
	finderRole = flip.Role
	if err := config.ValidateRole("finder", finderRole); err != nil {
		return &usageError{msg: err.Error()}
	}
	if config.IsOAuth(finderRole.Auth) {
		if err := subscriptionPreflight(ctx, finderRole); err != nil {
			return err
		}
	}
	doVerify := !f.quick && !f.noVerify
	if doVerify {
		if err := config.ValidateRole("verifier", verifierRole); err != nil {
			return &usageError{msg: err.Error()}
		}
		if config.IsOAuth(verifierRole.Auth) {
			if err := subscriptionPreflight(ctx, verifierRole); err != nil {
				return err
			}
		}
	}

	repo, err := scope.GitRoot(".")
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	excludes := append(append([]string{}, cfg.Review.Excludes...), f.excludes...)
	maxDiffDefault, maxFileContextDefault := defaultMaxDiffBytes, defaultMaxFileCtxBytes
	if f.quick {
		maxDiffDefault, maxFileContextDefault = defaultQuickDiffBytes, defaultQuickFileCtxBytes
	}
	maxDiff, err := envIntAtLeast("ROAST_MAX_DIFF_BYTES", maxDiffDefault, 1)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	build := scope.BuildOpts{
		Base:            f.base,
		UncommittedOnly: f.uncommittedOnly,
		StagedOnly:      f.staged,
		Paths:           f.paths,
		Excludes:        excludes,
		IncludeVendored: f.includeVendored,
		Scope:           f.scope,
		// No one file's diff can be shown past the whole-diff budget, so
		// Build need not hold more of it than that.
		MaxFileDiffBytes: maxDiff,
	}
	tree := rcontext.Tree{}
	if f.staged {
		tree, err = rcontext.PinIndexTree(repo)
		if err != nil {
			return fmt.Errorf("pin staged tree: %w", err)
		}
		build.StagedTreeOID = tree.OID
	}
	sc, err := scope.Build(repo, build)
	if err != nil {
		return err
	}
	for _, note := range sc.Notes {
		logf(stderr, "%s", note)
	}
	if sc.Empty() {
		result := &review.Result{Assessment: review.AssessmentNoDiff, Findings: []review.Finding{}, Dropped: []review.Finding{}, Notes: []string{}}
		report := render.Report{Result: result, Mode: "code", FilesReviewed: []string{}, Task: f.task}
		writeReport(stdout, stderr, f, report)
		return nil
	}
	inputKind, reviewedInput := "diff", sc.Diff
	if survey {
		inputKind, reviewedInput = "manifest", sc.Manifest
	}
	runID := record.NewRunID()
	// Pin the reviewed corpus whether or not a replayable snapshot can be
	// recorded (snapshots are refused without a HEAD, with an unmerged index,
	// or under ROAST_SNAPSHOT=0). Only when Git cannot write the tree at all
	// do readers fall back to the live tree, which context guards separately.
	var pin snapshot.Pin
	indexTree := tree.OID
	if !sc.Staged {
		var pinErr error
		pin, pinErr = snapshot.PinWorktree(repo, nil)
		if pinErr != nil {
			logf(stderr, "could not pin the working tree (%v); reading it live with .git, ignored, and denylisted paths blocked", pinErr)
		} else {
			tree = rcontext.PinnedWorktree(pin.Tree)
		}
	}
	snapshotMeta, snapshotCaveats := captureSnapshot(repo, runID, sc, indexTree, pin)
	snapshotDurable := false
	if snapshotMeta.Captured {
		defer func() {
			if !snapshotDurable {
				_ = snapshot.Delete(repo, runID)
			}
		}()
	}

	ground := rcontext.NewQuoteGrounder(repo, tree, sc.Diff, "")
	originalDiffBytes := len(sc.Diff)
	maxFileContext, err := envIntAtLeast("ROAST_MAX_FILE_CTX_BYTES", maxFileContextDefault, 1)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	breakdown := scope.Fit(&sc, maxDiff)

	mode := f.mode
	if mode == "auto" {
		mode = review.ClassifyMode(sc.Files)
	}
	if mode != "code" && mode != "prose" && mode != "mixed" {
		return &usageError{msg: fmt.Sprintf("invalid --mode %q", f.mode)}
	}
	langs, err := resolveLangs(f.langs, sc.Files, mode)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	finderPrompt := cfg.Prompts.FinderCode
	if mode == "prose" {
		finderPrompt = cfg.Prompts.FinderProse
	}
	system, err := prompts.Finder(mode, f.scope, f.quick, langs, finderPrompt)
	if err != nil {
		return err
	}
	verifierSystem, err := prompts.Verifier(cfg.Prompts.Verifier)
	if err != nil {
		return err
	}
	var user string
	if survey {
		user = prompts.FinderUser(prompts.FinderUserVars{Task: f.task, Survey: true, Manifest: sc.Manifest})
	} else {
		user = prompts.FinderUser(prompts.FinderUserVars{
			Task:        f.task,
			Diff:        sc.Diff,
			FileContext: rcontext.BuildFileContext(repo, sc.Files, maxFileContext, tree),
		})
	}

	maxTokensTotal, err := resolveIntCeiling(changed, "max-tokens-total", f.maxTokensTotal, "ROAST_MAX_TOTAL_TOKENS", defaultMaxTotalTokens)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	maxVerified, err := resolveIntCeiling(changed, "max-verified", f.maxVerified, "ROAST_MAX_VERIFIED", defaultMaxVerified)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	requestTimeout := f.requestTimeout
	if !changed("request-timeout") {
		requestTimeout, err = envDuration("ROAST_REQUEST_TIMEOUT", defaultRequestTimeout)
		if err != nil {
			return &usageError{msg: err.Error()}
		}
	}
	maxAttempts := f.maxAttempts
	if !changed("max-attempts") {
		maxAttempts, err = envIntAtLeast("ROAST_MAX_ATTEMPTS", defaultMaxAttempts, 1)
		if err != nil {
			return &usageError{msg: err.Error()}
		}
	}
	ledger := llm.NewLedger(maxTokensTotal)
	interactive := f.interactive && !f.quick
	maxTurns := f.maxTurns
	if survey {
		interactive = true
		if !changed("max-turns") {
			maxTurns = surveyMaxTurnsDefault
		}
	}
	if interactive && !survey {
		minLines, limitErr := envIntAtLeast("ROAST_INTERACTIVE_MIN_DIFF_LINES", 80, 0)
		if limitErr != nil {
			return &usageError{msg: limitErr.Error()}
		}
		minFiles, limitErr := envIntAtLeast("ROAST_INTERACTIVE_MIN_FILES", 4, 0)
		if limitErr != nil {
			return &usageError{msg: limitErr.Error()}
		}
		if strings.Count(sc.Diff, "\n") < minLines && len(sc.Files) < minFiles {
			logf(stderr, "diff is small; using a single structured finder call")
			interactive = false
		}
	}
	deadlineDuration := f.deadline
	if !changed("deadline") {
		deadlineDuration = defaultDeadline
		if survey {
			deadlineDuration = surveyDeadlineDefault
		}
		if value := os.Getenv("ROAST_DEADLINE"); value != "" {
			deadlineDuration, err = parseNonNegativeDuration("ROAST_DEADLINE", value)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
		}
	}
	var deadline time.Time
	if deadlineDuration > 0 {
		deadline = time.Now().Add(deadlineDuration)
	}
	budgets := map[string]any{
		"deadline_duration": deadlineDuration.String(),
		"request_timeout":   requestTimeout.String(),
		"max_total_tokens":  maxTokensTotal,
		"verifier_cap":      maxVerified,
		"max_turns":         maxTurns,
		"max_attempts":      maxAttempts,
	}
	tools := []llm.Tool(nil)
	if interactive {
		tools = rcontext.ExplorationTools(repo, tree)
	}
	floor := cfg.Review.ConfidenceFloor
	if f.quick {
		floor = cfg.Review.QuickConfidenceFloor
	}
	promptCeiling, err := envIntAtLeast("ROAST_MAX_PROMPT_BYTES", defaultMaxPromptBytes, 1)
	if err != nil {
		return &usageError{msg: err.Error()}
	}
	budgets["max_prompt_bytes"] = promptCeiling
	if promptBytes, ceiling := len(system)+len(user), promptCeiling; promptBytes > ceiling {
		return &usageError{msg: fmt.Sprintf("finder prompt is %d bytes, over the %d-byte ceiling; narrow the review scope or adjust ROAST_MAX_DIFF_BYTES, ROAST_MAX_FILE_CTX_BYTES, or ROAST_MAX_PROMPT_BYTES", promptBytes, ceiling)}
	}
	scopeBaseOID := sc.BaseOID
	if scopeBaseOID == "" {
		scopeBaseOID = snapshotMeta.Base
	}
	scopeHeadOID := sc.HeadOID
	if scopeHeadOID == "" {
		scopeHeadOID = snapshotMeta.Head
	}
	envValues, envNames := record.CaptureEnv(cfg.CredentialEnvNames()...)
	meta := record.Meta{
		RunID: runID, TS: time.Now().UTC().Format(time.RFC3339Nano), Repo: repo,
		Argv:  append([]string(nil), os.Args[1:]...),
		Roast: record.BuildMeta{Version: buildinfo.Version, Commit: buildinfo.Commit},
		Scope: record.ScopeMeta{
			Kind: f.scope, Base: sc.Base, UncommittedOnly: f.uncommittedOnly,
			StagedOnly: f.staged, IncludeVendored: f.includeVendored,
			Paths: append([]string(nil), f.paths...), Excludes: append([]string(nil), excludes...),
			BaseOID: scopeBaseOID, MergeBaseOID: sc.MergeBaseOID, HeadOID: scopeHeadOID,
			Survey: sc.Survey, Staged: sc.Staged, Files: append([]string(nil), sc.Files...),
		},
		Derived: record.DerivedMeta{
			Mode: mode, Langs: append([]string(nil), langs...), Quick: f.quick,
			Interactive: interactive, MaxTurns: maxTurns, ConfidenceFloor: floor,
			DoVerify: doVerify, Task: f.task, MaxSeverity: f.maxSeverity,
			MaxDiffBytes: maxDiff, MaxFileCtxBytes: maxFileContext,
		},
		Roles: map[string]record.RoleMeta{
			"finder":   roleMeta(finderRole, system),
			"verifier": roleMeta(verifierRole, verifierSystem),
		},
		Snapshot: snapshotMeta,
		Digests: record.DigestMeta{
			InputKind: inputKind, Input: record.Digest(reviewedInput), InputBytes: len(reviewedInput),
			System: record.Digest(system), User: record.Digest(user),
		},
		Budgets: budgets, Env: envValues, EnvNamesOnly: envNames, Caveats: snapshotCaveats,
	}
	if err := initializeRunArtifacts(meta, reviewedInput, system, user, verifierSystem); err != nil {
		_ = record.RemoveRun(runID)
		return err
	}
	snapshotDurable = true
	responseRecorder := record.NewResponseRecorder(runID)
	captureResponse := func(response llm.RawResponse) error {
		return responseRecorder.Append(response.Role, response.Protocol, response.Model, response.ContentType, response.Body)
	}
	finder, err := llm.New(ctx, clientConfig("finder", finderRole, ledger, captureResponse, maxAttempts, requestTimeout))
	if err != nil {
		return err
	}
	logf(stderr, "finder=%s profile=%s protocol=%s endpoint=%s", finderRole.Model, finderRole.Profile, finderRole.Protocol, finderRole.BaseURL)

	result, err := review.Find(ctx, finder, review.FindInput{
		System: system, User: user,
		Quick: f.quick, ConfidenceFloor: floor,
		Interactive: interactive,
		Tools:       tools,
		MaxTurns:    maxTurns,
		Ground:      ground,
		Deadline:    deadline,
		Survey:      survey,
		Capture:     record.OpenFindingSpill(runID),
	})
	if err != nil {
		return err
	}

	usage := map[string]llm.Usage{"finder": finder.Usage()}
	partial := result.Incomplete
	if doVerify && len(result.Findings) > 0 {
		verifier, newErr := llm.New(ctx, clientConfig("verifier", verifierRole, ledger, captureResponse, maxAttempts, requestTimeout))
		if newErr != nil {
			return newErr
		}
		logf(stderr, "verifying %d finding(s) with %s (profile=%s protocol=%s)", len(result.Findings), verifierRole.Model, verifierRole.Profile, verifierRole.Protocol)
		result = verify.Verify(ctx, verifier, result, verify.Deps{
			SystemPrompt: verifierSystem,
			UserPrompt: func(finding review.Finding) string {
				return verifierPrompt(repo, tree, finding)
			},
			Tools:       rcontext.ExplorationTools(repo, tree),
			MaxTurns:    8,
			MaxVerified: maxVerified,
			Ledger:      ledger,
			Log:         func(message string) { fmt.Fprintln(stderr, message) },
		})
		usage["verifier"] = verifier.Usage()
		for _, finding := range result.Findings {
			if finding.Verdict != nil && finding.Verdict.Undecided {
				partial = true
			}
		}
	}

	var reviewKind, completion, stopReason string
	if survey {
		reviewKind = "survey_triage"
		if result.Incomplete || result.StopReason == "deadline" || result.StopReason == "token_budget" || result.StopReason == "budget_exhausted" {
			completion = "partial"
			stopReason = result.StopReason
			if stopReason == "" {
				stopReason = "incomplete_no_submission"
			}
		} else {
			completion, stopReason = "complete", "converged"
			if result.Assessment == review.AssessmentNoFindings {
				result.Assessment = review.AssessmentNoFindingsInSampledAreas
			}
		}
	}
	report := render.Report{
		Result: result, Mode: mode, FilesReviewed: sc.Files, Task: f.task,
		RunID: runID, Budgets: budgets,
		Quick: f.quick, Verified: doVerify,
		DiffTruncated: len(breakdown) > 0, OriginalDiffBytes: originalDiffBytes,
		SizeBreakdown: toRenderEntries(breakdown), Usage: usage,
		PromptPacks: langs,
		ReviewKind:  reviewKind, Completion: completion,
		ManifestFileCount: len(sc.Files), StopReason: stopReason,
	}
	if spent, limit := ledger.Committed(); limit > 0 {
		budgets["ledger_committed_tokens"] = spent
	}
	report.Prior = priorNotes(repo, result)
	writeReport(stdout, stderr, f, report)
	meta.Usage = usageMeta(usage)
	meta.Budgets = budgets
	finalErr := finalizeRunArtifacts(meta, report, repo)
	if finalErr == nil {
		runupload.MaybeAutoUpload(cfg, record.DefaultDir(), runID)
		if reportPath, pathErr := record.ReportPath(runID); pathErr == nil {
			logf(stderr, "run %s · report %s", runID, reportPath)
		}
	}
	return reviewExitError(finalErr, gateRank, result.MaxSeverity(), partial)
}

func reviewExitError(finalErr error, gateRank, maxSeverity int, partial bool) error {
	if finalErr != nil {
		return finalErr
	}
	if gateRank >= 0 && maxSeverity >= gateRank {
		return &gateExit{code: ExitFindings}
	}
	if partial {
		return &gateExit{code: ExitPartial}
	}
	return nil
}

func roleMeta(role config.ResolvedRole, systemPrompt string) record.RoleMeta {
	return record.RoleMeta{
		Profile: role.Profile, BaseURL: role.BaseURL, Model: role.Model, Protocol: role.Protocol,
		MaxOutputTokens: role.MaxOutputTokens, PromptDigest: record.Digest(systemPrompt),
	}
}

func captureSnapshot(repo, runID string, sc scope.Scope, indexTree string, pin snapshot.Pin) (record.SnapshotMeta, []string) {
	if !snapshot.Enabled() {
		return record.SnapshotMeta{Reason: "disabled by ROAST_SNAPSHOT"}, []string{"Git snapshot capture was disabled; this run cannot be replayed."}
	}
	head := sc.HeadOID
	if head == "" {
		head = snapshot.HeadOID(repo)
	}
	base := sc.BaseOID
	if base == "" && sc.Base != "" {
		base = snapshot.RevOID(repo, sc.Base)
	}
	snap, err := snapshot.Capture(repo, snapshot.Options{RunID: runID, HeadOID: head, BaseOID: base, IndexTree: indexTree, Worktree: pin})
	if err != nil {
		return record.SnapshotMeta{Reason: err.Error(), Head: head, Base: base}, []string{"Git snapshot capture failed: " + err.Error()}
	}
	diffPair := "base..worktree"
	if sc.Staged {
		diffPair = "head..index"
	} else if sc.Survey {
		diffPair = "head"
	} else if sc.Base == "" {
		diffPair = "head..worktree"
	}
	meta := record.SnapshotMeta{
		Captured: true, Ref: snap.Ref, Commit: snap.Commit,
		IndexTree: snap.IndexTree, WorktreeTree: snap.WorktreeTree,
		Head: snap.Head, Base: snap.Base, DiffPair: diffPair,
		FileModes:     "regular and executable files plus symlinks; empty directories are not represented",
		ExcludedPaths: append([]string(nil), snap.Excluded...),
	}
	baseTreeSource := base
	if diffPair == "head..index" || diffPair == "head..worktree" || diffPair == "head" {
		baseTreeSource = head
	}
	if baseTreeSource != "" {
		meta.BaseTree = snapshot.TreeOID(repo, baseTreeSource)
	}
	var caveats []string
	if len(snap.Excluded) > 0 {
		caveats = append(caveats, fmt.Sprintf("snapshot excluded %d denylisted untracked path(s)", len(snap.Excluded)))
	}
	return meta, caveats
}

func initializeRunArtifacts(meta record.Meta, input, finderSystem, finderUser, verifierSystem string) error {
	if _, err := record.WriteMetaRequired(meta); err != nil {
		return fmt.Errorf("initialize run record: %w", err)
	}
	for name, content := range map[string]string{
		"input": input, "finder_system": finderSystem,
		"finder_user": finderUser, "verifier_system": verifierSystem,
	} {
		if _, err := record.WriteArtifactRequired(meta.RunID, name, content); err != nil {
			return fmt.Errorf("initialize run artifact %s: %w", name, err)
		}
	}
	return nil
}

func finalizeRunArtifacts(meta record.Meta, report render.Report, repo string) error {
	resultJSON := render.ToJSON(report)
	if _, err := record.WriteArtifactRequired(meta.RunID, "result.json", resultJSON); err != nil {
		return fmt.Errorf("finalize run result: %w", err)
	}
	if _, err := record.WriteReportRequired(meta.RunID, render.ToMarkdown(report, true)); err != nil {
		return fmt.Errorf("finalize run report: %w", err)
	}
	if _, err := record.WriteMetaRequired(meta); err != nil {
		return fmt.Errorf("finalize run metadata: %w", err)
	}
	runDir, err := record.RunDir(meta.RunID)
	if err != nil {
		return fmt.Errorf("resolve run bundle directory: %w", err)
	}
	if _, _, _, err := runbundle.Build(runDir); err != nil {
		return fmt.Errorf("build local run bundle: %w", err)
	}
	record.Record(report, repo, meta.Argv)
	return nil
}

func usageMeta(usage map[string]llm.Usage) map[string]record.UsageMeta {
	out := make(map[string]record.UsageMeta, len(usage))
	for role, value := range usage {
		out[role] = record.UsageMeta{
			UncachedInputTokens: value.UncachedInputTokens,
			CachedInputTokens:   value.CachedInputTokens,
			CacheWriteTokens:    value.CacheWriteTokens,
			OutputTokens:        value.OutputTokens, Calls: value.Calls,
		}
	}
	return out
}

func priorNotes(repo string, result *review.Result) map[string]string {
	seen := record.SeenBefore(repo, result.Findings, 3)
	if len(seen) == 0 {
		return nil
	}
	out := make(map[string]string, len(seen))
	for fingerprint, sighting := range seen {
		note := "seen previously in run " + sighting.RunTS
		if sighting.DropReason != "" {
			note += " (dropped: " + sighting.DropReason + ")"
		}
		out[fingerprint] = note
	}
	return out
}

func resolveIntCeiling(changed func(string) bool, flag string, value int, envName string, defaultValue int) (int, error) {
	if changed(flag) {
		if value < 0 {
			return 0, fmt.Errorf("--%s cannot be negative", flag)
		}
		return value, nil
	}
	return envIntAtLeast(envName, defaultValue, 0)
}

func envIntAtLeast(name string, fallback, minimum int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum {
		return 0, fmt.Errorf("%s must be an integer >= %d (got %q)", name, minimum, raw)
	}
	return value, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	return parseNonNegativeDuration(name, raw)
}

func parseNonNegativeDuration(name, raw string) (time.Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration (got %q)", name, raw)
	}
	return value, nil
}

// applyRoleFlags applies command-line role overrides. Endpoint and protocol
// flags follow the same rules as their ROAST_<ROLE>_* variables, including
// which credential may follow the role to its new endpoint.
func applyRoleFlags(changed func(string) bool, name string, role config.ResolvedRole, flags roleFlags) config.ResolvedRole {
	var baseURL, protocol *string
	if changed(name + "-base-url") {
		baseURL = &flags.baseURL
	}
	if changed(name + "-protocol") {
		protocol = &flags.protocol
	}
	if baseURL != nil || protocol != nil {
		role = config.OverrideEndpoint(name, role, baseURL, protocol)
	}
	if changed(name + "-model") {
		role.Model = flags.model
	}
	if changed(name + "-max-output-tokens") {
		role.MaxOutputTokens = flags.maxOutputTokens
	}
	return role
}

func clientConfig(roleName string, role config.ResolvedRole, ledger *llm.Ledger, capture func(llm.RawResponse) error, maxAttempts int, requestTimeout time.Duration) llm.ClientConfig {
	cfg := llm.ClientConfig{
		Role:    roleName,
		BaseURL: strings.TrimRight(role.BaseURL, "/"), APIKey: role.APIKey,
		Model: role.Model, Protocol: role.Protocol, MaxOutputTokens: role.MaxOutputTokens,
		MaxAttempts: maxAttempts, RequestTimeout: requestTimeout,
		Ledger: ledger, CaptureResponse: capture,
	}
	if role.Auth == config.AuthChatGPT {
		cfg.ChatGPT = true
		cfg.APIKey = ""
		cfg.TokenSource = chatGPTTokenSource(role.Account)
	} else if role.Auth == config.AuthCodex {
		cfg.Codex = true
		cfg.APIKey = ""
		cfg.CodexTokenSource = codexTokenSource(role.Account)
	}
	return cfg
}

func resolveLangs(in, files []string, mode string) ([]string, error) {
	if mode == "prose" {
		return []string{}, nil
	}
	if len(in) == 0 {
		return review.ClassifyLangs(files), nil
	}
	for _, lang := range in {
		if lang == "none" {
			if len(in) != 1 {
				return nil, fmt.Errorf("--lang none cannot be combined with another language pack")
			}
			return []string{}, nil
		}
		switch lang {
		case "go", "react", "frontend", "web":
		default:
			return nil, fmt.Errorf("unknown language pack %q", lang)
		}
	}
	return append([]string(nil), in...), nil
}

func verifierPrompt(repo string, tree rcontext.Tree, finding review.Finding) string {
	return prompts.VerifierUser(prompts.VerifierVars{
		Severity: string(finding.Severity), Confidence: finding.Confidence,
		File: finding.File, Lines: lineRange(finding.Lines),
		Description: finding.Description, CodeQuote: finding.CodeQuote,
		FixedCode:   finding.FixedCode,
		FileSnippet: rcontext.NumberedSnippet(repo, finding.File, 60_000, tree),
	})
}

func lineRange(lines []int) string {
	switch len(lines) {
	case 0:
		return "?"
	case 1:
		return fmt.Sprintf("%d", lines[0])
	default:
		return fmt.Sprintf("%d-%d", lines[0], lines[len(lines)-1])
	}
}

func writeReport(stdout, stderr io.Writer, f *reviewFlags, report render.Report) {
	if f.format == "json" || f.format == "both" {
		fmt.Fprintln(stdout, render.ToJSON(report))
	}
	if f.format == "markdown" || f.format == "both" {
		fmt.Fprintln(stderr, render.ToMarkdown(report, f.showDropped))
	}
}

func toRenderEntries(in []scope.SizeEntry) []render.SizeEntry {
	out := make([]render.SizeEntry, len(in))
	for i, entry := range in {
		out[i] = render.SizeEntry{Path: entry.Path, Bytes: entry.Bytes}
	}
	return out
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "roast: %s\n", textsafe.Text(fmt.Sprintf(format, args...)))
}
