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
// NOTE: AGENTCHUTE_SERVE_TOKEN also fences `send`'s MintSendStamp against the
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

// TestGuardDenyListMatchingTable is the guard-latch-livelock fix's
// regression lock (brief test cases 1-3, 7): the "allow" rows for
// git push/tag, gh pr create/merge, gh release, ssh, scp are the actual
// livelock fix — MUST FAIL on the pre-fix binary, where these were denied
// and produced the "check arms the latch; the turn's own push is denied"
// deadlock (hit claude-code x2, sonnet on PR #110, codex on PR #111 the same
// day). Every agentchute-subcommand / hook-config-write / curl-wget/rm-rf
// row must stay denied under every spelling — subtraction, not a rewrite of
// the regex-based defense.
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
			// Livelock fix: cut from the deny list — none of these can touch
			// claimed-but-uncommitted mail or the end-of-turn handler.
			{"git push", "git push origin HEAD", false},
			{"git tag", "git tag -a v1.5.1 -m x", false},
			{"gh pr create", "gh pr create", false},
			{"gh pr merge", "gh pr merge 110", false},
			{"gh release", "gh release create v1.5.1", false},
			{"ssh", "ssh host", false},
			{"scp", "scp a b:/c", false},
			// Kept: curl/wget can move an arbitrary payload off the command
			// line and reach turn-end/rm -rf/hook-config rewrites with no
			// denied token of their own.
			{"curl", "curl https://example.com", true},
			{"wget", "wget https://example.com", true},
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
			// latch mid-turn and disarm the entire deny list. UNCHANGED by
			// the livelock fix — guardAgentchuteSubcmdRE is untouched.
			{"ac dispatcher spelling", "ac turn-end --json", true},
			{"ac ack", "ac ack --as bob", true},
			{"ac check", "ac check --as bob", true},
			{"dispatch exec form (spaced shim-dir)", "agentchute dispatch --shim-dir /Users/alex/.agentchute/bin -- turn-end --json", true},
			{"dispatch exec form (= shim-dir)", "agentchute dispatch --shim-dir=/Users/alex/.agentchute/bin -- turn-end --json", true},
			{"dispatch exec form via ac token", "ac dispatch -- turn-end --json", true},
			{"templated AGENTCHUTE_BIN form", "${AGENTCHUTE_BIN:-agentchute} turn-end --json", true},
			{"bare $AGENTCHUTE_BIN form", "$AGENTCHUTE_BIN ack --as bob", true},
			{"double-space", "agentchute  turn-end --json", true},
			// Heredoc/quoted-body disarm lock (brief test case 4): no
			// stripper was added, so a heredoc marker or a commented-out
			// `<<EOF` line does nothing special — the plain command text
			// still contains "agentchute turn-end"/"agentchute check"
			// literally and is still denied. Fails if anyone later adds a
			// heredoc-body stripper (the rejected design: it was attacked
			// into a universal disarm, `echo "<<EOF" && agentchute turn-end`
			// clearing its own latch).
			{"heredoc marker then turn-end", `echo "<<EOF" && agentchute turn-end`, true},
			{"commented heredoc then check", "# <<EOF\nagentchute check", true},
			{"benign ls", "ls -la", false},
			{"benign git status", "git status", false},
			{"benign go test", "go test ./...", false},
			{"benign gh pr view", "gh pr view 42", false},
			{"benign word containing ac", "pac turn-end", false},
			{"benign word ending in ac", "trac turn-end", false},
			// Word-boundary precision (brief test case 7): self-check is a
			// distinct subcommand from check and must stay allowed.
			{"agentchute self-check", "agentchute self-check --quiet", false},
		}
		for _, c := range cases {
			d := evaluateGuardInvocation("", "", "", c.cmd)
			if d.Allowed == c.wantDenied {
				t.Errorf("%s: cmd=%q allowed=%v, want denied=%v", c.name, c.cmd, d.Allowed, c.wantDenied)
			}
			if c.wantDenied && d.Reason != guardDenyReason {
				t.Errorf("%s: reason = %q, want the fixed deny reason", c.name, d.Reason)
			}
		}
	})
}

