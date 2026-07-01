package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

type wrapperSpec struct {
	// Key is the canonical wrapper name used by the `ac` dispatcher
	// (`ac serve <Key>`). Name is the legacy generated-shim filename (ac-*),
	// retained only until the dispatcher fully replaces generated shims.
	Key        string
	Name       string
	Aliases    []string
	AgentID    string
	Vendor     string
	Candidates []string
}

var wrapperSpecs = []wrapperSpec{
	{Key: "claude", Name: "ac-claude", Aliases: []string{"claude", "claude-code"}, AgentID: "claude-code", Vendor: "anthropic", Candidates: []string{"claude", "claude-code"}},
	{Key: "codex", Name: "ac-codex", Aliases: []string{"codex"}, AgentID: "codex", Vendor: "openai", Candidates: []string{"codex"}},
	{Key: "gemini", Name: "ac-gemini", Aliases: []string{"gemini", "gemini-cli", "agy"}, AgentID: "gemini-cli", Vendor: "google", Candidates: []string{"gemini", "gemini-cli", "agy"}},
	{Key: "grok", Name: "ac-grok", Aliases: []string{"grok"}, AgentID: "grok", Vendor: "xai", Candidates: []string{"grok"}},
}

var legacyShimNamesBySetupWrapper = map[string][]string{
	"claude-code": {"ac-claude", "claude", "claude-code"},
	"codex":       {"ac-codex", "codex"},
	"gemini-cli":  {"ac-gemini", "gemini", "gemini-cli", "agy"},
	"grok":        {"ac-grok", "grok"},
}

var legacyShimSetupWrapperOrder = []string{"claude-code", "codex", "gemini-cli", "grok"}

// wrapperForToken resolves a dispatcher wrapper token (`ac serve <token>`) by
// canonical Key or alias. It deliberately does NOT match the legacy ac-* Name —
// the dispatcher addresses wrappers, not generated shim filenames.
func wrapperForToken(token string) (wrapperSpec, bool) {
	token = strings.TrimSpace(token)
	for _, spec := range wrapperSpecs {
		if spec.Key == token {
			return spec, true
		}
		for _, alias := range spec.Aliases {
			if alias == token {
				return spec, true
			}
		}
	}
	return wrapperSpec{}, false
}

// knownWrapperTokens lists every accepted wrapper token for error suggestions.
func knownWrapperTokens() []string {
	var out []string
	for _, spec := range wrapperSpecs {
		out = append(out, spec.Key)
	}
	return out
}

