package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func clearMuseIdentityEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AGENTCHUTE_AGENT_ID", "AGENTCHUTE_CONTROL_REPO", "AGENTCHUTE_LOOP_DIR", "AGENTCHUTE_SERVE_TOKEN", "AGENTCHUTE_GUARD", "AGENTCHUTE_RUNNER", "AGENTCHUTE_RUNNER_PID", "AGENTCHUTE_RECOVERED_VENDOR", "MUSE_SESSION_ID", "MUSE_THREAD_ID"} {
		t.Setenv(key, "")
	}
}

func museIdentityFixture(t *testing.T) (string, *loop.Config, *loop.ServeLease, string) {
	t.Helper()
	root, cfg, lease := liveBob(t)
	clearMuseIdentityEnv(t)
	remove, err := writeServeIdentity(cfg, runnerOptions{AgentID: "bob", Vendor: "meta", Guarded: true, WrapperArgs: []string{"muse"}}, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	dir, _ := serveIdentityDir()
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	// The test process is a hook, with a shell and Muse between it and a
	// live parent PID standing in for serve. Keep the actual lease fenced.
	parent := os.Getppid()
	claim, err := loop.ReadServeClaim(cfg, "bob")
	if err != nil {
		t.Fatal(err)
	}
	claim.PID = parent
	data, _ := json.Marshal(claim)
	mustWrite(t, filepath.Join(cfg.AgentStateDir("bob"), "serve.claim"), data)
	parentPath := filepath.Join(dir, strconv.Itoa(parent)+".json")
	if err := os.Rename(path, parentPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parentPath) })
	fakeAncestry(t, 41000, 41001, parent)
	fakeMuseProcesses(t, 41001)
	return root, cfg, lease, parentPath
}

func fakeMuseProcesses(t *testing.T, musePIDs ...int) {
	t.Helper()
	orig := serveIdentityArgv
	t.Cleanup(func() { serveIdentityArgv = orig })
	serveIdentityArgv = func(pid int) ([]string, error) {
		for _, musePID := range musePIDs {
			if pid == musePID {
				return []string{"/bin/muse-bin-1.4.7-R6101.1"}, nil
			}
		}
		return []string{"/bin/sh"}, nil
	}
}

func TestMuseIdentityMarkerAndEnvironmentPrecedence(t *testing.T) {
	for _, marker := range []string{"", "MUSE_SESSION_ID", "MUSE_THREAD_ID", "--muse-hook"} {
		t.Run(marker, func(t *testing.T) {
			root, cfg, lease, _ := museIdentityFixture(t)
			if marker != "" && marker != "--muse-hook" {
				t.Setenv(marker, "session")
			}
			if err := recoverServeIdentity(marker == "--muse-hook"); err != nil {
				t.Fatal(err)
			}
			if marker == "" {
				if os.Getenv("AGENTCHUTE_AGENT_ID") != "" {
					t.Fatal("no-marker command adopted a runner identity")
				}
				return
			}
			if os.Getenv("AGENTCHUTE_AGENT_ID") != "bob" || os.Getenv("AGENTCHUTE_CONTROL_REPO") != root || os.Getenv("AGENTCHUTE_LOOP_DIR") != cfg.LoopDir || resolveGuardSession() != lease.Token {
				t.Fatal("identity, pool or guard fence was not recovered")
			}
			t.Setenv("AGENTCHUTE_AGENT_ID", "explicit-env")
			t.Setenv("AGENTCHUTE_CONTROL_REPO", "environment-pool")
			t.Setenv("AGENTCHUTE_SERVE_TOKEN", "environment-token")
			if err := recoverServeIdentity(true); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("AGENTCHUTE_CONTROL_REPO") != "environment-pool" || os.Getenv("AGENTCHUTE_SERVE_TOKEN") != "environment-token" {
				t.Fatal("recovery changed explicit environment")
			}
			if id, _ := resolveAgentIDRaw("flag-id"); id != "flag-id" {
				t.Fatalf("flag lost precedence: %q", id)
			}
		})
	}
}

func TestMuseIdentityAncestrySelectsNearestLiveRecord(t *testing.T) {
	_, _, _, path := museIdentityFixture(t)
	parent := os.Getppid()
	t.Setenv("MUSE_SESSION_ID", "session")
	if err := recoverServeIdentity(false); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AGENTCHUTE_RUNNER_PID") != strconv.Itoa(parent) {
		t.Fatal("did not recover ancestor record")
	}
	clearMuseIdentityEnv(t)
	t.Setenv("MUSE_SESSION_ID", "session")
	// A broken nearer record must not fall through to an outer lane.
	mustWrite(t, filepath.Join(filepath.Dir(path), "41000.json"), []byte(`{}`))
	if err := recoverServeIdentity(false); !errors.Is(err, errForeignRunnerEnv) {
		t.Fatalf("invalid nearest record fell through: %v", err)
	}
}

