//go:build sshd_integration

package sshd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
)

// Rows 6-9: the predicate against REAL sshd, on both CI platforms.
//
// The finding said the unpinned case was only reproducible on a Tailscale host.
// It is not: an authorized_keys line WITHOUT command= reproduces the exact
// property under test — "this host does not apply a forced command" — inside the
// existing harness. That turns the bypass from a manual campaign step into a CI
// row.
//
// HONEST LIMIT, written here rather than discovered later: a command=-less line
// models FORCED COMMAND ABSENT, which is the property the predicate turns on. It
// does NOT model Tailscale's identity/ACL layer. The tiny row stays as the
// end-to-end confirmation; CI gets the mechanism.
func TestSSHDPinnedSessionServes(t *testing.T) {
	h := newSSHDHarness(t)
	hello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{})
	if hello.Agent != "codex" {
		t.Fatalf("hello = %+v", hello)
	}
}

// Row 7 — a second session over a LIVE mux master still serves.
//
// Without this row a predicate that lost SSH_ORIGINAL_COMMAND on attached
// sessions would break every one-shot after the first, and nothing else in the
// suite would catch it. Measured: every session attached to a master carries its
// own value and is still overridden.
func TestSSHDPinnedSessionServesOverALiveMuxMaster(t *testing.T) {
	h := newSSHDHarness(t)
	if hello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{}); hello.Agent != "codex" {
		t.Fatalf("first hello = %+v", hello)
	}
	// The first session leaves a ControlPersist master behind; this one attaches.
	if hello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{}); hello.Agent != "codex" {
		t.Fatalf("second hello over the live master = %+v", hello)
	}
}

