package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// check_a6_test.go — v2.5 implementation plan slice A6: the corrective
// auto-send is deleted; quarantine + a local warning are the only reaction
// to malformed inbox content, and no outbound mail is ever manufactured
// about it.

// TestCheckQuarantinesMalformedFilenameWithoutCorrective pins A6's done-when
// directly: a malformed (non-§6.1) filename is quarantined with a local
// warning, and — since the corrective send this used to trigger is deleted —
// no peer's inbox receives anything as a result.
func TestCheckQuarantinesMalformedFilenameWithoutCorrective(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	inboxDir := cfg.AgentInboxDir("alice")
	// Looks like it came from "bob" but fails the §6.1 canonical filename
	// encoding (missing the zero-padded seq segment) — exactly the shape
	// that used to trigger InferSenderFromFilename + SendCorrective to bob.
	malformed := filepath.Join(inboxDir, "from-bob_not-a-valid-seq.md")
	if err := os.WriteFile(malformed, []byte("---\nfrom: bob\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderrOut string
	withCwd(t, root, func() {
		stderrOut = captureStderr(t, func() {
			_, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
			if err != nil {
				t.Fatal(err)
			}
		})
	})

	if !strings.Contains(stderrOut, "quarantined") {
		t.Fatalf("expected a local quarantine warning; got:\n%s", stderrOut)
	}
	if strings.Contains(stderrOut, "notified") {
		t.Fatalf("expected no corrective-notify output (deleted in A6); got:\n%s", stderrOut)
	}
	if _, err := os.Stat(malformed); !os.IsNotExist(err) {
		t.Fatalf("malformed file was not moved out of the inbox: stat err = %v", err)
	}

	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("bob's inbox received %d message(s); want 0 (no corrective auto-send)", len(msgs))
	}
}

// TestCheckQuarantinesMalformedFrontmatterWithoutCorrective is the frontmatter
// half: a validly-named message whose frontmatter block fails to parse is
// quarantined with a local warning, and — same as above — no corrective is
// sent to the (filename-known) sender.
func TestCheckQuarantinesMalformedFrontmatterWithoutCorrective(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	mustWriteSeqInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte("---\nfrom: bob\nthis line has no colon\n---\n\nbody\n"))

	var stderrOut string
	withCwd(t, root, func() {
		stderrOut = captureStderr(t, func() {
			_, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
			if err != nil {
				t.Fatal(err)
			}
		})
	})

	if !strings.Contains(stderrOut, "quarantined") {
		t.Fatalf("expected a local quarantine warning; got:\n%s", stderrOut)
	}
	if strings.Contains(stderrOut, "notified") {
		t.Fatalf("expected no corrective-notify output (deleted in A6); got:\n%s", stderrOut)
	}

	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("bob's inbox received %d message(s); want 0 (no corrective auto-send)", len(msgs))
	}
}
