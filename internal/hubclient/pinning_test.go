package hubclient

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

// 2c has THREE outcomes, and the third is the one that matters most.
//
// The second probe may only ever NARROW the message. It must never be the thing
// that decides an operator is at fault, because it runs with -F /dev/null — the
// very isolation ruled out of scope for the real path, since it can make a
// ProxyJump-only hub unreachable. A probe that cannot connect has learned
// nothing, and "learned nothing" must not read as "producer 2".
func TestSecondProbeNarrowsOnlyWhatItActuallyObserved(t *testing.T) {
	rows := []struct {
		name   string
		stdout func(nonce string) string
		stderr string
		err    error
		want   pinningVerdict
	}{
		{
			name:   "an unauthorizable key still ran the command",
			stdout: func(n string) string { return n + "\n" },
			want:   pinningIntercepted,
		},
		{
			name:   "the key mattered — publickey refused",
			stdout: func(string) string { return "" },
			stderr: "alex@hub.example: Permission denied (publickey,password).",
			want:   pinningOperatorFallback,
		},
		{
			name:   "connect failure narrows NOTHING",
			stdout: func(string) string { return "" },
			stderr: "ssh: connect to host hub.example port 22: Operation timed out",
			want:   pinningUnpinnedUnattributed,
		},
		{
			name:   "proxy failure narrows NOTHING",
			stdout: func(string) string { return "" },
			stderr: "ssh: Could not resolve hostname jump.internal",
			want:   pinningUnpinnedUnattributed,
		},
		{
			name:   "timeout narrows NOTHING",
			stdout: func(string) string { return "" },
			err:    errProbeTimeout,
			want:   pinningUnpinnedUnattributed,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			opts := probeFixture(t)
			original := hubProbeRunner
			t.Cleanup(func() { hubProbeRunner = original })
			hubProbeRunner = func(args []string, _ time.Duration) (string, string, error) {
				// The second probe MUST isolate identity, or it is asking the same
				// question as the first and its answer means nothing.
				joined := strings.Join(args, " ")
				if !strings.Contains(joined, "-F /dev/null") || !strings.Contains(joined, "IdentityAgent=none") {
					t.Fatalf("second probe did not isolate the identity: %s", joined)
				}
				return row.stdout(nonceFrom(args)), row.stderr, row.err
			}
			if got := attributeUnpinnedHub(opts); got != row.want {
				t.Fatalf("verdict = %v, want %v", got, row.want)
			}
		})
	}
}

// 2b decides pinned vs not, and a pinned host must never reach the second probe
// at all — running it would cost an authentication and could only add noise.
func TestPinnedHubNeverReachesTheSecondProbe(t *testing.T) {
	opts := probeFixture(t)
	original := hubProbeRunner
	t.Cleanup(func() { hubProbeRunner = original })
	calls := 0
	hubProbeRunner = func(args []string, _ time.Duration) (string, string, error) {
		calls++
		// A pinned host starts a real hub session, hits EOF at the hello read and
		// writes nothing.
		return "", "", nil
	}
	if got, reason := probeHubPinning(opts); got != pinningPinned {
		t.Fatalf("verdict = %v (%s), want pinned", got, reason)
	}
	if calls != 1 {
		t.Fatalf("ran %d probes against a pinned hub; the second one is for narrowing an unpinned verdict", calls)
	}
}

func probeFixture(t *testing.T) SSHBuildOptions {
	t.Helper()
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "codex_ed25519")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return SSHBuildOptions{Remote: remote, AgentID: "codex", KeyPath: key, StateDir: dir}
}

func nonceFrom(args []string) string {
	last := args[len(args)-1]
	return strings.TrimPrefix(last, "echo ")
}

// Site 2 — exit 127 has two causes and they need opposite remedies. BOTH arms,
// or the fix trades one wrong message for another.
func TestExit127IsClassifiedByTheProbeNotTheShellText(t *testing.T) {
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name     string
		verdict  pinningVerdict
		wantCode string
		wantSays []string
	}{
		{
			name: "pinned host, binary really missing", verdict: pinningPinned,
			wantCode: "E_HUB_NO_BINARY",
			wantSays: []string{"DOES apply a forced command", "Reinstall agentchute on the hub"},
		},
		{
			name: "producer 1 — intercepting layer", verdict: pinningIntercepted,
			wantCode: "E_HUB_UNPINNED",
			wantSays: []string{"NOT PINNED", "revoking a key here cuts off nothing", "tailscale set --ssh=false"},
		},
		{
			name: "producer 2 — operator fallback", verdict: pinningOperatorFallback,
			wantCode: "E_HUB_UNPINNED",
			wantSays: []string{"authorization problem wearing", "agentchute hub authorize --agent codex"},
		},
		{
			name: "unattributed — names BOTH remedies", verdict: pinningUnpinnedUnattributed,
			wantCode: "E_HUB_UNPINNED",
			wantSays: []string{"NOT PINNED", "narrows nothing", "agentchute hub authorize"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			original := hubPinningProbe
			t.Cleanup(func() { hubPinningProbe = original })
			hubPinningProbe = func(SSHBuildOptions) (pinningVerdict, string) { return row.verdict, "" }

			err := classifyExit127(remote, "codex", "zsh:1: command not found: agentchute-hub\n")
			if got := ErrorCode(err); got != row.wantCode {
				t.Fatalf("code = %s, want %s (%v)", got, row.wantCode, err)
			}
			for _, want := range row.wantSays {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("message does not say %q: %v", want, err)
				}
			}
			// The remote's own words ride along as CORROBORATION. They must never
			// be the discriminator — this row's stderr is zsh's, and fish says
			// something different for the identical condition.
			if !strings.Contains(err.Error(), "the hub said: zsh:1: command not found: agentchute-hub") {
				t.Fatalf("the remote's last line is not carried as corroboration: %v", err)
			}
		})
	}
}

// The old message named a path the client never used. Whatever else changes, it
// must not come back.
func TestExit127NeverNamesAHardcodedInstallPath(t *testing.T) {
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []pinningVerdict{pinningPinned, pinningIntercepted, pinningOperatorFallback, pinningUnpinnedUnattributed} {
		original := hubPinningProbe
		hubPinningProbe = func(SSHBuildOptions) (pinningVerdict, string) { return verdict, "" }
		got := classifyExit127(remote, "codex", "").Error()
		hubPinningProbe = original
		if strings.Contains(got, "/usr/local/bin/agentchute") {
			t.Fatalf("verdict %v still names a hardcoded install path the client never used: %s", verdict, got)
		}
	}
}
