package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

type wi57Harness struct {
	pool   string
	cfg    *loop.Config
	remote *loop.RemoteConfig
	root   string
	agent  string
	now    time.Time
	regs   []hubwire.Register
}

type wi57RecordingConn struct {
	net.Conn
	pending    []byte
	onRegister func(hubwire.Register)
}

func (c *wi57RecordingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.pending = append(c.pending, p[:n]...)
	for {
		newline := bytes.IndexByte(c.pending, '\n')
		if newline < 0 {
			break
		}
		line := c.pending[:newline]
		c.pending = c.pending[newline+1:]
		var envelope struct {
			Type string `json:"t"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.Type != "register" {
			continue
		}
		var req hubwire.Register
		if json.Unmarshal(line, &req) == nil {
			c.onRegister(req)
		}
	}
	return n, err
}

// newWI57Harness routes production OneShot clients through the production hub
// dispatcher over net.Pipe. These package-level seams require serial tests;
// do not call t.Parallel in this file.
func newWI57Harness(t *testing.T, agent, vendor string) *wi57Harness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	clearGuardEnv(t)
	pool, cfg := newHubPool(t)
	remote, err := loop.ParseRemoteURL("ssh://hub.example/remote/pool")
	if err != nil {
		t.Fatal(err)
	}
	resolvedPool, err := filepath.EvalSymlinks(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := hubclient.WriteHubConfig(remote.HubID, &hubclient.HubConfig{
		URL: remote.URL, JoinedAs: []string{agent}, Pool: resolvedPool, Pool12: fixturePoolID,
	}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, loop.PointerFileName), []byte(remote.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 21, 0, 0, 0, time.UTC)
	writeWI57Registration(t, cfg, agent, vendor, "fixture-host", now)

	originalOpen := openRemoteOneShot
	originalSelfCheckNow := selfCheckNow
	selfCheckNow = func() time.Time { return now }
	h := &wi57Harness{pool: pool, cfg: cfg, remote: remote, root: root, agent: agent, now: now}
	openRemoteOneShot = func(discovered *loop.Config, actor string) (*hubclient.OneShot, error) {
		client, server := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- serveHubSession(ctx, server, hubSessionOptions{
				Agent: actor, Pool: pool, PoolID: fixturePoolID, HubBin: "test",
				now: func() time.Time { return now },
			})
		}()
		recordingClient := &wi57RecordingConn{Conn: client, onRegister: func(req hubwire.Register) {
			h.regs = append(h.regs, req)
		}}
		session, openErr := hubclient.OpenOneShotTransport(recordingClient, discovered.Remote, actor, "test-client")
		if openErr != nil {
			cancel()
			_ = client.Close()
			return nil, openErr
		}
		t.Cleanup(func() {
			cancel()
			_ = client.Close()
			select {
			case serveErr := <-done:
				if serveErr != nil {
					t.Errorf("hub session: %v", serveErr)
				}
			case <-time.After(time.Second):
				t.Error("hub session did not exit")
			}
		})
		return session, nil
	}
	t.Cleanup(func() {
		openRemoteOneShot = originalOpen
		selfCheckNow = originalSelfCheckNow
	})
	return h
}

func writeWI57Registration(t *testing.T, cfg *loop.Config, agent, vendor, host string, lastSeen time.Time) {
	t.Helper()
	reg := &loop.Registration{
		AgentID: agent, ProtocolVersion: loop.CurrentProtocolVersion, Vendor: vendor,
		ControlRepo: cfg.ControlRepo, Host: host, LastSeen: lastSeen,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agent), reg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentInboxDir(agent), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (h *wi57Harness) capture(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	withCwd(t, h.root, func() {
		stdout, stderr, err = captureStdoutStderr(t, fn)
	})
	return stdout, stderr, err
}

func lineWithPrefix(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line + "\n"
		}
	}
	return ""
}

func (h *wi57Harness) nextRegister(t *testing.T, before int) hubwire.Register {
	t.Helper()
	if got := len(h.regs) - before; got != 1 {
		t.Fatalf("new register wire frames = %d, want 1", got)
	}
	return h.regs[before]
}

func TestWI57RemoteVendorResolutionRenderingAndSweepSemantics(t *testing.T) {
	h := newWI57Harness(t, "work-tiny", "resolved-vendor")
	staleHub := "z-stale-hub"
	writeWI57Registration(t, h.cfg, staleHub, "test", "stale", h.now.Add(-2*loop.DefaultStaleAfter))
	shadowCfg := &loop.Config{ControlRepo: h.root, LoopDir: h.remote.ShadowLoopDir}
	writeWI57Registration(t, shadowCfg, "z-stale-shadow", "test", "stale", h.now.Add(-2*loop.DefaultStaleAfter))

	before := len(h.regs)
	registerOut, _, err := h.capture(t, func() error {
		return cmdRegister([]string{"--as", h.agent, "--control-repo", h.remote.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := lineWithPrefix(registerOut, "  vendor:"); got != "  vendor:        resolved-vendor\n" {
		t.Fatalf("register vendor line = %q", got)
	}
	if req := h.nextRegister(t, before); req.Vendor != nil || req.Sweep {
		t.Fatalf("bare register wire request = %#v, want Vendor:nil Sweep:false", req)
	}
	if _, err := os.Stat(h.cfg.AgentRegistrationPath(staleHub)); err != nil {
		t.Fatalf("register swept a hub peer: %v", err)
	}
	reg, err := loop.ReadRegistration(h.cfg.AgentRegistrationPath(h.agent))
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if reg.Vendor != "resolved-vendor" || reg.Host != host {
		t.Fatalf("hub registration vendor/host = %q/%q, want resolved-vendor/%q", reg.Vendor, reg.Host, host)
	}

	before = len(h.regs)
	remoteBoot, _, err := h.capture(t, func() error {
		return cmdBoot([]string{"--as", h.agent, "--control-repo", h.remote.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	localBoot, _, err := h.capture(t, func() error {
		return cmdBoot([]string{"--as", h.agent, "--control-repo", h.pool})
	})
	if err != nil || remoteBoot != localBoot {
		t.Fatalf("boot remote/local = %q/%q, err=%v", remoteBoot, localBoot, err)
	}
	if req := h.nextRegister(t, before); req.Vendor != nil || !req.Sweep {
		t.Fatalf("bare boot wire request = %#v, want Vendor:nil Sweep:true", req)
	}
	if _, err := os.Stat(h.cfg.AgentRegistrationPath(staleHub)); !os.IsNotExist(err) {
		t.Fatalf("remote boot did not sweep the hub peer: %v", err)
	}
	if _, err := os.Stat(shadowCfg.AgentRegistrationPath("z-stale-shadow")); err != nil {
		t.Fatalf("remote boot swept the local shadow: %v", err)
	}

	before = len(h.regs)
	remoteBootJSON, _, err := h.capture(t, func() error {
		return cmdBoot([]string{"--as", h.agent, "--control-repo", h.remote.URL, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	localBootJSON, _, err := h.capture(t, func() error {
		return cmdBoot([]string{"--as", h.agent, "--control-repo", h.pool, "--json"})
	})
	if err != nil || remoteBootJSON != localBootJSON {
		t.Fatalf("boot JSON remote/local = %q/%q, err=%v", remoteBootJSON, localBootJSON, err)
	}
	if req := h.nextRegister(t, before); req.Vendor != nil || !req.Sweep {
		t.Fatalf("bare boot JSON wire request = %#v, want Vendor:nil Sweep:true", req)
	}

	writeWI57Registration(t, h.cfg, staleHub, "test", "stale", h.now.Add(-2*loop.DefaultStaleAfter))
	before = len(h.regs)
	remoteSelf, _, err := h.capture(t, func() error {
		return cmdSelfCheck([]string{"--as", h.agent, "--control-repo", h.remote.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	localSelf, _, err := h.capture(t, func() error {
		return cmdSelfCheck([]string{"--as", h.agent, "--control-repo", h.pool})
	})
	if err != nil || remoteSelf != localSelf {
		t.Fatalf("self-check remote/local = %q/%q, err=%v", remoteSelf, localSelf, err)
	}
	if req := h.nextRegister(t, before); req.Vendor != nil || req.Sweep {
		t.Fatalf("bare self-check wire request = %#v, want Vendor:nil Sweep:false", req)
	}
	if _, err := os.Stat(h.cfg.AgentRegistrationPath(staleHub)); err != nil {
		t.Fatalf("self-check swept a hub peer: %v", err)
	}
	before = len(h.regs)
	remoteSelfJSON, _, err := h.capture(t, func() error {
		return cmdSelfCheck([]string{"--as", h.agent, "--control-repo", h.remote.URL, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	localSelfJSON, _, err := h.capture(t, func() error {
		return cmdSelfCheck([]string{"--as", h.agent, "--control-repo", h.pool, "--json"})
	})
	if err != nil || remoteSelfJSON != localSelfJSON {
		t.Fatalf("self-check JSON remote/local = %q/%q, err=%v", remoteSelfJSON, localSelfJSON, err)
	}
	if req := h.nextRegister(t, before); req.Vendor != nil || req.Sweep {
		t.Fatalf("bare self-check JSON wire request = %#v, want Vendor:nil Sweep:false", req)
	}

	before = len(h.regs)
	explicitOut, _, err := h.capture(t, func() error {
		return cmdRegister([]string{"--as", h.agent, "--vendor", "explicit-vendor", "--control-repo", h.remote.URL})
	})
	if err != nil || lineWithPrefix(explicitOut, "  vendor:") != "  vendor:        explicit-vendor\n" {
		t.Fatalf("explicit vendor output/error = %q/%v", explicitOut, err)
	}
	if req := h.nextRegister(t, before); req.Vendor == nil || *req.Vendor != "explicit-vendor" || req.Sweep {
		t.Fatalf("explicit register wire request = %#v", req)
	}
}

func TestWI57RemoteServeVendorAndLiveLeaseTurnEnd(t *testing.T) {
	h := newWI57Harness(t, "work-tiny", "resolved-vendor")
	originalOpenChannel := openRemoteServeChannel
	t.Cleanup(func() { openRemoteServeChannel = originalOpenChannel })
	errStopBeforeWrapper := errors.New("WI5.7 register request captured")
	runServe := func(vendor ...string) (op.RegisterReq, error) {
		var captured op.RegisterReq
		fake := &fakeRemoteServeChannel{token: "remote-token"}
		fake.registerFn = func(req op.RegisterReq) error {
			captured = req
			return errStopBeforeWrapper
		}
		openRemoteServeChannel = func(*loop.Config, string) (remoteServeChannel, error) { return fake, nil }
		args := []string{"--as", h.agent, "--control-repo", h.remote.URL, "--relaunch=false"}
		args = append(args, vendor...)
		args = append(args, "--", "wi57-wrapper-never-started")
		_, _, err := h.capture(t, func() error { return cmdServe(args) })
		return captured, err
	}

	bare, err := runServe()
	if !errors.Is(err, errStopBeforeWrapper) || bare.Vendor != nil || bare.Sweep {
		t.Fatalf("bare remote serve request = %#v, err=%v", bare, err)
	}
	explicit, err := runServe("--vendor", "xai")
	if !errors.Is(err, errStopBeforeWrapper) || explicit.Vendor == nil || *explicit.Vendor != "xai" || explicit.Sweep {
		t.Fatalf("explicit remote serve request = %#v, err=%v", explicit, err)
	}
	if _, err := runServe("--vendor", "bad/vendor"); err == nil || errors.Is(err, errStopBeforeWrapper) {
		t.Fatalf("remote serve invalid vendor error = %v", err)
	}

	lease, err := loop.AcquireServeLease(h.cfg, h.agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })
	withCwd(t, h.root, func() {
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", lease.Token)
		if err := cmdSelfCheck([]string{"--as", h.agent, "--control-repo", h.remote.URL, "--quiet"}); err != nil {
			t.Fatalf("self-check under live lease: %v", err)
		}
		stdout, _, turnErr := captureStdoutStderr(t, func() error {
			return cmdTurnEnd([]string{"--as", h.agent, "--control-repo", h.remote.URL, "--json"})
		})
		if turnErr != nil {
			t.Fatalf("turn-end under live lease: %v\n%s", turnErr, stdout)
		}
		var status turnEndJSON
		if err := json.Unmarshal([]byte(stdout), &status); err != nil || status.Blocked {
			t.Fatalf("turn-end JSON = %q, err=%v", stdout, err)
		}
	})
}

func TestWI57RemoteStatusTruncationAndSessionStartCache(t *testing.T) {
	h := newWI57Harness(t, "a-actor", "resolved-vendor")
	writeWI57Registration(t, h.cfg, "b-peer", "test", "normal-host", h.now)
	writeWI57Registration(t, h.cfg, "z-huge", "test", strings.Repeat("h", hubwire.MaxControlLine), h.now)

	stdout, stderr, err := h.capture(t, func() error { return cmdStatus([]string{"--as", h.agent}) })
	if err != nil || stderr != "" {
		t.Fatalf("remote status err/stderr = %v/%q", err, stderr)
	}
	resolvedRoot, err := filepath.EvalSymlinks(h.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"control_repo: " + h.remote.URL + "   (via pointer:" + filepath.Join(resolvedRoot, loop.PointerFileName) + ")\n",
		"loop_dir:     " + h.remote.ShadowLoopDir + "   (via remote)\n",
		"  (local shadow: this process's own loop dir, not the hub's)\n",
		"a-actor", "b-peer",
		"note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("remote status missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "z-huge") {
		t.Fatalf("oversize lexicographic tail row leaked through prefix truncation:\n%s", stdout)
	}

	if err := hubclient.RecordConnectFailure(h.remote, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	originalOpen := openRemoteOneShot
	openCalls := 0
	openRemoteOneShot = func(*loop.Config, string) (*hubclient.OneShot, error) {
		openCalls++
		return nil, errors.New("cached SessionStart dialed")
	}
	t.Cleanup(func() { openRemoteOneShot = originalOpen })
	stdout, stderr, err = h.capture(t, func() error {
		return cmdBoot([]string{"--as", h.agent, "--codex-hook", "SessionStart"})
	})
	if err != nil || stdout != "" || stderr != "hub unreachable; skipping (will retry next event)\n" || openCalls != 0 {
		t.Fatalf("cached SessionStart stdout/stderr/open/err = %q/%q/%d/%v", stdout, stderr, openCalls, err)
	}
}
