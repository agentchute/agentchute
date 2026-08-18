package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

func TestHubErrorCatalogGoldenTexts(t *testing.T) {
	t.Setenv("HOME", "/home/alex")
	versionText := "hub: hub speaks agentchute-hub v1; this CLI needs v2. The hub always upgrades first — run `agentchute " + "update` on the hub, then retry. This machine is not misconfigured and re-running `agentchute hub join` will NOT fix it: wait for the hub upgrade."
	usageText := strings.TrimPrefix(hubJoinUsage(errors.New("missing URL")).Error(), "missing URL\n")
	stale := loop.RecipientReachability{
		LastSeen:  time.Date(2026, 8, 17, 11, 59, 0, 0, time.UTC),
		Age:       time.Minute,
		Threshold: 30 * time.Second,
	}
	rows := []struct {
		name string
		got  string
		want string
	}{
		{"E_VERSION", hubVersionError(1, 2).Error(), versionText},
		{"E_IDENTITY", hubIdentityError("codex", "grok").Error(), "hub: this key is authorized as \"codex\" but you are acting as \"grok\". Fix --as/AGENTCHUTE_AGENT_ID, or join this machine as grok: agentchute hub join <url> --as grok"},
		{"E_POOL_MISMATCH hub", hubPoolMismatchError("/home/alex/code/agentchute", "9c4e12ab77f0", "41d2c8ab0917", "codex").Error(), "hub: this key is authorized for pool id 9c4e12ab77f0, but /home/alex/code/agentchute on the hub reports pool id 41d2c8ab0917 (or has no state/pool.id at all). The authorized_keys line's --pool was edited without its --pool-id, or the pool directory was replaced. On the hub, re-run: agentchute hub authorize --agent codex --replace-key --pool <the pool this key should serve> --key \"<key>\"."},
		{"E_POOL_MISMATCH client", hubClientPoolMismatchMessage("/home/alex/other-pool", "41d2…", "9c4e12ab77f0", "/home/alex/code/agentchute", "codex"), "hub: this key now serves pool /home/alex/other-pool (id 41d2…) on the hub, but this machine joined pool id 9c4e12ab77f0 (/home/alex/code/agentchute). The key line was re-pointed or the hub moved the pool. Re-join if the move is intended (agentchute hub join <url> --as codex), or re-authorize the key with the right --pool on the hub."},
		{"E_POOL_NOT_FOUND", hubPoolNotFoundError("/home/alex/code/agentchute", "codex").Error(), "hub: the authorized pool path /home/alex/code/agentchute no longer resolves on the hub. The hub operator should re-run hub authorize from the pool's current location (agentchute hub authorize --agent codex --replace-key --pool <new-path> --key \"<key>\")."},
		{"E_CHANNEL_LOST", hubChannelLostMessage([]string{"agentchute", "serve", "codex", "--relaunch=false"}), "hub: channel to the hub was lost; the wrapper was stopped (fenced). Relaunch with: agentchute serve codex --relaunch=false. (This lane was started with --relaunch=false; the default relaunches automatically, §6.7.)"},
		{"E_ORDER", hubOrderError().Error(), "hub: protocol order violation (tick before register on this channel). This is a client bug, not an operator problem — please report it."},
		{"E_HUB_IO", hubIOError(errors.New("no space left on device")).Error(), "hub: the hub could not write pool state: no space left on device. This is a hub-side problem; nothing was partially delivered unless the message text says otherwise."},
		{"E_FENCED", hubFencedError("codex").Error(), "serve: this lane was fenced out (lease reclaimed — likely a newer serve for codex, or a hub update). Restart this lane with its wrapper: ac --as codex serve <wrapper>"},
		{"duplicate authorize", duplicateHubAgentError("codex-tiny", "SHA256:Yk3n…", "2026-08-01").Error(), "hub authorize: \"codex-tiny\" already has an authorized key (SHA256:Yk3n…, added 2026-08-01). One key = one agent id. If this machine REPLACES the old one, re-run with --replace-key. If both machines should run, join the new one under its own id — ids are cheap, and a shared id would collide on the serve lease anyway. (Auto-derived names collide when two machines share a hostname; pick an explicit id on one of them: agentchute hub join <url> --as codex-tiny2.)"},
		{"unsafe pool path", unsafeHubAuthorizePath("pool", "/home/al ex/pool").Error(), "hub authorize: pool path contains characters outside the safe set [A-Za-z0-9._/+-] (spaces, quotes, and shell metacharacters are refused rather than escaped): \"/home/al ex/pool\". Move or symlink the pool to a plain path and re-run."},
		{"invalid pool identity", invalidHubAuthorizePoolID("/home/alex/code/agentchute/.agentchute/loop/state/pool.id").Error(), "hub authorize: /home/alex/code/agentchute/.agentchute/loop/state/pool.id is not a valid pool identity (must be a regular 0600 file containing exactly 12 lowercase hex characters). Nothing was written to authorized_keys. Inspect the file; if it is corrupt, delete it and re-run authorize to mint a fresh identity (existing key lines for this pool will then need re-authorizing)."},
		{"turn-end connect", hubTurnEndConnectError().Error(), "turn-end: could not reach the hub to commit claimed mail (connect failed after 5s). Nothing is lost: the claim is held on the hub and the guard latch stays armed; turn-end retries at the next turn boundary. If this persists, check the network and run agentchute doctor."},
		{"non-wrapper name", hubJoinNameError("work").Error(), "hub join: --name work does not name a wrapper this machine can launch (known: claude, codex, gemini, grok). --name is the LOCAL name you launch the lane with (ac serve <name>), so it must be a wrapper token — a lane named \"work\" would have no launch form at all. For an arbitrary pool id, use --as instead and launch with an explicit wrapper: agentchute hub join <url> --as work-tiny, then ac --as work-tiny serve claude."},
		{"join busy", hubLockBusyError("/home/alex/.agentchute/hub/.locks/3fa8c21b90de.lock", hubLockExclusive).Error(), "hub join: this hub is busy (lock ~/.agentchute/hub/.locks/3fa8c21b90de.lock). Either another agentchute hub join/rotate is running, or a `serve` lane is live against it — a lane holds this lock for as long as it runs, and migrating underneath it would delete state it is still writing. Stop the lane (or wait for the other join), then re-run."},
		{"serve blocked by migration", hubLockBusyError("/home/alex/.agentchute/hub/.locks/3fa8c21b90de.lock", hubLockShared).Error(), "serve: this hub is being migrated right now (lock ~/.agentchute/hub/.locks/3fa8c21b90de.lock held exclusively). The migration moves the directory this lane writes into, so serve will not start until it finishes; re-run in a moment."},
		{"invalid key version", hubJoinKeyVersionError("codex-tiny_ed25519.vold").Error(), "hub join: keys/codex-tiny_ed25519.vold is not a valid key version (expected .v<N>, N a positive decimal integer). Move it out of the keys directory and re-run."},
		{"bare join usage", usageText, "usage: agentchute hub join ssh://[user@]host[:port]/abs/path/to/pool (--name <local-name> | --as <agent-id>)\n  --name mints the pool id <local-name>-<hostname> (e.g. --name codex on host tiny -> codex-tiny) and must be a known wrapper token; --as uses your id verbatim.\n  The path is the pool's absolute path ON THE HUB (run `pwd` there)."},
		{"not registered sender", hubSenderNotRegisteredError("codex").Error(), "sender \"codex\" is not registered. Run `agentchute boot --as codex --vendor <vendor>` first (AGENTCHUTE.md §5.3)"},
		{"not registered agent", hubAgentNotRegisteredError("codex").Error(), "agent \"codex\" is not registered. Run `agentchute boot --as codex --vendor <vendor>` first (AGENTCHUTE.md §5.3)"},
		{"recipient unknown", unknownRecipientError("grok", errors.New("unknown")).Error(), "unknown agent \"grok\": no registration row. Check the id (agentchute status) — do NOT register on their behalf."},
		{"recipient stale", staleRecipientError("grok", stale).Error(), "\"grok\" was here, gone since 2026-08-17T11:59:00Z (1m0s ago); not sending (row older than stale_after=30s). They re-register at boot."},
		{"recipient racing", racingRecipientError("grok").Error(), "\"grok\" was here seconds ago — likely mid-restart; retry once."},
		{"recipient unreadable", unreadableRecipientError("grok").Error(), "\"grok\"'s registration could not be read (malformed); not sending. Inspect agents/grok.md by hand."},
		{"local name warning", hubLocalNameWarning("codex", "codex-tiny"), "warning: AGENTCHUTE_AGENT_ID=codex is a local name on this machine; every command resolves it to \"codex-tiny\" (this hub's names map). Unset it, or export the full id, if that is not what you want."},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.got != row.want {
				t.Fatalf("message:\n%q\nwant:\n%q", row.got, row.want)
			}
		})
	}
}

