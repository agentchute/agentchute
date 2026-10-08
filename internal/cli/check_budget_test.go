package cli

import (
	"strings"
	"testing"
)

// `--budget-bytes 0` lifts the budget on the local path (it is sent as -1 to
// the op, whose 0 means "default"); the default claims two of four 5,000-byte
// messages and says how many remain.
func TestCheckBudgetBytesFlag(t *testing.T) {
	for _, row := range []struct {
		name  string
		extra []string
		want  int
		line  string
	}{
		{"default budget", nil, 2, "(reached budget of 12288 bytes; 2 more pending)"},
		{"--budget-bytes 0 lifts it", []string{"--budget-bytes", "0"}, 4, ""},
		{"--budget-bytes 6000", []string{"--budget-bytes", "6000"}, 1, "(reached budget of 6000 bytes; 3 more pending)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			root, cfg := setupConsumeFixture(t)
			withCwd(t, root, func() {
				clearGuardEnv(t)
				for seq := uint64(1); seq <= 4; seq++ {
					mustWriteSeqInbox(t, cfg.AgentInboxDir("bob"), "alice", seq, []byte("---\nfrom: alice\n---\n\n"+strings.Repeat("x", 5000)+"\n"))
				}
				out, err := checkAs(t, "bob", row.extra...)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Count(out, "---- "); got != row.want {
					t.Fatalf("displayed %d messages, want %d:\n%s", got, row.want, out[:min(len(out), 400)])
				}
				if row.line != "" && !strings.Contains(out, row.line) {
					t.Fatalf("missing %q in:\n%s", row.line, out)
				}
				if row.line == "" && strings.Contains(out, "(reached budget") {
					t.Fatalf("budget line printed with the budget lifted:\n%s", out)
				}
			})
		})
	}
}
