package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
		// codex trusts hooks PER POSITION: ~/.codex/config.toml records
		// `[hooks.state."<root>/.codex/hooks.json:<event>:<group>:<index>"]`,
		// so inserting or reordering an entry in the template silently
		// untrusts every later one (they are skipped until re-trusted in the
		// TUI). Append new hooks at the END of their group, never in the
		// middle; codexHookTrustMissing is what serve checks before arming
		// the guard.
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

// ---------- codex hook trust (opus-xhigh H3a) ----------

// codexConfigPath is codex's user config: $CODEX_HOME/config.toml, else
// ~/.codex/config.toml.
func codexConfigPath() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".codex", "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// codexHooksStateKeyRE matches one trust entry header:
// [hooks.state."<abs>/.codex/hooks.json:<event>:<group>:<index>"].
var codexHooksStateKeyRE = regexp.MustCompile(`^\s*\[hooks\.state\."((?:[^"\\]|\\.)*)"\]\s*$`)

// codexHookEventKey maps a hooks.json event name to codex's trust-key
// spelling (SessionStart -> session_start).
func codexHookEventKey(event string) string {
	var b strings.Builder
	for i, r := range event {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// codexTrustEntry is one hook position with the trust hash codex records
// for it: key `<hooksPath>:<event>:<group>:<index>`, hash `sha256:<64 hex>`.
type codexTrustEntry struct {
	Key  string
	Hash string
}

// codexHookTrustHash reproduces codex's trust hash for one hook position
// (codex-rs/hooks/src/engine/discovery.rs hook_hash +
// codex-rs/config/src/fingerprint.rs version_for_toml): sha256 over the
// compact, key-sorted JSON of {event_name, matcher (when the group has one),
// hooks: [normalized handler]} where the handler is {type:"command", command,
// async:false, timeout (the file's value, else codex's 600 s default),
// statusMessage (when set)}. Verified against every entry codex wrote for this
// repo's installed template (PR #213 gate): all five positions match.
func codexHookTrustHash(event string, matcher *string, hook map[string]any) string {
	handler := map[string]any{"type": "command", "async": false}
	if cmd, ok := hook["command"].(string); ok {
		handler["command"] = cmd
	}
	handler["timeout"] = 600
	if tmo, ok := hook["timeout"]; ok {
		handler["timeout"] = tmo
	}
	if sm, ok := hook["statusMessage"]; ok {
		handler["statusMessage"] = sm
	}
	identity := map[string]any{"event_name": codexHookEventKey(event), "hooks": []any{handler}}
	if matcher != nil {
		identity["matcher"] = *matcher
	}
	serialized, _ := json.Marshal(identity) // Go sorts map keys, as sort_all_objects does
	sum := sha256.Sum256(serialized)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// codexExpectedTrust lists the trust entries codex records for every hook in
// hooksPath: one per `<event>:<group>:<index>`, keyed by position, with the
// hash the CURRENT file content yields.
func codexExpectedTrust(hooksPath string) ([]codexTrustEntry, error) {
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		return nil, err
	}
	var file struct {
		Hooks map[string][]struct {
			Matcher *string          `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", hooksPath, err)
	}
	events := make([]string, 0, len(file.Hooks))
	for event := range file.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	var out []codexTrustEntry
	for _, event := range events {
		for g, group := range file.Hooks[event] {
			for i, hook := range group.Hooks {
				out = append(out, codexTrustEntry{
					Key:  fmt.Sprintf("%s:%s:%d:%d", hooksPath, codexHookEventKey(event), g, i),
					Hash: codexHookTrustHash(event, group.Matcher, hook),
				})
			}
		}
	}
	return out, nil
}

// codexExpectedTrustKeys is codexExpectedTrust's keys only.
func codexExpectedTrustKeys(hooksPath string) ([]string, error) {
	entries, err := codexExpectedTrust(hooksPath)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	return keys, nil
}

// codexTrustedHashRE matches the one value a trust table carries.
var codexTrustedHashRE = regexp.MustCompile(`^\s*trusted_hash\s*=\s*"(sha256:[0-9a-f]{64})"\s*$`)

// codexTrustedHashes reads `[hooks.state."<key>"]` tables from a codex
// config and returns key -> well-formed trusted_hash. A header with no hash,
// or a malformed one, yields no entry: codex treats such a position as
// untrusted (HookTrustStatus::Untrusted), and so do we.
func codexTrustedHashes(data []byte) map[string]string {
	trusted := map[string]string{}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		if m := codexHooksStateKeyRE.FindStringSubmatch(line); m != nil {
			current = strings.ReplaceAll(m[1], `\"`, `"`)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			current = ""
			continue
		}
		if current == "" {
			continue
		}
		if m := codexTrustedHashRE.FindStringSubmatch(line); m != nil {
			trusted[current] = m[1]
		}
	}
	return trusted
}

// codexHookTrustMissing returns the positions in hooksPath that configPath
// does not trust AS INSTALLED, as `<event>:<group>:<index>` (suffixed
// " (modified)" when a hash is recorded but is not the current content's —
// codex's HookTrustStatus::Modified, skipped just like Untrusted). An
// unreadable config or hooks file is an error; the caller treats both as
// "not trusted".
func codexHookTrustMissing(configPath, hooksPath string) ([]string, error) {
	expected, err := codexExpectedTrust(hooksPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read codex config: %w", err)
	}
	trusted := codexTrustedHashes(data)
	var missing []string
	for _, e := range expected {
		pos := strings.TrimPrefix(e.Key, hooksPath+":")
		got, ok := trusted[e.Key]
		switch {
		case !ok:
			missing = append(missing, pos)
		case got != e.Hash:
			missing = append(missing, pos+" (modified)")
		}
	}
	return missing, nil
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

// refreshWrapperHook creates or refreshes the launched wrapper's repo hook and
// verifies exact parity with this binary before the wrapper starts. This makes
// each discovered control repo self-healing without a global repo registry.
func refreshWrapperHook(root, wrapper string) error {
	var target *hookWrapper
	for i := range hookWrappers {
		if hookWrappers[i].Name == wrapper {
			target = &hookWrappers[i]
			break
		}
	}
	if target == nil {
		return nil
	}

	want, err := fs.ReadFile(hooksFS, target.Src)
	if err != nil {
		return fmt.Errorf("read embedded template for %s: %w", target.Name, err)
	}
	dest := filepath.Join(root, target.Dest)
	got, err := os.ReadFile(dest)
	if err == nil && bytes.Equal(got, want) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", dest, err)
	}
	if err := installOneHook(*target, root, false, true); err != nil {
		return err
	}
	got, err = os.ReadFile(dest)
	if err != nil {
		return fmt.Errorf("read refreshed %s: %w", dest, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s does not match the canonical %s template after refresh", dest, target.Name)
	}
	return nil
}

// refreshHookCompatibility keeps every ALREADY-INSTALLED hookWrappers file
// under root working with this binary, independent of which wrappers are
// selected for membership in the current setup/update run: the v1.5.0
// cutover incident (docs/decisions/agentchute-v150-cutover-incident-and-
// fix.md) was a resync that only replayed wrapper membership, leaving an
// installed hook file invoking a subcommand this binary had removed.
// Existence-preserving no-create mode: a hookWrappers Dest that is not
// already installed stays uninstalled — installing one is a membership
// decision (setup's --wrappers loop), not a compatibility one. Returns the
// names of wrappers whose file content actually changed.
func refreshHookCompatibility(root string) ([]string, error) {
	var refreshed []string
	for _, w := range hookWrappers {
		dest := filepath.Join(root, w.Dest)
		before, err := os.ReadFile(dest)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", dest, err)
		}
		if err := installOneHook(w, root, false, true); err != nil {
			return nil, fmt.Errorf("refresh hook %s: %w", w.Name, err)
		}
		after, err := os.ReadFile(dest)
		if err != nil {
			return nil, fmt.Errorf("read refreshed %s: %w", dest, err)
		}
		if !bytes.Equal(before, after) {
			refreshed = append(refreshed, w.Name)
		}
	}
	return refreshed, nil
}

// verifyHookCompatibility scans every currently-installed hookWrappers file
// under root for subcommands unknown to this binary — the same signal
// doctor's hook_content_sanity BLOCKER reports (internal/cli/doctor.go),
// reusing its token scan (hookBodyUnknownSubcommands) so the two checks can
// never drift on what counts as "broken." Intended to run immediately after
// refreshHookCompatibility so a setup/update resync never finishes green
// with a hook file that would fail `unknown command` on its next
// invocation.
func verifyHookCompatibility(root string) error {
	var offenders []string
	for _, w := range hookWrappers {
		dest := filepath.Join(root, w.Dest)
		data, err := os.ReadFile(dest)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", dest, err)
		}
		body, err := hookCommandBody(data)
		if err != nil {
			offenders = append(offenders, fmt.Sprintf("%s (invalid JSON: %v)", w.Name, err))
			continue
		}
		if unknown := hookBodyUnknownSubcommands(body); len(unknown) > 0 {
			offenders = append(offenders, fmt.Sprintf("%s (`%s`)", w.Name, strings.Join(unknown, "`, `")))
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf("hook file(s) invoke unknown agentchute subcommand(s) after refresh: %s — run `agentchute hooks install --wrapper all --scope repo --force`", strings.Join(offenders, ", "))
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
	return discoverConfig(loop.DiscoverOpts{
		Cwd:            cwd,
		EnvControlRepo: os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:     os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
}
