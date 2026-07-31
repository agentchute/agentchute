package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// guard_test.go proves the v2.5 plan A7/C21-C25 guard-latch behavior: when
// `check` sets it, when it clears, when a foreign/dead latch must be treated
// as unset, the C25 deny-list matrix, and the two escape hatches (no serve
// token; token but no guard bit — the grok-under-serve case) that must leave
// everything byte-identical to pre-A7 behavior.
//
// NOTE: AGENTCHUTE_SERVE_TOKEN also fences `send`'s AllocateSeq against the
// FROM agent's own serve lease (unrelated to guard). Every test below sends
// its fixture message BEFORE arming the guard env, so alice's (lease-free)
// send is never itself fenced.

func TestGuardLatchSetOnClaim(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hello"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		latch, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("ReadGuardLatch: %v", err)
		}
		if latch.Session != "tok-1" {
			t.Errorf("latch.Session = %q, want tok-1", latch.Session)
		}
	})
}

func TestGuardLatchSetOnRedeliveredResidueDisplay(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		// Simulate a crashed prior turn: a message already sitting in
		// .claimed, with no latch (as if the guard had been off, or the
		// process crashed before ever setting one).
		claimedDir := cfg.AgentClaimedDir("bob")
		mustWriteSeqInbox(t, claimedDir, "alice", 1, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue\n"))

		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-2")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		latch, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("ReadGuardLatch: %v", err)
		}
		if latch.Session != "tok-2" {
			t.Errorf("latch.Session = %q, want tok-2 (set on redelivered-residue display)", latch.Session)
		}
	})
}

func TestGuardLatchNoArchiveDisplayAlsoSets(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-3")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob", "--no-archive"}) }); err != nil {
			t.Fatalf("cmdCheck --no-archive: %v", err)
		}
		// --no-archive must not claim (inbox message stays put)...
		if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 1 {
			t.Fatalf("inbox = %d after --no-archive; want 1 (untouched)", n)
		}
		// ...but the latch still sets, since displaying counts per C23.
		latch, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("ReadGuardLatch: %v", err)
		}
		if latch.Session != "tok-3" {
			t.Errorf("latch.Session = %q, want tok-3 (set on --no-archive display)", latch.Session)
		}
	})
}

func TestGuardClearRequiresMatchingSession(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := loop.SetGuardLatch(cfg, "bob", "tok-A"); err != nil {
			t.Fatal(err)
		}
		if err := loop.ClearGuardLatch(cfg, "bob", "tok-B"); err != nil {
			t.Fatal(err)
		}
		latch, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("latch should still exist after a mismatched clear: %v", err)
		}
		if latch.Session != "tok-A" {
			t.Errorf("latch.Session = %q, want tok-A (mismatched clear must not touch it)", latch.Session)
		}
		if err := loop.ClearGuardLatch(cfg, "bob", "tok-A"); err != nil {
			t.Fatal(err)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
			t.Fatalf("latch should be gone after a matching clear; err=%v", err)
		}
	})
}

func TestGuardForeignLatchTreatedUnset(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		// A crashed/relaunched session's dead latch.
		if err := loop.SetGuardLatch(cfg, "bob", "tok-old"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-new")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")

		d := evaluateGuardInvocation("", "", "", "git push origin main")
		if !d.Allowed {
			t.Errorf("foreign latch must not deny; decision=%+v", d)
		}

		// check must not self-deny either (its self-denial only fires for ITS
		// OWN session's latch).
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck must not self-deny under a foreign latch: %v", err)
		}
	})
}

