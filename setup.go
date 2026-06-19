package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

const (
	setupWakeTmux   = "tmux"
	setupWakeHerdr  = "herdr"
	setupWakeRunner = "runner"
	setupWakeBoth   = "both"

	setupPathBlockBegin = "# >>> agentchute setup PATH >>>"
	setupPathBlockEnd   = "# <<< agentchute setup PATH <<<"

	// install.sh writes its own PATH-managed region with a variable label/expr in
	// the marker: "# agentchute PATH entry for <label> (<expr>) begin" ... " end".
	// setup supersedes that region so the two installers never leave duplicate
	// PATH-prepend blocks (one from install.sh, one from setup) in a profile.
	installShPathMarkerPrefix = "# agentchute PATH entry for "
	installShPathMarkerBegin  = " begin"
	installShPathMarkerEnd    = " end"
)

type setupOptions struct {
	Wake        string
	Wrappers    string
	ControlRepo string
	ShimDir     string
	Profile     string
	Yes         bool
	DryRun      bool
	NoProfile   bool
	Aliases     bool
	InitNew     bool
}

type setupGlobalState struct {
	Version        int      `json:"version"`
	Wake           string   `json:"wake"`
	Wrappers       []string `json:"wrappers"`
	ShimDir        string   `json:"shim_dir,omitempty"`
	Profile        string   `json:"profile,omitempty"`
	NoProfile      bool     `json:"no_profile,omitempty"`
	PathBlock      bool     `json:"path_block"`
	ShimsInstalled bool     `json:"shims_installed"`
	Aliases        bool     `json:"aliases,omitempty"`
	UpdatedAt      string   `json:"updated_at"`
}

type setupPoolState struct {
	Version     int      `json:"version"`
	Wake        string   `json:"wake"`
	Wrappers    []string `json:"wrappers"`
	ControlRepo string   `json:"control_repo"`
	LoopDir     string   `json:"loop_dir"`
	Aliases     bool     `json:"aliases,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var opts setupOptions
	fs.StringVar(&opts.Wake, "wake", "", "wake paths to install: any comma-separated combination of runner, tmux, herdr (or all)")
	fs.StringVar(&opts.Wrappers, "wrappers", "all", "all (detected on PATH), none, or comma list: claude-code,codex,gemini-cli,grok")
	fs.StringVar(&opts.ControlRepo, "control-repo", "", "control repo path (default: env or current git/cwd root)")
	fs.StringVar(&opts.ShimDir, "shim-dir", "", "launcher shim directory (default: $HOME/.agentchute/bin)")
	fs.StringVar(&opts.Profile, "profile", "", "shell profile to update for launcher shims")
	fs.BoolVar(&opts.Yes, "yes", false, "skip confirmation prompts")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print plan without writing files")
	fs.BoolVar(&opts.NoProfile, "no-profile", false, "do not edit shell profile; print PATH hint instead")
	fs.BoolVar(&opts.Aliases, "aliases", false, "also install legacy same-name wrapper aliases")
	fs.BoolVar(&opts.InitNew, "init", false, "allow setup to initialize a non-project directory")
	if err := fs.Parse(args); err != nil {
		return setupUsage(err)
	}
	if fs.NArg() != 0 {
		return setupUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	root, err := resolveSetupRoot(opts.ControlRepo)
	if err != nil {
		return err
	}
	if opts.ShimDir == "" {
		opts.ShimDir, err = defaultSetupShimDir()
		if err != nil {
			return err
		}
	}
	opts.ShimDir, err = filepath.Abs(opts.ShimDir)
	if err != nil {
		return err
	}
	if strings.ContainsAny(opts.ShimDir, "\"`$\\") {
		return fmt.Errorf("invalid --shim-dir %q: must not contain quotes, dollar signs, backticks, or backslashes", opts.ShimDir)
	}
	if opts.Profile == "" {
		opts.Profile = strings.TrimSpace(os.Getenv("AGENTCHUTE_PROFILE"))
	}

	if strings.TrimSpace(opts.Wake) == "" {
		wake, err := promptSetupWake(opts.Yes)
		if err != nil {
			return err
		}
		opts.Wake = wake
	}
	canonWake, wakeDeprecation, err := normalizeSetupWake(opts.Wake)
	if err != nil {
		return err
	}
	opts.Wake = canonWake
	if wakeDeprecation != "" {
		fmt.Println(wakeDeprecation)
	}
	if err := guardSetupInitRoot(root, opts); err != nil {
		return err
	}

	wrappers, detected, err := resolveSetupWrappers(opts.Wrappers, opts.ShimDir)
	if err != nil {
		return err
	}
	sort.Strings(wrappers)

	printSetupPlan(os.Stdout, root, opts, wrappers, detected)
	if opts.DryRun {
		fmt.Println("\n(dry-run; no changes made)")
		return nil
	}
	if !opts.Yes {
		ok, err := promptSetupConfirm("\nApply setup? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil
		}
	}

	return applySetup(root, opts, wrappers)
}

func promptSetupConfirm(prompt string) (bool, error) {
	in, closeIn, err := setupPromptInput()
	if err != nil {
		return false, err
	}
	defer closeIn()
	return promptConfirm(in, os.Stdout, prompt)
}

func setupUsage(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return setupHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, setupHelp())
}

func setupHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, setupHelp())
}

func setupHelp() string {
	return strings.TrimSpace(`
Usage:
  agentchute setup [--wake runner[,tmux][,herdr] | all] [--wrappers all|none|<list>] [--yes] [--dry-run]

Scaffolds the control repo with agentchute init, stops local agentchute
pollers/runners, clears stale live registrations and repo Herdr names so agents
re-enroll, installs lifecycle hooks for the selected wrappers, and installs launcher
shims when runner is among the wake paths plus hookless wrappers when only tmux/herdr are selected.

Wake paths combine freely: pick any of runner, tmux, herdr (comma-separated) or
all. Each agent still wakes by a single method chosen at launch (runner shim,
tmux pane, or herdr pane); the selection here decides which infrastructure to
install for the pool.

Flags:
  --wake <set>           any comma-separated combination of runner, tmux, herdr,
                         or all (prompted when omitted). "both" is a deprecated
                         alias for all.
  --wrappers <set>       all (detected on PATH), none, or comma list
                         (claude-code,codex,gemini-cli,grok; default all)
  --control-repo <path>  repo to initialize (default env or current git/cwd root)
  --shim-dir <path>      launcher shim directory (default $HOME/.agentchute/bin)
  --profile <path>       shell profile to update for launcher shims
  --no-profile           do not edit shell profile; print PATH hint instead
  --aliases              also install legacy same-name wrapper aliases
  --init                 allow initializing a non-project directory
  --dry-run              print plan without writing files
  --yes                  skip confirmation prompts
`) + "\n"
}

func resolveSetupRoot(flagPath string) (string, error) {
	if strings.TrimSpace(flagPath) != "" {
		return filepath.Abs(flagPath)
	}
	if env := strings.TrimSpace(os.Getenv("AGENTCHUTE_CONTROL_REPO")); env != "" {
		return filepath.Abs(env)
	}
	root, _, err := resolveInitRoot()
	if err != nil {
		return "", err
	}
	return filepath.Abs(root)
}

func defaultSetupShimDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", fmt.Errorf("HOME unset; pass --shim-dir")
	}
	return filepath.Join(home, ".agentchute", "bin"), nil
}

// setupWakeMethods lists the real wake paths in canonical install order.
var setupWakeMethods = []string{setupWakeRunner, setupWakeTmux, setupWakeHerdr}

func validSetupWakeMethod(m string) bool {
	return m == setupWakeRunner || m == setupWakeTmux || m == setupWakeHerdr
}

// wakeSetContains reports whether the canonical comma-joined wake set includes
// method.
func wakeSetContains(wake, method string) bool {
	for _, m := range strings.Split(wake, ",") {
		if strings.TrimSpace(m) == method {
			return true
		}
	}
	return false
}

// normalizeSetupWake parses a raw --wake value — a comma-separated combination
// of runner/tmux/herdr, the keyword "all", or the deprecated "both" — into a
// canonical comma-joined set in install order. It returns the canonical value
// and an optional deprecation note to surface to the user.
func normalizeSetupWake(raw string) (string, string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	invalid := func(tok string) error {
		return fmt.Errorf("--wake %q is not recognized; choose any comma-separated combination of runner, tmux, herdr (or all)", tok)
	}
	if raw == "" {
		return "", "", invalid("")
	}
	var deprecation string
	selected := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		tok := strings.TrimSpace(part)
		switch tok {
		case "":
			// Reject empty tokens (stray/leading/trailing commas) for CLI typo
			// safety rather than silently dropping them.
			return "", "", invalid(raw)
		case "all":
			for _, m := range setupWakeMethods {
				selected[m] = true
			}
		case setupWakeBoth:
			for _, m := range setupWakeMethods {
				selected[m] = true
			}
			deprecation = "note: --wake both is deprecated; use --wake all (installing runner + tmux + herdr infrastructure)"
		default:
			if !validSetupWakeMethod(tok) {
				return "", "", invalid(tok)
			}
			selected[tok] = true
		}
	}
	var ordered []string
	for _, m := range setupWakeMethods {
		if selected[m] {
			ordered = append(ordered, m)
		}
	}
	if len(ordered) == 0 {
		return "", "", invalid(raw)
	}
	return strings.Join(ordered, ","), deprecation, nil
}

func guardSetupInitRoot(root string, opts setupOptions) error {
	if setupRootHasSpec(root) || opts.InitNew || strings.TrimSpace(opts.ControlRepo) != "" || strings.TrimSpace(os.Getenv("AGENTCHUTE_CONTROL_REPO")) != "" {
		return nil
	}
	if setupRootLooksProject(root) {
		return nil
	}
	if opts.Yes {
		return fmt.Errorf("refusing to initialize non-project directory %s with --yes; run from a project/control repo, pass --control-repo, or pass --init to opt in", root)
	}
	ok, err := promptSetupConfirm(fmt.Sprintf("\n%s has no AGENTCHUTE.md and does not look like a project root. Initialize here? [y/N]: ", root))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("aborted; run setup from a project/control repo or pass --init to opt in")
	}
	return nil
}

func setupRootHasSpec(root string) bool {
	info, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md"))
	return err == nil && !info.IsDir()
}

