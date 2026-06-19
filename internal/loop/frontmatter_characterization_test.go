package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file CHARACTERIZES the current behavior of the three frontmatter
// parsers that historically caused validator/recorder skew (WI-10). It is a
// behavior snapshot, NOT an aspiration: every assertion below documents what
// the code does TODAY. If a future change alters these outputs, that is a
// real behavior change and must be justified, not silently re-baselined.
//
// The three parsers under characterization:
//
//	A. InferSenderFromFrontmatter (inbox.go) — regex `from:` extractor over the
//	   first ---/--- block. Returns (sender, ok); ok=false unless a valid
//	   agent_id `from:` scalar is found. Only the `from:` field is observable.
//	B. parseFrontmatter (registration.go) — STRICT key:value parser. Returns
//	   (fields, body, error). Rejects indentation, non-key:value lines, dup
//	   keys, missing close. Also reached via ValidateMessageFrontmatter, which
//	   runs it as a pure validity GATE over message content.
//	C. ParseMessageFrontmatter (message.go) — LENIENT key:value parser. Returns
//	   map[string]string. Skips comments/blank lines, ignores non-key:value
//	   lines, last-write-wins on dup keys, strips surrounding quotes.
//
// The skew that motivated WI-10: a message is GATED by B (via
// ValidateMessageFrontmatter) but its fields are EXTRACTED by C. If an input
// passes B but C reads it differently (or vice-versa), the recorder and the
// validator disagree.

// fmProbe runs all three parsers over one input and returns a comparable
// snapshot. For A and C we feed the raw content; for B we feed the
// CRLF-normalized text exactly as ValidateMessageFrontmatter does.
type fmProbe struct {
	// A: InferSenderFromFrontmatter
	aSender string
	aOK     bool
	// B: parseFrontmatter (strict). bErr is the error string ("" if nil).
	bErr    string
	bFields map[string]string // scalar values only, for comparison
	bBody   string
	// C: ParseMessageFrontmatter (lenient)
	cMap map[string]string
}

func probeFrontmatter(t *testing.T, content string) fmProbe {
	t.Helper()
	p := fmProbe{}

	// A needs a file on disk.
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	p.aSender, p.aOK = InferSenderFromFrontmatter(path)

	// B (strict).
	fields, body, err := parseFrontmatter(content)
	if err != nil {
		p.bErr = err.Error()
	} else {
		p.bFields = map[string]string{}
		for k, v := range fields {
			p.bFields[k] = v.scalar
		}
		p.bBody = body
	}

	// C (lenient).
	p.cMap = ParseMessageFrontmatter([]byte(content))

	return p
}

// frontmatterFixtures is the shared fixture set covering the dimensions the
// task calls out: quoting, whitespace, indentation, comments, blank lines,
// CRLF, missing close, dup keys, lists, empty values, and `:`/`#` in values.
func frontmatterFixtures() map[string]string {
	return map[string]string{
		"quoted-value": "---\n" +
			`from: "codex"` + "\n" +
			`task: "do the thing"` + "\n" +
			"---\n\nbody\n",
		"single-quoted-value": "---\n" +
			"from: 'codex'\n" +
			"---\n\nbody\n",
		"unquoted-value":       "---\nfrom: codex\ntask: do-thing\n---\n\nbody\n",
		"leading-ws-in-value":  "---\nfrom:    codex\n---\n\nbody\n",
		"trailing-ws-in-value": "---\nfrom: codex   \n---\n\nbody\n",
		"indented-line":        "---\nfrom: codex\n  task: indented\n---\n\nbody\n",
		"comment-line":         "---\n# a comment\nfrom: codex\n---\n\nbody\n",
		"blank-line-in-block":  "---\nfrom: codex\n\ntask: after-blank\n---\n\nbody\n",
		"crlf":                 "---\r\nfrom: codex\r\ntask: t\r\n---\r\n\r\nbody\r\n",
		"missing-close":        "---\nfrom: codex\ntask: t\n\nbody without close\n",
		"dup-key":              "---\nfrom: codex\nfrom: gemini-cli\n---\n\nbody\n",
		"list-value":           "---\nfrom: codex\nworking_repos:\n  - /a\n  - /b\n---\n\nbody\n",
		"empty-value":          "---\nfrom: codex\ntask:\n---\n\nbody\n",
		"value-with-colon":     "---\nfrom: codex\ntask: a: b: c\n---\n\nbody\n",
		"value-with-hash":      "---\nfrom: codex\ntask: trailing # not-a-comment\n---\n\nbody\n",
		"non-keyvalue-line":    "---\nfrom: codex\nthis is just prose\n---\n\nbody\n",
		"ws-around-delim":      "---\nfrom: codex\n  ---  \n\nbody\n",
		"empty-key":            "---\n: value\n---\n\nbody\n",
		"body-only":            "no frontmatter, just body\n",
		"reply-required-true":  "---\nmessage_id: m1\nfrom: codex\nreply_required: true\n---\n\nbody\n",
	}
}

