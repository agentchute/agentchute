package cli

import (
	"strconv"
	"strings"
	"testing"
	"time"
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
	// Two bodies: one whose frontmatter `from` matches the filename, and one
	// whose `from` disagrees, so the reprinted SENDER MISMATCH warning is part
	// of the measured render (gate reviews of #215).
	bodies := map[string][]byte{
		"matching-from":    []byte("---\nfrom: alice\nreply_required: true\n---\n\nplease reply\n"),
		"mismatching-from": []byte("---\nfrom: " + strings.Repeat("claude-code-", 20) + "\nreply_required: true\n---\n\nplease reply\n"),
	}
	// Two recipients: the fixture's short "bob", and a 130-char id — the reply
	// line carries the recipient twice and ids have no length cap.
	longID := "r" + strings.Repeat("ecipient-", 14) + "end"
	for bodyName, stale := range bodies {
		for _, recipient := range []string{"bob", longID} {
			t.Run(bodyName+"/"+recipient[:3], func(t *testing.T) {
				render := func(n int, budget string) string {
					t.Helper()
					root, cfg := setupConsumeFixture(t)
					var out string
					withCwd(t, root, func() {
						clearGuardEnv(t)
						if recipient != "bob" {
							if err := cmdRegister([]string{"--as", recipient, "--vendor", "openai"}); err != nil {
								t.Fatal(err)
							}
						}
						for seq := uint64(1); seq <= uint64(n); seq++ {
							// Aged past oldMailBannerAfter so the STALE banner is part
							// of the measured render — the case codex measured.
							mustWriteAgedInbox(t, cfg.AgentInboxDir(recipient), "alice", seq, stale, 48*time.Hour)
						}
						var err error
						out, err = checkAs(t, recipient, "--budget-bytes", budget)
						if err != nil {
							t.Fatal(err)
						}
					})
					return out
				}
				one := render(1, "0")
				if !strings.Contains(one, "reply-required:") || !strings.Contains(one, "[!] STALE") {
					t.Fatalf("fixture did not render both the STALE banner and the reply-required line:\n%s", one)
				}
				if (bodyName == "mismatching-from") != strings.Contains(one, "[!] SENDER MISMATCH") {
					t.Fatalf("%s: mismatch warning presence is wrong:\n%s", bodyName, one)
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
			})
		}
	}
}
