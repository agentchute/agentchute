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
			// claude-code review, PR #89 BLOCKER 1: the installed `ac`
			// dispatcher spelling, its fully-expanded dispatch exec form, the
			// templated env-var form, and extra whitespace all bypassed the
			// old plain-substring match — proven live to self-clear the
			// latch mid-turn and disarm the entire deny list.
			{"ac dispatcher spelling", "ac turn-end --json", true},
			{"ac ack", "ac ack --as bob", true},
			{"ac check", "ac check --as bob", true},
			{"dispatch exec form (spaced shim-dir)", "agentchute dispatch --shim-dir /Users/alex/.agentchute/bin -- turn-end --json", true},
			{"dispatch exec form (= shim-dir)", "agentchute dispatch --shim-dir=/Users/alex/.agentchute/bin -- turn-end --json", true},
			{"dispatch exec form via ac token", "ac dispatch -- turn-end --json", true},
			{"templated AGENTCHUTE_BIN form", "${AGENTCHUTE_BIN:-agentchute} turn-end --json", true},
			{"bare $AGENTCHUTE_BIN form", "$AGENTCHUTE_BIN turn-end --json", true},
			{"double-space", "agentchute  turn-end --json", true},
			{"benign ls", "ls -la", false},
			{"benign git status", "git status", false},
			{"benign go test", "go test ./...", false},
			{"benign gh pr view", "gh pr view 42", false},
			{"benign word containing ac", "pac turn-end", false},
			{"benign word ending in ac", "trac turn-end", false},
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

// TestGuardCodexDenyJSONShapeMatchesCanonical pins codex-agentchute's own
// answer on review of PR #89: codex's PreToolUse deny shape is the SAME
// hookSpecificOutput/permissionDecision form as Claude's, not gate.go's
// older `{"decision":"block",...}` Stop convention (accepted but legacy).
func TestGuardCodexDenyJSONShapeMatchesCanonical(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return emitCodexGuardDecision(guardDecision{Allowed: false, Reason: guardDenyReason})
	})
	if err != nil {
		t.Fatalf("emitCodexGuardDecision: %v", err)
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
	if strings.Contains(out, `"decision"`) {
		t.Errorf("codex deny output still uses the legacy {\"decision\":...} shape: %s", out)
	}

	outAllow, err := captureStdout(t, func() error {
		return emitCodexGuardDecision(guardDecision{Allowed: true})
	})
	if err != nil {
		t.Fatalf("emitCodexGuardDecision(allow): %v", err)
	}
	if outAllow != "" {
		t.Errorf("allow decision emitted output: %q, want empty", outAllow)
	}
}

// TestGuardGeminiDenyJSONShapeIsTopLevelNotNested is codex review PR #89
// round 3 finding #3: gemini's BeforeTool contract is a top-level
// {"decision":"block","reason":...}, NOT the nested
// hookSpecificOutput/permissionDecision shape Claude/codex's PreToolUse event
// uses — round 1 incorrectly generalized codex's own answer to gemini too,
// making the gemini emitter send a shape gemini's BeforeTool would not
// recognize (an inert guard).
func TestGuardGeminiDenyJSONShapeIsTopLevelNotNested(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return emitGeminiGuardDecision(guardDecision{Allowed: false, Reason: guardDenyReason})
	})
	if err != nil {
		t.Fatalf("emitGeminiGuardDecision: %v", err)
	}
	var wrap struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
		t.Fatalf("unmarshal: %v\n%s", jerr, out)
	}
	if wrap.Decision != "block" {
		t.Errorf("decision = %q, want block", wrap.Decision)
	}
	if wrap.Reason != guardDenyReason {
		t.Errorf("reason = %q, want %q", wrap.Reason, guardDenyReason)
	}
	if strings.Contains(out, "hookSpecificOutput") {
		t.Errorf("gemini deny output must be top-level, not nested under hookSpecificOutput: %s", out)
	}

	outAllow, err := captureStdout(t, func() error {
		return emitGeminiGuardDecision(guardDecision{Allowed: true})
	})
	if err != nil {
		t.Fatalf("emitGeminiGuardDecision(allow): %v", err)
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

// TestCmdGuardClearStale covers the CLI surface of the mixed-hook-trust
// recovery path (claude-code review round 4, on codex's finding #1): no
// latch is a no-op success; a young latch is refused; an old latch clears
// REGARDLESS of a session mismatch, since age (not session identity) is the
// authorization here.
// TestCmdGuardRecoverNoOpsWhenDisarmed is grok's J-R6: a hookless/hand-run
// lane (no serve token, or no AGENTCHUTE_GUARD bit) gains no new state or
// ceremony from --recover — pure no-op.
func TestCmdGuardRecoverNoOpsWhenDisarmed(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		out, err := captureStdout(t, func() error {
			return cmdGuard([]string{"--recover", "--as", "bob"})
		})
		if err != nil {
			t.Fatalf("--recover while disarmed: %v", err)
		}
		if !strings.Contains(out, "disarmed") {
			t.Errorf("expected a disarmed no-op message, got %q", out)
		}
		if _, err := loop.ReadGuardRecoveredMark(cfg, "bob"); !os.IsNotExist(err) {
			t.Fatalf("no mark should be set while disarmed; err=%v", err)
		}
	})
}

