package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// hooksFS exposes the canonical hook templates under examples/hooks/ so
// `agentchute hooks install` can write them into the operator's
// .claude/, .codex/, .gemini/ trees without depending on a checked-out
// copy of the agentchute repo on the user's machine.
//
// The directory layout mirrors the install target:
//
//	examples/hooks/<wrapper>/<rel_path>
//
// where <rel_path> for a repo-scope install is appended to cwd (or
// $HOME for user-scope). Each wrapper's payload is the path the
// wrapper itself looks for at startup.
//
// The backing FS is embedded (//go:embed all:examples/hooks) in the root main
// package — //go:embed cannot reach a parent directory — and injected here by
// Main via cli.Assets. Paths are read relative to the embed root, e.g.
// "examples/hooks/claude-code/.claude/settings.json".
var hooksFS fs.FS

// hookWrapper is one supported wrapper's install descriptor. Only the
// canonical agent ids are supported by default; operators with
// non-canonical setups can copy the file by hand.
//
// Dest paths are resolved against the install scope root: for
// --scope repo this is the control-repo root (the dir holding
// AGENTCHUTE.md, discovered via loop.Discover); for --scope user
// it is $HOME. The historical "cwd-relative" framing was retired in
// v0.2.1 when --scope repo started anchoring at the control repo so
// that install from any subdirectory writes to the same place a
// wrapper-at-repo-root looks for its hooks.
type hookWrapper struct {
	Name string // user-facing wrapper key (claude-code | codex | gemini-cli)
	Src  string // path inside hooksFS (relative to embed root)
	Dest string // path relative to install scope root
}

var hookWrappers = []hookWrapper{
	{
		Name: "claude-code",
		Src:  "examples/hooks/claude-code/.claude/settings.json",
		Dest: ".claude/settings.json",
	},
	{
		Name: "codex",
		Src:  "examples/hooks/codex/.codex/hooks.json",
		Dest: ".codex/hooks.json",
	},
	{
		Name: "gemini-cli",
		Src:  "examples/hooks/gemini/.gemini/settings.json",
		Dest: ".gemini/settings.json",
	},
}

func cmdHooks(args []string) error {
	if len(args) < 1 {
		return hooksUsage(fmt.Errorf("missing subcommand"))
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "install":
		return cmdHooksInstall(rest)
	case "-h", "--help", "help":
		fmt.Print(hooksHelp())
		return nil
	default:
		return hooksUsage(fmt.Errorf("unknown subcommand %q", sub))
	}
}

func cmdHooksInstall(args []string) error {
	fs := flag.NewFlagSet("hooks install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	// Default --wrapper to "all" so the enrollment block's
	// `agentchute hooks install` (no flag) is a valid one-liner.
	// Operators who want only one wrapper still pass --wrapper explicitly.
	var wrapper, scope string
	var dryRun, force bool
	fs.StringVar(&wrapper, "wrapper", "all", "wrapper key: claude-code | codex | gemini-cli | all (default: all)")
	fs.StringVar(&scope, "scope", "repo", "install scope: repo (control-repo root) | user ($HOME-relative)")
	fs.BoolVar(&dryRun, "dry-run", false, "print what would be written without touching the filesystem")
	fs.BoolVar(&force, "force", false, "overwrite an existing hook file (default refuses)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(hooksInstallHelp())
			return nil
		}
		return hooksUsage(err)
	}
	if fs.NArg() != 0 {
		return hooksUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	var targets []hookWrapper
	if wrapper == "all" {
		targets = hookWrappers
	} else {
		for _, w := range hookWrappers {
			if w.Name == wrapper {
				targets = []hookWrapper{w}
				break
			}
		}
		if len(targets) == 0 {
			known := make([]string, 0, len(hookWrappers))
			for _, w := range hookWrappers {
				known = append(known, w.Name)
			}
			sort.Strings(known)
			return fmt.Errorf("--wrapper %q is not recognized; known: %s | all", wrapper, strings.Join(known, ", "))
		}
	}

	var scopeRoot string
	switch scope {
	case "repo":
		// Codex review #2: from a subdir, plain os.Getwd() lands the
		// hook files in the wrong place (the wrapper started at the
		// repo root won't see them). Anchor to the control-repo root
		// via the same discovery cascade init uses.
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err := discoverConfigForHooks(cwd)
		if err != nil {
			return fmt.Errorf("--scope repo: cannot discover control repo: %w", err)
		}
		scopeRoot = cfg.ControlRepo
	case "user":
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("--scope user: cannot resolve $HOME: %w", err)
		}
		scopeRoot = home
	default:
		return fmt.Errorf("--scope %q: must be repo or user", scope)
	}

	for _, w := range targets {
		if err := installOneHook(w, scopeRoot, dryRun, force); err != nil {
			return err
		}
	}
	return nil
}

