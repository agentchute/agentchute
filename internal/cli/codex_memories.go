package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// codex's `memories` feature (stable in 0.161/0.162; off by default, on with
// `[features] memories = true` in ~/.codex/config.toml) runs a background
// "Memory Writing Agent: Phase 2 (Consolidation)" turn INSIDE the session's own
// process: a hidden thread, working directory ~/.codex/memories, that runs
// tools with the lane's full environment. On 2026-10-08 one such thread in the
// live codex lane ran `agentchute send --from codex …` and delivered a SHIP
// verdict on PR #216 that the visible review thread never wrote (it later sent
// FIX). Evidence: claude-code, ~/.codex/logs_2.sqlite rows 68345106 and
// 68345850, turn_trigger=memory_consolidation. Same class as the shared daemon
// (codex_daemon.go): hidden execution under the lane's identity. But it runs
// in the lane's own process, so the runner-ancestry check cannot see it.
//
// Three layers: serve launches codex lanes with `--disable memories` (codex:
// "Equivalent to `-c features.memories=false`"); the guard refuses bus commands
// from a codex thread whose working directory is outside the control repo
// (evaluateCodexThreadCwd); doctor's codex_memories check warns when a running
// lane still has the feature on.

const (
	codexMemoriesFeature   = "memories"
	codexDisableFlag       = "--disable"
	codexEnableFlag        = "--enable"
	codexMemoriesConfigKey = "features.memories"
	codexFeaturesCommand   = "features"
	codexUnknownFeatureErr = "Unknown feature flag"
)

// codexMemoriesExplicit reports whether the operator's argv already decides
// the memories feature, either way: `--disable memories` or `--enable
// memories` (each also as `--flag=memories`), or a `-c`/`--config` override
// of features.memories in any spelling codex's parser takes (`-c k=v`,
// `-ck=v`, `-c=k=v`, `--config k=v`, `--config=k=v`). An explicit choice wins:
// serve adds nothing. Any position counts (codex takes these after a
// subcommand too); scanning stops at `--`, after which everything is prompt.
func codexMemoriesExplicit(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := ""
		if i+1 < len(args) {
			value = args[i+1]
		}
		switch {
		case a == "--":
			return false
		case a == codexDisableFlag || a == codexEnableFlag:
			if value == codexMemoriesFeature {
				return true
			}
			i++
		case a == codexDisableFlag+"="+codexMemoriesFeature || a == codexEnableFlag+"="+codexMemoriesFeature:
			return true
		case a == "-c" || a == "--config":
			if codexConfigSetsMemories(value) {
				return true
			}
			i++
		case strings.HasPrefix(a, "--config="):
			if codexConfigSetsMemories(strings.TrimPrefix(a, "--config=")) {
				return true
			}
		case strings.HasPrefix(a, "-c"):
			if codexConfigSetsMemories(strings.TrimPrefix(strings.TrimPrefix(a, "-c"), "=")) {
				return true
			}
		}
	}
	return false
}

// codexConfigSetsMemories reports whether one `-c key=value` override sets
// features.memories, directly or through an inline `features` table.
func codexConfigSetsMemories(kv string) bool {
	key, value, ok := strings.Cut(kv, "=")
	if !ok {
		return false
	}
	switch strings.TrimSpace(key) {
	case codexMemoriesConfigKey:
		return true
	case "features":
		return strings.Contains(value, codexMemoriesFeature)
	}
	return false
}

// codexMemoriesArgs is the pair serve inserts.
var codexMemoriesArgs = []string{codexDisableFlag, codexMemoriesFeature}

// ensureCodexMemoriesOff mirrors ensureCodexNoDaemon: when the real wrapper is
// codex, the operator did not decide the feature on the command line, and the
// installed codex takes `--disable memories` (probe), the pair is inserted
// directly after argv[0], a top-level option ahead of any subcommand. Skipped
// for the daemon-only forms (queue, agents, --remote): their sessions run in a
// daemon this flag cannot reach. Every other case returns args unchanged.
// Pure: the input slice is never mutated.
func ensureCodexMemoriesOff(spec wrapperSpec, args []string, probe func(bin string) bool) []string {
	if spec.Key != "codex" || len(args) == 0 {
		return args
	}
	if codexMemoriesExplicit(args[1:]) || codexNoDaemonIncompatible(args[1:]) {
		return args
	}
	if !probe(args[0]) {
		return args
	}
	out := make([]string, 0, len(args)+len(codexMemoriesArgs))
	out = append(out, args[0])
	out = append(out, codexMemoriesArgs...)
	return append(out, args[1:]...)
}

