package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): active agent commands
// must refuse to operate without a self-registration record. Each test
// asserts the refusal happens, the error message names the agent, and the
// pointer to `agentchute boot` is present so an LLM agent (or a human
// operator) reading the error knows exactly what to run next.

// All tests use setupBootFixture (which sets up the loop scaffold but does
// NOT register any agent). That gives us the "no registration on disk"
// state.

func TestCheckRefusesMissingSelfRegistration(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		})
		if err == nil {
			t.Fatal("check should refuse for unregistered agent; got nil error")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error missing 'not registered' wording: %v", err)
		}
		if !strings.Contains(err.Error(), "agentchute boot --as claude-code") {
			t.Errorf("error missing boot pointer: %v", err)
		}
		if !strings.Contains(err.Error(), "§5.3") {
			t.Errorf("error missing §5.3 anchor: %v", err)
		}
	})
}

func TestSendRefusesMissingSelfRegistration(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		// Register only the recipient. Sender has no registration.
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body", "y"})
		})
		if err == nil {
			t.Fatal("send should refuse for unregistered sender; got nil error")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error missing 'not registered' wording: %v", err)
		}
		if !strings.Contains(err.Error(), "agentchute boot --as claude-code") {
			t.Errorf("error missing boot pointer: %v", err)
		}
	})
}

func TestStatusAsRefusesMissingSelfRegistration(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdStatus([]string{"--as", "claude-code"})
		})
		if err == nil {
			t.Fatal("status --as should refuse for unregistered agent; got nil error")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error missing 'not registered' wording: %v", err)
		}
		// status's error also mentions the bare-status escape hatch.
		if !strings.Contains(err.Error(), "omit --as") {
			t.Errorf("error missing 'omit --as' escape hatch: %v", err)
		}
	})
}

func TestStatusBareStillWorksWithoutRegistration(t *testing.T) {
	// Pool-overview status (no --as) is a side-effect-free read; it must
	// not refuse even when no agent is registered.
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdStatus(nil)
		})
		if err != nil {
			t.Errorf("status with no --as should work even with no registrations; got %v", err)
		}
	})
}

func TestGateFinishBlocksOnMissingRegistration(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdGate([]string{"--as", "claude-code", "--before", "finish", "--json"})
		})
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked", err)
		}
		var got gateStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		// v0.2.1 extends missing-reg blocking from commit/release to every
		// phase. finish must now refuse for an unenrolled agent.
		joined := strings.Join(got.Reasons, " | ")
		if !strings.Contains(joined, "not registered") {
			t.Errorf("finish reasons missing 'not registered': %v", got.Reasons)
		}
	})
}

func TestGateContinueBlocksOnMissingRegistration(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdGate([]string{"--as", "claude-code", "--before", "continue", "--json"})
		})
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked", err)
		}
		var got gateStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		joined := strings.Join(got.Reasons, " | ")
		if !strings.Contains(joined, "not registered") {
			t.Errorf("continue reasons missing 'not registered': %v", got.Reasons)
		}
	})
}

func TestPendingSurfacesNeedsBoot(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		// Text mode: includes the boot pointer; stays exit 0 (read-only).
		out, err := captureStdout(t, func() error {
			return cmdPending([]string{"--as", "claude-code"})
		})
		if err != nil {
			t.Fatalf("pending should not error on missing reg (read-only); got %v", err)
		}
		if !strings.Contains(out, "not registered") {
			t.Errorf("text output missing 'not registered' wording:\n%s", out)
		}
		if !strings.Contains(out, "agentchute boot --as claude-code") {
			t.Errorf("text output missing boot pointer:\n%s", out)
		}
	})
}

func TestPendingJSONSurfacesNeedsBoot(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPending([]string{"--as", "claude-code", "--json"})
		})
		if err != nil {
			t.Fatalf("pending --json should not error on missing reg; got %v", err)
		}
		var got map[string]any
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got["needs_boot"] != true {
			t.Errorf("needs_boot = %v, want true; JSON:\n%s", got["needs_boot"], out)
		}
		hint, _ := got["boot_hint"].(string)
		if !strings.Contains(hint, "agentchute boot --as claude-code") {
			t.Errorf("boot_hint missing or wrong: %q", hint)
		}
	})
}

func TestPendingFailIfAnyReturnsExit2OnNeedsBoot(t *testing.T) {
	// codex review guardrail: --fail-if-any is the actionable-work
	// scheduler-preflight; needs_boot IS actionable work, so it must
	// exit 2. Other pending modes stay exit 0 (read-only context).
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdPending([]string{"--as", "claude-code", "--fail-if-any"})
		})
		if !errors.Is(err, errFailIfAny) {
			t.Errorf("err = %v, want errFailIfAny on needs_boot", err)
		}
	})
}

func TestPendingClaudeHookStaysExit0OnNeedsBoot(t *testing.T) {
	// codex guardrail: hook-envelope modes inject context but never
	// fail the turn. needs_boot lands in additionalContext, exit 0.
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPending([]string{"--as", "claude-code", "--claude-hook", "UserPromptSubmit"})
		})
		if err != nil {
			t.Errorf("claude-hook should stay exit 0 on needs_boot; got %v", err)
		}
		var got map[string]any
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		hookOut, _ := got["hookSpecificOutput"].(map[string]any)
		ctx, _ := hookOut["additionalContext"].(string)
		if !strings.Contains(ctx, "not registered") {
			t.Errorf("additionalContext missing 'not registered':\n%s", ctx)
		}
		if !strings.Contains(ctx, "agentchute boot --as claude-code") {
			t.Errorf("additionalContext missing boot pointer:\n%s", ctx)
		}
	})
}
