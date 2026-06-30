package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadRegistrationParsesFrontmatterAndBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.md")
	mustWrite(t, path, []byte(`---
agent_id: codex
vendor: openai
control_repo: /tmp/repo
working_repos:
  - /tmp/repo
  - "/tmp/other repo"
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: exhausted
restart_at: 2026-05-09T18:00:00Z
last_active: 2026-05-09T16:00:12.123456Z
---

# Notes

review-first
`))

	reg, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg.AgentID != "codex" || reg.Vendor != "openai" || reg.WakeMethod != "tmux" || reg.WakeTarget != "%1" {
		t.Fatalf("unexpected scalar fields: %#v", reg)
	}
	if len(reg.WorkingRepos) != 2 || reg.WorkingRepos[1] != "/tmp/other repo" {
		t.Fatalf("WorkingRepos = %#v", reg.WorkingRepos)
	}
	if reg.Status != StatusExhausted {
		t.Fatalf("Status = %q", reg.Status)
	}
	if reg.RestartAt == nil || reg.RestartAt.UTC().Format(time.RFC3339) != "2026-05-09T18:00:00Z" {
		t.Fatalf("RestartAt = %#v", reg.RestartAt)
	}
	if reg.LastActive == nil || reg.LastActive.UTC().Format(time.RFC3339Nano) != "2026-05-09T16:00:12.123456Z" {
		t.Fatalf("LastActive = %#v", reg.LastActive)
	}
	if reg.Body == "" || reg.Body[0] != '#' {
		t.Fatalf("Body = %q", reg.Body)
	}
}

func TestWriteRegistrationRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alex.md")
	lastSeen := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)
	lastActive := time.Date(2026, 5, 9, 16, 9, 0, 0, time.UTC)
	reg := &Registration{
		AgentID:      "alex",
		Vendor:       "human",
		ControlRepo:  "/tmp/repo",
		WorkingRepos: []string{"/tmp/repo"},
		WakeMethod:   "tmux",
		WakeTarget:   "%5",
		LastSeen:     lastSeen,
		Status:       StatusActive,
		LastActive:   &lastActive,
		Body:         "# Alex\n",
	}

	if err := WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registration mode = %o, want 600", got)
	}
	got, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != reg.AgentID || got.Vendor != reg.Vendor || got.LastActive == nil {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestWriteRegistrationExclusiveRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.md")
	reg := &Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Now().UTC(),
		Status:      StatusActive,
	}
	if err := WriteRegistrationExclusive(path, reg); err != nil {
		t.Fatal(err)
	}
	reg.Host = "other"
	if err := WriteRegistrationExclusive(path, reg); !os.IsExist(err) {
		t.Fatalf("second exclusive write err = %v, want os.IsExist", err)
	}
	got, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "" {
		t.Fatalf("exclusive collision overwrote registration: %#v", got)
	}
}

func TestReadFileLimit_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	// A symlink in the registration/inbox path must be refused, not silently
	// followed to its target — a peer could plant a symlink to /etc/passwd or
	// to another agent's private state.
	if _, err := ReadFileLimit(link, MaxRegistrationBytes); err == nil {
		t.Fatal("ReadFileLimit followed a symlink, want refusal")
	}

	// A real regular file still reads fine.
	if data, err := ReadFileLimit(target, MaxRegistrationBytes); err != nil || string(data) != "secret" {
		t.Fatalf("ReadFileLimit(regular) = %q, %v; want \"secret\", nil", data, err)
	}
}