func setupRootLooksProject(root string) bool {
	for _, marker := range []string{
		".git",
		".hg",
		".svn",
		"go.mod",
		"package.json",
		"pyproject.toml",
		"Cargo.toml",
		"pom.xml",
		"Gemfile",
		"mix.exs",
		"WORKSPACE",
		"deno.json",
	} {
		if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
			return true
		}
	}
	return false
}

func promptSetupWake(yes bool) (string, error) {
	if yes {
		return "", fmt.Errorf("--wake is required with --yes")
	}
	in, closeIn, err := setupPromptInput()
	if err != nil {
		return "", fmt.Errorf("--wake is required when no terminal is available")
	}
	defer closeIn()
	fmt.Fprintln(os.Stdout, "Wake paths to install (each agent wakes by how it is launched):")
	fmt.Fprintln(os.Stdout, "  runner  PTY launcher + local socket; works anywhere, incl. plain shells")
	fmt.Fprintln(os.Stdout, "  tmux    peers send-keys into your tmux pane")
	fmt.Fprintln(os.Stdout, "  herdr   peers send-keys into your herdr pane")
	fmt.Fprint(os.Stdout, "Choose any combination (comma-separated) or 'all' [runner]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return setupWakeRunner, nil
	}
	return answer, nil
}

func setupPromptInput() (io.Reader, func(), error) {
	if stat, err := os.Stdin.Stat(); err == nil && stat.Mode()&os.ModeCharDevice != 0 {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open("/dev/tty")
	if err != nil {
		return nil, func() {}, err
	}
	return f, func() { _ = f.Close() }, nil
}

// setupWrapper describes a wrapper that `agentchute setup` knows about. The
// set is broader than hookWrappers: a wrapper is enrollable through the runner
// shim (which routes any vendor through `agentchute run`) even when its CLI has
// no repo hook system. Hookable wrappers additionally have a hookWrappers entry
// whose lifecycle hooks setup installs; hookless wrappers (e.g. grok, whose CLI
// exposes no settings.json/hooks.json) rely on the shim wake path alone.
type setupWrapper struct {
	Name       string
	Candidates []string
	Hookable   bool
}

var setupWrappers = []setupWrapper{
	{Name: "claude-code", Candidates: []string{"claude", "claude-code"}, Hookable: true},
	{Name: "codex", Candidates: []string{"codex"}, Hookable: true},
	{Name: "gemini-cli", Candidates: []string{"gemini", "gemini-cli"}, Hookable: true},
	{Name: "grok", Candidates: []string{"grok"}, Hookable: false},
}

func wrapperIsKnownForSetup(name string) bool {
	for _, w := range setupWrappers {
		if w.Name == name {
			return true
		}
	}
	return false
}

func setupWrapperNames() []string {
	names := make([]string, 0, len(setupWrappers))
	for _, w := range setupWrappers {
		names = append(names, w.Name)
	}
	return names
}

func resolveSetupWrappers(raw, shimDir string) ([]string, map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return detectSetupWrappers(shimDir), detectSetupWrapperPaths(shimDir), nil
	}
	if raw == "none" {
		return nil, detectSetupWrapperPaths(shimDir), nil
	}
	seen := map[string]bool{}
	var wrappers []string
	for _, part := range strings.Split(raw, ",") {
		key := strings.TrimSpace(part)
		if key == "" {
			continue
		}
		if !wrapperIsKnownForSetup(key) {
			return nil, nil, fmt.Errorf("--wrappers %q is not recognized; known: %s, all, none", key, strings.Join(setupWrapperNames(), ", "))
		}
		if !seen[key] {
			wrappers = append(wrappers, key)
			seen[key] = true
		}
	}
	if len(wrappers) == 0 {
		return nil, nil, fmt.Errorf("--wrappers must not be empty")
	}
	return wrappers, detectSetupWrapperPaths(shimDir), nil
}

func detectSetupWrappers(shimDir string) []string {
	paths := detectSetupWrapperPaths(shimDir)
	var wrappers []string
	for _, w := range setupWrappers {
		if paths[w.Name] != "" {
			wrappers = append(wrappers, w.Name)
		}
	}
	return wrappers
}

func detectSetupWrapperPaths(shimDir string) map[string]string {
	out := map[string]string{}
	for _, w := range setupWrappers {
		for _, candidate := range w.Candidates {
			path := findExecutableOutsideDir(candidate, shimDir)
			if path != "" {
				out[w.Name] = path
				break
			}
		}
	}
	return out
}

func findExecutableOutsideDir(name, skipDir string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if skipDir != "" && samePath(absDir, skipDir) {
			continue
		}
		path := filepath.Join(absDir, name)
		if executableFileProblem(path) == "" {
			return path
		}
	}
	return ""
}

