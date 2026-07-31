package cli

import (
	"strings"
	"testing"
)

// send_done_when_test.go — the warn-only done-when check (v2.5 plan B9,
// AGENTS.md Communication Rules rule 2): an --ask body with no verifiable
// done-when line gets a stderr warning; never a blocked send.

func TestSendAskWithoutDoneWhenWarns(t *testing.T) {
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
	if !strings.Contains(stderr, "no 'done-when' line") {
		t.Fatalf("expected a done-when warning, got stderr=%q", stderr)
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
