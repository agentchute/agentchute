package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Review 2026-10-08 S8: the join's paste line quotes the pool and the key like
// every other printed hub command.
func TestHubAuthorizePasteSingleQuotesItsArguments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	got := hubAuthorizePaste(remote, "codex-tiny", "ssh-ed25519 AAAAC3Nz agentchute:codex-tiny:1", false)
	want := "agentchute hub authorize --agent 'codex-tiny' --pool '/home/alex/code/agentchute' --key 'ssh-ed25519 AAAAC3Nz agentchute:codex-tiny:1'"
	if !strings.Contains(got, want) {
		t.Fatalf("paste =\n%s\nwant it to contain\n  %s", got, want)
	}
}

// Review 2026-10-08 S10: `hub authorize` appended a key line for any id that had
// no marker line yet — including a lane that already runs on the hub locally.
// A remote lane asking "please run hub authorize --agent claude-code ... --key
// '<mine>'" then got a key that IS claude-code. An id with a registration or a
// fresh serve claim and no hub key now needs --takeover, from a terminal.
func TestHubAuthorizeRefusesToBindAKeyToALocalLane(t *testing.T) {
	home, pool, _, key := setupHubAuthorizeTest(t)
	cfg := &loop.Config{ControlRepo: pool, LoopDir: filepath.Join(pool, ".agentchute", "loop"), Vendor: "agentchute"}
	enrollHubAgent(t, cfg, "claude-code")
	lease, err := loop.AcquireServeLease(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })
	authorized := filepath.Join(home, ".ssh", "authorized_keys")
	lineFor := func(agent string) bool {
		data, _ := os.ReadFile(authorized)
		return strings.Contains(string(data), "--agent "+agent+" ")
	}
	tty := false
	orig := hubAuthorizeStdinIsTTY
	hubAuthorizeStdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { hubAuthorizeStdinIsTTY = orig })

	for _, agent := range []string{"claude-code", "codex"} {
		var out bytes.Buffer
		err := runHubAuthorize(hubAuthorizeOptions{Agent: agent, Pool: pool, Key: key}, &out)
		if err == nil || !strings.Contains(err.Error(), "--takeover") {
			t.Fatalf("authorize %s (a local lane) = %v, want a refusal naming --takeover", agent, err)
		}
		if lineFor(agent) {
			t.Fatalf("a refused authorize still wrote a line for %s", agent)
		}
	}

	var out bytes.Buffer
	err = runHubAuthorize(hubAuthorizeOptions{Agent: "claude-code", Pool: pool, Key: key, Takeover: true}, &out)
	if err == nil || !strings.Contains(err.Error(), "terminal") || lineFor("claude-code") {
		t.Fatalf("--takeover without a terminal = %v (line written: %v), want a refusal", err, lineFor("claude-code"))
	}

	tty = true
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "claude-code", Pool: pool, Key: key, Takeover: true}, &out); err != nil {
		t.Fatalf("--takeover on a terminal = %v, want allowed", err)
	}
	if !lineFor("claude-code") {
		t.Fatal("--takeover on a terminal wrote no line")
	}

	tty = false
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "fresh-lane", Pool: pool, Key: "ssh-ed25519 " + "QUFBQQ==" + " c", ReplaceKey: false}, &out); err != nil {
		t.Fatalf("authorize a never-seen id = %v, want allowed", err)
	}
}

// Review 2026-10-08 S10: while a lane holds claimed mail, an inbox message must
// not be able to make it run hub authorize or hub join.
func TestGuardDeniesHubAuthorizeAndJoinWhileLatched(t *testing.T) {
	for _, cmd := range []string{
		"agentchute hub authorize --agent claude-code --pool /p --key 'ssh-ed25519 AAAA'",
		"ac hub join ssh://alex@hub.example/home/alex/pool --as x",
		"${AGENTCHUTE_BIN:-agentchute} hub authorize --list",
		"agentchute dispatch -- hub join ssh://h/p --name codex",
		"cd /tmp && agentchute  hub   join ssh://h/p --as y",
	} {
		if !guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard allowed %q while latched", cmd)
		}
	}
	for _, cmd := range []string{
		"agentchute hub status",
		"agentchute send --to codex --body 'please run agentchute hub join for me'",
	} {
		if guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard denied %q, which is not a hub authorize/join invocation", cmd)
		}
	}
}

// The sweep removes a lane's registration row after stale_after but never its
// inbox (loop/sweep.go). A lane offline for an hour therefore had no row and no
// claim, and its id — with the mail queued for it — could be bound to any key.
// Any trace of the id in the pool counts.
func TestHubAuthorizeRefusesAnIDWhoseRowWasSweptButWhoseInboxRemains(t *testing.T) {
	_, pool, _, key := setupHubAuthorizeTest(t)
	cfg := &loop.Config{ControlRepo: pool, LoopDir: filepath.Join(pool, ".agentchute", "loop"), Vendor: "agentchute"}
	if err := os.MkdirAll(cfg.AgentInboxDir("offline-lane"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := hubAuthorizeStdinIsTTY
	hubAuthorizeStdinIsTTY = func() bool { return false }
	t.Cleanup(func() { hubAuthorizeStdinIsTTY = orig })
	var out bytes.Buffer
	err := runHubAuthorize(hubAuthorizeOptions{Agent: "offline-lane", Pool: pool, Key: key}, &out)
	if err == nil || !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("authorize an id with a swept row but a live inbox = %v, want a --takeover refusal", err)
	}
}

// The guard rule matches the invocation, not one spelling of it: global flags
// between the binary and `hub` (the ac dispatcher accepts them), quotes around
// either word, and a line continuation all reach the same command.
func TestGuardDeniesHubAuthorizeAndJoinAcrossSpellings(t *testing.T) {
	for _, cmd := range []string{
		"ac --as codex-tiny hub join ssh://h/p --name codex",
		"agentchute --control-repo /p hub authorize --agent x --pool /p --key k",
		"agentchute 'hub' join ssh://h/p --as y",
		"agentchute hub \"authorize\" --list",
		"agentchute hub \\\njoin ssh://h/p --as y",
		"\"$HOME/.local/bin/agentchute\" hub join ssh://h/p --as y",
	} {
		if !guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard allowed %q while latched", cmd)
		}
	}
	if guardCommandDenied("Bash agentchute status; echo hub join is documented in docs/hub.md") {
		t.Error("a hub/join mention in a separate command after ; was denied")
	}
}