// codexFeaturesOutput runs `<bin> <args...> features list` under env in dir
// and returns its combined output. A variable so tests can install a fake;
// nothing is cached.
var codexFeaturesOutput = func(bin string, env []string, dir string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexHelpProbeTimeout)
	defer cancel()
	argv := append(append([]string{}, args...), codexFeaturesCommand, "list")
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.WaitDelay = codexHelpProbeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return string(out), err
	}
	return string(out), nil
}

// codexFeatureState reads one feature's effective state from `codex features
// list`: the row whose first column is the name, state in the last column.
func codexFeatureState(out, feature string) (enabled, found bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != feature {
			continue
		}
		switch f[len(f)-1] {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// codexHelpAdvertises reports whether a `codex --help` text lists flag as its
// own word (a lookalike such as `--no-daemon-ish` does not count).
func codexHelpAdvertises(help, flag string) bool {
	for _, word := range strings.Fields(help) {
		if word == flag {
			return true
		}
	}
	return false
}

// codexHelpHasSubcommand reports whether a `codex --help` text lists name in
// its Commands section (clap indents those rows by exactly two spaces).
func codexHelpHasSubcommand(help, name string) bool {
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "  "+name+" ") {
			return true
		}
	}
	return false
}

// codexHelpMemo runs `<bin> --help` at most once per binary for one launch, so
// the --no-daemon and --disable probes share a single help run.
func codexHelpMemo(env []string) func(bin string) (string, bool) {
	cache := map[string]struct {
		out string
		ok  bool
	}{}
	return func(bin string) (string, bool) {
		if c, hit := cache[bin]; hit {
			return c.out, c.ok
		}
		out, err := codexHelpOutput(bin, env)
		cache[bin] = struct {
			out string
			ok  bool
		}{out, err == nil}
		return out, err == nil
	}
}

// codexMemoriesProbe is what serve learned about `--disable memories`.
type codexMemoriesProbe int

const (
	// codexMemoriesOffWorks: the installed codex takes the flag and then
	// reports the feature off.
	codexMemoriesOffWorks codexMemoriesProbe = iota
	// codexMemoriesAbsent: this codex has no memories feature (no
	// --disable, no `features` command, or "Unknown feature flag"), so
	// there is nothing to turn off.
	codexMemoriesAbsent
	// codexMemoriesUnknown: a probe failed; the launch goes ahead without
	// the flag and serve warns.
	codexMemoriesUnknown
)

// probeCodexMemoriesOff checks that the installed codex takes `--disable
// memories`: `codex --help` must advertise --disable and a `features`
// command, and `codex --disable memories features list` must succeed and
// report the feature off. codex refuses to start on an unknown feature name
// ("Error: Unknown feature flag: <name>", exit 1), so a codex that has
// --disable but predates the memories feature must never get the flag. The
// features run happens only after help listed the command: an older codex
// would read `features list` as a prompt and start a session.
func probeCodexMemoriesOff(bin string, env []string, help func(string) (string, bool)) (codexMemoriesProbe, string) {
	out, ok := help(bin)
	if !ok {
		return codexMemoriesUnknown, bin + " --help failed"
	}
	if !codexHelpAdvertises(out, codexDisableFlag) || !codexHelpHasSubcommand(out, codexFeaturesCommand) {
		return codexMemoriesAbsent, ""
	}
	feat, err := codexFeaturesOutput(bin, env, "", codexMemoriesArgs)
	if strings.Contains(feat, codexUnknownFeatureErr) {
		return codexMemoriesAbsent, ""
	}
	if err != nil {
		return codexMemoriesUnknown, fmt.Sprintf("%s %s features list: %v", bin, strings.Join(codexMemoriesArgs, " "), err)
	}
	enabled, found := codexFeatureState(feat, codexMemoriesFeature)
	switch {
	case !found:
		return codexMemoriesAbsent, ""
	case enabled:
		return codexMemoriesUnknown, fmt.Sprintf("%s still reports memories on with %s", bin, strings.Join(codexMemoriesArgs, " "))
	}
	return codexMemoriesOffWorks, ""
}

// ---------- codex lifecycle hooks from a foreign thread ----------