func TestGuardDenyListMatchingTable(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		if err := loop.SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			name       string
			cmd        string
			wantDenied bool
		}{
			{"git push", "git push origin main", true},
			{"git tag", "git tag v1.0.0", true},
			{"gh release", "gh release create v1.0.0", true},
			{"gh pr merge", "gh pr merge 42", true},
			{"curl", "curl https://example.com", true},
			{"wget", "wget https://example.com", true},
			{"ssh", "ssh some-host", true},
			{"scp", "scp file some-host:", true},
			{"rm -rf", "rm -rf /tmp/x", true},
			{"claude settings write", "echo x > .claude/settings.json", true},
			{"codex hooks write", "echo x > .codex/hooks.json", true},
			{"gemini settings write", "echo x > .gemini/settings.json", true},
			{"agentchute ack", "agentchute ack --as bob", true},
			{"agentchute check", "agentchute check --as bob", true},
			{"agentchute turn-end", "agentchute turn-end --as bob", true},
			{"agentchute update", "agentchute update", true},
			{"agentchute setup", "agentchute setup --yes", true},
			{"agentchute clean", "agentchute clean --owed --as bob", true},
			{"benign ls", "ls -la", false},
			{"benign git status", "git status", false},
			{"benign go test", "go test ./...", false},
			{"benign gh pr view", "gh pr view 42", false},
		}
		for _, c := range cases {
			d := evaluateGuardInvocation("", "", "", c.cmd)
			if d.Allowed == c.wantDenied {
				t.Errorf("%s: cmd=%q allowed=%v, want denied=%v", c.name, c.cmd, d.Allowed, c.wantDenied)
			}
			if c.wantDenied && d.Reason != guardDenyReason {
				t.Errorf("%s: reason = %q, want the fixed C25 deny reason", c.name, d.Reason)
			}
		}
	})
}

func TestGuardAllowWhenNoToken(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := loop.SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
			t.Fatal(err)
		}
		clearGuardEnv(t) // no AGENTCHUTE_SERVE_TOKEN at all
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")

		d := evaluateGuardInvocation("", "", "", "git push origin main")
		if !d.Allowed {
			t.Errorf("no session token must allow unconditionally; decision=%+v", d)
		}
	})
}

// TestGuardAllowWhenTokenButNoGuardBit is the grok-under-serve case (C22): a
// serve lane carries a token, but the vendor's hooks cannot clear a latch, so
// serve never exports AGENTCHUTE_GUARD for it. The latch must never arm, and
// `ack` must work exactly as it did before A7.
func TestGuardAllowWhenTokenButNoGuardBit(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-grok")
		// AGENTCHUTE_GUARD intentionally left unset.

		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
			t.Fatalf("latch must never be set without the guard bit; err=%v", err)
		}
		out, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) })
		if err != nil {
			t.Fatalf("ack must be unaffected without the guard bit: %v", err)
		}
		if !strings.Contains(out, "acked ") {
			t.Errorf("ack did not commit; out=%q", out)
		}
	})
}

func TestGuardClaudeDenyJSONShape(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return emitClaudeGuardDecision(guardDecision{Allowed: false, Reason: guardDenyReason})
	})
	if err != nil {
		t.Fatalf("emitClaudeGuardDecision: %v", err)
	}
	var wrap struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
		t.Fatalf("unmarshal: %v\n%s", jerr, out)
	}
	if wrap.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", wrap.HookSpecificOutput.HookEventName)
	}
	if wrap.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("permissionDecision = %q, want deny", wrap.HookSpecificOutput.PermissionDecision)
	}
	if wrap.HookSpecificOutput.PermissionDecisionReason != guardDenyReason {
		t.Errorf("permissionDecisionReason = %q, want %q", wrap.HookSpecificOutput.PermissionDecisionReason, guardDenyReason)
	}

	outAllow, err := captureStdout(t, func() error {
		return emitClaudeGuardDecision(guardDecision{Allowed: true})
	})
	if err != nil {
		t.Fatalf("emitClaudeGuardDecision(allow): %v", err)
	}
	if outAllow != "" {
		t.Errorf("allow decision emitted output: %q, want empty", outAllow)
	}
}

// TestGuardLatchSurvivesClaimedDrainedOutsideTurnEnd is the claim-then-abandon
// property named throughout the plan: the latch is NEVER derived from
// .claimed emptiness, so anything OTHER than turn-end draining .claimed
// (a stray removal, a bare `ack` on an unguarded build sharing this loop dir,
// etc.) must not unlock it.
func TestGuardLatchSurvivesClaimedDrainedOutsideTurnEnd(t *testing.T) {
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
		before, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("latch should be set after claim: %v", err)
		}

		if err := os.RemoveAll(cfg.AgentClaimedDir("bob")); err != nil {
			t.Fatal(err)
		}

		after, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("latch must survive .claimed being drained by anything other than turn-end: %v", err)
		}
		if after.Session != before.Session {
			t.Errorf("latch session changed: before=%q after=%q", before.Session, after.Session)
		}
	})
}
