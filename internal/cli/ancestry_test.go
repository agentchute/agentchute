package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// H1 (review 2026-10-08): serve pins identity and the fence in its child's env,
// and every lane command trusts that env. codex 0.161 ran hooks and tool
// commands under a shared daemon that kept the FIRST serve's env, so they
// acted as a lane they did not belong to. AGENTCHUTE_RUNNER_PID names the serve;
// a process whose ancestry does not reach it is running on someone else's env.

// fakeAncestry replaces the platform parent lookup with a chain rooted at this
// test process: chain[i] is the parent of chain[i-1], and chain[0]'s child is
// os.Getpid(). The last entry's parent is 1 (init/launchd).
func fakeAncestry(t *testing.T, chain ...int) {
	t.Helper()
	parents := map[int]int{}
	child := os.Getpid()
	for _, pid := range chain {
		parents[child] = pid
		child = pid
	}
	parents[child] = 1
	orig := parentPIDOf
	parentPIDOf = func(pid int) (int, error) {
		ppid, ok := parents[pid]
		if !ok {
			return 0, errors.New("no such process")
		}
		return ppid, nil
	}
	t.Cleanup(func() { parentPIDOf = orig })
}

func TestRunnerAncestryCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runnerPID string
		foreign   bool
	}{
		{"unset (hand-run session)", "", false},
		{"runner is the parent", "41000", false},
		{"runner is a grandparent (hook under a shell under the wrapper)", "42000", false},
		{"runner is not an ancestor (shared daemon)", "77777", true},
		{"malformed pid", "not-a-pid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAncestry(t, 41000, 42000)
			t.Setenv("AGENTCHUTE_RUNNER_PID", tc.runnerPID)
			err := runnerAncestryCheck()
			if got := errors.Is(err, errForeignRunnerEnv); got != tc.foreign {
				t.Fatalf("runnerAncestryCheck() = %v, foreign=%v want %v", err, got, tc.foreign)
			}
		})
	}
}

// A lookup that cannot finish proves nothing about foreignness: it is reported,
// but never as errForeignRunnerEnv, so fail-closed callers do not wedge a lane
// on a platform quirk.
func TestRunnerAncestryCheckUnknownIsNotForeign(t *testing.T) {
	orig := parentPIDOf
	parentPIDOf = func(int) (int, error) { return 0, errors.New("sysctl: operation not permitted") }
	t.Cleanup(func() { parentPIDOf = orig })
	t.Setenv("AGENTCHUTE_RUNNER_PID", "77777")
	err := runnerAncestryCheck()
	if err == nil || errors.Is(err, errForeignRunnerEnv) {
		t.Fatalf("runnerAncestryCheck() = %v, want a non-foreign lookup error", err)
	}

	parentPIDOf = func(int) (int, error) { return 0, errAncestryUnsupported }
	if err := runnerAncestryCheck(); err != nil {
		t.Fatalf("unsupported platform: runnerAncestryCheck() = %v, want nil (skipped)", err)
	}
}

// The fail-closed commands refuse BEFORE touching the pool; the fail-open hook
// commands warn on stderr and never block the harness.
func TestForeignRunnerEnvFailsClosedForConsumeAndSend(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "work item"}); err != nil {
			t.Fatal(err)
		}
		fakeAncestry(t, 41000)
		t.Setenv("AGENTCHUTE_RUNNER_PID", "77777")

		for _, tc := range []struct {
			name string
			run  func() error
		}{
			{"send", func() error { return cmdSend([]string{"--from", "bob", "--to", "alice", "--body", "reply"}) }},
			{"check", func() error { return cmdCheck([]string{"--as", "bob"}) }},
			{"check --no-archive", func() error { return cmdCheck([]string{"--as", "bob", "--no-archive"}) }},
			{"ack", func() error { return cmdAck([]string{"--as", "bob"}) }},
			{"turn-end", func() error { return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"}) }},
		} {
			out, err := captureStdout(t, tc.run)
			if !errors.Is(err, errForeignRunnerEnv) {
				t.Fatalf("%s under a foreign runner env = %v, want errForeignRunnerEnv\n%s", tc.name, err, out)
			}
			if strings.Contains(out, "work item") {
				t.Fatalf("%s showed mail to a process outside its runner:\n%s", tc.name, out)
			}
		}
		if got := countDirFiles(t, cfg.AgentInboxDir("bob")); got != 1 {
			t.Fatalf("bob inbox = %d files, want the message untouched", got)
		}
		if got := countDirFiles(t, cfg.AgentInboxDir("alice")); got != 0 {
			t.Fatalf("alice inbox = %d files, want the foreign send refused", got)
		}
	})
}

