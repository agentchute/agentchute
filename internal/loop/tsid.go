package loop

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// TsID is the timestamp-based message identity introduced for the v2.5
// migration. The timestamp and suffix together replace the legacy per-pair
// sequence counter.
type TsID struct {
	To     string
	From   string
	Stamp  string
	Suffix string
}

const tsStampLayout = "20060102T150405"

var (
	tsStampRE    = regexp.MustCompile(`^\d{8}T\d{12}Z$`)
	tsSuffixRE   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	tsFilenameRE = regexp.MustCompile(
		`^(\d{8}T\d{12}Z)_from-(` + agentIDPattern + `)_r([0-9a-f]{32})\.md$`,
	)
	tsRefRE = regexp.MustCompile(
		`^to-(` + agentIDPattern + `)_from-(` + agentIDPattern + `)_(\d{8}T\d{12}Z)_r([0-9a-f]{32})$`,
	)
)

// rand128hex returns a 32-lowercase-hex-char (128-bit) crypto/rand suffix
// (C4). 128 bits makes a real collision unreachable; the caller still retries
// on link EEXIST (a same-microsecond sender racing itself, or, astronomically,
// a suffix collision) rather than relying on uniqueness alone. Package var
// (mirrors lease.go's mintServeToken) so tests can force a collision.
var rand128hex = func() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// FormatStamp returns the fixed-width, microsecond-precision UTC wire form.
func FormatStamp(t time.Time) string {
	t = t.UTC()
	return t.Format(tsStampLayout) + fmt.Sprintf("%06dZ", t.Nanosecond()/1000)
}

// ParseStamp parses the exact fixed-width timestamp wire form.
func ParseStamp(stamp string) (time.Time, bool) {
	if !tsStampRE.MatchString(stamp) {
		return time.Time{}, false
	}
	base, err := time.Parse(tsStampLayout, stamp[:15])
	if err != nil {
		return time.Time{}, false
	}
	micros, err := strconv.ParseInt(stamp[15:21], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return base.Add(time.Duration(micros) * time.Microsecond), true
}

// Filename returns the timestamp-format inbox filename.
func (t TsID) Filename() string {
	return fmt.Sprintf("%s_from-%s_r%s%s", t.Stamp, t.From, t.Suffix, inboxFilenameSuffix)
}

// RefString returns the canonical timestamp-format in_reply_to reference.
func (t TsID) RefString() string {
	return fmt.Sprintf("to-%s_from-%s_%s_r%s", t.To, t.From, t.Stamp, t.Suffix)
}

// Equal reports whether two timestamp identities denote the same delivery key.
func (t TsID) Equal(other TsID) bool {
	return t.To == other.To &&
		t.From == other.From &&
		t.Stamp == other.Stamp &&
		t.Suffix == other.Suffix
}

// Validate checks that t's full identity conforms to the C3 grammar (To/From
// as valid agent ids; Stamp as a well-formed C2 timestamp; Suffix as 32
// lowercase hex chars) — the shape a delivery path may safely turn into a
// filename. A caller-supplied identity that fails this MUST be rejected
// before any filepath.Join or filesystem write (codex PR #99 review: an
// unvalidated Stamp/Suffix embedding a path separator or ".." could
// otherwise escape the inbox directory once joined into a path — the same
// class of hazard B3 already closed for a typo'd --to before it could
// manufacture a state dir one layer down).
func (t TsID) Validate() error {
	if err := ValidateAgentID(t.To); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if err := ValidateAgentID(t.From); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if _, ok := ParseStamp(t.Stamp); !ok {
		return fmt.Errorf("stamp %q does not match the C2 wire form", t.Stamp)
	}
	if !tsSuffixRE.MatchString(t.Suffix) {
		return fmt.Errorf("suffix %q does not match the C4 128-bit-hex form", t.Suffix)
	}
	return nil
}

// ParseTsFilename parses a timestamp-format inbox filename. To is not encoded
// in the filename and is therefore left empty in the returned identity.
func ParseTsFilename(name string) (TsID, bool) {
	m := tsFilenameRE.FindStringSubmatch(name)
	if m == nil {
		return TsID{}, false
	}
	if _, ok := ParseStamp(m[1]); !ok {
		return TsID{}, false
	}
	if err := ValidateAgentID(m[2]); err != nil {
		return TsID{}, false
	}
	return TsID{From: m[2], Stamp: m[1], Suffix: m[3]}, true
}

// ParseTsRef parses a timestamp-format in_reply_to reference.
func ParseTsRef(ref string) (TsID, bool) {
	m := tsRefRE.FindStringSubmatch(ref)
	if m == nil {
		return TsID{}, false
	}
	if err := ValidateAgentID(m[1]); err != nil {
		return TsID{}, false
	}
	if err := ValidateAgentID(m[2]); err != nil {
		return TsID{}, false
	}
	if _, ok := ParseStamp(m[3]); !ok {
		return TsID{}, false
	}
	return TsID{To: m[1], From: m[2], Stamp: m[3], Suffix: m[4]}, true
}
