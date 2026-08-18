package hubwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/agentchute/agentchute/internal/op"
)

// RawFrame is one decoded control object and its optional byte-exact trailer.
type RawFrame struct {
	Control []byte
	Body    []byte
	T       string
	ID      int64
	Re      int64
	HasBody bool
}

type envelope struct {
	T       string `json:"t"`
	ID      int64  `json:"id"`
	Re      int64  `json:"re"`
	BodyLen *int64 `json:"body_len"`
}

type Reader struct{ r *bufio.Reader }

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, MaxControlLine+1)}
}

func (r *Reader) Read() (RawFrame, error) {
	line, err := r.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return RawFrame{}, protocolError(CodeTooLarge, "control frame exceeds 64 KiB")
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return RawFrame{}, io.EOF
		}
		// Say WHICH read failure this was. Only a clean EOF at a frame boundary
		// takes the branch above; everything else lands here — a partial line at
		// EOF, a reset, a use-of-closed-connection, an i/o timeout — and the
		// underlying error used to be discarded, so one sentence stood for
		// several distinguishable causes. That cost M6 a diagnostic round: the
		// message reads as "the frame was truncated" when it actually means
		// "the read did not end at a boundary", which is a much weaker claim.
		// Same defect as the E_CHANNEL_LOST arm one layer up (#167, #169).
		return RawFrame{}, protocolError(CodeMalformedFrame, "truncated control frame: "+err.Error())
	}
	if len(line) > MaxControlLine {
		return RawFrame{}, protocolError(CodeTooLarge, "control frame exceeds 64 KiB")
	}
	control := line[:len(line)-1]
	if len(control) == 0 || !utf8.Valid(control) {
		return RawFrame{}, protocolError(CodeMalformedFrame, "control frame is not a UTF-8 JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(control, &object); err != nil || object == nil {
		return RawFrame{}, protocolError(CodeMalformedFrame, "control frame is not a JSON object")
	}
	var env envelope
	if err := json.Unmarshal(control, &env); err != nil || env.T == "" {
		return RawFrame{}, protocolError(CodeMalformedFrame, "control frame has invalid envelope fields")
	}
	if err := validateMandatoryFields(env.T, control); err != nil {
		return RawFrame{}, err
	}
	out := RawFrame{Control: append([]byte(nil), control...), T: env.T, ID: env.ID, Re: env.Re}
	if env.BodyLen == nil {
		return out, nil
	}
	out.HasBody = true
	if *env.BodyLen < 0 {
		return RawFrame{}, protocolError(CodeMalformedFrame, "body_len must be non-negative")
	}
	if *env.BodyLen > MaxBody {
		return RawFrame{}, protocolError(CodeTooLarge, "body exceeds 4 MiB")
	}
	out.Body = make([]byte, int(*env.BodyLen))
	if _, err := io.ReadFull(r.r, out.Body); err != nil {
		return RawFrame{}, protocolError(CodeMalformedFrame, "truncated body trailer")
	}
	return out, nil
}

func validateMandatoryFields(frameType string, control []byte) error {
	switch frameType {
	case "send-ok":
		var required struct {
			Committed      *bool   `json:"committed"`
			DurabilityNote *string `json:"durability_note"`
			OwedNote       *string `json:"owed_note"`
		}
		if err := json.Unmarshal(control, &required); err != nil || required.Committed == nil || required.DurabilityNote == nil || required.OwedNote == nil {
			return protocolError(CodeMalformedFrame, "send-ok requires committed, durability_note, and owed_note")
		}
	case "tick-ok":
		var required struct {
			Warnings *[]string `json:"warnings"`
		}
		if err := json.Unmarshal(control, &required); err != nil || required.Warnings == nil {
			return protocolError(CodeMalformedFrame, "tick-ok requires warnings")
		}
	case "register-ok":
		var required struct {
			Warnings *[]string       `json:"warnings"`
			Announce json.RawMessage `json:"announce"`
		}
		if err := json.Unmarshal(control, &required); err != nil || required.Warnings == nil || required.Announce == nil {
			return protocolError(CodeMalformedFrame, "register-ok requires announce and warnings")
		}
		if !bytes.Equal(required.Announce, []byte("null")) {
			var announce struct {
				Warnings *[]string `json:"warnings"`
			}
			if err := json.Unmarshal(required.Announce, &announce); err != nil || announce.Warnings == nil {
				return protocolError(CodeMalformedFrame, "register-ok announce requires warnings")
			}
		}
	case "note":
		var note Note
		if err := json.Unmarshal(control, &note); err != nil {
			return protocolError(CodeMalformedFrame, "invalid note frame")
		}
		return ValidateNoteLevel(note.Level)
	}
	return nil
}

