package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func makeTarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractAgentchuteAcceptsExactRejectsOthers(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agentchute")

	// Exact member extracts.
	good := makeTarGz(t, map[string][]byte{"agentchute": []byte("BINARY-BYTES")})
	tmp, err := extractAgentchute(good, target)
	if err != nil {
		t.Fatalf("exact agentchute should extract: %v", err)
	}
	if got, _ := os.ReadFile(tmp); string(got) != "BINARY-BYTES" {
		t.Fatalf("extracted contents = %q", got)
	}
	if info, _ := os.Stat(tmp); info.Mode()&0o100 == 0 {
		t.Errorf("extracted mode %v missing owner-exec bit", info.Mode())
	}
	os.Remove(tmp)

	// A traversal / nested / wrong name yields no agentchute member -> error,
	// and writes nothing.
	for _, bad := range []map[string][]byte{
		{"../agentchute": []byte("x")},
		{"bin/agentchute": []byte("x")},
		{"agentchutex": []byte("x")},
		{"evil.sh": []byte("x")},
	} {
		if _, err := extractAgentchute(makeTarGz(t, bad), target); err == nil {
			t.Errorf("archive %v should be rejected (no exact agentchute member)", keysOf(bad))
		}
	}
}

func keysOf(m map[string][]byte) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}

func TestFetchChecksumExactMatch(t *testing.T) {
	asset := "agentchute_9.9.9_darwin_arm64.tar.gz"
	hash := strings.Repeat("a", 64)
	body := "deadbeef" + strings.Repeat("0", 56) + "  some-other-file\n" +
		hash + "  " + asset + "\n" +
		strings.Repeat("b", 64) + "  " + asset + ".sig\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	got, err := fetchChecksum(srv.URL, asset)
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Fatalf("checksum = %q, want %q", got, hash)
	}

	// A filename that only appears as a substring (.sig) must not be matched
	// for the bare asset.
	if _, err := fetchChecksum(srv.URL, "agentchute_9.9.9_linux_amd64.tar.gz"); err == nil {
		t.Error("missing asset should error, not match a substring")
	}
}

func TestDownloadVerifyExtractSuccessAndMismatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agentchute")
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("NEW-BINARY")})
	sum := sha256.Sum256(archive)
	correct := hex.EncodeToString(sum[:])
	asset := "agentchute_1.2.3_test.tar.gz"

	newSrv := func(checksum string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "checksums.txt"):
				fmt.Fprintf(w, "%s  %s\n", checksum, asset)
			case strings.HasSuffix(r.URL.Path, asset):
				w.Write(archive)
			default:
				http.NotFound(w, r)
			}
		}))
	}

	// Correct checksum -> extracts.
	srv := newSrv(correct)
	oldBase := updateGitHubBase
	updateGitHubBase = srv.URL
	tmp, err := downloadVerifyExtract("v1.2.3", asset, target)
	updateGitHubBase = oldBase
	srv.Close()
	if err != nil {
		t.Fatalf("valid checksum should extract: %v", err)
	}
	if got, _ := os.ReadFile(tmp); string(got) != "NEW-BINARY" {
		t.Fatalf("extracted = %q", got)
	}
	os.Remove(tmp)

	// Wrong checksum -> aborts, target untouched.
	srv = newSrv(strings.Repeat("f", 64))
	updateGitHubBase = srv.URL
	_, err = downloadVerifyExtract("v1.2.3", asset, target)
	updateGitHubBase = oldBase
	srv.Close()
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum mismatch should abort; got %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Error("target binary must not be created on checksum mismatch")
	}
}

func TestResolveLatestVersionFollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "/agentchute/agentchute/releases/tag/v9.9.9", http.StatusFound)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	oldBase := updateGitHubBase
	updateGitHubBase = srv.URL
	defer func() { updateGitHubBase = oldBase }()

	tag, err := resolveLatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v9.9.9" {
		t.Fatalf("resolved tag = %q, want v9.9.9", tag)
	}
}

func TestResolveBinaryTargetRefusesShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "agentchute")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec real \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBinaryTarget(shim); err == nil || !strings.Contains(err.Error(), "shim") {
		t.Fatalf("a shebang script must be refused as a shim; got %v", err)
	}

	// A native-ish binary in a writable dir resolves.
	bin := filepath.Join(dir, "real-agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 3}, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBinaryTarget(bin)
	if err != nil {
		t.Fatalf("native binary in writable dir should resolve: %v", err)
	}
	wantReal, _ := filepath.EvalSymlinks(bin) // EvalSymlinks canonicalizes /var -> /private/var on macOS
	if got != wantReal {
		t.Fatalf("resolved = %q, want %q", got, wantReal)
	}
}

