package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// withFakeHerdrList installs a fake `herdr` binary that answers `agent list`
// with the given name->pane bindings, logs `rename` invocations to renameLog,
// and reports `agent get` as not-found. Relocated verbatim (simple-again Gate
// 6a) from the deleted reachability_test.go into this shared test-helper file
// because register_test.go and herdr_state_test.go (both Gate 6c, untouched
// here) still depend on it.
func withFakeHerdrList(t *testing.T, renameLog string, bindings map[string]string) {
	t.Helper()
	old := herdrProbeBinary
	var items []string
	for name, pane := range bindings {
		items = append(items, fmt.Sprintf(`{"name":"%s","pane_id":"%s"}`, name, pane))
	}
	listJSON := fmt.Sprintf(`{"result":{"agents":[%s]}}`, strings.Join(items, ","))
	path := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\n" +
		"sub=\"$2\"\n" +
		"case \"$sub\" in\n" +
		"  list) printf '%s\\n' '" + listJSON + "' ; exit 0 ;;\n" +
		"  rename) printf '%s %s\\n' \"$3\" \"$4\" >> '" + renameLog + "' ; exit 0 ;;\n" +
		"  get) printf '{\"error\":{\"code\":\"agent_not_found\"}}\\n' ; exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrProbeBinary = path
	t.Cleanup(func() { herdrProbeBinary = old })
}

// renameLogContents reads a herdr rename log written by withFakeHerdrList,
// returning "" when the log was never written. Relocated verbatim (simple-again
// Gate 6a) from the deleted reachability_test.go for herdr_state_test.go.
func renameLogContents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

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

// mustWriteAgedInbox writes an inbox file and back-dates its filesystem mtime
// to `arrival`. The watchdog now derives message age from mtime (arrival on
// this host), not the sender-encoded filename timestamp, so tests that need an
// inbox file aged past the message-age threshold must control its mtime rather
// than rely on a past filename timestamp.
func mustWriteAgedInbox(t *testing.T, path string, arrival time.Time) {
	t.Helper()
	mustWrite(t, path, []byte("hi"))
	if err := os.Chtimes(path, arrival, arrival); err != nil {
		t.Fatal(err)
	}
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
		data, err := hooksFS.ReadFile(h.Src)
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

// mustExampleRepo / readExampleReg were relocated here from the deleted
// herdr_state_test.go (simple-again Gate 6c). They are shared fixture helpers
// used by presence/register/presenced tests, unrelated to the retired herdr probe.
func mustExampleRepo(t *testing.T, root string) {
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
}

func readExampleReg(t *testing.T, root, agentID string) *loop.Registration {
	t.Helper()
	reg, err := loop.ReadRegistration(filepath.Join(root, ".agentchute", "loop", "agents", agentID+".md"))
	if err != nil {
		t.Fatalf("read registration %s: %v", agentID, err)
	}
	return reg
}
