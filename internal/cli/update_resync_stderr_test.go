package cli

import (
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

// Issue #183, from the first v1.6.0 field report. `update` reported
//
//	WARNING: binary updated to v1.6.0 but `setup` re-sync FAILED: exit status 1
//
// and the actual cause — the symlink guard refusing to write through an
// AGENTCHUTE.md -> AGENTS.md symlink — was found only by re-running the printed
// command by hand.
//
// PREMISE CORRECTION, measured before any of this was written. update does NOT
// swallow the child's stderr. updateRunResync has wired the child's Stderr to
// os.Stderr since v0.9.1, v1.5.7 included, and Main prints command errors to
// stderr as well. The message DID reach the terminal. What failed is that it
// arrived a dozen lines above a WARNING which then named only an exit status, so
// the line an operator's eye lands on carried no cause.
//
// That makes "capture the stderr instead of streaming it" the wrong repair: it
// would trade a detached message for no live output at all, and a slow setup
// would go silent until it finished. The child still streams exactly as before;
// a bounded tail is kept alongside it so the WARNING can quote the end.
func TestUpdateResyncFailureQuotesWhatSetupSaid(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions used by the writable probe")
	}
	const guardMessage = "init: AGENTCHUTE.md is a symlink; refusing to read or write through it — replace it with a real file"
	root, bin, srv := newUpdateResyncFixture(t)
	defer srv.Close()

	oldBase, oldTarget, oldResync := updateGitHubBase, resolveUpdateTargetForTest, updateRunResync
	updateGitHubBase = srv.URL
	resolveUpdateTargetForTest = bin
	updateRunResync = func(string, []string, string) (string, error) {
		return "writing hooks\n" + guardMessage + "\n", errors.New("exit status 1")
	}
	t.Cleanup(func() {
		updateGitHubBase, resolveUpdateTargetForTest, updateRunResync = oldBase, oldTarget, oldResync
	})

	var updateErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			mustExampleRepo(t, root)
			cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
			if err != nil {
				t.Fatal(err)
			}
			if err := writeSetupPoolState(cfg, "runner", nil, ""); err != nil {
				t.Fatal(err)
			}
			updateErr = cmdUpdate([]string{"--version", "v0.5.0"})
		})
	})

	if updateErr == nil {
		t.Fatal("a failed re-sync must still be a non-zero update")
	}
	// The issue itself: the cause, next to the warning.
	if !strings.Contains(stderr, guardMessage) {
		t.Fatalf("the re-sync failure did not say what the child said:\n%s", stderr)
	}
	// Attributed, so an operator can tell the sentence came from the child and
	// not from update — the convention hub join already uses for ssh.
	if !strings.Contains(stderr, "said:") {
		t.Fatalf("the quoted output is not attributed to the child:\n%s", stderr)
	}
	// Still actionable: the manual command is what the field report recovered
	// with, so it has to survive this change.
	if !strings.Contains(stderr, "Finish the re-sync manually") {
		t.Fatalf("the manual re-sync command is gone:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Serve leases were NOT invalidated") {
		t.Fatalf("the lease-safety sentence is gone:\n%s", stderr)
	}
}

// The other half, and the one that catches a revert at the source: the REAL
// runner, against a stand-in that writes to stderr and exits non-zero. It must
// return what the child said AND still let the child's output through live.
func TestUpdateRunResyncTeesTheChildStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in for the child process")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "agentchute")
	script := "#!/bin/sh\necho 'progress on stdout'\necho 'init: AGENTCHUTE.md is a symlink' >&2\nexit 1\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var captured string
	var runErr error
	live := captureStderr(t, func() {
		captured, runErr = updateRunResync(target, []string{"--yes"}, dir)
	})

	if runErr == nil {
		t.Fatal("the stand-in exited 1; the runner reported success")
	}
	if !strings.Contains(captured, "AGENTCHUTE.md is a symlink") {
		t.Fatalf("the runner returned no trace of what the child said: %q", captured)
	}
	if !strings.Contains(live, "AGENTCHUTE.md is a symlink") {
		t.Fatalf("the child's stderr no longer reaches the terminal live:\n%s", live)
	}
}

// A failing child can be verbose. The quote is a pointer to the cause, not a
// transcript — an unbounded one would bury the manual command underneath it.
func TestResyncFailureDetailKeepsTheEndAndSaysItTrimmed(t *testing.T) {
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	detail := resyncFailureDetail(strings.Join(lines, "\n") + "\n")

	if strings.Contains(detail, "line 1\n") {
		t.Fatalf("kept the beginning; the cause of a failure is at the END:\n%s", detail)
	}
	if !strings.Contains(detail, "line 40") {
		t.Fatalf("dropped the last line, which is where the error is:\n%s", detail)
	}
	if got := strings.Count(detail, "line "); got > resyncStderrTailLines {
		t.Fatalf("quoted %d lines, want at most %d", got, resyncStderrTailLines)
	}
	// Silent truncation reads as "that was all it said", which is how an operator
	// stops looking for the rest.
	if !strings.Contains(detail, "earlier line") {
		t.Fatalf("trimmed output without saying so:\n%s", detail)
	}

	t.Run("blank output produces no empty block", func(t *testing.T) {
		if got := resyncFailureDetail("   \n\n\t\n"); got != "" {
			t.Fatalf("whitespace-only stderr produced a block: %q", got)
		}
	})

	t.Run("short output is quoted whole, with no trim note", func(t *testing.T) {
		detail := resyncFailureDetail("the only thing it said\n")
		if !strings.Contains(detail, "the only thing it said") {
			t.Fatalf("dropped the one line there was: %q", detail)
		}
		if strings.Contains(detail, "earlier line") {
			t.Fatalf("claimed a trim that did not happen: %q", detail)
		}
	})
}

func newUpdateResyncFixture(t *testing.T) (root, bin string, srv *httptest.Server) {
	t.Helper()
	root = t.TempDir()
	installDir := t.TempDir()
	bin = filepath.Join(installDir, "agentchute")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	asset := fmt.Sprintf("agentchute_0.5.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := makeTarGz(t, map[string][]byte{"agentchute": []byte("NEW-BINARY-BYTES")})
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", checksum, asset)
		case strings.HasSuffix(r.URL.Path, asset):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	return root, bin, srv
}
