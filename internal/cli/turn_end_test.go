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

// TestTurnEndArchivesUnconditionallyWithoutGuardSession is codex review PR
// #89 finding #1: a hand-run hook-capable session (no serve token at all)
// must still have its claimed mail committed by turn-end, matching the
// pre-A7 unconditional `ack` behavior the removed standalone Stop-hook entry
// used to provide. turn-end's archive step must default to "commit" and
// withhold ONLY when a latch actively names a different session — not
// whenever THIS session has no guard concept at all.
func TestTurnEndArchivesUnconditionallyWithoutGuardSession(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
			t.Fatalf(".claimed = %d after claim; want 1", n)
		}

		out, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		})
		if err != nil {
			t.Fatalf("turn-end: %v\n%s", err, out)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Errorf(".claimed = %d after turn-end with no guard session; want 0 (committed)", n)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
			t.Errorf("archive = %d after turn-end with no guard session; want 1 (committed)", n)
		}
	})
}

// TestTurnEndMalformedLatchDoesNotWedge is codex review PR #89 finding #4: a
// corrupt/unparseable guard.latch file must fail open at every step, not
// become a permanent hard error that blocks the finish gate from ever
// running again. Mirrors evaluateGuardDecision's own fail-open posture on a
// read it cannot make sense of.
func TestTurnEndMalformedLatchDoesNotWedge(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}

		// Corrupt the latch file in place (truncated/invalid JSON).
		mustWrite(t, cfg.GuardLatchPath("bob"), []byte("{not valid json"))

		out, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		})
		if err != nil {
			t.Fatalf("turn-end must fail open on a corrupt latch, not hard-error: %v\n%s", err, out)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Errorf(".claimed = %d after turn-end with a corrupt latch; want 0 (fail-open commits)", n)
		}

		// A second turn-end call must ALSO succeed (the corrupt file, if left
		// in place by a non-overwriting clear, would otherwise wedge every
		// future call identically).
		if _, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		}); err != nil {
			t.Fatalf("second turn-end after a corrupt latch must also succeed: %v", err)
		}
	})
}

// TestGuardArmedWithoutHooksEverFiringStillRecoversViaTurnEnd is codex review
// PR #89 finding #3: during a codex hook-trust rollout window (a
// newly-changed project-local hook definition isn't re-trusted/loaded yet),
// a lane can be armed (check sets the latch) while NEITHER the PreToolUse
// guard NOR the Stop-hook turn-end call ever actually runs. This binary
// cannot detect codex's own hook-trust state, so it cannot avoid arming in
// that window — but it must never be an unrecoverable wedge. This test
// proves the escape hatch: `check`/`ack` correctly self-deny (their error
// text names `turn-end` as the fix), and a DIRECT `turn-end` invocation —
// exactly what a stuck model/human would run next — always succeeds and
// clears the latch, with no dependency on any hook actually having fired.
func TestGuardArmedWithoutHooksEverFiringStillRecoversViaTurnEnd(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		// check arms the latch exactly as it would under any guarded serve
		// session — independent of whether codex has actually loaded/trusted
		// the new PreToolUse/Stop hook definitions this PR ships.
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}

		// Neither hook ever fires in this simulated rollout window: no guard
		// PreToolUse call, no Stop-hook turn-end call. The model tries the
		// commands it would normally reach for and must be redirected, not
		// silently stuck.
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err == nil || !strings.Contains(err.Error(), "turn-end") {
			t.Fatalf("second check err = %v, want a denial naming turn-end as the fix", err)
		}
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) }); err == nil || !strings.Contains(err.Error(), "turn-end") {
			t.Fatalf("ack err = %v, want a denial naming turn-end as the fix", err)
		}

		// The named recovery command always works: turn-end has no
		// self-denial of its own, and the PreToolUse guard that would (if
		// active) deny running it is exactly the hook that isn't firing in
		// this window — so it is never itself the thing blocking recovery.
		if _, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		}); err != nil {
			t.Fatalf("turn-end must always recover a stuck lane: %v", err)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
			t.Fatalf("latch should be cleared after the recovery turn-end call; err=%v", err)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
			t.Errorf("archive = %d after recovery turn-end; want 1 (committed)", n)
		}
	})
}

