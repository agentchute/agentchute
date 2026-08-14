package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// send_body_file_test.go covers `agentchute send --body-file <path>`: the
// no-shell body path that exists so a lane holding the §15 guard latch can
// still send a REAL (multi-line) reply. Every other multi-line body form
// (`< file`, a pipe, `--body "$(cat file)"`) is executable shell syntax and is
// rejected by the guard's inert-direct-send tokenizer, which made a same-turn
// reply mechanically impossible — lanes parked their reply "for next turn" and
// then never got one, because the pull-only wake never fires on an empty
// inbox. guard_test.go's TestGuardDirectSendDataSinkException holds the other
// half: that a --body-file invocation stays inert under the CURRENT tokenizer,
// with no guard.go change.
//
// Fixture ids come from setupSendFixture (send_v011_test.go): claude-code
// sends, codex receives.

func bodyFileInboxCount(t *testing.T, cfg *loop.Config, agent string) int {
	t.Helper()
	entries, err := os.ReadDir(cfg.AgentInboxDir(agent))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

func assertNothingDelivered(t *testing.T, cfg *loop.Config) {
	t.Helper()
	if n := bodyFileInboxCount(t, cfg, "codex"); n != 0 {
		t.Fatalf("expected no delivered message, got %d", n)
	}
}

// The body must land VERBATIM. The fixture body deliberately carries the exact
// characters that get mangled when a body travels through a shell instead of
// through the binary: backticks and `$(...)` are evaluated by an unquoted
// heredoc (which has silently blanked real messages on this bus), and
// `$AGENTCHUTE_SERVE_TOKEN` is the expansion that turned an earlier guard
// exception into a token-exfil path. Reading the file in-process means none of
// them is ever handed to a shell.
func TestSendBodyFileDeliversFileContentVerbatim(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		body := "## Report\n\nline one\nline two with `backticks` and $(echo hi)\n\n- $AGENTCHUTE_SERVE_TOKEN stays literal\n"
		path := filepath.Join(t.TempDir(), "reply.md")
		mustWrite(t, path, []byte(body))

		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", path}); err != nil {
			t.Fatalf("cmdSend --body-file: %v", err)
		}
		got := readMostRecentInboxMessage(t, cfg, "codex")
		if !strings.Contains(got, body) {
			t.Errorf("delivered message does not contain the file body verbatim.\ngot:\n%s\nwant body:\n%s", got, body)
		}
	})
}

// An empty file is a legal (empty) body, not an error, and must NOT fall
// through to the stdin path — falling through would make an empty file
// indistinguishable from "no body flag at all" and pick up whatever happens to
// be on stdin.
func TestSendBodyFileEmptyFileDoesNotFallBackToStdin(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		path := filepath.Join(t.TempDir(), "empty.md")
		mustWrite(t, path, []byte(""))

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString("STDIN BODY MUST NOT BE USED"); err != nil {
			t.Fatal(err)
		}
		w.Close()
		defer r.Close()
		prev := sendStdin
		sendStdin = r
		defer func() { sendStdin = prev }()

		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", path}); err != nil {
			t.Fatalf("cmdSend --body-file (empty file): %v", err)
		}
		if got := readMostRecentInboxMessage(t, cfg, "codex"); strings.Contains(got, "STDIN BODY MUST NOT BE USED") {
			t.Errorf("--body-file with an empty file fell back to stdin:\n%s", got)
		}
	})
}

func TestSendBodyFileMissingFileIsHardError(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		path := filepath.Join(t.TempDir(), "nope.md")
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", path})
		if err == nil {
			t.Fatal("expected an error for a missing --body-file path")
		}
		if !strings.Contains(err.Error(), "--body-file") {
			t.Errorf("error should name the flag, got: %v", err)
		}
		assertNothingDelivered(t, cfg)
	})
}

// A directory is a readable path but not a readable body; it must fail the
// same way a missing file does rather than delivering garbage or nothing.
func TestSendBodyFileDirectoryIsHardError(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", t.TempDir()})
		if err == nil {
			t.Fatal("expected an error when --body-file points at a directory")
		}
		assertNothingDelivered(t, cfg)
	})
}

func TestSendBodyAndBodyFileAreMutuallyExclusive(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		path := filepath.Join(t.TempDir(), "reply.md")
		mustWrite(t, path, []byte("from the file"))

		err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body", "inline", "--body-file", path})
		if err == nil {
			t.Fatal("expected --body and --body-file together to be refused")
		}
		if !strings.Contains(err.Error(), "--body-file") || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("error should name both flags and say they are mutually exclusive, got: %v", err)
		}
		assertNothingDelivered(t, cfg)
	})
}

