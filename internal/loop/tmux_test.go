package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPokeTargetContextCancelsSleep(t *testing.T) {
	oldBinary := tmuxBinary
	oldSleep := pokeSleep
	t.Cleanup(func() {
		tmuxBinary = oldBinary
		pokeSleep = oldSleep
	})

	tmuxBinary = "true"
	pokeSleep = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- PokeTargetContext(ctx, "%1")
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PokeTargetContext error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PokeTargetContext did not return after context cancellation")
	}
}

func TestPokeTargetContextSendsTaggedWakePrompt(t *testing.T) {
	oldBinary := tmuxBinary
	oldSleep := pokeSleep
	t.Cleanup(func() {
		tmuxBinary = oldBinary
		pokeSleep = oldSleep
	})

	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	tmuxPath := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + shellSingleQuote(logPath) + "\n"
	if err := os.WriteFile(tmuxPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxBinary = tmuxPath
	pokeSleep = time.Millisecond

	if err := PokeTargetContext(context.Background(), "%1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "-t %1 "+tmuxWakePrompt+"\n") {
		t.Fatalf("tmux log missing wake prompt %q:\n%s", tmuxWakePrompt, got)
	}
	if !strings.Contains(got, "-t %1 Enter\n") {
		t.Fatalf("tmux log missing Enter:\n%s", got)
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
