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
			opts.AgentID = nextContextualAgentIDByFilesystem(cfg, opts.ContextualBaseID, opts.AgentID)
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
	// Initial (lock-free) read of `existing` is advisory only: it resolves the
	// wake_method/target, which keys the tmux pane lock and the result fields.
	// The AUTHORITATIVE read that feeds the registration merge is re-taken under
	// the per-agent lock in publishRegistrationOnce (Fix A) so a concurrent
	// locked UpdateLastSeen / markRunnerOffline cannot be clobbered by a stale
	// merge.
	existing, err := loop.ReadRegistration(regPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing registration: %w", err)
	}

	wakeMethod, wakeTarget, preservedFromExisting, warnings := resolveWakeForRegistration(opts, existing)

	publish := func() (*registerResult, error) {
		return publishRegistrationOnce(cfg, opts, host, now, regPath, wakeMethod, wakeTarget, warnings, preservedFromExisting)
	}
	// Lock order is pane -> agent: the tmux pane lock (taken here) wraps the
	// publish closure, and publishRegistrationOnce takes the per-agent lock
	// INSIDE it. No code path takes agent-then-pane (UpdateLastSeen,
	// markRunnerOffline, and clearStaleRunnerWakeTargets take only the agent
	// lock; the peer prunes here are lock-free), so this nesting cannot deadlock.
	if strings.TrimSpace(wakeMethod) == "tmux" && strings.TrimSpace(wakeTarget) != "" {
		return withTmuxPaneRegistrationLock(cfg, host, wakeTarget, publish)
	}
	return publish()
}

