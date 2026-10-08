package loop

import (
	"fmt"
	"strconv"
	"unicode/utf8"
)

// Peer-chosen text printed OUTSIDE a frame — a file name another process put
// in a pool directory, or an error that embeds one — must never start a line
// of its own or carry a control byte to a terminal (gate reviews of #215).
// These helpers live here, below every printer, so an error built in this
// package is safe at every sink, not only the ones that remember to escape.

// MaxPeerNameRunes caps a peer-chosen name printed outside a frame.
const MaxPeerNameRunes = 256

// MaxPeerErrorRunes caps an error text that embeds peer-chosen names.
const MaxPeerErrorRunes = 1024

// QuotedCapped renders s as ONE physical line: Go-quoted, so every line
// break, control byte, Unicode separator and bidi control is an escape, and
// capped at max runes with the original length noted.
func QuotedCapped(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return strconv.Quote(s)
	}
	r := []rune(s)
	return fmt.Sprintf("%s… (%d characters)", strconv.Quote(string(r[:max])), len(r))
}

// OneLine renders peer-controlled text for a line of program output that is
// NOT framed: unchanged when it is valid UTF-8, at most max runes, and every
// rune printable (strconv.IsPrint); otherwise QuotedCapped.
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

// oneLineError renders its wrapped error through OneLine; errors.Is and
// errors.As still see the wrapped error.
type oneLineError struct{ err error }

func (e oneLineError) Error() string { return OneLine(e.err.Error(), MaxPeerErrorRunes) }
func (e oneLineError) Unwrap() error { return e.err }

// OneLineError wraps err (nil stays nil) so its text is one bounded line:
// for errors whose text embeds a path some peer chose (an os.PathError or
// LinkError on an inbox or agents entry).
func OneLineError(err error) error {
	if err == nil {
		return nil
	}
	return oneLineError{err}
}