func TestHubLeaseHeldCatalogFreshAndStale(t *testing.T) {
	now := time.Date(2026, 8, 17, 21, 0, 0, 0, time.UTC)
	cfg := &loop.Config{ControlRepo: "/home/alex/code/agentchute", LoopDir: filepath.Join(t.TempDir(), ".agentchute", "loop")}
	writeCatalogClaim(t, cfg, loop.ServeClaim{ID: "codex", PID: 48122, LastSeen: now.Add(-2 * time.Second)})
	wantFresh := "runner for codex is already active (serve lease held by hub pid 48122, fresh 2s ago). A second machine serving the same id must pick a distinct --as; if a connection just dropped, this clears within ~20s."
	if got := hubLeaseHeldError(cfg, "codex", now).Error(); got != wantFresh {
		t.Fatalf("fresh = %q, want %q", got, wantFresh)
	}
	writeCatalogClaim(t, cfg, loop.ServeClaim{ID: "codex", PID: 48122, LastSeen: now.Add(-3 * time.Hour)})
	wantPath := filepath.Join(cfg.LoopDir, "state", "codex", "serve.claim")
	wantStale := "runner for codex looks DEAD (lease stale 3h) but hub pid 48122 still reads alive on the same boot as this claim — either that process is frozen but real, or this is OS pid reuse within one boot. On the hub: inspect `ps -p 48122`; if it is unrelated, delete " + wantPath + " and relaunch."
	if got := hubLeaseHeldError(cfg, "codex", now).Error(); got != wantStale {
		t.Fatalf("stale = %q, want %q", got, wantStale)
	}
}

