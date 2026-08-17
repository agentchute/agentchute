package cli

import (
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

func TestResolveAgentIDRemoteNamesMap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTCHUTE_AGENT_ID", "")
	remote, err := loop.ParseRemoteURL("ssh://hub.example/remote/pool")
	if err != nil {
		t.Fatal(err)
	}
	if err := hubclient.WriteHubConfig(remote.HubID, &hubclient.HubConfig{
		URL:      remote.URL,
		JoinedAs: []string{"codex-tiny"},
		Names:    map[string]string{"codex": "codex-tiny"},
		Pool:     "/remote/pool",
		Pool12:   "0123456789ab",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &loop.Config{Remote: remote, LoopDir: remote.ShadowLoopDir}

	for _, tt := range []struct {
		name       string
		flagID     string
		fallbackID []string
		want       string
	}{
		{name: "local name", flagID: "codex", want: "codex-tiny"},
		{name: "joined id", flagID: "codex-tiny", want: "codex-tiny"},
		{name: "unmapped passthrough", flagID: "reviewer", want: "reviewer"},
		{name: "direct launch fallback", fallbackID: []string{"codex"}, want: "codex-tiny"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAgentID(tt.flagID, cfg, tt.fallbackID...)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("resolved id = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveAgentIDRemoteEnvName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTCHUTE_AGENT_ID", "codex")
	remote, err := loop.ParseRemoteURL("ssh://hub.example/remote/pool")
	if err != nil {
		t.Fatal(err)
	}
	if err := hubclient.WriteHubConfig(remote.HubID, &hubclient.HubConfig{
		URL:      remote.URL,
		JoinedAs: []string{"codex-tiny"},
		Names:    map[string]string{"codex": "codex-tiny"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAgentID("", &loop.Config{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if got != "codex-tiny" {
		t.Fatalf("resolved env id = %q, want codex-tiny", got)
	}
}
