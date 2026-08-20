package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Tail of grok's #167 sweep. Both of these gave up silently, and in both cases
// the silence is indistinguishable from the ordinary "nothing to do here" that
// the same code path produces every day.

// git failing is not the same as there being no git. The first is worth a word;
// the second is the common case and must stay quiet, or every join in a
// non-repository prints a warning and operators learn to ignore warnings.
func TestPointerExcludeSaysSoWhenGitFailsForAnyOtherReason(t *testing.T) {
	t.Run("not a repository stays silent", func(t *testing.T) {
		root := t.TempDir()
		stderr := captureStderr(t, func() {
			if err := appendHubPointerExclude(root); err != nil {
				t.Fatalf("a plain directory must not fail the join: %v", err)
			}
		})
		if strings.TrimSpace(stderr) != "" {
			t.Fatalf("a directory that is simply not a repository warned:\n%s", stderr)
		}
	})

	t.Run("git missing entirely is reported", func(t *testing.T) {
		// A PATH with no git at all: exec fails to start it, which is not an
		// ExitError and therefore not "not a repository".
		t.Setenv("PATH", t.TempDir())
		root := t.TempDir()
		stderr := captureStderr(t, func() {
			if err := appendHubPointerExclude(root); err != nil {
				t.Fatalf("a missing git must not fail the join: %v", err)
			}
		})
		if !strings.Contains(stderr, "warning:") {
			t.Fatalf("a git that could not run at all was silent:\n%s", stderr)
		}
		// It must name the file, or the warning is about nothing the operator
		// can look for.
		if !strings.Contains(stderr, loop.PointerFileName) {
			t.Fatalf("the warning does not name the pointer file:\n%s", stderr)
		}
	})

	t.Run("a real repository still gets the exclude line", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		root := t.TempDir()
		if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		if err := appendHubPointerExclude(root); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), loop.PointerFileName) {
			t.Fatalf("the pointer was not excluded:\n%s", data)
		}
	})
}

// setupLocalHost answers the WEAK question — "could this be ours?" — and returns
// true for both unknowns, because almost every caller uses it to refuse.
//
// The pair that STOPS a process needs more for exactly one of those unknowns.
// The other is deliberately left alone, and the difference is the whole point:
// a record with no host proceeds, because the check immediately downstream is
// stronger evidence than any hostname string — the cmdline of a LOCAL pid,
// matched against this pool. Refusing those would stop `setup` from stopping
// runners recorded before the host field existed.
func TestSignalPathRefusesOnlyWhenTheHostCannotBeCompared(t *testing.T) {
	rows := []struct {
		name    string
		host    string
		local   string
		want    bool
		wantWhy string
	}{
		{
			// The case with no backstop at the point of decision.
			name:    "the record names a machine and we do not know our own name",
			host:    "tiny",
			local:   "",
			want:    true,
			wantWhy: "cannot be compared",
		},
		{
			// Left alone on purpose: the cmdline check is the real proof, and
			// older runner records carry no host at all.
			name:  "the record names no host — the cmdline check decides",
			host:  "",
			local: "",
		},
		{name: "no host, known local name", host: "", local: "hub44"},
		{name: "our name is known and matches", host: "hub44", local: "hub44"},
		{name: "our name is known and differs", host: "tiny", local: "hub44"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			original := localHostname
			localHostname = func() string { return row.local }
			t.Cleanup(func() { localHostname = original })

			got, why := setupHostSaysElsewhereUnknown(row.host)
			if got != row.want {
				t.Fatalf("refuses = %v (%q), want %v", got, why, row.want)
			}
			if row.wantWhy != "" && !strings.Contains(why, row.wantWhy) {
				t.Fatalf("reason = %q, want it to carry %q", why, row.wantWhy)
			}
			if got && !strings.Contains(why, row.host) {
				t.Fatalf("the refusal does not name the host it could not compare: %q", why)
			}
			if !got && why != "" {
				t.Fatalf("a non-refusal carried a reason: %q", why)
			}
		})
	}

	// The weak form is unchanged, deliberately: the refusal-shaped callers
	// depend on "might be ours" meaning yes.
	t.Run("the weak form still says yes to both unknowns", func(t *testing.T) {
		original := localHostname
		localHostname = func() string { return "" }
		t.Cleanup(func() { localHostname = original })
		if !setupLocalHost("tiny") {
			t.Fatal("an unknown local hostname changed the weak form's answer")
		}
		localHostname = func() string { return "hub44" }
		if !setupLocalHost("") {
			t.Fatal("a record with no host changed the weak form's answer")
		}
	})
}

// The caller row. Without it the rule above is a function nothing calls: the
// mutation that deletes the guard from stopSetupRunner passes every assertion
// in this file, which is how a helper ends up correct and unused.
func TestStopSetupRunnerRefusesWhenItCannotCompareTheHost(t *testing.T) {
	root := t.TempDir()
	loopDir := filepath.Join(root, ".agentchute", "loop")
	stateDir := filepath.Join(loopDir, "state", "codex-agentchute")
	mustMkdir(t, stateDir)
	mustWrite(t, filepath.Join(stateDir, "runner.json"),
		[]byte(`{"agent_id":"codex-agentchute","host":"tiny","runner_pid":222,"started_at":"2026-01-01T00:00:00Z","status":"active"}`+"\n"))
	cfg := &loop.Config{ControlRepo: root, LoopDir: loopDir, Vendor: "agentchute"}

	signaled := map[int]bool{}
	oldAlive, oldSignal := setupProcessAlive, setupSignalProcess
	setupProcessAlive = func(int) bool { return true }
	setupSignalProcess = func(pid int, _ os.Signal) error { signaled[pid] = true; return nil }
	t.Cleanup(func() { setupProcessAlive, setupSignalProcess = oldAlive, oldSignal })

	t.Run("a record from elsewhere, on a machine that cannot name itself", func(t *testing.T) {
		original := localHostname
		localHostname = func() string { return "" }
		t.Cleanup(func() { localHostname = original })

		stopped, note := stopSetupRunner(cfg, "codex-agentchute")
		if stopped || signaled[222] {
			t.Fatalf("signalled a pid from a record naming another machine (stopped=%v)", stopped)
		}
		// Silence here would be the old behaviour wearing a different shape:
		// the operator needs to know why nothing was stopped.
		if !strings.Contains(note, "hostname could not be determined") {
			t.Fatalf("the refusal said nothing usable: %q", note)
		}
		if !strings.Contains(note, "tiny") {
			t.Fatalf("the refusal does not name the host it could not compare: %q", note)
		}
	})

	// The control: with our own name known and matching, the same record is
	// stopped exactly as before. Without this the row above is satisfied by a
	// function that never stops anything.
	t.Run("the same record on the machine it names is still stopped", func(t *testing.T) {
		original := localHostname
		localHostname = func() string { return "tiny" }
		t.Cleanup(func() { localHostname = original })
		oldCmdline := setupProcessCommandLine
		setupProcessCommandLine = func(int) string {
			return "/usr/local/bin/agentchute serve --as codex-agentchute --control-repo " + root + " --loop-dir " + loopDir + " -- codex"
		}
		t.Cleanup(func() { setupProcessCommandLine = oldCmdline })

		if stopped, note := stopSetupRunner(cfg, "codex-agentchute"); !stopped {
			t.Fatalf("a runner on this very machine was not stopped: %q", note)
		}
		if !signaled[222] {
			t.Fatal("no signal was sent")
		}
	})
}
