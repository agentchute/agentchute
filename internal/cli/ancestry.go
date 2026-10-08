package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ancestry.go — does this process run under the serve that pinned its env?
//
// serve pins identity and the fence into its child's env (runnerChildEnv), and
// every lane command trusts that env first: discovery prefers
// AGENTCHUTE_CONTROL_REPO over cwd, the guard session IS the serve token. That
// assumes the wrapper runs its hooks and tool commands in its own process tree.
// codex 0.161 broke it: TUI sessions run through one shared per-user daemon that
// kept the env of the serve which launched codex first, so every codex lane's
// hooks and sends acted as that lane, in that pool (review 2026-10-08, H1).
//
// AGENTCHUTE_RUNNER_PID names the serve. A real lane process descends from it
// (hook → shell → wrapper → serve); a daemon child does not (its parent is
// init/launchd), and neither does a background job orphaned by its shell.

// errForeignRunnerEnv is the refusal: the env names a runner this process does
// not run under.
var errForeignRunnerEnv = errors.New("agentchute: this process carries a runner's env but does not run under that runner")

// errAncestryUnsupported means the platform has no parent lookup; the check is
// skipped rather than guessed.
var errAncestryUnsupported = errors.New("process ancestry is not available on this platform")

// parentPIDOf returns pid's parent. Platform files supply it; tests replace it.
var parentPIDOf = platformParentPID

// maxAncestryHops bounds the walk. Real chains are a handful deep; the bound
// only stops a lookup that cycles.
const maxAncestryHops = 64

// runnerAncestryCheck returns nil when AGENTCHUTE_RUNNER_PID is unset (a
// hand-run session) or is this process or one of its ancestors. It returns
// errForeignRunnerEnv when the walk reaches init without meeting the runner, or
// the value is not a pid. Any other error means the walk could not finish —
// that proves nothing either way, and callers treat it as a warning.
func runnerAncestryCheck() error {
	raw := strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID"))
	if raw == "" {
		return nil
	}
	want, err := strconv.Atoi(raw)
	if err != nil || want <= 0 {
		return fmt.Errorf("%w: AGENTCHUTE_RUNNER_PID=%q is not a pid", errForeignRunnerEnv, raw)
	}
	pid := os.Getpid()
	for hop := 0; hop < maxAncestryHops; hop++ {
		if pid == want {
			return nil
		}
		ppid, err := parentPIDOf(pid)
		if errors.Is(err, errAncestryUnsupported) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot confirm this process runs under AGENTCHUTE_RUNNER_PID=%d: parent of pid %d: %w", want, pid, err)
		}
		if ppid <= 1 || ppid == pid {
			break
		}
		pid = ppid
	}
	return fmt.Errorf("%w: AGENTCHUTE_RUNNER_PID=%d is not an ancestor of this process (pid %d), so its AGENTCHUTE_* env belongs to another lane — a shared daemon or background host started it (codex 0.161: relaunch the lane with --no-daemon); run this from the lane's own session",
		errForeignRunnerEnv, want, os.Getpid())
}

// requireRunnerAncestry is the fail-closed policy, for the commands that
// consume, commit or send as the lane (check, ack, turn-end, send): a foreign
// env refuses; a walk that could not finish only warns, so a platform quirk
// never wedges a lane.
func requireRunnerAncestry(command string) error {
	err := runnerAncestryCheck()
	if err == nil {
		return nil
	}
	if errors.Is(err, errForeignRunnerEnv) {
		return err
	}
	fmt.Fprintf(os.Stderr, "warning: agentchute %s: %v\n", command, err)
	return nil
}

// warnRunnerAncestry is the fail-open policy, for hook commands that must never
// block the harness (guard, pending, self-check): it prints one stderr line and
// reports whether the env is this process's own.
func warnRunnerAncestry(command string) bool {
	err := runnerAncestryCheck()
	if err == nil {
		return true
	}
	fmt.Fprintf(os.Stderr, "warning: agentchute %s: %v\n", command, err)
	return !errors.Is(err, errForeignRunnerEnv)
}