func TestValidateRejectsMalformedWakeTarget(t *testing.T) {
	base := func() *Registration {
		return &Registration{
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: "/tmp/repo",
			LastSeen:    time.Now().UTC(),
			Status:      StatusActive,
		}
	}

	// Live formats must pass Validate().
	live := []struct {
		method, target string
	}{
		{"tmux", "%0"},
		{"tmux", "%1"},
		{"tmux", "main:0.0"},
		{"herdr", "claude-code-agentchute"},
		{"herdr", "codex-agentchute"},
		{RunnerWakeMethod, "unix:/Users/alex/code/agentchute/.agentchute/loop/state/grok-agentchute/runner.sock"},
	}
	for _, lf := range live {
		reg := base()
		reg.WakeMethod = lf.method
		reg.WakeTarget = lf.target
		if err := reg.Validate(); err != nil {
			t.Errorf("live registration %s/%q failed Validate(): %v", lf.method, lf.target, err)
		}
	}

	// Malformed / hostile targets must fail Validate().
	bad := []struct {
		name, method, target string
	}{
		{"tmux foreign pane no percent", "tmux", "main"},
		{"tmux leading dash", "tmux", "-t"},
		{"tmux injection", "tmux", "%0;reboot"},
		{"herdr traversal", "herdr", "../../etc/passwd"},
		{"runner non-unix", RunnerWakeMethod, "/tmp/evil.sock"},
		{"runner newline", RunnerWakeMethod, "unix:/tmp/evil\n.sock"},
	}
	for _, b := range bad {
		reg := base()
		reg.WakeMethod = b.method
		reg.WakeTarget = b.target
		if err := reg.Validate(); err == nil {
			t.Errorf("%s: malformed registration %s/%q passed Validate(), want error", b.name, b.method, b.target)
		}
	}
}

func TestUpdateLastSeenPreservesBody(t *testing.T) {
	cfg := newLockTestConfig(t)
	path := cfg.AgentRegistrationPath("codex")
	mustWrite(t, path, []byte(`---
agent_id: codex
vendor: openai
control_repo: /tmp/repo
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: active
---

# Keep this body
`))

	next := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	if err := UpdateLastSeen(cfg, "codex", next); err != nil {
		t.Fatal(err)
	}
	reg, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg.LastSeen.UTC().Format(time.RFC3339) != "2026-05-10T00:00:00Z" {
		t.Fatalf("LastSeen = %s", reg.LastSeen.Format(time.RFC3339))
	}
	if reg.Body != "# Keep this body\n" {
		t.Fatalf("Body = %q", reg.Body)
	}
}

func TestUpdateLastActivePreservesBody(t *testing.T) {
	cfg := newLockTestConfig(t)
	path := cfg.AgentRegistrationPath("codex")
	mustWrite(t, path, []byte(`---
agent_id: codex
vendor: openai
control_repo: /tmp/repo
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: active
---

# Keep this body
`))

	next := time.Date(2026, 5, 10, 0, 1, 0, 0, time.UTC)
	if err := UpdateLastActive(cfg, "codex", next); err != nil {
		t.Fatal(err)
	}
	reg, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg.LastActive == nil || !reg.LastActive.Equal(next) {
		t.Fatalf("LastActive = %v, want %v", reg.LastActive, next)
	}
	if reg.Body != "# Keep this body\n" {
		t.Fatalf("Body = %q", reg.Body)
	}
}

func TestUpdateLastSeenUsesStructuredRegistrationWrite(t *testing.T) {
	cfg := newLockTestConfig(t)
	path := cfg.AgentRegistrationPath("codex")
	mustWrite(t, path, []byte(`---
agent_id: codex
vendor: openai
control_repo: /tmp/repo
custom_field: preserved-by-line-edit-only
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: active
---
`))

	next := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	if err := UpdateLastSeen(cfg, "codex", next); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "custom_field:") {
		t.Fatalf("UpdateLastSeen preserved unknown frontmatter field:\n%s", string(data))
	}
}

