package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func gateArgs(phase string, extra ...string) []string {
	base := []string{"--as", "claude-code", "--before", phase}
	return append(base, extra...)
}

func TestGateHelpDocumentsBlockersAndHookExitCodes(t *testing.T) {
	help := gateHelp()
	for _, want := range []string{
		"malformed inbox files",
		"asker-owned only",
		"blocked in text/--json modes",
		"--codex-hook Stop",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("gate help missing %q:\n%s", want, help)
		}
	}
}

// v0.9.0 `.owed` redesign: reply obligations are asker-owned only. A recipient
// is NEVER blocked at finish by a reply_required message — an asker-owned `.owed`
// obligation surfaces as a non-blocking gate warning, so gate --before consensus
// (and finish) must CLEAR even with an outstanding owed obligation and an empty
// inbox.
func TestGateDoesNotBlockOnOwedObligation(t *testing.T) {
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

		// Seed an asker-owned obligation (we are owed a reply BY codex).
		now := time.Now().UTC()
		key := loop.MsgID{To: "codex", From: "claude-code", Seq: 1}
		if err := loop.RecordOwed(cfg, "claude-code", key, now.Add(30*time.Minute), now); err != nil {
			t.Fatal(err)
		}

		// gate --before consensus must CLEAR (owed is a warning, not a blocker).
		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if err != nil {
			t.Fatalf("gate consensus with owed obligation err = %v, want nil (clear)", err)
		}
		if !strings.Contains(out, "clear") {
			t.Errorf("gate output = %q, want clear", out)
		}
		// finish must clear too.
		if _, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) }); err != nil {
			t.Errorf("gate finish with owed obligation err = %v, want nil (clear)", err)
		}
	})
}

// gate --before finish blocks on unread direct mail and clears once consumed.
func TestGateFinishBlocksOnUnreadMail(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}

		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		mustWriteSeqInbox(t, inboxDir, "codex", 1,
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))

		_, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked", err)
		}
	})
}

// gate --before consensus does NOT consult the stale-reg threshold. Stale
// registration alone (with empty inbox + ledger) must pass consensus.
func TestGateConsensusIgnoresStaleReg(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}

		// Backdate the `.live` presence (GATE 3: the freshness SOURCE) so it
		// would trigger stale_reg on a commit/release phase.
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")
		mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC().Add(-2*StaleRegThreshold))

		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if err != nil {
			t.Errorf("consensus with stale reg err = %v, want nil (stale-reg is commit/release only)", err)
		}
	})
}

// gate --before commit blocks when registration is stale; --ack-stale-reg
// with --require-confirm unblocks (operator explicitly acknowledged the warn).
func TestGateCommitBlocksOnStaleRegAndUnblocksWithAck(t *testing.T) {
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
		// GATE 3: freshness comes from `.live`, not registration last_seen. boot
		// wrote a fresh `.live`; overwrite it with a stale one so commit blocks
		// on stale presence. (The registration's own last_seen stays fresh,
		// proving the SOURCE is `.live`.)
		mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC().Add(-2*StaleRegThreshold))

		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("commit")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("commit with stale reg err = %v, want errBlocked", err)
		}

		// Acknowledgment without --require-confirm has no effect (spec: ack
		// only counts when paired with --require-confirm).
		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("commit", "--ack-stale-reg")) })
		if !errors.Is(err, errBlocked) {
			t.Errorf("commit with bare --ack-stale-reg err = %v, want errBlocked", err)
		}

		// Both flags set: gate unblocks.
		_, err = captureStdout(t, func() error {
			return cmdGate(gateArgs("commit", "--require-confirm", "--ack-stale-reg"))
		})
		if err != nil {
			t.Errorf("commit with --require-confirm + --ack-stale-reg err = %v, want nil", err)
		}
	})
}

// GATE 3: the commit-phase StaleReg signal is driven by the `.live` presence
// fact, NOT registration last_seen. A fresh `.live` => not stale even when the
// registration's own last_seen is forced old; an old OR absent `.live` => stale.
func TestGateStaleRegDrivenByLive(t *testing.T) {
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

		// Force the REGISTRATION last_seen old — this must NOT make commit stale,
		// because `.live` is the source.
		regPath := cfg.AgentRegistrationPath("claude-code")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		reg.LastSeen = time.Now().UTC().Add(-10 * StaleRegThreshold)
		if err := loop.WriteRegistration(regPath, reg); err != nil {
			t.Fatal(err)
		}

		// Fresh `.live` => commit clear despite the stale registration last_seen.
		mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC())
		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("commit", "--json")) })
		if err != nil {
			t.Fatalf("commit with fresh .live (stale reg last_seen) err = %v; want nil; out=%s", err, out)
		}
		var got gateStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.StaleReg {
			t.Errorf("StaleReg = true with a fresh .live; want false (source must be .live, not reg.LastSeen)")
		}

		// Old `.live` => commit blocks on stale presence.
		mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC().Add(-2*StaleRegThreshold))
		if _, err := captureStdout(t, func() error { return cmdGate(gateArgs("commit")) }); !errors.Is(err, errBlocked) {
			t.Errorf("commit with old .live err = %v; want errBlocked", err)
		}

		// Absent `.live` (registered but no presence) => commit blocks on stale.
		if err := os.Remove(filepath.Join(cfg.LoopDir, "live", "claude-code.live")); err != nil {
			t.Fatal(err)
		}
		out, err = captureStdout(t, func() error { return cmdGate(gateArgs("commit", "--json")) })
		if !errors.Is(err, errBlocked) {
			t.Errorf("commit with absent .live err = %v; want errBlocked", err)
		}
		got = gateStatus{}
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if !got.StaleReg {
			t.Errorf("StaleReg = false with an absent .live; want true")
		}
		// Absent presence must NOT leak the misleading "age 0s > threshold".
		if strings.Contains(strings.Join(got.Reasons, " | "), "age 0s") {
			t.Errorf("absent-.live reason leaked \"age 0s\": %v", got.Reasons)
		}
	})
}

