package conformance

import "testing"

// fm_test.go — v2.5 plan B8 vectors: the one flat key:value frontmatter
// grammar that message envelopes and registration rows now share.

// FM1 — the grammar accepts every well-formed block and extracts exactly the
// documented fields.
func TestFM1_Accept(t *testing.T) {
	v := vectorByID(t, "FM1", "frontmatter_accept")
	for _, c := range v.AcceptCases {
		got, err := ParseFrontmatterFields(c.Input)
		if err != nil {
			t.Errorf("%s: expected ACCEPT, got error: %v", c.Name, err)
			continue
		}
		if len(got) != len(c.Fields) {
			t.Errorf("%s: field count mismatch: got %v, want %v", c.Name, got, c.Fields)
			continue
		}
		for k, want := range c.Fields {
			if got[k] != want {
				t.Errorf("%s: field %q: got %q, want %q (map=%v)", c.Name, k, got[k], want, got)
			}
		}
	}
}

// FM2 — the same grammar hard-rejects every malformed block: indentation, a
// duplicate key, a non-key:value line (including a `#` comment — this
// grammar has no comment syntax), an empty key, and a missing closing
// delimiter. Rejection is whole-block: no partial field extraction survives.
func TestFM2_Reject(t *testing.T) {
	v := vectorByID(t, "FM2", "frontmatter_reject")
	for _, c := range v.RejectCases {
		if _, err := ParseFrontmatterFields(c.Input); err == nil {
			t.Errorf("%s: expected REJECT, got no error", c.Name)
		}
	}
}
