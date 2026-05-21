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
	setupWakeRunner = "runner"
	setupWakeBoth   = "both"

	setupPathBlockBegin = "# >>> agentchute setup PATH >>>"
	setupPathBlockEnd   = "# <<< agentchute setup PATH <<<"
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
	InitNew     bool
}

type setupGlobalState struct {
	Version        int      `json:"version"`
	Wake           string   `json:"wake"`
	Wrappers       []string `json:"wrappers"`
	ShimDir        string   `json:"shim_dir,omitempty"`
	Profile        string   `json:"profile,omitempty"`
	PathBlock      bool     `json:"path_block"`
	ShimsInstalled bool     `json:"shims_installed"`
	UpdatedAt      string   `json:"updated_at"`
}

type setupPoolState struct {
	Version     int      `json:"version"`
	Wake        string   `json:"wake"`
	Wrappers    []string `json:"wrappers"`
	ControlRepo string   `json:"control_repo"`
	LoopDir     string   `json:"loop_dir"`
	UpdatedAt   string   `json:"updated_at"`
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var opts setupOptions
	fs.StringVar(&opts.Wake, "wake", "", "primary wake path: tmux | runner | both")
	fs.StringVar(&opts.Wrappers, "wrappers", "all", "all (detected on PATH), none, or comma list: claude-code,codex,gemini-cli")
	fs.StringVar(&opts.ControlRepo, "control-repo", "", "control repo path (default: env or current git/cwd root)")
	fs.StringVar(&opts.ShimDir, "shim-dir", "", "launcher shim directory (default: $HOME/.agentchute/bin)")
	fs.StringVar(&opts.Profile, "profile", "", "shell profile to update for launcher shims")
	fs.BoolVar(&opts.Yes, "yes", false, "skip confirmation prompts")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print plan without writing files")
	fs.BoolVar(&opts.NoProfile, "no-profile", false, "do not edit shell profile; print PATH hint instead")
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
	opts.Wake = strings.TrimSpace(opts.Wake)
	if !validSetupWake(opts.Wake) {
		return fmt.Errorf("--wake must be one of tmux, runner, both")
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
  agentchute setup [--wake tmux|runner|both] [--wrappers all|none|<list>] [--yes] [--dry-run]

Scaffolds the control repo with agentchute init, installs lifecycle hooks for
the selected wrappers, and installs launcher shims only for runner/both modes.

Flags:
  --wake <mode>          tmux | runner | both (prompted when omitted)
  --wrappers <set>       all (detected on PATH), none, or comma list
                         (claude-code,codex,gemini-cli; default all)
  --control-repo <path>  repo to initialize (default env or current git/cwd root)
  --shim-dir <path>      launcher shim directory (default $HOME/.agentchute/bin)
  --profile <path>       shell profile to update for launcher shims
  --no-profile           do not edit shell profile; print PATH hint instead
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

func validSetupWake(wake string) bool {
	switch wake {
	case setupWakeTmux, setupWakeRunner, setupWakeBoth:
		return true
	default:
		return false
	}
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
	fmt.Fprint(os.Stdout, "Primary wake path [runner/tmux/both] (runner): ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	answer := strings.TrimSpace(strings.ToLower(line))
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

func resolveSetupWrappers(raw, shimDir string) ([]string, map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return detectSetupWrappers(shimDir), detectSetupWrapperPaths(shimDir), nil
	}
	if raw == "none" {
		return nil, detectSetupWrapperPaths(shimDir), nil
	}
	known := map[string]bool{}
	for _, w := range hookWrappers {
		known[w.Name] = true
	}
	seen := map[string]bool{}
	var wrappers []string
	for _, part := range strings.Split(raw, ",") {
		key := strings.TrimSpace(part)
		if key == "" {
			continue
		}
		if !known[key] {
			return nil, nil, fmt.Errorf("--wrappers %q is not recognized; known: claude-code, codex, gemini-cli, all, none", key)
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
	for _, w := range hookWrappers {
		if paths[w.Name] != "" {
			wrappers = append(wrappers, w.Name)
		}
	}
	return wrappers
}

func detectSetupWrapperPaths(shimDir string) map[string]string {
	out := map[string]string{}
	for _, w := range hookWrappers {
		for _, candidate := range setupWrapperCandidates(w.Name) {
			path := findExecutableOutsideDir(candidate, shimDir)
			if path != "" {
				out[w.Name] = path
				break
			}
		}
	}
	return out
}

func setupWrapperCandidates(name string) []string {
	switch name {
	case "claude-code":
		return []string{"claude", "claude-code"}
	case "codex":
		return []string{"codex"}
	case "gemini-cli":
		return []string{"gemini", "gemini-cli"}
	default:
		return []string{name}
	}
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
	fmt.Fprintln(w, "agentchute setup")
	fmt.Fprintf(w, "  control repo: %s\n", root)
	fmt.Fprintf(w, "  wake:         %s\n", opts.Wake)
	if len(wrappers) == 0 {
		fmt.Fprintln(w, "  wrappers:     none")
	} else {
		fmt.Fprintf(w, "  wrappers:     %s\n", strings.Join(wrappers, ", "))
	}
	fmt.Fprintf(w, "  init:         %s\n", filepath.Join(root, "AGENTCHUTE.md"))
	if len(wrappers) > 0 {
		fmt.Fprintln(w, "  hooks:        repo scope, force/idempotent")
	}
	if setupNeedsShims(opts.Wake) {
		fmt.Fprintf(w, "  shims:        %s\n", opts.ShimDir)
		if opts.NoProfile {
			fmt.Fprintln(w, "  profile:      skipped (--no-profile)")
		} else if profile := setupProfilePath(opts.Profile); profile != "" {
			fmt.Fprintf(w, "  profile:      %s\n", profile)
		} else {
			fmt.Fprintln(w, "  profile:      skipped (no supported shell profile detected)")
		}
	}
	if opts.Wrappers == "all" {
		for _, wrapper := range hookWrappers {
			if detected[wrapper.Name] == "" {
				fmt.Fprintf(w, "  detected:     %s not found on PATH; skipped\n", wrapper.Name)
			} else {
				fmt.Fprintf(w, "  detected:     %s -> %s\n", wrapper.Name, detected[wrapper.Name])
			}
		}
	}
	if opts.Wake == setupWakeTmux && os.Getenv("TMUX") == "" {
		fmt.Fprintln(w, "  warning:      TMUX is not set; start wrappers inside tmux for tmux wake")
	}
}

func setupNeedsShims(wake string) bool {
	return wake == setupWakeRunner || wake == setupWakeBoth
}

func applySetup(root string, opts setupOptions, wrappers []string) error {
	return runInDir(root, func() error {
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
		globalState, _ := readSetupGlobalState()
		poolState, _ := readSetupPoolState(cfg)
		for _, wrapper := range droppedWrappers(poolState.Wrappers, wrappers) {
			if err := removeSetupHook(wrapper, root); err != nil {
				return err
			}
		}
		for _, wrapper := range wrappers {
			hook, ok := hookWrapperByName(wrapper)
			if !ok {
				return fmt.Errorf("hooks install %s: unknown wrapper", wrapper)
			}
			if err := installOneHook(hook, root, false, true); err != nil {
				return fmt.Errorf("hooks install %s: %w", wrapper, err)
			}
		}

		currentNeedsShims := setupNeedsShims(opts.Wake) && len(wrappers) > 0
		for _, wrapper := range droppedWrappers(globalState.Wrappers, wrappers) {
			if err := removeSetupShimsForWrapper(globalState.ShimDir, wrapper); err != nil {
				return err
			}
		}
		if currentNeedsShims {
			if err := cmdShims([]string{
				"install",
				"--dir", opts.ShimDir,
				"--wrapper", strings.Join(wrappers, ","),
				"--force",
			}); err != nil {
				return fmt.Errorf("shims install: %w", err)
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
			if globalState.PathBlock {
				if err := setupRemovePathBlock(globalState.Profile); err != nil {
					return err
				}
			}
		}

		if err := writeSetupPoolState(cfg, opts.Wake, wrappers); err != nil {
			return err
		}
		profile := setupProfilePath(opts.Profile)
		pathBlock := currentNeedsShims && !opts.NoProfile && profile != "" &&
			(setupProfileHasBlock(profile) || !setupPathContains(opts.ShimDir))
		if err := writeSetupGlobalState(setupGlobalState{
			Version:        1,
			Wake:           opts.Wake,
			Wrappers:       wrappers,
			ShimDir:        opts.ShimDir,
			Profile:        profile,
			PathBlock:      pathBlock,
			ShimsInstalled: setupNeedsShims(opts.Wake) && len(wrappers) > 0,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}

		fmt.Println("\nsetup complete.")
		switch opts.Wake {
		case setupWakeTmux:
			fmt.Println("Restart selected wrappers from this repo inside tmux, then run `agentchute doctor --as <id>`.")
		case setupWakeRunner, setupWakeBoth:
			fmt.Println("Open one new shell, restart selected wrappers from this repo, then run `agentchute doctor --as <id>`.")
		}
		return nil
	})
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
	if opts.NoProfile || setupPathContains(opts.ShimDir) {
		if !setupPathContains(opts.ShimDir) {
			fmt.Printf("warning: add %s to PATH before your wrapper binaries\n", opts.ShimDir)
		}
		return nil
	}
	profile := setupProfilePath(opts.Profile)
	if profile == "" {
		fmt.Printf("warning: add %s to PATH before your wrapper binaries\n", opts.ShimDir)
		return nil
	}
	return setupWritePathBlock(profile, opts.ShimDir)
}

func setupProfilePath(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return ""
	}
	switch {
	case strings.HasSuffix(os.Getenv("SHELL"), "zsh"):
		return filepath.Join(home, ".zshrc")
	case strings.HasSuffix(os.Getenv("SHELL"), "bash"):
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	case strings.HasSuffix(os.Getenv("SHELL"), "fish"):
		return filepath.Join(home, ".config", "fish", "config.fish")
	case strings.HasSuffix(os.Getenv("SHELL"), "sh"):
		return filepath.Join(home, ".profile")
	default:
		return ""
	}
}

func setupPathContains(dir string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			entry = "."
		}
		abs, err := filepath.Abs(entry)
		if err == nil && samePath(abs, absDir) {
			return true
		}
	}
	return false
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
		return fmt.Sprintf("%s\nif not contains %s $PATH\n    set -gx PATH %s $PATH\nend\n%s\n",
			setupPathBlockBegin, expr, expr, setupPathBlockEnd)
	}
	return fmt.Sprintf("%s\ncase \":$PATH:\" in\n  *\":%s:\"*) ;;\n  *) export PATH=\"%s:$PATH\" ;;\nesac\n%s\n",
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
	start := strings.Index(existing, setupPathBlockBegin)
	if start < 0 {
		return existing, false
	}
	end := strings.Index(existing[start:], setupPathBlockEnd)
	if end < 0 {
		return existing, false
	}
	end += start + len(setupPathBlockEnd)
	if end < len(existing) && existing[end] == '\n' {
		end++
	}
	next := strings.TrimRight(existing[:start], "\n")
	if next != "" && strings.TrimSpace(existing[end:]) != "" {
		next += "\n\n"
	}
	next += strings.TrimLeft(existing[end:], "\n")
	return next, true
}

func setupBackupPath(path string) string {
	return fmt.Sprintf("%s.agentchute-backup-%s", path, time.Now().UTC().Format("20060102T150405Z"))
}

func removeSetupShims(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	for _, spec := range shimSpecs {
		path := filepath.Join(dir, spec.Name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove shim %s: %w", path, err)
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
		path := filepath.Join(dir, spec.Name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove shim %s: %w", path, err)
		}
	}
	fmt.Printf("removed setup-managed %s shims from %s\n", wrapper, dir)
	return nil
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

func writeSetupPoolState(cfg *loop.Config, wake string, wrappers []string) error {
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