func printSetupPlan(w io.Writer, root string, opts setupOptions, wrappers []string, detected map[string]string) {
	shimWrappers := setupShimWrappers(opts.Wake, wrappers)
	hookWrappers := setupHookableWrappers(wrappers)

	fmt.Fprintln(w, "agentchute setup")
	fmt.Fprintf(w, "  control repo: %s\n", root)
	fmt.Fprintf(w, "  wake:         %s\n", opts.Wake)
	if len(wrappers) == 0 {
		fmt.Fprintln(w, "  wrappers:     none")
	} else {
		fmt.Fprintf(w, "  wrappers:     %s\n", strings.Join(wrappers, ", "))
	}
	fmt.Fprintf(w, "  init:         %s\n", filepath.Join(root, "AGENTCHUTE.md"))
	fmt.Fprintln(w, "  reset:        stop local agentchute pollers/runners, clear live agents/*.md, release repo Herdr names")
	if len(hookWrappers) > 0 {
		fmt.Fprintln(w, "  hooks:        repo scope, force/idempotent")
	}
	if len(shimWrappers) > 0 {
		fmt.Fprintf(w, "  shims:        %s\n", opts.ShimDir)
		if opts.Aliases {
			fmt.Fprintln(w, "  aliases:      legacy same-name wrapper aliases")
		}
		if !setupNeedsShims(opts.Wake) {
			fmt.Fprintf(w, "  shim wrappers: %s (hookless startup enrollment)\n", strings.Join(shimWrappers, ", "))
		}
		if opts.NoProfile {
			fmt.Fprintln(w, "  profile:      skipped (--no-profile)")
		} else if profiles := setupPlausibleProfiles(opts.Profile); len(profiles) > 0 {
			fmt.Fprintf(w, "  profile:      %s\n", strings.Join(profiles, ", "))
		} else {
			fmt.Fprintln(w, "  profile:      skipped (no supported shell profile detected)")
		}
	}
	if opts.Wrappers == "all" {
		for _, wrapper := range setupWrappers {
			if detected[wrapper.Name] == "" {
				fmt.Fprintf(w, "  detected:     %s not found on PATH; skipped\n", wrapper.Name)
			} else {
				fmt.Fprintf(w, "  detected:     %s -> %s\n", wrapper.Name, detected[wrapper.Name])
			}
		}
	}
	if wakeSetContains(opts.Wake, setupWakeTmux) && os.Getenv("TMUX") == "" {
		fmt.Fprintln(w, "  warning:      TMUX is not set; start wrappers inside tmux for tmux wake")
	}
	if wakeSetContains(opts.Wake, setupWakeHerdr) && os.Getenv("HERDR_ENV") == "" {
		fmt.Fprintln(w, "  warning:      HERDR_ENV is not set; start wrappers inside herdr for herdr wake")
	}
}

func setupNeedsShims(wake string) bool {
	return wakeSetContains(wake, setupWakeRunner)
}

func setupShimWrappers(wake string, wrappers []string) []string {
	if setupNeedsShims(wake) {
		// INVARIANT: Install all known wrappers when runner is among the wake
		// paths so that a later install of a real binary works immediately.
		return setupWrapperNames()
	}
	if wakeSetContains(wake, setupWakeTmux) || wakeSetContains(wake, setupWakeHerdr) {
		// INVARIANT: In tmux/herdr-only mode, we must install shims for hookless
		// wrappers (grok) so they can enroll on startup via the shim.
		return compactSetupWrappers(wrappers, func(w setupWrapper) bool { return !w.Hookable })
	}
	return nil
}

// printSetupCompletionGuidance prints how to restart wrappers for the selected
// wake paths. A single path gets a one-line instruction; a combination lists
// the launch style per path.
func printSetupCompletionGuidance(w io.Writer, wake string) {
	if !strings.Contains(wake, ",") {
		switch wake {
		case setupWakeTmux:
			fmt.Fprintln(w, "Restart selected wrappers from this repo inside tmux, then run `agentchute doctor --as <id>`.")
		case setupWakeHerdr:
			fmt.Fprintln(w, "Restart selected wrappers from this repo inside herdr, then run `agentchute doctor --as <id>`.")
		default: // runner
			fmt.Fprintln(w, "Open one new shell, restart selected wrappers from this repo, then run `agentchute doctor --as <id>`.")
		}
		return
	}
	fmt.Fprintln(w, "Restart wrappers per the path they use:")
	if wakeSetContains(wake, setupWakeRunner) {
		fmt.Fprintln(w, "  runner: open one new shell, then launch wrappers from this repo via the ac-* launcher.")
	}
	if wakeSetContains(wake, setupWakeTmux) {
		fmt.Fprintln(w, "  tmux:   launch wrappers from this repo inside tmux.")
	}
	if wakeSetContains(wake, setupWakeHerdr) {
		fmt.Fprintln(w, "  herdr:  launch wrappers from this repo inside herdr.")
	}
	fmt.Fprintln(w, "Then run `agentchute doctor --as <id>`.")
}

func setupHookableWrappers(wrappers []string) []string {
	return compactSetupWrappers(wrappers, func(w setupWrapper) bool { return w.Hookable })
}

func compactSetupWrappers(wrappers []string, keep func(setupWrapper) bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range wrappers {
		w, ok := setupWrapperByName(name)
		if !ok || !keep(w) || seen[name] {
			continue
		}
		out = append(out, name)
		seen[name] = true
	}
	return out
}

func setupWrapperByName(name string) (setupWrapper, bool) {
	for _, w := range setupWrappers {
		if w.Name == name {
			return w, true
		}
	}
	return setupWrapper{}, false
}

func previousSetupShimWrappers(state setupGlobalState) []string {
	wrappers := setupShimWrappers(state.Wake, state.Wrappers)
	if len(wrappers) == 0 && state.ShimsInstalled {
		return state.Wrappers
	}
	return wrappers
}

