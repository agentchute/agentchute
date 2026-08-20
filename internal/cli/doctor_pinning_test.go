package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

// Row 10/11's doctor half: the verdict-to-severity mapping, both arms. The
// verdict TEXT is pinned in internal/hubclient and the host behaviour in
// integration/sshd; what only this layer decides is whether an operator is
// stopped or waved through.
func TestHubPinningCheckSeverity(t *testing.T) {
	remote := &loop.RemoteConfig{Host: "hub44", PoolPath: "/srv/pool"}

	t.Run("pinned is OK, and doctor does not repeat the probe's prose", func(t *testing.T) {
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, hubclient.PinningState) {
			return "some message the probe would only produce when UNPINNED", hubclient.PinningPinned
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
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, hubclient.PinningState) {
			return verdict, hubclient.PinningNotPinned
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
		defer swapPinningVerdict(t, func(_ *loop.RemoteConfig, agentID string) (string, hubclient.PinningState) {
			seen = agentID
			return "", hubclient.PinningPinned
		})()
		hubPinningCheck(remote, "opus-high")
		if seen != "opus-high" {
			t.Fatalf("probe received agent id %q, want opus-high", seen)
		}
	})
}

// Rows 21/22: the third severity, and its positive control.
//
// UNVERIFIED is a WARN carrying the reason. Not OK — that was the defect, and
// the assertion that catches a regression to it is that the OK sentence must not
// appear. Not a blocker either: a transient probe failure would exit doctor
// non-zero and train operators to ignore it. And explicitly not INFO, because an
// operator scanning for problems skims past info, which makes it functionally
// identical to OK for the only purpose that matters.
func TestDoctorWarnsWhenPinningCouldNotBeVerified(t *testing.T) {
	remote := &loop.RemoteConfig{Host: "hub44", PoolPath: "/srv/pool"}
	const reason = "could not verify pinning — the probe did not reach hub44 (Network is unreachable)"

	t.Run("unverified is a WARN that carries the reason", func(t *testing.T) {
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, hubclient.PinningState) {
			return reason, hubclient.PinningUnverified
		})()
		got := hubPinningCheck(remote, "opus-high")
		if got.Severity != severityWarn {
			t.Fatalf("severity = %v, want WARN — OK hides it and blocker teaches operators to ignore doctor", got.Severity)
		}
		if got.Message != reason {
			t.Fatalf("doctor rewrote the reason:\n got: %s\nwant: %s", got.Message, reason)
		}
		// The exact regression: printing the pinned sentence for a check that
		// never ran.
		if strings.Contains(got.Message, "pinned by sshd") {
			t.Fatalf("an unverified probe rendered the OK sentence:\n%s", got.Message)
		}
	})

	// The positive control for the row above: a probe that RAN and found no
	// nonce is still OK, so the fix cannot be "warn whenever unsure".
	t.Run("a probe that ran and found the host pinned is still OK", func(t *testing.T) {
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, hubclient.PinningState) {
			return "", hubclient.PinningPinned
		})()
		got := hubPinningCheck(remote, "opus-high")
		if got.Severity != severityOK {
			t.Fatalf("severity = %v, want OK", got.Severity)
		}
		if !strings.Contains(got.Message, "pinned by sshd") {
			t.Fatalf("the OK arm lost its sentence: %s", got.Message)
		}
	})

	// And the blocker arm must not have been swept into the warn bucket.
	t.Run("not pinned is still a BLOCKER", func(t *testing.T) {
		defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, hubclient.PinningState) {
			return "hub: NOT PINNED — hub44 ran a command this machine chose", hubclient.PinningNotPinned
		})()
		if got := hubPinningCheck(remote, "opus-high"); got.Severity != severityBlocker {
			t.Fatalf("severity = %v, want BLOCKER", got.Severity)
		}
	})
}

// The connect-failure path has the same three-way shape: E_HUB_PINNING_UNVERIFIED
// must produce the named check as a WARN, not the blocker its two neighbours get.
func TestDoctorReportsUnverifiedPinningOnAConnectFailure(t *testing.T) {
	cfg := newDoctorHubFixture(t)
	original := openRemoteOneShot
	openRemoteOneShot = func(*loop.Config, string) (*hubclient.OneShot, error) {
		return nil, &hubclient.Error{Code: "E_HUB_PINNING_UNVERIFIED", Msg: "hub: connected, but the hub did not run `agentchute-hub`", Retriable: true}
	}
	t.Cleanup(func() { openRemoteOneShot = original })

	report := runRemoteDoctorChecks(cfg, "opus-high", time.Now().UTC())
	pinning, has := doctorCheckNamed(report, "hub_pinning")
	if !has {
		t.Fatalf("hub_pinning absent; checks = %v", doctorCheckNames(report))
	}
	if pinning.Severity != severityWarn {
		t.Fatalf("severity = %v, want WARN — an unreachable probe is not evidence of an unpinned hub", pinning.Severity)
	}
}

func swapPinningVerdict(t *testing.T, fn func(*loop.RemoteConfig, string) (string, hubclient.PinningState)) func() {
	t.Helper()
	prev := hubPinningVerdict
	hubPinningVerdict = fn
	return func() { hubPinningVerdict = prev }
}

