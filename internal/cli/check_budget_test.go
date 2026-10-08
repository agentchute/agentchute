package cli

import (
	"strconv"
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

// The op's per-message overhead is an upper bound on the real renderer: with
// the stale banner and the reply-required command line both printed, two
// messages whose single render is R bytes each must not both be displayed
// under a budget of 2R-1 (codex measured 1,278 rendered under a 1,034 budget
// with the old 192-byte allowance).
func TestCheckBudgetBoundCoversRenderer(t *testing.T) {
	stale := []byte("---\nfrom: alice\nreply_required: true\n---\n\nplease reply\n")
	render := func(n int, budget string) string {
		t.Helper()
		root, cfg := setupConsumeFixture(t)
		var out string
		withCwd(t, root, func() {
			clearGuardEnv(t)
			for seq := uint64(1); seq <= uint64(n); seq++ {
				mustWriteSeqInbox(t, cfg.AgentInboxDir("bob"), "alice", seq, stale)
			}
			var err error
			out, err = checkAs(t, "bob", "--budget-bytes", budget)
			if err != nil {
				t.Fatal(err)
			}
		})
		return out
	}
	one := render(1, "0")
	if !strings.Contains(one, "reply-required:") {
		t.Fatalf("fixture did not render the reply-required line:\n%s", one)
	}
	// Everything but the trailing CLAIMED note is one message's render.
	single := strings.Index(one, "note: messages CLAIMED")
	if single < 0 {
		t.Fatalf("no CLAIMED note in:\n%s", one)
	}
	two := render(2, strconv.Itoa(2*single-1))
	if got := strings.Count(two, "---- "); got != 1 {
		t.Fatalf("displayed %d messages under a budget one byte short of two renders (%d each); the overhead bound under-counts the renderer:\n%s", got, single, two)
	}
	if !strings.Contains(two, "(reached budget of") {
		t.Fatalf("no budget line:\n%s", two)
	}
}
