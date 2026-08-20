package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

// Issue #164. `hub join` read the host-key fingerprint from
// <hubdir>/known_hosts, discarded the error if that failed, and recorded a
// config with no fingerprint at all. Nothing ever re-recorded it, and
// findHubMigrationCandidate skips a candidate whose fingerprint is empty — so
// that hub could never be migrated. The operator instead got a duplicate-key
// refusal telling them to pass --replace-key, which is the wrong remedy: the
// machine had not been replaced.
//
// The asymmetry is the defect. Migration already has an ssh-keyscan fallback
// (hubJoinDiscoverFingerprint) for exactly this, so the path that RECORDS the
// value was less robust than the path that CONSUMES it.

func TestHubJoinFallsBackToKeyscanWhenKnownHostsYieldsNothing(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	withCwd(t, root, func() {
		hubJoinFingerprint = func(*loop.RemoteConfig) (string, error) {
			return "", errors.New("known_hosts unreadable")
		}
		scans := 0
		hubJoinDiscoverFingerprint = func(*loop.RemoteConfig) (string, error) {
			scans++
			return "SHA256:scanned", nil
		}
		hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
			return successfulHubHello("codex-tiny"), nil, nil
		}
		if err := cmdHubJoin([]string{remote.URL, "--name", "codex"}); err != nil {
			t.Fatal(err)
		}
		if scans != 1 {
			t.Fatalf("keyscan fallback ran %d time(s), want 1", scans)
		}
		cfg, err := hubclient.ReadHubConfig(remote.HubID)
		if err != nil {
			t.Fatal(err)
		}
		// The whole point: a value gets recorded, and it is the one the fallback
		// found. Recording "" here is what made the hub unmigrateable.
		if cfg.HostKeyFingerprint != "SHA256:scanned" {
			t.Fatalf("recorded fingerprint = %q, want the scanned one", cfg.HostKeyFingerprint)
		}
	})
}

// The control, and it is load-bearing. A fix that simply always keyscans would
// pass the row above while adding a network probe to every healthy join — and
// ssh-keyscan is a second connection to a host the join has already reached.
func TestHubJoinDoesNotKeyscanWhenKnownHostsAnswers(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	withCwd(t, root, func() {
		hubJoinFingerprint = func(*loop.RemoteConfig) (string, error) {
			return "SHA256:fromKnownHosts", nil
		}
		scans := 0
		hubJoinDiscoverFingerprint = func(*loop.RemoteConfig) (string, error) {
			scans++
			return "SHA256:scanned", nil
		}
		hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
			return successfulHubHello("codex-tiny"), nil, nil
		}
		if err := cmdHubJoin([]string{remote.URL, "--name", "codex"}); err != nil {
			t.Fatal(err)
		}
		if scans != 0 {
			t.Fatalf("keyscan ran %d time(s) on a join whose known_hosts answered; the fallback is for when it does not", scans)
		}
		cfg, err := hubclient.ReadHubConfig(remote.HubID)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.HostKeyFingerprint != "SHA256:fromKnownHosts" {
			t.Fatalf("recorded fingerprint = %q, want the known_hosts one", cfg.HostKeyFingerprint)
		}
	})
}

// When BOTH sources fail the join still completes — it has already authenticated
// and written the key, and refusing here would strand a working machine over a
// diagnostic value. But it must stop being SILENT, which is the second half of
// the issue: the error at the read site was discarded outright.
func TestHubJoinSaysSoWhenNoFingerprintCanBeRecorded(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	var joinErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			hubJoinFingerprint = func(*loop.RemoteConfig) (string, error) {
				return "", errors.New("known_hosts unreadable")
			}
			hubJoinDiscoverFingerprint = func(*loop.RemoteConfig) (string, error) {
				return "", errors.New("keyscan could not reach the host")
			}
			hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
				return successfulHubHello("codex-tiny"), nil, nil
			}
			joinErr = cmdHubJoin([]string{remote.URL, "--name", "codex"})
		})
	})

	if joinErr != nil {
		t.Fatalf("the join failed over a missing fingerprint: %v", joinErr)
	}
	// Both errors, because they are different problems with different fixes —
	// an unreadable known_hosts is local, an unreachable keyscan is not.
	for _, want := range []string{"known_hosts unreadable", "keyscan could not reach the host"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the discarded error is still discarded (missing %q):\n%s", want, stderr)
		}
	}
	// The consequence, in the operator's terms. Without it this is a warning
	// about an internal field nobody has a reason to care about — and the
	// symptom arrives much later, as a duplicate-key refusal recommending
	// --replace-key, which is the wrong remedy.
	if !strings.Contains(strings.ToLower(stderr), "migrat") {
		t.Fatalf("the warning does not say what breaks later:\n%s", stderr)
	}
	if !strings.Contains(stderr, "hub join") {
		t.Fatalf("the warning does not say how to recover:\n%s", stderr)
	}
	// And it must not be mistaken for a failed join.
	if strings.Contains(stderr, "--replace-key") {
		t.Fatalf("the warning offers the remedy this issue exists to stop recommending:\n%s", stderr)
	}

	cfg, err := hubclient.ReadHubConfig(remote.HubID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostKeyFingerprint != "" {
		t.Fatalf("recorded %q with no source able to supply one", cfg.HostKeyFingerprint)
	}
	// The join itself has to be complete, or the warning is describing the wrong
	// failure: the machine is joined, it just cannot be migrated yet.
	if !containsString(cfg.JoinedAs, "codex-tiny") {
		t.Fatalf("join did not complete: %+v", cfg)
	}
}
