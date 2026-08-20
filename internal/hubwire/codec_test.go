package hubwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/op"
)

func roundTrip(t *testing.T, frame any, body []byte) RawFrame {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	errCh := make(chan error, 1)
	go func() { errCh <- NewWriter(client).Write(frame, body) }()
	got, err := NewReader(server).Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRoundTripEveryFrameTypeOverNetPipe(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 123, time.UTC)
	str := "openai"
	bio := "bio"
	frames := []struct {
		name  string
		frame any
		body  []byte
	}{
		{"hello", Hello{RequestBase: RequestBase{T: "hello", ID: 1}, Proto: Protocol, V: 1, MinV: 1, Agent: "codex", Bin: "1.6.0"}, nil},
		{"hello-ok", HelloOK{ResponseBase: ResponseBase{T: "hello-ok", Re: 1}, V: 1, Agent: "codex", Pool: "/pool", Pool12: "0123456789ab", Writable: true, HubBin: "1.6.0", HubTime: now}, nil},
		{"send", Send{RequestBase: RequestBase{T: "send", ID: 2}, To: "grok", Ask: true, ReplyByS: 60, ServeToken: "token"}, []byte("body")},
		{"send-ok", SendOK{ResponseBase: ResponseBase{T: "send-ok", Re: 2}, Filename: "m.md", Ref: "ref", Committed: true, DurabilityNote: "", OwedNote: ""}, nil},
		{"msg", Message{ResponseBase: ResponseBase{T: "msg", Re: 3}, Filename: "m.md", Sender: "grok", Stamp: now.Format(time.RFC3339Nano), Redelivered: true, ReplyRequired: true, ReplyRef: "ref"}, []byte{}},
		{"owed-item", OwedItem{ResponseBase: ResponseBase{T: "owed-item", Re: 3}, To: "grok", From: "codex", Stamp: "stamp", Suffix: "suffix", By: now, RecordedAt: now, Ref: "ref"}, nil},
		{"check", Check{RequestBase: RequestBase{T: "check", ID: 3}, Limit: 2}, nil},
		{"check-ok", CheckOK{ResponseBase: ResponseBase{T: "check-ok", Re: 3}, Claimed: 1, Redelivered: 1, Quarantined: 1, OwedExpired: 1}, nil},
		{"ack", Ack{RequestBase: RequestBase{T: "ack", ID: 4}}, nil},
		{"ack-item", AckItem{ResponseBase: ResponseBase{T: "ack-item", Re: 4}, Filename: "m.md", ArchivePath: "/archive/m.md"}, nil},
		{"ack-ok", AckOK{ResponseBase: ResponseBase{T: "ack-ok", Re: 4}, Acked: 1, GateClear: true}, nil},
		{"register", Register{RequestBase: RequestBase{T: "register", ID: 5}, Vendor: &str, Host: "tiny", Bio: &bio, WorkingRepos: []string{"/repo"}, Announce: true}, nil},
		{"register-ok", RegisterOK{ResponseBase: ResponseBase{T: "register-ok", Re: 5}, Announce: &Announce{Warnings: []string{}}, Reg: Registration{AgentID: "codex", LastSeen: now}, Warnings: []string{}}, []byte("registration body")},
		{"status", Status{RequestBase: RequestBase{T: "status", ID: 6}}, nil},
		{"status-ok", StatusOK{ResponseBase: ResponseBase{T: "status-ok", Re: 6}, Agents: []op.StatusAgent{}, Now: now}, nil},
		{"gate", Gate{RequestBase: RequestBase{T: "gate", ID: 7}, Phase: op.GatePhaseFinish}, nil},
		{"gate-ok", GateOK{ResponseBase: ResponseBase{T: "gate-ok", Re: 7}, GateResp: op.GateResp{Agent: "codex", Phase: op.GatePhaseFinish}}, nil},
		{"pending", Pending{RequestBase: RequestBase{T: "pending", ID: 8}, ShowBody: true}, nil},
		{"pending-ok", PendingOK{ResponseBase: ResponseBase{T: "pending-ok", Re: 8}, Unread: 1}, nil},
		{"clean-owed", CleanOwed{RequestBase: RequestBase{T: "clean-owed", ID: 9}, Apply: true}, nil},
		{"clean-owed-ok", CleanOwedOK{ResponseBase: ResponseBase{T: "clean-owed-ok", Re: 9}, Agent: "codex", Pruned: []string{}}, nil},
		{"lease-acquire", LeaseAcquire{RequestBase: RequestBase{T: "lease-acquire", ID: 10}}, nil},
		{"lease-ok", LeaseOK{ResponseBase: ResponseBase{T: "lease-ok", Re: 10}, Token: "token"}, nil},
		{"tick", Tick{RequestBase: RequestBase{T: "tick", ID: 11}}, nil},
		{"tick-ok", TickOK{ResponseBase: ResponseBase{T: "tick-ok", Re: 11}, Warnings: []string{}}, nil},
		{"lease-release", LeaseRelease{RequestBase: RequestBase{T: "lease-release", ID: 12}}, nil},
		{"release-ok", ReleaseOK{ResponseBase: ResponseBase{T: "release-ok", Re: 12}}, nil},
		{"note", Note{ResponseBase: ResponseBase{T: "note", Re: 3}, Level: op.NoteInfo, Msg: "note"}, nil},
		{"error", Error{ResponseBase: ResponseBase{T: "error", Re: 3}, Code: "E_HUB_IO", Msg: "broken", Retriable: false, ClaimedHeld: true}, nil},
	}
	for _, tc := range frames {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.frame, tc.body)
			if got.T != tc.name {
				t.Fatalf("t = %q", got.T)
			}
			if !bytes.Equal(got.Body, tc.body) || got.HasBody != (tc.body != nil) {
				t.Fatalf("body = %q has=%v, want %q has=%v", got.Body, got.HasBody, tc.body, tc.body != nil)
			}
		})
	}
}

func TestBodyTrailerBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, MaxBody} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := bytes.Repeat([]byte{'x'}, size)
			got := roundTrip(t, Send{RequestBase: RequestBase{T: "send", ID: 1}, To: "codex"}, body)
			if !bytes.Equal(got.Body, body) {
				t.Fatalf("body bytes differ at %d", size)
			}
		})
	}
	_, err := Encode(Send{RequestBase: RequestBase{T: "send", ID: 1}}, make([]byte, MaxBody+1))
	assertCode(t, err, CodeTooLarge)
}

func TestReaderRejectsTruncationAndOversize(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		code string
	}{
		{"empty truncated line", []byte("{"), CodeMalformedFrame},
		{"valid json without LF", []byte(`{"t":"check","id":1}`), CodeMalformedFrame},
		{"truncated trailer zero of one", []byte("{\"t\":\"send\",\"id\":1,\"body_len\":1}\n"), CodeMalformedFrame},
		{"oversize declared trailer", []byte(fmt.Sprintf("{\"t\":\"send\",\"id\":1,\"body_len\":%d}\n", MaxBody+1)), CodeTooLarge},
		{"oversize line", append(bytes.Repeat([]byte{' '}, MaxControlLine), '\n'), CodeTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReader(bytes.NewReader(tc.in)).Read()
			assertCode(t, err, tc.code)
		})
	}
}

func TestControlAndTrailerExactBoundaries(t *testing.T) {
	prefix := []byte(`{"t":"future","id":1,"pad":"`)
	suffix := []byte(`"}` + "\n")
	line := append(append(append([]byte(nil), prefix...), bytes.Repeat([]byte{'x'}, MaxControlLine-len(prefix)-len(suffix))...), suffix...)
	if len(line) != MaxControlLine {
		t.Fatalf("fixture line = %d", len(line))
	}
	if _, err := NewReader(bytes.NewReader(line)).Read(); err != nil {
		t.Fatalf("exact control boundary: %v", err)
	}
	oneOver := append(append([]byte(nil), line[:len(line)-1]...), 'x', '\n')
	if _, err := NewReader(bytes.NewReader(oneOver)).Read(); err == nil {
		t.Fatal("one-byte-oversize control line accepted")
	}
	if _, err := NewReader(bytes.NewReader(line[:len(line)-1])).Read(); err == nil {
		t.Fatal("exact-size control line without LF accepted")
	}

	header := []byte(fmt.Sprintf("{\"t\":\"send\",\"id\":1,\"body_len\":%d}\n", MaxBody))
	truncated := append(header, bytes.Repeat([]byte{'b'}, MaxBody-1)...)
	if _, err := NewReader(bytes.NewReader(truncated)).Read(); err == nil {
		t.Fatal("max-size trailer truncated by one byte accepted")
	}
}

