package cli

import (
	"encoding/json"
	"fmt"
	"github.com/agentchute/agentchute/internal/loop"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var retiredTemplateRuleFixtures = []string{
	"Bash(agentchute send:*)", "Bash(go test:*)", "Bash(go build:*)", "Bash(go vet:*)",
}

func TestClaudeGitAutoAllowRejectsQuotedFlags(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	permissions := decodeSettings(t, tmpl)["permissions"].(map[string]any)
	allow := stringList(permissions["allow"])
	for _, command := range []string{
		"git diff '--output=x'", "git diff \"--output=x\"", "git diff\t--output=x",
		"git diff --out\"\"put=x", "git log '--output=x'", "git show '--output=x'",
		"git fetch '--upload-pack=unsafe' origin", "git fetch\t--upload-pack=unsafe origin",
	} {
		for _, rule := range allow {
			if claudeBashRuleMatches(rule, command) {
				t.Errorf("dangerous command %q is auto-approved by %q", command, rule)
			}
		}
	}
	if ask, ok := permissions["ask"].([]any); ok && len(ask) > 0 {
		t.Fatal("ask rules would prompt in bypass mode")
	}
}

func TestGuardHookReadsLargeInputAndRejectsMalformedInput(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_RUNNER_PID", "")
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "input-session")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		if err := loop.SetGuardLatch(cfg, "bob", "input-session"); err != nil {
			t.Fatal(err)
		}
		bash, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": guardPipelineDenySubstrings[0] + " # " + strings.Repeat("x", 2<<20)}})
		write, _ := json.Marshal(map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": "notes.md", "content": strings.Repeat("x", 2<<20)}})
		for _, row := range []struct {
			name, body string
			deny       bool
		}{
			{"large denied Bash", string(bash), true},
			{"large ordinary Write", string(write), false},
			{"truncated JSON", `{"tool_name":"Bash","tool_input":`, true},
			{"invalid argument shape", `{"tool_name":"Bash","tool_input":"not an object"}`, true},
			{"non-empty garbage", "garbage", true},
			{"no input", "", false},
		} {
			t.Run(row.name, func(t *testing.T) {
				out, err := captureStdout(t, func() error {
					return runGuardHook([]string{"--pre-tool-use", "--as", "bob"}, strings.NewReader(row.body))
				})
				if err != nil {
					t.Fatal(err)
				}
				if denied := strings.Contains(out, `"permissionDecision":"deny"`); denied != row.deny {
					t.Fatalf("denied=%v want=%v output=%s", denied, row.deny, out)
				}
			})
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "foreign-session")
		out, err := captureStdout(t, func() error {
			return runGuardHook([]string{"--pre-tool-use", "--as", "bob"}, strings.NewReader("garbage"))
		})
		if err != nil || out != "" {
			t.Fatalf("malformed input activated a foreign latch: %v %s", err, out)
		}
	})
}

func TestGuardDataIsNotAWrite(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "review"); err != nil {
		t.Fatal(err)
	}
	hook := claudeHook(t).Dest
	rows := []struct {
		name, tool string
		input      map[string]any
	}{
		{"Write literal path", "Write", map[string]any{"file_path": "/repo/docs.md", "content": hook}},
		{"Write prose ending in path", "Write", map[string]any{"file_path": "/repo/docs.md", "content": "Inspect /repo/" + hook}},
		{"Edit literal path", "Edit", map[string]any{"file_path": "/repo/docs.md", "old_string": "x", "new_string": hook}},
		{"MultiEdit nested content", "MultiEdit", map[string]any{"file_path": "/repo/docs.md", "edits": []any{map[string]any{"old_string": "x", "new_string": hook}}}},
		{"Read hook", "Read", map[string]any{"file_path": "/repo/" + hook}},
		{"MCP read hook", "mcp__fs__read_file", map[string]any{"path": "/repo/" + hook}},
		{"MCP query", "mcp__search__search", map[string]any{"query": hook}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"tool_name": r.tool, "tool_input": r.input})
			if d := evaluateGuardDecisionFor(cfg, "bob", "review", parseGuardToolUse(body)); !d.Allowed {
				t.Errorf("ordinary data/read denied: %s", body)
			}
		})
	}
}

func TestGuardRelativeSymlinkTarget(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "review"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, claudeHook(t).Dest)
	mustWrite(t, target, []byte("{}"))
	if err := os.Symlink(target, filepath.Join(root, "notes.json")); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		body := `{"tool_name":"mcp__fs__write_file","tool_input":{"path":"notes.json","content":"{}"}}`
		if d := evaluateGuardDecisionFor(cfg, "bob", "review", parseGuardToolUse([]byte(body))); d.Allowed {
			t.Error("relative symlink to hook file allowed")
		}
	})
}

