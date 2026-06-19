package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// repoListFlag accumulates --working-repo flag occurrences.
type repoListFlag []string

func (r *repoListFlag) String() string { return strings.Join(*r, ",") }
func (r *repoListFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	*r = append(*r, v)
	return nil
}

// registerOpts is the input bundle for performRegister. Callers (cmdRegister,
// cmdBoot) parse flags then hand the values here; the helper does the
// host/wake auto-detection, existing-registration merge, and write. The
// `*Provided` booleans distinguish "flag explicitly cleared to empty" from
// "flag never supplied" — the merge logic for re-registers depends on it.
type registerOpts struct {
	AgentID, Vendor string
	Host            string
	WakeMethod      string
	WakeTarget      string
	Bio             string
	WorkingRepos    []string

	ContextualIdentity bool
	ContextualBaseID   string
	HostProvided       bool
	WakeMethodProvided bool
	WakeTargetProvided bool
	BioProvided        bool
	ClearStaleTmuxWake bool
	PruneStalePeerTmux bool
}

// registerResult is performRegister's outcome.
//
// `Refreshed` follows the v0.1.1 spec rev3 §A.1 wire semantics: true
// whenever performRegister touched the registration file (whether fresh
// enrollment or an update to an existing registration). It is NOT a
// signal of "was there a prior registration"; that distinct semantic is
// `ExistingFound`, used only for UX (text-mode "Refreshed" vs "Registered"
// verb choice) and never serialized into a published wire format.
type registerResult struct {
	Reg                *loop.Registration
	InboxDir           string
	Refreshed          bool   // true on every successful write; matches spec rev3 Test 8.
	ExistingFound      bool   // true if a prior registration file existed before this call.
	ResolvedWakeMethod string // post-merge wake_method actually written (may come from existing reg)
	ResolvedWakeTarget string // post-merge wake_target actually written
	ResolvedHost       string // post-merge host actually written
	PeerWakeStale      []peerWakeStale
	Warnings           []string
}

// performRegister writes / refreshes a registration on disk. Shared between
// register-like commands so host/wake detection and existing-field merge
// behavior stays centralized. By default, an existing tmux wake binding is
// preserved when the current process is not in tmux; hook-driven self-checks
// opt into ClearStaleTmuxWake because they describe the live wrapper process.
func performRegister(cfg *loop.Config, opts registerOpts, now time.Time) (*registerResult, error) {
	if err := loop.ValidateAgentID(opts.AgentID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Vendor) == "" {
		return nil, fmt.Errorf("missing --vendor (recommended values: anthropic, openai, local, human)")
	}

	host := opts.Host
	if !opts.HostProvided {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			fmt.Fprintf(os.Stderr, "warning: os.Hostname() failed (%v); registering with empty host\n", err)
		}
	}

	result, err := performRegisterOnce(cfg, opts, host, now)
	if err == nil {
		return result, nil
	}
	for attempts := 0; opts.ContextualIdentity && os.IsExist(err) && attempts < 100; attempts++ {
		// A concurrent startup command (e.g. boot + self-check fired from the
		// same SessionStart hook) may have just created the same-pane
		// same-vendor registration we were about to claim exclusively. Both
		// processes resolved the same contextual base before either write was
		// visible; suffixing here would mint a spurious "<base>-2" duplicate
		// for one wrapper in one pane. If the pane now owns a matching live id,
		// adopt it — the retry takes the existing-registration merge path and
		// refreshes it in place. Only suffix when the colliding registration
		// belongs to a different pane (a genuine distinct lane).
		if adoptID, ok := agentIDForCurrentHerdrPane(cfg, opts.Vendor); ok {
			opts.AgentID = adoptID
		} else if adoptID, ok := agentIDForCurrentTmuxPane(cfg, opts.Vendor); ok {
			opts.AgentID = adoptID
		} else {
			nextID, nextErr := nextContextualAgentIDByFilesystem(cfg, opts.ContextualBaseID, opts.AgentID)
			if nextErr != nil {
				return nil, nextErr
			}
			opts.AgentID = nextID
		}
		result, err = performRegisterOnce(cfg, opts, host, now)
		if err == nil {
			return result, nil
		}
	}
	return nil, err
}

