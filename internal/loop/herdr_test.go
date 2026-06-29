package loop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeHerdr installs a fake `herdr` executable in dir. Every invocation
// appends its argv (tab-separated, one line per call) to logPath so a test can
// assert exactly which `agent send` / `pane send-keys` calls the poke made.
// exitCode drives the error path (a non-zero exit makes runHerdr fail).
//
// Name→pane resolution no longer shells out to `herdr agent get`: the loop poke
// resolves the pane via the injected SetHerdrPaneResolverHook (see
// stubHerdrPaneResolver), so the fake binary only needs to log argv.
func writeFakeHerdr(t *testing.T, dir, logPath string, exitCode int) string {
	t.Helper()
	herdrPath := filepath.Join(dir, "herdr")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s\\t' \"$@\" >> " + shellSingleQuote(logPath) + "\n")
	b.WriteString("printf '\\n' >> " + shellSingleQuote(logPath) + "\n")
	b.WriteString("exit " + strconv.Itoa(exitCode) + "\n")
	if err := os.WriteFile(herdrPath, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return herdrPath
}

// stubHerdrPaneResolver injects a herdr pane resolver hook for the duration of
// the test and restores the prior hook on cleanup. In loop-only test linkage the
// root package's init() never runs, so the production resolver
// (SetHerdrPaneResolverHook(herdrAgentPaneID)) is never wired and
// herdrPaneIDForAgent — which now REQUIRES the hook (the loop/main boundary) —
// would otherwise error. ok=false makes resolution report no live pane.
func stubHerdrPaneResolver(t *testing.T, paneID string, ok bool) {
	t.Helper()
	prev := herdrPaneResolverHook
	t.Cleanup(func() { SetHerdrPaneResolverHook(prev) })
	SetHerdrPaneResolverHook(func(string) (string, bool) { return paneID, ok })
}

// The herdr poke resolves the name→pane via the injected resolver hook, then
// delivers the wake prompt as literal text WITHOUT a trailing CR and submits via
// a real `pane send-keys <pane> Enter` key event. The old `agent send "<text>\r"`
// shape never submitted, because herdr `agent send` writes literal text and the
// recipient TUI submits on an Enter key event.
func TestPokeHerdrResolvesPaneAndSendsEnterKeyEvent(t *testing.T) {
	oldBinary, oldSleep := herdrBinary, pokeSleep
	t.Cleanup(func() { herdrBinary, pokeSleep = oldBinary, oldSleep })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, 0)
	pokeSleep = time.Millisecond
	stubHerdrPaneResolver(t, "w3:p9", true)

	if err := PokeHerdrTargetContext(context.Background(), "codex-agentchute"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	if !strings.Contains(got, "agent\tsend\tcodex-agentchute\t"+herdrWakePrompt+"\t") {
		t.Errorf("herdr log missing literal wake prompt:\n%q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("herdr wake must NOT inject a carriage return (the no-op submit bug):\n%q", got)
	}
	if !strings.Contains(got, "pane\tsend-keys\tw3:p9\tEnter\t") {
		t.Errorf("herdr log missing Enter key event to resolved pane w3:p9:\n%q", got)
	}
}

func TestPokeHerdrEmptyTargetNoOp(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, 0)

	if err := PokeHerdrTargetContext(context.Background(), "   "); err != nil {
		t.Fatalf("empty target should be a no-op, got %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("herdr binary should not be invoked for an empty target (log exists: %v)", err)
	}
}

func TestPokeHerdrRejectsInjectionShapedTarget(t *testing.T) {
	oldBinary, oldSleep := herdrBinary, pokeSleep
	t.Cleanup(func() { herdrBinary, pokeSleep = oldBinary, oldSleep })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	sentinel := filepath.Join(dir, "pwned")
	// A target laden with shell metacharacters. The wake_target shape validator
	// now rejects it up front (a herdr target must be an agent-id slug), so it
	// never reaches herdr at all — strictly stronger than the prior argv-only
	// defense. Belt-and-suspenders: also assert no shell metacharacter was
	// evaluated and the binary was never invoked.
	evil := "x\"; touch " + sentinel + " #"
	herdrBinary = writeFakeHerdr(t, dir, logPath, 0)
	pokeSleep = time.Millisecond

	if err := PokeHerdrTargetContext(context.Background(), evil); err == nil {
		t.Fatal("injection-shaped herdr target must be rejected before the poke")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("herdr binary must not be invoked for a rejected target (log exists: %v)", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("shell metacharacters in target were evaluated — argv safety broken")
	}
}

// The pane resolver hook is REQUIRED: with no hook wired (the default in
// loop-only test linkage, where the root package's init() never runs) the poke
// must fail fast with a wrapped "resolver not wired" error and never invoke
// herdr. This replaces the old test that asserted the removed `agent get`
// fallback behavior.
func TestPokeHerdrRequiresPaneResolverHook(t *testing.T) {
	prev := herdrPaneResolverHook
	t.Cleanup(func() { SetHerdrPaneResolverHook(prev) })
	SetHerdrPaneResolverHook(nil)

	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })
	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, 0)

	err := PokeHerdrTargetContext(context.Background(), "ghost-agent")
	if err == nil {
		t.Fatal("expected error when the herdr pane resolver hook is unset")
	}
	if !strings.Contains(err.Error(), "resolve herdr pane") {
		t.Fatalf("error should wrap the pane-resolution context; got %v", err)
	}
	if !strings.Contains(err.Error(), "resolver not wired") {
		t.Fatalf("error should name the unset resolver hook; got %v", err)
	}
	// Resolution fails up front, so herdr must never be invoked.
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Fatalf("herdr binary must not be invoked when resolution fails (log exists: %v)", statErr)
	}
}