func TestMuseIdentityRejectsStaleAndUnrelatedRecords(t *testing.T) {
	for _, stale := range []string{"unrelated", "token", "age", "dead pid"} {
		t.Run(stale, func(t *testing.T) {
			_, cfg, _, path := museIdentityFixture(t)
			t.Setenv("MUSE_SESSION_ID", "session")
			switch stale {
			case "unrelated", "dead pid":
				other := 2147483647
				if stale == "dead pid" {
					fakeAncestry(t, other)
				}
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), strconv.Itoa(other)+".json")); err != nil {
					t.Fatal(err)
				}
			case "token", "age":
				claim, err := loop.ReadServeClaim(cfg, "bob")
				if err != nil {
					t.Fatal(err)
				}
				if stale == "token" {
					claim.ServeToken = consumeForeignToken
				} else {
					claim.LastSeen = time.Now().Add(-time.Hour)
				}
				data, _ := json.Marshal(claim)
				mustWrite(t, filepath.Join(cfg.AgentStateDir("bob"), "serve.claim"), data)
			}
			if err := runnerAncestryCheck(); !errors.Is(err, errForeignRunnerEnv) {
				t.Fatalf("%s record accepted: %v", stale, err)
			}
			if os.Getenv("AGENTCHUTE_AGENT_ID") != "" {
				t.Fatal("failed recovery adopted identity")
			}
		})
	}
}

func TestMuseIdentityRequiresReadableDirectMuseAncestor(t *testing.T) {
	for _, name := range []string{"/bin/muse", "/bin/muse-bin-1.4.7-R6101.1", "/bin/claude", "unreadable"} {
		t.Run(name, func(t *testing.T) {
			_, _, _, _ = museIdentityFixture(t)
			serveIdentityArgv = func(pid int) ([]string, error) {
				if name == "unreadable" {
					return nil, errors.New("process vanished")
				}
				if pid == 41001 {
					return []string{name}, nil
				}
				return []string{"/bin/sh"}, nil
			}
			err := recoverServeIdentity(true)
			wantOK := strings.HasPrefix(filepath.Base(name), "muse")
			if (err == nil) != wantOK {
				t.Fatalf("recovery with %s: %v", name, err)
			}
			if !wantOK && (os.Getenv("AGENTCHUTE_AGENT_ID") != "" || !errors.Is(err, errForeignRunnerEnv)) {
				t.Fatal("unverified ancestry adopted the parent lane")
			}
		})
	}
}

func TestMuseMissingIdentityCommandPolicies(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	clearMuseIdentityEnv(t)
	t.Setenv("MUSE_SESSION_ID", "session")
	fakeAncestry(t, 41000)
	fakeMuseProcesses(t)
	withCwd(t, root, func() {
		for _, tc := range []struct {
			name string
			run  func() error
		}{
			{"check", func() error { return cmdCheck([]string{"--as", "bob"}) }},
			{"ack", func() error { return cmdAck([]string{"--as", "bob"}) }},
			{"turn-end", func() error { return cmdTurnEnd([]string{"--as", "bob", "--muse-hook", "Stop"}) }},
			{"send", func() error { return cmdSend([]string{"--from", "bob", "--to", "alice", "--body", "forbidden"}) }},
		} {
			if err := tc.run(); !errors.Is(err, errForeignRunnerEnv) {
				t.Fatalf("%s did not fail closed: %v", tc.name, err)
			}
		}
		before := string(mustRead(t, filepath.Join(cfg.AgentsDir(), "bob.md")))
		for _, run := range []func() error{
			func() error { return cmdSelfCheck([]string{"--quiet"}) },
			func() error { return cmdPending([]string{"--muse-hook", "UserPromptSubmit"}) },
			func() error { return cmdBoot([]string{"--muse-hook", "SessionStart"}) },
			func() error {
				return runGuardHook([]string{"--pre-tool-use", "--muse-hook", "PreToolUse"}, strings.NewReader(`{"tool_name":"bash","tool_input":{"command":"curl example.com"}}`))
			},
		} {
			_, stderr, err := captureStdoutStderr(t, run)
			if err != nil || strings.Count(strings.TrimSpace(stderr), "\n") != 0 || !strings.Contains(stderr, "Muse identity unavailable") {
				t.Fatalf("fail-open hook: err=%v stderr=%q", err, stderr)
			}
		}
		if after := string(mustRead(t, filepath.Join(cfg.AgentsDir(), "bob.md"))); before != after {
			t.Fatal("fail-open hook rewrote another lane's registration")
		}
	})
}

func TestServeIdentityPrivateWriteSweepAndRemove(t *testing.T) {
	_, cfg, lease, path := museIdentityFixture(t)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar permissions: %v %v", info, err)
	}
	stale := filepath.Join(filepath.Dir(path), "2147483647.json")
	mustWrite(t, stale, []byte(`{}`))
	sweepServeIdentities(filepath.Dir(path))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("dead PID record was not swept")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("live PID record was swept")
	}
	remove, err := writeServeIdentity(cfg, runnerOptions{AgentID: "bob", Vendor: "meta", WrapperArgs: []string{"muse"}}, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	remove()
	selfPath := filepath.Join(filepath.Dir(path), strconv.Itoa(os.Getpid())+".json")
	if _, err := os.Stat(selfPath); !os.IsNotExist(err) {
		t.Fatalf("sidecar not removed: %v", err)
	}
}

