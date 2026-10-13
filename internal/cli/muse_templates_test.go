package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestMuseWrapperSetupDispatchAndSubmit(t *testing.T) {
	for _, alias := range []string{"muse", "muse-code"} {
		plan, err := parseDispatch([]string{"serve", alias, "--provider", "echo"})
		if err != nil || plan.Wrapper.Key != "muse" || plan.Wrapper.AgentID != "muse" || plan.Wrapper.Vendor != "meta" || !plan.Wrapper.Guarded {
			t.Fatalf("dispatch %s: %+v %v", alias, plan, err)
		}
	}
	if !wrapperIsKnownForSetup("muse") {
		t.Fatal("setup does not know muse")
	}
	if names := shimNamesForAgent("muse"); len(names) != 1 || names[0] != "ac-muse" {
		t.Fatalf("shim names = %v", names)
	}
	if w, ok := hookWrapperForAgent("muse"); !ok || w != "muse" {
		t.Fatalf("doctor wrapper = %q %v", w, ok)
	}
	got := promptInjectionBytes(runnerOptions{AgentID: "custom-meta-lane", WrapperArgs: []string{"/path/muse"}, Prompt: "cue"})
	if string(got) != bracketedPasteStart+"cue"+bracketedPasteEnd+codexEnhancedEnter {
		t.Fatalf("Muse submit = %q", got)
	}
}

func TestMuseTemplateInstallsAndPassesDoctor(t *testing.T) {
	data, err := fs.ReadFile(hooksFS, "examples/hooks/muse/.muse/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var template struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &template); err != nil {
		t.Fatal(err)
	}
	for event, command := range map[string]string{
		"SessionStart":     "boot --muse-hook SessionStart",
		"UserPromptSubmit": "self-check --muse-hook UserPromptSubmit",
		"PreToolUse":       "guard --pre-tool-use --muse-hook PreToolUse",
		"Stop":             "turn-end --muse-hook Stop",
	} {
		groups := template.Hooks[event]
		if len(groups) != 1 || groups[0].Matcher != "*" || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != "agentchute "+command || groups[0].Hooks[0].Timeout != 10 {
			t.Fatalf("invalid %s template: %+v", event, groups)
		}
	}
	if len(template.Hooks) != 4 {
		t.Fatal("unexpected lifecycle or unordered turn-start commands")
	}
	root := hooksRepoFixture(t)
	bin := filepath.Join(t.TempDir(), "agentchute")
	if err := os.Symlink(os.Args[0], bin); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdHooks([]string{"install", "--wrapper", "muse", "--scope", "repo"}) }); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []doctorCheck{checkHookContentSanity(cfg), checkHookFilePresence(cfg, "muse", doctorOptions{Now: time.Now()})} {
		if c.Severity != severityOK {
			t.Fatalf("doctor rejected template: %+v", c)
		}
	}
}

func TestMuseGuardShellInputsAndReminder(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		for _, tc := range []struct {
			body string
			deny bool
		}{
			{`{"tool_name":"bash","tool_input":{"command":"curl example.com"}}`, true},
			{`{"tool_name":"bash_input","tool_input":{"input":["curl example.com"]}}`, true},
			{`{"tool_name":"bash","tool_input":{"extra":{"text":"wget example.com"}}}`, true},
			{`{"tool_name":"bash","tool_input":{"command":"echo ready"}}`, false},
			{`{"tool_name":"submit_reminder_decision","session_id":"different","tool_input":{"advisory_text":"curl is denied","decision":"none"}}`, false},
			{`{"tool_name":"write_file","tool_input":{"path":".muse/hooks.json","content":"{}"}}`, true},
		} {
			out, err := captureStdout(t, func() error {
				return runGuardHook([]string{"--pre-tool-use", "--muse-hook", "PreToolUse"}, strings.NewReader(tc.body))
			})
			if err != nil {
				t.Fatal(err)
			}
			m := decodeJSON(t, out)
			if tc.deny {
				hso, _ := m["hookSpecificOutput"].(map[string]any)
				if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "deny" || hso["permissionDecisionReason"] == "" {
					t.Fatalf("deny shape for %s: %s", tc.body, out)
				}
			} else if len(m) != 0 {
				t.Fatalf("allow shape: %s", out)
			}
		}
	})
}

func TestMuseStopBlocksOnlyFirstAttempt(t *testing.T) {
	root, _, _, _ := museIdentityFixture(t)
	t.Setenv("MUSE_SESSION_ID", "session")
	original := hookStdin
	hookStdin = func() *os.File { return os.Stdin }
	t.Cleanup(func() { hookStdin = original })
	withCwd(t, root, func() {
		for _, active := range []string{"false", "true"} {
			out, err := captureStdout(t, func() error {
				return withStdin(t, `{"stop_hook_active":`+active+`}`, func() error { return cmdTurnEnd([]string{"--muse-hook", "Stop"}) })
			})
			if err != nil {
				t.Fatal(err)
			}
			m := decodeJSON(t, out)
			if active == "false" && (m["decision"] != "block" || m["reason"] == "") {
				t.Fatalf("first Stop must block unread mail: %s", out)
			}
			if active == "true" && len(m) != 0 {
				t.Fatalf("active Stop must not loop: %s", out)
			}
		}
	})
}

func TestServeMuseLaunchesGuardedAndCleansIdentity(t *testing.T) {
	root := setupShortRunFixture(t)
	t.Setenv("HOME", t.TempDir())
	outPath := filepath.Join(root, "identity.json")
	wrapper := filepath.Join(root, "muse")
	mustWrite(t, wrapper, []byte("#!/bin/sh\ncat \"$HOME/.agentchute/serve/$AGENTCHUTE_RUNNER_PID.json\" > "+shellQuote(outPath)+"\n"))
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := cmdServe([]string{"--as", "muse", "--", wrapper}); err != nil {
			t.Fatal(err)
		}
	})
	var identity serveIdentity
	if err := json.Unmarshal(mustRead(t, outPath), &identity); err != nil || identity.AgentID != "muse" || identity.Vendor != "meta" || !identity.Guarded || identity.ServeToken == "" {
		t.Fatalf("runner identity = %+v %v", identity, err)
	}
	dir, _ := serveIdentityDir()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("runner left identity records: %v %v", entries, err)
	}
}
