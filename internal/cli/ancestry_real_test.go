//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// The real platform lookup, no fake: the test binary's parent is an ancestor,
// and a live child of the test is not — the shape of a daemon whose env names a
// runner it was never started by.
func TestRunnerAncestryCheckRealProcesses(t *testing.T) {
	if ppid, err := platformParentPID(os.Getpid()); err != nil || ppid != os.Getppid() {
		t.Fatalf("platformParentPID(self) = %d, %v; want %d", ppid, err, os.Getppid())
	}

	t.Setenv("AGENTCHUTE_RUNNER_PID", strconv.Itoa(os.Getppid()))
	if err := runnerAncestryCheck(); err != nil {
		t.Fatalf("parent as runner: %v", err)
	}

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start a child process: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	t.Setenv("AGENTCHUTE_RUNNER_PID", strconv.Itoa(child.Process.Pid))
	if err := runnerAncestryCheck(); !errors.Is(err, errForeignRunnerEnv) {
		t.Fatalf("non-ancestor as runner: %v, want errForeignRunnerEnv", err)
	}
}