func writeCatalogClaim(t *testing.T, cfg *loop.Config, claim loop.ServeClaim) {
	t.Helper()
	path := filepath.Join(cfg.AgentStateDir(claim.ID), "serve.claim")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorHubAuthorizedKeysUsesAuthorizeAudit(t *testing.T) {
	_, pool, executable, key := setupHubAuthorizeTest(t)
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if check := checkHubAuthorizedKeysAudit(); check.Severity != severityOK || !strings.Contains(check.Message, "PASS agentchute:codex:") {
		t.Fatalf("healthy audit = %#v", check)
	}
	if err := os.Chmod(executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if check := checkHubAuthorizedKeysAudit(); check.Severity != severityBlocker || !strings.Contains(check.Message, "binary does not exist or is not executable") {
		t.Fatalf("broken audit = %#v", check)
	}
}

func TestDoctorWarnsWhenControlRepoEnvOverridesPointer(t *testing.T) {
	root := t.TempDir()
	pointerURL := "ssh://alex@hub.example/home/alex/code/agentchute"
	if err := os.WriteFile(filepath.Join(root, loop.PointerFileName), []byte(pointerURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "/tmp/other-pool")
	got := doctorControlRepoEnvWarning(root)
	if !strings.Contains(got, "overrides the pointer") || !strings.Contains(got, pointerURL) || !strings.Contains(got, "Unset it") {
		t.Fatalf("warning = %q", got)
	}
}

func TestHubJoinWarnsWhenAgentEnvNamesMappedLocalName(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	hubJoinProbe = func(_ *loop.RemoteConfig, agentID, _ string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello(agentID), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_AGENT_ID", "codex")
	stderr := captureStderr(t, func() {
		if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, hubLocalNameWarning("codex", "codex-tiny")) {
		t.Fatalf("stderr = %q", stderr)
	}
}