func TestTurnEndDifferentMailSameCount(t *testing.T) {
	for _, mode := range []string{"claude", "codex", "gemini"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := setupConsumeFixture(t)
			withCwd(t, root, func() {
				clearGuardEnv(t)
				t.Setenv("AGENTCHUTE_RUNNER_PID", "")
				send := func(body string) {
					if _, err := captureStdout(t, func() error { return cmdSend([]string{"--from", "alice", "--to", "bob", "--body", body}) }); err != nil {
						t.Fatal(err)
					}
				}
				stop := func(active bool) (string, error) {
					stopInput(t, fmt.Sprintf(`{"cwd":%q,"session_id":"review-session","turn_id":"review-turn","stop_hook_active":%v}`, root, active))
					out, _, err := captureStdoutStderr(t, func() error {
						args := []string{"--as", "bob", "--json"}
						if mode == "codex" {
							args = []string{"--as", "bob", "--codex-hook", "Stop"}
						}
						if mode == "gemini" {
							args = []string{"--as", "bob", "--gemini-hook", "AfterAgent"}
						}
						return cmdTurnEnd(args)
					})
					return out, err
				}
				blocks := func(out string, err error) bool {
					if mode == "codex" {
						return strings.Contains(out, `"decision":"block"`)
					}
					if mode == "gemini" {
						return strings.Contains(out, `"decision":"deny"`)
					}
					return err == errBlocked
				}
				send("first message")
				if out, err := stop(false); !blocks(out, err) {
					t.Fatalf("initial stop did not block: %v %s", err, out)
				}
				if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
					t.Fatal(err)
				}
				send("NEW message")
				out, err := stop(true)
				if !blocks(out, err) {
					t.Errorf("NEW unread mail allowed because its count matches the old count: err=%v out=%s", err, out)
				}
			})
		})
	}
}

func TestMergeKeepsPreexistingUserRule(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range retiredTemplateRuleFixtures {
		t.Run(rule, func(t *testing.T) {
			before, _ := json.Marshal(map[string]any{"model": "user-chosen", "permissions": map[string]any{"allow": []string{rule}}, "hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "./scripts/user-stop.sh"}}}}}})
			got, _, err := mergeSettingsHookFile(before, tmpl)
			if err != nil {
				t.Fatal(err)
			}
			v := decodeSettings(t, got)
			if !hasString(stringList(v["permissions"].(map[string]any)["allow"]), rule) {
				t.Errorf("first installation deleted user rule %q despite no prior agentchute hooks", rule)
			}
			if !strings.Contains(string(got), "./scripts/user-stop.sh") {
				t.Error("user Stop lost")
			}
			if again, current, err := mergeSettingsHookFile(got, tmpl); err != nil || !current || again != nil {
				t.Errorf("preserved rule causes permanent drift: current=%v err=%v", current, err)
			}
			// No unrelated scalar or user hook is needed to stop the whole-file
			// replacement shortcut from deleting an ambiguous permission rule.
			minimal, _ := json.Marshal(map[string]any{"permissions": map[string]any{"allow": []string{rule}}})
			merged, _, err := mergeSettingsHookFile(minimal, tmpl)
			if err != nil {
				t.Fatal(err)
			}
			if !hasString(stringList(decodeSettings(t, merged)["permissions"].(map[string]any)["allow"]), rule) {
				t.Error("whole-file shortcut deleted user rule")
			}
		})
	}
}

func TestGuardWriteSchemasAndToolCwd(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "review"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, claudeHook(t).Dest)
	mustWrite(t, target, []byte("{}"))
	if err := os.Symlink(target, filepath.Join(root, "notes.json")); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ tool, key string }{
		{"Write", "file_path"}, {"Edit", "file_path"}, {"MultiEdit", "file_path"},
		{"NotebookEdit", "notebook_path"}, {"write_file", "file_path"},
		{"mcp__fs__write_file", "path"}, {"mcp__fs__edit_file", "path"},
		{"write_to_file", "TargetFile"},
	}
	// The process stays in a different directory: cwd must come from the
	// event or tool input, not whichever repo the hook process inherited.
	withCwd(t, t.TempDir(), func() {
		for _, row := range rows {
			t.Run(row.tool, func(t *testing.T) {
				for _, fromArgs := range []bool{false, true} {
					args := map[string]any{row.key: "notes.json", "content": "harmless"}
					cwd := root
					if fromArgs {
						args["cwd"] = root
						cwd = t.TempDir()
					}
					body, _ := json.Marshal(map[string]any{"tool_name": row.tool, "tool_input": args, "cwd": cwd})
					if evaluateGuardDecisionFor(cfg, "bob", "review", parseGuardToolUse(body)).Allowed {
						t.Errorf("symlink target allowed (tool cwd=%v)", fromArgs)
					}
				}
			})
		}
	})
}