// hookStdin is where the lifecycle hooks (boot, pending, turn-end) read
// their input. Tests replace it rather than swap the process-wide os.Stdin,
// which serve's input copier reads.
var hookStdin = func() *os.File { return os.Stdin }

// readHookStdin reads a hook's JSON input. A terminal is never read, and a
// pipe that stays open is given up on after two seconds.
func readHookStdin(f *os.File) []byte {
	if f == nil {
		return nil
	}
	if info, err := f.Stat(); err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		done <- data
	}()
	select {
	case data := <-done:
		return data
	case <-time.After(2 * time.Second):
		return nil
	}
}

// codexHookFromForeignThread reads a codex lifecycle hook's input (SessionStart,
// UserPromptSubmit, Stop: codex 0.162 requires `cwd` in each) and reports
// whether the event comes from a thread whose working directory is outside
// cfg's control repo. codex memory consolidation runs such a thread inside
// the lane's own process, with the lane's env, in ~/.codex/memories; its
// events reach these hooks too. The caller then does nothing: no
// registration write, no archive, no gate verdict, no context — otherwise
// turn-end commits the lane's claimed mail mid-turn and pending and the gate
// feed "unread mail" into the hidden turn (claude-code and grok reviews of
// #217). Same containment and the same fail-open cases as the guard rule
// (guardPathWithin): no input, no cwd, an unresolvable cwd, or a remote lane.
func codexHookFromForeignThread(cfg *loop.Config) (cwd string, foreign bool) {
	return codexHookForeignCwd(cfg, readHookStdin(hookStdin()))
}

// codexHookForeignCwd is codexHookFromForeignThread over an input already
// read (turn-end reads its input once for this and for stop_hook_active).
func codexHookForeignCwd(cfg *loop.Config, body []byte) (cwd string, foreign bool) {
	cwd = parseGuardHookCwd(body)
	if cwd == "" || cfg == nil || cfg.Remote != nil || cfg.ControlRepo == "" {
		return cwd, false
	}
	return cwd, codexHomeContains(cwd) || !guardPathWithin(cwd, cfg.ControlRepo)
}

// codexHomeContains reports whether dir is inside codex's home ($CODEX_HOME,
// default ~/.codex), where codex runs its memory thread. That counts as a
// foreign thread even when the control repo contains it — a pool at $HOME
// (#222). A missing home or an unresolvable dir answers false, leaving the
// control-repo rule to decide.
func codexHomeContains(dir string) bool {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		home = filepath.Join(userHome, ".codex")
	}
	if _, err := os.Stat(home); err != nil {
		return false
	}
	if _, err := filepath.EvalSymlinks(dir); err != nil {
		return false
	}
	return guardPathWithin(dir, home)
}

// ---------- doctor: codex_memories ----------

// laneProcess is one direct child of a serve process, with its exact argv.
type laneProcess struct {
	PID  int
	Argv []string
}

// listServeChildren returns the direct children of servePID with their exact
// argv. A variable so doctor tests can install a fake process table.
var listServeChildren = scanServeChildren

func scanServeChildren(servePID int) ([]laneProcess, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHelpProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var children []laneProcess
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, perr := strconv.Atoi(f[0])
		ppid, qerr := strconv.Atoi(f[1])
		if perr != nil || qerr != nil || ppid != servePID {
			continue
		}
		// Exact argv from the kernel, never `ps` text: a binary path with a
		// space would split.
		argv, aerr := readProcessArgv(pid)
		if aerr != nil || len(argv) == 0 {
			continue
		}
		children = append(children, laneProcess{PID: pid, Argv: argv})
	}
	return children, nil
}

// codexLaneServePID finds the serve process that runs agentID's wrapper on
// this host: the runner pid in this process's own env when doctor runs inside
// that lane (local and hub lanes alike), else the pid in the local serve
// claim when the claim was written on this host.
func codexLaneServePID(cfg *loop.Config, agentID string) int {
	if strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")) == agentID {
		if pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID"))); err == nil && pid > 0 {
			return pid
		}
	}
	if cfg.Remote != nil {
		return 0
	}
	claim, err := loop.ReadServeClaim(cfg, agentID)
	if err != nil || claim.PID <= 0 || claim.Host != localHostname() {
		return 0
	}
	return claim.PID
}

