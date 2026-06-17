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
// appends its argv (tab-separated, one line per call) to logPath. When
// respondJSON is set, `agent get` emits a pane_id JSON document on stdout so
// the poke's name→pane resolution succeeds. exitCode drives the error path.
func writeFakeHerdr(t *testing.T, dir, logPath string, respondJSON bool, exitCode int) string {
	getResponse := ""
	if respondJSON {
		getResponse = `{"result":{"agent":{"pane_id":"w3:p9"}}}`
	}
	return writeFakeHerdrWithGetResponse(t, dir, logPath, getResponse, exitCode)
}

// writeFakeHerdrWithGetResponse is like writeFakeHerdr but lets a test control
// the exact stdout `agent get` emits, to exercise malformed/empty resolution.
func writeFakeHerdrWithGetResponse(t *testing.T, dir, logPath, getResponse string, exitCode int) string {
	t.Helper()
	herdrPath := filepath.Join(dir, "herdr")
	respPath := filepath.Join(dir, "getresp")
	if err := os.WriteFile(respPath, []byte(getResponse), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s\\t' \"$@\" >> " + shellSingleQuote(logPath) + "\n")
	b.WriteString("printf '\\n' >> " + shellSingleQuote(logPath) + "\n")
	// `cat` the canned response file so arbitrary JSON needs no shell escaping.
	b.WriteString("if [ \"$1\" = agent ] && [ \"$2\" = get ]; then cat " + shellSingleQuote(respPath) + "; fi\n")
	b.WriteString("exit " + strconv.Itoa(exitCode) + "\n")
	if err := os.WriteFile(herdrPath, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return herdrPath
}

// The herdr poke must (1) resolve the agent name to a pane via `agent get`,
// (2) deliver the wake prompt as literal text WITHOUT a trailing CR, and
// (3) submit via a real `pane send-keys <pane> Enter` key event. The old
// `agent send "<text>\r"` shape never submitted, because herdr `agent send`
// writes literal text and the recipient TUI submits on an Enter key event.
func TestPokeHerdrResolvesPaneAndSendsEnterKeyEvent(t *testing.T) {
	oldBinary, oldSleep := herdrBinary, pokeSleep
	t.Cleanup(func() { herdrBinary, pokeSleep = oldBinary, oldSleep })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, true, 0)
	pokeSleep = time.Millisecond

	if err := PokeHerdrTargetContext(context.Background(), "codex-agentchute"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	if !strings.Contains(got, "agent\tget\tcodex-agentchute\t") {
		t.Errorf("herdr log missing pane resolution (`agent get`):\n%q", got)
	}
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
	herdrBinary = writeFakeHerdr(t, dir, logPath, true, 0)

	if err := PokeHerdrTargetContext(context.Background(), "   "); err != nil {
		t.Fatalf("empty target should be a no-op, got %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("herdr binary should not be invoked for an empty target (log exists: %v)", err)
	}
}

func TestPokeHerdrPassesArgsAsSingleArgsNoShellEval(t *testing.T) {
	oldBinary, oldSleep := herdrBinary, pokeSleep
	t.Cleanup(func() { herdrBinary, pokeSleep = oldBinary, oldSleep })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	sentinel := filepath.Join(dir, "pwned")
	// A target laden with shell metacharacters. Because the adapter uses argv
	// exec (never sh -c), it must arrive verbatim as one argument and must not
	// execute the embedded command.
	evil := "x\"; touch " + sentinel + " #"
	herdrBinary = writeFakeHerdr(t, dir, logPath, true, 0)
	pokeSleep = time.Millisecond

	if err := PokeHerdrTargetContext(context.Background(), evil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "agent\tget\t"+evil+"\t") {
		t.Fatalf("evil target not passed verbatim as one arg:\n%q", got)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("shell metacharacters in target were evaluated — argv safety broken")
	}
}

func TestPokeHerdrWrapsResolutionError(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	// agent get exits non-zero (no JSON) → resolution fails before any send.
	herdrBinary = writeFakeHerdr(t, dir, filepath.Join(dir, "herdr.log"), false, 1)

	err := PokeHerdrTargetContext(context.Background(), "ghost-agent")
	if err == nil {
		t.Fatal("expected error when herdr `agent get` fails")
	}
	if !strings.Contains(err.Error(), "resolve herdr pane") {
		t.Fatalf("error should wrap the pane-resolution context; got %v", err)
	}
}

func TestPokeHerdrRejectsMalformedResolution(t *testing.T) {
	cases := []struct {
		name     string
		response string
		exitCode int
	}{
		{"malformed JSON", "not json{", 0},
		{"valid JSON, empty pane_id", `{"result":{"agent":{"pane_id":""}}}`, 0},
		{"valid JSON, no pane_id field", `{"result":{"agent":{}}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldBinary := herdrBinary
			t.Cleanup(func() { herdrBinary = oldBinary })

			dir := t.TempDir()
			logPath := filepath.Join(dir, "herdr.log")
			herdrBinary = writeFakeHerdrWithGetResponse(t, dir, logPath, tc.response, tc.exitCode)

			err := PokeHerdrTargetContext(context.Background(), "codex-agentchute")
			if err == nil {
				t.Fatal("expected resolution error, got nil")
			}
			if !strings.Contains(err.Error(), "resolve herdr pane") {
				t.Fatalf("error should wrap pane-resolution context; got %v", err)
			}
			// No text or Enter must be sent when resolution fails.
			if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "agent\tsend") || strings.Contains(string(data), "send-keys") {
				t.Fatalf("must not send text/Enter after failed resolution:\n%q", data)
			}
		})
	}
}

func TestPokeHerdrCancelsDuringInterKeySleep(t *testing.T) {
	oldBinary, oldSleep := herdrBinary, pokeSleep
	t.Cleanup(func() { herdrBinary, pokeSleep = oldBinary, oldSleep })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, true, 0)
	pokeSleep = time.Hour

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
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("PokeHerdrTargetContext error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PokeHerdrTargetContext did not return after context cancellation")
	}
}