func TestMuseNestedStopCannotCommitParentLane(t *testing.T) {
	for _, mode := range []string{"inherited marker", "explicit Muse hook"} {
		t.Run(mode, func(t *testing.T) {
			_, cfg, _, _ := museIdentityFixture(t)
			t.Setenv("MUSE_SESSION_ID", "parent-session")
			if _, err := captureStdout(t, func() error { return cmdCheck(nil) }); err != nil {
				t.Fatal(err)
			}
			clearMuseIdentityEnv(t)
			// hook -> nested wrapper -> tool shell -> lane Muse -> serve.
			fakeAncestry(t, 42000, 42001, 41001, os.Getppid())
			args := []string{"--json"}
			if mode == "explicit Muse hook" {
				fakeMuseProcesses(t, 42000, 41001)
				args = []string{"--muse-hook", "Stop"}
			} else {
				// Match S7's nested ac launch: every lane identity key is gone.
				t.Setenv("AGENTCHUTE_RUNNER_PID", "12345")
				t.Setenv("MUSE_SESSION_ID", "parent-session")
				dropNestedLaneIdentity("nested-wrapper")
			}
			_, err := captureStdout(t, func() error { return cmdTurnEnd(args) })
			if err == nil || os.Getenv("AGENTCHUTE_AGENT_ID") != "" {
				t.Fatalf("nested wrapper recovered parent identity: %v", err)
			}
			if countMessageFiles(t, cfg.AgentClaimedDir("bob")) != 1 || countMessageFiles(t, cfg.ArchiveDir()) != 0 {
				t.Fatal("nested Stop committed parent mail")
			}
			if _, err := loop.ReadGuardLatch(cfg, "bob"); err != nil {
				t.Fatalf("nested Stop cleared parent latch: %v", err)
			}
		})
	}
}

func TestNonMuseServeDoesNotNeedOrWriteIdentity(t *testing.T) {
	for _, wrapper := range []string{"claude", "codex", "gemini", "agy", "grok"} {
		t.Run(wrapper, func(t *testing.T) {
			t.Setenv("HOME", "")
			remove, err := writeServeIdentity(nil, runnerOptions{WrapperArgs: []string{wrapper}}, "token")
			if err != nil {
				t.Fatalf("non-Muse launch depends on HOME: %v", err)
			}
			remove()
		})
	}
}

func TestMuseScrubbedHookRoundTripUsesRunnerPool(t *testing.T) {
	_, cfg, lease, _ := museIdentityFixture(t)
	foreignCwd := hooksRepoFixture(t)
	withCwd(t, foreignCwd, func() {
		t.Setenv("MUSE_SESSION_ID", "session")
		out, err := captureStdout(t, func() error { return cmdBoot([]string{"--muse-hook", "SessionStart"}) })
		if err != nil || !strings.Contains(out, "bob enrolled (meta)") {
			t.Fatalf("boot %v: %s", err, out)
		}
		if err := os.Remove(filepath.Join(cfg.AgentsDir(), "bob.md")); err != nil {
			t.Fatal(err)
		}
		out, err = captureStdout(t, func() error { return cmdSelfCheck([]string{"--muse-hook", "UserPromptSubmit"}) })
		if err != nil || !strings.Contains(out, "UserPromptSubmit") || !strings.Contains(out, "alice") {
			t.Fatalf("self-check pending context %v: %s", err, out)
		}
		if strings.Contains(out, "not registered") {
			t.Fatal("pending ran before self-repair")
		}
		if _, err := captureStdout(t, func() error { return cmdCheck(nil) }); err != nil {
			t.Fatal(err)
		}
		if latch, err := loop.ReadGuardLatch(cfg, "bob"); err != nil || latch.Session != lease.Token {
			t.Fatalf("claim did not latch the recovered session: %v %+v", err, latch)
		}
		out, err = captureStdout(t, func() error {
			return runGuardHook([]string{"--pre-tool-use", "--muse-hook", "PreToolUse"}, strings.NewReader(`{"tool_name":"bash_input","tool_input":{"input":"curl example.com"}}`))
		})
		if err != nil || !strings.Contains(out, `"permissionDecision":"deny"`) {
			t.Fatalf("latched guard %v: %s", err, out)
		}
		out, err = captureStdout(t, func() error { return cmdTurnEnd([]string{"--muse-hook", "Stop"}) })
		if err != nil || strings.TrimSpace(out) != "{}" {
			t.Fatalf("Stop %v: %s", err, out)
		}
		if countMessageFiles(t, cfg.ArchiveDir()) != 1 || countMessageFiles(t, cfg.AgentClaimedDir("bob")) != 0 {
			t.Fatal("Stop did not commit the recovered lane's mail")
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
			t.Fatalf("Stop left latch: %v", err)
		}
		if countMessageFiles(t, filepath.Join(foreignCwd, ".agentchute", "loop", "agents")) != 0 {
			t.Fatal("hooks registered in cwd's unrelated pool")
		}
	})
}
