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
	ServeToken      string

	HostProvided bool
	BioProvided  bool
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
// dedup. The retained behavior is: write the registration record. A fresh
// serve lease owned by another process refuses the registration regardless
// of row age or presence; stale same-id state is merged as crash recovery.
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
	if os.IsExist(err) {
		// WriteRegistrationExclusive closed a creation race with a writer that
		// did not take our agent lock. Re-read once through the normal merge
		// path; its live-owner check decides whether this caller may adopt the
		// now-existing row.
		return publishRegistrationOnce(cfg, opts, host, now)
	}
	return result, err
}

// publishRegistrationOnce writes one registration under the per-agent lock
// (v2.5 plan B5: `.live` is deleted — presence is registration `last_seen`
// age plus, where it matters, a live serve claim). The write is: re-read the
// existing registration (for the field merge), build the no-wake record,
// ensure the inbox dir, then write — exclusively (create-if-not-exists) on a
// fresh row so a concurrent same-id create is re-read before this process may
// merge it.
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
		if registrationLiveElsewhere(cfg, opts.AgentID, opts.ServeToken, now) {
			return fmt.Errorf("agent id %q is live elsewhere; pick a distinct name (--as %s-2?)", opts.AgentID, opts.AgentID)
		}

		reg = &loop.Registration{
			AgentID:         opts.AgentID,
			ProtocolVersion: loop.CurrentProtocolVersion,
			Vendor:          opts.Vendor,
			ControlRepo:     cfg.ControlRepo,
			WorkingRepos:    opts.WorkingRepos,
			Host:            host,
			LastSeen:        now,
		}

		if existingFound {
			if len(opts.WorkingRepos) == 0 {
				reg.WorkingRepos = existing.WorkingRepos
			}
			reg.Body = existing.Body
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

		if !existingFound {
			// Atomic create-if-not-exists. EEXIST propagates so performRegister
			// can re-read the winner and apply the same live-owner refusal before
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
		// Preserve raw os.ErrExist so performRegister can re-read an
		// exclusive-create race exactly once.
		return nil, err
	}

	return &registerResult{
		Reg:           reg,
		InboxDir:      inboxDir,
		Refreshed:     true, // AGENTCHUTE.md §5: any successful boot/register write reports refreshed
		ExistingFound: existingFound,
		ResolvedHost:  host,
	}, nil
}

func registrationLiveElsewhere(cfg *loop.Config, agentID, serveToken string, now time.Time) bool {
	claim, err := loop.ReadServeClaim(cfg, agentID)
	if err != nil || loop.ClaimIsStale(claim, now) {
		return false
	}
	return strings.TrimSpace(serveToken) == "" || claim.ServeToken != strings.TrimSpace(serveToken)
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
		ServeToken:   os.Getenv("AGENTCHUTE_SERVE_TOKEN"),
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

	agentID, err = resolveAgentID(agentID)
	if err != nil {
		return err
	}
	opts.AgentID = agentID
	opts.Vendor = resolveAgentVendor(vendor, agentID, cfg)

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
		ar, err := loop.AnnounceEnrollment(cfg, reg)
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
