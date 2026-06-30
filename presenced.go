package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// presenced is the OPT-IN, OFF-BY-DEFAULT host presence daemon (WI-E4). It
// periodically runs the read-only E1 presence scan and, for a discovered
// wrapper it can identify with HIGH confidence, creates its registration with
// ZERO agent cooperation — the only path that covers bare launches that never
// ran a hook or the runner.
//
// It is BUILT but INERT: nothing in setup, the hook templates, or any
// auto-start path references it. It runs only when a human invokes
// `agentchute presenced`. Default runtime behavior is unchanged.
//
// IDENTITY MIS-ATTRIBUTION is the whole risk, so the high-confidence gate is
// deliberately narrow (see identifyHighConfidencePresence + decidePresenceAction):
// on ANY ambiguity the daemon REPORTS read-only and writes nothing, and it
// NEVER clobbers a registration that belongs to a different identity.
//
// Pull-only (simple-again Gate 6c): a registration publishes NO wake state, so
// the daemon no longer resolves or verifies a reachable wake endpoint, and the
// REPAIR path (add a resolved wake to a no-wake reg) is gone — a registration is
// either present (skip) or absent (create a plain no-wake record). High
// confidence now means only: an in-pool presence whose id maps to exactly one
// known wrapper vendor.

// presenceCandidate is a discovered wrapper presence the daemon has identified
// with HIGH confidence: a single derivable vendor/id for an in-pool presence.
// Only candidates that reach this shape are eligible to be written; everything
// ambiguous is reported instead.
type presenceCandidate struct {
	AgentID string
	Vendor  string
	Cwd     string
	Source  UnenrolledProcess
}

// presenceActionKind is the write decision for one high-confidence candidate.
type presenceActionKind int

const (
	actionCreate       presenceActionKind = iota // no existing reg -> create a no-wake registration
	actionSkipConflict                           // existing reg under a different identity -> never clobber
	actionSkipHealthy                            // existing same-identity reg -> already enrolled, leave it
)

// presenceReport is one line of the daemon's per-pass output: what was
// discovered and what the daemon did (or refused to do, read-only).
type presenceReport struct {
	Source  UnenrolledProcess
	AgentID string // derived id (empty when the candidate was not identified)
	Wrote   bool   // true only when a registration was actually written this pass
	Message string
}

