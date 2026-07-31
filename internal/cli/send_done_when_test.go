package cli

import (
	"strings"
	"testing"
)

// send_done_when_test.go — the done-when warning (v2.5 plan B9) was REMOVED
// (post-1.5.x friction program, item 3): it grepped for the single literal
// spelling "done-when", but AGENTS.md's Communication Rules rule 2 requires
// only a stated, verifiable completion condition in free-form style — no
// spelling is mandated, so a one-spelling substring check false-positived
// on every legitimate variant (the retired six-label envelope's ACCEPTANCE:,
// which codex still sends). This file now proves the warning is gone for
// every shape, not narrowed to recognize a second hardcoded label.

// item 3 of the post-1.5.x friction program: the warning fired on every
// --ask sent with an ACCEPTANCE: line instead of DONE-WHEN: — including
// the retired six-label envelope's own spelling, which codex still uses —
// even though AGENTS.md's current Communication Rules (rule 2) require
// only a stated, verifiable completion condition, in free-form style
// ("no required label order... style is free", AGENTS.md:171). A
// substring check for one literal spelling cannot honestly verify a
// requirement the spec explicitly declines to pin to any spelling, so the
// warning is removed rather than widened to a second hardcoded label
// (widening just moves the same false-positive to the next spelling
// someone reasonably chooses). This test proves an ACCEPTANCE:-shaped
// envelope — the exact shape that misfired today — no longer warns, and
// TestSendAskWithNoCompletionConditionNeverWarns proves the warning is
// gone outright, not narrowed.
func TestSendAskWithAcceptanceLineNeverWarns(t *testing.T) {
	root, _ := setupSendFixture(t)
	var stderr string
	withCwd(t, root, func() {
		stderr = captureStderr(t, func() {
			if _, err := captureStdout(t, func() error {
				return cmdSend([]string{
					"--from", "claude-code", "--to", "codex",
					"--ask", "--body", "please review this.\n\nACCEPTANCE: verdict issued for head abc1234",
				})
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
		})
	})
	if strings.Contains(stderr, "done-when") {
		t.Fatalf("an ACCEPTANCE:-shaped envelope must not warn about a missing done-when line, got stderr=%q", stderr)
	}
}

// The warning is removed outright (not narrowed to a wider label set): an
// --ask with no stated completion condition at all no longer warns either.
// AGENTS.md rule 2 is unchanged and still expects one — this only removes
// the mechanical nag that could not check it honestly.
func TestSendAskWithNoCompletionConditionNeverWarns(t *testing.T) {
	root, _ := setupSendFixture(t)
	var stderr string
	withCwd(t, root, func() {
		stderr = captureStderr(t, func() {
			if _, err := captureStdout(t, func() error {
				return cmdSend([]string{
					"--from", "claude-code", "--to", "codex",
					"--ask", "--body", "please take a look at this",
				})
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
		})
	})
	if strings.Contains(stderr, "done-when") {
		t.Fatalf("the done-when warning was removed; got stderr=%q", stderr)
	}
}

func TestSendAskWithDoneWhenIsSilent(t *testing.T) {
	root, _ := setupSendFixture(t)
	var stderr string
	withCwd(t, root, func() {
		stderr = captureStderr(t, func() {
			if _, err := captureStdout(t, func() error {
				return cmdSend([]string{
					"--from", "claude-code", "--to", "codex",
					"--ask", "--body", "please review this.\n\nDONE-WHEN: PR #100 has a SHIP/FIX verdict",
				})
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
		})
	})
	if strings.Contains(stderr, "no 'done-when' line") {
		t.Fatalf("expected no done-when warning, got stderr=%q", stderr)
	}
}

func TestSendNonAskNeverWarnsAboutDoneWhen(t *testing.T) {
	root, _ := setupSendFixture(t)
	var stderr string
	withCwd(t, root, func() {
		stderr = captureStderr(t, func() {
			if _, err := captureStdout(t, func() error {
				return cmdSend([]string{
					"--from", "claude-code", "--to", "codex",
					"--body", "status update, no reply needed",
				})
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
		})
	})
	if strings.Contains(stderr, "no 'done-when' line") {
		t.Fatalf("non-ask send must never warn about done-when, got stderr=%q", stderr)
	}
}