func TestVersionIsOlder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.5.0", true},
		{"0.5.0", "0.5.0", false},
		{"0.5.1", "0.5.0", false},
		{"1.0.0", "0.9.9", false},
		{"0.9.9", "1.0.0", true},
		{"dev", "0.5.0", false}, // unparseable never blocks
		{"0.5.0", "dev", false},
	}
	for _, c := range cases {
		if got := versionIsOlder(c.a, c.b); got != c.want {
			t.Errorf("versionIsOlder(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionTagRE(t *testing.T) {
	for _, ok := range []string{"v0.5.0", "v1.2.3", "v0.5.0-rc.1"} {
		if !versionTagRE.MatchString(ok) {
			t.Errorf("%q should be a valid tag", ok)
		}
	}
	for _, bad := range []string{"0.5.0", "v1.2", "vx.y.z", "v1.2.3.4", "latest", ""} {
		if versionTagRE.MatchString(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestCmdUpdateRefusesMissingPoolState(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root) // creates AGENTCHUTE.md + loop dir, but NO state/setup.json
		err := cmdUpdate([]string{"--version", "v0.5.0", "--dry-run"})
		if err == nil || !strings.Contains(err.Error(), "saved setup state") {
			t.Fatalf("update without saved setup state must refuse; got %v", err)
		}
		// The refusal must now point the user at --no-resync as the escape hatch.
		if !strings.Contains(err.Error(), "--no-resync") {
			t.Fatalf("missing-state refusal must mention --no-resync; got %v", err)
		}
	})
}

// --no-resync allows a binary-only update even when there is NO saved setup
// state (a pool created by an older binary or via `init`): the update must not
// refuse and the dry-run plan must report the re-sync/reset as skipped.
func TestUpdate_NoResyncAllowsBinaryOnlyWhenNoSetupState(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root) // AGENTCHUTE.md + loop dir, but NO state/setup.json
		out, err := captureStdout(t, func() error {
			return cmdUpdate([]string{"--version", "v0.5.0", "--no-resync", "--dry-run"})
		})
		if err != nil {
			t.Fatalf("--no-resync must not require saved setup state: %v", err)
		}
		if !strings.Contains(out, "skipped (--no-resync)") {
			t.Fatalf("dry-run plan should report re-sync/reset skipped under --no-resync; got:\n%s", out)
		}
		// finding 5 (2026-08-11 hook-refresh-reliability follow-up): the
		// dry-run plan must name exactly what --no-resync leaves stale, not
		// just that the re-sync was skipped.
		for _, want := range []string{"hooks", "enrollment blocks", "AGENTCHUTE.md"} {
			if !strings.Contains(out, want) {
				t.Errorf("dry-run plan under --no-resync must name %q as staying stale; got:\n%s", want, out)
			}
		}
	})
}

