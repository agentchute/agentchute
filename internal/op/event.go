package op

import "time"

// Note levels. The level IS the stream (DESIGN.md §4.3, F4): a renderer sends
// NoteWarn to stderr as "warning: <Msg>" and NoteInfo to stdout as "<Msg>".
// Msg NEVER carries its own level prefix — the renderer adds it — so the wire
// and the local path cannot drift. A third level is a spec change, never an
// implementer's choice.
const (
	NoteWarn = "warn"
	NoteInfo = "info"
)

// Event is the single ordered, typed stream a streaming op produces. Exactly
// one arm is non-nil (C4/D2). Events are emitted in PRODUCTION order — a
// quarantine warning lands between the messages it actually occurred between,
// an expired obligation where the code produced it — so ordering survives the
// wire and nothing buffers until return.
type Event struct {
	Message *MessageEvent `json:"message,omitempty"`
	Note    *NoteEvent    `json:"note,omitempty"`
	Owed    *OwedEvent    `json:"owed,omitempty"`
	Ack     *AckItemEvent `json:"ack,omitempty"`
}

// MessageEvent is one inbox message reaching a consumer.
//
// Body is the message file's bytes VERBATIM (frontmatter included) — the
// renderer does its own extraction/sanitization, so one field means one thing
// in every op that emits it. Claim always fills it; Pending fills it only
// under PendingReq.ShowBody.
//
// Stamp is the message's timestamp in RFC3339Nano (UTC), the same spelling
// `pending` already prints. ReplyRef is the precomputed ref a reply must echo
// when ReplyRequired, or "" when the filename carries no parseable identity.
type MessageEvent struct {
	Filename      string `json:"filename"`
	Sender        string `json:"sender"`
	Stamp         string `json:"stamp"`
	Redelivered   bool   `json:"redelivered,omitempty"`
	ReplyRequired bool   `json:"reply_required,omitempty"`
	ReplyRef      string `json:"reply_ref,omitempty"`
	Body          []byte `json:"body,omitempty"`
}

// NoteEvent is a line that today goes straight to a stream, carried in-stream
// so a remote client renders it in the right POSITION and not merely with the
// right text. Level is exactly NoteWarn or NoteInfo.
type NoteEvent struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// OwedEvent is one asker-owned reply obligation — the full loop.OwedEntry
// field set (E4), because `pending --json` serializes every one of them. Ref
// is the precomputed Key().RefString() convenience; the rest is byte-for-byte
// what the ledger holds.
type OwedEvent struct {
	To         string    `json:"to"`
	From       string    `json:"from"`
	Seq        uint64    `json:"seq,omitempty"`
	Stamp      string    `json:"stamp,omitempty"`
	Suffix     string    `json:"suffix,omitempty"`
	By         time.Time `json:"by"`
	RecordedAt time.Time `json:"recorded_at"`
	Ref        string    `json:"ref"`
}

// AckItemEvent is one message committed (archived) by Ack, emitted as it
// commits rather than collected — the commit already happened per item, so a
// mid-stream emit failure loses a report, never a commit.
type AckItemEvent struct {
	Filename    string `json:"filename"`
	ArchivePath string `json:"archive_path"`
}

// NewMessageEvent, NewNoteEvent, NewOwedEvent and NewAckItemEvent are the ONLY
// supported way to build an Event: each sets exactly one arm, so the union
// invariant cannot be violated by a caller assembling a literal.
func NewMessageEvent(m MessageEvent) Event { return Event{Message: &m} }

func NewNoteEvent(level, msg string) Event {
	return Event{Note: &NoteEvent{Level: level, Msg: msg}}
}

func NewOwedEvent(o OwedEvent) Event { return Event{Owed: &o} }

func NewAckItemEvent(a AckItemEvent) Event { return Event{Ack: &a} }

// Arms reports how many arms of the union are set. Exactly one is the
// invariant; the count exists so a test can assert it without reflection.
func (e Event) Arms() int {
	n := 0
	if e.Message != nil {
		n++
	}
	if e.Note != nil {
		n++
	}
	if e.Owed != nil {
		n++
	}
	if e.Ack != nil {
		n++
	}
	return n
}

// Valid reports whether the union invariant holds.
func (e Event) Valid() bool { return e.Arms() == 1 }