func TestLargeRegistrationBodyUsesTrailer(t *testing.T) {
	body := bytes.Repeat([]byte{'b'}, MaxControlLine+1)
	frame := RegisterOK{ResponseBase: ResponseBase{T: "register-ok", Re: 2}, Announce: nil, Reg: Registration{AgentID: "codex"}, Warnings: []string{}}
	got := roundTrip(t, frame, body)
	if got.T != "register-ok" || len(got.Control)+1 > MaxControlLine || !bytes.Equal(got.Body, body) {
		t.Fatalf("control=%d body=%d", len(got.Control)+1, len(got.Body))
	}
}

func TestUnknownFieldsIgnoredAndMandatoryFieldsEnforced(t *testing.T) {
	raw, err := NewReader(strings.NewReader("{\"t\":\"hello\",\"id\":1,\"proto\":\"agentchute-hub\",\"v\":1,\"min_v\":1,\"agent\":\"codex\",\"bin\":\"dev\",\"future\":true}\n")).Read()
	if err != nil {
		t.Fatal(err)
	}
	var hello Hello
	if err := raw.Decode(&hello); err != nil || hello.Agent != "codex" {
		t.Fatalf("decode = %+v, %v", hello, err)
	}
	for _, line := range []string{
		`{"t":"send-ok","re":2,"durability_note":"","owed_note":""}` + "\n",
		`{"t":"send-ok","re":2,"committed":true,"owed_note":""}` + "\n",
		`{"t":"send-ok","re":2,"committed":true,"durability_note":""}` + "\n",
		`{"t":"tick-ok","re":2}` + "\n",
		`{"t":"register-ok","re":2,"announce":null}` + "\n",
		`{"t":"note","re":2,"level":"debug","msg":"x"}` + "\n",
	} {
		_, err := NewReader(strings.NewReader(line)).Read()
		assertCode(t, err, CodeMalformedFrame)
	}
}

