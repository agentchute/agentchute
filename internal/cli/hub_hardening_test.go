package cli

import (
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Review 2026-10-08 S8: the join's paste line quotes the pool and the key like
// every other printed hub command.
func TestHubAuthorizePasteSingleQuotesItsArguments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	got := hubAuthorizePaste(remote, "codex-tiny", "ssh-ed25519 AAAAC3Nz agentchute:codex-tiny:1", false)
	want := "agentchute hub authorize --agent 'codex-tiny' --pool '/home/alex/code/agentchute' --key 'ssh-ed25519 AAAAC3Nz agentchute:codex-tiny:1'"
	if !strings.Contains(got, want) {
		t.Fatalf("paste =\n%s\nwant it to contain\n  %s", got, want)
	}
}
