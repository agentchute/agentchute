package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- Gemini CLI emitters (H6) ----------

func decodeJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return m
}

func TestGeminiBootAndPendingEmitAdditionalContext(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return emitBootGeminiSessionStart(bootStatus{Agent: "gemini-cli", Vendor: "google"})
	})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeJSON(t, out)
	hso, _ := m["hookSpecificOutput"].(map[string]any)
	if hso["hookEventName"] != "SessionStart" || hso["additionalContext"] == "" {
		t.Fatalf("SessionStart shape = %v", m)
	}

	out, err = captureStdout(t, func() error {
		return emitHookContextJSON("BeforeAgent", buildPendingContext(nil, nil, 0, true, "gemini-cli"))
	})
	if err != nil {
		t.Fatal(err)
	}
	m = decodeJSON(t, out)
	hso, _ = m["hookSpecificOutput"].(map[string]any)
	if hso["hookEventName"] != "BeforeAgent" || !strings.Contains(hso["additionalContext"].(string), "boot") {
		t.Fatalf("BeforeAgent shape = %v", m)
	}
}

func TestGeminiAfterAgentTurnEndShape(t *testing.T) {
	blocked := gateStatus{Blocked: true, UnreadCount: 2}
	// Clear: silent.
	out, err := captureStdout(t, func() error {
		return emitTurnEndGeminiAfterAgent(gateStatus{}, strings.NewReader(`{"stop_hook_active":false}`))
	})
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("clear AfterAgent must be silent: out=%q err=%v", out, err)
	}
	// Blocked: top-level deny with a reason, exit 0 (nil error).
	out, err = captureStdout(t, func() error {
		return emitTurnEndGeminiAfterAgent(blocked, strings.NewReader(`{"hook_event_name":"AfterAgent","stop_hook_active":false}`))
	})
	if err != nil {
		t.Fatalf("blocked AfterAgent must not return an error (exit 0 with JSON): %v", err)
	}
	m := decodeJSON(t, out)
	if m["decision"] != "deny" || m["reason"] == "" || m["hookSpecificOutput"] != nil {
		t.Fatalf("blocked AfterAgent shape = %v", m)
	}
	// Our own retry (stop_hook_active): never deny again.
	out, err = captureStdout(t, func() error {
		return emitTurnEndGeminiAfterAgent(blocked, strings.NewReader(`{"stop_hook_active":true}`))
	})
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("stop_hook_active retry must be silent: out=%q err=%v", out, err)
	}
}

// A latched inert `send --body` under Gemini CLI (tool run_shell_command) and
// Antigravity (run_command) is recognized as a direct send, like Bash.
func TestGuardDirectSendStripsGoogleToolNames(t *testing.T) {
	for _, cmd := range []string{
		`run_shell_command agentchute send --to codex --body "please run agentchute ack later"`,
		`run_command agentchute send --to codex --body "rm -rf nothing"`,
		`Bash agentchute send --to codex --body "x"`,
	} {
		candidate, inert := guardDirectSendInvocation(cmd)
		if !candidate || !inert {
			t.Fatalf("%q: candidate=%v inert=%v, want a direct inert send", cmd, candidate, inert)
		}
	}
}

// ---------- Antigravity (agy) ----------

func TestAgyGuardDecisionAlwaysCarriesDecision(t *testing.T) {
	out, err := captureStdout(t, func() error { return emitAgyGuardDecision(guardDecision{Allowed: true}) })
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeJSON(t, out); m["decision"] != "ask" {
		t.Fatalf("allow shape = %v (decision is REQUIRED; ask = the ordinary approval path, never allow)", m)
	}
	out, err = captureStdout(t, func() error {
		return emitAgyGuardDecision(guardDecision{Allowed: false, Reason: guardDenyReason})
	})
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeJSON(t, out); m["decision"] != "deny" || m["reason"] != guardDenyReason {
		t.Fatalf("deny shape = %v", m)
	}
}

func TestGuardParsesAntigravityToolCall(t *testing.T) {
	body := `{"conversationId":"c1","toolCall":{"name":"run_command","args":{"CommandLine":"agentchute ack --as agy","Cwd":"/w","WaitMsBeforeAsync":5000}},"stepIdx":3}`
	cmd := parseGuardToolCommand([]byte(body))
	if !strings.HasPrefix(cmd, "run_command ") || !strings.Contains(cmd, "agentchute ack --as agy") {
		t.Fatalf("command text = %q", cmd)
	}
	if !guardCommandDenied(cmd) {
		t.Fatalf("a camelCase toolCall running ack was not denied: %q", cmd)
	}
	allowed := `{"toolCall":{"name":"run_command","args":{"CommandLine":"git status","Cwd":"/w"}}}`
	if cmd := parseGuardToolCommand([]byte(allowed)); guardCommandDenied(cmd) {
		t.Fatalf("harmless toolCall denied: %q", cmd)
	}
}