func TestForeignRunnerEnvFailsOpenForHooks(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		fakeAncestry(t, 41000)
		t.Setenv("AGENTCHUTE_RUNNER_PID", "77777")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")

		regPath := cfg.AgentRegistrationPath("bob")
		before, err := os.ReadFile(regPath)
		if err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct {
			name string
			run  func() error
		}{
			{"guard", func() error { return withStdin(t, "", func() error { return cmdGuard([]string{"--pre-tool-use"}) }) }},
			{"pending", func() error { return cmdPending([]string{"--as", "bob"}) }},
			{"self-check", func() error { return cmdSelfCheck([]string{"--quiet"}) }},
		} {
			var runErr error
			stderr := captureStderr(t, func() {
				_, runErr = captureStdout(t, tc.run)
			})
			if runErr != nil {
				t.Fatalf("%s under a foreign runner env = %v, want it to fail open", tc.name, runErr)
			}
			if !strings.Contains(stderr, "AGENTCHUTE_RUNNER_PID=77777") {
				t.Fatalf("%s printed no foreign-env warning; stderr=%q", tc.name, stderr)
			}
		}

		// The guard fails open for real: a latch this env would match does not
		// turn into a deny, because the latch is not this process's to enforce.
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-foreign")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		if err := loop.SetGuardLatch(cfg, "bob", "tok-foreign"); err != nil {
			t.Fatal(err)
		}
		var guardErr error
		guardOut := ""
		_ = captureStderr(t, func() {
			guardOut, guardErr = captureStdout(t, func() error {
				return withStdin(t, `{"tool_name":"Bash","tool_input":{"command":"agentchute `+`ack --as bob"}}`, func() error {
					return cmdGuard([]string{"--pre-tool-use"})
				})
			})
		})
		if guardErr != nil || strings.TrimSpace(guardOut) != "" {
			t.Fatalf("guard under a foreign runner env = %q, %v; want a silent allow", guardOut, guardErr)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "")
		t.Setenv("AGENTCHUTE_GUARD", "")

		// self-check fails open by NOT writing: repairing a registration from
		// another runner's env is the harm this check exists to stop.
		after, err := os.ReadFile(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("self-check rewrote bob's registration from a foreign runner env")
		}
	})
}

// sanity for the helper itself, so a broken fake cannot make the rows above
// pass vacuously.
func TestFakeAncestryChainReachesInit(t *testing.T) {
	fakeAncestry(t, 41000, 42000)
	pid := os.Getpid()
	var seen []string
	for i := 0; i < 5 && pid > 1; i++ {
		ppid, err := parentPIDOf(pid)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, strconv.Itoa(ppid))
		pid = ppid
	}
	if got := strings.Join(seen, ","); got != "41000,42000,1" {
		t.Fatalf("fake chain = %s", got)
	}
}

// withStdin gives a hook command a stdin holding content, already at EOF after
// it, so a test never blocks on the terminal the test binary inherited.
func withStdin(t *testing.T, content string, fn func() error) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = orig }()
	return fn()
}

// A serve running as PID 1 (a container's init) is a real runner: the walk must
// compare the parent with the runner BEFORE stopping at init (PR #211 gate,
// codex P2). Before the fix every descendant was refused as foreign.
func TestRunnerAncestryCheckRecognizesARunnerAtPIDOne(t *testing.T) {
	fakeAncestry(t, 41000, 1)
	t.Setenv("AGENTCHUTE_RUNNER_PID", "1")
	if err := runnerAncestryCheck(); err != nil {
		t.Fatalf("a child of a PID-1 serve was refused: %v", err)
	}
	t.Setenv("AGENTCHUTE_RUNNER_PID", "77777")
	if err := runnerAncestryCheck(); !errors.Is(err, errForeignRunnerEnv) {
		t.Fatalf("a chain ending at init without the runner = %v, want errForeignRunnerEnv", err)
	}
}
