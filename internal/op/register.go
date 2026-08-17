package op

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// RegisterReq is the registration write path's full input (C2/D1). Three
// deliberately different field shapes:
//
//   - Host is a plain string, CLIENT-resolved (D1a) — not a presence pointer.
//     With no --host flag the LOCAL semantics are not "preserve": the caller
//     substitutes os.Hostname(). Resolving a nil host hub-side would record the
//     HUB's hostname for every remote self-check — the wrong machine — so the
//     caller resolves first and the op treats what it receives as explicit.
//   - Vendor is a presence pointer, hub-resolved when nil (D1b). Bare hook
//     invocations carry no --vendor, and the client-side fallback reads the
//     registration through cfg — which on a remote lane is the mail-free
//     shadow, so a custom id's vendor would never resolve.
//   - Bio is a presence pointer because "nil = keep, non-nil = set (empty
//     clears)" IS the local behavior; without it every remote re-register would
//     clobber a hand-set bio.
//
// Sweep is boot's discriminator: the sweep this op wraps is boot's post-register
// pool hygiene, not every registrant's. Always sweeping would add a pool-wide
// sweep to register, self-check, turn-end's step-0 repair and serve's startup
// registration; never sweeping would strand a remote boot, whose own filesystem
// sweep would walk the mail-free shadow.
//
// ServeToken is what lets a lane's own one-shot ops register while that lane's
// channel holds a fresh serve claim — without it they would be refused as "live
// elsewhere" by their own lease.
type RegisterReq struct {
	Vendor       *string  `json:"vendor,omitempty"`
	Host         string   `json:"host"`
	Bio          *string  `json:"bio,omitempty"`
	WorkingRepos []string `json:"working_repos,omitempty"`
	Announce     bool     `json:"announce,omitempty"`
	Sweep        bool     `json:"sweep,omitempty"`
	ServeToken   string   `json:"serve_token,omitempty"`
}

// RegistrationView is a JSON-tagged mirror of loop.Registration's eight fields,
// under the frontmatter's own key names. A mirror is required, not a
// preference: loop.Registration carries no JSON tags (it is
// frontmatter-serialized) and internal/loop does not change in M1. The two
// conversion funcs below are the single place that mapping lives, and
// StatusAgent reuses four of these tag names verbatim.
type RegistrationView struct {
	AgentID         string    `json:"agent_id"`
	ProtocolVersion int       `json:"v"`
	Vendor          string    `json:"vendor"`
	ControlRepo     string    `json:"control_repo"`
	WorkingRepos    []string  `json:"working_repos,omitempty"`
	Host            string    `json:"host"`
	LastSeen        time.Time `json:"last_seen"`
	Body            string    `json:"body,omitempty"`
}

// AnnounceView is the tagged mirror of loop.AnnounceResult, for the same reason
// RegistrationView is one.
type AnnounceView struct {
	Sent     int      `json:"sent"`
	Total    int      `json:"total"`
	Warnings []string `json:"warnings,omitempty"`
}

// RegisterResp carries the op's own two facts (announce fan-out, post-register
// inbox depth); the other six are FORCED, not optional — they are exactly what
// today's registerResult hands its callers, under the same names and semantics.
// An op returning less could not reproduce what the CLI already renders.
//
// Announce is nil unless the request set Announce. An announce that failed
// outright leaves it nil and appends "announce failed: <err>" to Warnings,
// which the client prints through the same warning loop — same bytes, same
// stderr position.
//
// There is deliberately no Created bool: it is exactly !ExistingFound in every
// returned response, and the boot renderer already derives its verb that way.
type RegisterResp struct {
	Announce      *AnnounceView    `json:"announce,omitempty"`
	Pending       int              `json:"pending"`
	Reg           RegistrationView `json:"reg"`
	InboxDir      string           `json:"inbox_dir"`
	Refreshed     bool             `json:"refreshed"`
	ExistingFound bool             `json:"existing_found"`
	ResolvedHost  string           `json:"resolved_host"`
	Warnings      []string         `json:"warnings,omitempty"`
}

// Register writes / refreshes a registration, then — when asked — sweeps stale
// peers and announces enrollment.
//
// `now` is a NON-WIRE argument, like Context: the hub mints it from its own
// clock (§2) and it never rides a request struct. The local adapter passes its
// caller's clock, which is what keeps performRegister's pinned signature
// meaningful (register_test.go asserts the written LastSeen equals the injected
// value, and it does so from concurrent goroutines with different values, so a
// package-level clock seam would not do).
//
// A fresh serve lease owned by another process refuses the registration
// regardless of row age; stale same-id state is merged as crash recovery.
func Register(cfg *loop.Config, ctx Context, req RegisterReq, now time.Time) (RegisterResp, error) {
	return register(cfg, ctx, req, now, nil)
}

// RegisterWithPrecommitValidation lets a transport reject a response shape
// before the corresponding registration becomes visible. The validator runs
// under the agent lock after the final merge but before any filesystem write.
func RegisterWithPrecommitValidation(cfg *loop.Config, ctx Context, req RegisterReq, now time.Time, validate func(RegisterResp) error) (RegisterResp, error) {
	return register(cfg, ctx, req, now, validate)
}

