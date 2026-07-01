package cli

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
// cmdBoot) parse flags then hand the values here; the helper does host
// detection, the existing-registration merge, and the write.
//
// Pull-only (simple-again Gate 6c): a registration publishes NO wake state, so
// there is no wake-method/wake-target input and no tmux/herdr autodetect. The
// `*Provided` booleans distinguish "flag explicitly cleared to empty" from
// "flag never supplied" — the merge logic for re-registers depends on it.
type registerOpts struct {
	AgentID, Vendor string
	Host            string
	Bio             string
	WorkingRepos    []string

	ContextualIdentity bool
	ContextualBaseID   string
	HostProvided       bool
	BioProvided        bool

	// WI-E3 launch provenance (advisory). When non-empty these are written into
	// the registration so verify views are truthful and the launch-bypass warning
	// can detect a raw launch. Empty values PRESERVE the existing registration's
	// provenance on a re-register (a last_seen-style refresh must not wipe how the
	// lane enrolled), so plain callers that never set them stay byte-identical.
	LaunchedBy string
	ShimName   string
	HookEvent  string
}

// registerResult is performRegister's outcome.
//
// `Refreshed` follows the registration wire semantics (AGENTCHUTE.md §5): true
// whenever performRegister touched the registration file (whether fresh
// enrollment or an update to an existing registration). It is NOT a
// signal of "was there a prior registration"; that distinct semantic is
// `ExistingFound`, used only for UX (text-mode "Refreshed" vs "Registered"
// verb choice) and never serialized into a published wire format.
type registerResult struct {
	Reg           *loop.Registration
	InboxDir      string
	Refreshed     bool   // true on every successful registration write (AGENTCHUTE.md §5).
	ExistingFound bool   // true if a prior registration file existed before this call.
	ResolvedHost  string // post-merge host actually written
	Warnings      []string
}

// performRegister writes / refreshes a registration on disk. Shared between
// register-like commands so host detection and the existing-field merge stay
// centralized.
//
// Pull-only (simple-again Gate 6c): a registration carries no wake state, so
// there is no wake autodetect, no tmux pane lock, and no same-pane/stale-peer
// dedup. The retained behavior is: write the registration record + the initial
// `.live` presence (Gate 3) + the -N contextual-id-collision suffix retry.
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

	result, err := publishRegistrationOnce(cfg, opts, host, now)
	if err == nil {
		return result, nil
	}
	for attempts := 0; opts.ContextualIdentity && os.IsExist(err) && attempts < 100; attempts++ {
		// A concurrent startup command (e.g. boot + self-check fired from the
		// same SessionStart hook) may have just created the contextual id we were
		// about to claim exclusively. Both processes resolved the same contextual
		// base before either write was visible. Under pull-only there is no live
		// pane to map back to a registration, so the colliding id is always a
		// distinct lane: suffix to the next free `<base>-N` and retry.
		nextID, nextErr := nextContextualAgentIDByFilesystem(cfg, opts.ContextualBaseID, opts.AgentID)
		if nextErr != nil {
			return nil, nextErr
		}
		opts.AgentID = nextID
		result, err = publishRegistrationOnce(cfg, opts, host, now)
		if err == nil {
			return result, nil
		}
	}
	return nil, err
}

