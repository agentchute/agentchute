//go:build sshd_integration

package sshd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

func TestSSHDJoinAuthorizeVerifyHappyPath(t *testing.T) {
	h := newSSHDHarness(t)
	checkout := h.newCheckout()

	stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--as", "work-tiny")
	if err != nil {
		t.Fatalf("hub join: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "joined: work-tiny @ "+h.remote.URL) {
		t.Fatalf("join output = %q", stdout)
	}
	pointer, err := os.ReadFile(filepath.Join(checkout, ".agentchute-control-repo"))
	if err != nil || strings.TrimSpace(string(pointer)) != h.remote.URL {
		t.Fatalf("pointer = %q, %v", pointer, err)
	}

	remote := parseRemoteForHome(t, h)
	data, err := os.ReadFile(remote.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg hubclient.HubConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.URL != h.remote.URL || cfg.Pool != h.pool || cfg.Pool12 != sshdPoolID || len(cfg.JoinedAs) != 1 || cfg.JoinedAs[0] != "work-tiny" {
		t.Fatalf("client config = %+v", cfg)
	}
	authorized, err := os.ReadFile(h.authorized)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(authorized), "agentchute:work-tiny:"+sshdPoolID); count != 1 {
		t.Fatalf("authorized marker count = %d\n%s", count, authorized)
	}

	stdout, stderr, err = h.runCLI(checkout, "register", "--as", "work-tiny", "--vendor", "test")
	if err != nil {
		t.Fatalf("remote register: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	registered, err := loop.ReadRegistration(h.cfg.AgentRegistrationPath("work-tiny"))
	if err != nil {
		t.Fatal(err)
	}
	localHost, _ := os.Hostname()
	if registered.Host != strings.TrimSpace(localHost) {
		t.Fatalf("remote register host = %q, want joining host %q", registered.Host, localHost)
	}
	stdout, stderr, err = h.runCLI(checkout, "status", "--as", "work-tiny")
	if err != nil {
		t.Fatalf("remote status: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "control_repo: "+h.remote.URL) || !strings.Contains(stdout, "work-tiny") {
		t.Fatalf("remote status output = %q", stdout)
	}
}

func TestSSHDIdentityMismatchExecutesNoOperation(t *testing.T) {
	h := newSSHDHarness(t)
	before := snapshotPool(t, h)
	frame := rawHello(t, h, "codex", "grok", hubwire.Protocol, 1, 1)
	assertWireError(t, frame, hubwire.CodeIdentity)
	after := snapshotPool(t, h)
	if before != after {
		t.Fatalf("pool changed after identity refusal\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestSSHDVersionMismatchExecutesNoOperation(t *testing.T) {
	h := newSSHDHarness(t)
	before := snapshotPool(t, h)
	frame := rawHello(t, h, "codex", "codex", hubwire.Protocol, 2, 2)
	assertWireError(t, frame, hubwire.CodeVersion)
	after := snapshotPool(t, h)
	if before != after {
		t.Fatalf("pool changed after version refusal\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestSSHDHostKeyChangeRefused(t *testing.T) {
	h := newSSHDHarness(t)
	h.stop()
	h.hostKey = filepath.Join(h.root, "replacement_host_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-f", h.hostKey)
	h.writeConfig()
	h.start()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := hubclient.Probe(ctx, h.remote, "codex", "sshd-integration", h.keys["codex"])
	if code := hubclient.ErrorCode(err); code != "E_HOSTKEY_CHANGED" {
		t.Fatalf("host-key change error = %v, code %q", err, code)
	}
}

func parseRemoteForHome(t *testing.T, h *sshdHarness) *loop.RemoteConfig {
	t.Helper()
	return parseRemoteURLForHome(t, h, h.remote.URL)
}

// parseRemoteURLForHome resolves any hub URL against the harness client HOME,
// which is what decides the hub dir the CLI will use.
func parseRemoteURLForHome(t *testing.T, h *sshdHarness, url string) *loop.RemoteConfig {
	t.Helper()
	old, present := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", h.clientHome); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if present {
			_ = os.Setenv("HOME", old)
		} else {
			_ = os.Unsetenv("HOME")
		}
	}()
	remote, err := loop.ParseRemoteURL(url)
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func rawHello(t *testing.T, h *sshdHarness, pinned, acting, proto string, version, minVersion int) hubwire.RawFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := h.open(ctx, pinned, "discard-this-command", hubclient.SSHBuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	request := hubwire.Hello{
		RequestBase: hubwire.RequestBase{T: "hello", ID: 1},
		Proto:       proto,
		V:           version,
		MinV:        minVersion,
		Agent:       acting,
		Bin:         "sshd-integration",
	}
	if err := hubwire.NewWriter(session).Write(request, nil); err != nil {
		t.Fatal(err)
	}
	frame, err := hubwire.NewReader(session).Read()
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func assertWireError(t *testing.T, frame hubwire.RawFrame, code string) {
	t.Helper()
	if frame.T != "error" {
		t.Fatalf("frame type = %q, want error", frame.T)
	}
	var wireErr hubwire.Error
	if err := frame.Decode(&wireErr); err != nil {
		t.Fatal(err)
	}
	if wireErr.Code != code {
		t.Fatalf("error code = %q, want %q (%s)", wireErr.Code, code, wireErr.Msg)
	}
}

func snapshotPool(t *testing.T, h *sshdHarness) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(filepath.Join(h.pool, ".agentchute", "loop"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(h.pool, path)
		if err != nil {
			return err
		}
		out.WriteString(rel)
		out.WriteByte('\n')
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out.Write(data)
			out.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}
