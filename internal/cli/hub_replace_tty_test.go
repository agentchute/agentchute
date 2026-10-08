package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Follow-up to review 2026-10-08 S10 (PR #216's open residual): --replace-key
// swapped the key of an id that already HAS a hub key with no terminal check —
// the same takeover --takeover guards, for a lane that was already remote. It
// is now gated exactly like --takeover.
func TestHubAuthorizeReplaceKeyNeedsATerminal(t *testing.T) {
	home, pool, _, key := setupHubAuthorizeTest(t)
	authorized := filepath.Join(home, ".ssh", "authorized_keys")
	tty := false
	orig := hubAuthorizeStdinIsTTY
	hubAuthorizeStdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { hubAuthorizeStdinIsTTY = orig })

	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first authorize of a fresh id = %v (no terminal needed)", err)
	}
	before := readAuthorizeTestFile(t, authorized)
	key2 := "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("attacker-key"))

	err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key2, ReplaceKey: true}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("--replace-key without a terminal = %v, want a refusal", err)
	}
	if after := readAuthorizeTestFile(t, authorized); !bytes.Equal(before, after) {
		t.Fatal("a refused --replace-key changed authorized_keys")
	}

	tty = true
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key2, ReplaceKey: true}, &bytes.Buffer{}); err != nil {
		t.Fatalf("--replace-key on a terminal = %v, want allowed", err)
	}
	if got := string(readAuthorizeTestFile(t, authorized)); !strings.Contains(got, strings.Fields(key2)[1]) {
		t.Fatalf("--replace-key on a terminal did not replace: %q", got)
	}

	// Re-authorizing the SAME key is not a swap and needs no terminal.
	tty = false
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key2, ReplaceKey: true}, &bytes.Buffer{}); err != nil {
		t.Fatalf("--replace-key with the key already authorized = %v, want a no-op", err)
	}
}

// Rotation's auto-authorize asks the hub for a --replace-key. With no terminal
// here, the hub would refuse it, so the ssh is not attempted at all: the join
// prints the single-quoted command for the operator to run on the hub.
func TestHubJoinRotationWithoutATerminalPrintsTheAuthorizeCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	origTTY, origAuth := hubJoinStdinIsTTY, hubJoinAutoAuthorize
	hubJoinStdinIsTTY = func() bool { return false }
	hubJoinAutoAuthorize = runHubJoinAutoAuthorize
	t.Cleanup(func() { hubJoinStdinIsTTY, hubJoinAutoAuthorize = origTTY, origAuth })

	if err := runHubJoinAutoAuthorize(remote, "codex-tiny", "ssh-ed25519 AAAA k", true); !errors.Is(err, errHubReplaceNeedsTerminal) {
		t.Fatalf("auto-authorize --replace-key without a terminal = %v, want errHubReplaceNeedsTerminal before any ssh", err)
	}

	pubPath := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(pubPath, []byte("ssh-ed25519 AAAAC3Nz agentchute:codex-tiny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ok bool
	out, _ := captureStdout(t, func() error {
		var err error
		ok, err = authorizeHubJoinKey(remote, "codex-tiny", hubKeyVersion{Private: strings.TrimSuffix(pubPath, ".pub"), Public: pubPath}, true)
		return err
	})
	if ok {
		t.Fatal("a rotation that could not authorize reported success")
	}
	if want := "agentchute hub authorize --agent 'codex-tiny' --pool '/home/alex/pool' --key 'ssh-ed25519 AAAAC3Nz agentchute:codex-tiny' --replace-key"; !strings.Contains(out, want) {
		t.Fatalf("output does not hand the operator the command:\n%s\nwant it to contain\n  %s", out, want)
	}
	if !strings.Contains(out, "terminal") {
		t.Fatalf("output does not say why it did not authorize itself:\n%s", out)
	}
}

func TestHubAutoAuthorizeSSHAllocatesATerminalOnlyForAnInteractiveOperator(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	if args := hubAutoAuthorizeSSHArgs(remote, "cmd", true); !containsArg(args, "-t") {
		t.Fatalf("interactive operator: argv %v has no -t, so the hub's --replace-key gate cannot see a terminal", args)
	}
	if args := hubAutoAuthorizeSSHArgs(remote, "cmd", false); containsArg(args, "-t") {
		t.Fatalf("non-interactive: argv %v forces a terminal", args)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
