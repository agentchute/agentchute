package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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
	})
}

// With --no-resync the apply path swaps the binary but must NEVER invoke the
// destructive setup re-sync seam (no bus reset, registrations preserved). We
// assert via the updateRunResync seam: a stub that records invocation must stay
// untouched, while the binary is replaced by the served archive.
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

	withCwd(t, root, func() {
		mustExampleRepo(t, root) // deliberately NO saved setup state
		if err := cmdUpdate([]string{"--version", "v0.5.0", "--no-resync"}); err != nil {
			t.Fatalf("--no-resync binary-only update failed: %v", err)
		}
	})

	if resyncCalled {
		t.Fatal("--no-resync must NOT invoke the destructive setup re-sync")
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY-BYTES" {
		t.Fatalf("binary should be swapped to the served archive; got %q", got)
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
		if err := writeSetupPoolState(cfg, "tmux", nil); err != nil {
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
