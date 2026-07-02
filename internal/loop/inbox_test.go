package loop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSeqInbox drops a canonical (from,seq) message into inbox via the
// production seq writer and returns the resulting Message the way the removed
// WriteInboxMessage did. Test-only fixture replacing the deleted legacy nonce
// writer; seq must be unique per (from) within inbox to avoid a link collision.
func writeSeqInbox(t *testing.T, inbox, from string, seq uint64, content []byte) Message {
	t.Helper()
	id := MsgID{From: from, Seq: seq}
	if _, err := writeSeqMessage(inbox, id, content); err != nil {
		t.Fatal(err)
	}
	return Message{
		Path:     filepath.Join(inbox, id.Filename()),
		Filename: id.Filename(),
		Sender:   from,
	}
}

func TestWriteListArchiveMessage(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	archive := filepath.Join(root, "archive")

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	msg := writeSeqInbox(t, inbox, "codex", 1, []byte("hello\n"))
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
	consumed := time.Date(2026, 5, 9, 16, 33, 0, 0, time.UTC)

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	msg := writeSeqInbox(t, inbox, "codex", 1, []byte("hello\n"))

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

func TestArchiveMessageIdempotentWhenSourceAlreadyArchived(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	archive := filepath.Join(root, "archive")
	consumed := time.Date(2026, 5, 9, 16, 33, 0, 0, time.UTC)

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	msg := writeSeqInbox(t, inbox, "codex", 1, []byte("hello\n"))

	first, err := ArchiveMessage(msg, archive, "claude-code", consumed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ArchiveMessage(msg, archive, "claude-code", consumed)
	if err != nil {
		t.Fatalf("re-archiving an already moved message should be idempotent: %v", err)
	}
	if second != first {
		t.Fatalf("second archive path = %q, want %q", second, first)
	}
	if _, err := os.Stat(msg.Path); !os.IsNotExist(err) {
		t.Fatalf("source should remain absent after idempotent archive, stat err=%v", err)
	}
	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("archive entries = %d, want 1", len(entries))
	}
}

func TestArchiveMessageIdempotentWhenDestinationIsSameFile(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	archive := filepath.Join(root, "archive")
	consumed := time.Date(2026, 5, 9, 16, 33, 0, 0, time.UTC)

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	msg := writeSeqInbox(t, inbox, "codex", 1, []byte("hello\n"))
	dest := ArchiveMessageDest(msg, archive, "claude-code", consumed)
	if err := os.Link(msg.Path, dest); err != nil {
		t.Skipf("hardlink setup unavailable: %v", err)
	}

	got, err := ArchiveMessage(msg, archive, "claude-code", consumed)
	if err != nil {
		t.Fatalf("same-inode archive collision should be idempotent: %v", err)
	}
	if got != dest {
		t.Fatalf("archive path = %q, want %q", got, dest)
	}
	if _, err := os.Stat(msg.Path); !os.IsNotExist(err) {
		t.Fatalf("source should be removed after same-inode idempotent archive, stat err=%v", err)
	}
	if body, err := os.ReadFile(dest); err != nil || string(body) != "hello\n" {
		t.Fatalf("archive body = %q err=%v, want hello", body, err)
	}
}

func TestWriteSeqMessageCleansTempFile(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	msg := writeSeqInbox(t, inbox, "codex", 1, []byte("hello\n"))
	if _, err := os.Stat(msg.Path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempFilePrefix) {
			t.Fatalf("temp file should be cleaned after successful link; found %s", e.Name())
		}
	}
}

// ListInboxMessagesWithSkipped must surface files that look like message
// attempts but fail seq parsing (hand-written with a bad seq), while keeping
// expected noise (.tmp_*, dotfiles, dirs, symlinks) silent.
func TestListInboxMessagesWithSkippedReportsMalformedNames(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox", "claude-code")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Valid message — should appear in msgs.
	writeSeqInbox(t, inbox, "codex", 1, []byte("hello\n"))

	// 2. Malformed: seq-shaped name with a non-numeric seq (a peer hand-writing
	//    a broken canonical name).
	mustWrite(t, filepath.Join(inbox, "from-gemini-cli_seq-notdigits.md"),
		[]byte("---\nfrom: gemini-cli\n---\n"))

	// 3. Malformed: no canonical structure at all.
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
	// Sorted lexicographically ('f' < 's').
	want := []string{
		"from-gemini-cli_seq-notdigits.md",
		"stray-message.md",
	}
	for i, name := range want {
		if skipped[i] != name {
			t.Errorf("skipped[%d] = %q, want %q", i, skipped[i], name)
		}
	}
}

