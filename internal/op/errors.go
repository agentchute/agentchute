package op

import (
	"errors"
	"fmt"

	"github.com/agentchute/agentchute/internal/loop"
)

// The op-layer sentinel set — exactly eight, no more (F6). Four are
// RE-EXPORTS of loop sentinels (plain aliases, NOT redeclared errors), so
// errors.Is matches across both packages and a loop error travelling through
// op still satisfies the CLI's existing checks; four are new.
//
// Every sentinel has exactly one wire code (CodeFor, DESIGN.md §4.4.2).
// Adding a ninth without a code arm fails errors_test.go's list test.
var (
	// ErrNotRegistered is the enforced-enrollment refusal (AGENTCHUTE.md
	// §5.3). ONE sentinel, TWO renderers: send says "sender %q is not
	// registered…", check/status say "agent %q is not registered…" — the
	// texts are not byte-identical today and each call site keeps its own.
	ErrNotRegistered = errors.New("op: agent is not registered")

	// ErrRecipientUnknown / ErrRecipientUnreadable / ErrFenced / ErrLeaseHeld
	// are re-exports: the SAME error values internal/loop raises.
	ErrRecipientUnknown    = loop.ErrRecipientUnknown
	ErrRecipientUnreadable = loop.ErrRecipientUnreadable
	ErrFenced              = loop.ErrFenced
	ErrLeaseHeld           = loop.ErrLeaseHeld

	// ErrRecipientStale is the PREFLIGHT arm (C29b) and ErrRecipientRacing
	// the UNDER-LOCK arm (C29c) of the same underlying condition. The CLI
	// classifies the two by position in cmdSend today; the hub must emit two
	// distinct codes, so the distinction becomes explicit at the seam. Both
	// WRAP the underlying *loop.ErrRecipientStale, reachable via errors.As,
	// because the C29 renderer needs its fields.
	ErrRecipientStale  = errors.New("op: recipient registration is stale")
	ErrRecipientRacing = errors.New("op: recipient registration went stale under the delivery lock")

	// ErrOrder is a Channel.Tick before Channel.Register (§3.4/§6.1).
	ErrOrder = errors.New("op: tick before register on this channel")
)

// staleAt wraps a *loop.ErrRecipientStale under one of the two stale-arm
// sentinels. errors.Is finds the sentinel (which arm), errors.As finds the
// loop error (the fields the C29 text needs).
func staleAt(arm error, cause *loop.ErrRecipientStale) error {
	return fmt.Errorf("%w: %w", arm, cause)
}

// CodeFor maps an error to its wire code (DESIGN.md §4.4.2). A FUNCTION, not
// a table: the default arm is E_HUB_IO, so no non-nil error can ever reach the
// wire without a code. CodeFor(nil) is "".
//
// loop.ErrInboxMissing has no code of its own and takes the default arm
// deliberately — it is an I/O fact about the pool, not a protocol outcome.
func CodeFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotRegistered):
		return "E_NOT_REGISTERED"
	case errors.Is(err, ErrRecipientUnknown):
		return "E_RECIPIENT_UNKNOWN"
	case errors.Is(err, ErrRecipientUnreadable):
		return "E_RECIPIENT_UNREADABLE"
	case errors.Is(err, ErrRecipientStale):
		return "E_RECIPIENT_STALE"
	case errors.Is(err, ErrRecipientRacing):
		return "E_RECIPIENT_RACING"
	case errors.Is(err, ErrFenced):
		return "E_FENCED"
	case errors.Is(err, ErrLeaseHeld):
		return "E_LEASE_HELD"
	case errors.Is(err, ErrOrder):
		return "E_ORDER"
	default:
		return "E_HUB_IO"
	}
}
