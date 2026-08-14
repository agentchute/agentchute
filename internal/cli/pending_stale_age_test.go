package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// pending_stale_age_test.go — the 2026-08-14 stale-mail incident.
//
// A lane woke to mail that had sat unread for ~31h, read it as a live
// instruction, and broadcast a false alarm to the fleet. `check` DID print its
// age banner (that path was already correct), but the wake cue the lane read
// FIRST — pending's UserPromptSubmit hook context — carried only an RFC3339
// timestamp: `pending` marked an entry Stale only when --stale-after was
// passed, no installed hook template passes it, and buildPendingContext then
// dropped even the [stale] marker that emitPendingText already printed.
//
// These tests pin all three halves of the fix: annotation on by default,
// age + [stale] reaching the model-facing hook context, and the norm line.

// hookContext runs cmdPending in the given fixture with --claude-hook and
// returns the additionalContext string the model actually sees.
func hookContext(t *testing.T, root string, extra ...string) string {
	t.Helper()
	var ctx string
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPending(pendingArgs(append([]string{"--claude-hook", "UserPromptSubmit"}, extra...)...))
		})
		if err != nil {
			t.Fatal(err)
		}
		var wrap struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		ctx = wrap.HookSpecificOutput.AdditionalContext
	})
	return ctx
}

// TestPendingHookContextCarriesAgeAndStale is THE regression test for the
// incident: with no flags at all — exactly how the installed hook template
// invokes pending — day-old mail must reach the model as stale, with an age.
func TestPendingHookContextCarriesAgeAndStale(t *testing.T) {
	root, cfg := setupSendFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("claude-code"), "grok", 1,
		[]byte("---\nfrom: grok\n---\n\nsave context, restarting\n"), 31*time.Hour)

	ctx := hookContext(t, root)
	if !strings.Contains(ctx, "[stale]") {
		t.Fatalf("wake cue dropped the [stale] marker for 31h-old mail; got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "(31h ago)") {
		t.Fatalf("wake cue missing the human-readable age; got:\n%s", ctx)
	}
	if !strings.Contains(ctx, staleNorm) {
		t.Fatalf("wake cue missing the stale-mail norm line; got:\n%s", ctx)
	}
}

// TestPendingHookContextFreshMailUnmarked is the other side: mail that really
// is current must not be annotated, or [stale] becomes noise the reader learns
// to skip — the exact failure mode that made the age banner ineffective.
func TestPendingHookContextFreshMailUnmarked(t *testing.T) {
	root, cfg := setupSendFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("claude-code"), "grok", 1,
		[]byte("---\nfrom: grok\n---\n\nhi\n"), 20*time.Minute)

	ctx := hookContext(t, root)
	if strings.Contains(ctx, "[stale]") {
		t.Fatalf("20m-old mail was marked stale; got:\n%s", ctx)
	}
	if strings.Contains(ctx, staleNorm) {
		t.Fatalf("norm line printed with no stale mail; got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "(20m ago)") {
		t.Fatalf("wake cue should still carry the age for fresh mail; got:\n%s", ctx)
	}
}

// TestPendingStaleDefaultMatchesCheckThreshold pins the two surfaces to ONE
// threshold: 23h is under it (matching TestCheckNoBannerJustUnderThreshold),
// 25h is over. Without this, drifting oldMailBannerAfter would silently
// desynchronize the wake cue from the consume banner.
func TestPendingStaleDefaultMatchesCheckThreshold(t *testing.T) {
	for _, c := range []struct {
		name      string
		age       time.Duration
		wantStale bool
	}{
		{"just under 24h", 23 * time.Hour, false},
		{"just over 24h", 25 * time.Hour, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, cfg := setupSendFixture(t)
			mustWriteAgedInbox(t, cfg.AgentInboxDir("claude-code"), "grok", 1,
				[]byte("---\nfrom: grok\n---\n\nhi\n"), c.age)

			ctx := hookContext(t, root)
			if got := strings.Contains(ctx, "[stale]"); got != c.wantStale {
				t.Fatalf("stale = %v, want %v for age %s; got:\n%s", got, c.wantStale, c.age, ctx)
			}
		})
	}
}

// TestPendingStaleAfterOverrides proves the existing flag still wins in both
// directions — no new knob was added, the old one just gained a default.
func TestPendingStaleAfterOverrides(t *testing.T) {
	t.Run("tighter threshold marks fresh mail stale", func(t *testing.T) {
		root, cfg := setupSendFixture(t)
		mustWriteAgedInbox(t, cfg.AgentInboxDir("claude-code"), "grok", 1,
			[]byte("---\nfrom: grok\n---\n\nhi\n"), time.Hour)

		ctx := hookContext(t, root, "--stale-after", "30m")
		if !strings.Contains(ctx, "[stale]") {
			t.Fatalf("--stale-after 30m did not mark 1h-old mail stale; got:\n%s", ctx)
		}
	})

	t.Run("0s disables annotation entirely", func(t *testing.T) {
		root, cfg := setupSendFixture(t)
		mustWriteAgedInbox(t, cfg.AgentInboxDir("claude-code"), "grok", 1,
			[]byte("---\nfrom: grok\n---\n\nhi\n"), 30*24*time.Hour)

		ctx := hookContext(t, root, "--stale-after", "0s")
		if strings.Contains(ctx, "[stale]") {
			t.Fatalf("--stale-after 0s should disable annotation; got:\n%s", ctx)
		}
		if strings.Contains(ctx, staleNorm) {
			t.Fatalf("--stale-after 0s should suppress the norm line; got:\n%s", ctx)
		}
	})
}

// TestPendingTextAndJSONCarryAge covers the two non-hook output modes, so the
// operator-facing and machine-facing views agree with the wake cue.
func TestPendingTextAndJSONCarryAge(t *testing.T) {
	root, cfg := setupSendFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("claude-code"), "grok", 1,
		[]byte("---\nfrom: grok\n---\n\nhi\n"), 3*24*time.Hour)

	withCwd(t, root, func() {
		text, err := captureStdout(t, func() error { return cmdPending(pendingArgs()) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "(3d ago)") || !strings.Contains(text, "[stale]") {
			t.Fatalf("text output missing age or stale marker; got:\n%s", text)
		}
		if !strings.Contains(text, staleNorm) {
			t.Fatalf("text output missing the norm line; got:\n%s", text)
		}

		out, err := captureStdout(t, func() error { return cmdPending(pendingArgs("--json")) })
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Messages []struct {
				Age   string `json:"age"`
				Stale bool   `json:"stale"`
			} `json:"messages"`
		}
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("got %d messages, want 1:\n%s", len(got.Messages), out)
		}
		if got.Messages[0].Age != "3d" || !got.Messages[0].Stale {
			t.Fatalf("JSON entry = %+v, want age 3d and stale true", got.Messages[0])
		}
	})
}
