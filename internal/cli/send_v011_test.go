package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// setupSendFixture registers claude-code + codex in a fresh control repo
// and returns the loop config. Pull-only: sends deliver by writing the inbox
// unconditionally.
func setupSendFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic", "--host", "peer-host"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

// readMostRecentInboxMessage returns the body of the most recently dropped
// non-archive message file for `agent`.
func readMostRecentInboxMessage(t *testing.T, cfg *loop.Config, agent string) string {
	t.Helper()
	inbox := cfg.AgentInboxDir(agent)
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().After(newestMod) {
			newest = filepath.Join(inbox, e.Name())
			newestMod = info.ModTime()
		}
	}
	if newest == "" {
		t.Fatalf("no inbox messages for %s", agent)
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Pull-only (Gate 6c): TestSendInfersSenderFromCurrentTmuxPane was removed.
// Sender inference from a tmux/herdr pane is gone (registrations carry no wake
// target to map a pane back to an id); --from comes from --as / $AGENTCHUTE_AGENT_ID.

// AGENTCHUTE.md §6.4: --ask sets reply_required: true frontmatter
// AND prepends ## ASK to the body.
func TestSendAskSetsReplyRequiredAndHeading(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--ask", "--body", "look at the diff"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	if !strings.Contains(body, "reply_required: true") {
		t.Errorf("frontmatter missing reply_required: true:\n%s", body)
	}
	if !strings.Contains(body, "## ASK") {
		t.Errorf("body missing ## ASK heading:\n%s", body)
	}
	if !strings.Contains(body, "look at the diff") {
		t.Errorf("body missing user content:\n%s", body)
	}
}

func TestSendAskPreservesExistingAskHeading(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--ask", "--body", "## ASK\n\nalready has heading"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	count := strings.Count(body, "## ASK")
	if count != 1 {
		t.Errorf("## ASK appears %d times in body; expected 1 (idempotent):\n%s", count, body)
	}
}

// v0.9.0 `.owed` redesign REGRESSION: reply obligations are asker-owned only.
// End-to-end: the asker sends --ask (records a `.owed` entry), the recipient
// replies with --reply-to <ref> (delivers with in_reply_to; NO recipient-side
// ledger), and the asker's `check` consumes the reply and discharges the `.owed`
// entry via ClearOwed — all WITHOUT any recipient-side pending-reply ledger.
func TestSendAskReplyDischargesAskerOwedViaCheck(t *testing.T) {
	root, cfg := setupSendFixture(t)

	// 1. Asker (claude-code) sends --ask to codex. This records claude-code's
	//    asker-owned `.owed` obligation keyed (To:codex, From:claude-code, Seq).
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--ask", "--body", "please review the diff"}); err != nil {
			t.Fatal(err)
		}
	})

	owed, err := loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 1 {
		t.Fatalf("after --ask, .owed has %d entries, want 1", len(owed.Owed))
	}
	key := owed.Owed[0].Key()
	if key.To != "codex" || key.From != "claude-code" {
		t.Fatalf("owed key = %+v, want To=codex From=claude-code", key)
	}
	ref := key.RefString()

	// 2. Recipient (codex) replies with --reply-to <ref>. Only in_reply_to is
	//    emitted; there is NO recipient-side ledger to mutate. Confirm the wire
	//    message carries the ref.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "codex", "--to", "claude-code",
			"--reply-to", ref, "--body", "looks good"}); err != nil {
			t.Fatal(err)
		}
	})
	replyBody := readMostRecentInboxMessage(t, cfg, "claude-code")
	if !strings.Contains(replyBody, "in_reply_to: "+ref) {
		t.Errorf("reply frontmatter missing in_reply_to %q:\n%s", ref, replyBody)
	}

	// 3. Asker (claude-code) runs check. Consuming the reply whose in_reply_to
	//    matches the outstanding `.owed` entry discharges it (ClearOwed).
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "claude-code"}) }); err != nil {
			t.Fatal(err)
		}
	})

	after, err := loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Owed) != 0 {
		t.Errorf(".owed has %d entries after the asker consumed the reply, want 0 (ClearOwed did not discharge): %+v", len(after.Owed), after.Owed)
	}
}

// --reply-to with no outstanding obligation is silent OK (the reference is
// just a threading hint; there is no recipient-side ledger, and an asker with
// no matching `.owed` entry simply delivers the reply with in_reply_to).
func TestSendReplyToWithoutMatchingEntryIsSilent(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--reply-to", "to-codex_from-claude-code_seq-00000000000000000009",
			"--body", "b"})
		if err != nil {
			t.Errorf("err = %v, want nil (unmatched --reply-to is a benign threading hint)", err)
		}
	})
}

func TestSendJSONShape(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex",
				"--body", "b", "--json"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var got sendResult
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.From != "claude-code" || got.To != "codex" {
			t.Errorf("from/to mismatch: %+v", got)
		}
		if got.Filename == "" {
			t.Error("Filename empty")
		}
	})
}

// Real-bake follow-up: self-send with --ask is a loop hazard (claude-code
// owes claude-code a reply). Today the delivery succeeds but stderr must
// warn so the operator pauses on the unusual shape. Per AGENTCHUTE.md
// §6.4, replies should default reply_required: false — a warning here
// reinforces that convention at the CLI surface.
func TestSendWarnsOnSelfSendWithAsk(t *testing.T) {
	root, _ := setupSendFixture(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()
	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		bs := make([]byte, 1024)
		for {
			n, _ := r.Read(bs)
			if n > 0 {
				buf.Write(bs[:n])
			}
			if n == 0 {
				close(done)
				return
			}
		}
	}()

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "claude-code",
			"--ask", "--body", "self-obligation"}); err != nil {
			t.Fatal(err)
		}
	})
	_ = w.Close()
	<-done

	got := buf.String()
	if !strings.Contains(got, "self-send") || !strings.Contains(got, "--ask") {
		t.Errorf("stderr missing self-send + --ask warning:\n%s", got)
	}
}

// Codex pre-merge ask: assert --reply-to does NOT implicitly set
// reply_required. The two flags are orthogonal per AGENTCHUTE.md §6.4:
// reply_required MUST NOT be inferred or propagated from in_reply_to.
func TestSendReplyToWithoutAskDoesNotSetReplyRequired(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--reply-to", "2026-05-19T00:00:00.000000Z",
			"--body", "thanks"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "reply_required:") {
			t.Errorf("--reply-to without --ask leaked reply_required line: %q (full:\n%s)", line, body)
		}
	}
}
