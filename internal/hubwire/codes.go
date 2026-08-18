package hubwire

import (
	"errors"

	"github.com/agentchute/agentchute/internal/op"
)

const (
	CodeVersion        = "E_VERSION"
	CodeIdentity       = "E_IDENTITY"
	CodePoolNotFound   = "E_POOL_NOT_FOUND"
	CodePoolIDInvalid  = "E_POOL_ID_INVALID"
	CodePoolMismatch   = "E_POOL_MISMATCH"
	CodeMalformedFrame = "E_MALFORMED_FRAME"
	CodeTooLarge       = "E_TOO_LARGE"
	CodeUnsupported    = "E_UNSUPPORTED"
	// CodeUnpinned: the hub was reached WITHOUT an authorized_keys forced
	// command, so the agent id and pool for the session were chosen by the caller
	// rather than pinned by sshd. Hub-emitted: only the hub can observe it.
	CodeUnpinned = "E_UNPINNED"
)

const (
	EmitterHub    = "hub"
	EmitterClient = "client"
	EmitterBoth   = "both"
)

// Emitters is the complete code-emitter classification. E_POOL_MISMATCH is
// the only code emitted by both sides.
var Emitters = map[string]string{
	"E_NOT_REGISTERED":       EmitterHub,
	"E_RECIPIENT_UNKNOWN":    EmitterHub,
	"E_RECIPIENT_UNREADABLE": EmitterHub,
	"E_RECIPIENT_STALE":      EmitterHub,
	"E_RECIPIENT_RACING":     EmitterHub,
	"E_FENCED":               EmitterHub,
	"E_LEASE_HELD":           EmitterHub,
	"E_ORDER":                EmitterHub,
	"E_HUB_IO":               EmitterHub,
	CodeVersion:              EmitterHub,
	CodeIdentity:             EmitterHub,
	CodePoolNotFound:         EmitterHub,
	CodePoolIDInvalid:        EmitterHub,
	CodePoolMismatch:         EmitterBoth,
	CodeMalformedFrame:       EmitterHub,
	CodeTooLarge:             EmitterHub,
	CodeUnsupported:          EmitterHub,
	CodeUnpinned:             EmitterHub,
	"E_CONNECT":              EmitterClient,
	"E_UNAUTHORIZED":         EmitterClient,
	"E_HOSTKEY_CHANGED":      EmitterClient,
	"E_CHANNEL_LOST":         EmitterClient,
	"E_SEND_UNKNOWN":         EmitterClient,
	"E_HELLO_TIMEOUT":        EmitterClient,
	"E_HUB_NO_BINARY":        EmitterClient,
	"E_NOT_JOINED":           EmitterClient,
	"E_NO_SSH":               EmitterClient,
}

// ProtocolError is a named session/codec failure. Operation errors keep their
// mapping in op.CodeFor; the codec contributes only the eight codes above.
type ProtocolError struct {
	Code        string
	Msg         string
	Retriable   bool
	ClaimedHeld bool
}

func (e *ProtocolError) Error() string { return e.Msg }

func protocolError(code, msg string) error {
	return &ProtocolError{Code: code, Msg: msg}
}

// CodeFor is the complete hub-side error mapping.
func CodeFor(err error) string {
	if err == nil {
		return ""
	}
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return op.CodeFor(err)
}
