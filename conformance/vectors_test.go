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
	NextMessage    vectorMsg `json:"next_message,omitempty"`
	DifferentMsg   vectorMsg `json:"different_message,omitempty"`
	InvalidMessage vectorMsg `json:"invalid_message,omitempty"`
	MalformedItems []string  `json:"malformed_items,omitempty"`
}

type vectorMsg struct {
	From          string            `json:"from,omitempty"`
	Body          string            `json:"body,omitempty"`
	ReplyRequired bool              `json:"reply_required,omitempty"`
	InReplyTo     string            `json:"in_reply_to,omitempty"`
	Key           string            `json:"key,omitempty"`
	Extra         map[string]string `json:"extra,omitempty"`
	Seq           uint64            `json:"seq,omitempty"`
}

func (m vectorMsg) msg() Msg {
	return Msg{
		From:          m.From,
		Body:          m.Body,
		ReplyRequired: m.ReplyRequired,
		InReplyTo:     m.InReplyTo,
		Key:           m.Key,
		Extra:         m.Extra,
		Seq:           m.Seq,
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
	for _, id := range []string{"R1", "D1", "D2", "O1", "C1", "C2", "E1", "B1", "Q1"} {
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