func TestTurnEndTurnIdentityFailsClosed(t *testing.T) {
	root, _ := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_RUNNER_PID", "")
		if _, err := captureStdout(t, func() error { return cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "unread"}) }); err != nil {
			t.Fatal(err)
		}
		stop := func(turn string, active bool) error {
			body, _ := json.Marshal(map[string]any{"session_id": "session", "turn_id": turn, "stop_hook_active": active})
			stopInput(t, string(body))
			_, _, err := captureStdoutStderr(t, func() error { return cmdTurnEnd([]string{"--as", "bob", "--json"}) })
			return err
		}
		if err := stop("one", false); err != errBlocked {
			t.Fatalf("first: %v", err)
		}
		// No self-check: another hook's active flag cannot reuse an old turn.
		if err := stop("two", true); err != errBlocked {
			t.Fatalf("new turn reused prior block: %v", err)
		}
		if err := stop("two", true); err != nil {
			t.Fatalf("same turn retry: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := stop("", true); err != errBlocked {
				t.Fatalf("unknown turn passed: %v", err)
			}
		}
	})
}

func TestClaudeJSONStopUsesTranscriptTurn(t *testing.T) {
	root, _ := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_RUNNER_PID", "")
		if _, err := captureStdout(t, func() error { return cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "unread"}) }); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "transcript.jsonl")
		transcript := `{"type":"user","uuid":"prompt-one","message":{"content":"work"}}` + "\n"
		mustWrite(t, path, []byte(transcript))
		stop := func(active bool) error {
			body, _ := json.Marshal(map[string]any{"session_id": "claude-session", "transcript_path": path, "stop_hook_active": active})
			stopInput(t, string(body))
			_, _, err := captureStdoutStderr(t, func() error { return cmdTurnEnd([]string{"--as", "bob", "--json"}) })
			return err
		}
		if err := stop(false); err != errBlocked {
			t.Fatalf("first: %v", err)
		}
		transcript += `{"type":"user","uuid":"tool-result","message":{"content":[{"type":"tool_result"}]}}` + "\n" + `{"type":"user","uuid":"hook-feedback","isMeta":true,"message":{"content":"retry"}}` + "\n"
		mustWrite(t, path, []byte(transcript))
		if err := stop(true); err != nil {
			t.Fatalf("same prompt retry: %v", err)
		}
		transcript += `{"type":"user","uuid":"prompt-two","message":{"content":[{"type":"text","text":"new work"}]}}` + "\n"
		mustWrite(t, path, []byte(transcript))
		if err := stop(true); err != errBlocked {
			t.Fatalf("new prompt with no self-check passed: %v", err)
		}
		mustWrite(t, path, []byte("invalid transcript\n"))
		if err := stop(true); err != errBlocked {
			t.Fatalf("unknown transcript passed: %v", err)
		}
	})
}

func TestStopTranscriptEvidenceIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	prompt := `{"type":"user","uuid":"one","message":{"content":"work"}}` + "\n"
	for _, row := range []struct{ name, tail, want string }{
		{"empty new prompt", `{"type":"user","uuid":"two","message":{"content":[]}}` + "\n", "two"},
		{"missing new UUID", `{"type":"user","message":{"content":"new work"}}` + "\n", ""},
		{"partial JSON", `{"type":"user"`, ""},
		{"prompt outside tail", strings.Repeat(`{"type":"assistant"}`+"\n", 450000), ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			mustWrite(t, path, []byte(prompt+row.tail))
			if got := lastTranscriptUserID(path); got != row.want {
				t.Fatalf("turn=%q want %q", got, row.want)
			}
		})
	}
}

func TestMergePreservesCommentAndDuplicateInputs(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`{"model":"x",// a comment
 "env":{"A":"b"}}`, `{"env":{"A":"b","A":"c"}}`} {
		if out, _, err := mergeSettingsHookFile([]byte(s), tmpl); err == nil || out != nil {
			t.Errorf("ambiguous input rewritten: out=%q err=%v", out, err)
		}
	}
}
