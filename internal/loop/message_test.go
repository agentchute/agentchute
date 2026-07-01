package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComposeMessageQuotesInReplyTo(t *testing.T) {
	msg := string(ComposeMessage(
		"codex",
		"2026-05-09T16:00:00.000000Z\nforged: true",
		"body",
	))

	// in_reply_to is the one optional scalar and must still be quoted when its
	// value contains a newline/colon (injection-safety).
	if want := `in_reply_to: "2026-05-09T16:00:00.000000Z\nforged: true"`; !strings.Contains(msg, want) {
		t.Fatalf("message missing %q:\n%s", want, msg)
	}

	// Envelope minimality (AGENTCHUTE.md §6.4, P1 residue cleanup): `to`,
	// `task`, `status` are gone from ComposeMessage's signature entirely, not
	// just unemitted — assert no frontmatter line carries those bare keys
	// (line-scoped so it does not false-match `in_reply_to:`).
	for _, line := range strings.Split(msg, "\n") {
		key, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		switch key {
		case "to", "task", "status":
			t.Fatalf("message unexpectedly emits %q line:\n%s", key, msg)
		}
	}
}

func TestValidateMessageFrontmatter(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name: "valid frontmatter + body",
			content: `---
from: codex
to: claude-code
---

body text
`,
			wantErr: false,
		},
		{
			name:    "body-only (no frontmatter at all)",
			content: "just body text, no frontmatter\n",
			wantErr: false,
		},
		{
			name: "opening --- but no closing ---",
			content: `---
from: codex
to: claude-code

body without closing
`,
			wantErr: true,
		},
		{
			name: "opening --- but body literally contains --- as scalar value",
			content: `---
from: codex
task: ---
to: claude-code
---

body
`,
			// Closing --- IS found at line 4 (because "task: ---" puts --- in
			// the scalar value, then line 4 is the actual closer). The parser
			// will see "task: ---" as a malformed value, but parseFrontmatter
			// in registration.go is tolerant of arbitrary scalar values.
			// This case actually parses OK; documenting it.
			wantErr: false,
		},
		{
			name: "garbage inside frontmatter block",
			content: `---
this is not a key:value line
nor is this
---

body
`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		err := ValidateMessageFrontmatter([]byte(c.content))
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

func TestCorrectiveBodyFormat(t *testing.T) {
	got := CorrectiveBody(
		".agentchute/loop/malformed/2026-05-12T04-00-00Z_to-claude-code_bad.md",
		"filename does not match the canonical seq format (§6.1)",
		"§6.1",
	)
	want := "malformed item: .agentchute/loop/malformed/2026-05-12T04-00-00Z_to-claude-code_bad.md\n" +
		"reason: filename does not match the canonical seq format (§6.1)\n" +
		"action: re-send per AGENTCHUTE.md §6.1\n"
	if got != want {
		t.Errorf("CorrectiveBody mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestSendCorrectiveWritesMessageAndSkipsPokeForEmptyTarget(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	newReg(t, cfg, "claude-code", "anthropic", "", "")    // self
	offender := newReg(t, cfg, "codex", "openai", "", "") // offender (pull-only: delivery is inbox-write only, no poke)

	msg, err := SendCorrective(cfg, "claude-code", offender.AgentID,
		".agentchute/loop/malformed/bad.md", "filename does not match §6.1", "§6.1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(msg.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	// protocol-v2 envelope cut: the corrective no longer emits task/status; its
	// content carries the §11.1 corrective body (CorrectiveBody) instead.
	for _, want := range []string{
		"malformed item: .agentchute/loop/malformed/bad.md",
		"reason: filename does not match §6.1",
		"action: re-send per AGENTCHUTE.md §6.1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("corrective message missing %q:\n%s", want, text)
		}
	}
}

func TestAnnounceEnrollmentNoPeers(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	self := newReg(t, cfg, "claude-code", "anthropic", "", "I do synthesis.")

	result, err := AnnounceEnrollment(cfg, self)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 || result.Sent != 0 {
		t.Fatalf("Total=%d Sent=%d, want both 0", result.Total, result.Sent)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", result.Warnings)
	}
}

func TestAnnounceEnrollmentSendsToPeersSkipsSelfAndExamples(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	self := newReg(t, cfg, "claude-code", "anthropic", "", "synthesis")
	newReg(t, cfg, "codex", "openai", "", "review")
	newReg(t, cfg, "gemini-cli", "google", "", "external review")

	// .example.md files exist alongside live registrations; must be skipped.
	mustWrite(t, filepath.Join(cfg.AgentsDir(), "codex.example.md"), []byte("---\nagent_id: codex\nvendor: openai\ncontrol_repo: "+cfg.ControlRepo+"\nlast_seen: 2026-05-09T16:08:36Z\nstatus: active\n---\n"))

	result, err := AnnounceEnrollment(cfg, self)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Fatalf("Total=%d, want 2 (codex + gemini-cli, self skipped, .example.md skipped)", result.Total)
	}
	if result.Sent != 2 {
		t.Fatalf("Sent=%d, want 2", result.Sent)
	}

	for _, peer := range []string{"codex", "gemini-cli"} {
		entries, err := os.ReadDir(cfg.AgentInboxDir(peer))
		if err != nil {
			t.Fatalf("read %s inbox: %v", peer, err)
		}
		if len(entries) != 1 {
			t.Errorf("%s inbox has %d entries, want 1", peer, len(entries))
		}
		content, err := os.ReadFile(filepath.Join(cfg.AgentInboxDir(peer), entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		if !strings.Contains(text, "Agent registration: claude-code (anthropic)") {
			t.Errorf("%s inbox missing declarative header:\n%s", peer, text)
		}
		if !strings.Contains(text, "synthesis") {
			t.Errorf("%s inbox missing bio body:\n%s", peer, text)
		}
		if strings.Contains(text, "Hi ") {
			t.Errorf("%s inbox contains chatty 'Hi ' salutation — should be declarative-neutral:\n%s", peer, text)
		}
	}

	// Self never got a message.
	if entries, err := os.ReadDir(cfg.AgentInboxDir("claude-code")); err != nil || len(entries) != 0 {
		t.Errorf("self inbox should be empty, got entries=%v err=%v", entries, err)
	}
}

func TestAnnounceEnrollmentMissingInboxIsWarningNotFatal(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	self := newReg(t, cfg, "claude-code", "anthropic", "", "synthesis")
	peer := newReg(t, cfg, "codex", "openai", "", "review")

	// Remove the peer's inbox dir to simulate a busted registration.
	if err := os.RemoveAll(cfg.AgentInboxDir(peer.AgentID)); err != nil {
		t.Fatal(err)
	}

	result, err := AnnounceEnrollment(cfg, self)
	if err != nil {
		t.Fatalf("missing inbox should not be fatal: %v", err)
	}
	if result.Total != 1 || result.Sent != 0 {
		t.Fatalf("Total=%d Sent=%d, want 1/0", result.Total, result.Sent)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected a warning for the missing inbox")
	}
}

func TestAnnouncementBodyDefaultPlaceholder(t *testing.T) {
	self := &Registration{AgentID: "claude-code", Vendor: "anthropic", Body: ""}
	body := announcementBody(self)
	if !strings.HasPrefix(body, "Agent registration: claude-code (anthropic)") {
		t.Errorf("missing declarative header:\n%s", body)
	}
	if !strings.Contains(body, "(no bio set") {
		t.Errorf("expected placeholder when bio empty:\n%s", body)
	}
}

func TestAnnouncementBodyEmbedsBio(t *testing.T) {
	self := &Registration{AgentID: "claude-code", Vendor: "anthropic", Body: "Good at: architecture, synthesis. Less good at: low-level perf."}
	body := announcementBody(self)
	if !strings.Contains(body, "Good at: architecture") {
		t.Errorf("bio not embedded:\n%s", body)
	}
	if strings.Contains(body, "(no bio set") {
		t.Errorf("placeholder leaked when bio present:\n%s", body)
	}
}

// Simple-again Gate 6a (pull-only): newRunnerReg and the three poke tests
// (TestAnnounceEnrollment_RefusesUnownedRunnerSocket,
// TestSendCorrective_RefusesUnownedRunnerSocket,
// TestAnnounceEnrollment_OwnedRunnerSocketPokes) were removed. Their subject —
// the registration-driven wake poke (and its recipient-binding refusal) inside
// AnnounceEnrollment / SendCorrective — no longer exists: both now deliver by
// the inbox file write alone and never poke.

func setupAnnounceFixture(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	if err := ensurePrivateDir(cfg.AgentsDir()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newReg(t *testing.T, cfg *Config, agentID, vendor, wakeTarget, body string) *Registration {
	t.Helper()
	_ = wakeTarget // pull-only (Gate 6c): registrations carry no wake target
	reg := &Registration{
		AgentID:     agentID,
		Vendor:      vendor,
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC(),
		Status:      StatusActive,
		Body:        body,
	}
	if err := WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(cfg.AgentInboxDir(agentID)); err != nil {
		t.Fatal(err)
	}
	return reg
}
