package loop

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

const testTsSuffix = "9f2c04aa71de4b02b6d1c33f08e95a17"

func TestTsIDWorkedExampleRoundTrips(t *testing.T) {
	id := TsID{
		To:     "claude-code",
		From:   "codex-agentchute",
		Stamp:  "20260730T182415123456Z",
		Suffix: testTsSuffix,
	}
	filename := "20260730T182415123456Z_from-codex-agentchute_r" + testTsSuffix + ".md"
	ref := "to-claude-code_from-codex-agentchute_20260730T182415123456Z_r" + testTsSuffix

	if got := id.Filename(); got != filename {
		t.Fatalf("Filename() = %q, want %q", got, filename)
	}
	parsedFilename, ok := ParseTsFilename(filename)
	if !ok {
		t.Fatal("worked filename did not parse")
	}
	if parsedFilename.From != id.From || parsedFilename.Stamp != id.Stamp || parsedFilename.Suffix != id.Suffix {
		t.Fatalf("ParseTsFilename = %+v, want filename fields from %+v", parsedFilename, id)
	}

	if got := id.RefString(); got != ref {
		t.Fatalf("RefString() = %q, want %q", got, ref)
	}
	parsedRef, ok := ParseTsRef(ref)
	if !ok || !parsedRef.Equal(id) {
		t.Fatalf("ParseTsRef = (%+v, %v), want (%+v, true)", parsedRef, ok, id)
	}
}

func TestFormatParseStampRoundTrip(t *testing.T) {
	cases := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 30, 18, 24, 15, 123456789, time.FixedZone("offset", -5*60*60)),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	for _, input := range cases {
		stamp := FormatStamp(input)
		if len(stamp) != 22 {
			t.Fatalf("FormatStamp(%s) length = %d, want 22: %q", input, len(stamp), stamp)
		}
		got, ok := ParseStamp(stamp)
		if !ok {
			t.Fatalf("ParseStamp(%q) failed", stamp)
		}
		want := input.UTC().Truncate(time.Microsecond)
		if !got.Equal(want) {
			t.Fatalf("ParseStamp(FormatStamp(%s)) = %s, want %s", input, got, want)
		}
	}
}

func TestFormatStampLexicographicSortIsChronological(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	times := make([]time.Time, 500)
	for i := range times {
		times[i] = base.Add(time.Duration(rng.Int63n(int64(20 * 365 * 24 * time.Hour)))).Truncate(time.Microsecond)
	}
	chronological := append([]time.Time(nil), times...)
	sort.Slice(chronological, func(i, j int) bool { return chronological[i].Before(chronological[j]) })
	stamps := make([]string, len(times))
	for i, value := range times {
		stamps[i] = FormatStamp(value)
	}
	sort.Strings(stamps)
	for i, stamp := range stamps {
		if want := FormatStamp(chronological[i]); stamp != want {
			t.Fatalf("sorted stamp[%d] = %q, want chronological %q", i, stamp, want)
		}
	}
}

func TestTsIDParsersRejectNonCanonicalForms(t *testing.T) {
	validStamp := "20260730T182415123456Z"
	validFilename := validStamp + "_from-codex_r" + testTsSuffix + ".md"
	validRef := "to-claude-code_from-codex_" + validStamp + "_r" + testTsSuffix

	filenameCases := []string{
		"",
		"2026-07-30T18:24:15.123456Z_from-codex_r" + testTsSuffix + ".md",
		"20260730T18241512345Z_from-codex_r" + testTsSuffix + ".md",
		validStamp + "_from-codex_r9F2c04aa71de4b02b6d1c33f08e95a17.md",
		validStamp + "_from-codex_r9f2c.md",
		validStamp + "_from-CODEX_r" + testTsSuffix + ".md",
		"20260230T182415123456Z_from-codex_r" + testTsSuffix + ".md",
		validFilename + ".md",
	}
	for _, value := range filenameCases {
		if _, ok := ParseTsFilename(value); ok {
			t.Errorf("ParseTsFilename(%q) accepted non-canonical value", value)
		}
	}

	refCases := []string{
		"",
		"to-claude-code_from-codex_2026-07-30T18:24:15.123456Z_r" + testTsSuffix,
		"to-claude-code_from-codex_" + validStamp + "_r9F2c04aa71de4b02b6d1c33f08e95a17",
		"to-claude-code_from-codex_" + validStamp + "_r9f2c",
		"to-Claude_from-codex_" + validStamp + "_r" + testTsSuffix,
		validRef + "_extra",
	}
	for _, value := range refCases {
		if _, ok := ParseTsRef(value); ok {
			t.Errorf("ParseTsRef(%q) accepted non-canonical value", value)
		}
	}
}

func TestParseStampRejectsNonCanonicalForms(t *testing.T) {
	for _, stamp := range []string{
		"",
		"20260730T18241512345Z",
		"20260730T1824151234567Z",
		"2026-07-30T18:24:15.123456Z",
		"20260230T182415123456Z",
		"20260730t182415123456Z",
	} {
		if _, ok := ParseStamp(stamp); ok {
			t.Errorf("ParseStamp(%q) accepted non-canonical value", stamp)
		}
	}
}
