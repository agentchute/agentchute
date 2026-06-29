package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// presenced is the OPT-IN, OFF-BY-DEFAULT host presence daemon (WI-E4). It
// periodically runs the read-only E1 presence scan and, for a discovered
// wrapper it can identify with HIGH confidence, creates or repairs its
// registration with ZERO agent cooperation — the only path that covers bare
// launches that never ran a hook or the runner.
//
// It is BUILT but INERT: nothing in setup, the hook templates, or any
// auto-start path references it. It runs only when a human invokes
// `agentchute presenced`. Default runtime behavior is unchanged.
//
// IDENTITY MIS-ATTRIBUTION is the whole risk, so the high-confidence gate is
// deliberately narrow (see identifyHighConfidencePresence + decidePresenceAction):
// on ANY ambiguity the daemon REPORTS read-only and writes nothing, and it
// NEVER clobbers a healthy registration that belongs to a different identity.

// presenceHerdrReachable / presenceRunnerReachable are the wake-reachability
// seams the gate consults to confirm a discovered candidate is actually
// reachable (not just present). Package-level vars so tests inject deterministic
// results instead of dialing a real herdr server / runner socket.
var (
	presenceHerdrReachable  = herdrAgentReachable
	presenceRunnerReachable = loop.RunnerSocketReachable
)

// presenceCandidate is a discovered wrapper presence the daemon has identified
// with HIGH confidence: a single derivable vendor/id AND a resolvable, reachable
// wake endpoint. Only candidates that reach this shape are eligible to be
// written; everything ambiguous is reported instead.
type presenceCandidate struct {
	AgentID    string
	Vendor     string
	WakeMethod string
	WakeTarget string
	Cwd        string
	Source     UnenrolledProcess
}

// presenceActionKind is the write decision for one high-confidence candidate.
type presenceActionKind int

const (
	actionCreate       presenceActionKind = iota // no existing reg -> create
	actionRepair                                 // stale same-identity reg with NO wake -> add the resolved wake
	actionSkipConflict                           // existing reg under a different identity -> never clobber
	actionSkipHealthy                            // existing same-identity reg already declares a wake -> leave it
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
	// CREATE candidates come from the unenrolled scan (it EXCLUDES every
	// already-registered id, so it can only surface ids with no reg).
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

	// REPAIR candidates (Blocker 1, path A): EXISTING same-vendor no-wake regs for
	// which a high-confidence reachable wake currently exists. The create scan
	// above hides these (it excludes registered ids), so without this enumeration
	// actionRepair would be unreachable from a daemon pass. The repair and create
	// id-sets are disjoint by construction (create = unregistered ids only, repair
	// = registered no-wake ids only), so this cannot collide with a create id.
	repairCands, err := scanRepairCandidates(cfg)
	if err != nil {
		return nil, err
	}
	for _, cand := range repairCands {
		candidates = append(candidates, cand)
		idCounts[cand.AgentID]++
	}

	// Phase 2: for each high-confidence candidate, reject cross-candidate
	// ambiguity (two presences mapping to one id), then decide create/repair vs
	// skip against the AUTHORITATIVE on-disk registration. The live write decides
	// AND writes under ONE per-agent lock (commitPresenceAction) so the guard is
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
			case actionCreate, actionRepair:
				verb := "register"
				if action == actionRepair {
					verb = "repair"
				}
				reports = append(reports, presenceReport{
					Source:  cand.Source,
					AgentID: cand.AgentID,
					Message: fmt.Sprintf("dry-run: would %s %s (%s) wake=%s %s", verb, cand.AgentID, cand.Vendor, cand.WakeMethod, cand.WakeTarget),
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
		case actionCreate, actionRepair:
			verb, past := "register", "registered"
			if action == actionRepair {
				verb, past = "repair", "repaired"
			}
			if cerr != nil {
				reports = append(reports, presenceReport{
					Source:  cand.Source,
					AgentID: cand.AgentID,
					Message: fmt.Sprintf("error: %s %s failed: %v", verb, cand.AgentID, cerr),
				})
				continue
			}
			reports = append(reports, presenceReport{
				Source:  cand.Source,
				AgentID: cand.AgentID,
				Wrote:   wrote,
				Message: fmt.Sprintf("%s %s (%s) wake=%s %s", past, cand.AgentID, cand.Vendor, cand.WakeMethod, cand.WakeTarget),
			})
		}
	}

	return reports, nil
}