func TestListInboxMessagesWithSkippedIgnoresVanishedEntries(t *testing.T) {
	oldReadInboxDir := readInboxDir
	t.Cleanup(func() { readInboxDir = oldReadInboxDir })

	inbox := t.TempDir()
	valid := MsgID{From: "codex", Seq: 1}.Filename()
	vanished := MsgID{From: "gemini-cli", Seq: 2}.Filename()
	readInboxDir = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{
			fakeDirEntry{name: ".tmp_in-flight", infoErr: &os.PathError{Op: "lstat", Path: ".tmp_in-flight", Err: os.ErrNotExist}},
			fakeDirEntry{name: vanished, infoErr: &os.PathError{Op: "lstat", Path: vanished, Err: os.ErrNotExist}},
			fakeDirEntry{name: valid, mode: 0o600},
		}, nil
	}

	msgs, skipped, err := ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Filename != valid {
		t.Fatalf("msgs = %#v, want one surviving message %s", msgs, valid)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want empty; vanished files should be ignored", skipped)
	}
}

func TestListInboxMessagesWithSkippedReturnsNonVanishedInfoError(t *testing.T) {
	oldReadInboxDir := readInboxDir
	t.Cleanup(func() { readInboxDir = oldReadInboxDir })

	infoErr := &os.PathError{Op: "lstat", Path: "blocked", Err: os.ErrPermission}
	readInboxDir = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{fakeDirEntry{name: "blocked", infoErr: infoErr}}, nil
	}

	_, _, err := ListInboxMessagesWithSkipped(t.TempDir())
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want os.ErrPermission", err)
	}
}

type fakeDirEntry struct {
	name    string
	mode    os.FileMode
	infoErr error
}

func (e fakeDirEntry) Name() string      { return e.name }
func (e fakeDirEntry) IsDir() bool       { return e.mode.IsDir() }
func (e fakeDirEntry) Type() os.FileMode { return e.mode.Type() }
func (e fakeDirEntry) Info() (os.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return fakeFileInfo{name: e.name, mode: e.mode}, nil
}

type fakeFileInfo struct {
	name string
	mode os.FileMode
}

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return 0 }
func (i fakeFileInfo) Mode() os.FileMode  { return i.mode }
func (i fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (i fakeFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeFileInfo) Sys() any           { return nil }

// InferSenderFromFilename recovers the sender from a canonical seq filename.
// The legacy nonce-name inference path was removed in v0.9.0, so a legacy
// `_msg-`-shaped name (tombstone case) is no longer attributed.
func TestInferSenderFromFilenameRecoversSeqSender(t *testing.T) {
	cases := []struct {
		name       string
		filename   string
		wantSender string
		wantOK     bool
	}{
		{"valid seq", MsgID{From: "codex", Seq: 7}.Filename(), "codex", true},
		{"valid seq, hyphenated sender", MsgID{From: "gemini-cli", Seq: 1}.Filename(), "gemini-cli", true},
		// Tombstone: the removed legacy nonce format is no longer inferred.
		{"legacy nonce name not inferred", "2026-05-09T16-32-00-123456Z_from-codex_msg-abcd.md", "", false},
		{"seq not zero-padded to 20", "from-codex_seq-7.md", "", false},
		{"invalid slug (uppercase)", "from-CODEX_seq-00000000000000000001.md", "", false},
		{"unstructured", "stray-message.md", "", false},
		{"empty filename", "", "", false},
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
	src := filepath.Join(inbox, "garbage_from-gemini-cli.md")
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
	wantSuffix := "_to-claude-code_garbage_from-gemini-cli.md"
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
	link := filepath.Join(inbox, MsgID{From: "codex", Seq: 1}.Filename())
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

// A seq message's advisory Timestamp is populated from the file mtime (a seq
// name has no embedded timestamp). It is load-bearing for staleness/display,
// never an ordering key.
func TestListInboxMessagesSeqTimestampFromMtime(t *testing.T) {
	inbox := t.TempDir()
	writeSeqInbox(t, inbox, "alice", 1, []byte("seq"))
	path := filepath.Join(inbox, MsgID{From: "alice", Seq: 1}.Filename())

	mtime := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	msgs, err := ListInboxMessages(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d, want 1", len(msgs))
	}
	if msgs[0].Timestamp.IsZero() {
		t.Fatal("seq Timestamp must not be zero (would mark every seq message ancient)")
	}
	if !msgs[0].Timestamp.UTC().Truncate(time.Second).Equal(mtime.UTC().Truncate(time.Second)) {
		t.Fatalf("seq Timestamp = %s, want ~mtime %s", msgs[0].Timestamp.UTC(), mtime.UTC())
	}
}