// With --no-resync the apply path swaps the binary and invalidates leases, but
// must NEVER invoke the setup re-sync seam. We assert via updateRunResync: a
// stub that records invocation must stay untouched, while the old lease fences.
func TestUpdate_NoResyncSkipsSetupReSync(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions used by the writable probe")
	}
	root := t.TempDir()

	// A fake "installed binary" in a writable dir so resolveUpdateTarget/the
	// writable probe accept it, and the atomic rename has a real target.
	installDir := t.TempDir()
	bin := filepath.Join(installDir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}

	// Serve the release archive + checksum for v0.5.0.
	asset := fmt.Sprintf("agentchute_0.5.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("NEW-BINARY-BYTES")})
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", checksum, asset)
		case strings.HasSuffix(r.URL.Path, asset):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Seam: point the target binary resolver at our fake install, and record
	// whether the destructive re-sync was ever invoked.
	oldBase := updateGitHubBase
	oldTarget := resolveUpdateTargetForTest
	oldResync := updateRunResync
	updateGitHubBase = srv.URL
	resolveUpdateTargetForTest = bin
	resyncCalled := false
	updateRunResync = func(target string, setupArgs []string, controlRepo string) error {
		resyncCalled = true
		return nil
	}
	t.Cleanup(func() {
		updateGitHubBase = oldBase
		resolveUpdateTargetForTest = oldTarget
		updateRunResync = oldResync
	})

	// codex acceptance item 5 (update-fix-v2, docs/decisions/agentchute-
	// update-fix-v2.md): --no-resync waives the hook-compatibility
	// postcondition entirely — a pre-existing (even stale) hook file must
	// come out byte-identical, with no backup written.
	mustWriteStaleHook(t, root, "claude-code")
	staleHook := mustRead(t, filepath.Join(root, ".claude", "settings.json"))

	var lease *loop.ServeLease
	var out string
	withCwd(t, root, func() {
		mustExampleRepo(t, root) // deliberately NO saved setup state
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		lease, err = loop.AcquireServeLease(cfg, "codex-agentchute")
		if err != nil {
			t.Fatal(err)
		}
		out, err = captureStdout(t, func() error {
			return cmdUpdate([]string{"--version", "v0.5.0", "--no-resync"})
		})
		if err != nil {
			t.Fatalf("--no-resync binary-only update failed: %v", err)
		}
	})

	if resyncCalled {
		t.Fatal("--no-resync must NOT invoke the setup re-sync")
	}
	// finding 5 (2026-08-11 hook-refresh-reliability follow-up): the apply
	// path must name exactly what --no-resync leaves stale too, not just the
	// dry-run plan.
	for _, want := range []string{"hooks", "enrollment blocks", "AGENTCHUTE.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output under --no-resync must name %q as staying stale; got:\n%s", want, out)
		}
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY-BYTES" {
		t.Fatalf("binary should be swapped to the served archive; got %q", got)
	}
	if err := loop.RenewLease(lease); !errors.Is(err, loop.ErrFenced) {
		t.Fatalf("old lease after --no-resync update = %v, want ErrFenced", err)
	}
	gotHook := mustRead(t, filepath.Join(root, ".claude", "settings.json"))
	if string(gotHook) != string(staleHook) {
		t.Errorf("--no-resync must leave hook bytes untouched; got %q, want stale %q", gotHook, staleHook)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json.bak")); !os.IsNotExist(err) {
		t.Errorf("--no-resync must write no hook backup; stat err = %v", err)
	}
}

// TestUpdateInvalidatesLeaseOnlyAfterSetupResyncSucceeds proves the reordering
// codex's vector 2 review required (2026-08-11 hook-refresh-reliability
// follow-up): lease invalidation must wait until the resync succeeds, not
// happen immediately after the binary swap. Asserts the lease is still valid
// (not fenced) WHILE the resync runs, then fenced only after cmdUpdate
// returns successfully. Formerly named TestUpdateInvalidatesLeaseBeforeSetup-
// Resync and asserted the opposite ordering; that ordering was the bug.
func TestUpdateInvalidatesLeaseOnlyAfterSetupResyncSucceeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions used by the writable probe")
	}
	root := t.TempDir()
	installDir := t.TempDir()
	bin := filepath.Join(installDir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}

	asset := fmt.Sprintf("agentchute_0.5.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("NEW-BINARY-BYTES")})
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", checksum, asset)
		case strings.HasSuffix(r.URL.Path, asset):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase := updateGitHubBase
	oldTarget := resolveUpdateTargetForTest
	oldResync := updateRunResync
	updateGitHubBase = srv.URL
	resolveUpdateTargetForTest = bin
	t.Cleanup(func() {
		updateGitHubBase = oldBase
		resolveUpdateTargetForTest = oldTarget
		updateRunResync = oldResync
	})

	var lease *loop.ServeLease
	resyncCalled := false
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", nil, ""); err != nil {
			t.Fatal(err)
		}
		lease, err = loop.AcquireServeLease(cfg, "codex-agentchute")
		if err != nil {
			t.Fatal(err)
		}
		updateRunResync = func(target string, setupArgs []string, controlRepo string) error {
			resyncCalled = true
			if err := loop.RenewLease(lease); err != nil {
				t.Fatalf("lease at setup re-sync = %v, want no error (leases must not be invalidated before the resync succeeds)", err)
			}
			return nil
		}
		if err := cmdUpdate([]string{"--version", "v0.5.0"}); err != nil {
			t.Fatalf("update with re-sync failed: %v", err)
		}
	})
	if !resyncCalled {
		t.Fatal("normal update did not invoke setup re-sync")
	}
	if err := loop.RenewLease(lease); !errors.Is(err, loop.ErrFenced) {
		t.Fatalf("lease after a successful update = %v, want ErrFenced", err)
	}
}