// GATE 3: UpdateLastSeen is the shared heartbeat path (runner tick, check, send,
// status). Besides refreshing registration last_seen it must publish a fresh
// `.live` presence fact (busy=false; busy is advisory, set only by serve) so all
// heartbeat sites yield fresh presence with no per-call-site edits.
func TestUpdateLastSeenWritesLive(t *testing.T) {
	cfg := newLockTestConfig(t)
	path := cfg.AgentRegistrationPath("codex")
	mustWrite(t, path, []byte(`---
agent_id: codex
vendor: openai
control_repo: /tmp/repo
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: active
---
`))

	// No `.live` exists before the heartbeat.
	if _, err := ReadLive(cfg, "codex"); err == nil {
		t.Fatal("expected no .live before UpdateLastSeen")
	}

	if err := UpdateLastSeen(cfg, "codex", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if !IsLive(cfg, "codex", liveWindow, time.Now()) {
		t.Fatal("UpdateLastSeen did not publish a fresh .live")
	}
	live, err := ReadLive(cfg, "codex")
	if err != nil {
		t.Fatalf("ReadLive: %v", err)
	}
	if live.Busy {
		t.Error("UpdateLastSeen wrote busy=true; busy is advisory and must be false here")
	}
}

func TestReadRegistrationRejectsDuplicateFrontmatterKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.md")
	mustWrite(t, path, []byte(`---
agent_id: codex
vendor: openai
vendor: local
control_repo: /tmp/repo
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: active
---
`))

	if _, err := ReadRegistration(path); err == nil {
		t.Fatal("expected duplicate frontmatter key rejection")
	}
}

func TestRegistrationRejectsRelativeControlRepo(t *testing.T) {
	reg := &Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: "relative/path",
		LastSeen:    time.Now(),
		Status:      StatusActive,
	}
	if err := reg.Validate(); err == nil {
		t.Fatal("expected relative control_repo to be rejected")
	}
}

func TestRegistrationRejectsRelativeWorkingRepos(t *testing.T) {
	reg := &Registration{
		AgentID:      "codex",
		Vendor:       "openai",
		ControlRepo:  "/tmp/repo",
		WorkingRepos: []string{"/tmp/repo", "relative/elsewhere"},
		LastSeen:     time.Now(),
		Status:       StatusActive,
	}
	if err := reg.Validate(); err == nil {
		t.Fatal("expected relative working_repos entry to be rejected")
	}
}

func TestRegistrationRejectsInvalidAgentID(t *testing.T) {
	reg := &Registration{
		AgentID:     "Bad Agent",
		Vendor:      "openai",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Now(),
		Status:      StatusActive,
	}
	if err := reg.Validate(); err == nil {
		t.Fatal("expected invalid agent_id error")
	}
}

func TestReadFileLimitRejectsOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.bin")
	mustWrite(t, path, make([]byte, 1024))

	if _, err := ReadFileLimit(path, 1024); err != nil {
		t.Fatalf("exactly at limit should succeed: %v", err)
	}
	if _, err := ReadFileLimit(path, 1023); err == nil {
		t.Fatal("expected oversize rejection")
	}
}

func TestReadRegistrationRejectsOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.md")
	huge := make([]byte, MaxRegistrationBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	mustWrite(t, path, huge)
	if _, err := ReadRegistration(path); err == nil {
		t.Fatal("expected oversize-registration rejection")
	}
}

func TestReadRegistrationsLenientReturnsParseableAndErrors(t *testing.T) {
	dir := t.TempDir()
	// Three files: one valid, one with malformed frontmatter, one valid.
	mustWrite(t, filepath.Join(dir, "alpha.md"), []byte(`---
agent_id: alpha
vendor: human
control_repo: /tmp/repo
last_seen: 2026-05-12T00:00:00Z
status: active
---
`))
	mustWrite(t, filepath.Join(dir, "broken.md"), []byte(`---
agent_id: broken
vendor: human
this line lacks a colon
---
`))
	mustWrite(t, filepath.Join(dir, "beta.md"), []byte(`---
agent_id: beta
vendor: human
control_repo: /tmp/repo
last_seen: 2026-05-12T00:00:00Z
status: active
---
`))
	// Also drop entries that MUST be silently skipped.
	mustWrite(t, filepath.Join(dir, "README.md"), []byte("not a registration\n"))
	mustWrite(t, filepath.Join(dir, "claude.example.md"), []byte("---\nagent_id: claude\nvendor: anthropic\n---\n"))
	mustWrite(t, filepath.Join(dir, ".hidden.md"), []byte("not a registration\n"))

	regs, errs := ReadRegistrationsLenient(dir)
	if len(regs) != 2 {
		t.Fatalf("expected 2 parseable registrations, got %d", len(regs))
	}
	got := map[string]bool{regs[0].AgentID: true, regs[1].AgentID: true}
	if !got["alpha"] || !got["beta"] {
		t.Fatalf("expected alpha + beta, got %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 parse error, got %d (%v)", len(errs), errs)
	}
	if !strings.Contains(errs[0].Path, "broken.md") {
		t.Fatalf("expected broken.md in error path, got %s", errs[0].Path)
	}
}

