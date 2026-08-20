//go:build sshd_integration

package sshd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
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

// Row 8 — the predicate accepts any NON-EMPTY original command, not the sentinel
// string. A hub that string-matched `agentchute-hub` would be coupled to the
// client version that sends it, and every older or newer client would be refused
// by a check that is supposed to be about pinning. helloOverSession already
// requests a non-sentinel string; this row says so out loud and picks one that
// could not be mistaken for the sentinel.
func TestSSHDPinnedSessionServesForAnyRequestedCommand(t *testing.T) {
	h := newSSHDHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := h.open(ctx, "codex", "definitely-not-the-sentinel-v99", hubclient.SSHBuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client, err := hubclient.OpenOneShotTransport(raw, h.remote, "codex", "sshd-integration")
	if err != nil {
		t.Fatal(err)
	}
	hello := client.Hello()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if hello.Agent != "codex" {
		t.Fatalf("a pinned session was refused for requesting a non-sentinel command: %+v", hello)
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

	// WHY THE CLASSIFIER IS NOT DRIVEN FROM THIS ROW, measured rather than
	// assumed. The review asked for it here. Production never passes -F, so the
	// only way to make ssh read an operator config is to move HOME — and `ssh -v`
	// under a moved HOME shows it reading /Users/<me>/.ssh/config and offering
	// /Users/<me>/.ssh/id_ed25519 anyway: on macOS ssh resolves ~ through getpwuid,
	// not $HOME. The row would then pass or fail on whether the machine running it
	// happens to have an ssh config, and would silently test the developer's own
	// key. That is the isolation failure this milestone has already been bitten
	// by, so it is refused here.
	//
	// The classifier is driven instead where the fixture CAN be deterministic:
	// TestSSHDExit127IsReclassifiedByCauseNotByGuess dials the production path
	// against an unrestricted authorized line and asserts E_HUB_UNPINNED with the
	// producer-2 remedy. Same classifier, same real sshd, no dependency on
	// whatever ssh config the host machine keeps. What THIS row pins is the half
	// that needs a config: ssh widening the identity set past what the caller
	// named, under production's own IdentitiesOnly=yes.
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
// config, deliberately NOT through the harness wrapper.
//
// IdentitiesOnly=yes is SET, matching production BuildSSHInvocation, and that is
// the point rather than a detail: the defect being reproduced is that
// IdentitiesOnly does not do what its name suggests. It excludes the agent and
// the default identity files, but a config-named IdentityFile is still offered.
// Leaving it unset would have reproduced something weaker than production and let
// a "fix" that merely sets IdentitiesOnly look like it closed the hole.
func sshWithOperatorConfig(t *testing.T, h *sshdHarness, config, key, remoteCommand string) (string, error) {
	t.Helper()
	args := []string{
		"-F", config,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + h.knownHosts,
		"-o", "IdentityAgent=none",
		"-o", "IdentitiesOnly=yes",
		"-i", key,
		"-p", strconv.Itoa(h.port),
		h.user + "@127.0.0.1",
		remoteCommand,
	}
	// The comment above claims IdentitiesOnly=yes is set, and a comment cannot
	// enforce itself: this helper shipped for one round with the option in the
	// WRONG function while the comment here said otherwise, and every row still
	// passed. Assert the claim instead of asserting it in prose.
	assertHasOption(t, args, "IdentitiesOnly=yes")
	out, err := exec.Command(h.ssh, args...).CombinedOutput()
	return string(out), err
}

func assertHasOption(t *testing.T, args []string, option string) {
	t.Helper()
	for _, arg := range args {
		if arg == option {
			return
		}
	}
	t.Fatalf("invocation is missing -o %s, so it is not production-equivalent: %q", option, args)
}

// Row 12 — the two exit-127 causes, distinguished on REAL sshd by what the host
// actually does. Both arms, because one alone trades one wrong message for
// another.
//
// WHERE THE CLASSIFICATION ITSELF IS COVERED — read this before deleting either
// row as redundant. This row covers only the HOST's behaviour: what sshd does
// with the sentinel, pinned and unpinned. The end-to-end classification of that
// behaviour into an error code lives in
// TestSSHDExit127IsReclassifiedByCauseNotByGuess, which dials the production path
// on the same real sshd and asserts E_HUB_UNPINNED for an unpinned host and
// E_HUB_NO_BINARY for a pinned host whose binary is gone. The verdict enum and its
// three-way attribution are pinned separately by the unit table in
// internal/hubclient.
//
// This comment previously said the verdict was pinned ONLY by that unit table.
// That stopped being true when the arms-A/B row landed, and a stale
// coverage-lives-elsewhere claim is how a row gets deleted for being redundant
// when it is not. It is the third time in this program that a comment outlived
// the arrangement it described.
//
// This row does not call PinningVerdict, and that is now a choice rather than a
// constraint: the ControlPersist master that made the fixture report "sshd did
// not exit" is handled by rememberProbeMux, which the rows that do run the probes
// use. Keeping this row probe-free keeps it a statement about sshd alone.
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

// addUnrestrictedAgent mints a key and authorizes it with NO command= — PRODUCER
// 2's shape: sshd authenticates the connection, but no forced command applies, so
// the request reaches a login shell. (Producer 1 is sshd being bypassed entirely;
// see the note above TestSSHDPinningProbeVerdicts for why real sshd cannot show
// it.)
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

// Rows 10/11/15 — the probes themselves, against real sshd.
//
// SPEC INCONSISTENCY, reported rather than papered over, and since ratified as
// the spec's rather than this row's — the spec paragraph is being amended after
// merge. The spec calls a
// `command=`-less authorized_keys line "producer 1, modelled" (rows 9-11) and
// expects row 10 to yield the NOT PINNED text — but the same spec's 2c attribution
// decides producer 1 vs 2 by whether an UNAUTHORIZABLE throwaway key still runs
// the command. On real sshd that key is refused, so this fixture attributes as
// producer 2, always. The two halves of the spec cannot both hold. The classifier
// is the half that is right: attribution follows what was observed, and the spec's
// own "honest limit" paragraph already concedes this fixture does not model
// Tailscale's identity layer. Row 10 below asserts what actually happens.
//
// DEVIATION FROM THE SPEC'S ROWS, ratified by the fidelity review as tautological
// rather than effort: producer 1
// — an intercepting layer such as Tailscale SSH — is NOT reproducible here, and I
// would rather say so than fake it. Producer 1 is defined by sshd being bypassed,
// so a fixture sshd cannot exhibit it: reproducing its predicate (an arbitrary
// unknown key authenticates AND the client's command runs) needs either
// AuthorizedKeysCommand, which OpenSSH refuses unless every parent directory is
// unwritable by group and other — the harness root lives under /tmp, mode 1777, so
// sshd logs "bad ownership or modes" and the row would pass only on machines with
// a private TMPDIR — or an in-process SSH server, which means a new dependency on
// the last change before a tag. Both were tried and rejected in that order.
//
// So producer 1 is pinned where it can be pinned honestly: the classifier arm in
// internal/hubclient (hubProbeRunner is a seam, and the nonce-echoed branch is
// mutation-red there), and the doctor severity in internal/cli. What real sshd
// contributes is the arm it genuinely decides — producer 2 — below.
//
// These DO run PinningVerdict, and they handle the ControlPersist master row 12
// avoided: the probe's mux path is registered with the harness first, so the
// harness's own reap finds and closes it before teardown. Without that
// registration the isolation directory is one the harness never computed, the reap
// misses it, and the fixture reports "sshd did not exit".
func TestSSHDPinningProbeVerdicts(t *testing.T) {
	t.Run("row 11 — a pinned host: no nonce comes back, and the probe mutates nothing", func(t *testing.T) {
		h := newSSHDHarness(t)
		rememberProbeMux(t, h, "codex")
		before := poolTree(t, h.pool)
		authBefore := h.authCount()

		verdict, state := hubclient.PinningVerdict(probeRemote(h), "codex")
		if state != hubclient.PinningPinned {
			t.Fatalf("a pinned host was reported %v: %s", state, verdict)
		}
		if after := poolTree(t, h.pool); after != before {
			t.Fatalf("the probe wrote into the pool:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		// A pinned host starts a real hub session which hits EOF at the hello read
		// and returns before touching registration, lease or inbox. One
		// authentication, and nothing else.
		if got := h.authCount() - authBefore; got > 1 {
			t.Fatalf("the probe cost %d authentications; it is meant to reuse the master", got)
		}
	})

	t.Run("row 10 — an unpinned host: the nonce comes back, and the verdict is a blocker attributed to producer 2", func(t *testing.T) {
		h := newSSHDHarness(t)
		addUnrestrictedAgent(t, h, "drifter")
		rememberProbeMux(t, h, "drifter")

		verdict, state := hubclient.PinningVerdict(probeRemote(h), "drifter")
		if state != hubclient.PinningNotPinned {
			// Explicitly not "anything but pinned": UNVERIFIED here would mean
			// the probe never ran, and this row would be reporting a fixture
			// failure as a finding about the host.
			t.Fatalf("an unpinned host was reported %v; sshd applies no forced command for that key:\n%s", state, verdict)
		}
		// Row 15 on real sshd. The throwaway key the probe mints is authorized
		// nowhere, so sshd refuses it — and that refusal is precisely what
		// separates producer 2 from producer 1. The verdict must send the
		// operator to AUTHORIZE a key, not to disable an interception that is
		// not happening.
		for _, want := range []string{
			"NOT with the key agentchute pinned",
			"fell back to another identity",
			"hub authorize --agent drifter",
		} {
			if !strings.Contains(verdict, want) {
				t.Fatalf("verdict does not attribute this to producer 2 (missing %q):\n%s", want, verdict)
			}
		}
		// The defect this whole change exists to remove: exit 127 read as a
		// missing binary. The binary is right there, and the row above proves it.
		if !strings.Contains(verdict, "The binary on the hub is fine") {
			t.Fatalf("verdict still lets exit 127 read as a missing binary:\n%s", verdict)
		}
		if strings.Contains(verdict, "tailscale") || strings.Contains(verdict, "NOT PINNED —") {
			t.Fatalf("verdict misattributes producer 2 as an intercepting layer:\n%s", verdict)
		}
	})
}

// rememberProbeMux tells the harness about the ControlPath the probe will use, so
// its existing reap closes the master. The probe multiplexes by design — it is
// meant to cost no extra authentication — and that is worth keeping.
func rememberProbeMux(t *testing.T, h *sshdHarness, agentID string) {
	t.Helper()
	rememberProbeMuxKey(t, h, agentID, h.keys[agentID])
}

// rememberProbeMuxKey is the same for a key the harness did not mint — the mux
// isolation key includes the RESOLVED key path, so a probe run with a different
// identity lands in a directory the harness never computed and its reap misses.
func rememberProbeMuxKey(t *testing.T, h *sshdHarness, agentID, keyPath string) {
	t.Helper()
	invocation, err := hubclient.BuildSSHInvocation(hubclient.SSHBuildOptions{
		Remote: probeRemote(h), AgentID: agentID, KeyPath: keyPath, StateDir: h.clientState,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.rememberMuxPath(invocation)
}

func probeRemote(h *sshdHarness) *loop.RemoteConfig {
	remote := *h.remote
	remote.HubDir = h.clientState
	return &remote
}

// Row 12 — the re-classification itself, BOTH ARMS, through the production dial.
//
// One arm alone would only trade one wrong message for another: an
// unconditional "unpinned" would fix the operator-fallback case and start
// telling operators with a genuinely missing binary to go disable Tailscale.
// So both, on the same real sshd, differing only in which cause is present.
func TestSSHDExit127IsReclassifiedByCauseNotByGuess(t *testing.T) {
	t.Run("arm A — unpinned host: E_HUB_UNPINNED", func(t *testing.T) {
		h := newSSHDHarness(t)
		addUnrestrictedAgent(t, h, "drifter")
		rememberProbeMux(t, h, "drifter")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_, _, err := hubclient.Probe(ctx, probeRemote(h), "drifter", "sshd-integration", h.keys["drifter"])
		if err == nil {
			t.Fatal("an unpinned host served a session")
		}
		if got := hubclient.ErrorCode(err); got != "E_HUB_UNPINNED" {
			t.Fatalf("exit 127 on an UNPINNED host classified as %s, want E_HUB_UNPINNED:\n%v", got, err)
		}
		// The producer-2 remedy, which row 13 cannot assert without reading the
		// host machine's own ssh config. Getting the code right while sending the
		// operator to the wrong fix would trade one wrong message for another.
		for _, want := range []string{"hub authorize --agent drifter", "The binary on the hub is fine"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the producer-2 message is missing %q:\n%v", want, err)
			}
		}
		if strings.Contains(err.Error(), "tailscale") {
			t.Fatalf("producer 2 was blamed on an intercepting layer:\n%v", err)
		}
	})

	// The control. Same harness, same sentinel, same exit 127 — but the forced
	// command IS applied and the binary it names is gone. Without this arm the
	// change could report "unpinned" for every 127 and still pass arm A.
	t.Run("arm B — pinned host, binary renamed away: still E_HUB_NO_BINARY", func(t *testing.T) {
		h := newSSHDHarness(t)
		rememberProbeMux(t, h, "codex")
		moved := h.binary + ".moved-away"
		if err := os.Rename(h.binary, moved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Rename(moved, h.binary) })
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_, _, err := hubclient.Probe(ctx, probeRemote(h), "codex", "sshd-integration", h.keys["codex"])
		if err == nil {
			t.Fatal("the hub served a session with its binary renamed away")
		}
		if got := hubclient.ErrorCode(err); got != "E_HUB_NO_BINARY" {
			t.Fatalf("a genuinely missing binary on a PINNED host classified as %s, want E_HUB_NO_BINARY:\n%v", got, err)
		}
		if strings.Contains(err.Error(), "tailscale") {
			t.Fatalf("a missing binary sent the operator to disable an interception that is not happening:\n%v", err)
		}
	})
}
