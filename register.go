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
	hostProvided := false
	wakeMethodProvided := false
	wakeTargetProvided := false
	bioProvided := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			hostProvided = true
		case "wake-method":
			wakeMethodProvided = true
		case "wake-target":
			wakeTargetProvided = true
		case "bio":
			bioProvided = true
		}
	})

	// Default --host to OS hostname when not explicit. If os.Hostname() fails,
	// leave empty + warn (per AGENTCHUTE.md §5: empty host = legacy/unknown).
	if !hostProvided {
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
	wakeTargetFromEnv := false
	if !wakeTargetProvided {
		canInferTmuxPane := !wakeMethodProvided || strings.TrimSpace(wakeMethod) == "tmux"
		if canInferTmuxPane {
			if tp := os.Getenv("TMUX_PANE"); tp != "" {
				wakeTarget = tp
				wakeTargetFromEnv = true
				if !wakeMethodProvided {
					wakeMethod = "tmux"
				}
			}
		}
	}
	wakeTargetResolved := wakeTargetProvided || wakeTargetFromEnv

	if fs.NArg() != 0 {
		return registerUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID == "" {
		return fmt.Errorf("missing agent identity; pass --as or set AGENTCHUTE_AGENT_ID")
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}
	vendor = strings.TrimSpace(vendor)
	if vendor == "" {
		return fmt.Errorf("missing --vendor (recommended values: anthropic, openai, local, human)")
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

	// Build the registration. If a live registration already exists, preserve
	// fields the user did not pass on this invocation so re-running register is
	// idempotent for the fields the user names.
	regPath := cfg.AgentRegistrationPath(agentID)
	now := time.Now().UTC()
	reg := &loop.Registration{
		AgentID:      agentID,
		Vendor:       vendor,
		ControlRepo:  cfg.ControlRepo,
		WorkingRepos: workingRepos,
		Host:         host,
		WakeMethod:   wakeMethod,
		WakeTarget:   wakeTarget,
		LastSeen:     now,
		Status:       loop.StatusActive,
	}

	existing, err := loop.ReadRegistration(regPath)
	if err == nil {
		if len(workingRepos) == 0 {
			reg.WorkingRepos = existing.WorkingRepos
		}
		// Host is NOT preserved on re-register: if the agent has moved to a
		// different machine, os.Hostname() should win, not the stale value
		// in the existing registration. Explicit --host overrides above.
		if !wakeMethodProvided && !wakeTargetResolved {
			reg.WakeMethod = existing.WakeMethod
			reg.WakeTarget = existing.WakeTarget
			wakeMethod = existing.WakeMethod
			wakeTarget = existing.WakeTarget
		}
		if existing.LastActive != nil {
			reg.LastActive = existing.LastActive
		}
		reg.Body = existing.Body
		// Status and RestartAt are NOT preserved. `register` means "this agent
		// is active now": an agent previously marked exhausted/offline with a
		// future RestartAt would otherwise stay invisible to watchdog pokes
		// even after re-enrolling.
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing registration: %w", err)
	}

	if bioProvided {
		reg.Body = bio
	}

	if err := loop.WriteRegistration(regPath, reg); err != nil {
		return fmt.Errorf("write registration: %w", err)
	}

	// Create inbox dir per AGENTCHUTE.md §4 ("creates its inbox directory if needed").
	inboxDir := cfg.AgentInboxDir(agentID)
	if err := loop.EnsurePrivateDir(inboxDir); err != nil {
		return fmt.Errorf("create inbox dir: %w", err)
	}

	fmt.Printf("Registered %s\n", agentID)
	fmt.Printf("  vendor:        %s\n", vendor)
	fmt.Printf("  host:          %s\n", host)
	fmt.Printf("  wake_method:   %q\n", wakeMethod)
	fmt.Printf("  wake_target:   %q\n", wakeTarget)
	fmt.Printf("  control_repo:  %s%s\n", cfg.ControlRepo, formatOriginSuffix(cfg.ControlRepoOrigin))
	fmt.Printf("  loop_dir:      %s%s\n", cfg.LoopDir, formatOriginSuffix(cfg.LoopDirOrigin))
	fmt.Printf("  registration:  %s\n", regPath)
	fmt.Printf("  inbox:         %s\n", inboxDir)
	if !reg.IsPokable() {
		fmt.Println("  (non-pokable: senders skip the wake poke; you must poll your own inbox)")
	}

	if announce {
		result, err := loop.AnnounceEnrollment(cfg, reg, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: announce failed: %v\n", err)
		} else {
			for _, w := range result.Warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
			if result.Total == 0 {
				fmt.Println("  announce:      no peers to announce to")
			} else {
				fmt.Printf("  announce:      sent to %d of %d peer(s)\n", result.Sent, result.Total)
			}
		}
	}
	return nil
}

func registerUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute register --as <agent-id> --vendor <vendor> [--host <name>] [--wake-method <adapter>] [--wake-target <addr>] [--bio <text>] [--announce] [--working-repo <path>]... [--control-repo <path>] [--loop-dir <path>]", err)
}
