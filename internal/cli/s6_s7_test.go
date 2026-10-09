package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// ---------- S6: --body-file ----------

// Another pool's serve.claim is refused, not only this pool's: every pool
// keeps its state in <loop>/state.
func TestBodyFileRefusesAnyPoolsStateTree(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	other := t.TempDir()
	for _, path := range []string{
		filepath.Join(other, ".agentchute", "loop", "state", "codex", "serve.claim"),
		filepath.Join(other, "hubs", "h1", ".agentchute", "loop", "state", "bob", "spool", "x.md"),
		filepath.Join(other, ".agentchute", "LOOP", "STATE", "bob", "serve.claim"),
	} {
		mustWrite(t, path, []byte(`{"serve_token":"secret"}`))
		if _, err := readSendBodyFile(cfg, path); err == nil || !strings.Contains(err.Error(), "state/ tree") {
			t.Errorf("--body-file %s: err = %v, want the state-tree refusal", path, err)
		}
	}
	ok := filepath.Join(other, "notes", "reply.md")
	mustWrite(t, ok, []byte("hello"))
	if body, err := readSendBodyFile(cfg, ok); err != nil || body != "hello" {
		t.Fatalf("an ordinary file: body=%q err=%v", body, err)
	}
}

// A path re-pointed between the checks and the open is refused: a symlink
// put in the last component (O_NOFOLLOW), and a DIRECTORY of the checked path
// re-pointed into a state tree (the open follows it; the fstat identity of
// what was opened is not the checked file's). Replacing the file's content at
// the same, allowed place is not a swap into a state tree, and Linux may even
// reuse the inode number for it, so it is not asserted.
func TestBodyFileSwappedBetweenCheckAndOpenIsRefused(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	stateDir := cfg.AgentStateDir("bob")
	claim := filepath.Join(stateDir, "serve.claim")
	mustWrite(t, claim, []byte(`{"serve_token":"secret"}`))
	for _, swap := range []string{"symlink to the claim", "directory re-pointed into a state tree"} {
		t.Run(swap, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "drafts")
			// Named like the claim, in an ordinary directory: allowed at check.
			body := filepath.Join(dir, "serve.claim")
			mustWrite(t, body, []byte("hello"))
			restore := afterSendBodyFileCheck
			t.Cleanup(func() { afterSendBodyFileCheck = restore })
			afterSendBodyFileCheck = func(resolved string) {
				if swap == "symlink to the claim" {
					_ = os.Remove(resolved)
					if err := os.Symlink(claim, resolved); err != nil {
						t.Error(err)
					}
					return
				}
				parent := filepath.Dir(resolved)
				if err := os.Rename(parent, parent+".moved"); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(stateDir, parent); err != nil {
					t.Error(err)
				}
			}
			got, err := readSendBodyFile(cfg, body)
			if err == nil || strings.Contains(got, "secret") {
				t.Fatalf("a swapped body file was read: body=%q err=%v", got, err)
			}
		})
	}
}

// ---------- S7: launches inside a lane ----------

func TestNestedLaneLaunchIsDecidedByTheParent(t *testing.T) {
	restore := laneParentPID
	t.Cleanup(func() { laneParentPID = restore })
	for _, row := range []struct {
		name       string
		runnerPID  string
		parent     int
		nested     bool
		keepsIdent bool
	}{
		{"the runner's own launch", "4242", 4242, false, true},
		{"a launch typed inside the lane", "4242", 999, true, false},
		{"no runner pid: cannot tell, unchanged", "", 999, false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
			t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok")
			t.Setenv("AGENTCHUTE_GUARD", "1")
			t.Setenv("AGENTCHUTE_RUNNER", "1")
			t.Setenv("AGENTCHUTE_RUNNER_PID", row.runnerPID)
			t.Setenv("AGENTCHUTE_CONTROL_REPO", "/pool")
			laneParentPID = func() int { return row.parent }
			if got := nestedLaneLaunch(); got != row.nested {
				t.Fatalf("nestedLaneLaunch = %v, want %v", got, row.nested)
			}
			env := strings.Join(wrapperBypassEnv("test"), "\n")
			for _, k := range []string{"AGENTCHUTE_AGENT_ID=", "AGENTCHUTE_SERVE_TOKEN=", "AGENTCHUTE_GUARD=", "AGENTCHUTE_RUNNER="} {
				if strings.Contains(env, k) != row.keepsIdent {
					t.Errorf("%s present = %v, want %v", k, strings.Contains(env, k), row.keepsIdent)
				}
			}
			if !strings.Contains(env, "AGENTCHUTE_CONTROL_REPO=/pool") {
				t.Error("the pool locator was dropped too")
			}
		})
	}
}