func cmdPresenced(args []string) error {
	fs := flag.NewFlagSet("presenced", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var controlRepo, loopDir string
	var intervalSeconds int
	var once, dryRun bool
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.IntVar(&intervalSeconds, "interval", 60, "scan cadence in seconds")
	fs.BoolVar(&once, "once", false, "run a single scan pass and exit")
	fs.BoolVar(&dryRun, "dry-run", false, "report high-confidence matches but write nothing (read-only)")

	if err := fs.Parse(args); err != nil {
		return presencedUsage(err)
	}
	if fs.NArg() != 0 {
		return presencedUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if intervalSeconds <= 0 {
		return presencedUsage(fmt.Errorf("interval must be positive"))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runPass := func() error {
		reports, err := presencedPass(cfg, dryRun, time.Now().UTC())
		if err != nil {
			return err
		}
		emitPresencedReports(os.Stdout, reports, dryRun)
		return nil
	}

	if once {
		return runPass()
	}

	for {
		if err := runPass(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(intervalSeconds) * time.Second):
		}
	}
}

// presencedPass runs one read-only scan, identifies the high-confidence
// candidates, applies the ambiguity + conflict guards, and (unless dryRun)
// writes the eligible registrations. It returns a per-presence report and never
// returns an error for an individual candidate — one bad candidate must not
// abort the pass. The only error it propagates is a scan failure.
func presencedPass(cfg *loop.Config, dryRun bool, now time.Time) ([]presenceReport, error) {
	found, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		return nil, err
	}

	var reports []presenceReport
	var candidates []presenceCandidate
	idCounts := map[string]int{}

	// Phase 1: identify. Anything not high-confidence is reported read-only.
	//
	// Pull-only: CREATE candidates come from the unenrolled scan (it EXCLUDES
	// every already-registered id, so it can only surface ids with no reg). There
	// is no REPAIR pass — a registration carries no wake to fill in.
	for _, p := range found {
		cand, reason, ok := identifyHighConfidencePresence(cfg, p)
		if !ok {
			reports = append(reports, presenceReport{
				Source:  p,
				Message: "reported (ambiguous, no write): " + reason,
			})
			continue
		}
		candidates = append(candidates, cand)
		idCounts[cand.AgentID]++
	}

	// Phase 2: for each high-confidence candidate, reject cross-candidate
	// ambiguity (two presences mapping to one id), then decide create vs skip
	// against the AUTHORITATIVE on-disk registration. The live write decides AND
	// writes under ONE per-agent lock (commitPresenceAction) so the guard is
	// re-evaluated inside the write lock (Blocker 2).
	for _, cand := range candidates {
		if idCounts[cand.AgentID] > 1 {
			reports = append(reports, presenceReport{
				Source:  cand.Source,
				AgentID: cand.AgentID,
				Message: fmt.Sprintf("reported (ambiguous, no write): %d presences map to derived id %q; cannot attribute identity", idCounts[cand.AgentID], cand.AgentID),
			})
			continue
		}

		// Dry-run: a lock-free decision is sufficient for a read-only preview (no
		// write happens, so there is no decide→write window to protect).
		if dryRun {
			action, reason := decidePresenceAction(cfg, cand)
			switch action {
			case actionSkipConflict, actionSkipHealthy:
				reports = append(reports, presenceReport{
					Source:  cand.Source,
					AgentID: cand.AgentID,
					Message: "reported (no write): " + reason,
				})
			case actionCreate:
				reports = append(reports, presenceReport{
					Source:  cand.Source,
					AgentID: cand.AgentID,
					Message: fmt.Sprintf("dry-run: would register %s (%s)", cand.AgentID, cand.Vendor),
				})
			}
			continue
		}

		action, reason, wrote, cerr := commitPresenceAction(cfg, cand, now)
		switch action {
		case actionSkipConflict, actionSkipHealthy:
			reports = append(reports, presenceReport{
				Source:  cand.Source,
				AgentID: cand.AgentID,
				Message: "reported (no write): " + reason,
			})
		case actionCreate:
			if cerr != nil {
				reports = append(reports, presenceReport{
					Source:  cand.Source,
					AgentID: cand.AgentID,
					Message: fmt.Sprintf("error: register %s failed: %v", cand.AgentID, cerr),
				})
				continue
			}
			reports = append(reports, presenceReport{
				Source:  cand.Source,
				AgentID: cand.AgentID,
				Wrote:   wrote,
				Message: fmt.Sprintf("registered %s (%s)", cand.AgentID, cand.Vendor),
			})
		}
	}

	return reports, nil
}

// identifyHighConfidencePresence maps a discovered presence to a HIGH-CONFIDENCE
// identity. It returns ok=true ONLY when ALL of the following hold:
//
//	(a) cwd→pool: guaranteed by the scan (it only surfaces presences whose cwd
//	    resolves, via loop.Discover, to exactly this pool's control repo);
//	(b) known wrapper, single vendor: the id/name maps to exactly one vendor via
//	    vendorForAgentID (an unrecognized name => vendor ambiguous => not ok).
//
// Pull-only (Gate 6c): there is no wake endpoint to resolve or verify, so a
// herdr agent or a live runner whose id maps to a known vendor is a CREATE
// candidate directly. A tmux pane (no vendor derivable from a pane id) or a raw
// process (no derivable lane identity from the bare command) returns ok=false
// with a reason so the caller REPORTS it read-only instead of guessing.
func identifyHighConfidencePresence(cfg *loop.Config, p UnenrolledProcess) (presenceCandidate, string, bool) {
	switch p.Kind {
	case "herdr":
		name := strings.TrimSpace(p.Hint)
		vendor := vendorForAgentID(name)
		if vendor == "" {
			return presenceCandidate{}, fmt.Sprintf("herdr agent %q: vendor not derivable from name (ambiguous identity)", name), false
		}
		if err := loop.ValidateAgentID(name); err != nil {
			return presenceCandidate{}, fmt.Sprintf("herdr agent %q: not a valid agent id (%v)", name, err), false
		}
		return presenceCandidate{
			AgentID: name,
			Vendor:  vendor,
			Cwd:     p.Cwd,
			Source:  p,
		}, "", true

	case "runner-socket":
		id := strings.TrimSpace(p.Hint)
		vendor := vendorForAgentID(id)
		if vendor == "" {
			return presenceCandidate{}, fmt.Sprintf("runner %q: vendor not derivable from id (ambiguous identity)", id), false
		}
		if err := loop.ValidateAgentID(id); err != nil {
			return presenceCandidate{}, fmt.Sprintf("runner %q: not a valid agent id (%v)", id, err), false
		}
		return presenceCandidate{
			AgentID: id,
			Vendor:  vendor,
			Cwd:     p.Cwd,
			Source:  p,
		}, "", true

	case "tmux":
		// A pane id carries no vendor signal; identifying the lane would require
		// guessing. Report read-only.
		return presenceCandidate{}, fmt.Sprintf("tmux pane %s: vendor not derivable from a pane id (ambiguous identity)", p.Hint), false

	case "process":
		// A raw wrapper process has a derivable vendor but no addressable lane
		// identity to attribute a registration to. Report read-only.
		return presenceCandidate{}, fmt.Sprintf("%s: no derivable lane identity for a raw process (ambiguous identity)", p.Hint), false

	default:
		return presenceCandidate{}, fmt.Sprintf("%s: unrecognized presence kind", p.Kind), false
	}
}

// decidePresenceAction reads the AUTHORITATIVE on-disk registration for the
// candidate's derived id and decides what the daemon may safely do. It is the
// last line of the identity-mis-attribution guardrail and it NEVER writes:
//
//   - no existing reg                  -> actionCreate
//   - existing reg, DIFFERENT vendor   -> actionSkipConflict (never clobber)
//   - existing reg unreadable/corrupt  -> actionSkipConflict (never clobber)
//   - existing reg, SAME vendor        -> actionSkipHealthy (already enrolled)
func decidePresenceAction(cfg *loop.Config, cand presenceCandidate) (presenceActionKind, string) {
	existing, err := loop.ReadRegistration(cfg.AgentRegistrationPath(cand.AgentID))
	return decidePresenceActionFor(cand, existing, err)
}

// decidePresenceActionFor is the pure decision over an ALREADY-READ registration
// (and the error from reading it). It carries the identity-mis-attribution
// guardrail in one place so the lock-free preview (decidePresenceAction / dry-run)
// and the in-lock commit (commitPresenceAction) apply byte-identical rules. The
// caller is responsible for WHERE the read happened: dry-run reads lock-free;
// commitPresenceAction reads under the per-agent lock so the decision is made
// against the authoritative on-disk view the write then publishes (Blocker 2).
func decidePresenceActionFor(cand presenceCandidate, existing *loop.Registration, err error) (presenceActionKind, string) {
	if err != nil {
		if os.IsNotExist(err) {
			return actionCreate, ""
		}
		return actionSkipConflict, fmt.Sprintf("existing registration for %q is unreadable (%v); refusing to overwrite", cand.AgentID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(existing.Vendor), strings.TrimSpace(cand.Vendor)) {
		return actionSkipConflict, fmt.Sprintf("%q already registered under vendor %q; candidate vendor is %q — refusing to clobber a different identity", cand.AgentID, existing.Vendor, cand.Vendor)
	}
	return actionSkipHealthy, fmt.Sprintf("%q already enrolled (vendor %q); nothing to do", cand.AgentID, existing.Vendor)
}

// commitPresenceAction re-evaluates the write decision against the AUTHORITATIVE
// on-disk registration and performs the create write — ALL inside ONE per-agent
// lock (Blocker 2). The guard (no reg→create; different vendor→skip;
// unreadable→skip; same-vendor→skip) is re-run HERE, under the lock, immediately
// before the write, against the same locked view the write publishes. This closes
// the decide→write TOCTOU window: a concurrent create committed between an earlier
// (lock-free) decision and this write can no longer be clobbered, because the
// decision that gates the write is made after acquiring the lock.
//
// It deliberately does NOT route through performRegister: performRegister takes
// its OWN WithAgentLock, so calling it here would acquire the same agent lock
// twice on one call stack and deadlock (see the no-nested-lock contract on
// withAgentLock). Instead it writes the registration directly via the lock-free
// atomic primitives (EnsurePrivateDir + WriteRegistration) while the lock is held.
//
// It returns the action ACTUALLY taken under the lock (which may differ from any
// earlier lock-free preview) so the caller can report truthfully, and wrote=true
// only when a registration was written this call.
func commitPresenceAction(cfg *loop.Config, cand presenceCandidate, now time.Time) (action presenceActionKind, reason string, wrote bool, err error) {
	lockErr := loop.WithAgentLock(cfg, cand.AgentID, func() error {
		regPath := cfg.AgentRegistrationPath(cand.AgentID)
		existing, rerr := loop.ReadRegistration(regPath)
		action, reason = decidePresenceActionFor(cand, existing, rerr)
		switch action {
		case actionSkipConflict, actionSkipHealthy:
			return nil
		case actionCreate:
			return writePresenceReg(cfg, cand, now, &wrote)
		default:
			return nil
		}
	})
	if lockErr != nil {
		err = lockErr
	}
	return action, reason, wrote, err
}

// writePresenceReg publishes a fresh registration for a create decision while the
// caller already holds the agent lock. It ensures the inbox dir BEFORE the
// registration is visible (so a peer can never observe a live reg with no inbox,
// matching performRegister's ordering) and writes via the lock-free atomic
// primitive. On success it sets *wrote=true.
func writePresenceReg(cfg *loop.Config, cand presenceCandidate, now time.Time, wrote *bool) error {
	if err := loop.EnsurePrivateDir(cfg.AgentInboxDir(cand.AgentID)); err != nil {
		return fmt.Errorf("create inbox dir: %w", err)
	}
	reg := buildPresenceRegistration(cfg, cand, now)
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(cand.AgentID), reg); err != nil {
		return err
	}
	*wrote = true
	return nil
}

// buildPresenceRegistration constructs the no-wake registration the daemon writes
// for a high-confidence CREATE candidate. It stamps the `presenced` launch
// provenance and is minimally invasive: no peer pruning, no enrollment announce.
func buildPresenceRegistration(cfg *loop.Config, cand presenceCandidate, now time.Time) *loop.Registration {
	host := ""
	if h, herr := os.Hostname(); herr == nil {
		host = h
	}
	return &loop.Registration{
		AgentID:     cand.AgentID,
		Vendor:      cand.Vendor,
		ControlRepo: cfg.ControlRepo,
		Host:        host,
		LastSeen:    now,
		Status:      loop.StatusActive,
		LaunchedBy:  loop.LaunchedByPresenced,
	}
}

// emitPresencedReports prints a per-pass summary. Read-only / dry-run passes are
// labeled so an operator can tell a reporting pass from a writing one.
func emitPresencedReports(w io.Writer, reports []presenceReport, dryRun bool) {
	mode := "presenced"
	if dryRun {
		mode = "presenced (dry-run)"
	}
	if len(reports) == 0 {
		fmt.Fprintf(w, "%s: no unenrolled wrappers discovered in this pool\n", mode)
		return
	}
	for _, r := range reports {
		fmt.Fprintf(w, "%s: %s\n", mode, r.Message)
	}
}

func presencedUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute presenced [--control-repo <path>] [--loop-dir <path>] [--interval 60] [--once] [--dry-run]\n\nOPT-IN host presence daemon: discovers unenrolled wrapper processes and\nauto-enrolls HIGH-CONFIDENCE matches with zero agent cooperation. OFF by default —\nruns only when invoked here; never wired into setup or hooks. Use --dry-run to preview.", err)
}
