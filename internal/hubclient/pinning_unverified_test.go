package hubclient

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 2b's third state (spec §2b-note, amended 2026-08-20).
//
// As first written, 2b said "anything else ⇒ pinned, including exit 127". That
// is right for a host that was REACHED and refused to run our nonce, and wrong
// for a probe that never ran — which the implementation reported as PINNED,
// printing the OK sentence for a verification that had not happened.
//
// The discriminator is NOT `err != nil`, and getting that wrong trades a false
// assurance for a false alarm in the other direction: a genuinely pinned host
// whose binary is missing exits 127, and that probe ran perfectly. Only three
// things mean SSH ITSELF failed — it could not be started, our timeout fired, or
// it exited 255, ssh's own reserved status as distinct from the remote command's
// status it otherwise passes through.

// exitErrorWithCode produces a real *exec.ExitError, because that is what the
// production discriminator type-asserts on. Fabricating a look-alike would test
// the row's own fiction.
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	// The code is passed as an ARGUMENT, never interpolated into the command
	// string: nothing here is attacker-controlled, but a shell string built by
	// concatenation is a habit worth not having in the tree.
	err := exec.Command("sh", "-c", `exit "$1"`, "sh", strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatalf("a child asked to exit %d reported success", code)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("wanted an ExitError with code %d, got %T %v", code, err, err)
	}
	return err
}

func TestProbeReportsUnverifiedOnlyWhenSSHItselfFailed(t *testing.T) {
	rows := []struct {
		name       string
		stderr     string
		err        func(t *testing.T) error
		want       pinningVerdict
		wantReason string
	}{
		{
			// Row 17.
			name: "ssh could not be started at all",
			err: func(*testing.T) error {
				return &exec.Error{Name: "ssh", Err: exec.ErrNotFound}
			},
			want:       pinningUnverified,
			wantReason: "could not run ssh",
		},
		{
			// Row 18.
			name:       "the probe timed out",
			err:        func(*testing.T) error { return errProbeTimeout },
			want:       pinningUnverified,
			wantReason: "did not return within its timeout",
		},
		{
			// Row 19: 255 is ssh's own reserved status, and the reason must carry
			// what ssh said or the operator gets "could not verify" with no lead.
			name:       "ssh exited 255 — its own failure",
			stderr:     "ssh: connect to host hub.example port 22: Operation timed out\n",
			err:        func(t *testing.T) error { return exitErrorWithCode(t, 255) },
			want:       pinningUnverified,
			wantReason: "Operation timed out",
		},
		{
			// ROW 20, the control that stops the over-correction. Check this one
			// first: a fix that treats any error as unverified passes every row
			// above and silently breaks every missing-binary host.
			name:   "remote exit 127 — the probe RAN and found no nonce",
			stderr: "zsh:1: command not found: agentchute-hub\n",
			err:    func(t *testing.T) error { return exitErrorWithCode(t, 127) },
			want:   pinningPinned,
		},
		{
			name: "a clean run with no nonce is pinned",
			err:  func(*testing.T) error { return nil },
			want: pinningPinned,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			opts := probeFixture(t)
			original := hubProbeRunner
			t.Cleanup(func() { hubProbeRunner = original })
			hubProbeRunner = func([]string, time.Duration) (string, string, error) {
				return "", row.stderr, row.err(t)
			}

			got, reason := probeHubPinning(opts)
			if got != row.want {
				t.Fatalf("verdict = %v (%q), want %v", got, reason, row.want)
			}
			if row.wantReason != "" && !strings.Contains(reason, row.wantReason) {
				t.Fatalf("reason = %q, want it to carry %q", reason, row.wantReason)
			}
			if row.want == pinningPinned && reason != "" {
				t.Fatalf("a pinned verdict carried a reason: %q", reason)
			}
		})
	}
}