// TestCmdGuardRecoverArchivesOwnLatchOnly mirrors turn-end's P1 guarantee:
// --recover archives THIS session's own claimed mail (own-latch match) but
// never a foreign/dead latch's residue.
func TestCmdGuardRecoverArchivesOwnLatchOnly(t *testing.T) {
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

		out, err := captureStdout(t, func() error {
			return cmdGuard([]string{"--recover", "--as", "bob"})
		})
		if err != nil {
			t.Fatalf("--recover: %v", err)
		}
		if !strings.Contains(out, "recovered mailbox access") {
			t.Errorf("expected a recovery confirmation, got %q", out)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Errorf(".claimed = %d after recover; want 0 (own-session mail committed)", n)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
			t.Errorf("archive = %d after recover; want 1", n)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
			t.Fatalf("own latch should be cleared after recover; err=%v", err)
		}
		mark, err := loop.ReadGuardRecoveredMark(cfg, "bob")
		if err != nil {
			t.Fatalf("recovered mark should be set: %v", err)
		}
		if mark.Session != "tok-1" {
			t.Errorf("mark.Session = %q, want tok-1", mark.Session)
		}
	})
}

// TestCmdGuardRecoverDoesNotArchiveForeignLatch is the P1-preservation half
// of grok's requirement: a foreign/dead latch's residue must survive
// --recover untouched, same as it survives turn-end.
func TestCmdGuardRecoverDoesNotArchiveForeignLatch(t *testing.T) {
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
			return cmdGuard([]string{"--recover", "--as", "bob"})
		}); err != nil {
			t.Fatalf("--recover: %v", err)
		}
		if n := countMessageFiles(t, claimedDir); n != 1 {
			t.Errorf(".claimed = %d after recover; want 1 (foreign-latch residue must survive)", n)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 0 {
			t.Errorf("archive = %d after recover; want 0", n)
		}
	})
}

// TestGuardRecoverCommandItselfNotDenied confirms `guard --recover` is not
// itself on the C25 deny list: it must be reachable even while a (fresh,
// currently-armed) latch is set, since it is the escape hatch for exactly
// that state.
func TestGuardRecoverCommandItselfNotDenied(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		if err := loop.SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
			t.Fatal(err)
		}
		d := evaluateGuardInvocation("", "", "", "agentchute guard --recover --as bob")
		if !d.Allowed {
			t.Errorf("guard --recover must not be on the deny list; decision=%+v", d)
		}
	})
}