// setupRunRuntimeReset is the DESTRUCTIVE phase of setup: it stops local
// pollers/runners, clears runtime state files and repo Herdr names, then deletes
// live registrations so agents re-enroll. It is a package var so tests can inject
// a failure to prove the ordering invariant below. It is invoked LAST in
// applySetup — after every idempotent, recoverable write (init/enrollment, hooks,
// shims, PATH block, saved setup state) has landed — so a mid-setup failure can
// never leave the bus with cleared registrations AND no wake infrastructure.
var setupRunRuntimeReset = func(root string, cfg *loop.Config, wrappers []string) error {
	reset := resetSetupRuntimeState(root, cfg, wrappers)
	if len(reset.Pollers) > 0 {
		fmt.Printf("stopped %d local poller(s): %s\n", len(reset.Pollers), strings.Join(reset.Pollers, ", "))
	}
	if len(reset.Runners) > 0 {
		fmt.Printf("stopped %d local runner(s): %s\n", len(reset.Runners), strings.Join(reset.Runners, ", "))
	}
	if len(reset.RuntimeFiles) > 0 {
		fmt.Printf("cleared %d runtime state file(s)\n", len(reset.RuntimeFiles))
	}
	if len(reset.HerdrNames) > 0 {
		fmt.Printf("released %d herdr name(s): %s\n", len(reset.HerdrNames), strings.Join(reset.HerdrNames, ", "))
	}
	for _, warning := range reset.Warnings {
		fmt.Printf("warning: setup reset: %s\n", warning)
	}
	cleared, err := clearSetupLiveRegistrations(cfg)
	if err != nil {
		return err
	}
	if len(cleared) > 0 {
		fmt.Printf("cleared %d stale live registration(s): %s\n", len(cleared), strings.Join(cleared, ", "))
	}
	return nil
}

func applySetup(root string, opts setupOptions, wrappers []string) error {
	return runInDir(root, func() error {
		// Phase 1 — idempotent scaffolding. cmdInit writes AGENTCHUTE.md and the
		// per-wrapper enrollment blocks (CLAUDE.md/CODEX.md/GEMINI.md/AGENTS.md).
		// Re-runnable; safe to repeat after a partial failure.
		if err := cmdInit([]string{"--yes"}); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		cfg, err := loop.Discover(loop.DiscoverOpts{
			ControlRepoFlag: root,
			Cwd:             root,
			EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
		})
		if err != nil {
			return fmt.Errorf("discover initialized repo: %w", err)
		}
		// Capture the prior saved state BEFORE we overwrite it below: the
		// dropped-wrapper hook/shim cleanup is computed against the previous
		// install's wrapper/shim sets.
		globalState, _ := readSetupGlobalState()
		poolState, _ := readSetupPoolState(cfg)

		// Phase 2 — idempotent wake-infrastructure writes (hooks, shims, PATH
		// block, saved setup state). These run BEFORE the destructive reset so a
		// failure here leaves prior wake infrastructure intact and a re-run
		// recovers cleanly. The reset does not depend on any of these writes.
		for _, wrapper := range droppedWrappers(poolState.Wrappers, wrappers) {
			if err := removeSetupHook(wrapper, root); err != nil {
				return err
			}
		}
		for _, wrapper := range wrappers {
			hook, ok := hookWrapperByName(wrapper)
			if !ok {
				// Known but hookless wrapper (e.g. grok: its CLI has no repo
				// hook system). It is enrolled through the runner shim only;
				// there is no lifecycle hook to install.
				if wrapperIsKnownForSetup(wrapper) {
					continue
				}
				return fmt.Errorf("hooks install %s: unknown wrapper", wrapper)
			}
			if err := installOneHook(hook, root, false, true); err != nil {
				return fmt.Errorf("hooks install %s: %w", wrapper, err)
			}
		}

		currentShimWrappers := setupShimWrappers(opts.Wake, wrappers)
		currentNeedsShims := len(currentShimWrappers) > 0
		for _, wrapper := range droppedWrappers(previousSetupShimWrappers(globalState), currentShimWrappers) {
			if err := removeSetupShimsForWrapper(globalState.ShimDir, wrapper); err != nil {
				return err
			}
		}
		if currentNeedsShims {
			shimArgs := []string{
				"install",
				"--dir", opts.ShimDir,
				"--wrapper", strings.Join(currentShimWrappers, ","),
				"--force",
			}
			if opts.Aliases {
				shimArgs = append(shimArgs, "--aliases")
			}
			if err := cmdShims(shimArgs); err != nil {
				return fmt.Errorf("shims install: %w", err)
			}
			if !opts.Aliases {
				for _, wrapper := range currentShimWrappers {
					if err := removeSetupAliasShimsForWrapper(opts.ShimDir, wrapper); err != nil {
						return err
					}
				}
			}
			if err := setupEnsureShimPath(opts); err != nil {
				return err
			}
		}
		if !currentNeedsShims {
			if globalState.ShimsInstalled {
				if err := removeSetupShims(globalState.ShimDir); err != nil {
					return err
				}
			}
			// Precedence as INVARIANT: when the selected mode needs no shims,
			// remove the PATH block from all plausible profiles.
			targets := setupPlausibleProfiles(opts.Profile)
			if globalState.Profile != "" {
				found := false
				for _, t := range targets {
					if t == globalState.Profile {
						found = true
						break
					}
				}
				if !found {
					targets = append(targets, globalState.Profile)
				}
			}
			for _, p := range targets {
				if err := setupRemovePathBlock(p); err != nil {
					return err
				}
			}
		}

		if err := writeSetupPoolState(cfg, opts.Wake, wrappers, currentNeedsShims && opts.Aliases); err != nil {
			return err
		}
		profiles := setupPlausibleProfiles(opts.Profile)
		pathBlock := currentNeedsShims && !opts.NoProfile && len(profiles) > 0
		if pathBlock {
			// If any target profile is missing the block, we record that shims
			// need a path block. This is a bit conservative.
			anyMissing := false
			for _, p := range profiles {
				if !setupProfileHasBlock(p) || !pathContains(opts.ShimDir, os.Getenv("PATH")) {
					anyMissing = true
					break
				}
			}
			pathBlock = anyMissing
		}

		if err := writeSetupGlobalState(setupGlobalState{
			Version:        1,
			Wake:           opts.Wake,
			Wrappers:       wrappers,
			ShimDir:        opts.ShimDir,
			Profile:        opts.Profile,
			NoProfile:      opts.NoProfile,
			ShimsInstalled: currentNeedsShims,
			Aliases:        currentNeedsShims && opts.Aliases,
			PathBlock:      pathBlock,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}

		// Phase 3 — destructive runtime reset, LAST. By the time we reach here
		// every recoverable write above has landed, so even if the reset (or a
		// crash during it) fails partway, the bus retains its wake infrastructure
		// (hooks/shims/enrollment) and a `setup` re-run recovers cleanly.
		if err := setupRunRuntimeReset(root, cfg, wrappers); err != nil {
			return err
		}

		fmt.Println("\nsetup complete.")
		printSetupCompletionGuidance(os.Stdout, opts.Wake)
		return nil
	})
}

func clearSetupLiveRegistrations(cfg *loop.Config) ([]string, error) {
	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read live registrations: %w", err)
	}

	var cleared []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") || name == "README.md" {
			continue
		}
		path := filepath.Join(cfg.AgentsDir(), name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect live registration %s: %w", name, err)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove live registration %s: %w", name, err)
		}
		cleared = append(cleared, name)
	}
	sort.Strings(cleared)
	return cleared, nil
}