func TestReadRegistrationsLenientMissingDirIsCleanEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	regs, errs := ReadRegistrationsLenient(dir)
	if regs != nil {
		t.Fatalf("expected nil regs for missing dir, got %v", regs)
	}
	if errs != nil {
		t.Fatalf("expected nil errs for missing dir (treated as empty), got %v", errs)
	}
}

func TestReadRegistrationRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	link := filepath.Join(root, "codex.md")
	mustWrite(t, target, []byte(`---
agent_id: codex
vendor: openai
control_repo: /tmp/repo
wake_method: tmux
wake_target: "%1"
last_seen: 2026-05-09T16:08:36Z
status: active
---
`))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := ReadRegistration(link); err == nil {
		t.Fatal("expected symlink-registration rejection")
	}
}

// WI-E2: the four self-healing reachability cache fields round-trip through the
// registration read/write path (backward-compatible; absent = old behavior).
// WI-E3: launch provenance (launched_by / shim_name / hook_event) round-trips
// through write/read, and an absent-provenance registration stays byte-identical
// to the pre-upgrade format (no new keys emitted).
func TestRegistration_LaunchProvenanceRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gemini.md")
	reg := &Registration{
		AgentID:     "gemini-cli",
		Vendor:      "google",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Now().UTC(),
		Status:      StatusActive,
		LaunchedBy:  LaunchedByRunner,
		ShimName:    "ac-gemini",
		HookEvent:   "boot",
	}
	if err := WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.LaunchedBy != LaunchedByRunner {
		t.Fatalf("LaunchedBy = %q, want %q", got.LaunchedBy, LaunchedByRunner)
	}
	if got.ShimName != "ac-gemini" {
		t.Fatalf("ShimName = %q, want ac-gemini", got.ShimName)
	}
	if got.HookEvent != "boot" {
		t.Fatalf("HookEvent = %q, want boot", got.HookEvent)
	}

	// Backward-compat: a registration with NO provenance fields reads back with
	// all three absent AND serializes byte-identically (no new keys present).
	plain := &Registration{
		AgentID:     "gemini-cli",
		Vendor:      "google",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Now().UTC(),
		Status:      StatusActive,
	}
	plainPath := filepath.Join(t.TempDir(), "plain.md")
	if err := WriteRegistration(plainPath, plain); err != nil {
		t.Fatal(err)
	}
	gotPlain, err := ReadRegistration(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotPlain.LaunchedBy != "" || gotPlain.ShimName != "" || gotPlain.HookEvent != "" {
		t.Fatalf("plain registration grew provenance fields: %#v", gotPlain)
	}
	data, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"launched_by", "shim_name", "hook_event"} {
		if strings.Contains(string(data), key) {
			t.Fatalf("plain registration serialized %q key (not byte-identical to pre-upgrade):\n%s", key, data)
		}
	}
}