// An explicitly EMPTY --body still conflicts: the conflict is about which flag
// was PASSED, not about which one happens to be non-empty. Testing `body != ""`
// instead of flag presence would silently prefer the file here.
func TestSendEmptyBodyStillConflictsWithBodyFile(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		path := filepath.Join(t.TempDir(), "reply.md")
		mustWrite(t, path, []byte("from the file"))

		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body", "", "--body-file", path}); err == nil {
			t.Fatal(`expected --body "" together with --body-file to be refused`)
		}
		assertNothingDelivered(t, cfg)
	})
}

// --ask and --reply-to compose with a file body exactly as they do with
// --body: the `## ASK` heading is prepended to the file content, and both
// frontmatter fields are emitted.
func TestSendBodyFileComposesWithAskAndReplyTo(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		body := "please review\n\nsecond paragraph\n"
		path := filepath.Join(t.TempDir(), "ask.md")
		mustWrite(t, path, []byte(body))

		_, _, err := captureStdoutStderr(t, func() error {
			return cmdSend([]string{
				"--from", "claude-code", "--to", "codex",
				"--ask", "--reply-to", "some-prior-ref",
				"--body-file", path,
			})
		})
		if err != nil {
			t.Fatalf("cmdSend --body-file --ask --reply-to: %v", err)
		}
		got := readMostRecentInboxMessage(t, cfg, "codex")
		for _, want := range []string{"reply_required: true", "in_reply_to:", "some-prior-ref", "## ASK", body} {
			if !strings.Contains(got, want) {
				t.Errorf("delivered message missing %q:\n%s", want, got)
			}
		}
	})
}

// Security bound (the review question the brief raised): --body-file is a new
// file-read primitive available to a latched lane, which the guard otherwise
// gives no way to read a file at all. The loop's state/ tree holds serve.claim,
// whose serve_token is the live fence epoch — reading that into a peer's inbox
// is exactly the exfiltration path the guard tokenizer's own comment exists to
// prevent. Refuse the whole state/ tree, symlinks included.
func TestSendBodyFileRefusesLoopStateDir(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		stateDir := cfg.AgentStateDir("claude-code")
		mustMkdir(t, stateDir)
		claim := filepath.Join(stateDir, "serve.claim")
		mustWrite(t, claim, []byte(`{"id":"claude-code","serve_token":"deadbeef"}`))

		err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", claim})
		if err == nil {
			t.Fatal("expected --body-file to refuse a path inside the loop state/ tree")
		}
		if !strings.Contains(err.Error(), "state") {
			t.Errorf("refusal should name the state tree, got: %v", err)
		}
		assertNothingDelivered(t, cfg)

		// A symlink pointing into state/ must be refused too — resolving only
		// the lexical path would let `ln -s` walk straight around the check.
		link := filepath.Join(t.TempDir(), "innocent.md")
		if err := os.Symlink(claim, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", link}); err == nil {
			t.Fatal("expected --body-file to refuse a symlink into the loop state/ tree")
		}
		assertNothingDelivered(t, cfg)
	})
}

// The refusal is scoped to state/ ONLY: inbox, archive, and the agents/
// registry are wire-protocol-public to every peer in the pool, so quoting one
// back to a peer is ordinary coordination, not exfiltration. A blanket
// "nothing under the loop dir" rule would break that for no security gain.
func TestSendBodyFileAllowsNonStateLoopPaths(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		path := filepath.Join(cfg.LoopDir, "notes.md")
		mustWrite(t, path, []byte("shared notes\nsecond line\n"))

		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", path}); err != nil {
			t.Fatalf("--body-file on a non-state loop path should be allowed: %v", err)
		}
		if got := readMostRecentInboxMessage(t, cfg, "codex"); !strings.Contains(got, "shared notes") {
			t.Errorf("body not delivered:\n%s", got)
		}
	})
}

// Ordering: a bad --body-file is reported on its own terms, before the
// recipient preflight runs, so the operator sees the mistake they actually
// made instead of a downstream registration/freshness complaint that depends
// on live pool state.
func TestSendBodyFileErrorPrecedesRecipientPreflight(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		missing := filepath.Join(t.TempDir(), "nope.md")
		err := cmdSend([]string{"--from", "claude-code", "--to", "ghost", "--body-file", missing})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "--body-file") {
			t.Errorf("--body-file error should win over the unknown-recipient preflight, got: %v", err)
		}
	})
}

// The usage text must teach the flag; a body path nobody can discover is the
// same friction this change exists to remove.
func TestSendUsageDocumentsBodyFile(t *testing.T) {
	usage := sendUsage(os.ErrInvalid).Error()
	if !strings.Contains(usage, "--body-file") {
		t.Errorf("send usage does not mention --body-file:\n%s", usage)
	}
}
