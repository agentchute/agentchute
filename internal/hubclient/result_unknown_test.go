package hubclient

import (
	"errors"
	"strings"
	"testing"
)

// #171 — a one-shot that fails AFTER the hub committed and streamed its work.
//
// The hub commits the claim and streams it, the client prints all of it, and
// then the terminal frame is lost. The operator sees their messages followed by
// "channel to the hub was lost", does the obvious thing, and at-least-once
// re-delivers what they were just shown. Nothing is lost — the safety property
// holds exactly as designed. What was wrong is the report: two situations
// calling for opposite actions rendered identically.
func TestResultIsUnknownOnlyWhenSomethingWasAlreadyStreamed(t *testing.T) {
	lost := &Error{Code: "E_CHANNEL_LOST", Msg: "hub: channel to the hub was lost", Retriable: true}

	t.Run("nothing streamed keeps its classification", func(t *testing.T) {
		// The answer IS known here: nothing happened, and "re-run" is correct
		// advice. Re-coding this would trade a false failure for a false doubt.
		got := resultUnknownIfStreamed(lost, 0)
		if got != error(lost) {
			t.Fatalf("a drop with no streamed output was re-coded: %v", got)
		}
	})

	t.Run("streamed output makes the result unknown", func(t *testing.T) {
		got := resultUnknownIfStreamed(lost, 3)
		if ErrorCode(got) != "E_RESULT_UNKNOWN" {
			t.Fatalf("code = %s, want E_RESULT_UNKNOWN", ErrorCode(got))
		}
		var e *Error
		if !errors.As(got, &e) {
			t.Fatalf("not a hub error: %T", got)
		}
		// NOT retriable, for the same reason E_SEND_UNKNOWN is not: the safe
		// action is to look, not to repeat.
		if e.Retriable {
			t.Fatal("marked retriable — a blind re-run is exactly what duplicates the output")
		}
		if !errors.Is(errors.Unwrap(e), error(lost)) && e.Cause != error(lost) {
			t.Fatal("the original classification was discarded rather than carried")
		}

		text := got.Error()
		// It must say all three things, because any one alone misleads.
		for _, want := range []string{
			"did happen",  // what was printed is real
			"may have",    // the hub may have committed
			"re-delivers", // a re-run duplicates
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("message is missing %q:\n%s", want, text)
			}
		}
		// And it must not assert the failure the operator would otherwise infer.
		if strings.Contains(text, "channel to the hub was lost") {
			t.Fatalf("re-used the bare-failure sentence this exists to replace:\n%s", text)
		}
		// It names how many items were already shown, so the operator can match
		// it against what is on their screen.
		if !strings.Contains(text, "3 item") {
			t.Fatalf("does not say how much was already delivered:\n%s", text)
		}
	})

	t.Run("a nil error stays nil", func(t *testing.T) {
		if got := resultUnknownIfStreamed(nil, 5); got != nil {
			t.Fatalf("invented an error from a successful call: %v", got)
		}
	})
}
