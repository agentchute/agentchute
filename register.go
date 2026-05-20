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

	HostProvided       bool
	WakeMethodProvided bool
	WakeTargetProvided bool
	BioProvided        bool
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
}

// performRegister writes / refreshes a registration on disk. Shared between
// `register` and `boot --as`. Honors the same TMUX_PANE auto-detection and
// existing-field-preservation rules cmdRegister has always implemented;
// extracted so boot's session-start ritual matches register's behavior bit-
// for-bit without duplicating the merge logic.
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

	// Auto-detect wake_target from TMUX_PANE only when we're sure the recipient
	// wake method is tmux. Two safe cases:
	//   1. Neither flag explicit (common: CLI invoked from inside tmux).
	//   2. --wake-method=tmux is explicit but --wake-target is missing.
	// If --wake-method is explicit and != "tmux" (or explicit ""), we must NOT
	// silently bind a tmux pane address to a different / disabled adapter.
	wakeMethod, wakeTarget := opts.WakeMethod, opts.WakeTarget
	wakeTargetFromEnv := false
	if !opts.WakeTargetProvided {
		canInferTmuxPane := !opts.WakeMethodProvided || strings.TrimSpace(wakeMethod) == "tmux"
		if canInferTmuxPane {
			if tp := os.Getenv("TMUX_PANE"); tp != "" {
				wakeTarget = tp
				wakeTargetFromEnv = true
				if !opts.WakeMethodProvided {
					wakeMethod = "tmux"
				}
			}
		}
	}
	wakeTargetResolved := opts.WakeTargetProvided || wakeTargetFromEnv

	regPath := cfg.AgentRegistrationPath(opts.AgentID)
	reg := &loop.Registration{
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

	existing, err := loop.ReadRegistration(regPath)
	existingFound := false
	if err == nil {
		existingFound = true
		if len(opts.WorkingRepos) == 0 {
			reg.WorkingRepos = existing.WorkingRepos
		}
		// Host is NOT preserved on re-register: if the agent has moved to a
		// different machine, os.Hostname() should win, not the stale value
		// in the existing registration. Explicit --host overrides above.
		if !opts.WakeMethodProvided && !wakeTargetResolved {
			reg.WakeMethod = existing.WakeMethod
			reg.WakeTarget = existing.WakeTarget
			wakeMethod = existing.WakeMethod
			wakeTarget = existing.WakeTarget
		}
		if existing.LastActive != nil {
			reg.LastActive = existing.LastActive
		}
		reg.Body = existing.Body
		// Status and RestartAt are NOT preserved. `register` / `boot` mean
		// "this agent is active now": an agent previously marked exhausted/
		// offline with a future RestartAt would otherwise stay invisible to
		// watchdog pokes even after re-enrolling.
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing registration: %w", err)
	}

	if opts.BioProvided {
		reg.Body = opts.Bio
	}

	if err := loop.WriteRegistration(regPath, reg); err != nil {
		return nil, fmt.Errorf("write registration: %w", err)
	}

	inboxDir := cfg.AgentInboxDir(opts.AgentID)
	if err := loop.EnsurePrivateDir(inboxDir); err != nil {
		return nil, fmt.Errorf("create inbox dir: %w", err)
	}

	return &registerResult{
		Reg:                reg,
		InboxDir:           inboxDir,
		Refreshed:          true, // spec rev3 §A.1: any successful boot write reports refreshed
		ExistingFound:      existingFound,
		ResolvedWakeMethod: wakeMethod,
		ResolvedWakeTarget: wakeTarget,
		ResolvedHost:       host,
	}, nil
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
	fs.StringVar(&wakeMethod, "wake-method", "", "wake adapter (e.g., tmux); leave empty for non-pokable agents")
	fs.StringVar(&wakeTarget, "wake-target", "", "wake target opaque to agentchute; for wake_method=tmux, accepts %pane or session:window.pane")
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
		WakeMethod:   wakeMethod,
		WakeTarget:   wakeTarget,
		Bio:          bio,
		WorkingRepos: workingRepos,
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

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID == "" {
		return fmt.Errorf("missing agent identity; pass --as or set AGENTCHUTE_AGENT_ID")
	}
	opts.AgentID = agentID
	opts.Vendor = strings.TrimSpace(vendor)

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
	if !reg.IsPokable() {
		fmt.Println("  (non-pokable: senders skip the wake poke; you must poll your own inbox)")
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