// When the hook is wired but reports no live pane (ok=false), resolution fails
// and the error is wrapped with the pane-resolution context — and no text or
// Enter is sent.
func TestPokeHerdrWrapsResolutionError(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, 0)
	stubHerdrPaneResolver(t, "", false)

	err := PokeHerdrTargetContext(context.Background(), "ghost-agent")
	if err == nil {
		t.Fatal("expected error when the resolver reports no live pane")
	}
	if !strings.Contains(err.Error(), "resolve herdr pane") {
		t.Fatalf("error should wrap the pane-resolution context; got %v", err)
	}
	if !strings.Contains(err.Error(), "no pane_id reported") {
		t.Fatalf("error should report the missing pane_id; got %v", err)
	}
	// No text or Enter must be sent when resolution fails.
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Fatalf("herdr binary must not be invoked when resolution fails (log exists: %v)", statErr)
	}
}

func TestPokeHerdrCancelsDuringInterKeySleep(t *testing.T) {
	oldBinary, oldSleep := herdrBinary, pokeSleep
	t.Cleanup(func() { herdrBinary, pokeSleep = oldBinary, oldSleep })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, 0)
	pokeSleep = time.Hour
	stubHerdrPaneResolver(t, "w3:p9", true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- PokeHerdrTargetContext(ctx, "codex-agentchute") }()

	// Wait until `agent send` has run — the resolve + send execs are then done
	// and the goroutine is blocked on the inter-key sleep — so cancelling tests
	// the sleep path deterministically rather than racing an exec.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "agent\tsend\t") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("`agent send` never ran; cannot reach the inter-key sleep")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		// Cancellation can surface two equivalent ways depending on whether the
		// cancel lands during the inter-key sleep (context.Canceled) or while
		// the `agent send` exec is still draining (the killed child reports
		// "signal: killed"). Both prove the cancel propagated and the turn was
		// abandoned before the Enter key event; accept either.
		if err == nil {
			t.Fatal("PokeHerdrTargetContext returned nil after cancellation, want a cancellation error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "context canceled") && !strings.Contains(msg, "signal: killed") {
			t.Fatalf("PokeHerdrTargetContext error = %v, want context canceled or signal: killed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PokeHerdrTargetContext did not return after context cancellation")
	}

	// In neither cancellation path should the submitting Enter key event have
	// been sent.
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "send-keys") {
		t.Fatalf("Enter was sent despite cancellation before the inter-key sleep:\n%q", data)
	}
}