// TestUpdate_ResyncFailureLeavesLeaseIntact is the failure-mode half of the
// same reordering: when the resync fails, existing supervisors must keep
// running on the prior binary rather than being fenced into hooks that might
// still be broken.
func TestUpdate_ResyncFailureLeavesLeaseIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions used by the writable probe")
	}
	root := t.TempDir()
	installDir := t.TempDir()
	bin := filepath.Join(installDir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}

	asset := fmt.Sprintf("agentchute_0.5.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("NEW-BINARY-BYTES")})
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", checksum, asset)
		case strings.HasSuffix(r.URL.Path, asset):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase := updateGitHubBase
	oldTarget := resolveUpdateTargetForTest
	oldResync := updateRunResync
	updateGitHubBase = srv.URL
	resolveUpdateTargetForTest = bin
	updateRunResync = func(target string, setupArgs []string, controlRepo string) error {
		return errors.New("injected resync failure")
	}
	t.Cleanup(func() {
		updateGitHubBase = oldBase
		resolveUpdateTargetForTest = oldTarget
		updateRunResync = oldResync
	})

	var lease *loop.ServeLease
	var updateErr error
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", nil, ""); err != nil {
			t.Fatal(err)
		}
		lease, err = loop.AcquireServeLease(cfg, "codex-agentchute")
		if err != nil {
			t.Fatal(err)
		}
		updateErr = cmdUpdate([]string{"--version", "v0.5.0"})
	})

	if updateErr == nil {
		t.Fatal("expected the injected resync failure to surface as a non-zero update error")
	}
	if err := loop.RenewLease(lease); err != nil {
		t.Fatalf("lease after a FAILED resync = %v, want no error — a failed resync must never fence the fleet", err)
	}
}

// TestPrintRestartWarningDoneFalseDoesNotClaimFenced is the regression test
// codex asked for on PR #130's gate: the pre-resync (done=false) banner
// previously printed "will be invalidated once the setup re-sync succeeds"
// on its own first line, but then UNCONDITIONALLY continued with "Running
// supervisors are fenced" and "RESTART every active agent now" — a direct
// contradiction of the resync-gated ordering (neither is true until
// invalidation actually runs), and exactly the kind of prompt that would
// defeat the safety fix by pushing an operator to restart into a possibly-
// incomplete update. done=true must still say both.
func TestPrintRestartWarningDoneFalseDoesNotClaimFenced(t *testing.T) {
	pending := captureStderr(t, func() {
		printRestartWarning("v0.5.0", []string{"codex-agentchute"}, false)
	})
	for _, mustNotContain := range []string{"are fenced", "RESTART every active agent now"} {
		if strings.Contains(pending, mustNotContain) {
			t.Errorf("done=false banner must not claim %q before the resync has succeeded:\n%s", mustNotContain, pending)
		}
	}
	if !strings.Contains(pending, "will be invalidated once the setup re-sync succeeds") {
		t.Errorf("done=false banner missing the resync-gated timing statement:\n%s", pending)
	}

	done := captureStderr(t, func() {
		printRestartWarning("v0.5.0", []string{"codex-agentchute"}, true)
	})
	for _, mustContain := range []string{"are fenced", "RESTART every active agent now"} {
		if !strings.Contains(done, mustContain) {
			t.Errorf("done=true banner missing %q:\n%s", mustContain, done)
		}
	}
}

// codex review on PR #110 [P1]: the forced-verification-failure test in
// setup_test.go proves cmdSetup's own contract, but acceptance item 3
// ("forced verification failure propagates through setup/update resync")
// also requires proving it through the UPDATE apply path specifically —
// non-zero, actionable output, and the final done=true restart banner
// (printRestartWarning(..., true), the "serve leases were invalidated"
// past-tense line) never reached. updateRunResync stands in for the
// exec'd `setup` subprocess dying non-zero the way it would if the
// post-refresh hook compatibility verification (setup.go's Phase 2.5)
// failed inside it — this is update.go's existing, unchanged resync-
// failure handling, exercised against that specific failure shape.
func TestUpdate_ResyncVerificationFailurePreventsFinalRestartBanner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions used by the writable probe")
	}
	root := t.TempDir()
	installDir := t.TempDir()
	bin := filepath.Join(installDir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}

	asset := fmt.Sprintf("agentchute_0.5.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("NEW-BINARY-BYTES")})
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", checksum, asset)
		case strings.HasSuffix(r.URL.Path, asset):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase := updateGitHubBase
	oldTarget := resolveUpdateTargetForTest
	oldResync := updateRunResync
	updateGitHubBase = srv.URL
	resolveUpdateTargetForTest = bin
	updateRunResync = func(target string, setupArgs []string, controlRepo string) error {
		return errors.New("hook compatibility verification failed: hook file(s) invoke unknown agentchute subcommand(s) after refresh: claude-code (`poller`) — run `agentchute hooks install --wrapper all --scope repo --force`")
	}
	t.Cleanup(func() {
		updateGitHubBase = oldBase
		resolveUpdateTargetForTest = oldTarget
		updateRunResync = oldResync
	})

	var updateErr error
	var stderr string
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", nil, ""); err != nil {
			t.Fatal(err)
		}
		stderr = captureStderr(t, func() {
			updateErr = cmdUpdate([]string{"--version", "v0.5.0"})
		})
	})

	if updateErr == nil {
		t.Fatal("expected a resync verification failure to surface as a non-zero update error")
	}
	for _, want := range []string{"re-sync FAILED", "Finish the re-sync manually", "hooks install --wrapper all --scope repo --force"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing actionable content %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "agentchute updated to v0.5.0; serve leases were invalidated.") {
		t.Errorf("final done=true restart banner must NOT print after a resync verification failure:\n%s", stderr)
	}
}

