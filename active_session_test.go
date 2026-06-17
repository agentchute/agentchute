package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestActiveSessionPIDFromSkipsHookShell(t *testing.T) {
	lookup := func(pid int) (processInfo, bool) {
		switch pid {
		case 10:
			return processInfo{ParentPID: 20, Command: "sh"}, true
		case 20:
			return processInfo{ParentPID: 30, Command: "/usr/local/bin/codex"}, true
		default:
			return processInfo{}, false
		}
	}
	if got := activeSessionPIDFrom(10, lookup); got != 20 {
		t.Fatalf("activeSessionPIDFrom shell parent = %d, want wrapper pid 20", got)
	}
}

func TestActiveSessionPIDFromKeepsNonShellParent(t *testing.T) {
	lookup := func(pid int) (processInfo, bool) {
		return processInfo{ParentPID: 20, Command: "/usr/local/bin/codex"}, true
	}
	if got := activeSessionPIDFrom(10, lookup); got != 10 {
		t.Fatalf("activeSessionPIDFrom non-shell parent = %d, want 10", got)
	}
}

func TestActiveSessionPIDFromFallsBackWhenLookupFails(t *testing.T) {
	lookup := func(pid int) (processInfo, bool) {
		return processInfo{}, false
	}
	if got := activeSessionPIDFrom(10, lookup); got != 0 {
		t.Fatalf("activeSessionPIDFrom lookup failure = %d, want pid 0", got)
	}
}

func TestActiveSessionPIDFromUnknownAncestryReturnsZero(t *testing.T) {
	lookup := func(pid int) (processInfo, bool) {
		return processInfo{ParentPID: 1, Command: "launchd"}, true
	}
	if got := activeSessionPIDFrom(10, lookup); got != 0 {
		t.Fatalf("activeSessionPIDFrom unknown ancestry = %d, want pid 0", got)
	}
}

func TestActiveSessionAliveAllowsRecentHeartbeatOnlyTemporarily(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	session := &loop.ActiveSession{
		AgentID:  "codex-agentchute",
		Host:     localHostname(),
		LastSeen: now.Add(-30 * time.Second),
	}
	if !activeSessionAliveAt(session, now) {
		t.Fatal("recent active-session heartbeat should prove liveness")
	}
	session.LastSeen = now.Add(-(activeSessionMaxAge + time.Second))
	if activeSessionAliveAt(session, now) {
		t.Fatal("stale active-session heartbeat without a live pid should not prove liveness")
	}
}

func TestSelfCheckViaShellPrefersRunnerPID(t *testing.T) {
	root := setupBootFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	runnerPID := os.Getpid()
	helper := shellQuote(os.Args[0]) + " -test.run=TestActiveSessionSelfCheckHelperProcess --" +
		" --as codex --vendor openai --control-repo " + shellQuote(root) +
		" --loop-dir " + shellQuote(filepath.Join(root, ".examplecorp", "loop")) +
		" --quiet"
	cmd := exec.Command("sh", "-c", helper)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"AGENTCHUTE_SELF_CHECK_HELPER=1",
		"AGENTCHUTE_RUNNER=1",
		"AGENTCHUTE_RUNNER_PID="+strconv.Itoa(runnerPID),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("self-check helper err = %v\n%s", err, out)
	}
	session, err := loop.LoadActiveSession(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if session.PID != runnerPID {
		t.Fatalf("session PID = %d, want runner pid %d", session.PID, runnerPID)
	}
}

func TestActiveSessionSelfCheckHelperProcess(t *testing.T) {
	if os.Getenv("AGENTCHUTE_SELF_CHECK_HELPER") != "1" {
		return
	}
	idx := -1
	for i, arg := range os.Args {
		if arg == "--" {
			idx = i
			break
		}
	}
	if idx < 0 {
		fmt.Fprintln(os.Stderr, "missing --")
		os.Exit(2)
	}
	if err := cmdSelfCheck(os.Args[idx+1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
