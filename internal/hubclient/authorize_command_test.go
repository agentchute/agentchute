package hubclient

import (
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Review 2026-10-08 S8: every "run this ON THE HUB" command is pasted into a
// shell by an operator, so every argument is single-quoted — the pool above
// all, which used to be printed bare.
func TestHubAuthorizeCommandSingleQuotesEveryArgument(t *testing.T) {
	got := HubAuthorizeCommand("codex-tiny", "/home/alex/pool;id", "ssh-ed25519 AAAAC3Nz agentchute:codex-tiny:1", true)
	want := "agentchute hub authorize --agent 'codex-tiny' --pool '/home/alex/pool;id' --key 'ssh-ed25519 AAAAC3Nz agentchute:codex-tiny:1' --replace-key"
	if got != want {
		t.Fatalf("command =\n  %s\nwant\n  %s", got, want)
	}
	if got := HubAuthorizeCommand("a", "/p'q", "k", false); !strings.Contains(got, `--pool '/p'\''q'`) {
		t.Fatalf("an embedded quote is not escaped: %s", got)
	}
}

func TestUnpinnedFallbackMessageQuotesThePool(t *testing.T) {
	remote := &loop.RemoteConfig{Host: "hub.example", PoolPath: "/home/alex/code/agentchute"}
	msg := hubUnpinnedOperatorFallbackMessage(remote, "codex-tiny")
	if !strings.Contains(msg, "--agent 'codex-tiny' --pool '/home/alex/code/agentchute' --key ") {
		t.Fatalf("message does not quote its arguments:\n%s", msg)
	}
}