// #1 regression: path resolution must not require a writable dir (so --dry-run
// never mutates), while the apply-only writable probe correctly fails read-only.
func TestResolveTargetSplitFromWritableProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F'}, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBinaryTarget(bin); err != nil {
		t.Fatalf("resolveBinaryTarget must not require writability (dry-run safe): %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := ensureWritableDir(bin); err == nil {
		t.Error("ensureWritableDir must fail on a read-only install dir")
	}
	// And it left no probe files behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agentchute-update-probe") {
			t.Errorf("probe file leaked: %s", e.Name())
		}
	}
}

// #3 regression: a valid `--wrappers none` install (empty wrapper list) updates,
// replaying `--wrappers none` rather than being refused.
func TestCmdUpdateWrappersNoneReplays(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "tmux", nil, "1h"); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error {
			return cmdUpdate([]string{"--version", "v0.5.0", "--dry-run"})
		})
		if err != nil {
			t.Fatalf("wrappers-none update should not be refused: %v", err)
		}
		if !strings.Contains(out, "--wrappers none") {
			t.Fatalf("re-sync plan should replay `--wrappers none`; got:\n%s", out)
		}
	})
}

// C9 regression: update's setup re-sync replays the pool's configured
// --stale-after rather than silently reverting it to the 1h default.
func TestCmdUpdateReplaysStaleAfter(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", nil, "45m"); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error {
			return cmdUpdate([]string{"--version", "v0.5.0", "--dry-run"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "--stale-after 45m") {
			t.Fatalf("re-sync plan should replay `--stale-after 45m`; got:\n%s", out)
		}
	})
}

// #4 regression: an agentchute member larger than the cap is rejected, not
// silently truncated.
func TestExtractRejectsOversizedMember(t *testing.T) {
	old := updateMaxAsset
	updateMaxAsset = 8
	t.Cleanup(func() { updateMaxAsset = old })
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("0123456789ABCDEF")}) // 16 > 8
	if _, err := extractAgentchute(archive, filepath.Join(t.TempDir(), "agentchute")); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized member must be rejected; got %v", err)
	}
}

// #6 regression: a 64-char non-hex checksum is rejected.
func TestFetchChecksumRejectsNonHex(t *testing.T) {
	asset := "agentchute_9.9.9_test.tar.gz"
	body := strings.Repeat("g", 64) + "  " + asset + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	if _, err := fetchChecksum(srv.URL, asset); err == nil || !strings.Contains(err.Error(), "non-hex") {
		t.Fatalf("non-hex checksum must be rejected; got %v", err)
	}
}

func TestInstalledHookWrappers(t *testing.T) {
	repo := t.TempDir()
	if got := installedHookWrappers(repo); len(got) != 0 {
		t.Fatalf("no hook files installed: want none, got %v", got)
	}
	mustWriteCanonicalHook(t, repo, "claude-code")
	mustWriteCanonicalHook(t, repo, "codex")
	if got := strings.Join(installedHookWrappers(repo), ","); got != "claude-code,codex" {
		t.Fatalf("installed = %q, want claude-code,codex (and no wrapper without a hook file)", got)
	}
}