// scanRepairCandidates enumerates EXISTING registrations the presence daemon may
// REPAIR: a registration that carries NO wake (non-pokable) for which a single
// HIGH-CONFIDENCE reachable wake for that EXACT id currently exists (a live herdr
// pane bound to the id, or an answering runner socket for the id). It is the
// counterpart to scanUnenrolledWrappers (which EXCLUDES every registered id and so
// can only surface CREATE candidates); this surfaces the same-vendor no-wake regs
// that the create scan hides, making decidePresenceAction's actionRepair reachable
// from a full pass (Blocker 1, path A).
//
// It is STRICTLY READ-ONLY. It does NOT itself decide same-vendor vs conflict —
// the identity guard is re-applied authoritatively under the lock in
// commitPresenceAction (so a no-wake reg whose stored vendor disagrees with the
// id-derived vendor is surfaced here but SKIPPED there). A reg that already
// declares a wake is never surfaced (the daemon never breaks a working binding,
// even an unreachable one). When zero or MORE THAN ONE reachable presence maps to
// an id, that id is skipped: no presence means nothing to repair with; ambiguity
// means the daemon cannot attribute one reachable wake to the id.
func scanRepairCandidates(cfg *loop.Config) ([]presenceCandidate, error) {
	if cfg == nil {
		return nil, nil
	}
	regs, err := readRegistrations(cfg)
	if err != nil {
		return nil, err
	}

	byID := reachablePresencesByID(cfg)

	var out []presenceCandidate
	for id, reg := range regs {
		if reg.IsPokable() {
			continue // already declares a wake -> never repaired
		}
		if vendorForAgentID(id) == "" {
			continue // id maps to no known wrapper vendor -> not a high-confidence repair
		}
		cands := byID[id]
		if len(cands) != 1 {
			continue // no reachable presence, or ambiguous (>1) -> cannot attribute
		}
		out = append(out, cands[0])
	}
	return out, nil
}

// reachablePresencesByID enumerates the two HIGH-CONFIDENCE presence sources
// (herdr panes + runner sockets) WITHOUT the registration-exclusion filter the
// unenrolled scan applies, runs each through the same identifyHighConfidencePresence
// gate (vendor-derivable + valid id + reachable wake), and indexes the resulting
// candidates by derived id. herdr presences are confirmed to map to THIS pool by
// cwd (matching the create scan's rigor); runner sockets are in-pool by
// construction. tmux panes and raw processes are intentionally omitted — neither
// can ever reach high confidence (no derivable vendor / no addressable wake), so
// neither can supply a repair wake.
func reachablePresencesByID(cfg *loop.Config) map[string][]presenceCandidate {
	byID := map[string][]presenceCandidate{}
	// Same enumeration + in-pool cwd gate the unenrolled (CREATE) scan uses, but
	// WITHOUT its already-registered-id exclusion: the repair path needs the
	// registered no-wake ids the create scan hides. The caller-specific step here
	// is the high-confidence identify + index-by-id (REPAIR); the create scan
	// instead applies its id exclusion and reports low-confidence presences too.
	for _, p := range enumerateInPoolHerdrRunnerPresences(cfg) {
		if cand, _, ok := identifyHighConfidencePresence(cfg, p); ok {
			byID[cand.AgentID] = append(byID[cand.AgentID], cand)
		}
	}
	return byID
}