func register(cfg *loop.Config, ctx Context, req RegisterReq, now time.Time, validate func(RegisterResp) error) (RegisterResp, error) {
	if err := loop.ValidateAgentID(ctx.ActorID); err != nil {
		return RegisterResp{}, err
	}
	// Vendor PRESENCE, not emptiness (D1b). Non-nil is explicit and wins —
	// including an explicit empty, which is a refusal rather than an invitation
	// to guess. Only nil means "the HUB resolves it", and that resolution has to
	// live here rather than in the caller because on a remote lane the caller's
	// own view is the mail-free SHADOW: a custom id's vendor (the roster's
	// "sonnet" is the recorded example) would never resolve there, and every
	// step-0 repair would fail.
	//
	// Testing emptiness instead of presence would silently turn an explicit
	// `--vendor ""` on a canonical id into whatever the id happens to imply
	// (codex, PR #148 gate).
	var vendor string
	if req.Vendor != nil {
		vendor = strings.TrimSpace(*req.Vendor)
	} else {
		vendor = ResolveVendor(cfg, ctx.ActorID)
	}
	if vendor == "" {
		return RegisterResp{}, fmt.Errorf("missing --vendor (recommended values: anthropic, openai, local, human)")
	}

	resp, err := publishRegistrationOnce(cfg, ctx.ActorID, vendor, req, now, validate)
	if os.IsExist(err) {
		// WriteRegistrationExclusive closed a creation race with a writer that
		// did not take our agent lock. Re-read once through the normal merge
		// path; its live-owner check decides whether this caller may adopt the
		// now-existing row.
		resp, err = publishRegistrationOnce(cfg, ctx.ActorID, vendor, req, now, validate)
	}
	if err != nil {
		return RegisterResp{}, err
	}

	// C11's ordering: register self FIRST, then sweep peers. A sweep failure is
	// a warning, never an error — hygiene is best-effort and must not block a
	// session start.
	if req.Sweep {
		if _, serr := loop.SweepStaleRegistrations(cfg, ctx.ActorID, now); serr != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("sweep stale registrations: %v", serr))
		}
	}

	if req.Announce {
		reg := resp.Reg.Registration()
		if ar, aerr := loop.AnnounceEnrollment(cfg, reg); aerr != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("announce failed: %v", aerr))
		} else {
			resp.Announce = &AnnounceView{Sent: ar.Sent, Total: ar.Total, Warnings: ar.Warnings}
		}
	}

	// Post-register inbox depth: the op's own fact, for a caller that cannot
	// stat the pool itself. Best-effort — a listing failure reports zero.
	if msgs, _, lerr := loop.ListInboxMessagesWithSkipped(resp.InboxDir); lerr == nil {
		resp.Pending = len(msgs)
	}
	return resp, nil
}

// publishRegistrationOnce writes one registration under the per-agent lock. The
// write is: re-read the existing registration (for the field merge), build the
// no-wake record, ensure the inbox dir, then write — exclusively
// (create-if-not-exists) on a fresh row, so a concurrent same-id create is
// re-read before this process may merge it.
func publishRegistrationOnce(cfg *loop.Config, agentID, vendor string, req RegisterReq, now time.Time, validate func(RegisterResp) error) (RegisterResp, error) {
	regPath := cfg.AgentRegistrationPath(agentID)
	inboxDir := cfg.AgentInboxDir(agentID)

	var reg *loop.Registration
	var existingFound bool

	// The per-agent lock serializes the read-merge-write so a concurrent writer
	// cannot tear the registration or lose a field merge. ReadRegistration /
	// WriteRegistration / WriteRegistrationExclusive / EnsurePrivateDir are all
	// lock-free, so there is no agent-lock self-nesting on this stack.
	err := loop.WithAgentLock(cfg, agentID, func() error {
		// Authoritative re-read under the lock — the view the merge writes.
		existing, rerr := loop.ReadRegistration(regPath)
		if rerr == nil {
			existingFound = true
		} else if !os.IsNotExist(rerr) {
			return fmt.Errorf("read existing registration: %w", rerr)
		}
		if registrationLiveElsewhere(cfg, agentID, req.ServeToken, now) {
			return fmt.Errorf("agent id %q is live elsewhere; pick a distinct name (--as %s-2?)", agentID, agentID)
		}

		reg = &loop.Registration{
			AgentID:         agentID,
			ProtocolVersion: loop.CurrentProtocolVersion,
			Vendor:          vendor,
			ControlRepo:     cfg.ControlRepo,
			WorkingRepos:    req.WorkingRepos,
			Host:            req.Host,
			LastSeen:        now,
		}

		if existingFound {
			if len(req.WorkingRepos) == 0 {
				reg.WorkingRepos = existing.WorkingRepos
			}
			reg.Body = existing.Body
		}

		if req.Bio != nil {
			reg.Body = *req.Bio
		}

		resp := registerWriteResponse(reg, inboxDir, existingFound, req.Host)
		if validate != nil {
			if verr := validate(resp); verr != nil {
				return verr
			}
		}

		// Fix A2: create the inbox (and the agent-state dir) BEFORE publishing
		// the registration so a peer can never observe a live registration with
		// no inbox. A leftover empty inbox dir for an id whose exclusive create
		// then loses the race is harmless.
		if err := loop.EnsurePrivateDir(inboxDir); err != nil {
			return fmt.Errorf("create inbox dir: %w", err)
		}

		if !existingFound {
			// Atomic create-if-not-exists. EEXIST propagates so the caller can
			// re-read the winner and apply the same live-owner refusal before
			// deciding whether a same-id merge is safe.
			if werr := loop.WriteRegistrationExclusive(regPath, reg); werr != nil {
				if os.IsExist(werr) {
					return werr
				}
				return fmt.Errorf("write registration: %w", werr)
			}
		} else if werr := loop.WriteRegistration(regPath, reg); werr != nil {
			return fmt.Errorf("write registration: %w", werr)
		}
		return nil
	})
	if err != nil {
		// Preserve raw os.ErrExist so the caller can re-read an
		// exclusive-create race exactly once.
		return RegisterResp{}, err
	}

	return registerWriteResponse(reg, inboxDir, existingFound, req.Host), nil
}

