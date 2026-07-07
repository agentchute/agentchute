package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

func withFakeTmuxTargets(t *testing.T, targets ...string) {
	t.Helper()
	old := tmuxProbeBinary
	path := filepath.Join(t.TempDir(), "tmux")
	var cases strings.Builder
	for _, target := range targets {
		cases.WriteString("  '")
		cases.WriteString(target)
		cases.WriteString("') exit 0 ;;\n")
	}
	script := "#!/bin/sh\n" +
		"target=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"-t\" ]; then shift; target=\"$1\"; fi\n" +
		"  shift || true\n" +
		"done\n" +
		"case \"$target\" in\n" +
		cases.String() +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxProbeBinary = path
	t.Cleanup(func() {
		tmuxProbeBinary = old
	})
}

// Pull-only (Gate 6c): setTmuxPaneLockObserver was removed with the tmux
// pane-registration lock it observed.

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

// mustWriteLiveAt writes a `.live` presence fact for agentID with an explicit
// last_seen, used by Gate 3 readers' tests to force a fresh OR stale presence
// independently of registration last_seen (the freshness SOURCE is now `.live`).
// It writes the same on-disk shape loop.WriteLive produces (the exported
// loop.Live struct + the <loop>/live/<id>.live path), so loop.ReadLive /
// loop.LiveLastSeen read it back.
func mustWriteLiveAt(t *testing.T, cfg *loop.Config, agentID string, lastSeen time.Time) {
	t.Helper()
	live := loop.Live{
		ID:       agentID,
		LastSeen: lastSeen.UTC(),
		Busy:     false,
		PID:      os.Getpid(),
		Host:     "test-host",
	}
	data, err := json.MarshalIndent(live, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	mustWrite(t, filepath.Join(cfg.LoopDir, "live", agentID+".live"), data)
}

func mustWriteFreshPollerHeartbeat(t *testing.T, cfg *loop.Config, agentID string) {
	t.Helper()
	if err := loop.SavePollerHeartbeat(cfg, loop.PollerHeartbeat{
		AgentID:         agentID,
		Method:          "test",
		Host:            "test-host",
		IntervalSeconds: loop.DefaultPollerIntervalSeconds,
		LaunchEnabled:   true,
		LastSeen:        time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func mustExampleRepo(t *testing.T, root string) {
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
}
