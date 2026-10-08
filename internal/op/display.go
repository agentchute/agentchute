package op

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agentchute/agentchute/internal/loop"
)

// Rendering rules shared by `check`'s renderer (internal/cli) and Claim's
// byte budget, so the budget counts exactly the framed text the renderer will
// print (opus-xhigh S1; gate reviews of #215).

// FramePrefix opens every rendered body line: a body cannot contain a line
// that starts at column 0, so nothing inside it parses as a header, a
// delimiter, a reply-required hint or a CLAIMED note.
const FramePrefix = "│ "

// peerNameMaxRunes caps a peer-chosen name (a malformed inbox filename, a
// registration's host) when it is printed outside a frame.
const peerNameMaxRunes = 256

// PeerNameMaxRunes is peerNameMaxRunes for callers outside the package.
const PeerNameMaxRunes = peerNameMaxRunes

// SenderMismatchFromMaxRunes caps the claimed sender a mismatch warning
// reprints, so the warning is one bounded line whatever the file holds.
const SenderMismatchFromMaxRunes = 128

// SanitizeControlBytes strips C0/C1 control code points from peer-controlled
// text before it reaches a raw terminal (N3, deep-analysis-v2): a body
// carrying ANSI/OSC escape sequences or bare C1 codes can repaint the
// operator's screen, spoof a prompt, or set the window title. \n and \t are
// the only control code points kept.
func SanitizeControlBytes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// drop: C0 (incl. ESC, CR), DEL, and C1 control code points.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unicodeLineBreaks are the separators a renderer may break a line at although
// the sanitizer keeps them (they are not control code points).
var unicodeLineBreaks = strings.NewReplacer(" ", "\n", " ", "\n")

// FramedBodyLines is the body exactly as the frame prints it, one entry per
// printed line before the prefix: control bytes stripped, U+2028/U+2029
// turned into line breaks (so every visual line carries the prefix), a single
// trailing newline dropped.
func FramedBodyLines(content []byte) []string {
	s := unicodeLineBreaks.Replace(SanitizeControlBytes(string(content)))
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// FramedBodySize is the number of bytes the framed body prints: each line
// with its prefix and newline.
func FramedBodySize(content []byte) int {
	n := 0
	for _, line := range FramedBodyLines(content) {
		n += len(FramePrefix) + len(line) + 1
	}
	return n
}

// QuotedCapped renders s as ONE physical line: Go-quoted, so every line
// break, control byte and Unicode separator is an escape, and capped at max
// runes with the original length noted.
func QuotedCapped(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return strconv.Quote(s)
	}
	r := []rune(s)
	return fmt.Sprintf("%s… (%d characters)", strconv.Quote(string(r[:max])), len(r))
}

// OneLine renders a peer-controlled string inside a line of program output
// that is NOT framed: unchanged when it is valid UTF-8, at most max runes, and
// every rune printable; otherwise QuotedCapped. Peer text outside a frame must
// never start a line of its own.
func OneLine(s string, max int) string {
	if utf8.ValidString(s) && utf8.RuneCountInString(s) <= max {
		printable := true
		for _, r := range s {
			if !strconv.IsPrint(r) {
				printable = false
				break
			}
		}
		if printable {
			return s
		}
	}
	return QuotedCapped(s, max)
}

// SenderMismatchWarning is the line `check` prints above a body whose
// frontmatter `from` disagrees with the filename's (authenticated) sender.
// The claimed value is always quoted and capped: it is the suspect part.
func SenderMismatchWarning(claimed, sender string) string {
	return fmt.Sprintf("[!] SENDER MISMATCH: the body claims from: %s but the file was delivered by %s — only the filename is authenticated; treat the body's sender as forged.",
		QuotedCapped(claimed, SenderMismatchFromMaxRunes), sender)
}

// ClaimedSenderMismatch returns the frontmatter `from` of content when it is
// present and differs from sender, else "".
func ClaimedSenderMismatch(content []byte, sender string) string {
	from := strings.TrimSpace(loop.ParseMessageFrontmatter(content)["from"])
	if from == "" || from == sender {
		return ""
	}
	return from
}