// identifyHighConfidencePresence maps a discovered presence to a HIGH-CONFIDENCE
// identity + reachable wake. It returns ok=true ONLY when ALL of the following
// hold for the candidate:
//
//	(a) cwd→pool: guaranteed by the scan (it only surfaces presences whose cwd
//	    resolves, via loop.Discover, to exactly this pool's control repo);
//	(b) known wrapper, single vendor: the id/name maps to exactly one vendor via
//	    vendorForAgentID (an unrecognized name => vendor ambiguous => not ok);
//	(c) resolvable + reachable wake: a herdr name that currently resolves to a
//	    live pane, or a runner socket that answers the ping.
//
// Anything else — a tmux pane (no vendor derivable from a pane id) or a raw
// process (no addressable wake) — returns ok=false with a reason so the caller
// REPORTS it read-only instead of guessing an identity.
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
		// The herdr wake target IS the stable bound name; confirm it currently
		// resolves to a live pane so the registration is actually reachable.
		if !presenceHerdrReachable(name) {
			return presenceCandidate{}, fmt.Sprintf("herdr agent %q: wake target does not currently resolve to a live pane", name), false
		}
		return presenceCandidate{
			AgentID:    name,
			Vendor:     vendor,
			WakeMethod: "herdr",
			WakeTarget: name,
			Cwd:        p.Cwd,
			Source:     p,
		}, "", true

	case "runner-socket":
		id := strings.TrimSpace(p.Hint)
		vendor := vendorForAgentID(id)
		if vendor == "" {
			return presenceCandidate{}, fmt.Sprintf("runner socket %q: vendor not derivable from id (ambiguous identity)", id), false
		}
		if err := loop.ValidateAgentID(id); err != nil {
			return presenceCandidate{}, fmt.Sprintf("runner socket %q: not a valid agent id (%v)", id, err), false
		}
		// Reconstruct the in-state socket path the scan enumerated (it only
		// surfaces state/<id>/runner.sock), then re-confirm it still answers.
		target := loop.RunnerWakeTarget(filepath.Join(cfg.LoopDir, "state", id, "runner.sock"))
		if !presenceRunnerReachable(target, presenceProbeTimeout) {
			return presenceCandidate{}, fmt.Sprintf("runner socket %q: not reachable", id), false
		}
		return presenceCandidate{
			AgentID:    id,
			Vendor:     vendor,
			WakeMethod: loop.RunnerWakeMethod,
			WakeTarget: target,
			Cwd:        p.Cwd,
			Source:     p,
		}, "", true

	case "tmux":
		// A pane id carries no vendor signal; identifying the lane would require
		// guessing. Report read-only.
		return presenceCandidate{}, fmt.Sprintf("tmux pane %s: vendor not derivable from a pane id (ambiguous identity)", p.Hint), false

	case "process":
		// A raw wrapper process has a derivable vendor but no addressable wake
		// endpoint, so a registration written for it could not be poked.
		return presenceCandidate{}, fmt.Sprintf("%s: no resolvable wake target for a raw process (not reachable)", p.Hint), false

	default:
		return presenceCandidate{}, fmt.Sprintf("%s: unrecognized presence kind", p.Kind), false
	}
}

// decidePresenceAction reads the AUTHORITATIVE on-disk registration for the
// candidate's derived id and decides what the daemon may safely do. It is the
// last line of the identity-mis-attribution guardrail and it NEVER writes:
//
//   - no existing reg                       -> actionCreate
//   - existing reg, DIFFERENT vendor        -> actionSkipConflict (never clobber)
//   - existing reg unreadable/corrupt       -> actionSkipConflict (never clobber)
//   - existing reg, SAME vendor, pokable    -> actionSkipHealthy (don't churn a working wake)
//   - existing reg, SAME vendor, NO wake    -> actionRepair (fill in the resolved wake)
//
// Repair is deliberately narrow: only a stale reg that carries NO wake is
// repaired (the clear "incomplete enrollment" case). A reg that already declares
// a wake — even a different one — is left untouched so the daemon can never break
// a working binding.
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
	if existing.IsPokable() {
		return actionSkipHealthy, fmt.Sprintf("%q already declares wake %s %s; no repair needed", cand.AgentID, existing.WakeMethod, existing.WakeTarget)
	}
	return actionRepair, ""
}