// publishRegistrationOnce writes one registration under the per-agent lock and
// publishes the initial `.live` presence. The write is: re-read the existing
// registration (for the field merge), build the no-wake record, ensure the inbox
// dir, then write — exclusively (create-if-not-exists) on the fresh-contextual
// path so a concurrent same-id create surfaces as os.ErrExist for the caller's
// suffix retry, otherwise a plain atomic write.
func publishRegistrationOnce(cfg *loop.Config, opts registerOpts, host string, now time.Time) (*registerResult, error) {
	regPath := cfg.AgentRegistrationPath(opts.AgentID)
	inboxDir := cfg.AgentInboxDir(opts.AgentID)

	var reg *loop.Registration
	var existingFound bool

	// The per-agent lock serializes the read-merge-write so a concurrent writer
	// cannot tear the registration or lose a field merge. ReadRegistration /
	// WriteRegistration / WriteRegistrationExclusive / EnsurePrivateDir are all
	// lock-free, so there is no agent-lock self-nesting on this stack.
	err := loop.WithAgentLock(cfg, opts.AgentID, func() error {
		// Authoritative re-read under the lock — the view the merge writes.
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
			LastSeen:     now,
			Status:       loop.StatusActive,
			// WI-E3 launch provenance: a non-empty value from the caller wins (a
			// fresh runner/hook/manual launch updates how the lane enrolled); empty
			// values fall back to the existing registration below.
			LaunchedBy: opts.LaunchedBy,
			ShimName:   opts.ShimName,
			HookEvent:  opts.HookEvent,
		}

		if existingFound {
			if len(opts.WorkingRepos) == 0 {
				reg.WorkingRepos = existing.WorkingRepos
			}
			if existing.LastActive != nil {
				reg.LastActive = existing.LastActive
			}
			// WI-E3: preserve provenance the caller did not supply so a re-register
			// (e.g. a last_seen refresh that goes through performRegister) never
			// wipes the recorded launch provenance.
			if strings.TrimSpace(opts.LaunchedBy) == "" {
				reg.LaunchedBy = existing.LaunchedBy
			}
			if strings.TrimSpace(opts.ShimName) == "" {
				reg.ShimName = existing.ShimName
			}
			if strings.TrimSpace(opts.HookEvent) == "" {
				reg.HookEvent = existing.HookEvent
			}
			reg.Body = existing.Body
			// Status and RestartAt are NOT preserved. `register` / `boot` mean
			// "this agent is active now": an agent previously marked exhausted/
			// offline with a future RestartAt would otherwise stay invisible even
			// after re-enrolling.
		}

		if opts.BioProvided {
			reg.Body = opts.Bio
		}

		// Fix A2: create the inbox (and the agent-state dir) BEFORE publishing the
		// registration so a peer can never observe a live registration with no
		// inbox. A leftover empty inbox dir for an id whose exclusive create then
		// loses the race is harmless.
		if err := loop.EnsurePrivateDir(inboxDir); err != nil {
			return fmt.Errorf("create inbox dir: %w", err)
		}

		if !existingFound && opts.ContextualIdentity {
			// Atomic create-if-not-exists for the fresh-contextual collision path.
			// EEXIST propagates (via os.IsExist) so performRegister's retry loop
			// can suffix. The exclusive link guards against a different process
			// that never took our lock.
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

	// GATE 3: publish an initial `.live` presence fact at the point enrollment
	// is first established. Every enrollment path (boot, register) funnels
	// through here, so this is the single place a freshly registered agent gets
	// its first `.live` — letting it read LIVE immediately, before its first
	// UpdateLastSeen heartbeat tick (runner tick / check / send / status).
	// busy=false: busy is advisory and is set only by serve. WriteLive is a
	// separate atomic file write and takes no agent lock, so emitting it here
	// (after WithAgentLock has returned) is safe. Treated as fatal: with `.live`
	// the source of liveness, a registered agent with no initial `.live` would
	// read stale at gate/doctor until its first tick.
	if err := loop.WriteLive(cfg, opts.AgentID, false); err != nil {
		return nil, fmt.Errorf("write initial .live presence: %w", err)
	}

	return &registerResult{
		Reg:           reg,
		InboxDir:      inboxDir,
		Refreshed:     true, // AGENTCHUTE.md §5: any successful boot/register write reports refreshed
		ExistingFound: existingFound,
		ResolvedHost:  host,
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
		// Enforce the cap BEFORE checking whether the candidate is free —
		// otherwise a free past-cap candidate (e.g. base-101 absent while
		// base-2..base-100 are taken) would be handed out, defeating the cap
		// (codex WI-8 review). Mirrors availableContextualAgentID's ordering.
		if i > 100 {
			return "", fmt.Errorf("could not allocate a free agent id for base %q after %d attempts", baseID, 100)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath(candidate)); os.IsNotExist(err) {
			return candidate, nil
		}
	}
}

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, controlRepo, loopDir, bio string
	var announce bool
	var workingRepos repoListFlag
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, local, human)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
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
		Host:         host,
		Bio:          bio,
		WorkingRepos: workingRepos,
		// A hand-run `agentchute register` is the manual/raw enroll path.
		LaunchedBy: loop.LaunchedByManual,
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			opts.HostProvided = true
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
	fmt.Printf("  control_repo:  %s%s\n", cfg.ControlRepo, formatOriginSuffix(cfg.ControlRepoOrigin))
	fmt.Printf("  loop_dir:      %s%s\n", cfg.LoopDir, formatOriginSuffix(cfg.LoopDirOrigin))
	fmt.Printf("  registration:  %s\n", cfg.AgentRegistrationPath(agentID))
	fmt.Printf("  inbox:         %s\n", result.InboxDir)
	fmt.Println("  (pull-only: senders deliver to your inbox; you poll it yourself)")
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
	return fmt.Errorf("%w\nusage: agentchute register --as <agent-id> --vendor <vendor> [--host <name>] [--bio <text>] [--announce] [--working-repo <path>]... [--control-repo <path>] [--loop-dir <path>]", err)
}
