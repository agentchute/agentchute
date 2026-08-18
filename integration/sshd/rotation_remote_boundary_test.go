//go:build sshd_integration

package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A2 / DESIGN §10.3 "staged rotation crash recovery" — the REMOTE boundaries.
//
// Why these are sshd rows and the first three A2 rows are not, which is the
// whole finding behind this file:
//
// The remaining rotation transitions ("crashed after minting the staged key,
// before replacing it on the hub" and "crashed after replacing it on the hub,
// before promoting it locally") produce the SAME local key directory. Both leave
// v1 active, v2 staged, no `.tmp` — `prepareHubKey` reads only the keys dir and
// the active symlink, so it cannot distinguish them, and neither can any
// filesystem-local test. What differs is entirely on the hub: whether
// authorized_keys still carries v1 or already carries v2.
//
// So a unit row here would report coverage it does not have. These need real
// sshd, and the payoff is the assertion a unit row could never make: after
// recovery the lane actually CONNECTS with the key it ended up on.
//
// A third case a reader will look for and not find — authorized_keys carrying
// BOTH v1 and v2 — is deliberately absent. `hub authorize` rewrites the file
// through a temp-and-rename under a lock (`writeHubAuthorizedKeys`), and
// `replaceHubMarkerLines` collapses every marker line for the agent into one, so
// the replace has no intermediate on-hub state at all. A fixture for it could be
// constructed by hand, and it would stand for no real crash.
func TestSSHDRotationConvergesFromEitherRemoteBoundary(t *testing.T) {
	cases := []struct {
		name string
		// authorizeStaged reflects whether the crash happened after the hub-side
		// replace landed. It is the ONLY difference between the two fixtures.
		authorizeStaged bool
		why             string
	}{
		{
			name:            "crashed_before_the_hub_replace",
			authorizeStaged: false,
			why:             "the hub still authorizes v1; recovery must authorize the staged key and promote it",
		},
		{
			name:            "crashed_after_the_hub_replace",
			authorizeStaged: true,
			why:             "the hub already authorizes v2; recovery must promote it rather than mint a third key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh hub per case: authorized_keys is harness-global, so sharing one
			// would let case 1 observe case 2's authorization.
			h := newSSHDHarness(t)
			checkout, agentID := joinNamedCodex(t, h)

			remote := parseRemoteForHome(t, h)
			keysDir := filepath.Join(remote.HubDir, "keys")
			base := filepath.Join(keysDir, agentID+"_ed25519")
			v1Pub := activePubKey(t, h, agentID)

			// Reconstruct the post-crash state: a staged v2 minted beside the active
			// v1, exactly as mintStagedHubKey leaves it. Real ssh-keygen, because the
			// recovery path probes the key and a stub would remove the thing
			// under test.
			stagedPriv := base + ".v2"
			runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "agentchute:"+agentID, "-f", stagedPriv)
			stagedPub := pubKeyBody(t, stagedPriv+".pub")
			if stagedPub == v1Pub {
				t.Fatal("staged key is identical to the active one; the fixture proves nothing")
			}
			// The active pointer must still name v1 — that is what makes this a
			// half-finished rotation rather than a completed one.
			if got := activePubKey(t, h, agentID); got != v1Pub {
				t.Fatalf("fixture moved the active key before recovery ran: %s", got)
			}

			if tc.authorizeStaged {
				stdout, stderr, err := h.runCLI(checkout, "hub", "authorize",
					"--agent", agentID, "--pool", h.pool,
					"--key", strings.TrimSpace(readFileString(t, stagedPriv+".pub")),
					"--replace-key")
				if err != nil {
					t.Fatalf("stage the hub-side replace: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
				}
				if !authorizedContains(t, h, stagedPub) {
					t.Fatal("fixture did not actually authorize the staged key")
				}
			} else if !authorizedContains(t, h, v1Pub) {
				t.Fatal("fixture lost the v1 authorization; the hub must still be on v1 here")
			}

			// Masters opened by the join hold the PRE-rotation key. Rotation retires
			// that key out of the `.vN` naming the harness scans, so a surviving
			// master becomes undiscoverable and outlives the test.
			h.stopMuxMasters()

			// The recovery IS a plain re-run. Not --rotate-key: the operator does not
			// know a rotation was in flight, they just run the command again.
			stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex")
			if err != nil {
				t.Fatalf("recovery re-run (%s): %v\nstdout:\n%s\nstderr:\n%s", tc.why, err, stdout, stderr)
			}

			// Convergence, identical for both fixtures.
			active := activePubKey(t, h, agentID)
			if active != stagedPub {
				t.Fatalf("recovery did not converge on the staged key (%s)\n staged=%s\n active=%s\n v1=%s", tc.why, stagedPub, active, v1Pub)
			}
			if got := countVersionFiles(t, keysDir, agentID); got != 1 {
				t.Fatalf("recovery left %d key versions, want 1 — a stranded version is a credential nobody prunes", got)
			}
			if got := authorizedLinesFor(t, h, agentID); got != 1 {
				t.Fatalf("recovery left %d authorized_keys lines, want 1", got)
			}
			if !authorizedContains(t, h, stagedPub) {
				t.Fatal("the hub does not authorize the key the lane ended up on")
			}
			if authorizedContains(t, h, v1Pub) {
				t.Fatal("the retired v1 is still authorized on the hub")
			}

			// The assertion a filesystem-local row could not make: the lane actually
			// connects with what it converged on. Everything above is consistent
			// bookkeeping; this is the part that fails if the bookkeeping is wrong.
			if stdout, stderr, err := h.runCLI(checkout, "doctor"); err != nil {
				t.Fatalf("lane cannot use the converged key: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
			}
		})
	}
}

func pubKeyBody(t *testing.T, path string) string {
	t.Helper()
	fields := strings.Fields(readFileString(t, path))
	if len(fields) < 2 {
		t.Fatalf("malformed public key %s", path)
	}
	return fields[1]
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// countVersionFiles counts live `.vN` private keys. Retired versions are renamed
// out of this naming, so they correctly do not count.
func countVersionFiles(t *testing.T, keysDir, agentID string) int {
	t.Helper()
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		t.Fatalf("read keys dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, agentID+"_ed25519.v") || strings.Contains(name, ".pub") || strings.Contains(name, ".invalid.") {
			continue
		}
		n++
	}
	return n
}