// Row 9 — producer 1, modelled: a key with NO forced command, invoking the hub
// session directly. This is the finding's actual attack, in CI.
//
// The refusal must be E_UNPINNED, and the pool must be untouched: an unpinned
// caller choosing its own --agent and --pool is precisely what must not be served.
func TestSSHDUnpinnedKeyIsRefusedAndTouchesNothing(t *testing.T) {
	h := newSSHDHarness(t)
	before := poolTree(t, h.pool)

	// h.adminKey is already an UNRESTRICTED line in authorized_keys — no
	// command=, added by addAdminKey. That is producer 1's shape without any
	// harness surgery.
	out, err := sshDirect(t, h, h.adminKey, h.binary+" hub session --agent codex --pool "+h.pool+" --pool-id "+sshdPoolID)
	if err == nil {
		t.Fatalf("an unpinned caller was served; it chose its own --agent and --pool:\n%s", out)
	}
	if !strings.Contains(out, "E_UNPINNED") && !strings.Contains(out, "did not apply an `authorized_keys` forced command") {
		t.Fatalf("refusal is not the unpinned one:\n%s", out)
	}
	if after := poolTree(t, h.pool); after != before {
		t.Fatalf("the refused session wrote into the pool:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// sshDirect runs ssh WITHOUT the harness wrapper.
//
// The wrapper forces -F /dev/null -o IdentitiesOnly=yes (the F1 fix from #162),
// which is exactly what makes an operator-fallback unreproducible through it —
// two rows would otherwise pass by never reaching the condition. Rows that care
// about WHICH identity authenticates must call h.ssh themselves.
func sshDirect(t *testing.T, h *sshdHarness, key, remoteCommand string) (string, error) {
	t.Helper()
	args := []string{
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + h.knownHosts,
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-i", key,
		"-p", strconv.Itoa(h.port),
		h.user + "@127.0.0.1",
		remoteCommand,
	}
	cmd := exec.Command(h.ssh, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// poolTree is every path under the pool with its size — "touched nothing" has to
// mean exactly that, not "no file I thought to check".
func poolTree(t *testing.T, pool string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(pool, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(pool, path)
		b.WriteString(rel)
		if !info.IsDir() {
			b.WriteString(":" + strconv.FormatInt(info.Size(), 10))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Rows 13/14 — producer 2, the operator fallback, and its control.
//
// authorized_keys carries BOTH a pinned command= line for codex and an
// unrestricted "operator" line (h.adminKey, which addAdminKey already writes).
// The client offers a codex key that is NOT authorized, against an ssh_config
// whose IdentityFile is the operator key. ssh falls back to it, the hub accepts
// unrestricted, and the sentinel reaches a login shell — exit 127 with the binary
// perfectly fine.
//
// These MUST NOT go through h.clientBin. That wrapper forces
// -F /dev/null -o IdentitiesOnly=yes (the F1 fix from #162), which is exactly what
// makes an operator fallback unreproducible: the row would pass by never reaching
// the condition, which is the green-for-the-wrong-reason class this milestone has
// already been bitten by three times.
func TestSSHDOperatorFallbackIsSeenAsUnpinnedNotAsAMissingBinary(t *testing.T) {
	h := newSSHDHarness(t)

	// A codex key the hub has never authorized.
	unauthorized := filepath.Join(t.TempDir(), "codex_unauthorized_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "agentchute:codex", "-f", unauthorized)

	// An operator ssh_config that supplies the unrestricted key — the thing the
	// client never named and ssh offers anyway.
	cfg := filepath.Join(t.TempDir(), "operator_ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n  IdentityFile "+h.adminKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := sshWithOperatorConfig(t, h, cfg, unauthorized, "agentchute-hub")
	if err == nil {
		t.Fatalf("the sentinel was not refused; expected it to reach a login shell:\n%s", out)
	}
	// The hub ran what the CLIENT asked for, which is the whole signature: no
	// forced command was applied.
	if !strings.Contains(out, "agentchute-hub") {
		t.Fatalf("the sentinel did not reach a shell, so this row did not reproduce producer 2:\n%s", out)
	}
	if strings.Contains(out, "E_UNPINNED") {
		t.Fatalf("the hub session ran at all; producer 2 means it never started:\n%s", out)
	}
}

// Row 14 — the control. Same authorized_keys, same operator config, but codex's
// key IS authorized. Without this a fix that cried "unpinned" whenever a config
// identity exists would pass row 13 and break every healthy hub.
func TestSSHDOperatorConfigDoesNotBreakAPinnedSession(t *testing.T) {
	h := newSSHDHarness(t)
	cfg := filepath.Join(t.TempDir(), "operator_ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n  IdentityFile "+h.adminKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// codex's REAL key, which is authorized with a forced command. ssh must prefer
	// the explicitly named identity, the forced command must apply, and the
	// requested command must NOT run.
	out, err := sshWithOperatorConfig(t, h, cfg, h.keys["codex"], "echo THIS_MUST_NOT_RUN")
	if err != nil && strings.Contains(out, "THIS_MUST_NOT_RUN") {
		t.Fatalf("a pinned host ran a command the client chose:\n%s", out)
	}
	if strings.Contains(out, "THIS_MUST_NOT_RUN") {
		t.Fatalf("the forced command did not override the requested one:\n%s", out)
	}
}

// sshWithOperatorConfig runs ssh with an EXPLICIT -F pointing at an operator
// config, deliberately NOT through the harness wrapper. IdentitiesOnly is left
// unset, because the whole condition under test is ssh widening the identity set
// beyond what the caller named.
func sshWithOperatorConfig(t *testing.T, h *sshdHarness, config, key, remoteCommand string) (string, error) {
	t.Helper()
	args := []string{
		"-F", config,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + h.knownHosts,
		"-o", "IdentityAgent=none",
		"-i", key,
		"-p", strconv.Itoa(h.port),
		h.user + "@127.0.0.1",
		remoteCommand,
	}
	out, err := exec.Command(h.ssh, args...).CombinedOutput()
	return string(out), err
}

// Row 12 — the two exit-127 causes, distinguished on REAL sshd by what the host
// actually does. Both arms, because one alone trades one wrong message for
// another.
//
// What this row does NOT do, deliberately: it does not call PinningVerdict. That
// runs the probes, which open their own ssh connections with ControlPersist by
// design, and a master surviving the row makes the fixture report "sshd did not
// exit" during teardown. The row's assertions passed; the harness's shutdown did
// not. Rather than weaken the probe to suit a fixture, the verdict logic is
// pinned by the unit table in internal/hubclient (four verdicts, both arms of the
// classifier, each mutation red), and this row pins the thing only real sshd can
// show: WHAT THE HOST DOES with the sentinel.
func TestSSHDExit127HasTwoCausesRealSSHDCanTellApart(t *testing.T) {
	t.Run("unpinned: the sentinel reaches a login shell", func(t *testing.T) {
		h := newSSHDHarness(t)
		addUnrestrictedAgent(t, h, "drifter")
		out, err := sshDirect(t, h, h.keys["drifter"], "agentchute-hub")
		if err == nil {
			t.Fatalf("the sentinel ran successfully on an unpinned host:\n%s", out)
		}
		// The login shell got it — that IS the 127, and the hub binary is fine.
		if !strings.Contains(out, "agentchute-hub") {
			t.Fatalf("the sentinel never reached a shell, so this arm did not reproduce:\n%s", out)
		}
		if _, statErr := os.Stat(h.binary); statErr != nil {
			t.Fatalf("precondition: the hub binary must EXIST, or this arm proves nothing: %v", statErr)
		}
	})

	t.Run("pinned: the forced command overrides whatever was asked for", func(t *testing.T) {
		h := newSSHDHarness(t)
		out, _ := sshDirect(t, h, h.keys["codex"], "echo THIS_MUST_NOT_RUN")
		if strings.Contains(out, "THIS_MUST_NOT_RUN") {
			t.Fatalf("a pinned host ran a command the client chose:\n%s", out)
		}
	})
}

// addUnrestrictedAgent mints a key and authorizes it with NO command= — producer
// 1's shape for an id the client can actually join as.
func addUnrestrictedAgent(t *testing.T, h *sshdHarness, id string) {
	t.Helper()
	key := filepath.Join(h.clientState, "keys", id+"_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "agentchute:"+id, "-f", key)
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(h.authorized, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(pub); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	h.keys[id] = key
}