func performRegisterOnce(cfg *loop.Config, opts registerOpts, host string, now time.Time) (*registerResult, error) {
	regPath := cfg.AgentRegistrationPath(opts.AgentID)
	// PRE-LOCK phase. The initial (lock-free) read of `existing` is advisory: it
	// only feeds the DEFER decision in resolveWakeForRegistration (never the
	// written value). resolveWakeForRegistration also performs the side-effecting
	// live-context resolution (herdr binding, current-pane probe) here, ONCE, so
	// it is not re-run under the lock.
	existing, err := loop.ReadRegistration(regPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing registration: %w", err)
	}

	// liveWakeMethod/Target is the FINAL wake when deferToExisting is false;
	// otherwise it is ignored and the FINAL wake is derived in-lock from the
	// authoritative re-read of `existing`.
	liveWakeMethod, liveWakeTarget, deferToExisting, warnings := resolveWakeForRegistration(opts, existing)

	// Lock order is now agent -> pane (flipped from pane -> agent). The agent lock
	// is OUTERMOST so the FINAL wake — which keys the tmux pane lock and the
	// same-pane prune — is decided against the AUTHORITATIVE in-lock `existing`,
	// not a pre-lock snapshot that a concurrent writer can invalidate. The pane
	// lock is taken INSIDE the agent lock, keyed on the FINAL written target, in
	// publishRegistrationOnce.
	return publishRegistrationOnce(cfg, opts, host, now, regPath, liveWakeMethod, liveWakeTarget, warnings, deferToExisting)
}

