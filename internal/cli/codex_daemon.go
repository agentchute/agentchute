package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// codex 0.161 runs every TUI session through ONE shared per-user background
// process (`codex app-server --managed-daemon`), forked by the first `codex`
// launch and outliving it. Hooks and the agent's shell commands run as children
// of that daemon, so they inherit the daemon's environment — the env of
// whichever `agentchute serve` launched codex FIRST — not the env of the serve
// that owns the current session. serve pins identity and the fence token in the
// child env (runnerChildEnv), so a daemon-hosted lane acts with a stale token
// and, across repos, the wrong pool: `send` fences, `turn-end` exits 1.
//
// `codex --no-daemon` runs the session's app-server in-process, restoring the
// assumption that a wrapper's hooks run in the wrapper's own process tree.
// Root cause and evidence: .tmp/briefs/2026-10-08-codex-daemon-rootcause.md.

const codexNoDaemonFlag = "--no-daemon"

// codexHelpProbeTimeout bounds the `codex --help` probe serve runs at launch.
const codexHelpProbeTimeout = 10 * time.Second

// codexHelpOutput runs `<bin> --help` and returns its combined output. A
// variable so tests can install a fake; nothing is cached — the installed
// codex can change between launches.
var codexHelpOutput = func(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexHelpProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// codexSupportsNoDaemon reports whether the installed codex advertises
// --no-daemon as a top-level option. The flag must appear as its own word:
// an older codex, a failed probe, or a lookalike flag all answer no, and the
// launch proceeds unchanged (the daemon hazard then remains; doctor's
// codex_daemon_env check reports it).
func codexSupportsNoDaemon(bin string) bool {
	out, err := codexHelpOutput(bin)
	if err != nil {
		return false
	}
	for _, word := range strings.Fields(out) {
		if word == codexNoDaemonFlag {
			return true
		}
	}
	return false
}

// codexNoDaemonIncompatible reports whether codex 0.161 would refuse the flag
// for these args: the binary's own strings say "--no-daemon cannot be used
// with codex queue" / "... with codex agents" / "... with --remote" (both
// subcommands exist to talk to the daemon). Exact-token matches only, so a
// prompt that merely contains the word is not one.
func codexNoDaemonIncompatible(args []string) bool {
	if dispatchHasFlag(args, "--remote") {
		return true
	}
	for _, a := range args {
		if a == "queue" || a == "agents" {
			return true
		}
	}
	return false
}

// ensureCodexNoDaemon returns the wrapper argv serve should launch: when the
// real wrapper is codex, the installed binary advertises --no-daemon (probe),
// and the operator did not already pass it anywhere, the flag is inserted
// directly after argv[0] — a top-level option, ahead of any subcommand such as
// `resume` or `exec` (`codex exec --no-daemon` exits 2: the subcommands do not
// accept it). Skipped for the daemon-only forms (queue, agents, --remote),
// which codex refuses to combine with the flag. Every other case returns args
// unchanged. Pure: the input slice is never mutated.
func ensureCodexNoDaemon(spec wrapperSpec, args []string, probe func(bin string) bool) []string {
	if spec.Key != "codex" || len(args) == 0 {
		return args
	}
	if dispatchHasFlag(args[1:], codexNoDaemonFlag) || codexNoDaemonIncompatible(args[1:]) {
		return args
	}
	if !probe(args[0]) {
		return args
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, args[0], codexNoDaemonFlag)
	out = append(out, args[1:]...)
	return out
}

// serveWrapperArgs is serve's single wrapper-argv step, shared by both launch
// forms: `ac serve codex ...` re-execs `agentchute serve ... -- <real codex> ...`
// (dispatch.go / shims.go), and a hand-typed `agentchute serve -- codex ...`
// arrives here directly. The spec is resolved from argv[0]'s basename, so an
// absolute path from the dispatcher and a bare name both match.
func serveWrapperArgs(args []string) ([]string, wrapperSpec) {
	if len(args) == 0 {
		return args, wrapperSpec{}
	}
	spec, ok := wrapperSpecForName(filepath.Base(args[0]))
	if !ok {
		return args, wrapperSpec{}
	}
	return ensureCodexNoDaemon(spec, args, codexSupportsNoDaemon), spec
}

// ---------- doctor: codex_daemon_env ----------

// codexDaemonProcess is one running `codex app-server --managed-daemon` with
// the agentchute-relevant slice of its environment. EnvErr is set when the
// process table listed the daemon but its environment could not be read.
type codexDaemonProcess struct {
	PID     int
	Command string
	Env     map[string]string
	EnvErr  error
}

// listCodexDaemons enumerates running codex managed daemons. A variable so
// doctor tests can install a fake process table.
var listCodexDaemons = scanCodexDaemons

func scanCodexDaemons() ([]codexDaemonProcess, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHelpProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	daemons := parseCodexDaemonProcessTable(string(out))
	for i := range daemons {
		daemons[i].Env, daemons[i].EnvErr = readProcessEnv(ctx, daemons[i].PID)
	}
	return daemons, nil
}

// parseCodexDaemonProcessTable picks the managed daemons out of
// `ps -axo pid=,ppid=,command=` output: argv[0]'s basename is codex, followed
// by `app-server`, with `--managed-daemon` among the arguments. The daemon's
// own `pid-update-loop` helper and TUI sessions are not daemons.
func parseCodexDaemonProcessTable(table string) []codexDaemonProcess {
	var out []codexDaemonProcess
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		argv := fields[2:]
		if filepath.Base(argv[0]) != "codex" || argv[1] != "app-server" {
			continue
		}
		if !dispatchHasFlag(argv[2:], "--managed-daemon") {
			continue
		}
		out = append(out, codexDaemonProcess{PID: pid, Command: strings.Join(argv, " ")})
	}
	return out
}

