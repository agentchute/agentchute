package loop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemoteURLGrammarAndCanonicalForm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		raw  string
		want string
	}{
		{"ssh://Alex@HUB.Example:22/absolute/pool/", "ssh://Alex@hub.example/absolute/pool"},
		{"ssh://host-alias:2222/pool", "ssh://host-alias:2222/pool"},
		{"ssh://host/", "ssh://host/"},
	}
	for _, tt := range tests {
		got, err := ParseRemoteURL(tt.raw)
		if err != nil {
			t.Fatalf("ParseRemoteURL(%q): %v", tt.raw, err)
		}
		if got.URL != tt.want {
			t.Errorf("ParseRemoteURL(%q).URL = %q, want %q", tt.raw, got.URL, tt.want)
		}
		if len(got.HubID) != 12 {
			t.Errorf("ParseRemoteURL(%q).HubID = %q", tt.raw, got.HubID)
		}
	}
}

func TestParseRemoteURLRejectsInvalidGrammar(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, raw := range []string{
		"http://host/pool",
		"ssh://host",
		"ssh://-host/pool",
		"ssh://-user@host/pool",
		"ssh://user:password@host/pool",
		"ssh://host:0/pool",
		"ssh://host:65536/pool",
		"ssh://host:/pool",
		"ssh://host/pool?query=1",
		"ssh://host/pool#fragment",
		"ssh://host name/pool",
	} {
		if _, err := ParseRemoteURL(raw); err == nil {
			t.Errorf("ParseRemoteURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestDiscoverRemoteJoinedUsesShadowAndLocalRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, specFileName), []byte("# spec\n"))
	cwd := filepath.Join(repo, "nested")
	mustMkdir(t, cwd)
	remote, err := ParseRemoteURL("ssh://Nobody@UNROUTABLE.Invalid:22/remote/pool/")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, remote.ConfigPath, []byte("{}\n"))

	cfg, err := Discover(DiscoverOpts{Cwd: cwd, ControlRepoFlag: "ssh://Nobody@UNROUTABLE.Invalid:22/remote/pool/"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote == nil || cfg.Remote.URL != "ssh://Nobody@unroutable.invalid/remote/pool" {
		t.Fatalf("Remote = %#v", cfg.Remote)
	}
	if cfg.ControlRepo != repo {
		t.Fatalf("ControlRepo = %q, want local repo %q", cfg.ControlRepo, repo)
	}
	wantShadow := filepath.Join(home, ".agentchute", "hub", remote.HubID, ".agentchute", "loop")
	if cfg.LoopDir != wantShadow || cfg.Remote.ShadowLoopDir != wantShadow {
		t.Fatalf("shadow = %q / %q, want %q", cfg.LoopDir, cfg.Remote.ShadowLoopDir, wantShadow)
	}
	if cfg.Vendor != "agentchute" || cfg.LoopDirOrigin != "remote" {
		t.Fatalf("Vendor/LoopDirOrigin = %q/%q", cfg.Vendor, cfg.LoopDirOrigin)
	}
}

func TestDiscoverRemoteNotJoinedAndExplicitLoopRefusal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	url := "ssh://host.example/remote/pool"
	_, err := Discover(DiscoverOpts{Cwd: cwd, ControlRepoFlag: url})
	if !errors.Is(err, ErrRemoteNotJoined) {
		t.Fatalf("unjoined error = %v, want ErrRemoteNotJoined", err)
	}
	remote, err := ParseRemoteURL(url)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, remote.ConfigPath, []byte("{}\n"))
	_, err = Discover(DiscoverOpts{Cwd: cwd, ControlRepoFlag: url, EnvLoopDir: "/tmp/loop"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("explicit loop error = %v", err)
	}
}

func TestDiscoverRemoteEnvBeatsPointer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	envURL := "ssh://env.example/env/pool"
	pointerURL := "ssh://pointer.example/pointer/pool"
	for _, raw := range []string{envURL, pointerURL} {
		remote, err := ParseRemoteURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, remote.ConfigPath, []byte("{}\n"))
	}
	mustWrite(t, filepath.Join(cwd, PointerFileName), []byte(pointerURL+"\n"))
	cfg, err := Discover(DiscoverOpts{Cwd: cwd, EnvControlRepo: envURL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote == nil || cfg.Remote.URL != envURL || cfg.ControlRepoOrigin != "env" {
		t.Fatalf("remote/origin = %#v/%q", cfg.Remote, cfg.ControlRepoOrigin)
	}
}

func TestResolvePointerTargetPreservesSSHLocator(t *testing.T) {
	raw := "ssh://User@Host.Example/absolute/pool"
	got, err := ResolvePointerTarget(t.TempDir(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("ResolvePointerTarget = %q, want unchanged %q", got, raw)
	}
}

func TestRequireRemoteJoinRejectsNonRegularConfigPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := ParseRemoteURL("ssh://host.example/pool")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remote.ConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RequireRemoteJoin(remote); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("non-regular config error = %v", err)
	}
}