// TestFrontmatterCharacterization_Snapshot drives all three parsers over the
// shared fixtures and emits a comparison table (visible with -v). It does not
// assert; the targeted tests below pin the load-bearing comparisons. Run with
// `go test -run Characterization -v ./internal/loop` to see the table.
func TestFrontmatterCharacterization_Snapshot(t *testing.T) {
	fx := frontmatterFixtures()
	names := make([]string, 0, len(fx))
	for n := range fx {
		names = append(names, n)
	}
	// deterministic order
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		p := probeFrontmatter(t, fx[n])
		bSummary := "ok " + mapStr(p.bFields)
		if p.bErr != "" {
			bSummary = "ERR(" + p.bErr + ")"
		}
		t.Logf("%-22s | A=(%q,%v) | B=%s | C=%s",
			n, p.aSender, p.aOK, bSummary, mapStr(p.cMap))
	}
}

func mapStr(m map[string]string) string {
	if m == nil {
		return "<nil>"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
	}
	b.WriteByte('}')
	return b.String()
}

// --- Targeted characterization assertions (pin current behavior) ---

// strictGate reports whether parseFrontmatter (parser B, the validity gate used
// by ValidateMessageFrontmatter) ACCEPTS the input.
func strictGate(content string) bool {
	_, _, err := parseFrontmatter(content)
	return err == nil
}

// TestChar_StrictRejectsIndentation pins that the strict parser (B) rejects an
// indented line, while the lenient parser (C) silently absorbs it.
func TestChar_StrictRejectsIndentation(t *testing.T) {
	in := "---\nfrom: codex\n  task: indented\n---\n\nbody\n"
	if strictGate(in) {
		t.Fatal("B: expected strict parser to REJECT indented line")
	}
	c := ParseMessageFrontmatter([]byte(in))
	// C trims each line, so "  task: indented" becomes "task: indented".
	if c["task"] != "indented" {
		t.Fatalf("C: want task=indented, got %q (map=%v)", c["task"], c)
	}
	// from is still readable by C and A.
	if c["from"] != "codex" {
		t.Fatalf("C: want from=codex, got %q", c["from"])
	}
}

// TestChar_StrictRejectsNonKeyValueLine pins that a prose line inside the block
// is rejected by B but ignored by C.
func TestChar_StrictRejectsNonKeyValueLine(t *testing.T) {
	in := "---\nfrom: codex\nthis is just prose\n---\n\nbody\n"
	if strictGate(in) {
		t.Fatal("B: expected strict parser to REJECT non-key:value line")
	}
	c := ParseMessageFrontmatter([]byte(in))
	if c["from"] != "codex" {
		t.Fatalf("C: want from=codex, got %q (map=%v)", c["from"], c)
	}
}

