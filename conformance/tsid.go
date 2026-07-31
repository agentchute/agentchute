package conformance

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// tsid.go models the v2.5 plan B7 wire break: the timestamp+random-suffix
// message identity that REPLACES the per-(sender,recipient) sequence counter
// (SeqSender, deleted). Reimplemented independently here — same posture as
// durability.go — so the suite proves the SPEC's grammar and floor rule, not
// the reference Go implementation's code.
//
//   - stampRE/filenameRE mirror C2/C3 (the wire timestamp format and the
//     canonical filename grammar).
//   - legacySeqRE mirrors the OLD (to,from,seq) filename grammar this
//     replaces — kept here ONLY so DR1 can prove a lister recognizes BOTH
//     grammars during the migration window (dual-read).
//   - TsSender models C7's monotonic per-sender floor + C8's mint-then-
//     deliver lock ordering, replacing SeqSender.

const agentIDPattern = `[a-z0-9][a-z0-9-]*`

var (
	stampRE     = regexp.MustCompile(`^\d{8}T\d{12}Z$`)
	filenameRE  = regexp.MustCompile(`^(\d{8}T\d{12}Z)_from-(` + agentIDPattern + `)_r([0-9a-f]{32})\.md$`)
	legacySeqRE = regexp.MustCompile(`^from-(` + agentIDPattern + `)_seq-(\d{20})\.md$`)
	stampLayout = "20060102T150405"
)

// formatStamp returns the fixed-width, microsecond-precision UTC wire form (C2).
func formatStamp(t time.Time) string {
	t = t.UTC()
	return t.Format(stampLayout) + fmt.Sprintf("%06dZ", t.Nanosecond()/1000)
}

// parseStamp parses the exact fixed-width C2 timestamp wire form.
func parseStamp(stamp string) (time.Time, bool) {
	if !stampRE.MatchString(stamp) {
		return time.Time{}, false
	}
	base, err := time.Parse(stampLayout, stamp[:15])
	if err != nil {
		return time.Time{}, false
	}
	micros, err := strconv.ParseInt(stamp[15:21], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return base.Add(time.Duration(micros) * time.Microsecond), true
}

// tsFilename renders the C3 canonical filename for (from, stamp, suffix).
func tsFilename(from, stamp, suffix string) string {
	return fmt.Sprintf("%s_from-%s_r%s.md", stamp, from, suffix)
}

// parseTsFilename inverts tsFilename (C3). Returns ok=false for anything that
// doesn't match exactly, including a syntactically-shaped but semantically
// invalid stamp or an uppercase-hex/short suffix.
func parseTsFilename(name string) (from, stamp, suffix string, ok bool) {
	m := filenameRE.FindStringSubmatch(name)
	if m == nil {
		return "", "", "", false
	}
	if _, valid := parseStamp(m[1]); !valid {
		return "", "", "", false
	}
	return m[2], m[1], m[3], true
}

// classifyInboxName is DR1's dual-read classifier: a name is either the OLD
// (to,from,seq) grammar, the NEW (v2.5 plan B7) timestamp grammar, or garbage
// — mirroring the reference implementation's single choke point
// (parseAnyInboxName in internal/loop/inbox.go), reimplemented independently
// here per this package's "prove the spec, not the code" posture.
func classifyInboxName(name string) (kind, from string) {
	if m := legacySeqRE.FindStringSubmatch(name); m != nil {
		return "old", m[1]
	}
	if from, _, _, ok := parseTsFilename(name); ok {
		return "new", from
	}
	return "garbage", ""
}

// rand128hex returns a 32-lowercase-hex-char (128-bit) crypto/rand suffix (C4).
func rand128hex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // conformance model: a broken CSPRNG is not a case worth modeling
	}
	return hex.EncodeToString(b[:])
}

// TsSender models C7's monotonic per-sender send floor, replacing SeqSender.
// Unlike the deleted per-(from,to) counter, ONE floor per sender is enough:
// two different senders may legally mint the identical stamp, since their
// messages never share a filename (each carries its OWN from-<id>_...
// identity). Floor is exported so TS2 can construct a FRESH TsSender sharing
// a prior instance's Floor value — modeling "the process restarted and
// re-read the durable floor file" without a separate crash-simulation method.
type TsSender struct {
	From  string
	Floor string // last-issued C2 stamp; "" = never minted.
}

func NewTsSender(from, floor string) *TsSender { return &TsSender{From: from, Floor: floor} }

// Mint applies the C7 rule: stamp = now; if stamp <= floor, stamp = floor +
// 1 microsecond. Write-ahead: the floor is updated BEFORE any delivery is
// attempted, mirroring MintSendStamp's persist-then-attempt-delivery order —
// which is exactly why a crash after Mint returns can never cause a later
// mint (even from a freshly "restarted" TsSender sharing this Floor) to
// reissue at or before this stamp.
func (s *TsSender) Mint(now time.Time) string {
	stamp := formatStamp(now)
	if s.Floor != "" && stamp <= s.Floor {
		floorTime, _ := parseStamp(s.Floor)
		stamp = formatStamp(floorTime.Add(time.Microsecond))
	}
	s.Floor = stamp
	return stamp
}
