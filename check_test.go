package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// AGENTCHUTE.md §6.3 step 8 + §10.5: the reference CLI's `check` runs
// cooperative waking after own-inbox processing. With a stale-unread
// peer reachable from this host, the cycle attempts a poke and the
// outcome lands in the watchdog log (poke-attempted or poke-failed —
// either way the cycle ran).
func TestCmdCheckRunsCooperativeWaking(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".rehumanlabs", "loop"))

		// Register self (no TMUX_PANE = non-pokable, but the cooperation
		// cycle for the self-agent skips self anyway).
		if err := cmdRegister([]string{"--as", "checker", "--vendor", "test", "--wake-target", ""}); err != nil {
			t.Fatal(err)
		}

		// Drop in a peer registration directly. Stale last_seen + unread inbox
		// triggers the cooperative poke. Empty host = treated as same-host.
		cfg := &loop.Config{
			ControlRepo: root,
			LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
			Vendor:      "rehumanlabs",
		}
		stalePeerPath := cfg.AgentRegistrationPath("stale-peer")
		mustWrite(t, stalePeerPath, []byte(`---
agent_id: stale-peer
vendor: test
control_repo: `+root+`
wake_method: tmux
wake_target: "%9"
last_seen: 2020-01-01T00:00:00Z
status: active
---
`))
		mustMkdir(t, cfg.AgentInboxDir("stale-peer"))
		mustWrite(t, filepath.Join(cfg.AgentInboxDir("stale-peer"), "2020-01-01T00-00-00-000000Z_from-checker_msg-aaaa.md"), []byte(`---
message_id: 2020-01-01T00:00:00.000000Z
from: checker
to: stale-peer
---

stale
`))

		// Run check. Should process own (empty) inbox, then cooperate.
		if err := cmdCheck([]string{"--as", "checker"}); err != nil {
			t.Fatalf("cmdCheck: %v", err)
		}

		// Cooperation should have written to the watchdog log — either a
		// "poked stale-peer" or a "poke stale-peer failed" line (tmux send-keys
		// fails in the test env). Both confirm the cycle ran.
		logBytes, err := os.ReadFile(cfg.WatchdogLogPath())
		if err != nil {
			t.Fatalf("cooperation did not write watchdog log: %v", err)
		}
		logText := string(logBytes)
		if !strings.Contains(logText, "stale-peer") {
			t.Fatalf("watchdog log missing stale-peer cooperation entry:\n%s", logText)
		}
	})
}

// --no-archive must suppress cooperative waking alongside the other
// inbox-related side effects so dry-runs do not move, quarantine, or poke
// anything. Own last_seen still updates — that's pre-existing intentional
// behavior.
func TestCmdCheckNoArchiveSuppressesCooperation(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".rehumanlabs", "loop"))
		if err := cmdRegister([]string{"--as", "checker", "--vendor", "test", "--wake-target", ""}); err != nil {
			t.Fatal(err)
		}

		cfg := &loop.Config{
			ControlRepo: root,
			LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
			Vendor:      "rehumanlabs",
		}
		stalePeerPath := cfg.AgentRegistrationPath("stale-peer")
		mustWrite(t, stalePeerPath, []byte(`---
agent_id: stale-peer
vendor: test
control_repo: `+root+`
wake_method: tmux
wake_target: "%9"
last_seen: 2020-01-01T00:00:00Z
status: active
---
`))
		mustMkdir(t, cfg.AgentInboxDir("stale-peer"))
		mustWrite(t, filepath.Join(cfg.AgentInboxDir("stale-peer"), "2020-01-01T00-00-00-000000Z_from-checker_msg-aaaa.md"), []byte(`---
message_id: 2020-01-01T00:00:00.000000Z
from: checker
to: stale-peer
---

stale
`))

		if err := cmdCheck([]string{"--as", "checker", "--no-archive"}); err != nil {
			t.Fatalf("cmdCheck --no-archive: %v", err)
		}

		// Watchdog log SHOULD NOT exist (cooperation ran would have created it).
		if _, err := os.Stat(cfg.WatchdogLogPath()); err == nil {
			t.Fatal("--no-archive should have suppressed cooperation, but watchdog log was written")
		} else if !os.IsNotExist(err) {
			t.Fatalf("unexpected error checking watchdog log: %v", err)
		}
	})
}
