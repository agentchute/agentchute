package loop

import "testing"

// This file CHARACTERIZES the behavior of the single frontmatter parser
// (parseFrontmatter, registration.go) as seen through the two message-level
// entry points that wrap it: ValidateMessageFrontmatter (the §11.1 malformed
// gate) and ParseMessageFrontmatter (the field extractor). It is a behavior
// snapshot, NOT an aspiration: every assertion below documents what the code
// does TODAY. If a future change alters these outputs, that is a real
// behavior change and must be justified, not silently re-baselined.
//
// History: originally characterized THREE parsers. Parser A
// (InferSenderFromFrontmatter) was deleted in v2.5 plan A6 along with its
// sole production caller. Parser C (ParseMessageFrontmatter's own ad-hoc
// lenient scanner) was deleted in v2.5 plan B8 — it now routes through
// parseFrontmatter (parser B) like everything else in this package, closing
// the validator/recorder skew WI-10 named (a message could be GATED one way
// by B and have its fields EXTRACTED a different way by C). There is now
// exactly one grammar; the fixtures below are its accept/reject vectors,
// promoted to conformance/vectors/core.json as FM1/FM2.
//
// B8 REAL BEHAVIOR CHANGES for message frontmatter (not just the
// plan-sanctioned dup-key/indentation tightening) — see the PR body for the
// full list:
//   - `#` comment lines and other non-key:value prose lines now reject the
//     WHOLE block (previously silently skipped by parser C).
//   - An empty-key line (`: value`) now rejects the whole block (previously
//     recorded under an empty-string key).
//   - Quoted values with backslash escapes are now unquoted via
//     strconv.Unquote (previously a blunt strings.Trim left escapes literal).
//   - `null`/`~` values now collapse to empty string (previously kept as
//     literal text).

// frontmatterFixtures is the shared fixture set covering the dimensions the
// grammar cares about: quoting, whitespace, indentation, comments, blank
// lines, CRLF, missing close, dup keys, lists, empty values, and `:`/`#` in
// values. This is the FM1/FM2 vector source (conformance/vectors/core.json).
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

		// Added after codex's second-round finding on PR #100: the §6.4
		// grammar block claimed a key charset (ALPHA/DIGIT/_) and a fixed
		// 2-space list indent that NEITHER implementation actually enforces,
		// and FM1 never pinned the escaped-quote / null-sentinel RESULT
		// values (only that the block parsed). These fixtures close both gaps.
		"unusual-key-chars":     "---\nweird key-name!: value\nfrom: codex\n---\n\nbody\n",
		"list-zero-indent":      "---\nfrom: codex\nworking_repos:\n- /a\n- /b\n---\n\nbody\n",
		"list-tab-indent":       "---\nfrom: codex\nworking_repos:\n\t- /a\n\t- /b\n---\n\nbody\n",
		"quoted-value-escaped":  "---\nfrom: codex\ntask: \"a\\tb\"\n---\n\nbody\n",
		"null-value":            "---\nfrom: codex\ntask: null\n---\n\nbody\n",
		"tilde-value":           "---\nfrom: codex\ntask: ~\n---\n\nbody\n",
		"list-item-missing-gap": "---\nfrom: codex\nworking_repos:\n-nospace\n---\n\nbody\n",
	}
}

// frontmatterAcceptCases is FM1: fixture name -> the exact flat field map
// ParseMessageFrontmatter must return once ValidateMessageFrontmatter has
// confirmed the block is well-formed. "body-only" has no frontmatter block
// at all, which is valid-but-empty per §6.4, not a rejection.
func frontmatterAcceptCases() map[string]map[string]string {
	return map[string]map[string]string{
		"quoted-value":         {"from": "codex", "task": "do the thing"},
		"single-quoted-value":  {"from": "codex"},
		"unquoted-value":       {"from": "codex", "task": "do-thing"},
		"leading-ws-in-value":  {"from": "codex"},
		"trailing-ws-in-value": {"from": "codex"},
		"blank-line-in-block":  {"from": "codex", "task": "after-blank"},
		"crlf":                 {"from": "codex", "task": "t"},
		"list-value":           {"from": "codex", "working_repos": ""},
		"empty-value":          {"from": "codex", "task": ""},
		"value-with-colon":     {"from": "codex", "task": "a: b: c"},
		"value-with-hash":      {"from": "codex", "task": "trailing # not-a-comment"},
		"ws-around-delim":      {"from": "codex"},
		"body-only":            {},
		"reply-required-true":  {"message_id": "m1", "from": "codex", "reply_required": "true"},

		"unusual-key-chars":    {"weird key-name!": "value", "from": "codex"},
		"list-zero-indent":     {"from": "codex", "working_repos": ""},
		"list-tab-indent":      {"from": "codex", "working_repos": ""},
		"quoted-value-escaped": {"from": "codex", "task": "a\tb"},
		"null-value":           {"from": "codex", "task": ""},
		"tilde-value":          {"from": "codex", "task": ""},
	}
}

