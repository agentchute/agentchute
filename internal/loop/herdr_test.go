package loop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeFakeHerdr installs a fake `herdr` executable on a temp dir. It records
// the first two argv elements (one per line) to logPath, and writes the raw
// 3rd (target) and 4th (prompt) arguments — without a trailing newline — to
// targetPath and promptPath so tests can assert exact bytes, including the
// trailing carriage return. exitCode/stderrMsg drive the error path.
func writeFakeHerdr(t *testing.T, dir, logPath, targetPath, promptPath string, exitCode int, stderrMsg string) string {
	t.Helper()
	herdrPath := filepath.Join(dir, "herdr")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s\\n' \"$1\" \"$2\" >> " + shellSingleQuote(logPath) + "\n")
	b.WriteString("printf '%s' \"$3\" > " + shellSingleQuote(targetPath) + "\n")
	b.WriteString("printf '%s' \"$4\" > " + shellSingleQuote(promptPath) + "\n")
	if stderrMsg != "" {
		b.WriteString("printf '%s' " + shellSingleQuote(stderrMsg) + " 1>&2\n")
	}
	b.WriteString("exit " + strconv.Itoa(exitCode) + "\n")
	if err := os.WriteFile(herdrPath, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return herdrPath
}

func TestPokeHerdrSendsWakePromptWithCarriageReturn(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	targetPath := filepath.Join(dir, "target")
	promptPath := filepath.Join(dir, "prompt")
	herdrBinary = writeFakeHerdr(t, dir, logPath, targetPath, promptPath, 0, "")

	if err := PokeHerdrTargetContext(context.Background(), "codex-agentchute"); err != nil {
		t.Fatal(err)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(log); got != "agent\nsend\n" {
		t.Fatalf("argv[0:2] = %q, want \"agent\\nsend\\n\"", got)
	}

	target, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(target) != "codex-agentchute" {
		t.Fatalf("target arg = %q, want %q", target, "codex-agentchute")
	}

	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != herdrWakePrompt+"\r" {
		t.Fatalf("prompt arg = %q, want %q", prompt, herdrWakePrompt+"\r")
	}
	if len(prompt) == 0 || prompt[len(prompt)-1] != 0x0d {
		t.Fatalf("prompt must end in carriage return 0x0d (submits the turn); got bytes %v", prompt)
	}
}

func TestPokeHerdrEmptyTargetNoOp(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "herdr.log")
	herdrBinary = writeFakeHerdr(t, dir, logPath, filepath.Join(dir, "t"), filepath.Join(dir, "p"), 0, "")

	if err := PokeHerdrTargetContext(context.Background(), "   "); err != nil {
		t.Fatalf("empty target should be a no-op, got %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("herdr binary should not be invoked for an empty target (log exists: %v)", err)
	}
}

func TestPokeHerdrPassesTargetAsSingleArgNoShellEval(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target")
	sentinel := filepath.Join(dir, "pwned")
	// A target laden with shell metacharacters. Because the adapter uses argv
	// exec (never sh -c), this must arrive at the binary as one literal arg and
	// must not execute the embedded command.
	evil := "x\"; touch " + sentinel + " #"
	herdrBinary = writeFakeHerdr(t, dir, filepath.Join(dir, "log"), targetPath, filepath.Join(dir, "p"), 0, "")

	if err := PokeHerdrTargetContext(context.Background(), evil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != evil {
		t.Fatalf("target not passed verbatim as one arg: got %q want %q", got, evil)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("shell metacharacters in target were evaluated — argv safety broken")
	}
}

func TestPokeHerdrWrapsError(t *testing.T) {
	oldBinary := herdrBinary
	t.Cleanup(func() { herdrBinary = oldBinary })

	dir := t.TempDir()
	herdrBinary = writeFakeHerdr(t, dir, filepath.Join(dir, "log"), filepath.Join(dir, "t"), filepath.Join(dir, "p"), 1, "agent target not found")

	err := PokeHerdrTargetContext(context.Background(), "ghost-agent")
	if err == nil {
		t.Fatal("expected error when herdr exits non-zero")
	}
	if !strings.Contains(err.Error(), "herdr agent send") || !strings.Contains(err.Error(), "agent target not found") {
		t.Fatalf("error should wrap herdr context and stderr; got %v", err)
	}
}