func (f RawFrame) Decode(dst any) error {
	if err := json.Unmarshal(f.Control, dst); err != nil {
		return protocolError(CodeMalformedFrame, fmt.Sprintf("decode %s frame: %v", f.T, err))
	}
	return nil
}

type Writer struct{ w io.Writer }

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) Write(frame any, body []byte) error {
	line, err := encodeControl(frame, body)
	if err != nil {
		return err
	}
	if err := writeFull(w.w, line); err != nil {
		return err
	}
	if body != nil {
		return writeFull(w.w, body)
	}
	return nil
}

func Encode(frame any, body []byte) ([]byte, error) {
	control, err := encodeControl(frame, body)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(control)+len(body))
	out = append(out, control...)
	out = append(out, body...)
	return out, nil
}

func encodeControl(frame any, body []byte) ([]byte, error) {
	if err := validateOutboundFrame(frame); err != nil {
		return nil, err
	}
	control, err := json.Marshal(frame)
	if err != nil {
		return nil, protocolError(CodeMalformedFrame, fmt.Sprintf("encode control frame: %v", err))
	}
	if body != nil {
		if len(body) > MaxBody {
			return nil, protocolError(CodeTooLarge, "body exceeds 4 MiB")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(control, &fields); err != nil {
			return nil, protocolError(CodeMalformedFrame, "encoded frame is not a JSON object")
		}
		length, _ := json.Marshal(len(body))
		fields["body_len"] = length
		control, err = json.Marshal(fields)
		if err != nil {
			return nil, protocolError(CodeMalformedFrame, fmt.Sprintf("encode body length: %v", err))
		}
	}
	if len(control)+1 > MaxControlLine {
		return nil, protocolError(CodeTooLarge, "control frame exceeds 64 KiB")
	}
	out := make([]byte, 0, len(control)+1)
	out = append(out, control...)
	out = append(out, '\n')
	return out, nil
}

func validateOutboundFrame(frame any) error {
	switch frame := frame.(type) {
	case Note:
		return ValidateNoteLevel(frame.Level)
	case *Note:
		return ValidateNoteLevel(frame.Level)
	case TickOK:
		if frame.Warnings == nil {
			return protocolError(CodeMalformedFrame, "tick-ok requires warnings")
		}
	case *TickOK:
		if frame.Warnings == nil {
			return protocolError(CodeMalformedFrame, "tick-ok requires warnings")
		}
	case RegisterOK:
		if frame.Warnings == nil || (frame.Announce != nil && frame.Announce.Warnings == nil) {
			return protocolError(CodeMalformedFrame, "register-ok requires warnings")
		}
	case *RegisterOK:
		if frame.Warnings == nil || (frame.Announce != nil && frame.Announce.Warnings == nil) {
			return protocolError(CodeMalformedFrame, "register-ok requires warnings")
		}
	}
	return nil
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// EncodeStatus applies the response-line and row budgets to the op's sorted
// rows. The result always ends in LF and never exceeds MaxControlLine.
func EncodeStatus(re int64, resp op.StatusResp) ([]byte, StatusOK, error) {
	out := StatusOK{
		ResponseBase: ResponseBase{T: "status-ok", Re: re},
		Agents:       make([]op.StatusAgent, 0, min(len(resp.Agents), MaxStatusRows)),
		Now:          resp.Now,
	}
	for i, row := range resp.Agents {
		if i >= MaxStatusRows {
			out.Truncated = true
			break
		}
		candidate := out
		candidate.Agents = append(append([]op.StatusAgent(nil), out.Agents...), row)
		candidate.Truncated = false
		line, err := Encode(candidate, nil)
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) && pe.Code == CodeTooLarge {
				out.Truncated = true
				break
			}
			return nil, StatusOK{}, err
		}
		if len(line) > MaxControlLine {
			out.Truncated = true
			break
		}
		out.Agents = append(out.Agents, row)
	}
	if len(out.Agents) < len(resp.Agents) {
		out.Truncated = true
	}
	line, err := Encode(out, nil)
	if err != nil {
		return nil, StatusOK{}, err
	}
	return line, out, nil
}

func (w *Writer) WriteStatus(re int64, resp op.StatusResp) error {
	line, _, err := EncodeStatus(re, resp)
	if err != nil {
		return err
	}
	return writeFull(w.w, line)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func controlLine(encoded []byte) []byte {
	if i := bytes.IndexByte(encoded, '\n'); i >= 0 {
		return encoded[:i+1]
	}
	return encoded
}