// TestChar_StrictRejectsDupKey pins B rejecting a duplicate key; C is
// last-write-wins.
func TestChar_StrictRejectsDupKey(t *testing.T) {
	in := "---\nfrom: codex\nfrom: gemini-cli\n---\n\nbody\n"
	if strictGate(in) {
		t.Fatal("B: expected strict parser to REJECT duplicate key")
	}
	c := ParseMessageFrontmatter([]byte(in))
	if c["from"] != "gemini-cli" {
		t.Fatalf("C: dup key last-write-wins; want gemini-cli, got %q", c["from"])
	}
	// A (regex) matches the FIRST `from:` line.
	a := senderFromContent(t, in)
	if a != "codex" {
		t.Fatalf("A: regex matches first from:; want codex, got %q", a)
	}
}

// TestChar_CommentLine pins that B rejects a `#` comment line (it is not
// key:value) while C explicitly skips it.
func TestChar_CommentLine(t *testing.T) {
	in := "---\n# a comment\nfrom: codex\n---\n\nbody\n"
	if strictGate(in) {
		t.Fatal("B: expected strict parser to REJECT comment line (not key:value)")
	}
	c := ParseMessageFrontmatter([]byte(in))
	if _, ok := c["#"]; ok {
		t.Fatalf("C: comment must be skipped, got map=%v", c)
	}
	if c["from"] != "codex" {
		t.Fatalf("C: want from=codex, got %q", c)
	}
}

// TestChar_ValueWithColon pins that both B and C keep everything after the
// FIRST colon as the value (strings.Cut / IndexByte on first ':').
func TestChar_ValueWithColon(t *testing.T) {
	in := "---\nfrom: codex\ntask: a: b: c\n---\n\nbody\n"
	if !strictGate(in) {
		t.Fatal("B: expected strict parser to ACCEPT value-with-colon")
	}
	fields, _, _ := parseFrontmatter(in)
	if got := fields.scalar("task"); got != "a: b: c" {
		t.Fatalf("B: want task=%q, got %q", "a: b: c", got)
	}
	c := ParseMessageFrontmatter([]byte(in))
	if c["task"] != "a: b: c" {
		t.Fatalf("C: want task=%q, got %q", "a: b: c", c["task"])
	}
}

// TestChar_QuoteStripping pins the divergence in HOW B and C strip quotes.
// B (cleanScalar) uses strconv.Unquote first (interprets escapes) then a
// simple paired-quote strip. C (strings.Trim) strips ANY leading/trailing
// run of quote characters.
func TestChar_QuoteStripping(t *testing.T) {
	// Simple paired double-quote: both agree.
	in := "---\nfrom: codex\ntask: \"hello\"\n---\n\nbody\n"
	fields, _, _ := parseFrontmatter(in)
	c := ParseMessageFrontmatter([]byte(in))
	if fields.scalar("task") != "hello" {
		t.Fatalf("B: want hello, got %q", fields.scalar("task"))
	}
	if c["task"] != "hello" {
		t.Fatalf("C: want hello, got %q", c["task"])
	}

	// A value that is a quote-balanced string with embedded escape: B unquotes
	// (interprets \t), C strips outer quotes only.
	in2 := "---\nfrom: codex\ntask: \"a\\tb\"\n---\n\nbody\n"
	fields2, _, _ := parseFrontmatter(in2)
	c2 := ParseMessageFrontmatter([]byte(in2))
	if fields2.scalar("task") != "a\tb" {
		t.Fatalf("B: strconv.Unquote interprets escape; want a<TAB>b, got %q", fields2.scalar("task"))
	}
	if c2["task"] != "a\\tb" {
		t.Fatalf("C: literal quote-strip keeps backslash; want a\\tb, got %q", c2["task"])
	}
}