// TestMixedHookTrustStateRecoversViaGuardRecover is the full scenario codex
// round 3 finding #1 / grok's A1 attack / claude-code review round 4
// describe, implementing the RULED two-tier design: a lane where the
// PreToolUse guard is active but Stop never runs. A direct `turn-end`
// attempt is denied before recovery. After --recover: turn-end/ack/check
// (mailbox) are allowed and committed normally, but scope-expanding tools
// (git push, both via the direct binary name and the `ac` dispatcher
// spelling) stay denied for the REST of the session — even AFTER running
// the now-allowed turn-end, which is exactly the step grok's A1 attack used
// to fully unlock an earlier, broken draft.
func TestMixedHookTrustStateRecoversViaGuardRecover(t *testing.T) {
	root, _ := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "hi"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}

		// PreToolUse is active (the mixed state): direct turn-end AND push
		// are both denied, same as always.
		if evaluateGuardInvocation("", "", "", "agentchute turn-end --json").Allowed {
			t.Fatal("turn-end must be denied before recovery")
		}
		if evaluateGuardInvocation("", "", "", "git push origin main").Allowed {
			t.Fatal("push must be denied before recovery")
		}

		// Stop never runs (simulated by never calling cmdTurnEnd). Recover
		// instead.
		if _, err := captureStdout(t, func() error {
			return cmdGuard([]string{"--recover", "--as", "bob"})
		}); err != nil {
			t.Fatalf("--recover must succeed: %v", err)
		}

		// Mailbox restored: turn-end (any spelling) now allowed.
		if d := evaluateGuardInvocation("", "", "", "agentchute turn-end --json"); !d.Allowed {
			t.Fatalf("turn-end must be allowed after recover; decision=%+v", d)
		}
		if d := evaluateGuardInvocation("", "", "", "ac turn-end --json"); !d.Allowed {
			t.Fatalf("`ac turn-end` must be allowed after recover; decision=%+v", d)
		}
		// Scope-expanding tools stay denied — the whole point.
		if d := evaluateGuardInvocation("", "", "", "git push origin main"); d.Allowed {
			t.Fatalf("push must STILL be denied after recover (session-sticky mark); decision=%+v", d)
		}

		// Actually run the now-allowed turn-end (grok's A1 attack step) and
		// confirm push is STILL denied afterward — proving the mark is not
		// clearable by the very command it unlocked.
		if _, err := captureStdout(t, func() error {
			return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
		}); err != nil {
			t.Fatalf("turn-end after recover: %v", err)
		}
		if d := evaluateGuardInvocation("", "", "", "git push origin main"); d.Allowed {
			t.Fatalf("push must STILL be denied after running turn-end post-recover (grok A1): decision=%+v", d)
		}
		// A second mailbox call stays allowed too — the mark only ever
		// restricts the scope-expanding category, never mailbox.
		if d := evaluateGuardInvocation("", "", "", "ac turn-end --json"); !d.Allowed {
			t.Fatalf("mailbox commands must remain allowed post-recover; decision=%+v", d)
		}

		// A relaunch (new, different serve token) makes the mark inert.
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-2")
		if d := evaluateGuardInvocation("", "", "", "git push origin main"); !d.Allowed {
			t.Fatalf("push must be allowed again after a relaunch (new session token); decision=%+v", d)
		}
	})
}

// TestGuardRecoveredMarkFailsClosedOnCorruptFile is claude-code review round
// 4 item 4: a corrupt/unreadable recovered-mark file must fail CLOSED for
// scope-expanding purposes (treated as still recovered/restricted) — the
// opposite of the latch's own fail-open posture, deliberately, since this
// signal's only job is preventing regained scope-expanding capability.
func TestGuardRecoveredMarkFailsClosedOnCorruptFile(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		mustWrite(t, cfg.GuardRecoveredMarkPath("bob"), []byte("{not valid json"))
		d := evaluateGuardInvocation("", "", "", "git push origin main")
		if d.Allowed {
			t.Fatalf("a corrupt recovered mark must fail CLOSED (deny scope tools), not open; decision=%+v", d)
		}
		// Mailbox commands are unaffected by the mark either way.
		if d := evaluateGuardInvocation("", "", "", "agentchute check --as bob"); !d.Allowed {
			t.Fatalf("mailbox commands must not be affected by the recovered mark's fail-closed posture; decision=%+v", d)
		}
	})
}