func publishRegistrationOnce(cfg *loop.Config, opts registerOpts, host string, now time.Time, regPath string, wakeMethod, wakeTarget string, warnings []string, preservedFromExisting bool) (*registerResult, error) {
	inboxDir := cfg.AgentInboxDir(opts.AgentID)

	var reg *loop.Registration
	var existingFound bool

	// Fix A: the read of `existing`, the merge, the inbox/state dir creation, and
	// the write all run inside ONE per-agent lock so the read-modify-write is
	// atomic against concurrent locked writers (UpdateLastSeen / markRunnerOffline
	// / ledger). This is the agent lock; it nests inside the pane lock (pane ->
	// agent) and must never be re-acquired for the same agentID on this stack —
	// ReadRegistration / WriteRegistration / WriteRegistrationExclusive /
	// EnsurePrivateDir are all lock-free inner helpers, so there is no nesting.
	err := loop.WithAgentLock(cfg, opts.AgentID, func() error {
		// Authoritative re-read under the lock — this is the view the merge writes.
		existing, rerr := loop.ReadRegistration(regPath)
		if rerr == nil {
			existingFound = true
		} else if !os.IsNotExist(rerr) {
			return fmt.Errorf("read existing registration: %w", rerr)
		}

		reg = &loop.Registration{
			AgentID:      opts.AgentID,
			Vendor:       opts.Vendor,
			ControlRepo:  cfg.ControlRepo,
			WorkingRepos: opts.WorkingRepos,
			Host:         host,
			WakeMethod:   wakeMethod,
			WakeTarget:   wakeTarget,
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

		// WI-1 follow-up: when the wake was resolved as "preserve from existing",
		// the wakeMethod/wakeTarget args are a STALE pre-lock snapshot. Re-derive
		// the preserved wake from the AUTHORITATIVE in-lock `existing` so a
		// concurrent locked writer (clearStaleRunnerWakeTargets, another
		// re-register) is not clobbered/resurrected. We deliberately do NOT
		// re-call resolveWakeForRegistration here — it has side effects (herdr
		// pane binding, tmux probing). Only the preserved branch is stale; every
		// fresh-resolved or deliberate-clear value passes through unchanged.
		if preservedFromExisting {
			if existingFound {
				reg.WakeMethod = existing.WakeMethod
				reg.WakeTarget = existing.WakeTarget
			} else {
				// Existing vanished between the pre-lock read and the lock: do not
				// resurrect the stale snapshot — enroll non-pokable.
				reg.WakeMethod = ""
				reg.WakeTarget = ""
			}
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

		if !existingFound && opts.ContextualIdentity {
			// Atomic create-if-not-exists, preserved for the fresh-contextual
			// collision path. EEXIST propagates (via os.IsExist) so performRegister's
			// retry loop can adopt or suffix. Running inside the agent lock keeps it
			// atomic w.r.t. our own concurrent writers; the exclusive link still
			// guards against a different process that never took our lock.
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
		// err is returned verbatim so the contextual-collision retry loop in
		// performRegister can detect the exclusive-create race via os.IsExist
		// (WriteRegistrationExclusive returns the raw os.ErrExist).
		return nil, err
	}

	// The wake actually WRITTEN is reg.WakeMethod/reg.WakeTarget — for the
	// preserve-from-existing branch this is the in-lock re-derived value, not the
	// stale pre-lock args. The same-pane prune and the result's Resolved* fields
	// must report what was published, so read from reg.
	writtenWakeMethod, writtenWakeTarget := reg.WakeMethod, reg.WakeTarget

	// Peer pruning runs OUTSIDE the agent lock: it touches only PEER registration
	// files (never our own) and takes no agent lock, so keeping it out of the
	// locked region keeps that region minimal without changing behavior.
	var peerWakeStale []peerWakeStale
	if opts.PruneStalePeerTmux {
		stale, err := pruneStalePeerTmuxRegistrations(cfg, opts.AgentID)
		if err != nil {
			return nil, fmt.Errorf("prune stale peer tmux registrations: %w", err)
		}
		peerWakeStale = append(peerWakeStale, stale...)
	}
	if strings.TrimSpace(writtenWakeMethod) == "tmux" && strings.TrimSpace(writtenWakeTarget) != "" {
		samePane, err := pruneSamePanePeerTmuxRegistrations(cfg, opts.AgentID, host, writtenWakeTarget)
		if err != nil {
			return nil, fmt.Errorf("prune same-pane tmux registrations: %w", err)
		}
		peerWakeStale = append(peerWakeStale, samePane...)
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

func nextContextualAgentIDByFilesystem(cfg *loop.Config, baseID, current string) string {
	if strings.TrimSpace(baseID) == "" {
		baseID = current
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", baseID, i)
		if candidate == current {
			continue
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath(candidate)); os.IsNotExist(err) {
			return candidate
		}
		if i > 100 {
			return candidate
		}
	}
}

// resolveWakeForRegistration resolves the wake_method/wake_target to publish.
//
// The returned `preservedFromExisting` is true ONLY for the two branches that
// echo back the pre-lock `existing` registration's wake fields verbatim (the
// "tmux pane unreachable, preserve existing" branch and the bare existing
// fallback). For those branches the returned wake values are a STALE pre-lock
// snapshot: publishRegistrationOnce must re-derive them from the authoritative
// in-lock re-read of `existing` instead of writing these. Every other return is
// freshly resolved from live context (opts / current pane / herdr binding) or a
// deliberate clear ("", ""), so preservedFromExisting is false and the returned
// values are written as-is.
func resolveWakeForRegistration(opts registerOpts, existing *loop.Registration) (wakeMethod, wakeTarget string, preservedFromExisting bool, warnings []string) {
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
			// Preserve-from-existing: the returned values are a stale pre-lock
			// snapshot; publishRegistrationOnce re-derives them in-lock.
			return existing.WakeMethod, existing.WakeTarget, true, append(warnings, fmt.Sprintf("TMUX_PANE=%s is not reachable; preserving existing wake target", pane))
		}
		return "", "", false, append(warnings, fmt.Sprintf("TMUX_PANE=%s is not reachable; no wake target registered", pane))
	}

	if existing != nil && strings.TrimSpace(existing.WakeMethod) == "tmux" && opts.ClearStaleTmuxWake {
		return "", "", false, warnings
	}
	if existing != nil {
		// Preserve-from-existing: stale pre-lock snapshot, re-derived in-lock.
		return existing.WakeMethod, existing.WakeTarget, true, warnings
	}
	return "", "", false, warnings
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
