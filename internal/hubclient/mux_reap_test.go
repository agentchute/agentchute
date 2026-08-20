package hubclient

import "testing"

// grok's #167 sweep: `ssh -O check`/`-O exit` exits non-zero for several
// different reasons and the reap collapsed them into one condition.
//
// The four strings below are MEASURED from the same command, not guessed:
// a missing socket, a socket in a directory this process cannot enter, a path
// holding something that is not a socket, and a socket whose connect is refused.
// Only the first proves no master is running, and only the first may be reported
// as a successful reap.
func TestMuxReapTreatsOnlyAnAbsentSocketAsReaped(t *testing.T) {
	rows := []struct {
		name     string
		output   string
		wantGone bool
	}{
		{
			name:     "no socket there — the only proof of absence",
			output:   "Control socket connect(/tmp/rp/a.sock): No such file or directory\n",
			wantGone: true,
		},
		{
			// The one that matters. A socket this process cannot reach may be a
			// perfectly live master owned by another account; calling it reaped
			// tells a rotation or migration the old connection is closed when it
			// is not — which is how a stale forced-command snapshot survives.
			name:   "permission denied — may be a LIVE master",
			output: "Control socket connect(/tmp/rp/lk/x): Permission denied\n",
		},
		{
			name:   "not a socket — proves nothing about a master",
			output: "Control socket connect(/tmp/rp/plain): Socket operation on non-socket\n",
		},
		{
			// Looks like proof and is not: a unix socket whose listen backlog is
			// full refuses connects while its listener is alive, and a stale
			// socket file from a crashed master gives the identical error.
			name:   "connection refused — indistinguishable from a busy live master",
			output: "Control socket connect(/tmp/rp/dead.sock): Connection refused\n",
		},
		{
			// Not a connect failure at all: ssh rejected the path before trying.
			name:   "path too long — never reached the socket",
			output: "ControlPath too long ('/very/long/path' >= 104 bytes)\n",
		},
		{
			name:   "unrelated failure",
			output: "ssh: connect to host hub.example port 22: Connection timed out\n",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := muxSocketIsGone(row.output); got != row.wantGone {
				t.Fatalf("muxSocketIsGone(%q) = %v, want %v", row.output, got, row.wantGone)
			}
		})
	}
}

// Case-insensitively, because the test that this replaced lowercased its input
// and the replacement must not quietly become stricter than measured output.
func TestMuxReapMatchIsCaseInsensitive(t *testing.T) {
	if !muxSocketIsGone("CONTROL SOCKET CONNECT(/tmp/x): NO SUCH FILE OR DIRECTORY") {
		t.Fatal("an uppercase rendering of the absent-socket message was not recognised")
	}
}