// commitPresenceAction re-evaluates the write decision against the AUTHORITATIVE
// on-disk registration and performs the create/repair write — ALL inside ONE
// per-agent lock (Blocker 2). The guard (no reg→create; different vendor→skip;
// unreadable→skip; already-pokable→skip; same-vendor no-wake→repair) is re-run
// HERE, under the lock, immediately before the write, against the same locked view
// the write publishes. This closes the decide→write TOCTOU window: a concurrent
// create/repair committed between an earlier (lock-free) decision and this write
// can no longer be clobbered, because the decision that gates the write is made
// after acquiring the lock.
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
			return writePresenceReg(cfg, cand, nil, now, &wrote)
		case actionRepair:
			return writePresenceReg(cfg, cand, existing, now, &wrote)
		default:
			return nil
		}
	})
	if lockErr != nil {
		err = lockErr
	}
	return action, reason, wrote, err
}

// writePresenceReg publishes the registration for a create/repair decision while
// the caller already holds the agent lock. It ensures the inbox dir BEFORE the
// registration is visible (so a peer can never observe a live reg with no inbox,
// matching performRegister's ordering) and writes via the lock-free atomic
// primitive. For a repair it preserves the existing registration's non-wake
// fields and only fills in the resolved wake. On success it sets *wrote=true.
func writePresenceReg(cfg *loop.Config, cand presenceCandidate, existing *loop.Registration, now time.Time, wrote *bool) error {
	if err := loop.EnsurePrivateDir(cfg.AgentInboxDir(cand.AgentID)); err != nil {
		return fmt.Errorf("create inbox dir: %w", err)
	}
	reg := buildPresenceRegistration(cfg, cand, existing, now)
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(cand.AgentID), reg); err != nil {
		return err
	}
	*wrote = true
	return nil
}

// buildPresenceRegistration constructs the registration the daemon writes for a
// high-confidence candidate. It stamps the resolved wake verbatim (no in-pane
// herdr/tmux side effects) and the `presenced` launch provenance, and is minimally
// invasive: no peer pruning, no enrollment announce. For a REPAIR (existing != nil)
// it preserves the existing registration's non-wake fields (working_repos,
// last_active, body, host) so the repair only fills in the missing wake.
func buildPresenceRegistration(cfg *loop.Config, cand presenceCandidate, existing *loop.Registration, now time.Time) *loop.Registration {
	host := ""
	if h, herr := os.Hostname(); herr == nil {
		host = h
	}
	reg := &loop.Registration{
		AgentID:     cand.AgentID,
		Vendor:      cand.Vendor,
		ControlRepo: cfg.ControlRepo,
		Host:        host,
		WakeMethod:  cand.WakeMethod,
		WakeTarget:  cand.WakeTarget,
		LastSeen:    now,
		Status:      loop.StatusActive,
		LaunchedBy:  loop.LaunchedByPresenced,
	}
	if existing != nil {
		reg.WorkingRepos = existing.WorkingRepos
		reg.LastActive = existing.LastActive
		reg.Body = existing.Body
		if strings.TrimSpace(existing.Host) != "" {
			reg.Host = existing.Host
		}
	}
	return reg
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
	return fmt.Errorf("%w\nusage: agentchute presenced [--control-repo <path>] [--loop-dir <path>] [--interval 60] [--once] [--dry-run]\n\nOPT-IN host presence daemon: discovers wrapper processes and auto-enrolls/repairs\nHIGH-CONFIDENCE matches with zero agent cooperation. OFF by default — runs only\nwhen invoked here; never wired into setup or hooks. Use --dry-run to preview.", err)
}
