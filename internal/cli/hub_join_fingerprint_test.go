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
// config with no fingerprint at all — and findHubMigrationCandidate skipped a
// candidate whose fingerprint was empty, so that hub could never be migrated.
// The first fix added an ssh-keyscan fallback to the recording path.
//
// Review 2026-10-08 S11 removed ssh-keyscan from agentchute entirely: a keyscan
// shows a key without proving anyone holds it, so it can neither record nor
// prove a hub. What #164 protected is still protected, structurally: a migration
// now proves "same hub" with a probe pinned to the old hub's known_hosts, which
// does not need a recorded fingerprint at all (hub_hardening_test.go,
// TestHubMigrationRequiresTheNewHostToProveTheOldHostKey). These rows pin the
// recording side: known_hosts is the only source, and its failure is loud.

func TestHubJoinRecordsTheFingerprintFromKnownHosts(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	withCwd(t, root, func() {
		hubJoinFingerprint = func(*loop.RemoteConfig) (string, error) {
			return "SHA256:fromKnownHosts", nil
		}
		hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
			return successfulHubHello("codex-tiny"), nil, nil
		}
		if err := cmdHubJoin([]string{remote.URL, "--name", "codex"}); err != nil {
			t.Fatal(err)
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

// When known_hosts yields nothing the join still completes — it has already
// authenticated and written the key, and refusing here would strand a working
// machine over a diagnostic value. But it must not be SILENT (the second half of
// #164), and it must not invent a value from an unauthenticated source.
func TestHubJoinSaysSoWhenNoFingerprintCanBeRecorded(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	var joinErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			hubJoinFingerprint = func(*loop.RemoteConfig) (string, error) {
				return "", errors.New("known_hosts unreadable")
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
	if !strings.Contains(stderr, "known_hosts unreadable") {
		t.Fatalf("the read error is discarded:\n%s", stderr)
	}
	if strings.Contains(stderr, "keyscan") {
		t.Fatalf("the warning still describes a keyscan fallback that no longer exists:\n%s", stderr)
	}
	// The consequence, in the operator's terms, and how to recover.
	if !strings.Contains(strings.ToLower(stderr), "migrat") {
		t.Fatalf("the warning does not say what breaks later:\n%s", stderr)
	}
	if !strings.Contains(stderr, "hub join") {
		t.Fatalf("the warning does not say how to recover:\n%s", stderr)
	}
	if strings.Contains(stderr, "--replace-key") {
		t.Fatalf("the warning offers the remedy #164 exists to stop recommending:\n%s", stderr)
	}

	cfg, err := hubclient.ReadHubConfig(remote.HubID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostKeyFingerprint != "" {
		t.Fatalf("recorded %q with no source able to supply one", cfg.HostKeyFingerprint)
	}
	if !containsString(cfg.JoinedAs, "codex-tiny") {
		t.Fatalf("join did not complete: %+v", cfg)
	}
}