// TestTurnEndSurvivesUnresolvableVendorForNonCanonicalID is claude-code
// review PR #89 BLOCKER 2: the shipped hook entries invoke bare `turn-end`
// (no --vendor, by design — C26 ships it env-identity-only), so step 0's
// self-repair needs resolveAgentVendor to backfill a vendor from an EXISTING
// registration. A registration row can vanish out from under it (the codex
// concurrent-hooks race turn-end step 0 exists to survive) for a roster id
// that also does not vendor-prefix-match any canonical wrapper base (e.g.
// "sonnet") — resolveAgentVendor then has nothing left to fall back to and
// performRegister fails outright. The old unconditional abort-on-error left
// this session fully wedged (claimed mail never committed, latch never
// cleared, every later check/ack self-denied). turn-end must instead commit
// and clear regardless, surfacing the missing-registration state as a normal
// gate reason rather than an opaque command failure.
func TestTurnEndSurvivesUnresolvableVendorForNonCanonicalID(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "sonnet", "--vendor", "anthropic"}); err != nil {
			t.Fatalf("register sonnet: %v", err)
		}
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "sonnet", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "sonnet"}) }); err != nil {
			t.Fatalf("cmdCheck(sonnet): %v", err)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("sonnet")); n != 1 {
			t.Fatalf(".claimed = %d after claim; want 1", n)
		}

		// The concurrent-hooks race: the registration row disappears before
		// turn-end's step 0 runs. "sonnet" does not vendor-prefix-match any
		// canonical wrapper base, so resolveAgentVendor has no fallback.
		if err := os.Remove(cfg.AgentRegistrationPath("sonnet")); err != nil {
			t.Fatal(err)
		}

		// turn-end WITHOUT --vendor, exactly as the shipped hook templates
		// invoke it (C26: env-identity-only).
		if _, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "sonnet", "--json"})
		}); err != nil && !errors.Is(err, errBlocked) {
			t.Fatalf("turn-end must not hard-fail on an unresolvable vendor; got: %v", err)
		}

		if n := countMessageFiles(t, cfg.AgentClaimedDir("sonnet")); n != 0 {
			t.Errorf(".claimed = %d after turn-end; want 0 (committed despite the self-repair write failing)", n)
		}
		if _, err := loop.ReadGuardLatch(cfg, "sonnet"); !os.IsNotExist(err) {
			t.Errorf("latch should be cleared regardless of the self-repair failure; err=%v", err)
		}

		// The old bug left every later command self-denied against a latch
		// that could never clear. Re-registering (the human/boot fix for the
		// underlying missing-row problem) must be enough to work normally
		// again — proving the latch itself, not the registration gap, was
		// the thing standing between this session and recovery.
		if err := cmdRegister([]string{"--as", "sonnet", "--vendor", "anthropic"}); err != nil {
			t.Fatalf("re-register sonnet: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "sonnet"}) }); err != nil {
			t.Fatalf("check after recovery must not be denied: %v", err)
		}
	})
}

// TestTurnEndNoTokenArchivesDespiteStaleLatch is codex review PR #89 round 3
// finding #2: a STALE latch left behind by a crashed/prior guarded session
// must not permanently block archiving for every LATER unguarded/hand-run
// turn-end call. Comparing the stale latch's session against session=="" (an
// unguarded invocation can never equal a real token) made every such call
// treat it as foreign, withholding the commit forever — and step 2 never
// clears it either, since it also no-ops when session=="". Guard-disabled
// invocations must archive unconditionally regardless of what a past guarded
// session left on disk.
func TestTurnEndNoTokenArchivesDespiteStaleLatch(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
			t.Fatalf(".claimed = %d after claim; want 1", n)
		}
		// A stale latch from some earlier (now-dead) guarded session,
		// simulated directly rather than via check (which never sets one
		// without AGENTCHUTE_GUARD armed).
		if err := loop.SetGuardLatch(cfg, "bob", "tok-dead"); err != nil {
			t.Fatal(err)
		}

		// This invocation runs with NO serve token at all (a hand-run
		// session, or the guard bit simply never set).
		out, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		})
		if err != nil {
			t.Fatalf("turn-end: %v\n%s", err, out)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Errorf(".claimed = %d after no-token turn-end; want 0 (a stale latch must not block the commit)", n)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
			t.Errorf("archive = %d after no-token turn-end; want 1 (committed)", n)
		}

		// A second no-token turn-end call must ALSO still commit correctly
		// (proving this isn't a one-shot fluke and the loop truly never
		// recurs for later mail either).
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "again"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob) second: %v", err)
		}
		if _, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		}); err != nil {
			t.Fatalf("second turn-end: %v", err)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 2 {
			t.Errorf("archive = %d after second no-token turn-end; want 2", n)
		}
	})
}

// TestTurnEndWarnsWhenSessionIsRecovered is claude-code review round 4 item
// 3: turn-end must still archive/gate exactly as always while a session's
// recovered mark is set, but should print an informational warning that the
// session is in degraded guard state.
func TestTurnEndWarnsWhenSessionIsRecovered(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		if err := loop.SetGuardRecoveredMark(cfg, "bob", "tok-1"); err != nil {
			t.Fatal(err)
		}

		var turnEndErr error
		stderr := captureStderr(t, func() {
			_, turnEndErr = captureStdout(t, func() error {
				return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
			})
		})
		if turnEndErr != nil {
			t.Fatalf("turn-end must still work normally while recovered: %v", turnEndErr)
		}
		if !strings.Contains(stderr, "degraded guard state") {
			t.Errorf("expected a degraded-guard-state warning on stderr, got %q", stderr)
		}
	})
}