func TestStatusProducerBudgetsAndPrefix(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	row := func(id, host string) op.StatusAgent {
		return op.StatusAgent{AgentID: id, LastSeen: now, Host: host, ProtocolVersion: 3, InboxDepth: 1, Status: "fresh"}
	}
	small := make([]op.StatusAgent, 65)
	for i := range small {
		small[i] = row(fmt.Sprintf("agent-%02d", i), "host")
	}
	line, out, err := EncodeStatus(1, op.StatusResp{Agents: small[:64], Now: now})
	if err != nil || out.Truncated || len(out.Agents) != 64 || len(line) > MaxControlLine {
		t.Fatalf("64 rows: len=%d out=%+v err=%v", len(line), out, err)
	}
	_, out, err = EncodeStatus(1, op.StatusResp{Agents: small, Now: now})
	if err != nil || !out.Truncated || !reflect.DeepEqual(out.Agents, small[:64]) {
		t.Fatalf("65 rows: kept=%d truncated=%v err=%v", len(out.Agents), out.Truncated, err)
	}

	huge := strings.Repeat("h", MaxControlLine)
	_, out, err = EncodeStatus(1, op.StatusResp{Agents: []op.StatusAgent{row("a", huge), row("b", "ok"), row("c", "ok")}, Now: now})
	if err != nil || !out.Truncated || len(out.Agents) != 0 {
		t.Fatalf("oversize first: kept=%d truncated=%v err=%v", len(out.Agents), out.Truncated, err)
	}
	line, out, err = EncodeStatus(1, op.StatusResp{Agents: []op.StatusAgent{row("a", "ok"), row("b", "ok"), row("c", huge)}, Now: now})
	if err != nil || !out.Truncated || len(out.Agents) != 2 || len(line) > MaxControlLine {
		t.Fatalf("oversize last: line=%d kept=%d truncated=%v err=%v", len(line), len(out.Agents), out.Truncated, err)
	}

	base := row("boundary", "")
	baseLine, _, err := EncodeStatus(9, op.StatusResp{Agents: []op.StatusAgent{base}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	base.Host = strings.Repeat("h", MaxControlLine-len(baseLine))
	exact, exactOut, err := EncodeStatus(9, op.StatusResp{Agents: []op.StatusAgent{base}, Now: now})
	if err != nil || len(exact) != MaxControlLine || exactOut.Truncated {
		t.Fatalf("exact boundary: len=%d truncated=%v err=%v", len(exact), exactOut.Truncated, err)
	}
	base.Host += "h"
	short, dropped, err := EncodeStatus(9, op.StatusResp{Agents: []op.StatusAgent{base}, Now: now})
	if err != nil || !dropped.Truncated || len(dropped.Agents) != 0 || len(short) > MaxControlLine {
		t.Fatalf("one over: len=%d kept=%d truncated=%v err=%v", len(short), len(dropped.Agents), dropped.Truncated, err)
	}
	base.Host = base.Host[:len(base.Host)-1]
	flipped, flipOut, err := EncodeStatus(9, op.StatusResp{Agents: []op.StatusAgent{base, row("tail", "x")}, Now: now})
	if err != nil || !flipOut.Truncated || len(flipOut.Agents) != 1 || len(flipped) != MaxControlLine-1 {
		t.Fatalf("flag flip: len=%d kept=%d truncated=%v err=%v", len(flipped), len(flipOut.Agents), flipOut.Truncated, err)
	}
}

func TestRegistryCompleteness(t *testing.T) {
	opCodes := []string{"E_NOT_REGISTERED", "E_RECIPIENT_UNKNOWN", "E_RECIPIENT_UNREADABLE", "E_RECIPIENT_STALE", "E_RECIPIENT_RACING", "E_FENCED", "E_LEASE_HELD", "E_ORDER", "E_HUB_IO"}
	codecCodes := []string{CodeVersion, CodeIdentity, CodePoolNotFound, CodePoolIDInvalid, CodePoolMismatch, CodeMalformedFrame, CodeTooLarge, CodeUnsupported, CodeUnpinned}
	clientOnly := []string{"E_CONNECT", "E_UNAUTHORIZED", "E_HOSTKEY_CHANGED", "E_CHANNEL_LOST", "E_SEND_UNKNOWN", "E_HELLO_TIMEOUT", "E_HUB_NO_BINARY", "E_NOT_JOINED", "E_NO_SSH", "E_HUB_UNPINNED"}
	seen := map[string]bool{}
	opErrors := []error{
		op.ErrNotRegistered, op.ErrRecipientUnknown, op.ErrRecipientUnreadable,
		op.ErrRecipientStale, op.ErrRecipientRacing, op.ErrFenced,
		op.ErrLeaseHeld, op.ErrOrder, errors.New("default I/O"),
	}
	for i, err := range opErrors {
		if got := op.CodeFor(err); got != opCodes[i] {
			t.Fatalf("op.CodeFor(%v) = %s, want %s", err, got, opCodes[i])
		}
	}
	for _, set := range [][]string{opCodes, codecCodes} {
		for _, code := range set {
			if seen[code] {
				t.Fatalf("duplicate hub code %s", code)
			}
			seen[code] = true
			if Emitters[code] != EmitterHub && !(code == CodePoolMismatch && Emitters[code] == EmitterBoth) {
				t.Fatalf("hub code %s classified %q", code, Emitters[code])
			}
		}
	}
	if len(seen) != 18 {
		t.Fatalf("hub union = %d, want 18", len(seen))
	}
	for _, code := range clientOnly {
		if seen[code] || Emitters[code] != EmitterClient {
			t.Fatalf("client-only code %s overlaps or misclassified", code)
		}
	}
	if Emitters[CodePoolMismatch] != EmitterBoth {
		t.Fatalf("pool mismatch emitter = %q", Emitters[CodePoolMismatch])
	}
	// 29 with E_HUB_PINNING_UNVERIFIED, the fourth outcome of the exit-127 arm.
	// The count is asserted on purpose: a code added to the map and forgotten in
	// the spec's table is exactly the drift this row exists to catch, so it must
	// be moved deliberately rather than grow on its own.
	if len(Emitters) != 29 {
		t.Fatalf("emitter registry = %d rows, want 29", len(Emitters))
	}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("nil error, want %s", want)
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != want {
		t.Fatalf("err = %v (%T), want %s", err, err, want)
	}
}

func TestControlLineHelper(t *testing.T) {
	encoded, err := Encode(Send{RequestBase: RequestBase{T: "send", ID: 1}}, []byte("body\nbytes"))
	if err != nil {
		t.Fatal(err)
	}
	line := controlLine(encoded)
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Fatal("control line missing LF")
	}
	var envelope map[string]any
	if err := json.Unmarshal(bytes.TrimSuffix(line, []byte{'\n'}), &envelope); err != nil {
		t.Fatal(err)
	}
}

type shortWriter struct{ remaining int }

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	w.remaining -= len(p)
	return len(p), nil
}

func TestWriterSurfacesMidFrameFailure(t *testing.T) {
	err := NewWriter(&shortWriter{remaining: 5}).Write(Check{RequestBase: RequestBase{T: "check", ID: 1}}, nil)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err = %v", err)
	}
}
