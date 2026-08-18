package hubclient

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// The nonce probe (2b): ask the hub to run `echo <nonce>` and see whether it
// comes back.
//
// This is deliberately BEHAVIOURAL rather than another environment read. It
// tests the property the whole design rests on — a pinned host cannot run a
// command the client chose — so it is immune to every environment-variable quirk
// of any interceptor, present or future. Measured contrast: with a forced
// command the requested `echo` does not run; without one it does.
func TestNonceProbeInvocationReplacesTheSentinelAndNothingElse(t *testing.T) {
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example:2222/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "codex_ed25519")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	base, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: "codex", KeyPath: key, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	probe, nonce, err := buildPinningProbe(SSHBuildOptions{Remote: remote, AgentID: "codex", KeyPath: key, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	// The nonce must be shell-inert whatever login shell the hub runs: 16 random
	// bytes, hex. No metacharacter can appear, so the probe cannot be turned into
	// an injection by the hub's own shell.
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(nonce) {
		t.Fatalf("nonce = %q, want 32 lowercase hex characters", nonce)
	}

	if got, want := probe.Args[len(probe.Args)-1], "echo "+nonce; got != want {
		t.Fatalf("last argument = %q, want %q", got, want)
	}
	// Everything BEFORE the command must be identical to the real invocation.
	// The probe is meant to differ in exactly one place; anything else and it is
	// answering a question about a different connection than the one that failed.
	if a, b := strings.Join(base.Args[:len(base.Args)-1], " "), strings.Join(probe.Args[:len(probe.Args)-1], " "); a != b {
		t.Fatalf("the probe changed more than the command:\nreal:  %s\nprobe: %s", a, b)
	}
	if base.Args[len(base.Args)-1] != "agentchute-hub" {
		t.Fatalf("the real invocation no longer ends in the sentinel: %q", base.Args[len(base.Args)-1])
	}
}

// Two probes must not collide: each gets its own nonce, or a stale mux master's
// output could satisfy the next probe.
func TestEachPinningProbeGetsItsOwnNonce(t *testing.T) {
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "codex_ed25519")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := SSHBuildOptions{Remote: remote, AgentID: "codex", KeyPath: key, StateDir: dir}
	_, first, err := buildPinningProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := buildPinningProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two probes shared a nonce (%s); a reused one can be satisfied by an earlier probe's output", first)
	}
}

// The verdict reads the nonce as a WHOLE LINE.
//
// Substring matching would be satisfied by a hub that merely echoed the command
// back, or by any output that happened to contain it — and the whole point is
// that the hub RAN what we asked. Everything else, including exit 127, means
// pinned as far as this check is concerned: on a pinned host a missing binary is
// a different problem and correctly not this check's business.
func TestNonceVerdictRequiresTheWholeLine(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	rows := []struct {
		name     string
		stdout   string
		unpinned bool
	}{
		{"exact line", nonce + "\n", true},
		{"line among others", "motd\n" + nonce + "\n", true},
		{"no trailing newline", nonce, true},
		{"empty — a pinned host's hello EOF", "", false},
		{"substring only", "prefix" + nonce + "suffix\n", false},
		{"the command echoed back, not run", "echo " + nonce + "\n", false},
		{"unrelated", "command not found: agentchute-hub\n", false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := nonceEchoed(row.stdout, nonce); got != row.unpinned {
				t.Fatalf("nonceEchoed(%q) = %v, want %v", row.stdout, got, row.unpinned)
			}
		})
	}
}