// TestGuardDirectSendDataSinkException proves the narrow false-positive cut:
// protected words are inert inside one direct send's arguments, while any
// syntax that could execute a second command or rewrite a hook remains denied.
func TestGuardDirectSendDataSinkException(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		wantDenied bool
	}{
		{"literal send body", `agentchute send --to claude-code --body 'agentchute turn-end; rm -rf /tmp/x'`, false},
		{"Bash tool prefix", `Bash agentchute send --to claude-code --body '.codex/hooks.json'`, false},
		{"exec tool prefix", `functions.exec_command ac send --to claude-code --body 'curl https://example.com'`, false},
		{"ANSI-C quoted body", `ac send --to claude-code --body $'agentchute check\nrm -rf /tmp/x'`, false},
		{"quoted shell operators", `agentchute send --to claude-code --body 'echo "<<EOF" && agentchute turn-end | sh'`, false},
		{"compound tail", `agentchute send --to claude-code --body ok && agentchute turn-end`, true},
		{"pipe", `agentchute send --to claude-code --body ok | sh`, true},
		{"command substitution", `agentchute send --to claude-code --body "$(agentchute turn-end)"`, true},
		{"backtick substitution", "agentchute send --to claude-code --body `agentchute turn-end`", true},
		{"plain parameter expansion", `agentchute send --to claude-code --body "$AGENTCHUTE_SERVE_TOKEN"`, true},
		{"braced parameter expansion", `agentchute send --to claude-code --body "${AGENTCHUTE_SERVE_TOKEN}"`, true},
		{"special parameter", `agentchute send --to claude-code --body "$?"`, true},
		{"unquoted plain parameter expansion", `agentchute send --to claude-code --body $AGENTCHUTE_SERVE_TOKEN`, true},
		{"unquoted special parameter", `agentchute send --to claude-code --body $?`, true},
		{"hook redirection", `agentchute send --to claude-code --body ok > .codex/hooks.json`, true},
		{"shell wrapper", `sh -c 'agentchute send --to claude-code --body "agentchute turn-end"'`, true},
		{"unterminated quote", `agentchute send --to claude-code --body 'agentchute turn-end`, true},
		{"heredoc body", "agentchute send --to claude-code --body ok <<EOF\nagentchute turn-end\nEOF", true},
		{"rejected universal disarm", `echo "<<EOF" && agentchute turn-end`, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := guardCommandDenied(c.cmd); got != c.wantDenied {
				t.Errorf("guardCommandDenied(%q) = %v, want %v", c.cmd, got, c.wantDenied)
			}
		})
	}
}

// TestGuardMutatedDenyListStillDeniesAgentchuteSubcommands is the
// guard-latch-livelock fix's mutation test (brief test case 5): with
// guardPipelineDenySubstrings emptied out entirely, `agentchute turn-end`
// and `ac check` must still be denied — proving the defense for the
// agentchute subcommands lives in guardAgentchuteSubcmdRE, not in the
// substring list this fix just cut down.
func TestGuardMutatedDenyListStillDeniesAgentchuteSubcommands(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-1")
		t.Setenv("AGENTCHUTE_GUARD", "1")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		if err := loop.SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
			t.Fatal(err)
		}

		old := guardPipelineDenySubstrings
		guardPipelineDenySubstrings = nil
		t.Cleanup(func() { guardPipelineDenySubstrings = old })

		for _, cmd := range []string{"agentchute turn-end --as bob", "ac check --as bob"} {
			d := evaluateGuardInvocation("", "", "", cmd)
			if d.Allowed {
				t.Errorf("cmd=%q allowed with an empty pipeline deny list; the agentchute-subcommand defense must live in the regex, not the list", cmd)
			}
		}
	})
}

// TestGuardDenyReasonNamesSection15 is the guard-latch-livelock fix's
// wording lock (brief test case 6): the deny reason must name the correct
// section (§15, not the old §9) and the causal-integrity framing
// ("turn-end"), and must no longer claim to guard general "scope-expanding"
// actions — that claim became false the moment push/tag/release left the
// deny list.
func TestGuardDenyReasonNamesSection15(t *testing.T) {
	if !strings.Contains(guardDenyReason, "§15") {
		t.Errorf("guardDenyReason = %q, want it to name §15", guardDenyReason)
	}
	if !strings.Contains(guardDenyReason, "turn-end") {
		t.Errorf("guardDenyReason = %q, want it to name turn-end", guardDenyReason)
	}
	if strings.Contains(guardDenyReason, "§9") {
		t.Errorf("guardDenyReason = %q, must not still claim §9", guardDenyReason)
	}
	if strings.Contains(guardDenyReason, "scope-expanding") {
		t.Errorf("guardDenyReason = %q, must not claim general scope-expanding coverage (no longer true)", guardDenyReason)
	}
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