// frontmatterRejectCases is FM2: fixture names whose block
// ValidateMessageFrontmatter must error on, and whose ParseMessageFrontmatter
// must therefore return an empty map.
func frontmatterRejectCases() []string {
	return []string{
		"indented-line",
		"comment-line",
		"missing-close",
		"dup-key",
		"non-keyvalue-line",
		"empty-key",
		"list-item-missing-gap",
	}
}

// TestFrontmatterFixtures_AllClassified guards against a fixture being added
// without an FM1/FM2 classification (or a classification without a fixture).
func TestFrontmatterFixtures_AllClassified(t *testing.T) {
	fx := frontmatterFixtures()
	classified := map[string]bool{}
	for name := range frontmatterAcceptCases() {
		classified[name] = true
	}
	for _, name := range frontmatterRejectCases() {
		classified[name] = true
	}
	for name := range fx {
		if !classified[name] {
			t.Errorf("fixture %q has no FM1/FM2 classification", name)
		}
	}
	for name := range classified {
		if _, ok := fx[name]; !ok {
			t.Errorf("classified case %q has no fixture", name)
		}
	}
}

// TestFrontmatterAccept_FM1 drives the accept table: ValidateMessageFrontmatter
// must not error, and ParseMessageFrontmatter must return exactly the
// documented field map.
func TestFrontmatterAccept_FM1(t *testing.T) {
	fx := frontmatterFixtures()
	for name, want := range frontmatterAcceptCases() {
		in, ok := fx[name]
		if !ok {
			t.Fatalf("fixture %q not found", name)
		}
		if err := ValidateMessageFrontmatter([]byte(in)); err != nil {
			t.Fatalf("%s: expected ACCEPT, got error: %v", name, err)
		}
		got := ParseMessageFrontmatter([]byte(in))
		if len(got) != len(want) {
			t.Fatalf("%s: field count mismatch: got %v, want %v", name, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: field %q: got %q, want %q (map=%v)", name, k, got[k], v, got)
			}
		}
	}
}

// TestFrontmatterReject_FM2 drives the reject table: ValidateMessageFrontmatter
// must error, and ParseMessageFrontmatter must return an empty map (the two
// entry points can no longer disagree — this is the WI-10 fix).
func TestFrontmatterReject_FM2(t *testing.T) {
	fx := frontmatterFixtures()
	for _, name := range frontmatterRejectCases() {
		in, ok := fx[name]
		if !ok {
			t.Fatalf("fixture %q not found", name)
		}
		if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
			t.Fatalf("%s: expected REJECT, got no error", name)
		}
		if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
			t.Fatalf("%s: rejected block must extract as empty map, got %v", name, got)
		}
	}
}

// --- Focused characterization tests (named, for revert-verification) ---

// TestChar_IndentedLineRejected pins the plan-sanctioned tightening: an
// indented continuation line now rejects the whole block. Before B8,
// ParseMessageFrontmatter silently absorbed it as its own key.
func TestChar_IndentedLineRejected(t *testing.T) {
	in := "---\nfrom: codex\n  task: indented\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
		t.Fatal("expected indented continuation line to be REJECTED")
	}
	if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
		t.Fatalf("rejected block must extract as empty map, got %v", got)
	}
}

// TestChar_NonKeyValueLineRejected pins that a prose line inside the block
// (no colon) rejects the whole block. Before B8, ParseMessageFrontmatter
// silently skipped it.
func TestChar_NonKeyValueLineRejected(t *testing.T) {
	in := "---\nfrom: codex\nthis is just prose\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
		t.Fatal("expected non-key:value line to be REJECTED")
	}
	if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
		t.Fatalf("rejected block must extract as empty map, got %v", got)
	}
}

// TestChar_DupKeyRejected pins the plan-sanctioned tightening: a duplicate
// key now rejects the whole block. Before B8, ParseMessageFrontmatter was
// last-write-wins.
func TestChar_DupKeyRejected(t *testing.T) {
	in := "---\nfrom: codex\nfrom: gemini-cli\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
		t.Fatal("expected duplicate key to be REJECTED")
	}
	if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
		t.Fatalf("rejected block must extract as empty map, got %v", got)
	}
}

// TestChar_CommentLineRejected pins a REAL (not plan-named) B8 behavior
// change: a `#` comment line now makes the whole block unparseable, since
// parseFrontmatter has no comment syntax and message frontmatter is no
// longer more lenient about this than registration frontmatter. Before B8,
// ParseMessageFrontmatter silently skipped the comment and still recorded
// `from`.
func TestChar_CommentLineRejected(t *testing.T) {
	in := "---\n# a comment\nfrom: codex\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
		t.Fatal("expected comment line to be REJECTED (no comment syntax in the single engine)")
	}
	if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
		t.Fatalf("rejected block must extract as empty map (from is NO LONGER recovered), got %v", got)
	}
}