func installOneHook(w hookWrapper, scopeRoot string, dryRun, force bool) error {
	src, err := fs.ReadFile(hooksFS, w.Src)
	if err != nil {
		return fmt.Errorf("read embedded template for %s: %w", w.Name, err)
	}

	dest := filepath.Join(scopeRoot, w.Dest)

	// Existence + overwrite semantics. If the destination already has the
	// exact bytes we'd write, treat as a no-op (idempotent re-runs are a
	// feature; this is how `agentchute init` works too).
	existing, statErr := os.ReadFile(dest)
	if statErr == nil {
		if bytes.Equal(existing, src) {
			fmt.Printf("hooks install %s → %s: already current; skipping\n", w.Name, dest)
			return nil
		}
		if !force {
			return fmt.Errorf("%s already exists and differs from the canonical template; pass --force to overwrite (a backup at %s.bak will be written)", dest, dest)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat %s: %w", dest, statErr)
	}

	if dryRun {
		fmt.Printf("hooks install %s → %s (dry-run; would write %d bytes)\n", w.Name, dest, len(src))
		return nil
	}

	// Ensure parent dir exists at 0700 — hook files contain agent IDs and
	// host metadata that we don't want world-readable. Codex review #3:
	// MkdirAll only sets perms on directories it creates; if .claude/
	// already exists at 0755, MkdirAll leaves it alone. Tighten with an
	// explicit chmod after the mkdir.
	parentDir := filepath.Dir(dest)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return fmt.Errorf("mkdir parent for %s: %w", dest, err)
	}
	if err := os.Chmod(parentDir, 0o700); err != nil {
		return fmt.Errorf("chmod parent for %s: %w", dest, err)
	}

	// Backup any pre-existing file we're about to overwrite.
	if statErr == nil && force {
		backup := dest + ".bak"
		if err := os.WriteFile(backup, existing, 0o600); err != nil {
			return fmt.Errorf("write backup for %s: %w", dest, err)
		}
		fmt.Printf("hooks install %s → %s: existing file backed up to %s\n", w.Name, dest, backup)
	}

	// Atomic write: temp + rename, mirroring the inbox-delivery convention.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp_hook-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", dest, err)
	}
	if _, err := tmp.Write(src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write temp for %s: %w", dest, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close temp for %s: %w", dest, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("chmod temp for %s: %w", dest, err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename %s → %s: %w", tmp.Name(), dest, err)
	}

	fmt.Printf("hooks install %s → %s (%d bytes)\n", w.Name, dest, len(src))
	return nil
}

func hooksUsage(err error) error {
	return fmt.Errorf("%s\n%s", err.Error(), hooksHelp())
}

func hooksHelp() string {
	return strings.TrimSpace(`
Usage: agentchute hooks <subcommand> [flags]

Subcommands:
  install   Write the canonical hook template for the named wrapper.

Run 'agentchute hooks install -h' for install-specific flags.
`) + "\n"
}

func hooksInstallHelp() string {
	return strings.TrimSpace(`
Usage: agentchute hooks install [flags]

Writes the canonical hook template(s) into the operator's
.claude/, .codex/, .gemini/ tree. Atomic temp+rename; 0600 file,
0700 parent dir. Idempotent re-runs report "already current".

Flags:
  --wrapper <name>      claude-code | codex | gemini-cli | all
                        (default: all)
  --scope <scope>       repo (control-repo root) | user ($HOME)
                        (default: repo)
  --dry-run             print what would be written without writing
  --force               overwrite an existing diverged hook file
                        (writes a .bak backup first)
`) + "\n"
}

// discoverConfigForHooks walks the standard control-repo discovery
// cascade so --scope repo anchors at the repo root, not the user's
// current subdir. Mirrors init/boot/check's loop.Discover call shape.
func discoverConfigForHooks(cwd string) (*loop.Config, error) {
	return loop.Discover(loop.DiscoverOpts{
		Cwd:            cwd,
		EnvControlRepo: os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:     os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
}