// 2026-08-12: nil recorded membership (JSON null — state predating wrapper
// recording) alongside installed hook files is repairable bookkeeping: update
// adopts the installed set and records it itself, instead of replaying
// `--wrappers none` and prescribing `setup --wrappers <list>` as a manual
// follow-up (update-fix-v2's wrappersUnrecordedWarning). A dry-run shows the
// adoption in the plan but must not write state.
func TestCmdUpdateAdoptsInstalledHookMembership(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		mustWriteCanonicalHook(t, root, "claude-code")
		mustWriteCanonicalHook(t, root, "codex")
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", nil, "1h"); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := captureStdoutStderr(t, func() error {
			return cmdUpdate([]string{"--version", "v0.5.0", "--dry-run"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "--wrappers claude-code,codex") {
			t.Fatalf("re-sync plan should adopt the installed membership; got:\n%s", stdout)
		}
		if strings.Contains(stdout, "--wrappers none") {
			t.Fatalf("re-sync plan must not replay `--wrappers none` past an unrecorded membership; got:\n%s", stdout)
		}
		if !strings.Contains(stderr, "would adopt") {
			t.Fatalf("dry-run stderr should note the pending adoption; got:\n%s", stderr)
		}
		if strings.Contains(stderr, "setup --wrappers <list>`") {
			t.Fatalf("stderr must not prescribe a manual setup follow-up; got:\n%s", stderr)
		}
		state, err := readSetupPoolState(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if state.Wrappers != nil {
			t.Fatalf("dry-run must not record membership; got %v", state.Wrappers)
		}
	})
}

// codex FIX vector 1 (2026-08-12): hook files can legitimately exist outside
// membership (`hooks install --scope repo` writes no setup state), so a
// RECORDED `--wrappers none` — non-nil empty, `"wrappers": []` — must never
// be overridden by file presence. Only nil (null) membership is adopted.
func TestCmdUpdateExplicitNoneNotAdopted(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		mustWriteCanonicalHook(t, root, "claude-code")
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", []string{}, "1h"); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := captureStdoutStderr(t, func() error {
			return cmdUpdate([]string{"--version", "v0.5.0", "--dry-run"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "--wrappers none") {
			t.Fatalf("recorded explicit none must replay `--wrappers none`; got:\n%s", stdout)
		}
		if strings.Contains(stderr, "adopt") {
			t.Fatalf("recorded explicit none must not be adopted over; got stderr:\n%s", stderr)
		}
	})
}

// 2026-08-12: a no---version update while already on the latest release must
// say so and stop — not silently re-download the same asset (which reads as a
// hang) or fence the fleet for a no-op. An explicit `--version <current>`
// still runs the full reinstall (the escape hatch for a binary-swapped-but-
// resync-failed prior update).
func TestCmdUpdateAlreadyUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "/agentchute/agentchute/releases/tag/v1.2.3", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	oldBase := updateGitHubBase
	updateGitHubBase = srv.URL
	oldVersion := version
	version = "1.2.3"
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F'}, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTarget := resolveUpdateTargetForTest
	resolveUpdateTargetForTest = bin
	defer func() {
		updateGitHubBase = oldBase
		version = oldVersion
		resolveUpdateTargetForTest = oldTarget
	}()

	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		// codex FIX vector 2: adoption must be recorded even on this path —
		// the early exit returns before any resync could ever record it.
		mustWriteCanonicalHook(t, root, "claude-code")
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPoolState(cfg, "runner", nil, "1h"); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error { return cmdUpdate(nil) })
		if err != nil {
			t.Fatalf("same-version ambient update must be a clean no-op: %v", err)
		}
		if !strings.Contains(out, "already up to date") {
			t.Fatalf("want an already-up-to-date report; got:\n%s", out)
		}
		if strings.Contains(out, "Invalidated") {
			t.Fatalf("a no-op update must not invalidate serve leases; got:\n%s", out)
		}
		state, err := readSetupPoolState(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(state.Wrappers, ","); got != "claude-code" {
			t.Fatalf("adopted membership must be recorded despite the early exit; got %q", got)
		}
		if state.StaleAfter != "1h" {
			t.Fatalf("recording adoption must preserve stale_after; got %q", state.StaleAfter)
		}

		// Explicit --version of the current release still attempts the full
		// reinstall (and announces the download before the network phase).
		out, err = captureStdout(t, func() error {
			return cmdUpdate([]string{"--version", "v1.2.3"})
		})
		if err == nil {
			t.Fatal("explicit same-version reinstall should have attempted (and failed) the download against a 404 server")
		}
		if !strings.Contains(out, "Downloading v1.2.3") {
			t.Fatalf("apply path should announce the download; got:\n%s", out)
		}
	})
}
