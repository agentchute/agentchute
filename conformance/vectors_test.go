package conformance

import (
	"embed"
	"encoding/json"
	"fmt"
	"testing"
)

//go:embed vectors/*.json
var vectorFS embed.FS

type vectorSet struct {
	Schema  string       `json:"schema"`
	Vectors []testVector `json:"vectors"`
}

type testVector struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	AppliesTo      []string  `json:"applies_to,omitempty"`
	Agent          string    `json:"agent,omitempty"`
	Recipient      string    `json:"recipient,omitempty"`
	Reader         string    `json:"reader,omitempty"`
	Sender         string    `json:"sender,omitempty"`
	SenderPrefix   string    `json:"sender_prefix,omitempty"`
	SenderModulo   int       `json:"sender_modulo,omitempty"`
	BodyPrefix     string    `json:"body_prefix,omitempty"`
	Count          int       `json:"count,omitempty"`
	StaleSeconds   int       `json:"stale_seconds,omitempty"`
	Bodies         []string  `json:"bodies,omitempty"`
	Message        vectorMsg `json:"message,omitempty"`
	InvalidMessage vectorMsg `json:"invalid_message,omitempty"`
	MalformedItems []string  `json:"malformed_items,omitempty"`

	// TS1 (filename_grammar): a parse/sort/reject table over the C3 grammar.
	ValidNames   []string `json:"valid_names,omitempty"`
	InvalidNames []string `json:"invalid_names,omitempty"`

	// DR1 (dual_read_listing): a mixed inbox of old-grammar, new-grammar, and
	// garbage names, classified via classifyInboxName.
	OldNames     []string `json:"old_names,omitempty"`
	NewNames     []string `json:"new_names,omitempty"`
	GarbageNames []string `json:"garbage_names,omitempty"`

	// FM1/FM2 (frontmatter_accept/frontmatter_reject): the one flat
	// key:value envelope grammar's accept and reject tables (v2.5 plan B8),
	// promoted from internal/loop/frontmatter_characterization_test.go.
	AcceptCases []fmCase `json:"accept_cases,omitempty"`
	RejectCases []fmCase `json:"reject_cases,omitempty"`
}

// fmCase is one FM1/FM2 row: a named input and, for FM1, the exact flat
// field map ParseFrontmatterFields must return for it.
type fmCase struct {
	Name   string            `json:"name"`
	Input  string            `json:"input"`
	Fields map[string]string `json:"fields,omitempty"`
}

type vectorMsg struct {
	From          string            `json:"from,omitempty"`
	Body          string            `json:"body,omitempty"`
	ReplyRequired bool              `json:"reply_required,omitempty"`
	InReplyTo     string            `json:"in_reply_to,omitempty"`
	Extra         map[string]string `json:"extra,omitempty"`
}

func (m vectorMsg) msg() Msg {
	return Msg{
		From:          m.From,
		Body:          m.Body,
		ReplyRequired: m.ReplyRequired,
		InReplyTo:     m.InReplyTo,
		Extra:         m.Extra,
	}
}

func loadVectors(t *testing.T) map[string]testVector {
	t.Helper()
	data, err := vectorFS.ReadFile("vectors/core.json")
	if err != nil {
		t.Fatal(err)
	}
	var set vectorSet
	if err := json.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	if set.Schema != "agentchute-conformance-vectors-v1" {
		t.Fatalf("unexpected vector schema %q", set.Schema)
	}
	out := map[string]testVector{}
	for _, v := range set.Vectors {
		if v.ID == "" || v.Kind == "" {
			t.Fatalf("invalid vector with missing id/kind: %+v", v)
		}
		validateAppliesTo(t, v)
		if _, exists := out[v.ID]; exists {
			t.Fatalf("duplicate vector id %q", v.ID)
		}
		out[v.ID] = v
	}
	for _, id := range []string{"R1", "D1", "D2", "O1", "C1", "E1", "B1", "Q1", "TS1", "TS2", "TS3", "DR1", "FM1", "FM2"} {
		if _, ok := out[id]; !ok {
			t.Fatalf("missing vector %s", id)
		}
	}
	return out
}

func vectorByID(t *testing.T, id, kind string) testVector {
	t.Helper()
	v, ok := loadVectors(t)[id]
	if !ok {
		t.Fatalf("missing vector %s", id)
	}
	if v.Kind != kind {
		t.Fatalf("vector %s kind = %q, want %q", id, v.Kind, kind)
	}
	return v
}

func (v testVector) senderFor(i int) string {
	if v.SenderModulo <= 0 {
		return v.SenderPrefix
	}
	return fmt.Sprintf("%s%d", v.SenderPrefix, i%v.SenderModulo)
}

func validateAppliesTo(t *testing.T, v testVector) {
	t.Helper()
	if v.AppliesTo == nil {
		return
	}
	if len(v.AppliesTo) == 0 {
		t.Fatalf("vector %s has empty applies_to; omit it for universal", v.ID)
	}
	seen := map[string]bool{}
	for _, profile := range v.AppliesTo {
		if !knownProfile(profile) {
			t.Fatalf("vector %s applies_to has unknown profile %q", v.ID, profile)
		}
		if seen[profile] {
			t.Fatalf("vector %s applies_to repeats profile %q", v.ID, profile)
		}
		seen[profile] = true
	}
}
