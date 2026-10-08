package hubclient

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Review 2026-10-08 S11: the ssh transport buffered ALL of ssh's stderr for the
// life of the process — a chatty or hostile hub could grow a long-lived
// channel's memory without bound. It now keeps a capped tail, like the pinning
// probe already did; the tail is what classification reads.
func TestTransportKeepsOnlyACappedStderrTail(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("sh", "-c", `head -c 300000 /dev/zero | tr '\0' a >&2; printf 'the last line' >&2`)
	p, err := startProcessTransport(cmd, cancel)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	got, _ := p.diagnostics()
	if len(got) > transportStderrLimit {
		t.Fatalf("kept %d bytes of stderr, want at most %d", len(got), transportStderrLimit)
	}
	if !strings.HasSuffix(got, "the last line") {
		t.Fatalf("the tail was not kept: ...%q", got[max(0, len(got)-40):])
	}
}
