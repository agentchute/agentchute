package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// cmdSelfCheck is the active, hook-safe "I am alive" operation. Unlike
// pending/self-poll, it intentionally writes registration state: last_seen,
// host, and wake_method/wake_target are reconciled with the current process
// environment. It never archives inbox mail.
func cmdSelfCheck(args []string) error {
	fs := flag.NewFlagSet("self-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, wakeMethod, wakeTarget, controlRepo, loopDir, bio string
	var quiet, jsonOut bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, google, local)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
	fs.StringVar(&wakeMethod, "wake-method", "", "wake adapter; leave unset to reconcile from current tmux state")
	fs.StringVar(&wakeTarget, "wake-target", "", "wake target; for tmux, defaults to current $TMUX_PANE when reachable")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.StringVar(&bio, "bio", "", "short self-description for the registration body")
	fs.BoolVar(&quiet, "quiet", false, "suppress success output")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")

	if err := fs.Parse(args); err != nil {
		return selfCheckUsage(err)
	}
	if fs.NArg() != 0 {
		return selfCheckUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	opts := registerOpts{
		Host:               host,
		WakeMethod:         wakeMethod,
		WakeTarget:         wakeTarget,
		Bio:                bio,
		ClearStaleTmuxWake: true,
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
	if err := loop.ValidateAgentID(agentID); err != nil {
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

	status := selfCheckStatus{
		Agent:              agentID,
		Vendor:             opts.Vendor,
		Host:               result.ResolvedHost,
		WakeMethod:         result.ResolvedWakeMethod,
		WakeTarget:         result.ResolvedWakeTarget,
		LastSeen:           result.Reg.LastSeen.UTC().Format(time.RFC3339),
		TmuxPane:           currentTmuxPane(),
		TmuxPaneReachable:  currentTmuxPane() != "" && tmuxTargetReachable(currentTmuxPane()),
		PeerWakeStale:      result.PeerWakeStale,
		PeerWakeStaleCount: len(result.PeerWakeStale),
		Warnings:           result.Warnings,
	}

	switch {
	case jsonOut:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	case quiet:
		return nil
	default:
		emitSelfCheckText(status)
		return nil
	}
}

type selfCheckStatus struct {
	Agent              string          `json:"agent"`
	Vendor             string          `json:"vendor"`
	Host               string          `json:"host,omitempty"`
	WakeMethod         string          `json:"wake_method"`
	WakeTarget         string          `json:"wake_target"`
	LastSeen           string          `json:"last_seen"`
	TmuxPane           string          `json:"tmux_pane,omitempty"`
	TmuxPaneReachable  bool            `json:"tmux_pane_reachable"`
	PeerWakeStaleCount int             `json:"peer_wake_stale_count"`
	PeerWakeStale      []peerWakeStale `json:"peer_wake_stale,omitempty"`
	Warnings           []string        `json:"warnings,omitempty"`
}

func emitSelfCheckText(s selfCheckStatus) {
	fmt.Printf("self-check: %s (%s) last_seen=%s\n", s.Agent, s.Vendor, s.LastSeen)
	if s.WakeMethod != "" {
		fmt.Printf("  wake: %s %s\n", s.WakeMethod, s.WakeTarget)
	} else {
		fmt.Println("  wake: none (recipient must poll)")
	}
	if s.TmuxPane != "" && !s.TmuxPaneReachable {
		fmt.Printf("  tmux: current pane %s is not reachable; wake target cleared\n", s.TmuxPane)
	}
	if s.PeerWakeStaleCount > 0 {
		fmt.Printf("  peer wake stale: %d same-host tmux registration(s) removed\n", s.PeerWakeStaleCount)
	}
	for _, warning := range s.Warnings {
		fmt.Printf("  warning: %s\n", warning)
	}
}

func selfCheckUsage(err error) error {
	if err == flag.ErrHelp {
		return selfCheckHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, selfCheckHelp())
}

func selfCheckHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, selfCheckHelp())
}

func selfCheckHelp() string {
	return strings.TrimSpace(`
Usage: agentchute self-check --as <id> --vendor <vendor> [flags]

Hook-safe active self check. Refreshes/creates this agent's registration,
updates last_seen, reconciles wake_method/wake_target against current tmux
state, and removes stale same-host peer tmux registrations. Does not read,
archive, quarantine, or send inbox messages.

Flags:
  --as <id>              agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>      vendor or origin (anthropic, openai, google, local)
  --host <name>          host (defaults to OS hostname)
  --wake-method <m>      explicit wake adapter (empty clears)
  --wake-target <addr>   explicit wake target
  --bio <text>           short self-description
  --control-repo <p>     control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>         loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --quiet                suppress success output
  --json                 structured JSON output
`)
}
