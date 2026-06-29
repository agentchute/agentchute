package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

type shimSpec struct {
	Name       string
	Aliases    []string
	AgentID    string
	Vendor     string
	Candidates []string
}

var shimSpecs = []shimSpec{
	{Name: "ac-claude", Aliases: []string{"claude", "claude-code"}, AgentID: "claude-code", Vendor: "anthropic", Candidates: []string{"claude", "claude-code"}},
	{Name: "ac-codex", Aliases: []string{"codex"}, AgentID: "codex", Vendor: "openai", Candidates: []string{"codex"}},
	{Name: "ac-gemini", Aliases: []string{"gemini", "gemini-cli", "agy"}, AgentID: "gemini-cli", Vendor: "google", Candidates: []string{"gemini", "gemini-cli", "agy"}},
	{Name: "ac-grok", Aliases: []string{"grok"}, AgentID: "grok", Vendor: "xai", Candidates: []string{"grok"}},
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
	var dir, wrapper string
	var aliases, force, quiet bool
	fs.StringVar(&dir, "dir", "", "shim directory (default: $HOME/.agentchute/bin)")
	fs.StringVar(&wrapper, "wrapper", "all", "wrapper key(s): claude-code,codex,gemini-cli,grok or all")
	fs.BoolVar(&aliases, "aliases", false, "also install legacy same-name wrapper aliases")
	fs.BoolVar(&force, "force", false, "overwrite existing shim files")
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
	if err := loop.EnsurePrivateDir(absDir); err != nil {
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
	selected, err := selectShimSpecs(wrapper)
	if err != nil {
		return err
	}
	for _, spec := range selected {
		for _, name := range shimInstallNames(spec, aliases) {
			path := filepath.Join(absDir, name)
			if !force {
				if _, err := os.Lstat(path); err == nil {
					return fmt.Errorf("%s already exists; pass --force to overwrite", path)
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			if err := os.WriteFile(path, []byte(renderShimScript(exe, absDir, name)), 0o700); err != nil {
				return err
			}
		}
	}
	if !quiet {
		fmt.Printf("installed agentchute shims to %s\n", absDir)
		if !pathContains(absDir, os.Getenv("PATH")) {
			fmt.Printf("warning: %s is not on PATH; add it to PATH\n", absDir)
			fmt.Println("\nRecommended: run `agentchute setup` to wire PATH and lifecycle hooks automatically.")
		}
	}
	return nil
}

func shimInstallNames(spec shimSpec, aliases bool) []string {
	names := []string{spec.Name}
	if aliases {
		names = append(names, spec.Aliases...)
	}
	return names
}

func allShimCommandNames(aliases bool) []string {
	var names []string
	for _, spec := range shimSpecs {
		names = append(names, shimInstallNames(spec, aliases)...)
	}
	return names
}

func selectShimSpecs(wrapper string) ([]shimSpec, error) {
	wrapper = strings.TrimSpace(wrapper)
	if wrapper == "" || wrapper == "all" {
		return shimSpecs, nil
	}
	wanted := map[string]bool{}
	for _, part := range strings.Split(wrapper, ",") {
		key := strings.TrimSpace(part)
		if key == "" {
			continue
		}
		wanted[key] = true
	}
	if len(wanted) == 0 {
		return nil, fmt.Errorf("--wrapper must not be empty")
	}
	var selected []shimSpec
	matched := map[string]bool{}
	for _, spec := range shimSpecs {
		if wanted[spec.Name] || wanted[spec.AgentID] || wantedAny(wanted, spec.Aliases) {
			selected = append(selected, spec)
			if wanted[spec.Name] {
				matched[spec.Name] = true
			}
			if wanted[spec.AgentID] {
				matched[spec.AgentID] = true
			}
			for _, alias := range spec.Aliases {
				if wanted[alias] {
					matched[alias] = true
				}
			}
		}
	}
	for key := range wanted {
		if !matched[key] {
			return nil, fmt.Errorf("--wrapper %q is not recognized; known: claude-code, codex, gemini-cli, grok, all", key)
		}
	}
	return selected, nil
}

func wantedAny(wanted map[string]bool, values []string) bool {
	for _, v := range values {
		if wanted[v] {
			return true
		}
	}
	return false
}

func renderShimScript(agentchuteBin, shimDir, name string) string {
	return fmt.Sprintf(`#!/bin/sh
# agentchute shim v1
AGENTCHUTE_BIN=${AGENTCHUTE_BIN:-%s}
exec "$AGENTCHUTE_BIN" shims exec --name %s --shim-dir %s -- "$@"
`, shellQuote(agentchuteBin), shellQuote(name), shellQuote(shimDir))
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
	spec, ok := shimSpecForName(name)
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
		"run",
		"--vendor", spec.Vendor,
		"--control-repo", cfg.ControlRepo,
		"--loop-dir", cfg.LoopDir,
		"--shim-name", spec.Name,
		"--",
	}
	runArgs = append(runArgs, wrapperArgs...)
	return execReplace(agentchuteBin, runArgs, os.Environ())
}

func shimSpecForName(name string) (shimSpec, bool) {
	name = strings.TrimSpace(filepath.Base(name))
	for _, spec := range shimSpecs {
		if spec.Name == name {
			return spec, true
		}
		for _, alias := range spec.Aliases {
			if alias == name {
				return spec, true
			}
		}
	}
	return shimSpec{}, false
}

func resolveRealWrapper(spec shimSpec, shimDir string) (string, error) {
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
  agentchute shims install [--dir <path>] [--wrapper <name[,name...]>] [--aliases] [--force] [--quiet]
  agentchute shims exec --name <wrapper> --shim-dir <dir> -- [args...]

Wrapper keys: claude-code, codex, gemini-cli, grok (or all).

Launcher shims install namespaced commands such as ac-codex. They route through
agentchute run inside initialized pools and pass through to the real wrapper
elsewhere. Pass --aliases to also install legacy same-name wrapper aliases.
`)
}
