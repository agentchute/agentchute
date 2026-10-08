package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubwire"
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

// Shell spellings that the executing shell folds into the same invocation: a
// continuation right after the binary, $IFS or an ANSI-C whitespace escape
// between words, and the bare braced ${AGENTCHUTE_BIN}.
func TestGuardDeniesHubAuthorizeAndJoinThroughShellFolding(t *testing.T) {
	for _, cmd := range []string{
		"agentchute \\\nhub join ssh://h/p --as y",
		"agentchute hub${IFS}join ssh://h/p --as y",
		"agentchute hub$IFS'join' ssh://h/p --as y",
		"agentchute hub$'\\t'authorize --list",
		"${AGENTCHUTE_BIN} hub join ssh://h/p --as y",
	} {
		if !guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard allowed %q while latched", cmd)
		}
	}
}

// Review 2026-10-08 S11: ClearAllForwardings clears port forwards only. A user
// config with ForwardAgent yes handed the ssh agent to the hub — and the
// auto-authorize ssh passed no forwarding options at all, on the one path that
// reaches an unrestricted login.
func TestHubAutoAuthorizeSSHDisablesAgentAndX11Forwarding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example:2222/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	args := hubAutoAuthorizeSSHArgs(remote, "'agentchute' 'hub' 'authorize'")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-o ForwardAgent=no", "-o ForwardX11=no", "-o ConnectTimeout=5", "-p 2222"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("auto-authorize ssh argv %v is missing %q", args, want)
		}
	}
	if last := args[len(args)-1]; last != "'agentchute' 'hub' 'authorize'" {
		t.Fatalf("remote command must stay the last argument, got %q", last)
	}
}

// `hub session` is the hub's forced command; run directly it serves the wire for
// whatever --agent it is given, so it is held like authorize and join.
func TestGuardDeniesHubSessionWhileLatched(t *testing.T) {
	if !guardCommandDenied("Bash agentchute hub session --agent claude-code --pool /p --pool-id 0123456789ab") {
		t.Fatal("guard allowed a direct hub session while latched")
	}
}

// Review 2026-10-08 S11: joining replaced an existing ssh:// pointer to a
// DIFFERENT hub without asking, repointing every lane in this checkout at a hub
// of the caller's choosing. That now needs --replace, and it is refused before
// any key is minted or any connection is made.
func TestHubJoinRefusesToRepointAtADifferentHubWithoutReplace(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	other := "ssh://alex@other-hub.example/home/alex/pool"
	pointer := filepath.Join(root, loop.PointerFileName)
	if err := os.WriteFile(pointer, []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probes := 0
	hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
		probes++
		return successfulHubHello("codex-tiny"), nil, nil
	}
	var err error
	withCwd(t, root, func() { err = cmdHubJoin([]string{remote.URL, "--name", "codex"}) })
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("join over another hub's pointer = %v, want a refusal naming --replace", err)
	}
	if data, _ := os.ReadFile(pointer); strings.TrimSpace(string(data)) != other {
		t.Fatalf("pointer = %q, want it untouched", data)
	}
	if probes != 0 {
		t.Fatalf("the refused join still dialed the hub %d time(s)", probes)
	}
	if _, statErr := os.Stat(filepath.Join(remote.HubDir, "keys")); !os.IsNotExist(statErr) {
		t.Fatalf("the refused join minted a key (stat: %v)", statErr)
	}

	withCwd(t, root, func() { err = cmdHubJoin([]string{remote.URL, "--name", "codex", "--replace"}) })
	if err != nil {
		t.Fatalf("join --replace = %v, want it to proceed", err)
	}
	if data, _ := os.ReadFile(pointer); strings.TrimSpace(string(data)) != remote.URL {
		t.Fatalf("pointer after --replace = %q, want %s", data, remote.URL)
	}
}
