package cli

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
// pending, it intentionally writes registration state: last_seen and
// host are reconciled with the current process environment. It never archives
// inbox mail.
func cmdSelfCheck(args []string) error {
	fs := flag.NewFlagSet("self-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, controlRepo, loopDir, bio string
	var quiet, jsonOut bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, google, xai, local)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
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
		Host:       host,
		Bio:        bio,
		ServeToken: os.Getenv("AGENTCHUTE_SERVE_TOKEN"),
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			opts.HostProvided = true
		case "bio":
			opts.BioProvided = true
		case "vendor":
			opts.VendorProvided = true
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

	now := time.Now().UTC()
	if cfg.Remote != nil && remoteHookCached(cfg) {
		fmt.Fprintln(os.Stderr, "hub unreachable; skipping (will retry next event)")
		return nil
	}
	agentID, result, err := selfRepairRegistration(cfg, &opts, agentID, vendor, now)
	if err != nil {
		if cfg.Remote != nil && degradeRemoteHook(err) {
			return nil
		}
		return err
	}

	status := selfCheckStatus{
		Agent:    agentID,
		Vendor:   result.Reg.Vendor,
		Host:     result.ResolvedHost,
		LastSeen: result.Reg.LastSeen.UTC().Format(time.RFC3339),
		Warnings: result.Warnings,
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

// selfRepairRegistration resolves this process's identity and reconciles its
// live registration (last_seen/host) — exactly what cmdSelfCheck has always
// done. Shared with turn-end step 0 (v2.5 plan A7/C24) so "the agent is
// enrolled and present" can never diverge between the two entry points.
//
// opts must already carry the caller's Host/Bio; this fills in AgentID/Vendor
// on it (hence the pointer — callers that need the
// resolved opts.Vendor afterward, like
// cmdSelfCheck's status report, see it without a second resolve) and returns
// the resolved agent id alongside performRegister's result.
//
// The returned agent id is populated as soon as IDENTITY resolves, even if
// the registration WRITE itself then fails (e.g. --vendor was never passed
// and this id doesn't prefix-match a canonical wrapper base closely enough
// for resolveAgentVendor to backfill one — claude-code review, PR #89: the
// shipped turn-end hook entries omit --vendor by design (C26), and turn-end's
// caller needs a usable id to still run its OTHER steps even when this one
// fails). Only a genuine identity-resolution failure (no id could be
// determined at all) returns "" — that is the one case nothing downstream can
// proceed on.
func selfRepairRegistration(cfg *loop.Config, opts *registerOpts, agentIDFlag, vendorFlag string, now time.Time) (string, *registerResult, error) {
	agentID, err := resolveAgentID(agentIDFlag, cfg)
	if err != nil {
		return "", nil, err
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return "", nil, err
	}
	opts.AgentID = agentID
	if cfg.Remote != nil {
		opts.Vendor = strings.TrimSpace(vendorFlag)
	} else {
		opts.Vendor = resolveAgentVendor(vendorFlag, agentID, cfg)
	}

	result, err := performRegister(cfg, *opts, now)
	if err != nil {
		return agentID, nil, err
	}
	return agentID, result, nil
}

type selfCheckStatus struct {
	Agent    string   `json:"agent"`
	Vendor   string   `json:"vendor"`
	Host     string   `json:"host,omitempty"`
	LastSeen string   `json:"last_seen"`
	Warnings []string `json:"warnings,omitempty"`
}

func emitSelfCheckText(s selfCheckStatus) {
	fmt.Printf("self-check: %s (%s) last_seen=%s\n", s.Agent, s.Vendor, s.LastSeen)
	fmt.Println("  (pull-only: senders deliver to your inbox; you poll it yourself)")
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

Hook-safe active self check. Refreshes/creates this agent's registration and
updates last_seen. Pull-only: a registration publishes no wake state. Does not
read, archive, quarantine, or send inbox messages.

Flags:
  --as <id>              agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>      vendor or origin (anthropic, openai, google, xai, local)
  --host <name>          host (defaults to OS hostname)
  --bio <text>           short self-description
  --control-repo <p>     control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>         loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --quiet                suppress success output
  --json                 structured JSON output
`)
}
