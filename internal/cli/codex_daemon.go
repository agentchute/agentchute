package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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

// codexHelpProbeWaitDelay bounds how long the probe waits for the help
// process's pipes after it exits: a wrapper whose background child inherits
// stdout would otherwise hold CombinedOutput open past the deadline.
const codexHelpProbeWaitDelay = 2 * time.Second

// codexHelpOutput runs `<bin> --help` under env and returns its combined
// output. A variable so tests can install a fake; nothing is cached — the
// installed codex can change between launches.
var codexHelpOutput = func(bin string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexHelpProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	cmd.Env = env
	cmd.WaitDelay = codexHelpProbeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return string(out), err
	}
	return string(out), nil
}

// codexSupportsNoDaemon reports whether the installed codex advertises
// --no-daemon as a top-level option. The flag must appear as its own word:
// an older codex, a failed probe, or a lookalike flag all answer no, and the
// launch proceeds unchanged (the daemon hazard then remains; doctor's
// codex_daemon_env check reports it). env is the child env the wrapper itself
// will be launched with.
func codexSupportsNoDaemon(bin string, env []string) bool {
	out, ok := codexHelpMemo(env)(bin)
	return ok && codexHelpAdvertises(out, codexNoDaemonFlag)
}

// codexNoDaemonIncompatible reports whether codex 0.161 would refuse the flag
// for these args: the binary's own strings say "--no-daemon cannot be used
// with codex queue" / "... with codex agents" / "... with --remote" (both
// subcommands exist to talk to the daemon). `--remote` is matched anywhere
// (a flag); `queue`/`agents` only in the subcommand position — the first
// token that is not a flag, before any `--` — so `resume queue` or a prompt
// word `queue` does not drop the flag. Known gap: a value-taking top-level
// flag before the subcommand (`--model m queue`) hides it; codex then refuses
// the combination at launch, which is loud, not silent.
func codexNoDaemonIncompatible(args []string) bool {
	if dispatchHasFlag(args, "--remote") {
		return true
	}
	for _, a := range args {
		if a == "--" {
			return false
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a == "queue" || a == "agents"
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
func serveWrapperArgs(args []string, env []string) ([]string, wrapperSpec) {
	out, spec, _ := serveWrapperArgsNoted(args, env)
	return out, spec
}

// serveWrapperArgsNoted is serveWrapperArgs plus the reason, when there is
// one, that the memories probe could not confirm `--disable memories` (serve
// warns with it). --no-daemon is decided first, against the operator's argv
// exactly as typed; `--disable memories` (codex_memories.go) second. One
// `codex --help` run serves both probes.
func serveWrapperArgsNoted(args []string, env []string) ([]string, wrapperSpec, string) {
	if len(args) == 0 {
		return args, wrapperSpec{}, ""
	}
	spec, ok := wrapperSpecForName(filepath.Base(args[0]))
	if !ok {
		return args, wrapperSpec{}, ""
	}
	help := codexHelpMemo(env)
	noDaemon := func(bin string) bool {
		out, ok := help(bin)
		return ok && codexHelpAdvertises(out, codexNoDaemonFlag)
	}
	note := ""
	memoriesOff := func(bin string) bool {
		result, why := probeCodexMemoriesOff(bin, env, help)
		if result == codexMemoriesUnknown {
			note = why
		}
		return result == codexMemoriesOffWorks
	}
	out := ensureCodexNoDaemon(spec, args, noDaemon)
	return ensureCodexMemoriesOff(spec, out, memoriesOff), spec, note
}

// applyCodexLaunchArgs is serve's launch-time argv step, run only AFTER the
// serve lease is admitted (both local and remote paths) and with the child's
// own env, so a duplicate lane never executes the wrapper's --help (or
// `features list`) before being refused, and the probes see what the wrapper
// will see. Only a probe that said no warns: a deliberate skip (queue,
// agents, --remote, an operator's own memories choice) is not a missing flag,
// and a codex with no memories feature has nothing to turn off.
func applyCodexLaunchArgs(args []string, env []string) []string {
	out, spec, memoriesNote := serveWrapperArgsNoted(args, env)
	if spec.Key == "codex" && !dispatchHasFlag(out[1:], codexNoDaemonFlag) && !codexNoDaemonIncompatible(out[1:]) {
		fmt.Fprintf(os.Stderr, "warning: %s does not advertise %s; its shared app-server daemon hosts hooks under the FIRST serve's env (see doctor's codex_daemon_env)\n", out[0], codexNoDaemonFlag)
	}
	if memoriesNote != "" {
		fmt.Fprintf(os.Stderr, "warning: could not confirm that %s takes --disable memories (%s); launching without it. If codex memories are on, hidden consolidation threads run under this lane's identity (see doctor's codex_memories)\n", out[0], memoriesNote)
	}
	return out
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
		daemons[i].Env, daemons[i].EnvErr = readProcessEnv(daemons[i].PID)
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

// readProcessEnv returns the AGENTCHUTE_* environment of pid from the exact
// NUL-separated source the OS keeps — /proc/<pid>/environ on Linux,
// sysctl kern.procargs2 on macOS (codex_daemon_darwin.go) — never from `ps`
// text, which word-splits a value containing a space (a control repo named
// `Tmux workflow` would read as a mismatch). An empty map with a nil error is
// a readable environment with no agentchute keys.
func readProcessEnv(pid int) (map[string]string, error) {
	data, err := readProcessEnvBlock(pid)
	if err != nil {
		return nil, err
	}
	return parseProcessEnvBytes(data), nil
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

// parseProcArgs2Env extracts the environment block from a kern.procargs2
// buffer: a native-endian int32 argc, the executable path, NUL padding, argc
// NUL-terminated argv strings, then the NUL-terminated environment strings.
func parseProcArgs2Env(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("kern.procargs2: %d bytes, no argc", len(data))
	}
	argc := int(binary.NativeEndian.Uint32(data[:4]))
	i := 4
	for i < len(data) && data[i] != 0 { // executable path
		i++
	}
	for i < len(data) && data[i] == 0 { // padding
		i++
	}
	for n := 0; n < argc && i < len(data); n++ { // argv
		for i < len(data) && data[i] != 0 {
			i++
		}
		i++ // the terminator
	}
	if i > len(data) {
		i = len(data)
	}
	return data[i:], nil
}

// parseProcArgs2Argv extracts argv from a kern.procargs2 buffer (layout as
// in parseProcArgs2Env).
func parseProcArgs2Argv(data []byte) ([]string, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("kern.procargs2: %d bytes, no argc", len(data))
	}
	argc := int(binary.NativeEndian.Uint32(data[:4]))
	i := 4
	for i < len(data) && data[i] != 0 { // executable path
		i++
	}
	for i < len(data) && data[i] == 0 { // padding
		i++
	}
	argv := make([]string, 0, argc)
	for n := 0; n < argc && i < len(data); n++ {
		j := i
		for j < len(data) && data[j] != 0 {
			j++
		}
		argv = append(argv, string(data[i:j]))
		i = j + 1
	}
	if len(argv) != argc {
		return nil, fmt.Errorf("kern.procargs2: argc %d but %d strings", argc, len(argv))
	}
	return argv, nil
}

// splitNULArgv splits a /proc/<pid>/cmdline block: NUL-terminated strings.
func splitNULArgv(data []byte) []string {
	data = bytes.TrimSuffix(data, []byte{0})
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, len(parts))
	for i, p := range parts {
		argv[i] = string(p)
	}
	return argv
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

// checkCodexDaemonEnv reports whether a running shared codex daemon carries
// THIS lane's live serve token and this pool's control repo. A daemon pinned
// elsewhere hosts every codex session's hooks and shell commands under a stale
// or foreign identity (the hazard above). Without --as there is no lane to
// pin against, so the check only lists what it found (SKIP): a token that
// matches some other lane's claim is exactly the cross-lane false-OK codex
// reproduced (codex vs codex-l2). Never BLOCKER: the daemon is a per-user host
// condition outside the pool, and an unreadable process table or environment
// must not fail doctor for the other lanes. No token characters are ever
// printed — only which lane's live claim a token matched, if any.
func checkCodexDaemonEnv(cfg *loop.Config, agentID string) doctorCheck {
	const name = "codex_daemon_env"
	daemons, err := listCodexDaemons()
	if err != nil {
		return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("could not enumerate codex app-server daemons (%v); a shared daemon pinned to another serve breaks codex hooks and sends — launch codex lanes with --no-daemon (ac serve codex adds it)", err)}
	}
	if len(daemons) == 0 {
		return doctorCheck{Name: name, Severity: severityOK, Message: "no shared codex app-server daemon is running"}
	}
	tokens := poolServeTokens(cfg)
	laneToken := ""
	if agentID != "" {
		if claim, cerr := loop.ReadServeClaim(cfg, agentID); cerr == nil {
			laneToken = claim.ServeToken
		}
	}
	var rows []string
	problems := 0
	for _, d := range daemons {
		if d.EnvErr != nil {
			problems++
			rows = append(rows, fmt.Sprintf("pid %d: could not read its environment (%v)", d.PID, d.EnvErr))
			continue
		}
		token := d.Env["AGENTCHUTE_SERVE_TOKEN"]
		repo, hasRepo := d.Env["AGENTCHUTE_CONTROL_REPO"]
		owner, ownerLive := tokens[token]
		var faults []string
		tokenDesc := "token matches no live serve.claim in this pool"
		switch {
		case token == "":
			tokenDesc = "no AGENTCHUTE_SERVE_TOKEN"
			faults = append(faults, tokenDesc)
		case agentID == "":
			if ownerLive {
				tokenDesc = "token matches " + owner + "'s live serve"
			}
		case laneToken == "":
			if ownerLive {
				tokenDesc = "token matches " + owner + "'s live serve"
			}
			faults = append(faults, agentID+" has no live serve.claim to match")
		case token == laneToken:
			tokenDesc = "token matches " + agentID + "'s live serve"
		default:
			if ownerLive {
				tokenDesc = "token matches " + owner + "'s live serve, not " + agentID + "'s"
			}
			faults = append(faults, "token mismatch: not "+agentID+"'s live serve token")
		}
		repoDesc := "AGENTCHUTE_CONTROL_REPO=" + repo
		switch {
		case !hasRepo || repo == "":
			repoDesc = "no AGENTCHUTE_CONTROL_REPO"
			faults = append(faults, repoDesc)
		case repo != cfg.ControlRepo:
			faults = append(faults, "control repo mismatch: daemon has "+repo)
		}
		sort.Strings(faults)
		row := fmt.Sprintf("pid %d: %s, %s", d.PID, tokenDesc, repoDesc)
		if len(faults) > 0 {
			problems++
			row += " — " + strings.Join(faults, "; ")
		}
		rows = append(rows, row)
	}
	table := strings.Join(rows, "; ")
	if agentID == "" {
		return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("shared codex app-server daemon running: %s. No --as / $AGENTCHUTE_AGENT_ID, so it cannot be verified against a specific lane's serve token; run doctor --as <codex lane>", table)}
	}
	if problems == 0 {
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("shared codex app-server daemon is pinned to %s's live serve in this pool: %s. Hooks of every codex session on this host still run under that one env; --no-daemon (ac serve codex adds it) avoids the daemon entirely", agentID, table)}
	}
	return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("shared codex app-server daemon is not pinned to %s's live serve: %s. Every codex session on this host runs its hooks and shell commands as children of that daemon, inheriting its env — sends fence (token mismatch) and turn-end exits 1. Fix: `codex app-server daemon stop` (ends the sessions it hosts), then relaunch each codex lane with --no-daemon (ac serve codex adds it when codex advertises the flag)", agentID, table)}
}
