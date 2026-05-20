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

// Test 1 (spec rev3 Part 4) ledger half: gate --before consensus blocks when
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

		// gate --before consensus must exit 2.
		_, err = captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("gate consensus err = %v, want errBlocked", err)
		}

		// Clear the ledger entry; gate must now pass.
		if err := loop.MarkPendingReplied(cfg, "claude-code", entry.MessageID, "reply-msg-1", time.Now().UTC()); err != nil {
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

		inboxDir := filepath.Join(root, ".rehumanlabs", "loop", "inbox", "claude-code")
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

		// Backdate the registration's last_seen so it would trigger stale_reg.
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		regPath := cfg.AgentRegistrationPath("claude-code")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		reg.LastSeen = time.Now().UTC().Add(-2 * StaleRegThreshold)
		if err := loop.WriteRegistration(regPath, reg); err != nil {
			t.Fatal(err)
		}

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
		regPath := cfg.AgentRegistrationPath("claude-code")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		reg.LastSeen = time.Now().UTC().Add(-2 * StaleRegThreshold)
		if err := loop.WriteRegistration(regPath, reg); err != nil {
			t.Fatal(err)
		}

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

// --codex-hook Stop on blocked: emits {"decision":"block","reason":"..."} to
// stdout AND exits 0 (returned nil error). This is the codex-preferred shape.
func TestGateCodexHookStopBlockedShape(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		inboxDir := filepath.Join(root, ".rehumanlabs", "loop", "inbox", "claude-code")
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
		// §6.1.2 reference filename — too few segments).
		inboxDir := filepath.Join(root, ".rehumanlabs", "loop", "inbox", "claude-code")
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

// Codex review (c17e310): `release` phase must warn on WAKE_STALE peer
// registrations per spec rev3 §A.3. Warn-only — does not block release.
func TestGateReleaseWarnsOnWakeStalePeer(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMUX_PANE", "%2")
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}

		// Backdate codex's last_seen past the stale threshold (codex is pokable
		// per the TMUX_PANE auto-detect, so it qualifies as wake_stale).
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
			t.Errorf("err = %v, want nil (wake_stale is warn-only, must not block release)", err)
		}
		// Output must surface the wake_stale signal as a non-zero count.
		var got gateStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if !got.WakeStale {
			t.Errorf("WakeStale = false; want true with a backdated pokable peer")
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