// readProcessEnv returns the AGENTCHUTE_* environment of pid: /proc on Linux,
// `ps -E` (environment appended to the command) on macOS/BSD.
func readProcessEnv(ctx context.Context, pid int) (map[string]string, error) {
	if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ")); err == nil {
		return parseProcessEnvBytes(data), nil
	}
	out, err := exec.CommandContext(ctx, "ps", "-E", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("ps -E -p %d: %w", pid, err)
	}
	env := parseProcessEnvWords(string(out))
	if len(env) == 0 {
		return nil, fmt.Errorf("ps -E -p %d: no environment in output (another user's process, or an unsupported ps)", pid)
	}
	return env, nil
}

func parseProcessEnvBytes(data []byte) map[string]string {
	env := map[string]string{}
	for _, kv := range bytes.Split(data, []byte{0}) {
		if k, v, ok := strings.Cut(string(kv), "="); ok && strings.HasPrefix(k, "AGENTCHUTE_") {
			env[k] = v
		}
	}
	return env
}

func parseProcessEnvWords(line string) map[string]string {
	env := map[string]string{}
	for _, word := range strings.Fields(line) {
		if k, v, ok := strings.Cut(word, "="); ok && strings.HasPrefix(k, "AGENTCHUTE_") {
			env[k] = v
		}
	}
	return env
}

// poolServeTokens reads every serve.claim under <loop>/state/<id>/ without a
// lock (read-only, like the rest of doctor) and returns token -> id.
func poolServeTokens(cfg *loop.Config) map[string]string {
	tokens := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(cfg.LoopDir, "state"))
	if err != nil {
		return tokens
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		claim, err := loop.ReadServeClaim(cfg, e.Name())
		if err != nil || claim.ServeToken == "" {
			continue
		}
		tokens[claim.ServeToken] = e.Name()
	}
	return tokens
}

func tokenPrefix(token string) string {
	if len(token) > 8 {
		return token[:8] + "…"
	}
	return token
}

// checkCodexDaemonEnv reports whether a running shared codex daemon carries
// this pool's live serve token and control repo. A daemon pinned elsewhere
// hosts every codex session's hooks and shell commands under a stale or
// foreign identity (the hazard above). Always WARN, never BLOCKER: the daemon
// is a per-user host condition outside the pool, and an unreadable process
// table or environment must not fail doctor for the other lanes.
func checkCodexDaemonEnv(cfg *loop.Config) doctorCheck {
	const name = "codex_daemon_env"
	daemons, err := listCodexDaemons()
	if err != nil {
		return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("could not enumerate codex app-server daemons (%v); a shared daemon pinned to another serve breaks codex hooks and sends — launch codex lanes with --no-daemon (ac serve codex adds it)", err)}
	}
	if len(daemons) == 0 {
		return doctorCheck{Name: name, Severity: severityOK, Message: "no shared codex app-server daemon is running"}
	}
	tokens := poolServeTokens(cfg)
	var rows []string
	problems := 0
	for _, d := range daemons {
		if d.EnvErr != nil {
			problems++
			rows = append(rows, fmt.Sprintf("pid %d: could not read its environment (%v)", d.PID, d.EnvErr))
			continue
		}
		token := d.Env["AGENTCHUTE_SERVE_TOKEN"]
		repo := d.Env["AGENTCHUTE_CONTROL_REPO"]
		var faults []string
		if token == "" {
			faults = append(faults, "no AGENTCHUTE_SERVE_TOKEN")
		} else if _, live := tokens[token]; !live {
			faults = append(faults, "token mismatch: no live serve.claim in this pool carries it")
		}
		if repo != "" && repo != cfg.ControlRepo {
			faults = append(faults, "control repo mismatch: daemon has "+repo)
		}
		sort.Strings(faults)
		row := fmt.Sprintf("pid %d: AGENTCHUTE_SERVE_TOKEN=%s AGENTCHUTE_CONTROL_REPO=%s", d.PID, tokenPrefix(token), repo)
		if len(faults) > 0 {
			problems++
			row += " — " + strings.Join(faults, "; ")
		} else {
			row += " (matches " + tokens[token] + "'s live serve)"
		}
		rows = append(rows, row)
	}
	table := strings.Join(rows, "; ")
	if problems == 0 {
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("shared codex app-server daemon is pinned to this pool: %s. Hooks of every codex session on this host still run under that one env; --no-daemon (ac serve codex adds it) avoids the daemon entirely", table)}
	}
	return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("shared codex app-server daemon is not pinned to this pool's live serve: %s. Every codex session on this host runs its hooks and shell commands as children of that daemon, inheriting its env — sends fence (token mismatch) and turn-end exits 1. Fix: `codex app-server daemon stop` (ends the sessions it hosts), then relaunch each codex lane with --no-daemon (ac serve codex adds it when codex advertises the flag)", table)}
}