func cmdShims(args []string) error {
	if len(args) == 0 {
		return shimsUsage(fmt.Errorf("missing subcommand"))
	}
	switch args[0] {
	case "install":
		return cmdShimsInstall(args[1:])
	case "exec":
		return cmdShimsExec(args[1:])
	case "-h", "--help", "help":
		fmt.Print(shimsHelp())
		return nil
	default:
		return shimsUsage(fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

func cmdShimsInstall(args []string) error {
	fs := flag.NewFlagSet("shims install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var dir string
	var force, quiet bool
	fs.StringVar(&dir, "dir", "", "shim directory (default: $HOME/.agentchute/bin)")
	fs.BoolVar(&force, "force", false, "overwrite an existing agentchute-owned `ac` dispatcher")
	fs.BoolVar(&quiet, "quiet", false, "suppress status text")
	if err := fs.Parse(args); err != nil {
		return shimsUsage(err)
	}
	if fs.NArg() != 0 {
		return shimsUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".agentchute", "bin")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	// Writes-before-reset: install the new `ac` dispatcher BEFORE removing any
	// legacy per-wrapper ac-* shims, so a pool is never left with no launcher.
	if err := installDispatcher(absDir, exe, force); err != nil {
		return err
	}
	removed, err := removeLegacyWrapperShims(absDir)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Printf("installed agentchute dispatcher `ac` to %s\n", absDir)
		if len(removed) > 0 {
			fmt.Printf("removed %d legacy launcher shim(s): %s\n", len(removed), strings.Join(removed, ", "))
		}
		if !pathContains(absDir, os.Getenv("PATH")) {
			fmt.Printf("warning: %s is not on PATH; add it to PATH\n", absDir)
			fmt.Println("\nRecommended: run `agentchute setup` to wire PATH and lifecycle hooks automatically.")
		}
	}
	return nil
}

func legacyShimNamesForSetupWrapper(wrapper string) []string {
	names := legacyShimNamesBySetupWrapper[strings.TrimSpace(wrapper)]
	if len(names) == 0 {
		return nil
	}
	return append([]string(nil), names...)
}

func allLegacyShimNames() []string {
	var names []string
	for _, wrapper := range legacyShimSetupWrapperOrder {
		names = append(names, legacyShimNamesBySetupWrapper[wrapper]...)
	}
	return names
}

// renderDispatcherScript renders the single `ac` dispatcher that setup /
// `shims install` writes. It is wrapper-agnostic: it execs `agentchute dispatch`,
// which routes `ac <command>` and `ac serve <wrapper>` (parsed in dispatch.go). The
// --shim-dir argument lets dispatch exclude a stale same-name alias shim inside
// the shim dir during the transition (AGENTCHUTE_BIN override + shellQuote
// discipline).
func renderDispatcherScript(agentchuteBin, shimDir string) string {
	return fmt.Sprintf(`#!/bin/sh
# agentchute dispatcher v1
AGENTCHUTE_BIN=${AGENTCHUTE_BIN:-%s}
exec "$AGENTCHUTE_BIN" dispatch --shim-dir %s -- "$@"
`, shellQuote(agentchuteBin), shellQuote(shimDir))
}

// isAgentchuteDispatcher reports whether path is an agentchute-owned `ac`
// dispatcher: a REGULAR file (never a symlink) whose content carries both the
// `dispatch --shim-dir` exec line and the AGENTCHUTE_BIN override marker. A
// missing file, a symlink, or any file lacking the markers returns false, so the
// collision guard refuses to overwrite it.
func isAgentchuteDispatcher(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect dispatcher %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read dispatcher %s: %w", path, err)
	}
	text := string(data)
	return strings.Contains(text, "dispatch --shim-dir") && strings.Contains(text, "AGENTCHUTE_BIN="), nil
}

// installDispatcher writes the single `ac` dispatcher into absDir. It REFUSES to
// replace a symlink or a non-agentchute regular file at absDir/ac (collision
// guard — never clobber a user-owned `ac`; the system /usr/sbin/ac lives outside
// absDir and is never touched). An existing agentchute-owned dispatcher is
// overwritten only with force (idempotent setup re-runs pass force=true). The
// script is written 0o700.
func installDispatcher(absDir, agentchuteBin string, force bool) error {
	if err := loop.EnsurePrivateDir(absDir); err != nil {
		return err
	}
	path := filepath.Join(absDir, "ac")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite non-agentchute ac at %s", path)
		}
		owned, derr := isAgentchuteDispatcher(path)
		if derr != nil {
			return derr
		}
		if !owned {
			return fmt.Errorf("refusing to overwrite non-agentchute ac at %s", path)
		}
		if !force {
			return fmt.Errorf("%s already exists; pass --force to overwrite", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(renderDispatcherScript(agentchuteBin, absDir)), 0o700)
}

// removeLegacyWrapperShims removes the generated per-wrapper ac-* launchers (and
// their legacy same-name aliases) that the single `ac` dispatcher supersedes. It
// removes ONLY marker-matching agentchute shims (isAgentchuteShim) and silently
// skips non-existent and user-owned same-name files. Returns the removed names.
func removeLegacyWrapperShims(absDir string) ([]string, error) {
	if strings.TrimSpace(absDir) == "" {
		return nil, nil
	}
	var removed []string
	for _, name := range allLegacyShimNames() {
		path := filepath.Join(absDir, name)
		owned, err := isAgentchuteShim(path)
		if err != nil {
			return removed, err
		}
		if !owned {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove legacy shim %s: %w", path, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}

func cmdShimsExec(args []string) error {
	fs := flag.NewFlagSet("shims exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var name, shimDir string
	fs.StringVar(&name, "name", "", "shim command name")
	fs.StringVar(&shimDir, "shim-dir", "", "directory containing agentchute shims")
	if err := fs.Parse(args); err != nil {
		return shimsUsage(err)
	}
	spec, ok := wrapperSpecForName(name)
	if !ok {
		return fmt.Errorf("unknown shim name %q", name)
	}
	realWrapper, err := resolveRealWrapper(spec, shimDir)
	if err != nil {
		return err
	}
	wrapperArgs := append([]string{realWrapper}, fs.Args()...)
	if os.Getenv("AGENTCHUTE_SHIM_BYPASS") == "1" || os.Getenv("AGENTCHUTE_RUNNER") == "1" {
		return execReplace(realWrapper, wrapperArgs, os.Environ())
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		Cwd:            cwd,
		EnvControlRepo: os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:     os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		if loop.IsNoControlRepo(err) {
			return execReplace(realWrapper, wrapperArgs, os.Environ())
		}
		return fmt.Errorf("agentchute shim discovery failed from %s: %w", cwd, err)
	}
	agentchuteBin, err := os.Executable()
	if err != nil {
		return err
	}
	runArgs := []string{
		agentchuteBin,
		"serve",
		"--vendor", spec.Vendor,
		"--control-repo", cfg.ControlRepo,
		"--loop-dir", cfg.LoopDir,
		"--shim-name", spec.Name,
		"--",
	}
	runArgs = append(runArgs, wrapperArgs...)
	return execReplace(agentchuteBin, runArgs, os.Environ())
}

func wrapperSpecForName(name string) (wrapperSpec, bool) {
	name = strings.TrimSpace(filepath.Base(name))
	for _, spec := range wrapperSpecs {
		if spec.Name == name {
			return spec, true
		}
		for _, alias := range spec.Aliases {
			if alias == name {
				return spec, true
			}
		}
	}
	return wrapperSpec{}, false
}

func resolveRealWrapper(spec wrapperSpec, shimDir string) (string, error) {
	absShimDir := ""
	if shimDir != "" {
		if abs, err := filepath.Abs(shimDir); err == nil {
			absShimDir = abs
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if absShimDir != "" && samePath(absDir, absShimDir) {
			continue
		}
		for _, candidate := range spec.Candidates {
			path := filepath.Join(absDir, candidate)
			if executableFileProblem(path) == "" {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("could not find real wrapper for shim %q on PATH outside %s", spec.Name, shimDir)
}

func samePath(a, b string) bool {
	aa, errA := filepath.EvalSymlinks(a)
	bb, errB := filepath.EvalSymlinks(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return a == b
}

func pathContains(dir, pathEnv string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, entry := range filepath.SplitList(pathEnv) {
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

func pathResolvesToDir(dir, pathEnv string, names []string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, name := range names {
		found := false
		for _, entry := range filepath.SplitList(pathEnv) {
			if entry == "" {
				entry = "."
			}
			abs, err := filepath.Abs(entry)
			if err != nil {
				continue
			}
			path := filepath.Join(abs, name)
			if executableFileProblem(path) != "" {
				continue
			}
			if !samePath(abs, absDir) {
				return false
			}
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// pathIsPrioritized returns true if absDir is either the first entry in pathEnv
// or at least appears before any other directory that contains a wrapper
// executable with any of the names in candidates.
func pathIsPrioritized(absDir, pathEnv string, candidates []string) bool {
	absDir, err := filepath.Abs(absDir)
	if err != nil {
		return false
	}
	foundShimDir := false
	for _, entry := range filepath.SplitList(pathEnv) {
		if entry == "" {
			entry = "."
		}
		abs, err := filepath.Abs(entry)
		if err != nil {
			continue
		}
		if samePath(abs, absDir) {
			foundShimDir = true
			continue
		}
		// If we haven't found the shim dir yet, check if this dir shadows it.
		if !foundShimDir {
			for _, name := range candidates {
				if executableFileProblem(filepath.Join(abs, name)) == "" {
					return false // Shadowed by a real binary earlier in pathEnv
				}
			}
		}
	}
	return foundShimDir
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func shimsUsage(err error) error {
	if err == flag.ErrHelp {
		return shimsHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, shimsHelp())
}

func shimsHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, shimsHelp())
}

func shimsHelp() string {
	return strings.TrimSpace(`
Usage:
  agentchute shims install [--dir <path>] [--force] [--quiet]
  agentchute shims exec --name <wrapper> --shim-dir <dir> -- [args...]

` + "`shims install`" + ` installs the single ` + "`ac`" + ` dispatcher (default
$HOME/.agentchute/bin/ac) and removes any legacy per-wrapper ac-* launchers it
supersedes. The dispatcher routes ` + "`ac <command>`" + ` to agentchute and
` + "`ac serve <wrapper>`" + ` to the runner.
`)
}