// codexFeatureArgs returns the feature-affecting options from a codex argv
// (`-c`/`--config`, `--enable`, `--disable`, in every spelling), in order, for
// `codex features list`, which takes those and nothing else of the launch
// line (it refuses --profile). Scanning stops at `--`.
func codexFeatureArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return out
		case a == "-c" || a == "--config" || a == codexEnableFlag || a == codexDisableFlag:
			if i+1 < len(args) {
				out = append(out, a, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, codexEnableFlag+"=") || strings.HasPrefix(a, codexDisableFlag+"="):
			out = append(out, a)
		case strings.HasPrefix(a, "-c") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			out = append(out, "-c", strings.TrimPrefix(a[2:], "="))
		}
	}
	return out
}

// codexArgvDisablesMemories reports whether the argv carries `--disable
// memories` (either spelling) and no `--enable memories`, which codex applies
// after -c overrides.
func codexArgvDisablesMemories(args []string) bool {
	disabled := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if (a == codexDisableFlag || a == codexEnableFlag) && i+1 < len(args) {
			if args[i+1] == codexMemoriesFeature {
				if a == codexEnableFlag {
					return false
				}
				disabled = true
			}
			i++
			continue
		}
		switch a {
		case codexEnableFlag + "=" + codexMemoriesFeature:
			return false
		case codexDisableFlag + "=" + codexMemoriesFeature:
			disabled = true
		}
	}
	return disabled
}

// checkCodexMemories (per-agent) WARNs when agentID's running wrapper is
// codex with the memories feature on: codex then runs hidden consolidation
// threads inside the lane's process under its identity (see the top of this
// file). The running lane is serve's codex child, read from the kernel's
// process table; the feature state is codex's own answer for that argv
// (`codex <its -c/--enable/--disable> features list`), run from the control
// repo. SKIP when there is no codex lane to inspect on this host. Never a
// BLOCKER: a codex setting is not a pool fault.
func checkCodexMemories(cfg *loop.Config, agentID string) doctorCheck {
	const name = "codex_memories"
	servePID := codexLaneServePID(cfg, agentID)
	if servePID == 0 {
		return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("no serve process for %s on this host; nothing to inspect", agentID)}
	}
	children, err := listServeChildren(servePID)
	if err != nil {
		return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("could not list the children of serve pid %d (%v)", servePID, err)}
	}
	var lane *laneProcess
	for i := range children {
		if spec, ok := wrapperSpecForName(filepath.Base(children[i].Argv[0])); ok && spec.Key == "codex" {
			lane = &children[i]
			break
		}
	}
	if lane == nil {
		return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("%s's serve (pid %d) is not running codex", agentID, servePID)}
	}
	bin, args := lane.Argv[0], lane.Argv[1:]
	if codexArgvDisablesMemories(args) {
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("codex pid %d runs with --disable memories", lane.PID)}
	}
	fix := "relaunch with `ac serve codex` (it adds --disable memories), or set `[features] memories = false` in ~/.codex/config.toml"
	help, ok := codexHelpMemo(os.Environ())(bin)
	if !ok {
		return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("could not run %s --help, so codex pid %d's memories state is unknown; it was launched without --disable memories — %s", bin, lane.PID, fix)}
	}
	if !codexHelpHasSubcommand(help, codexFeaturesCommand) {
		return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("%s has no `features` command (no feature flags, so no memories feature)", bin)}
	}
	dir := ""
	if cfg.Remote == nil {
		dir = cfg.ControlRepo
	}
	out, ferr := codexFeaturesOutput(bin, os.Environ(), dir, codexFeatureArgs(args))
	if strings.Contains(out, codexUnknownFeatureErr) {
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("%s has no memories feature", bin)}
	}
	if ferr != nil {
		return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("could not read codex's feature state (%v); codex pid %d was launched without --disable memories — %s", ferr, lane.PID, fix)}
	}
	enabled, found := codexFeatureState(out, codexMemoriesFeature)
	switch {
	case !found:
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("%s has no memories feature", bin)}
	case !enabled:
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("codex reports memories off for pid %d", lane.PID)}
	}
	return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("codex memories are ON for %s (codex pid %d, launched without --disable memories): codex runs hidden memory-consolidation threads inside the lane's process, in ~/.codex/memories, with the lane's identity — on 2026-10-08 one sent a review verdict the lane never wrote. Fix: %s", agentID, lane.PID, fix)}
}