// P2a — the hub_pinning row must be PRESENT on the connect failure that proves
// the hub is unpinned.
//
// Every connect failure returns before the probe, and two of those codes are the
// proof itself: E_UNPINNED from the hub's own Site 1, and E_HUB_UNPINNED from
// this machine's classification of an exit 127. Reporting only hub_connect there
// omits the named check in the exact report an operator — or a script keyed on
// the check name — would look at first.
func TestDoctorReportsHubPinningOnAnUnpinnedConnectFailure(t *testing.T) {
	rows := []struct {
		name     string
		err      error
		wantRow  bool
		wantCode string
	}{
		{
			name:     "E_UNPINNED — the hub refused, on its own evidence",
			err:      &hubclient.Error{Code: hubwire.CodeUnpinned, Msg: "hub: this hub did not apply an authorized_keys forced command"},
			wantRow:  true,
			wantCode: hubwire.CodeUnpinned,
		},
		{
			name:     "E_HUB_UNPINNED — this machine's probes concluded it",
			err:      &hubclient.Error{Code: "E_HUB_UNPINNED", Msg: "hub: NOT PINNED — hub44 ran a command this machine chose"},
			wantRow:  true,
			wantCode: "E_HUB_UNPINNED",
		},
		// The control. An ordinary unreachable hub says nothing about pinning, and
		// a hub_pinning row there would be a verdict nothing measured.
		{
			name:    "E_CONNECT — unreachable says NOTHING about pinning",
			err:     &hubclient.Error{Code: "E_CONNECT", Msg: "hub: cannot reach hub44"},
			wantRow: false,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := newDoctorHubFixture(t)
			original := openRemoteOneShot
			openRemoteOneShot = func(*loop.Config, string) (*hubclient.OneShot, error) { return nil, row.err }
			t.Cleanup(func() { openRemoteOneShot = original })

			report := runRemoteDoctorChecks(cfg, "opus-high", time.Now().UTC())
			connect, hasConnect := doctorCheckNamed(report, "hub_connect")
			if !hasConnect || connect.Severity != severityBlocker {
				t.Fatalf("hub_connect = %+v, want a blocker", connect)
			}
			pinning, has := doctorCheckNamed(report, "hub_pinning")
			if has != row.wantRow {
				t.Fatalf("hub_pinning present = %v, want %v; checks = %v", has, row.wantRow, doctorCheckNames(report))
			}
			if !row.wantRow {
				return
			}
			if pinning.Severity != severityBlocker {
				t.Fatalf("hub_pinning severity = %v, want BLOCKER", pinning.Severity)
			}
			if !strings.Contains(pinning.Message, row.wantCode) {
				t.Fatalf("hub_pinning message does not name %s: %q", row.wantCode, pinning.Message)
			}
		})
	}
}

// The probe must NOT run again on this path: the verdict is already in hand, and
// re-probing a host that was just refused costs two more round trips to say what
// the operator was told a line earlier.
func TestDoctorDoesNotReprobeAfterAnUnpinnedConnectFailure(t *testing.T) {
	cfg := newDoctorHubFixture(t)
	probes := 0
	defer swapPinningVerdict(t, func(*loop.RemoteConfig, string) (string, hubclient.PinningState) {
		probes++
		return "", hubclient.PinningPinned
	})()
	original := openRemoteOneShot
	openRemoteOneShot = func(*loop.Config, string) (*hubclient.OneShot, error) {
		return nil, &hubclient.Error{Code: hubwire.CodeUnpinned, Msg: "hub: refused"}
	}
	t.Cleanup(func() { openRemoteOneShot = original })

	runRemoteDoctorChecks(cfg, "opus-high", time.Now().UTC())
	if probes != 0 {
		t.Fatalf("the behavioural probe ran %d time(s) on a path that already had the verdict", probes)
	}
}

func doctorCheckNamed(r doctorReport, name string) (doctorCheck, bool) {
	for _, check := range r.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return doctorCheck{}, false
}

func doctorCheckNames(r doctorReport) []string {
	names := make([]string, 0, len(r.Checks))
	for _, check := range r.Checks {
		names = append(names, check.Name+"/"+string(check.Severity))
	}
	return names
}

// newDoctorHubFixture builds the least state runRemoteDoctorChecks needs to
// reach the connect attempt: a hub config under a temp HOME, and an active-key
// symlink at 0600.
func newDoctorHubFixture(t *testing.T) *loop.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	const hubID = "abcdef123456"
	hubDir := filepath.Join(home, "hubdir")
	if err := os.MkdirAll(filepath.Join(hubDir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(hubDir, "keys", "opus-high_ed25519.1")
	if err := os.WriteFile(target, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(hubDir, "keys", "opus-high_ed25519")); err != nil {
		t.Fatal(err)
	}
	if err := hubclient.WriteHubConfig(hubID, &hubclient.HubConfig{
		URL:      "ssh://alex@hub44/srv/pool",
		JoinedAs: []string{"opus-high"},
		Pool:     "/srv/pool",
		Pool12:   hubID,
	}); err != nil {
		t.Fatal(err)
	}
	return &loop.Config{Remote: &loop.RemoteConfig{
		Host: "hub44", HubID: hubID, HubDir: hubDir, PoolPath: "/srv/pool",
	}}
}
