package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func turnEndArgs(extra ...string) []string {
	return append([]string{"--as", "claude-code", "--vendor", "anthropic"}, extra...)
}

// TestTurnEndRecreatesMissingRegistrationBeforeGating is the codex
// concurrent-hooks scenario (C24): the registration row is missing when
// turn-end fires (no prior boot/self-check ran). Step 0's self-repair must
// recreate it BEFORE step 3's gate evaluates, so the gate does not
// false-block on "not registered".
func TestTurnEndRecreatesMissingRegistrationBeforeGating(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		regPath := filepath.Join(root, ".agentchute", "loop", "agents", "claude-code.md")
		if _, err := os.Stat(regPath); err == nil {
			t.Fatal("registration should not pre-exist in a fresh fixture")
		}

		out, err := captureStdout(t, func() error { return cmdTurnEnd(turnEndArgs("--json")) })
		if err != nil {
			t.Fatalf("turn-end: %v\n%s", err, out)
		}
		if _, err := os.Stat(regPath); err != nil {
			t.Fatalf("registration was not recreated by turn-end's self-repair: %v", err)
		}
		var status gateStatus
		if jerr := json.Unmarshal([]byte(out), &status); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if status.MissingReg {
			t.Errorf("gate reported MissingReg=true after turn-end's self-repair; status=%+v", status)
		}
		if status.Blocked {
			t.Errorf("gate falsely blocked on the missing-registration race; status=%+v", status)
		}
	})
}

// TestTurnEndArchivesAndClearsLatchEvenWhenGateBlocked proves steps 0-2 run
// regardless of step 3's outcome: an unrelated unread message blocks the
// finish gate, but the message THIS session claimed still commits and the
// latch still clears.
func TestTurnEndArchivesAndClearsLatchEvenWhenGateBlocked(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "first"}); err != nil {
			t.Fatalf("cmdSend(first): %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); err != nil {
			t.Fatalf("latch should be set after claim: %v", err)
		}

		// msg2 arrives after, unclaimed — an unrelated finish blocker.
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "")
		t.Setenv("AGENTCHUTE_GUARD", "")
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "second"}); err != nil {
			t.Fatalf("cmdSend(second): %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		out, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		})
		if !errors.Is(err, errBlocked) {
			t.Fatalf("turn-end err = %v, want errBlocked (msg2 still unread)", err)
		}
		var status gateStatus
		if jerr := json.Unmarshal([]byte(out), &status); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if !status.Blocked || status.UnreadCount != 1 {
			t.Fatalf("status = %+v, want Blocked=true UnreadCount=1", status)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
			t.Errorf("archive = %d, want 1 (msg1 committed despite the unrelated block)", n)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Errorf(".claimed = %d, want 0 after archive", n)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
			t.Errorf("latch should be cleared after turn-end even though the gate blocked; err=%v", err)
		}
	})
}

// TestTurnEndDoesNotArchiveForeignLatchResidue is the gemini crash case
// (grok P1): claimed residue exists from a DIFFERENT (dead/foreign) session
// than the one now running turn-end. That residue must NOT be archived —
// it must survive for `check`'s own redelivery banner.
func TestTurnEndDoesNotArchiveForeignLatchResidue(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		claimedDir := cfg.AgentClaimedDir("bob")
		mustWriteSeqInbox(t, claimedDir, "alice", 1, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue\n"))
		if err := loop.SetGuardLatch(cfg, "bob", "tok-dead"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-new")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		if _, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		}); err != nil && !errors.Is(err, errBlocked) {
			t.Fatalf("turn-end: %v", err)
		}

		if n := countMessageFiles(t, claimedDir); n != 1 {
			t.Errorf(".claimed = %d, want 1 (foreign-latch residue must not be archived)", n)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 0 {
			t.Errorf("archive = %d, want 0 (nothing archived under a foreign latch)", n)
		}
		latch, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("dead latch should still be present (untouched by ClearGuardLatch(tok-new)): %v", err)
		}
		if latch.Session != "tok-dead" {
			t.Errorf("latch.Session = %q, want tok-dead (untouched)", latch.Session)
		}
	})
}

// TestTurnEndNoLatchStillSelfRepairsAndGates: without a serve session at all
// (a human, or an unguarded lane), turn-end still self-repairs the
// registration and still evaluates + emits the gate. It just never archives
// (that stays `ack`'s job for un-latched sessions).
func TestTurnEndNoLatchStillSelfRepairsAndGates(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		out, err := captureStdout(t, func() error { return cmdTurnEnd(turnEndArgs("--json")) })
		if err != nil {
			t.Fatalf("turn-end: %v\n%s", err, out)
		}
		regPath := filepath.Join(root, ".agentchute", "loop", "agents", "claude-code.md")
		if _, err := os.Stat(regPath); err != nil {
			t.Fatalf("registration was not created by turn-end's self-repair: %v", err)
		}
		var status gateStatus
		if jerr := json.Unmarshal([]byte(out), &status); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if status.Blocked {
			t.Errorf("gate falsely blocked; status=%+v", status)
		}
	})
}

// TestTurnEndCodexHookStopBlockedShapeMatchesGate proves --codex-hook Stop
// parity with gate.go's own emitGateCodexStop (analogous to
// TestGateCodexHookStopBlockedShape): turn-end reuses the exact same
// gateStatus + emitter, so the JSON shape can never drift.
func TestTurnEndCodexHookStopBlockedShapeMatchesGate(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		mustWriteSeqInbox(t, inboxDir, "codex", 1,
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		out, err := captureStdout(t, func() error {
			return cmdTurnEnd(append(turnEndArgs(), "--codex-hook", "Stop"))
		})
		if err != nil {
			t.Errorf("err = %v, want nil (codex Stop block exits 0)", err)
		}
		var wrap struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal codex Stop output: %v\n%s", jerr, out)
		}
		if wrap.Decision != "block" {
			t.Errorf("decision = %q, want block", wrap.Decision)
		}
		if !strings.Contains(wrap.Reason, "unread") {
			t.Errorf("reason missing unread context: %q", wrap.Reason)
		}
	})
}

func TestTurnEndCodexHookStopClearShapeMatchesGate(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")
		out, err := captureStdout(t, func() error {
			return cmdTurnEnd(append(turnEndArgs(), "--codex-hook", "Stop"))
		})
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("clear codex Stop should produce no output; got %q", out)
		}
	})
}