// --codex-hook Stop on blocked: emits {"decision":"block","reason":"..."} to
// stdout AND exits 0 (returned nil error). This is the codex-preferred shape.
func TestGateCodexHookStopBlockedShape(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		mustWriteSeqInbox(t, inboxDir, "codex", 1,
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish", "--codex-hook", "Stop")) })
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

// --codex-hook Stop on clear: empty output, exit 0. Codex sees no decision
// and stops normally.
func TestGateCodexHookStopClearShape(t *testing.T) {
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
		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish", "--codex-hook", "Stop")) })
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("clear codex Stop should produce no output; got %q", out)
		}
	})
}

func TestGateRejectsUnknownPhase(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdGate(gateArgs("frobnicate")) })
		if err == nil {
			t.Fatal("expected error for unknown phase")
		}
		if errors.Is(err, errBlocked) {
			t.Errorf("unknown phase should be command failure, not errBlocked: %v", err)
		}
	})
}

// v0.9.0 `.owed` redesign: a corrupt asker-owned `.owed` ledger must NOT block
// the gate — it is a NON-BLOCKING warning (gate is read-only and must never
// deadlock). (The removed recipient-side pending-reply ledger used to block on
// corruption; that ledger no longer exists.)
func TestGate_CorruptOwedLedgerIsWarningNotBlocker(t *testing.T) {
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

		// Corrupt the asker-owned .owed ledger on disk (unparseable JSON).
		path := filepath.Join(cfg.AgentStateDir("claude-code"), "owed.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{ this is not valid json"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		if err != nil {
			t.Fatalf("gate finish on corrupt owed ledger err = %v, want nil (warning, not blocker)", err)
		}
		if !strings.Contains(out, "clear") {
			t.Errorf("gate output should be clear; got:\n%s", out)
		}
		if !strings.Contains(strings.ToLower(out), "owed") {
			t.Errorf("expected a non-blocking owed-ledger warning; got:\n%s", out)
		}
	})
}

// Codex review (c17e310): gate must block on §11 protocol violations
// (malformed inbox files), not just on valid unread mail. Otherwise an
// agent can declare finish with quarantine work still owed.
func TestGateBlocksOnMalformedInbox(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		// Drop a malformed file directly in the inbox (won't parse as a
		// §6.1 reference filename — too few segments).
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		malformed := filepath.Join(inboxDir, "not-a-valid-message-name.md")
		if err := os.WriteFile(malformed, []byte("---\nfrom: ??\n---\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish", "--json")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked (malformed inbox files must block finish)", err)
		}
		if !strings.Contains(out, "malformed") {
			t.Errorf("output should mention malformed: %q", out)
		}
	})
}

// Codex review (c17e310): missing-registration must report a distinct
// reason from "registration is stale (age N > threshold)", since the
// missing case has no age to cite — leaking "age 0s > 30m" was misleading.
func TestGateMissingRegistrationDistinctReason(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		// Don't register; gate --before commit should block with a missing-reg reason.
		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("commit", "--json")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked", err)
		}
		var got gateStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		joined := strings.Join(got.Reasons, " | ")
		if !strings.Contains(joined, "no registration") && !strings.Contains(joined, "not registered") && !strings.Contains(joined, "missing") {
			t.Errorf("reasons missing the missing-registration phrasing: %v", got.Reasons)
		}
		if strings.Contains(joined, "age 0s") {
			t.Errorf("reasons leaked the misleading \"age 0s > threshold\" wording: %v", got.Reasons)
		}
	})
}

func TestGateRequiresPhaseFlag(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdGate([]string{"--as", "claude-code"}) })
		if err == nil {
			t.Fatal("expected error when --before is omitted")
		}
	})
}

func TestGateFinishAcceptsActiveSessionHeartbeat(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := loop.SaveActiveSession(cfg, loop.ActiveSession{
			AgentID:  "claude-code",
			Host:     localHostname(),
			PID:      os.Getpid(),
			LastSeen: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		if err != nil {
			t.Fatalf("finish with active session heartbeat err = %v; output=%q", err, out)
		}
		if !strings.Contains(out, "clear") {
			t.Errorf("gate output = %q, want clear", out)
		}
	})
}

// v0.2: gate --before continue is a sibling of finish, identical
// predicate (unread / malformed), different output framing for in-session
// continuation hooks.
func TestGateContinuePhaseSamePredicateAsFinish(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		mustWriteSeqInbox(t, inboxDir, "codex", 1,
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		_, errFinish := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		_, errContinue := captureStdout(t, func() error { return cmdGate(gateArgs("continue")) })
		if !errors.Is(errFinish, errBlocked) {
			t.Fatalf("finish err = %v, want errBlocked", errFinish)
		}
		if !errors.Is(errContinue, errBlocked) {
			t.Fatalf("continue err = %v, want errBlocked", errContinue)
		}
	})
}
