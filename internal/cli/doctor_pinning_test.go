package cli

import (
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Row 10/11's doctor half: the verdict-to-severity mapping, both arms. The
// verdict TEXT is pinned in internal/hubclient and the host behaviour in
// integration/sshd; what only this layer decides is whether an operator is
// stopped or waved through.
func TestHubPinningCheckSeverity(t *testing.T) {
	remote := &loop.RemoteConfig{Host: "hub44", PoolPath: "/srv/pool"}

	t.Run("pinned is OK, and doctor does not repeat the probe's prose", func(t *testing.T) {
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, bool) {
			return "some message the probe would only produce when UNPINNED", true
		})()
		got := hubPinningCheck(remote, "opus-high")
		if got.Name != "hub_pinning" {
			t.Fatalf("check name = %q", got.Name)
		}
		if got.Severity != severityOK {
			t.Fatalf("a pinned hub is severity %v, want OK", got.Severity)
		}
		if strings.Contains(got.Message, "UNPINNED") {
			t.Fatalf("the OK arm forwarded the unpinned message: %q", got.Message)
		}
	})

	// The one that matters. An unpinned hub means the agent id and pool for every
	// session are chosen by whoever connects, so doctor must BLOCK, not warn. A
	// warning here reads as "noted" and ships an unpinned hub.
	t.Run("unpinned is a BLOCKER and forwards the verdict verbatim", func(t *testing.T) {
		verdict := "hub: NOT PINNED — hub44 ran a command this machine chose"
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, bool) {
			return verdict, false
		})()
		got := hubPinningCheck(remote, "opus-high")
		if got.Severity != severityBlocker {
			t.Fatalf("an unpinned hub is severity %v, want BLOCKER", got.Severity)
		}
		if got.Message != verdict {
			t.Fatalf("doctor rewrote the verdict:\n got: %s\nwant: %s", got.Message, verdict)
		}
	})

	// The probe is told which agent id to ask about; passing the wrong one (or
	// none) makes the operator-fallback remedy name a key nobody has.
	t.Run("the agent id reaches the probe", func(t *testing.T) {
		var seen string
		defer swapPinningVerdict(t, func(_ *loop.RemoteConfig, agentID string) (string, bool) {
			seen = agentID
			return "", true
		})()
		hubPinningCheck(remote, "opus-high")
		if seen != "opus-high" {
			t.Fatalf("probe received agent id %q, want opus-high", seen)
		}
	})
}

func swapPinningVerdict(t *testing.T, fn func(*loop.RemoteConfig, string) (string, bool)) func() {
	t.Helper()
	prev := hubPinningVerdict
	hubPinningVerdict = fn
	return func() { hubPinningVerdict = prev }
}
