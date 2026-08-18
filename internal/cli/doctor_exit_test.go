package cli

import (
	"strings"
	"testing"
)

// doctor's printed summary and its process exit status must agree.
//
// The line said "exit 1" while entry.go returns 2 for errBlocked — the
// lifecycle-gate sentinel doctor uses, and the same one gate, turn-end, ack and
// boot rely on. A script parsing the line and one checking $? disagreed about
// the same run. The exit code is the contract (1 is reserved for "the command
// itself failed"), so the line was the wrong side.
func TestDoctorSummaryAgreesWithItsExitStatus(t *testing.T) {
	root := t.TempDir()
	var out string
	var err error
	withCwd(t, root, func() {
		out, err = captureStdout(t, func() error { return cmdDoctor(nil) })
	})
	if err != errBlocked {
		t.Fatalf("doctor in an unscaffolded dir returned %v, want errBlocked", err)
	}
	if !strings.Contains(out, "blocker(s)") {
		t.Fatalf("no blocker summary in %q", out)
	}
	if strings.Contains(out, "exit 1") {
		t.Fatalf("summary still claims exit 1 while the process exits 2: %q", out)
	}
	if !strings.Contains(out, "exit 2") {
		t.Fatalf("summary does not state the real exit status: %q", out)
	}
	// The other half of the contract, so this row fails if someone "fixes" it by
	// changing the exit code instead: errBlocked must still map to 2.
	if code := exitCodeForError(errBlocked); code != 2 {
		t.Fatalf("errBlocked maps to exit %d, want 2 — the gate sentinel is shared with gate/turn-end/ack/boot", code)
	}
}
