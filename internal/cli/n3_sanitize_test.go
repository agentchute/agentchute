package cli

import (
	"strings"
	"testing"
)

// n3_sanitize_test.go — N3 (deep-analysis-v2): raw message bodies and other
// peer-controlled display text reach a terminal unsanitized. Control bytes
// (ESC-driven ANSI/OSC sequences, C1 codes, bare CR) let a peer repaint the
// operator's screen, spoof a prompt, or set the window title. Fix: strip C0/C1
// control code points from peer-controlled text before it is printed on the
// human-facing text path, unconditionally (no stdout-TTY gating — bodies are
// spec'd UTF-8 free-form text, so control sequences are never legitimate
// payload). \n and \t are the only control code points kept.

// TestCheckSanitizesControlBytesInConsumedBody is the load-bearing N3 test for
// the primary exposure: `check`'s human-facing display of a consumed message
// body (printConsumedBody, check.go). An ANSI/OSC/C0-laden body must print
// inert — the escape/control bytes stripped, the human-readable text intact.
func TestCheckSanitizesControlBytesInConsumedBody(t *testing.T) {
	root, _ := setupConsumeFixture(t)

	// A body carrying: CSI color escape (ESC [ ... m), an OSC window-title
	// sequence terminated by BEL, a bare CR (overwrite trick), and a C1 CSI
	// byte (0x9B) with no ESC prefix — plus plain visible text and a real
	// newline that must survive.
	evilBody := "line one\x1b[31mRED\x1b[0m\r" +
		"\x1b]0;pwned\x07" +
		"\x9bevil-c1" +
		"\nline two"

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "bob", "--to", "alice", "--body", evilBody}); err != nil {
			t.Fatal(err)
		}
	})

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})

	for _, bad := range []string{"\x1b", "\r", "\x9b", "\x07"} {
		if strings.Contains(out, bad) {
			t.Fatalf("consumed body output still contains raw control byte %q; got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "RED") || !strings.Contains(out, "line two") {
		t.Fatalf("sanitization dropped legitimate visible text; got:\n%q", out)
	}
	// \n must survive stripping even though it is itself a control code
	// point: it sits immediately before "line two" in evilBody, with only
	// stripped bytes in between, so it must still sit immediately before
	// "line two" in the sanitized output.
	if !strings.Contains(out, "\nline two") {
		t.Fatalf("sanitization dropped \\n; got:\n%q", out)
	}
}

// TestSanitizeControlBytes pins the exact keep/drop boundary of the helper
// underlying the test above: \n and \t are the only control code points
// kept; all other C0, DEL, and C1 code points are dropped; ordinary text
// (including multi-byte UTF-8 and invalid UTF-8) passes through unchanged
// or degrades safely.
func TestSanitizeControlBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "hello world", "hello world"},
		{"newline and tab kept", "a\nb\tc", "a\nb\tc"},
		{"CR dropped", "a\rb", "ab"},
		{"ESC dropped", "a\x1bb", "ab"},
		{"BEL dropped", "a\x07b", "ab"},
		{"DEL dropped", "a\x7fb", "ab"},
		{"C1 CSI rune dropped (valid UTF-8 encoding of U+009B)", "a\u009bb", "ab"},
		// A bare 0x9b byte on its own is invalid UTF-8 (C1 requires the
		// 2-byte encoding \xc2\x9b to be valid UTF-8, matching the previous
		// case). Go's `range` over invalid UTF-8 yields U+FFFD before the
		// filter's C1 range check ever sees byte 0x9b \u2014 so it never passes
		// through raw either way, it just surfaces as the inert replacement
		// character instead of being silently dropped. Pin that behavior.
		{"invalid UTF-8 byte neutralized to U+FFFD, never passed through raw", "a\x9bb", "a\ufffdb"},
		{"C1 range boundary kept just outside", "a\xc2\xa0b", "a\u00a0b"}, // U+00A0 NBSP, just past C1 (U+009F)
		{"multi-byte UTF-8 preserved", "héllo 日本語", "héllo 日本語"},
		{"full ANSI CSI sequence stripped to bare text", "\x1b[31mRED\x1b[0m", "[31mRED[0m"},
		{"empty string", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeControlBytes(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeControlBytes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