func hookWrapperByName(name string) (hookWrapper, bool) {
	for _, wrapper := range hookWrappers {
		if wrapper.Name == name {
			return wrapper, true
		}
	}
	return hookWrapper{}, false
}

func droppedWrappers(previous, current []string) []string {
	currentSet := map[string]bool{}
	for _, wrapper := range current {
		currentSet[wrapper] = true
	}
	var dropped []string
	seen := map[string]bool{}
	for _, wrapper := range previous {
		if wrapper == "" || currentSet[wrapper] || seen[wrapper] {
			continue
		}
		dropped = append(dropped, wrapper)
		seen[wrapper] = true
	}
	sort.Strings(dropped)
	return dropped
}

func removeSetupHook(wrapperName, root string) error {
	wrapper, ok := hookWrapperByName(wrapperName)
	if !ok {
		return nil
	}
	src, err := hooksFS.ReadFile(wrapper.Src)
	if err != nil {
		return fmt.Errorf("read hook template for %s: %w", wrapperName, err)
	}
	dest := filepath.Join(root, wrapper.Dest)
	existing, err := os.ReadFile(dest)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read hook %s: %w", dest, err)
	}
	if string(existing) != string(src) {
		fmt.Printf("warning: not removing %s hook at %s; file differs from setup template\n", wrapperName, dest)
		return nil
	}
	if err := os.Remove(dest); err != nil {
		return fmt.Errorf("remove hook %s: %w", dest, err)
	}
	fmt.Printf("removed setup-managed %s hook at %s\n", wrapperName, dest)
	return nil
}

func runInDir(dir string, fn func() error) error {
	old, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(old) }()
	return fn()
}

func setupEnsureShimPath(opts setupOptions) error {
	pathEnv := os.Getenv("PATH")

	if !pathContains(opts.ShimDir, pathEnv) {
		fmt.Printf("warning: add %s to PATH\n", opts.ShimDir)
	}

	if opts.NoProfile {
		return nil
	}

	profiles := setupPlausibleProfiles(opts.Profile)
	if len(profiles) == 0 {
		return nil
	}

	for _, profile := range profiles {
		if err := setupWritePathBlock(profile, opts.ShimDir); err != nil {
			return err
		}
	}
	return nil
}

func setupPlausibleProfiles(override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{override}
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return nil
	}
	shell := os.Getenv("SHELL")
	var profiles []string
	switch {
	case strings.HasSuffix(shell, "zsh"):
		profiles = []string{filepath.Join(home, ".zshrc")}
	case strings.HasSuffix(shell, "bash"):
		if runtime.GOOS == "darwin" {
			profiles = []string{filepath.Join(home, ".bash_profile"), filepath.Join(home, ".bashrc")}
		} else {
			profiles = []string{filepath.Join(home, ".bashrc")}
		}
	case strings.HasSuffix(shell, "fish"):
		profiles = []string{filepath.Join(home, ".config", "fish", "config.fish")}
	case strings.HasSuffix(shell, "sh"):
		profiles = []string{filepath.Join(home, ".profile")}
	}
	// Always include .profile as a generic fallback for Bourne-family shells
	// (except fish which is incompatible).
	if !strings.HasSuffix(shell, "fish") {
		profile := filepath.Join(home, ".profile")
		found := false
		for _, p := range profiles {
			if p == profile {
				found = true
				break
			}
		}
		if !found {
			profiles = append(profiles, profile)
		}
	}
	return profiles
}

