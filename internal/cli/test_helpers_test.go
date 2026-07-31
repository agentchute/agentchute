package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mustWriteSeqInbox drops a canonical (from,seq) message file directly into
// inboxDir. It replaces the removed loop.WriteInboxMessage nonce-writer as a
// test fixture: the on-disk name is the canonical from-<from>_seq-<020d>.md that
// production's send path writes, so listers/gates/pending/status treat it as a
// real message. Seq must be unique per (from) within inboxDir to avoid collision.
func mustWriteSeqInbox(t *testing.T, inboxDir, from string, seq uint64, content []byte) {
	t.Helper()
	name := loop.MsgID{From: from, Seq: seq}.Filename()
	mustWrite(t, filepath.Join(inboxDir, name), content)
}

func mustWriteTsInbox(t *testing.T, inboxDir string, id loop.TsID, content []byte) {
	t.Helper()
	mustWrite(t, filepath.Join(inboxDir, id.Filename()), content)
}

// mustWriteAgedInbox is mustWriteSeqInbox plus a back-dated mtime, for tests
// exercising the check age banner (v2.5 plan A3, C18): age is sourced from
// file mtime today (Message.Timestamp), so back-dating mtime is how a test
// makes a message read as `age` old.
func mustWriteAgedInbox(t *testing.T, inboxDir, from string, seq uint64, content []byte, age time.Duration) {
	t.Helper()
	mustWriteSeqInbox(t, inboxDir, from, seq, content)
	path := filepath.Join(inboxDir, loop.MsgID{From: from, Seq: seq}.Filename())
	aged := time.Now().Add(-age)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
}

// clearGuardEnv resets both guard-enablement env vars (v2.5 plan A7/C22) to
// empty, via t.Setenv so the ambient process env can never leak a serve
// token/guard bit into a test that must run "as if this session were never
// launched under `ac serve`" — see the known go-test-under-runner env-leak
// class this codebase already works around elsewhere.
func clearGuardEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTCHUTE_SERVE_TOKEN", "")
	t.Setenv("AGENTCHUTE_GUARD", "")
}

// Pull-only (Gate 6c): setTmuxPaneLockObserver was removed with the tmux
// pane-registration lock it observed. v2.5 plan B5: withFakeTmuxTargets (its
// last caller, the presence scan's tmux enumeration) is gone too.

func mustWriteCanonicalHook(t *testing.T, root, wrapper string) {
	t.Helper()
	for _, h := range hookWrappers {
		if h.Name != wrapper {
			continue
		}
		data, err := fs.ReadFile(hooksFS, h.Src)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(root, h.Dest), data)
		return
	}
	t.Fatalf("unknown hook wrapper %q", wrapper)
}

// mustWriteStaleHook writes an old-binary-shape hook file for wrapper — a
// UserPromptSubmit hook invoking `poller ensure`, the exact subcommand the
// v1.5.0 cutover removed (docs/decisions/agentchute-v150-cutover-incident-
// and-fix.md) — so tests can reproduce the outage's stale-template shape
// without depending on which subcommand a future binary happens to remove.
func mustWriteStaleHook(t *testing.T, root, wrapper string) {
	t.Helper()
	for _, h := range hookWrappers {
		if h.Name != wrapper {
			continue
		}
		content := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"${AGENTCHUTE_BIN:-agentchute} poller ensure --vendor anthropic --quiet"}]}]}}`
		mustWrite(t, filepath.Join(root, h.Dest), []byte(content))
		return
	}
	t.Fatalf("unknown hook wrapper %q", wrapper)
}

func mustExampleRepo(t *testing.T, root string) {
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
}