// TestChar_NullSentinel pins that B maps `null`/`~` to empty string
// (cleanScalar) while C keeps the literal text.
func TestChar_NullSentinel(t *testing.T) {
	in := "---\nfrom: codex\ntask: null\n---\n\nbody\n"
	fields, _, _ := parseFrontmatter(in)
	c := ParseMessageFrontmatter([]byte(in))
	if fields.scalar("task") != "" {
		t.Fatalf("B: null sentinel -> empty; got %q", fields.scalar("task"))
	}
	if c["task"] != "null" {
		t.Fatalf("C: literal; want null, got %q", c["task"])
	}
}

// TestChar_WhitespaceAroundDelimiter pins that BOTH B and C treat a
// whitespace-padded `---` line as a delimiter (TrimSpace before compare). This
// is the codex-flagged consistency that was already fixed.
func TestChar_WhitespaceAroundDelimiter(t *testing.T) {
	in := "---\nfrom: codex\ntask: t\n  ---  \n\nbody\n"
	if !strictGate(in) {
		t.Fatal("B: padded --- must close the block")
	}
	c := ParseMessageFrontmatter([]byte(in))
	if c["task"] != "t" || c["from"] != "codex" {
		t.Fatalf("C: padded --- must close block; got map=%v", c)
	}
}

// TestChar_MissingClose pins that B ERRORS on a missing closing `---`, while C
// returns an EMPTY map (firstFrontmatterBlock returns ok=false), and A returns
// ok=false. ValidateMessageFrontmatter surfaces B's error.
func TestChar_MissingClose(t *testing.T) {
	in := "---\nfrom: codex\ntask: t\n\nbody without close\n"
	if strictGate(in) {
		t.Fatal("B: missing close must error")
	}
	c := ParseMessageFrontmatter([]byte(in))
	if len(c) != 0 {
		t.Fatalf("C: missing close -> empty map; got %v", c)
	}
	a := senderFromContent(t, in)
	if a != "" {
		t.Fatalf("A: missing close -> no sender; got %q", a)
	}
}

// TestChar_EmptyValueListVsScalar pins the empty-value divergence. B treats a
// bare `task:` as a potential LIST header (scalar empty, list possibly empty);
// C records an empty-string scalar.
func TestChar_EmptyValueListVsScalar(t *testing.T) {
	in := "---\nfrom: codex\ntask:\n---\n\nbody\n"
	fields, _, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("B: unexpected error %v", err)
	}
	if fields.scalar("task") != "" {
		t.Fatalf("B: empty value scalar should be empty, got %q", fields.scalar("task"))
	}
	c := ParseMessageFrontmatter([]byte(in))
	if v, ok := c["task"]; !ok || v != "" {
		t.Fatalf("C: empty value -> empty-string scalar present; got map=%v", c)
	}
}

// TestChar_ListValue pins that B parses `- ` list items into a list field
// (scalar empty), while C records only the HEADER key with an empty value and
// silently treats the `- /a` lines as their own (malformed) keys are NOT —
// actually `- /a` has no colon so C SKIPS them. Document that.
func TestChar_ListValue(t *testing.T) {
	in := "---\nfrom: codex\nworking_repos:\n  - /a\n  - /b\n---\n\nbody\n"
	fields, _, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("B: list parse unexpected error %v", err)
	}
	if got := fields.list("working_repos"); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("B: want [/a /b], got %v", got)
	}
	c := ParseMessageFrontmatter([]byte(in))
	// C: header has empty value; `- /a` lines contain a... no colon? "- /a"
	// has no ':' so IndexByte returns -1 and the line is skipped.
	if v, ok := c["working_repos"]; !ok || v != "" {
		t.Fatalf("C: want working_repos= (empty), got %v (map=%v)", v, c)
	}
	if len(c) != 2 { // from + working_repos
		t.Fatalf("C: list item lines must be skipped (no colon); got map=%v", c)
	}
}

// senderFromContent isolates A's `from:` extraction without ValidateAgentID
// rejecting (we pass a valid slug in tests that use it). It writes to a temp
// file and calls the production function.
func senderFromContent(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "p.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, _ := InferSenderFromFrontmatter(path)
	return s
}