func TestRegistration_ReachabilityFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.md")
	reachableAt := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	reg := &Registration{
		AgentID:            "codex",
		Vendor:             "openai",
		ControlRepo:        "/tmp/repo",
		WakeMethod:         "herdr",
		WakeTarget:         "codex-agentchute",
		LastSeen:           time.Now().UTC(),
		Status:             StatusActive,
		ReachableAt:        &reachableAt,
		ReachabilityMethod: "herdr",
		ReachabilityTarget: "codex-agentchute",
		ReachabilityError:  "prior probe: name not bound to a live pane",
	}
	if err := WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReachableAt == nil || !got.ReachableAt.Equal(reachableAt) {
		t.Fatalf("ReachableAt = %v, want %v", got.ReachableAt, reachableAt)
	}
	if got.ReachabilityMethod != "herdr" {
		t.Fatalf("ReachabilityMethod = %q, want herdr", got.ReachabilityMethod)
	}
	if got.ReachabilityTarget != "codex-agentchute" {
		t.Fatalf("ReachabilityTarget = %q, want codex-agentchute", got.ReachabilityTarget)
	}
	if got.ReachabilityError != "prior probe: name not bound to a live pane" {
		t.Fatalf("ReachabilityError = %q round-trip mismatch", got.ReachabilityError)
	}

	// Backward-compat: a registration with NO reachability fields reads back with
	// all four zero/absent (old behavior).
	plain := &Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Now().UTC(),
		Status:      StatusActive,
	}
	plainPath := filepath.Join(t.TempDir(), "plain.md")
	if err := WriteRegistration(plainPath, plain); err != nil {
		t.Fatal(err)
	}
	gotPlain, err := ReadRegistration(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotPlain.ReachableAt != nil || gotPlain.ReachabilityMethod != "" ||
		gotPlain.ReachabilityTarget != "" || gotPlain.ReachabilityError != "" {
		t.Fatalf("plain registration grew reachability fields: %#v", gotPlain)
	}
}

// WI-E2: IsReachable is true ONLY for a fresh, endpoint-bound cache hit. A
// wake-target change, a method mismatch, an expired timestamp, or an absent
// ReachableAt all invalidate the cache. IsPokable stays structural (unchanged).
func TestRegistration_IsReachableEndpointBoundTTL(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	ttl := 60 * time.Second

	base := func() *Registration {
		at := now.Add(-10 * time.Second)
		return &Registration{
			AgentID:            "codex",
			WakeMethod:         "herdr",
			WakeTarget:         "codex-agentchute",
			ReachableAt:        &at,
			ReachabilityMethod: "herdr",
			ReachabilityTarget: "codex-agentchute",
		}
	}

	if r := base(); !r.IsReachable(now, ttl) {
		t.Fatal("fresh endpoint-bound cache within ttl: IsReachable=false, want true")
	}

	// Expired.
	expired := base()
	old := now.Add(-2 * ttl)
	expired.ReachableAt = &old
	if expired.IsReachable(now, ttl) {
		t.Fatal("expired cache: IsReachable=true, want false")
	}

	// Method mismatch (endpoint changed).
	methodMismatch := base()
	methodMismatch.ReachabilityMethod = "tmux"
	if methodMismatch.IsReachable(now, ttl) {
		t.Fatal("method mismatch: IsReachable=true, want false (endpoint-bound)")
	}

	// Target mismatch (wake target changed → cache invalidated).
	targetMismatch := base()
	targetMismatch.WakeTarget = "codex-agentchute-2"
	if targetMismatch.IsReachable(now, ttl) {
		t.Fatal("target mismatch: IsReachable=true, want false (endpoint-bound)")
	}

	// Absent ReachableAt.
	absent := base()
	absent.ReachableAt = nil
	if absent.IsReachable(now, ttl) {
		t.Fatal("absent ReachableAt: IsReachable=true, want false")
	}

	// IsPokable stays purely structural: the cache fields do not touch it.
	pokable := base()
	pokable.ReachableAt = nil
	pokable.ReachabilityMethod = ""
	pokable.ReachabilityTarget = ""
	if !pokable.IsPokable() {
		t.Fatal("IsPokable=false for a reg with wake_method+wake_target; must stay structural")
	}
	noWake := &Registration{AgentID: "codex"}
	if noWake.IsPokable() {
		t.Fatal("IsPokable=true for a reg with no wake strings; must stay structural")
	}
}
