package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInboxFilenameRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 9, 16, 32, 0, 123456000, time.UTC)
	name := formatInboxFilename(now, "codex", "abcd")
	parsed, sender, nonce, err := ParseInboxFilename(name)
	if err != nil {
		t.Fatal(err)
	}
	if sender != "codex" {
		t.Fatalf("sender = %q, want codex", sender)
	}
	if nonce != "abcd" {
		t.Fatalf("nonce = %q, want abcd", nonce)
	}
	if !parsed.Equal(now) {
		t.Fatalf("timestamp = %s, want %s", parsed, now)
	}
}

func TestParseInboxFilenameRejectsInvalidCalendarDate(t *testing.T) {
	_, _, _, err := ParseInboxFilename("2026-02-31T16-32-00-123456Z_from-codex_msg-abcd.md")
	if err == nil {
		t.Fatal("expected invalid calendar date error")
	}
}

func TestWriteListArchiveMessage(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	archive := filepath.Join(root, "archive")
	now := time.Date(2026, 5, 9, 16, 32, 0, 123456000, time.UTC)

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	msg, err := WriteInboxMessage(inbox, now, "codex", []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(msg.Path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(msg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("message mode = %o, want 600", got)
	}

	msgs, err := ListInboxMessages(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Filename != msg.Filename {
		t.Fatalf("messages = %#v, want one %s", msgs, msg.Filename)
	}

	consumed := time.Date(2026, 5, 9, 16, 33, 0, 0, time.UTC)
	if _, err := ArchiveMessage(msgs[0], archive, "claude-code", consumed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(msg.Path); !os.IsNotExist(err) {
		t.Fatalf("source still exists or unexpected stat error: %v", err)
	}

	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	archiveInfo, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if got := archiveInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("archive dir mode = %o, want 700", got)
	}
	if len(entries) != 1 {
		t.Fatalf("archive entries = %d, want 1", len(entries))
	}
	want := "2026-05-09T16-33-00Z_to-claude-code_" + msg.Filename
	if entries[0].Name() != want {
		t.Fatalf("archive filename = %q, want %q", entries[0].Name(), want)
	}
}

func TestArchiveMessageRejectsExistingDestination(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	archive := filepath.Join(root, "archive")
	now := time.Date(2026, 5, 9, 16, 32, 0, 123456000, time.UTC)
	consumed := time.Date(2026, 5, 9, 16, 33, 0, 0, time.UTC)

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	msg, err := WriteInboxMessage(inbox, now, "codex", []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(archive, "2026-05-09T16-33-00Z_to-claude-code_"+msg.Filename)
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ArchiveMessage(msg, archive, "claude-code", consumed); err == nil {
		t.Fatal("expected archive collision error")
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "existing\n" {
		t.Fatalf("archive destination overwritten: got %q err %v", got, err)
	}
	if _, err := os.Stat(msg.Path); err != nil {
		t.Fatalf("source removed after failed archive: %v", err)
	}
}

func TestWriteInboxMessageIgnoresTempCleanupError(t *testing.T) {
	oldRemoveFile := removeFile
	t.Cleanup(func() {
		removeFile = oldRemoveFile
	})
	removeFile = func(string) error {
		return errors.New("cleanup failed")
	}

	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	now := time.Date(2026, 5, 9, 16, 32, 0, 123456000, time.UTC)
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	msg, err := WriteInboxMessage(inbox, now, "codex", []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(msg.Path); err != nil {
		t.Fatal(err)
	}
	// The temp file now uses a unique os.CreateTemp name (not the deterministic
	// .tmp_<final>), so locate the leftover by its tempFilePrefix rather than an
	// exact name.
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	foundTemp := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempFilePrefix) {
			foundTemp = true
			break
		}
	}
	if !foundTemp {
		t.Fatalf("a temp file should remain after fake cleanup failure; inbox entries: %v", entries)
	}
}

// TestWriteInboxMessage_ConcurrentSameSenderNoBodyMixup forces the temp-path
// collision window: N goroutines write distinct bodies as the same sender at a
// FIXED `now`, and generateNonce is pinned to a tiny pool so many attempts
// produce the same final filename (and, under the old deterministic
// `.tmp_<final>` scheme, the SAME temp path). With a shared temp, a late writer
// could clobber an earlier writer's body before it hard-linked the inode,
// delivering the wrong body to the winner. A unique temp per attempt
// (os.CreateTemp) makes each body land in its own inode, so every delivered
// file's body matches the writer that produced its filename.
func TestWriteInboxMessage_ConcurrentSameSenderNoBodyMixup(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pin generateNonce so concurrent writers at the same `now` heavily collide
	// on the final filename (and, under the OLD scheme, the SAME temp path),
	// exercising the clobber window — but guarantee progress: the FIRST handful
	// of draws cycle a tiny colliding pool; once exhausted, draws become unique
	// (a monotonic counter) so every writer eventually wins a distinct final
	// filename. Under the old deterministic temp, a clobbered body would surface
	// as a body mismatch on the winner; under the fix it cannot.
	oldGen := generateNonce
	t.Cleanup(func() { generateNonce = oldGen })
	collidingPool := []string{"aaaa", "bbbb", "cccc"}
	const collidingDraws = 18 // > n, so the early concurrent rush collides hard
	var nonceMu sync.Mutex
	draw := 0
	generateNonce = func() (string, error) {
		nonceMu.Lock()
		defer nonceMu.Unlock()
		d := draw
		draw++
		if d < collidingDraws {
			return collidingPool[d%len(collidingPool)], nil
		}
		// Unique tail: 4 lowercase hex chars derived from the draw counter so
		// retries always find a free final filename and the test terminates.
		return fmt.Sprintf("%04x", 0x1000+(d-collidingDraws)), nil
	}

	now := time.Date(2026, 5, 9, 16, 32, 0, 123456000, time.UTC)
	const n = 24

	type result struct {
		path string
		body string
	}
	results := make([]result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf("---\nfrom: codex\nto: claude-code\n---\n\nbody-marker-%03d\n", i)
			<-start
			msg, err := WriteInboxMessage(inbox, now, "codex", []byte(body))
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = result{path: msg.Path, body: body}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}
	// Every delivered file must contain exactly the body its own writer wrote.
	for i, r := range results {
		got, err := os.ReadFile(r.path)
		if err != nil {
			t.Fatalf("writer %d: read delivered file %s: %v", i, r.path, err)
		}
		if string(got) != r.body {
			t.Fatalf("writer %d: delivered file %s body mismatch\n got: %q\nwant: %q",
				i, r.path, string(got), r.body)
		}
	}
	// And every writer got a distinct final path (no two writers claimed one file).
	seen := make(map[string]int)
	for i, r := range results {
		if prev, ok := seen[r.path]; ok {
			t.Fatalf("writers %d and %d both delivered to %s", prev, i, r.path)
		}
		seen[r.path] = i
	}
}

// ListInboxMessagesWithSkipped must surface files that look like message
// attempts but fail §6.1 parsing (hand-written with bad nonces/timestamps),
// while keeping expected noise (.tmp_*, dotfiles, dirs, symlinks) silent.
func TestListInboxMessagesWithSkippedReportsMalformedNames(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Valid message — should appear in msgs.
	now := time.Date(2026, 5, 9, 16, 32, 0, 123456000, time.UTC)
	if _, err := WriteInboxMessage(inbox, now, "codex", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	// 2. Malformed: gemini-style invalid nonce (g, l, c, i are not hex).
	mustWrite(t, filepath.Join(inbox, "2026-05-11T17-30-00-000000Z_from-gemini-cli_msg-gcli.md"),
		[]byte("---\nfrom: gemini-cli\n---\n"))

	// 3. Malformed: missing the _from-/_msg- structure entirely.
	mustWrite(t, filepath.Join(inbox, "stray-message.md"),
		[]byte("not a agentchute message"))

	// 4. Expected noise (must NOT appear in skipped).
	mustWrite(t, filepath.Join(inbox, ".DS_Store"), []byte{})
	mustWrite(t, filepath.Join(inbox, ".tmp_in-flight"), []byte{})
	if err := os.MkdirAll(filepath.Join(inbox, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	msgs, skipped, err := ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d, want 1", len(msgs))
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v, want 2 entries (gemini-cli + stray)", skipped)
	}
	// Sorted (stray-message.md sorts before 2026-...).
	want := []string{
		"2026-05-11T17-30-00-000000Z_from-gemini-cli_msg-gcli.md",
		"stray-message.md",
	}
	for i, name := range want {
		if skipped[i] != name {
			t.Errorf("skipped[%d] = %q, want %q", i, skipped[i], name)
		}
	}
}

// InferSenderFromFilename should recover the sender when the filename retains
// §6.1 structural shape (`_from-<slug>_msg-...` + `.md`) but the timestamp or
// nonce is malformed (§11.1). Filenames missing the structural markers are
// too broken to reliably attribute and must be dropped without inference,
// even if the bare slug is recoverable from somewhere in the name.
func TestInferSenderFromFilenameRecoversFromMalformedNames(t *testing.T) {
	cases := []struct {
		name       string
		filename   string
		wantSender string
		wantOK     bool
	}{
		{"fully-valid §6.1", "2026-05-09T16-32-00-123456Z_from-codex_msg-abcd.md", "codex", true},
		{"bad nonce (non-hex)", "2026-05-09T16-32-00-123456Z_from-gemini-cli_msg-gcli.md", "gemini-cli", true},
		{"bad timestamp", "garbage-prefix_from-codex_msg-zzzz.md", "codex", true},
		{"no from segment", "2026-05-09T16-32-00-123456Z_msg-abcd.md", "", false},
		{"invalid slug (uppercase)", "_from-CODEX_msg-abcd.md", "", false},
		{"empty filename", "", "", false},
		// §11.1 narrowing: structural markers missing → no inference.
		{"missing _msg- segment", "2026-05-09T16-32-00-123456Z_from-codex_abcd.md", "", false},
		{"missing .md suffix", "2026-05-09T16-32-00-123456Z_from-codex_msg-abcd", "", false},
		{"only _from- segment", "junk_from-codex_junk", "", false},
	}
	for _, c := range cases {
		got, ok := InferSenderFromFilename(c.filename)
		if got != c.wantSender || ok != c.wantOK {
			t.Errorf("%s: got (%q, %v), want (%q, %v)",
				c.name, got, ok, c.wantSender, c.wantOK)
		}
	}
}

// InferSenderFromFrontmatter should extract `from:` from a YAML frontmatter
// block when the filename was already malformed and the sender capture failed.
func TestInferSenderFromFrontmatterReadsFromField(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.md")
	mustWrite(t, good, []byte(`---
from: gemini-cli
to: claude-code
task: hi
---

body
`))
	if got, ok := InferSenderFromFrontmatter(good); !ok || got != "gemini-cli" {
		t.Errorf("got (%q, %v), want (gemini-cli, true)", got, ok)
	}

	bad := filepath.Join(dir, "bad.md")
	mustWrite(t, bad, []byte(`no frontmatter at all
just a body
`))
	if got, ok := InferSenderFromFrontmatter(bad); ok {
		t.Errorf("expected ok=false for body-only, got (%q, true)", got)
	}

	invalid := filepath.Join(dir, "invalid.md")
	mustWrite(t, invalid, []byte(`---
from: BAD AGENT
to: claude-code
---
`))
	if got, ok := InferSenderFromFrontmatter(invalid); ok {
		t.Errorf("expected ok=false for invalid slug %q", got)
	}

	// Body lines that LOOK like frontmatter fields must NOT be inferred —
	// only the first ---/--- block counts.
	bodyOnly := filepath.Join(dir, "body-only.md")
	mustWrite(t, bodyOnly, []byte(`no frontmatter, just body text
that happens to mention from: codex deep in the message
about an unrelated topic.
`))
	if got, ok := InferSenderFromFrontmatter(bodyOnly); ok {
		t.Errorf("body-only file mis-inferred sender %q; should not match", got)
	}

	// Frontmatter block with closing --- ; body below it has another from:
	// that should be ignored.
	multi := filepath.Join(dir, "multi.md")
	mustWrite(t, multi, []byte(`---
from: claude-code
to: codex
---

Body discussing from: gemini-cli (not the real sender).
`))
	got, ok := InferSenderFromFrontmatter(multi)
	if !ok || got != "claude-code" {
		t.Errorf("multi-from file: got (%q, %v), want (claude-code, true)", got, ok)
	}
}

// QuarantineInboxFile moves a malformed file to malformed/ with a
// collision-resistant name, and refuses to overwrite an existing one.
func TestQuarantineInboxFile(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	malformed := filepath.Join(root, "malformed")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(inbox, "garbage_from-gemini-cli_msg-gcli.md")
	mustWrite(t, src, []byte("malformed content\n"))

	now := time.Date(2026, 5, 12, 4, 0, 0, 0, time.UTC)
	dest, err := QuarantineInboxFile(src, malformed, "claude-code", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still exists after quarantine: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read quarantined file: %v", err)
	}
	if string(got) != "malformed content\n" {
		t.Errorf("content not preserved through quarantine: %q", got)
	}
	wantSuffix := "_to-claude-code_garbage_from-gemini-cli_msg-gcli.md"
	if !strings.HasSuffix(dest, wantSuffix) {
		t.Errorf("quarantined name %q missing expected suffix %q", dest, wantSuffix)
	}

	// Second call with the same source name should not overwrite the first.
	mustWrite(t, src, []byte("second content\n"))
	if _, err := QuarantineInboxFile(src, malformed, "claude-code", now); err == nil {
		t.Fatal("expected collision error on second quarantine with same name + ts; got nil")
	}
}

func TestListInboxMessagesSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.md")
	if err := os.WriteFile(target, []byte("not an inbox message\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(inbox, "2026-05-09T16-32-00-123456Z_from-codex_msg-abcd.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	msgs, err := ListInboxMessages(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("symlink inbox entry was listed: %#v", msgs)
	}
}