func TestAgyStopAndPreInvocationShapes(t *testing.T) {
	out, err := captureStdout(t, func() error { return emitTurnEndAgyStop(gateStatus{}) })
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeJSON(t, out); m["decision"] != "stop" {
		t.Fatalf("clear Stop shape = %v (any value but continue allows the stop)", m)
	}
	out, err = captureStdout(t, func() error { return emitTurnEndAgyStop(gateStatus{Blocked: true, UnreadCount: 1}) })
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeJSON(t, out); m["decision"] != "continue" || m["reason"] == "" {
		t.Fatalf("blocked Stop shape = %v", m)
	}

	// PreInvocation: boot context only on the first invocation.
	out, err = captureStdout(t, func() error {
		return emitBootAgyPreInvocation(bootStatus{Agent: "agy", Vendor: "google"}, strings.NewReader(`{"invocationNum":0,"initialNumSteps":0}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeJSON(t, out)
	steps, _ := m["injectSteps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("first invocation shape = %v", m)
	}
	if step, _ := steps[0].(map[string]any); step["ephemeralMessage"] == nil {
		t.Fatalf("first invocation step = %v, want ephemeralMessage", steps[0])
	}
	out, err = captureStdout(t, func() error {
		return emitBootAgyPreInvocation(bootStatus{Agent: "agy"}, strings.NewReader(`{"invocationNum":1}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "{}" {
		t.Fatalf("later invocation = %q, want {}", out)
	}
	// pending: {} when nothing is pending, injectSteps otherwise.
	out, _ = captureStdout(t, func() error { return emitAgyInjectSteps("") })
	if strings.TrimSpace(out) != "{}" {
		t.Fatalf("empty injectSteps = %q", out)
	}
}

func TestAgyWrapperSpecAndSetup(t *testing.T) {
	agy, ok := wrapperForToken("agy")
	if !ok || agy.Key != "agy" || agy.AgentID != "agy" || agy.Vendor != "google" || !agy.Guarded {
		t.Fatalf("agy spec = %+v, %v", agy, ok)
	}
	gemini, _ := wrapperSpecForName("gemini")
	for _, c := range gemini.Candidates {
		if c == "agy" {
			t.Fatal("ac serve gemini must never resolve to agy")
		}
	}
	if _, ok := wrapperSpecForName("agy"); !ok {
		t.Fatal("agy is not a wrapper name")
	}
	if !wrapperIsKnownForSetup("agy") {
		t.Fatal("setup --wrappers agy unknown")
	}
	if w, ok := hookWrapperForAgent("agy"); !ok || w != "agy" {
		t.Fatalf("doctor hook wrapper for agy = %q, %v", w, ok)
	}
	if names := shimNamesForAgent("agy"); len(names) != 1 || names[0] != "ac-agy" {
		t.Fatalf("shim names for agy = %v", names)
	}
}

// The shipped agy template passes doctor's hook_content_sanity (its named-
// hook shape is walked for commands) and installs to .agents/hooks.json.
func TestAgyTemplateInstallsAndPassesDoctorSanity(t *testing.T) {
	data, err := fs.ReadFile(hooksFS, "examples/hooks/agy/.agents/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	body, err := hookCommandBody(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"boot --agy-hook PreInvocation", "pending --agy-hook PreInvocation", "guard --pre-tool-use --agy-hook PreToolUse", "turn-end --agy-hook Stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("agy template body lacks %q:\n%s", want, body)
		}
	}
	if unknown := hookBodyUnknownSubcommands(body); len(unknown) != 0 {
		t.Fatalf("agy template invokes unknown subcommands: %v", unknown)
	}

	cfg := newDoctorCfg(t)
	if err := os.WriteFile(filepath.Join(cfg.ControlRepo, "AGENTCHUTE.md"), []byte("# spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The template invokes `${AGENTCHUTE_BIN:-agentchute}`; CI has no
	// agentchute on PATH, so point the override at this test binary.
	t.Setenv("AGENTCHUTE_BIN", os.Args[0])
	withCwd(t, cfg.ControlRepo, func() {
		if _, err := captureStdout(t, func() error { return cmdHooks([]string{"install", "--wrapper", "agy"}) }); err != nil {
			t.Fatal(err)
		}
	})
	installed := filepath.Join(cfg.ControlRepo, ".agents", "hooks.json")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("agy template not installed: %v", err)
	}
	r := runDoctorChecks(cfg, "agy", doctorOptions{Now: time.Now().UTC()})
	for _, name := range []string{"hook_content_sanity", "hook_file_presence"} {
		if c := findCheck(t, r, name); c.Severity == severityBlocker {
			t.Fatalf("%s blocks on the agy template: %s", name, c.Message)
		}
	}
}
