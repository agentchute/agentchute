package loop

import (
	"context"
	"errors"
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