func registerWriteResponse(reg *loop.Registration, inboxDir string, existingFound bool, resolvedHost string) RegisterResp {
	return RegisterResp{
		Reg:           RegistrationViewOf(reg),
		InboxDir:      inboxDir,
		Refreshed:     true, // §5: any successful boot/register write reports refreshed
		ExistingFound: existingFound,
		ResolvedHost:  resolvedHost,
	}
}

// registrationLiveElsewhere reports whether a FRESH serve claim owns this id and
// the caller does not hold its token.
func registrationLiveElsewhere(cfg *loop.Config, agentID, serveToken string, now time.Time) bool {
	claim, err := loop.ReadServeClaim(cfg, agentID)
	if err != nil || loop.ClaimIsStale(claim, now) {
		return false
	}
	return strings.TrimSpace(serveToken) == "" || claim.ServeToken != strings.TrimSpace(serveToken)
}

// RegistrationViewOf and RegistrationView.Registration are the two conversion
// funcs — the single place the loop.Registration <-> wire-view mapping lives.
func RegistrationViewOf(reg *loop.Registration) RegistrationView {
	if reg == nil {
		return RegistrationView{}
	}
	return RegistrationView{
		AgentID:         reg.AgentID,
		ProtocolVersion: reg.ProtocolVersion,
		Vendor:          reg.Vendor,
		ControlRepo:     reg.ControlRepo,
		WorkingRepos:    reg.WorkingRepos,
		Host:            reg.Host,
		LastSeen:        reg.LastSeen,
		Body:            reg.Body,
	}
}

func (v RegistrationView) Registration() *loop.Registration {
	return &loop.Registration{
		AgentID:         v.AgentID,
		ProtocolVersion: v.ProtocolVersion,
		Vendor:          v.Vendor,
		ControlRepo:     v.ControlRepo,
		WorkingRepos:    v.WorkingRepos,
		Host:            v.Host,
		LastSeen:        v.LastSeen,
		Body:            v.Body,
	}
}

// ResolveVendor is the hub-side vendor resolution a nil RegisterReq.Vendor
// triggers (D1b): the actor's EXISTING registration row first, then the
// canonical-id table. Empty means neither could name one, which is the
// caller's missing-vendor refusal.
//
// It lives here, not in the CLI, because the row it reads must be the HUB's:
// a remote lane resolving locally would read its mail-free shadow. The CLI's
// resolveAgentVendor keeps its own signature and delegates the same two
// fallbacks here, so there is ONE canonical-id table rather than two that can
// drift.
func ResolveVendor(cfg *loop.Config, agentID string) string {
	if cfg != nil {
		if reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err == nil {
			if v := strings.TrimSpace(reg.Vendor); v != "" {
				return v
			}
		}
	}
	return vendorForAgentID(agentID)
}

// vendorForAgentID maps a canonical wrapper id (or a `<canon>-suffix` variant
// of one) to its vendor.
func vendorForAgentID(agentID string) string {
	switch {
	case MatchesCanonicalID(agentID, "claude-code"):
		return "anthropic"
	case MatchesCanonicalID(agentID, "codex"):
		return "openai"
	case MatchesCanonicalID(agentID, "gemini-cli"):
		return "google"
	case MatchesCanonicalID(agentID, "grok"):
		return "xai"
	default:
		return ""
	}
}

// MatchesCanonicalID reports whether agentID is a canonical wrapper id or a
// `<canon>-suffix` variant of one.
//
// Exported and single-sourced deliberately: internal/cli needs the same
// predicate for wrapper/hook identity, and a verbatim twin in two packages is
// the same drift risk as two vendor tables one level down — change the suffix
// rule in one and vendor resolution silently diverges from hook installation
// (opus-xhigh, PR #148 gate).
func MatchesCanonicalID(agentID, canon string) bool {
	agentID = strings.TrimSpace(agentID)
	canon = strings.TrimSpace(canon)
	return agentID == canon || strings.HasPrefix(agentID, canon+"-")
}