func setupWritePathBlock(profile, dir string) error {
	existing, err := os.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read profile %s: %w", profile, err)
	}
	block := setupRenderPathBlock(profile, dir)
	next := replaceSetupBlock(string(existing), block)
	if string(existing) == next {
		fmt.Printf("PATH profile block already current in %s\n", profile)
		return nil
	}
	if len(existing) > 0 {
		backup := setupBackupPath(profile)
		if err := os.WriteFile(backup, existing, 0o600); err != nil {
			return fmt.Errorf("write profile backup %s: %w", backup, err)
		}
		fmt.Printf("profile backup written to %s\n", backup)
	}
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		return fmt.Errorf("mkdir profile dir: %w", err)
	}
	if err := os.WriteFile(profile, []byte(next), 0o600); err != nil {
		return fmt.Errorf("write profile %s: %w", profile, err)
	}
	fmt.Printf("updated PATH profile block in %s\n", profile)
	return nil
}

func setupRemovePathBlock(profile string) error {
	if strings.TrimSpace(profile) == "" {
		return nil
	}
	existing, err := os.ReadFile(profile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read profile %s: %w", profile, err)
	}
	next, changed := removeSetupBlock(string(existing))
	if !changed {
		return nil
	}
	backup := setupBackupPath(profile)
	if err := os.WriteFile(backup, existing, 0o600); err != nil {
		return fmt.Errorf("write profile backup %s: %w", backup, err)
	}
	if err := os.WriteFile(profile, []byte(next), 0o600); err != nil {
		return fmt.Errorf("write profile %s: %w", profile, err)
	}
	fmt.Printf("removed PATH profile block from %s (backup: %s)\n", profile, backup)
	return nil
}

func setupProfileHasBlock(profile string) bool {
	data, err := os.ReadFile(profile)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), setupPathBlockBegin) &&
		strings.Contains(string(data), setupPathBlockEnd)
}

func setupRenderPathBlock(profile, dir string) string {
	expr := setupPathExpr(dir)
	if strings.HasSuffix(profile, "config.fish") {
		return fmt.Sprintf("%s\nif test \"$PATH[1]\" != %s\n    set -gx PATH %s $PATH\nend\n%s\n",
			setupPathBlockBegin, expr, expr, setupPathBlockEnd)
	}
	return fmt.Sprintf("%s\ncase \"$PATH\" in\n  \"%s:\"*) ;;\n  *) export PATH=\"%s:$PATH\" ;;\nesac\n%s\n",
		setupPathBlockBegin, expr, expr, setupPathBlockEnd)
}

func setupPathExpr(dir string) string {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home != "" && strings.HasPrefix(dir, home+string(os.PathSeparator)) {
		return "$HOME/" + filepath.ToSlash(strings.TrimPrefix(dir, home+string(os.PathSeparator)))
	}
	return dir
}

func replaceSetupBlock(existing, block string) string {
	without, changed := removeSetupBlock(existing)
	if strings.TrimSpace(without) == "" {
		return block
	}
	if !strings.HasSuffix(without, "\n") {
		without += "\n"
	}
	if changed {
		return without + block
	}
	return without + "\n" + block
}

func removeSetupBlock(existing string) (string, bool) {
	// First retire any install.sh-managed PATH region(s) so setup supersedes them
	// instead of stacking a second managed block in the same profile.
	out, legacyRemoved := removeInstallShPathBlocks(existing)

	start := strings.Index(out, setupPathBlockBegin)
	if start < 0 {
		return out, legacyRemoved
	}
	end := strings.Index(out[start:], setupPathBlockEnd)
	if end < 0 {
		return out, legacyRemoved
	}
	end += start + len(setupPathBlockEnd)
	if end < len(out) && out[end] == '\n' {
		end++
	}
	next := strings.TrimRight(out[:start], "\n")
	if next != "" && strings.TrimSpace(out[end:]) != "" {
		next += "\n\n"
	}
	next += strings.TrimLeft(out[end:], "\n")
	return next, true
}

// removeInstallShPathBlocks strips every install.sh-managed PATH region from the
// profile text. install.sh delimits its region with marker lines of the form
// "# agentchute PATH entry for <label> (<expr>) begin" ... " end" where the
// label/expr vary, so we match on the stable prefix and begin/end suffixes.
func removeInstallShPathBlocks(existing string) (string, bool) {
	changed := false
	out := existing
	for {
		bStart, bEnd := installShMarkerLine(out, installShPathMarkerBegin)
		if bStart < 0 {
			break
		}
		// Find the matching end marker after the begin marker.
		eStart, eEnd := installShMarkerLine(out[bEnd:], installShPathMarkerEnd)
		if eStart < 0 {
			break
		}
		eStart += bEnd
		eEnd += bEnd
		if eEnd < len(out) && out[eEnd] == '\n' {
			eEnd++
		}
		prefix := strings.TrimRight(out[:bStart], "\n")
		suffix := strings.TrimLeft(out[eEnd:], "\n")
		next := prefix
		if next != "" && strings.TrimSpace(suffix) != "" {
			next += "\n\n"
		}
		next += suffix
		out = next
		changed = true
	}
	return out, changed
}

