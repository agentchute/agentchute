package hubclient

import (
	"errors"
	"strings"
	"testing"
)

// E_CHANNEL_LOST is the fallback arm of classifySSHFailure — reached only after
// host-key, permission-denied, exit-127, hello-timeout and connect are ruled
// out. It is therefore the one error that means "we could not classify this",
// and it had been discarding two of the three signals available at that point.
//
// These rows pin what each signal contributes, because the whole value of the
// arm is telling three different failures apart. A version that renders only the
// read error passes nothing here except the read-only row, which is the point:
// "EOF" restates what "channel lost" already means.
func TestChannelLostDetailSeparatesTheThreeSignals(t *testing.T) {
	cases := []struct {
		name    string
		waitErr error
		stderr  string
		cause   error
		want    []string
		absent  []string
	}{
		{
			// The distinction that motivated this: ssh faithfully propagating a
			// non-zero exit from the remote hub session looks identical to a dropped
			// connection unless the status is reported.
			name:    "remote command exited non-zero",
			waitErr: errors.New("exit status 1"),
			cause:   errors.New("EOF"),
			want:    []string{"exit status 1", "read: EOF"},
		},
		{
			// Killed by a signal is a third case again, and it points at something
			// tearing the process down rather than at the transport.
			name:    "torn down by a signal",
			waitErr: errors.New("signal: killed"),
			cause:   errors.New("EOF"),
			want:    []string{"signal: killed"},
		},
		{
			// ssh's own account, when it gave one.
			name:   "ssh explains itself",
			stderr: "Connection to hub closed by remote host.\nclient_loop: send disconnect: Broken pipe\n",
			cause:  errors.New("read: connection reset by peer"),
			want:   []string{"ssh: client_loop: send disconnect: Broken pipe", "connection reset by peer"},
		},
		{
			// Nothing to add is not the same as something empty: the message must
			// stay exactly as it was rather than grow an empty parenthetical.
			name: "no signals at all",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := channelLostDetail(tc.waitErr, tc.stderr, tc.cause)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("detail = %q, want empty — an empty parenthetical is worse than none", got)
				}
				return
			}
			if !strings.HasPrefix(got, " (") || !strings.HasSuffix(got, ")") {
				t.Fatalf("detail is not a parenthetical: %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("detail %q does not carry %q", got, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Fatalf("detail %q leaks %q", got, absent)
				}
			}
		})
	}
}

// ssh's stderr can carry remote paths, so only the last non-empty line is taken
// and it is length-capped. Both halves are asserted: an unbounded tail would put
// arbitrary remote output into an error message that gets pasted into issues.
func TestChannelLostDetailBoundsStderr(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := channelLostDetail(nil, "earlier line with /home/someone/secret/path\n"+long+"\n", nil)
	if strings.Contains(got, "secret") {
		t.Fatalf("detail included an earlier stderr line: %q", got)
	}
	if len(got) > 260 {
		t.Fatalf("detail is %d bytes; the stderr tail is not capped", len(got))
	}
	if !strings.HasSuffix(got, "…)") {
		t.Fatalf("a truncated tail must say so: %q", got[len(got)-20:])
	}
}