// End to end through the real exec: the shim and the `ac` dispatcher, run as
// a child of this test with the lane env, exec a fake wrapper that records
// its environment. With this test as the runner (the child's parent) the
// identity is kept; with another runner pid it is gone.
func TestWrapperLaunchedInsideALaneDropsTheLaneIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec replacement and shell-script fakes")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, via := range []string{"shim", "dispatch"} {
		for _, row := range []struct {
			name      string
			runnerPID int
			keeps     bool
		}{
			{"runner's own launch", os.Getpid(), true},
			{"typed inside the lane", 1, false},
		} {
			t.Run(via+"/"+row.name, func(t *testing.T) {
				dir := t.TempDir()
				record := filepath.Join(dir, "env")
				bin := filepath.Join(dir, "bin")
				mustWrite(t, filepath.Join(bin, "codex"), []byte("#!/bin/sh\nenv > "+shellQuote(record)+"\n"))
				if err := os.Chmod(filepath.Join(bin, "codex"), 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(self, "-test.run", "^TestS7LaunchHelper$", "-test.count=1")
				cmd.Env = append(os.Environ(),
					"ACTEST_S7_HELPER="+via,
					"ACTEST_S7_RUNNER_PID="+fmt.Sprint(row.runnerPID),
					"PATH="+bin+":/usr/bin:/bin",
				)
				out, err := cmd.CombinedOutput()
				data, rerr := os.ReadFile(record)
				if rerr != nil {
					t.Fatalf("the wrapper did not run: %v\n%s (%v)", rerr, out, err)
				}
				env := string(data)
				for _, k := range []string{"AGENTCHUTE_AGENT_ID=bob", "AGENTCHUTE_SERVE_TOKEN=tok", "AGENTCHUTE_GUARD=1"} {
					if strings.Contains(env, k) != row.keeps {
						t.Errorf("%s in the wrapper's env = %v, want %v", k, strings.Contains(env, k), row.keeps)
					}
				}
			})
		}
	}
}

// TestS7LaunchHelper runs only as the child of the row above. Its lane env
// is set HERE, from ACTEST_* values, so a TestMain that strips AGENTCHUTE_*
// cannot remove it first.
func TestS7LaunchHelper(t *testing.T) {
	via := os.Getenv("ACTEST_S7_HELPER")
	if via == "" {
		t.Skip("helper process only")
	}
	for k, v := range map[string]string{
		"AGENTCHUTE_AGENT_ID":    "bob",
		"AGENTCHUTE_SERVE_TOKEN": "tok",
		"AGENTCHUTE_GUARD":       "1",
		"AGENTCHUTE_RUNNER":      "1",
		"AGENTCHUTE_RUNNER_PID":  os.Getenv("ACTEST_S7_RUNNER_PID"),
	} {
		os.Setenv(k, v)
	}
	var err error
	if via == "shim" {
		err = cmdShimsExec([]string{"--name", "codex", "--shim-dir", t.TempDir()})
	} else {
		err = cmdDispatch([]string{"--shim-dir", t.TempDir(), "--", "serve", "codex"})
	}
	t.Fatalf("exec did not replace the process: %v", err)
}

// A serve typed inside a lane resolves its own id, not the parent's.
func TestServeTypedInsideALaneDoesNotActAsTheLane(t *testing.T) {
	restore := laneParentPID
	t.Cleanup(func() { laneParentPID = restore })
	laneParentPID = func() int { return 999 }
	t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
	t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok")
	t.Setenv("AGENTCHUTE_RUNNER_PID", "4242")
	dropNestedLaneIdentity("serve")
	for _, k := range laneIdentityKeys {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s survived a nested serve's start", k)
		}
	}
	// The runner's own launch is untouched.
	t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
	t.Setenv("AGENTCHUTE_RUNNER_PID", "999")
	dropNestedLaneIdentity("serve")
	if os.Getenv("AGENTCHUTE_AGENT_ID") != "bob" {
		t.Fatal("the runner's own serve lost its identity")
	}
	_ = loop.CurrentProtocolVersion
}

// End to end through cmdServe: a serve whose parent is not the lane's runner
// launches its wrapper under its OWN id (the wrapper's canonical id here),
// never the parent lane's.
func TestServeTypedInsideALaneLaunchesUnderItsOwnID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake wrapper")
	}
	restore := laneParentPID
	t.Cleanup(func() { laneParentPID = restore })
	laneParentPID = func() int { return 999 }
	root := setupShortRunFixture(t)
	record := filepath.Join(root, "child-id")
	wrapper := filepath.Join(root, "codex")
	mustWrite(t, wrapper, []byte("#!/bin/sh\nprintf '%s' \"$AGENTCHUTE_AGENT_ID\" > "+shellQuote(record)+"\n"))
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		// The parent lane's env, as a command typed in its shell sees it.
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "parent-token")
		t.Setenv("AGENTCHUTE_RUNNER", "1")
		t.Setenv("AGENTCHUTE_RUNNER_PID", "4242")
		if err := cmdServe([]string{
			"--control-repo", root,
			"--loop-dir", filepath.Join(root, ".agentchute", "loop"),
			"--interval", "5",
			"--idle-grace", "100ms",
			"--", wrapper,
		}); err != nil {
			t.Fatalf("cmdServe: %v", err)
		}
	})
	if got := string(mustRead(t, record)); got != "codex" {
		t.Fatalf("the nested serve launched its wrapper as %q, want its own id codex (not the parent lane bob)", got)
	}
}
