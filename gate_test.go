package main

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
		"corrupt or unreadable pending-reply ledger",
		"blocked in text/--json modes",
		"--codex-hook Stop",
		"--gemini-hook AfterAgent",
		"current Gemini templates use BeforeAgent + --json",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("gate help missing %q:\n%s", want, help)
		}
	}
}

// Reply-obligation ledger half: gate --before consensus blocks when
// a pending-reply ledger entry exists, then clears after the entry transitions
// to replied/deferred.
func TestGateConsensusBlocksOnPendingReply(t *testing.T) {
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
		entry := loop.PendingReplyEntry{
			MessageID:        "2026-05-19T17:53:59.561894Z",
			From:             "codex",
			To:               "claude-code",
			Task:             "review",
			OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
			ArchivePath:      "archive/x.md",
		}
		if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")

		// gate --before consensus must exit 2.
		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("gate consensus err = %v, want errBlocked", err)
		}

		// Clear the ledger entry; gate must now pass.
		if err := loop.MarkPendingReplied(cfg, "claude-code", entry.MessageID, entry.From, "reply-msg-1", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if err != nil {
			t.Errorf("gate consensus after reply err = %v, want nil", err)
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
		_, err := loop.WriteInboxMessage(inboxDir, time.Now().UTC(), "codex",
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		if err != nil {
			t.Fatal(err)
		}

		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
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
		_, err := loop.WriteInboxMessage(inboxDir, time.Now().UTC(), "codex",
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		if err != nil {
			t.Fatal(err)
		}
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

// TestGate_CorruptLedgerBlocksWithQuarantineNotFatal: a corrupt/unparseable
// pending-replies.json must NOT brick the gate with a fatal command error
// (which would block EVERY phase until a human edits state), and must NOT be
// silently treated as "no obligations" (that would false-clear finish).
// Instead the gate BLOCKS (errBlocked) with an actionable, quarantine-style
// remediation, mirroring the malformed-inbox handling.
func TestGate_CorruptLedgerBlocksWithQuarantineNotFatal(t *testing.T) {
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
		// Fresh poller heartbeat so liveness does not independently block — we
		// want to prove the corrupt ledger is the (only) blocking reason.
		mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")

		// Corrupt the ledger on disk (unparseable JSON).
		path := cfg.PendingRepliesPath("claude-code")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{ this is not valid json"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("gate finish on corrupt ledger err = %v, want errBlocked (blocked, not fatal, not clear)", err)
		}
		// The blocking reason must be actionable and quarantine-flavored — not a
		// generic "0 pending" clear and not a raw parse error crash.
		if !strings.Contains(out, "blocked") {
			t.Errorf("gate output should show blocked; got:\n%s", out)
		}
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "ledger") {
			t.Errorf("blocking reason should reference the ledger; got:\n%s", out)
		}
		if !strings.Contains(lower, "corrupt") && !strings.Contains(lower, "quarantine") && !strings.Contains(lower, "unreadable") {
			t.Errorf("blocking reason should be a corrupt/quarantine remediation; got:\n%s", out)
		}
	})
}

// TestGate_CorruptLedgerJSONOutputReportsBlocked: the --json shape must report
// blocked:true with a reason for a corrupt ledger (not crash, not clear).
func TestGate_CorruptLedgerJSONOutputReportsBlocked(t *testing.T) {
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
		path := cfg.PendingRepliesPath("claude-code")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish", "--json")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("gate finish --json on corrupt ledger err = %v, want errBlocked", err)
		}
		var status gateStatus
		if jerr := json.Unmarshal([]byte(out), &status); jerr != nil {
			t.Fatalf("unmarshal gate JSON: %v\n%s", jerr, out)
		}
		if !status.Blocked {
			t.Errorf("status.Blocked = false on corrupt ledger; want true\n%s", out)
		}
		if len(status.Reasons) == 0 {
			t.Errorf("status.Reasons empty on corrupt ledger; want a remediation reason\n%s", out)
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

// Pull-only (Gate 6c): there are no pokable peers, so `release` never raises a
// WAKE_STALE warning. The wake_stale field stays in the gate JSON shape (always
// false) for wire-shape stability; release must still clear (not block) even with
// a backdated peer present.
func TestGateReleaseNoWakeStaleUnderPullOnly(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}

		// Backdate codex's last_seen past the stale threshold — under pull-only it
		// is not pokable, so this never produces a wake_stale signal.
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		codexPath := cfg.AgentRegistrationPath("codex")
		reg, err := loop.ReadRegistration(codexPath)
		if err != nil {
			t.Fatal(err)
		}
		reg.LastSeen = time.Now().UTC().Add(-2 * StaleRegThreshold)
		if err := loop.WriteRegistration(codexPath, reg); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdGate(gateArgs("release", "--json")) })
		if err != nil {
			t.Errorf("err = %v, want nil (release must clear)", err)
		}
		var got gateStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.WakeStale || got.WakeStaleCount != 0 {
			t.Errorf("WakeStale=%v count=%d; want false/0 under pull-only", got.WakeStale, got.WakeStaleCount)
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
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic", "--wake-target", ""}); err != nil {
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
// predicate (unread / malformed / pending-replies), different output
// framing for in-session continuation hooks.
func TestGateContinuePhaseSamePredicateAsFinish(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		_, err := loop.WriteInboxMessage(inboxDir, time.Now().UTC(), "codex",
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		if err != nil {
			t.Fatal(err)
		}
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

// Legacy/experimental --gemini-hook AfterAgent emits decision:deny on block,
// decision:allow on clear. Always exit 0 — the JSON is the signal. The shipped
// Gemini hook template uses BeforeAgent + --json instead.
func TestGateGeminiHookAfterAgentBlockedShape(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		_, err := loop.WriteInboxMessage(inboxDir, time.Now().UTC(), "codex",
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error {
			return cmdGate(gateArgs("continue", "--gemini-hook", "AfterAgent"))
		})
		if err != nil {
			t.Errorf("err = %v, want nil (gemini hook exit 0)", err)
		}
		var wrap struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if wrap.Decision != "deny" {
			t.Errorf("decision = %q, want deny", wrap.Decision)
		}
		if !strings.Contains(wrap.Reason, "unread") {
			t.Errorf("reason missing 'unread': %q", wrap.Reason)
		}
	})
}

func TestGateGeminiHookAfterAgentClearShape(t *testing.T) {
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
			return cmdGate(gateArgs("continue", "--gemini-hook", "AfterAgent"))
		})
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		var wrap struct {
			Decision string `json:"decision"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if wrap.Decision != "allow" {
			t.Errorf("decision = %q, want allow on clear inbox", wrap.Decision)
		}
	})
}
