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
			Status:      loop.StatusActive,
		},
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()
	for _, want := range []string{"control_repo:", "codex", "active"} {
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

// GATE 3: the LAST_SEEN/AGE presence columns come from the `.live` fact, not
// registration last_seen. An agent with a `.live` shows that timestamp/age even
// when its registration last_seen differs; an agent with no `.live` shows "-".
func TestStatus_PresenceFromLive(t *testing.T) {
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
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: root,
			// Registration last_seen is wildly stale on purpose — presence must
			// come from `.live`, not from here.
			LastSeen: now.Add(-99 * time.Hour),
			Status:   loop.StatusActive,
		},
		"grok": { // no `.live` written -> presence renders "-"
			AgentID:     "grok",
			Vendor:      "xai",
			ControlRepo: root,
			LastSeen:    now.Add(-time.Minute),
			Status:      loop.StatusActive,
		},
	}

	liveSeen := now.Add(-2 * time.Minute)
	mustWriteLiveAt(t, cfg, "codex", liveSeen)

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()

	if got := statusColumnValue(t, text, "AGE", "codex"); got != "2m0s" {
		t.Errorf("codex AGE = %q, want 2m0s (from .live, not reg.LastSeen):\n%s", got, text)
	}
	if got, want := statusColumnValue(t, text, "LAST_SEEN", "codex"), liveSeen.UTC().Format(time.RFC3339); got != want {
		t.Errorf("codex LAST_SEEN = %q, want %q (from .live):\n%s", got, want, text)
	}
	if got := statusColumnValue(t, text, "AGE", "grok"); got != "-" {
		t.Errorf("grok AGE = %q, want - (no .live):\n%s", got, text)
	}
	if got := statusColumnValue(t, text, "LAST_SEEN", "grok"); got != "-" {
		t.Errorf("grok LAST_SEEN = %q, want - (no .live):\n%s", got, text)
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

// WI-E1: status appends a "PRESENT BUT NOT ENROLLED" section listing wrappers in
// this pool that have no live registration.
func TestStatus_PresentButNotEnrolledSection(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	stubPresenceListers(t)
	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{{Name: "ghost-agent", Cwd: root}}
	}

	var buf bytes.Buffer
	printUnenrolledSection(&buf, cfg)
	text := buf.String()
	if !strings.Contains(text, "PRESENT BUT NOT ENROLLED") {
		t.Fatalf("missing presence section:\n%s", text)
	}
	if !strings.Contains(text, "ghost-agent") {
		t.Fatalf("presence section missing ghost-agent hint:\n%s", text)
	}
}

// With no unenrolled wrappers, the section is quiet (no output).
func TestStatus_PresentButNotEnrolledSectionQuietWhenClean(t *testing.T) {
	cfg, _ := presencePoolCfg(t)
	stubPresenceListers(t)

	var buf bytes.Buffer
	printUnenrolledSection(&buf, cfg)
	if buf.Len() != 0 {
		t.Fatalf("expected no output when nothing is unenrolled, got:\n%s", buf.String())
	}
}

// v0.1.2 UX nit: status without --as / $AGENTCHUTE_AGENT_ID prints the
// pool overview without claiming an agent identity and without touching
// anyone's last_seen. Codex review aligned: diagnostic command, not a
// lifecycle event.
func TestCmdStatusWithoutAgentIDPrintsPoolWithoutSideEffects(t *testing.T) {
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

	// Backdate codex's last_seen so we can detect a side-effecting update.
	regPath := cfg.AgentRegistrationPath("codex")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	// Registration stores last_seen at second precision; use a wall-second
	// timestamp so the post-write read compares equal.
	backdated := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	reg.LastSeen = backdated
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// Make sure no env var is set; otherwise --as would be implicit.
	// Use t.Setenv with empty value so test cleanup restores any prior
	// shell state (codex review on 37d87e1).
	t.Setenv("AGENTCHUTE_AGENT_ID", "")

	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error { return cmdStatus(nil) })
		if err != nil {
			t.Fatalf("cmdStatus with no --as returned err: %v", err)
		}
		if !strings.Contains(out, "codex") {
			t.Errorf("pool overview missing codex agent: %q", out)
		}
	})

	// last_seen must not have been touched by the no-flag invocation.
	after, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeen.Equal(backdated) {
		t.Errorf("status without --as updated last_seen: %v -> %v", backdated, after.LastSeen)
	}
}

// With --as set, status still ticks the caller's last_seen (preserves the
// historical acting-agent mode).
func TestCmdStatusWithAgentIDRefreshesLastSeen(t *testing.T) {
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
	if !after.LastSeen.After(before.LastSeen) {
		t.Errorf("status with --as did NOT refresh last_seen: %v → %v", before.LastSeen, after.LastSeen)
	}
}