// Row 16: the one-liner that was wrong. A buildPinningProbe failure is local —
// the probe demonstrably never left this machine — so it cannot be evidence
// that the host is pinned.
func TestProbeReportsUnverifiedWhenItCannotEvenBeBuilt(t *testing.T) {
	original := hubProbeRunner
	t.Cleanup(func() { hubProbeRunner = original })
	ran := false
	hubProbeRunner = func([]string, time.Duration) (string, string, error) {
		ran = true
		return "", "", nil
	}

	// No remote and no key: BuildSSHInvocation cannot produce an invocation.
	got, reason := probeHubPinning(SSHBuildOptions{})
	if got != pinningUnverified {
		t.Fatalf("verdict = %v, want unverified", got)
	}
	if strings.TrimSpace(reason) == "" {
		t.Fatal("unverified with no reason; the operator is told nothing")
	}
	if ran {
		t.Fatal("the runner was invoked despite the invocation never being built")
	}
}

// The operator-facing half. "I could not check" must never read as "I checked
// and it is fine", and the message has to say which way to lean meanwhile.
func TestUnverifiedVerdictLeansUnverifiedNotPinned(t *testing.T) {
	original := hubProbeRunner
	t.Cleanup(func() { hubProbeRunner = original })
	hubProbeRunner = func([]string, time.Duration) (string, string, error) {
		return "", "ssh: connect to host hub44 port 22: Network is unreachable\n", exitErrorWithCode(t, 255)
	}
	opts := probeFixture(t)
	message, state := PinningVerdict(opts.Remote, "codex")

	if state != PinningUnverified {
		t.Fatalf("state = %v, want PinningUnverified", state)
	}
	for _, want := range []string{"could not verify pinning", "Network is unreachable", "treat the hub as unverified rather than pinned"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message is missing %q:\n%s", want, message)
		}
	}
	// The two claims it must NOT make: that the hub is unpinned, or that it is
	// pinned. Neither was observed.
	if strings.Contains(message, "NOT PINNED") {
		t.Fatalf("an unverified probe asserted the hub is unpinned:\n%s", message)
	}
	if strings.Contains(message, "pinned by sshd") {
		t.Fatalf("an unverified probe asserted the hub is pinned:\n%s", message)
	}
}

// Rows 23/24: the exit-127 arm's fourth outcome, and its control.
func TestClassifyExit127HasAFourthOutcomeWhenTheProbeCouldNotRun(t *testing.T) {
	remote := probeFixture(t).Remote

	t.Run("unverified gets its own code, retriable, asserting neither cause", func(t *testing.T) {
		original := hubPinningProbe
		t.Cleanup(func() { hubPinningProbe = original })
		hubPinningProbe = func(SSHBuildOptions) (pinningVerdict, string) {
			return pinningUnverified, "ssh: connect to host hub44 port 22: Network is unreachable"
		}

		err := classifyExit127(remote, "codex", "zsh:1: command not found: agentchute-hub\n")
		if got := ErrorCode(err); got != "E_HUB_PINNING_UNVERIFIED" {
			t.Fatalf("code = %s, want E_HUB_PINNING_UNVERIFIED", got)
		}
		var e *Error
		if !errors.As(err, &e) || !e.Retriable {
			t.Fatalf("the unverified arm must be retriable; the usual cause is transient: %#v", err)
		}
		text := err.Error()
		// It must name BOTH causes, because it cannot tell them apart.
		for _, want := range []string{"missing", "no forced command was applied", "Network is unreachable"} {
			if !strings.Contains(text, want) {
				t.Fatalf("message is missing %q:\n%s", want, text)
			}
		}
		// And it must borrow neither neighbour's claim: a code's name IS its
		// assertion, and neither assertion is known here.
		if strings.Contains(text, "NOT PINNED") {
			t.Fatalf("reused the unpinned arm's claim:\n%s", text)
		}
		if strings.Contains(text, "the hub DOES apply a forced command") {
			t.Fatalf("reused the missing-binary arm's claim:\n%s", text)
		}
	})

	// The control. Without it, a change that reported every 127 as unverified
	// would pass the row above and break every genuinely missing binary.
	t.Run("a probe that RAN and found the host pinned is still E_HUB_NO_BINARY", func(t *testing.T) {
		original := hubPinningProbe
		t.Cleanup(func() { hubPinningProbe = original })
		hubPinningProbe = func(SSHBuildOptions) (pinningVerdict, string) { return pinningPinned, "" }

		err := classifyExit127(remote, "codex", "")
		if got := ErrorCode(err); got != "E_HUB_NO_BINARY" {
			t.Fatalf("code = %s, want E_HUB_NO_BINARY", got)
		}
	})
}