// TestChar_EmptyKeyRejected pins another REAL B8 behavior change: a line
// with nothing before the colon now rejects the whole block. Before B8,
// ParseMessageFrontmatter recorded a spurious empty-string-keyed entry.
func TestChar_EmptyKeyRejected(t *testing.T) {
	in := "---\n: value\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
		t.Fatal("expected empty key to be REJECTED")
	}
	if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
		t.Fatalf("rejected block must extract as empty map, got %v", got)
	}
}

// TestChar_ValueWithColon pins that everything after the FIRST colon is kept
// as the value (strings.Cut on first ':').
func TestChar_ValueWithColon(t *testing.T) {
	in := "---\nfrom: codex\ntask: a: b: c\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err != nil {
		t.Fatalf("expected ACCEPT, got %v", err)
	}
	if got := ParseMessageFrontmatter([]byte(in))["task"]; got != "a: b: c" {
		t.Fatalf("want task=%q, got %q", "a: b: c", got)
	}
}

// TestChar_QuoteStripping pins a REAL B8 behavior change: quoted values now
// run through cleanScalar (strconv.Unquote first, falling back to a
// paired-quote strip), so backslash escapes are INTERPRETED. Before B8,
// ParseMessageFrontmatter did a blunt strings.Trim(val, `"'`) and left
// escapes literal.
func TestChar_QuoteStripping(t *testing.T) {
	in := "---\nfrom: codex\ntask: \"hello\"\n---\n\nbody\n"
	if got := ParseMessageFrontmatter([]byte(in))["task"]; got != "hello" {
		t.Fatalf("want hello, got %q", got)
	}

	in2 := "---\nfrom: codex\ntask: \"a\\tb\"\n---\n\nbody\n"
	if got := ParseMessageFrontmatter([]byte(in2))["task"]; got != "a\tb" {
		t.Fatalf("strconv.Unquote interprets escape; want a<TAB>b, got %q", got)
	}
}

// TestChar_NullSentinel pins a REAL B8 behavior change: `null`/`~` now map to
// empty string in message frontmatter too (cleanScalar, shared with
// registration). Before B8, ParseMessageFrontmatter kept the literal text.
func TestChar_NullSentinel(t *testing.T) {
	in := "---\nfrom: codex\ntask: null\n---\n\nbody\n"
	if got := ParseMessageFrontmatter([]byte(in))["task"]; got != "" {
		t.Fatalf("null sentinel -> empty string; got %q", got)
	}
}

// TestChar_WhitespaceAroundDelimiter pins that a whitespace-padded `---` line
// still closes the block (TrimSpace before compare).
func TestChar_WhitespaceAroundDelimiter(t *testing.T) {
	in := "---\nfrom: codex\ntask: t\n  ---  \n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err != nil {
		t.Fatalf("padded --- must close the block: %v", err)
	}
	got := ParseMessageFrontmatter([]byte(in))
	if got["task"] != "t" || got["from"] != "codex" {
		t.Fatalf("padded --- must close block; got map=%v", got)
	}
}

// TestChar_MissingClose pins that a missing closing `---` errors, and
// ParseMessageFrontmatter returns an empty map for it.
func TestChar_MissingClose(t *testing.T) {
	in := "---\nfrom: codex\ntask: t\n\nbody without close\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err == nil {
		t.Fatal("missing close must error")
	}
	if got := ParseMessageFrontmatter([]byte(in)); len(got) != 0 {
		t.Fatalf("missing close -> empty map; got %v", got)
	}
}

// TestChar_EmptyValueScalar pins that a bare `task:` (no list items follow)
// records an empty-string scalar.
func TestChar_EmptyValueScalar(t *testing.T) {
	in := "---\nfrom: codex\ntask:\n---\n\nbody\n"
	if err := ValidateMessageFrontmatter([]byte(in)); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	got := ParseMessageFrontmatter([]byte(in))
	if v, ok := got["task"]; !ok || v != "" {
		t.Fatalf("empty value -> empty-string scalar present; got map=%v", got)
	}
}

// TestChar_ListValue pins that `- ` list items parse into a list field
// (parseFrontmatter's own fields.list), while ParseMessageFrontmatter's flat
// map surfaces only the header key with an empty scalar — no current message
// field is list-valued, so this is documentation, not a load-bearing path.
func TestChar_ListValue(t *testing.T) {
	in := "---\nfrom: codex\nworking_repos:\n  - /a\n  - /b\n---\n\nbody\n"
	fields, _, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("list parse unexpected error %v", err)
	}
	if got := fields.list("working_repos"); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("want [/a /b], got %v", got)
	}
	got := ParseMessageFrontmatter([]byte(in))
	if v, ok := got["working_repos"]; !ok || v != "" {
		t.Fatalf("want working_repos= (empty), got %v (map=%v)", v, got)
	}
	if len(got) != 2 {
		t.Fatalf("want exactly 2 keys (from + working_repos); got map=%v", got)
	}
}