// publishRegistrationOnce is the IN-LOCK phase. `liveWakeMethod`/`liveWakeTarget`
// are the FINAL wake when `deferToExisting` is false; when true they are ignored
// and the FINAL wake is derived from the AUTHORITATIVE in-lock re-read of the
// registration. The FINAL wake (not any pre-lock value) keys BOTH the tmux pane
// lock and the same-pane prune, so the two can never target different panes.
func publishRegistrationOnce(cfg *loop.Config, opts registerOpts, host string, now time.Time, regPath string, liveWakeMethod, liveWakeTarget string, warnings []string, deferToExisting bool) (*registerResult, error) {
	inboxDir := cfg.AgentInboxDir(opts.AgentID)

	var reg *loop.Registration
	var existingFound bool
	// samePaneCandidates is the write-time snapshot of same-pane peers FOUND
	// under the self-agent + pane lock, keyed on the FINAL written target. The
	// REMOVE is NOT done here: it would require taking each PEER's agent lock
	// while we still hold our own agent lock + the pane lock, which creates a
	// self-agent -> pane -> peer-agent chain that can deadlock against a peer's
	// own agent -> pane ordering. Instead we collect candidates in-lock and
	// revalidate-remove them AFTER the critical section is released (below).
	var samePaneCandidates []peerWakeStale
	var samePaneHost, samePaneTarget string
	// ourMtime is OUR registration FILE's mtime, stat'd INSIDE the critical
	// section immediately after the write succeeds — the ACTUAL OS publish time.
	// It is carried out of WithAgentLock to the after-lock same-pane removal,
	// where it drives the last-writer-wins tie-breaker. We deliberately use the
	// file mtime (real write order) and NOT reg.LastSeen (the pre-write `now`
	// captured by the caller), because a stalled registrant's LastSeen can be
	// older than its actual write, which misorders the tie-break.
	//
	// ourMtimeKnown tracks whether that stat SUCCEEDED. If it failed we cannot
	// establish OUR own publish order, so the same-pane prune is SKIPPED entirely
	// (fail-closed): without our publish time the tie-break is undefined and a
	// blind delete could wipe a legitimately-newer peer. The stale-peer prune is
	// unaffected (it has no publish-order tie-break).
	var ourMtime time.Time
	var ourMtimeKnown bool

	// Lock order is agent -> pane. The per-agent lock is OUTERMOST so the read of
	// `existing`, the FINAL-wake decision, the merge, the inbox dir creation, the
	// write, AND the same-pane prune all see one consistent, serialized view. The
	// agent lock is held slightly longer than before because the tmux pane lock is
	// now acquired INSIDE it (bounded by the pane lock's own 5s timeout / 30s
	// stale-break); that is the correctness/latency trade and is acceptable
	// because withTmuxPaneRegistrationLock has exactly ONE caller (here), so no
	// other path takes pane-then-agent and this nesting cannot deadlock.
	//
	// No agent-lock self-nesting on this stack: ReadRegistration /
	// WriteRegistration / WriteRegistrationExclusive / EnsurePrivateDir and the
	// peer-prune helpers are all lock-free; the pane lock is a DIFFERENT lock
	// (a mkdir lock keyed on host+target, never an agent lock).
	err := loop.WithAgentLock(cfg, opts.AgentID, func() error {
		// Authoritative re-read under the lock — this is the view the merge writes
		// and the view the FINAL wake is decided against.
		existing, rerr := loop.ReadRegistration(regPath)
		if rerr == nil {
			existingFound = true
		} else if !os.IsNotExist(rerr) {
			return fmt.Errorf("read existing registration: %w", rerr)
		}

		// Decide the FINAL wake against the AUTHORITATIVE in-lock `existing`.
		// When deferToExisting is true the live phase found no wake, so honor the
		// in-lock existing's wake (or empty if no existing) — this both preserves
		// an existing binding across an unreachable pane AND lets a concurrent
		// nil->existing create supply the wake instead of being clobbered with
		// empty (defect #2). Otherwise the live/explicit/clear value stands.
		finalWakeMethod, finalWakeTarget := liveWakeMethod, liveWakeTarget
		if deferToExisting {
			if existingFound {
				finalWakeMethod, finalWakeTarget = existing.WakeMethod, existing.WakeTarget
			} else {
				finalWakeMethod, finalWakeTarget = "", ""
			}
		}

		reg = &loop.Registration{
			AgentID:      opts.AgentID,
			Vendor:       opts.Vendor,
			ControlRepo:  cfg.ControlRepo,
			WorkingRepos: opts.WorkingRepos,
			Host:         host,
			WakeMethod:   finalWakeMethod,
			WakeTarget:   finalWakeTarget,
			LastSeen:     now,
			Status:       loop.StatusActive,
		}

		if existingFound {
			if len(opts.WorkingRepos) == 0 {
				reg.WorkingRepos = existing.WorkingRepos
			}
			if existing.LastActive != nil {
				reg.LastActive = existing.LastActive
			}
			reg.Body = existing.Body
			// Status and RestartAt are NOT preserved. `register` / `boot` mean
			// "this agent is active now": an agent previously marked exhausted/
			// offline with a future RestartAt would otherwise stay invisible to
			// watchdog pokes even after re-enrolling.
		}

		if opts.BioProvided {
			reg.Body = opts.Bio
		}

		// Fix A2: create the inbox (and the agent-state dir) BEFORE publishing the
		// registration so a peer can never observe a live registration with no
		// inbox. (WithAgentLock already created the state dir for the lockfile; we
		// ensure the inbox here, still pre-write.) A leftover empty inbox dir for an
		// id whose exclusive create then loses the race is harmless.
		if err := loop.EnsurePrivateDir(inboxDir); err != nil {
			return fmt.Errorf("create inbox dir: %w", err)
		}

		// commit writes the registration and FINDS (does NOT remove) the same-pane
		// peers, both keyed on the FINAL target. When the FINAL wake is tmux, both
		// run under the pane lock (taken just below); otherwise both run under the
		// agent lock only. The same-pane REMOVE is deliberately deferred to AFTER
		// the critical section (see samePaneCandidates above): removing requires the
		// PEER's agent lock, which must not be taken while we hold our own agent
		// lock + the pane lock.
		commit := func() error {
			if !existingFound && opts.ContextualIdentity {
				// Atomic create-if-not-exists, preserved for the fresh-contextual
				// collision path. EEXIST propagates (via os.IsExist) so
				// performRegister's retry loop can adopt or suffix. Running inside the
				// agent lock keeps it atomic w.r.t. our own concurrent writers; the
				// exclusive link still guards against a different process that never
				// took our lock.
				if werr := loop.WriteRegistrationExclusive(regPath, reg); werr != nil {
					if os.IsExist(werr) {
						return werr
					}
					return fmt.Errorf("write registration: %w", werr)
				}
			} else if werr := loop.WriteRegistration(regPath, reg); werr != nil {
				return fmt.Errorf("write registration: %w", werr)
			}

			// Capture OUR file mtime = the ACTUAL publish time, in-lock, right
			// after the write succeeds. This is the publish-order signal the
			// same-pane tie-breaker uses (instead of the pre-write reg.LastSeen).
			// ourMtimeKnown records stat success; on failure the same-pane prune
			// is skipped below (fail-closed — no own publish order, no safe prune).
			if info, serr := os.Stat(regPath); serr == nil {
				ourMtime = info.ModTime()
				ourMtimeKnown = true
			}

			// Same-pane FIND keyed on the FINAL written target — a write-time
			// snapshot taken under the pane lock (when tmux) so it is consistent with
			// the target we just wrote. This is a pure scan: it takes no agent lock
			// and removes nothing, so running it inside our agent lock introduces no
			// self-nesting. The actual revalidated REMOVE runs after WithAgentLock
			// returns, with the self-agent + pane locks already released.
			if strings.TrimSpace(reg.WakeMethod) == "tmux" && strings.TrimSpace(reg.WakeTarget) != "" {
				peers, perr := findSamePanePeerTmuxRegistrations(cfg, opts.AgentID, host, reg.WakeTarget)
				if perr != nil {
					return fmt.Errorf("find same-pane tmux registrations: %w", perr)
				}
				samePaneCandidates = peers
				samePaneHost = host
				samePaneTarget = reg.WakeTarget
			}
			return nil
		}

		// agent -> pane: take the tmux pane lock (keyed on the FINAL target) INSIDE
		// the agent lock for tmux writes, so the write + same-pane prune are
		// serialized per pane. Non-tmux / empty-target writes need no pane lock.
		if strings.TrimSpace(reg.WakeMethod) == "tmux" && strings.TrimSpace(reg.WakeTarget) != "" {
			_, perr := withTmuxPaneRegistrationLock(cfg, host, reg.WakeTarget, func() (*registerResult, error) {
				return nil, commit()
			})
			return perr
		}
		return commit()
	})
	if err != nil {
		// err is returned verbatim so the contextual-collision retry loop in
		// performRegister can detect the exclusive-create race via os.IsExist
		// (WriteRegistrationExclusive returns the raw os.ErrExist).
		return nil, err
	}

	// The wake actually WRITTEN is reg.WakeMethod/reg.WakeTarget (the FINAL,
	// in-lock value). The result's Resolved* fields report what was published.
	writtenWakeMethod, writtenWakeTarget := reg.WakeMethod, reg.WakeTarget

	// Same-pane REMOVE runs HERE, AFTER WithAgentLock has returned, so the
	// self-agent lock and the tmux pane lock are both released. Each removal takes
	// the PEER's own agent lock, re-reads the peer reg, and deletes ONLY if the
	// fresh reg still maps to our FINAL pane (a peer that moved pane / went
	// non-tmux between the in-lock FIND and this REMOVE keeps its valid reg). With
	// our own locks released, at most one peer lock is held at a time, so there is
	// no self-agent -> pane -> peer-agent chain and no AB-BA deadlock with a peer's
	// own agent -> pane ordering. The reported set reflects what was ACTUALLY
	// removed post-revalidation, not the raw find candidates.
	var peerWakeStale []peerWakeStale
	if len(samePaneCandidates) > 0 && ourMtimeKnown {
		// ourMtime is OUR registration FILE's mtime, stat'd in-lock right after
		// the write (the ACTUAL publish time). It feeds the last-writer-wins
		// tie-breaker so a reciprocal same-pane delete leaves exactly one survivor
		// (the actual later writer, by real OS write order) rather than wiping
		// both regs. mtime is used over reg.LastSeen because LastSeen is the
		// pre-write `now` and can be stale relative to a stalled writer's write.
		//
		// FAIL-CLOSED: the prune runs ONLY when ourMtimeKnown — if the in-lock
		// stat of our own reg failed we have no publish order for OURSELVES, so
		// the tie-break is undefined and we skip same-pane pruning entirely rather
		// than risk deleting a legitimately-newer peer on uncertainty. (The peer
		// side is independently fail-closed in revalidateAndRemovePeer.)
		removed, perr := revalidateAndRemoveSamePanePeers(cfg, samePaneCandidates, opts.AgentID, samePaneHost, samePaneTarget, ourMtime)
		if perr != nil {
			return nil, fmt.Errorf("prune same-pane tmux registrations: %w", perr)
		}
		peerWakeStale = append(peerWakeStale, removed...)
	}
	// Stale different-pane peer pruning runs OUTSIDE both locks: it scans all peer
	// registrations for unreachable tmux targets, then revalidates+removes each
	// under that PEER's own agent lock (no self-agent or pane lock held here). It
	// is independent of our FINAL target (and of the pane lock), so keeping it out
	// of the locked region keeps that region minimal.
	if opts.PruneStalePeerTmux {
		stale, err := pruneStalePeerTmuxRegistrations(cfg, opts.AgentID)
		if err != nil {
			return nil, fmt.Errorf("prune stale peer tmux registrations: %w", err)
		}
		peerWakeStale = append(peerWakeStale, stale...)
	}

	return &registerResult{
		Reg:                reg,
		InboxDir:           inboxDir,
		Refreshed:          true, // spec rev3 §A.1: any successful boot write reports refreshed
		ExistingFound:      existingFound,
		ResolvedWakeMethod: writtenWakeMethod,
		ResolvedWakeTarget: writtenWakeTarget,
		ResolvedHost:       host,
		PeerWakeStale:      peerWakeStale,
		Warnings:           warnings,
	}, nil
}

