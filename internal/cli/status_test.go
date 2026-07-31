package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestPrintStatusIncludesAgentsAndInboxDepth(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	mustMkdir(t, cfg.AgentsDir())
	mustMkdir(t, cfg.AgentInboxDir("codex"))
	mustWriteSeqInbox(t, cfg.AgentInboxDir("codex"), "claude-code", 1, []byte("hi"))

	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	regs := map[string]*loop.Registration{
		"codex": {
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: root,
			LastSeen:    now.Add(-2 * time.Minute),
		},
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()
	for _, want := range []string{"control_repo:", "codex", "fresh"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output missing %q:\n%s", want, text)
		}
	}
	foundDepth := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "codex" && fields[2] == "1" {
			foundDepth = true
		}
	}
	if !foundDepth {
		t.Fatalf("status output missing inbox depth 1 for codex:\n%s", text)
	}
}

// v2.5 plan B5: the LAST_SEEN/AGE presence columns come directly from the
// registration's own last_seen (`.live` is deleted).
func TestStatusLastSeenAgeFromRegistration(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	mustMkdir(t, cfg.AgentsDir())

	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	lastSeen := now.Add(-2 * time.Minute)
	regs := map[string]*loop.Registration{
		"codex": {
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: root,
			LastSeen:    lastSeen,
		},
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()

	if got := statusColumnValue(t, text, "AGE", "codex"); got != "2m0s" {
		t.Errorf("codex AGE = %q, want 2m0s:\n%s", got, text)
	}
	if got, want := statusColumnValue(t, text, "LAST_SEEN", "codex"), lastSeen.UTC().Format(time.RFC3339); got != want {
		t.Errorf("codex LAST_SEEN = %q, want %q:\n%s", got, want, text)
	}
}

// v2.5 plan B5: STATUS is derived — fresh / stale (would sweep) / lease-held,
// via ReadServeClaim — instead of the old .live-published Status field.
func TestStatusColumnDerivesFreshStaleAndLeaseHeld(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	mustMkdir(t, cfg.AgentsDir())

	now := time.Now().UTC()
	regs := map[string]*loop.Registration{
		"fresh-agent": {
			AgentID:     "fresh-agent",
			Vendor:      "openai",
			ControlRepo: root,
			LastSeen:    now.Add(-time.Minute),
		},
		"stale-agent": {
			AgentID:     "stale-agent",
			Vendor:      "openai",
			ControlRepo: root,
			LastSeen:    now.Add(-2 * time.Hour),
		},
		"leased-agent": {
			AgentID:     "leased-agent",
			Vendor:      "openai",
			ControlRepo: root,
			// Old-looking row, but a live claim immunizes it (C12).
			LastSeen: now.Add(-2 * time.Hour),
		},
	}
	if _, err := loop.AcquireServeLease(cfg, "leased-agent"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()

	if got := statusColumnValue(t, text, "STATUS", "fresh-agent"); got != "fresh" {
		t.Errorf("fresh-agent STATUS = %q, want fresh:\n%s", got, text)
	}
	if got := statusColumnValue(t, text, "STATUS", "stale-agent"); got != "stale-would-sweep" {
		t.Errorf("stale-agent STATUS = %q, want stale-would-sweep:\n%s", got, text)
	}
	if got := statusColumnValue(t, text, "STATUS", "leased-agent"); got != "lease-held" {
		t.Errorf("leased-agent STATUS = %q, want lease-held:\n%s", got, text)
	}
}

func TestStatusProtocolVersionColumnAndWarnings(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	mustMkdir(t, cfg.AgentsDir())

	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	regs := map[string]*loop.Registration{
		"codex": {
			AgentID:         "codex",
			ProtocolVersion: loop.CurrentProtocolVersion,
			Vendor:          "openai",
			ControlRepo:     root,
			LastSeen:        now,
		},
		"gemini-cli": {
			AgentID:     "gemini-cli",
			Vendor:      "google",
			ControlRepo: root,
			LastSeen:    now,
		},
		"future": {
			AgentID:         "future",
			ProtocolVersion: loop.CurrentProtocolVersion + 1,
			Vendor:          "test",
			ControlRepo:     root,
			LastSeen:        now,
		},
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()

	if got := statusColumnValue(t, text, "PROTO", "codex"); got != "v2" {
		t.Fatalf("codex PROTO = %q, want v2:\n%s", got, text)
	}
	if got := statusColumnValue(t, text, "PROTO", "gemini-cli"); got != "legacy" {
		t.Fatalf("gemini-cli PROTO = %q, want legacy:\n%s", got, text)
	}
	if got := statusColumnValue(t, text, "PROTO", "future"); got != "v3!" {
		t.Fatalf("future PROTO = %q, want v3!:\n%s", got, text)
	}
	if !strings.Contains(text, "PROTOCOL WARNINGS:") || !strings.Contains(text, "future reports protocol v3; expected v2") {
		t.Fatalf("status missing protocol mismatch warning:\n%s", text)
	}
	if strings.Contains(text, "gemini-cli reports protocol") {
		t.Fatalf("absent-v legacy registration must not warn:\n%s", text)
	}
}

// Pull-only (Gate 6c): TestStatus_ShowsReachableColumn and
// TestStatus_ShowsCachedReachableAge were removed. The WAKE / REACHABLE / CACHED
// columns (and statusReachableProbe) are gone — registrations carry no wake state
// and presence comes from `.live` (the LAST_SEEN/AGE columns above).

// statusColumnValue extracts the value under the named header for the agent row,
// robust to columns being added (it keys off the header position, not the last
// field). Assumes every cell is a single whitespace-free token (true for the
// status table).
func statusColumnValue(t *testing.T, text, header, agentID string) string {
	t.Helper()
	var headerFields, row []string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "AGENT" {
			headerFields = f
		}
		if f[0] == agentID {
			row = f
		}
	}
	if headerFields == nil {
		t.Fatalf("no header row in status output:\n%s", text)
	}
	if row == nil {
		t.Fatalf("no %s row in status output:\n%s", agentID, text)
	}
	idx := -1
	for i, h := range headerFields {
		if h == header {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("header %q not found in %v", header, headerFields)
	}
	if idx >= len(row) {
		t.Fatalf("row %v has no column %d (%s)", row, idx, header)
	}
	return row[idx]
}

func TestCmdStatusWithoutAgentIDFailsWithHint(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	t.Setenv("AGENTCHUTE_AGENT_ID", "")
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdStatus(nil) })
		if err == nil {
			t.Fatal("cmdStatus with no identity returned nil error")
		}
		if err.Error() != missingAgentIdentityHint {
			t.Fatalf("error = %q, want %q", err, missingAgentIdentityHint)
		}
	})
}

// B1: CLI touches no longer refresh liveness — only serve's lease-gated
// HeartbeatRegistration does. `status --as` still requires the agent to be
// enrolled (the registration-exists preflight stays), but no longer bumps
// last_seen as a side effect of that preflight.
func TestCmdStatusWithAgentIDDoesNotRefreshLastSeen(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))

	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
	})

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	regPath := cfg.AgentRegistrationPath("codex")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to second precision so the stored value matches what's
	// written, then read-back to anchor the assertion on the actual
	// pre-test stored state (codex review on 37d87e1: nanosecond
	// precision was masking a missing-update bug).
	backdated := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	reg.LastSeen = backdated
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	before, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdStatus([]string{"--as", "codex"}) }); err != nil {
			t.Fatal(err)
		}
	})

	after, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeen.Equal(before.LastSeen) {
		t.Errorf("status with --as refreshed last_seen (B1: CLI touches must not): %v → %v", before.LastSeen, after.LastSeen)
	}
}