// installShMarkerLine returns the start and end byte offsets of the first whole
// line in s that begins with the install.sh marker prefix and ends with the
// given marker suffix (begin/end). Returns (-1, -1) when no such line exists.
func installShMarkerLine(s, suffix string) (int, int) {
	idx := 0
	for idx < len(s) {
		lineEnd := strings.IndexByte(s[idx:], '\n')
		var line string
		var absEnd int
		if lineEnd < 0 {
			line = s[idx:]
			absEnd = len(s)
		} else {
			line = s[idx : idx+lineEnd]
			absEnd = idx + lineEnd
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, installShPathMarkerPrefix) && strings.HasSuffix(trimmed, suffix) {
			return idx, absEnd
		}
		if lineEnd < 0 {
			break
		}
		idx = absEnd + 1
	}
	return -1, -1
}

func setupBackupPath(path string) string {
	return fmt.Sprintf("%s.agentchute-backup-%s", path, time.Now().UTC().Format("20060102T150405Z"))
}

func removeSetupShims(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	for _, name := range allShimCommandNames(true) {
		path := filepath.Join(dir, name)
		if err := removeAgentchuteShim(path); err != nil {
			return err
		}
	}
	fmt.Printf("removed agentchute shims from %s\n", dir)
	return nil
}

func removeSetupShimsForWrapper(dir, wrapper string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	specs, err := selectShimSpecs(wrapper)
	if err != nil {
		return nil
	}
	for _, spec := range specs {
		for _, name := range shimInstallNames(spec, true) {
			if err := removeAgentchuteShim(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	fmt.Printf("removed setup-managed %s shims from %s\n", wrapper, dir)
	return nil
}

func removeSetupAliasShimsForWrapper(dir, wrapper string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	specs, err := selectShimSpecs(wrapper)
	if err != nil {
		return nil
	}
	for _, spec := range specs {
		for _, alias := range spec.Aliases {
			if err := removeAgentchuteShim(filepath.Join(dir, alias)); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeAgentchuteShim(path string) error {
	owned, err := isAgentchuteShim(path)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove shim %s: %w", path, err)
	}
	return nil
}

func isAgentchuteShim(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read shim %s: %w", path, err)
	}
	text := string(data)
	return strings.Contains(text, "shims exec --name") && strings.Contains(text, "AGENTCHUTE_BIN="), nil
}

func setupGlobalStatePath() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if home == "" {
			return "", fmt.Errorf("HOME unset")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "agentchute", "setup.json"), nil
}

// canonicalizePersistedWake best-effort normalizes a wake value read back from
// saved setup state so legacy/non-canonical values map to the same set the
// set-aware predicates expect. Critically, pre-combination installs persisted
// `Wake: "both"`; without this, setupShimWrappers("both", …) returns nil and
// previousSetupShimWrappers mis-computes the prior shim set, orphaning shims on
// a narrowing re-setup. Invalid or empty values are left untouched (callers that
// require a wake mode validate separately).
func canonicalizePersistedWake(wake string) string {
	if canon, _, err := normalizeSetupWake(wake); err == nil {
		return canon
	}
	return wake
}

func readSetupGlobalState() (setupGlobalState, error) {
	path, err := setupGlobalStatePath()
	if err != nil {
		return setupGlobalState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return setupGlobalState{}, err
	}
	var state setupGlobalState
	if err := json.Unmarshal(data, &state); err != nil {
		return setupGlobalState{}, err
	}
	state.Wake = canonicalizePersistedWake(state.Wake)
	return state, nil
}

func readSetupPoolState(cfg *loop.Config) (setupPoolState, error) {
	data, err := os.ReadFile(filepath.Join(cfg.LoopDir, "state", "setup.json"))
	if err != nil {
		return setupPoolState{}, err
	}
	var state setupPoolState
	if err := json.Unmarshal(data, &state); err != nil {
		return setupPoolState{}, err
	}
	state.Wake = canonicalizePersistedWake(state.Wake)
	return state, nil
}

func writeSetupGlobalState(state setupGlobalState) error {
	path, err := setupGlobalStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir setup state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write setup state: %w", err)
	}
	return nil
}

func writeSetupPoolState(cfg *loop.Config, wake string, wrappers []string, aliases bool) error {
	stateDir := filepath.Join(cfg.LoopDir, "state")
	if err := loop.EnsurePrivateDir(stateDir); err != nil {
		return err
	}
	state := setupPoolState{
		Version:     1,
		Wake:        wake,
		Wrappers:    wrappers,
		ControlRepo: cfg.ControlRepo,
		LoopDir:     cfg.LoopDir,
		Aliases:     aliases,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(stateDir, "setup.json"), data, 0o600); err != nil {
		return fmt.Errorf("write pool setup state: %w", err)
	}
	return nil
}