func nextContextualAgentIDByFilesystem(cfg *loop.Config, baseID, current string) (string, error) {
	if strings.TrimSpace(baseID) == "" {
		baseID = current
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", baseID, i)
		if candidate == current {
			continue
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath(candidate)); os.IsNotExist(err) {
			return candidate, nil
		}
		if i > 100 {
			return "", fmt.Errorf("could not allocate a free agent id for base %q after %d attempts", baseID, 100)
		}
	}
}

// resolveWakeForRegistration is the PRE-LOCK phase of wake resolution. It runs
// the side-effecting, live-context resolution (herdr pane binding, current tmux
// pane + reachability probe, explicit opts) BEFORE the agent lock and must not be
// re-run in-lock. It does NOT depend on a stable view of `existing` (the `existing`
// arg is only consulted to decide whether to DEFER, never to produce the written
// value).
//
// The returned `deferToExisting` partitions the result into two in-lock behaviors
// (see publishRegistrationOnce):
//
//   - deferToExisting=false — a wake was resolved from LIVE context, or the caller
//     deliberately CLEARED it. The returned (wakeMethod, wakeTarget) is the FINAL
//     wake and is written verbatim. Covers: explicit --wake-* flags, a reachable
//     current pane, a successful herdr binding, and the deliberate clears
//     (ClearStaleTmuxWake, herdr-bind-failure, explicit non-tmux without target).
//
//   - deferToExisting=true — NO live wake was found. The returned values are
//     irrelevant; the FINAL wake is derived in-lock from the AUTHORITATIVE re-read
//     of `existing` (its wake if present, else empty). This covers both the
//     "preserve existing across an unreachable/absent pane" branches AND the
//     fresh-no-context fallthrough. Deferring the fallthrough (rather than writing
//     empty) is what stops a stale/empty pre-lock decision from clobbering a wake
//     written by a concurrent nil->existing create seen only under the lock.
func resolveWakeForRegistration(opts registerOpts, existing *loop.Registration) (wakeMethod, wakeTarget string, deferToExisting bool, warnings []string) {
	wakeMethod, wakeTarget = opts.WakeMethod, opts.WakeTarget

	if opts.WakeMethodProvided || opts.WakeTargetProvided {
		if !opts.WakeTargetProvided && strings.TrimSpace(wakeMethod) == "tmux" {
			if pane := currentTmuxPane(); pane != "" && tmuxTargetReachable(pane) {
				wakeTarget = pane
			} else if pane != "" {
				warnings = append(warnings, fmt.Sprintf("TMUX_PANE=%s is not reachable; explicit wake_method=tmux still needs --wake-target", pane))
			}
		}
		// Explicit wake_method=herdr without a target: bind this pane to the
		// agent id and use that stable name as the target.
		if !opts.WakeTargetProvided && strings.TrimSpace(wakeMethod) == "herdr" {
			method, target, herdrWarnings, ok := herdrWakeForRegistration(opts)
			warnings = append(warnings, herdrWarnings...)
			if ok {
				return method, target, false, warnings
			}
			// Could not bind (collision / rename failure / no herdr binary):
			// clear method+target so the agent enrolls non-pokable rather than
			// with an invalid wake_method=herdr and an empty target (which
			// registration validation would reject).
			return "", "", false, warnings
		}
		return wakeMethod, wakeTarget, false, warnings
	}

	// Native herdr wake for bare launches inside a herdr pane. Skipped when
	// running under `agentchute run` (the runner socket owns the wake — never
	// switch to herdr just because HERDR_ENV is also set) and takes precedence
	// over tmux when both terminal envs are present.
	if !underAgentchuteRunner() && herdrEnvActive() {
		method, target, herdrWarnings, ok := herdrWakeForRegistration(opts)
		warnings = append(warnings, herdrWarnings...)
		if ok {
			return method, target, false, warnings
		}
		// Binding failed (collision or rename error): fall through to tmux /
		// existing-target preservation below.
	}

	if pane := currentTmuxPane(); pane != "" {
		if tmuxTargetReachable(pane) {
			return "tmux", pane, false, warnings
		}
		if opts.ClearStaleTmuxWake {
			return "", "", false, append(warnings, fmt.Sprintf("TMUX_PANE=%s is not reachable; clearing tmux wake target", pane))
		}
		if existing != nil {
			// Defer-to-existing: no live wake; the in-lock authoritative `existing`
			// supplies the FINAL wake. Returned values are ignored by the publish.
			return "", "", true, append(warnings, fmt.Sprintf("TMUX_PANE=%s is not reachable; preserving existing wake target", pane))
		}
		// No live wake AND no pre-lock existing: still DEFER (not a deliberate
		// clear). A concurrent nil->existing create seen only under the lock then
		// supplies the wake instead of being clobbered with empty (defect #2).
		return "", "", true, append(warnings, fmt.Sprintf("TMUX_PANE=%s is not reachable; no wake target registered", pane))
	}

	if existing != nil && strings.TrimSpace(existing.WakeMethod) == "tmux" && opts.ClearStaleTmuxWake {
		// Deliberate clear of a stale tmux wake by the live wrapper process.
		return "", "", false, warnings
	}
	if existing != nil {
		// Defer-to-existing: no live context; in-lock `existing` supplies the wake.
		return "", "", true, warnings
	}
	// No live context, no pre-lock existing: DEFER so an in-lock nil->existing
	// create is honored, not clobbered with empty (defect #2).
	return "", "", true, warnings
}

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, wakeMethod, wakeTarget, controlRepo, loopDir, bio string
	var announce bool
	var workingRepos repoListFlag
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, local, human)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
	fs.StringVar(&wakeMethod, "wake-method", "", "wake adapter (e.g., tmux, herdr); leave empty for non-pokable agents")
	fs.StringVar(&wakeTarget, "wake-target", "", "wake target opaque to agentchute; tmux accepts %pane or session:window.pane; herdr defaults to the agent id (stable name)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.StringVar(&bio, "bio", "", "short self-description for the registration body (markdown allowed)")
	fs.BoolVar(&announce, "announce", false, "after registering, send a direct enrollment notification to every existing peer")
	fs.Var(&workingRepos, "working-repo", "additional repo this agent edits (repeatable)")

	if err := fs.Parse(args); err != nil {
		return registerUsage(err)
	}

	// Track which fields the caller explicitly named so re-running register
	// preserves existing registration values for fields the user did not pass.
	// Explicit "" still clears.
	opts := registerOpts{
		Host:               host,
		WakeMethod:         wakeMethod,
		WakeTarget:         wakeTarget,
		Bio:                bio,
		WorkingRepos:       workingRepos,
		PruneStalePeerTmux: true,
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			opts.HostProvided = true
		case "wake-method":
			opts.WakeMethodProvided = true
		case "wake-target":
			opts.WakeTargetProvided = true
		case "bio":
			opts.BioProvided = true
		}
	})

	if fs.NArg() != 0 {
		return registerUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
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

	contextualBase, contextual, err := contextualIdentityBase(agentID, vendor)
	if err != nil {
		return err
	}
	agentID, err = resolveAgentID(agentID, vendor, cfg)
	if err != nil {
		return err
	}
	opts.AgentID = agentID
	opts.Vendor = resolveAgentVendor(vendor, agentID, cfg)
	opts.ContextualIdentity = contextual
	opts.ContextualBaseID = contextualBase

	now := time.Now().UTC()
	result, err := performRegister(cfg, opts, now)
	if err != nil {
		return err
	}
	reg := result.Reg

	fmt.Printf("Registered %s\n", agentID)
	fmt.Printf("  vendor:        %s\n", opts.Vendor)
	fmt.Printf("  host:          %s\n", result.ResolvedHost)
	fmt.Printf("  wake_method:   %q\n", result.ResolvedWakeMethod)
	fmt.Printf("  wake_target:   %q\n", result.ResolvedWakeTarget)
	fmt.Printf("  control_repo:  %s%s\n", cfg.ControlRepo, formatOriginSuffix(cfg.ControlRepoOrigin))
	fmt.Printf("  loop_dir:      %s%s\n", cfg.LoopDir, formatOriginSuffix(cfg.LoopDirOrigin))
	fmt.Printf("  registration:  %s\n", cfg.AgentRegistrationPath(agentID))
	fmt.Printf("  inbox:         %s\n", result.InboxDir)
	if len(result.PeerWakeStale) > 0 {
		fmt.Printf("  pruned_tmux:   %d stale same-host peer registration(s)\n", len(result.PeerWakeStale))
	}
	if !reg.IsPokable() {
		fmt.Println("  (non-pokable: senders skip the wake poke; you must poll your own inbox)")
	}
	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	if announce {
		ar, err := loop.AnnounceEnrollment(cfg, reg, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: announce failed: %v\n", err)
		} else {
			for _, w := range ar.Warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
			if ar.Total == 0 {
				fmt.Println("  announce:      no peers to announce to")
			} else {
				fmt.Printf("  announce:      sent to %d of %d peer(s)\n", ar.Sent, ar.Total)
			}
		}
	}
	return nil
}

func registerUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute register --as <agent-id> --vendor <vendor> [--host <name>] [--wake-method <adapter>] [--wake-target <addr>] [--bio <text>] [--announce] [--working-repo <path>]... [--control-repo <path>] [--loop-dir <path>]", err)
}
