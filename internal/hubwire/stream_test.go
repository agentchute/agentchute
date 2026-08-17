package hubwire

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/agentchute/agentchute/internal/op"
)

type measuringWriter struct {
	writes   int
	maxWrite int
}

func (w *measuringWriter) Write(p []byte) (int, error) {
	w.writes++
	if len(p) > w.maxWrite {
		w.maxWrite = len(p)
	}
	return len(p), nil
}

func TestStreamThirtyTwoMaximumBodiesNeverCombinesBodies(t *testing.T) {
	transport := &measuringWriter{}
	w := NewWriter(transport)
	body := bytes.Repeat([]byte{'x'}, MaxBody)
	for i := 0; i < 32; i++ {
		if err := w.Write(Message{ResponseBase: ResponseBase{T: "msg", Re: 2}, Filename: "m", Sender: "grok"}, body); err != nil {
			t.Fatal(err)
		}
	}
	// One small control write plus one body write per event. A codec that
	// concatenates control+body, or buffers a second body, exceeds this bound.
	if transport.writes != 64 || transport.maxWrite != MaxBody {
		t.Fatalf("writes=%d max=%d, want 64 writes with max one body (%d)", transport.writes, transport.maxWrite, MaxBody)
	}
}

type failCallWriter struct {
	bytes.Buffer
	calls  int
	failAt int
}

func (w *failCallWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls >= w.failAt {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(p)
}

func TestTypedEventStreamOrderAndFailureBoundaries(t *testing.T) {
	first := Message{ResponseBase: ResponseBase{T: "msg", Re: 2}, Filename: "m", Sender: "grok"}
	note := Note{ResponseBase: ResponseBase{T: "note", Re: 2}, Level: op.NoteInfo, Msg: "mid-stream"}
	owed := OwedItem{ResponseBase: ResponseBase{T: "owed-item", Re: 2}, To: "grok", From: "codex", Ref: "r"}

	// Failure after the first emitted item: its control+body are complete and
	// the next frame contributes no partial bytes.
	afterFirst := &failCallWriter{failAt: 3}
	w := NewWriter(afterFirst)
	if err := w.Write(first, []byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(note, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failure = %v", err)
	}
	r := NewReader(bytes.NewReader(afterFirst.Bytes()))
	raw, err := r.Read()
	if err != nil || raw.T != "msg" || string(raw.Body) != "body" {
		t.Fatalf("preserved first event = %s %q, %v", raw.T, raw.Body, err)
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("partial next frame survived: %v", err)
	}

	// Failure after a mid-stream note: msg then note remain in production
	// order; the later owed item contributes no partial frame.
	afterNote := &failCallWriter{failAt: 4}
	w = NewWriter(afterNote)
	if err := w.Write(first, []byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(note, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(owed, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failure = %v", err)
	}
	r = NewReader(bytes.NewReader(afterNote.Bytes()))
	var got []string
	for {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, raw.T)
	}
	if len(got) != 2 || got[0] != "msg" || got[1] != "note" {
		t.Fatalf("order = %v", got)
	}
}

func TestInterleavedNoteLevelsPreserveFrameOrder(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	frames := []any{
		Message{ResponseBase: ResponseBase{T: "msg", Re: 2}, Filename: "a", Sender: "grok"},
		Note{ResponseBase: ResponseBase{T: "note", Re: 2}, Level: op.NoteWarn, Msg: "warn"},
		OwedItem{ResponseBase: ResponseBase{T: "owed-item", Re: 2}, To: "grok", From: "codex", Ref: "r"},
		Note{ResponseBase: ResponseBase{T: "note", Re: 2}, Level: op.NoteInfo, Msg: "info"},
	}
	for _, frame := range frames {
		if err := w.Write(frame, nil); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(&buf)
	want := []string{"msg", "note", "owed-item", "note"}
	for i, typ := range want {
		raw, err := r.Read()
		if err != nil || raw.T != typ {
			t.Fatalf("frame %d = %s, %v", i, raw.T, err)
		}
		if raw.T == "note" {
			var note Note
			if err := raw.Decode(&note); err != nil {
				t.Fatal(err)
			}
			wantLevel := op.NoteWarn
			if i == 3 {
				wantLevel = op.NoteInfo
			}
			if note.Level != wantLevel {
				t.Fatalf("note %d level = %s", i, note.Level)
			}
		}
	}
}

func TestPendingMaximumBodyUsesOrdinaryTrailer(t *testing.T) {
	body := bytes.Repeat([]byte{'p'}, MaxBody)
	got := roundTrip(t, Message{ResponseBase: ResponseBase{T: "msg", Re: 9}, Filename: "pending", Sender: "grok"}, body)
	if got.T != "msg" || len(got.Body) != MaxBody {
		t.Fatalf("pending msg = %s body=%d", got.T, len(got.Body))
	}
}
