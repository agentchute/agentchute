package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID string
	var controlRepo string
	var loopDir string
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")

	if err := fs.Parse(args); err != nil {
		return statusUsage(err)
	}
	if fs.NArg() != 0 {
		return statusUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	// --as / $AGENTCHUTE_AGENT_ID is now optional. When omitted, status
	// behaves as a pool-overview operator command: it prints the registry
	// without claiming an agent identity and without ticking anyone's
	// last_seen. The acting-agent mode (caller IS one of the pool agents,
	// wants their last_seen refreshed as a side effect) is preserved when
	// --as / env is set. v0.1.2 UX nit per codex review.
	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID != "" {
		if err := loop.ValidateAgentID(agentID); err != nil {
			return err
		}
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

	now := time.Now().UTC()
	if agentID != "" {
		// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): status --as
		// acts AS the agent (refreshes its last_seen). Refuse for an
		// unregistered id. Pool-overview status (no --as) stays
		// unaffected and remains a side-effect-free read.
		selfPath := cfg.AgentRegistrationPath(agentID)
		if _, err := os.Stat(selfPath); err == nil {
			if err := loop.UpdateLastSeen(cfg, agentID, now); err != nil {
				return fmt.Errorf("update last_seen for %s: %w", agentID, err)
			}
		} else if os.IsNotExist(err) {
			return fmt.Errorf("agent %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first, or omit --as to view the pool overview (AGENTCHUTE.md §5.3)", agentID, agentID)
		} else {
			return fmt.Errorf("stat own registration: %w", err)
		}
	}

	regs, err := readRegistrations(cfg)
	if err != nil {
		return err
	}

	printStatus(os.Stdout, cfg, regs, now)
	return nil
}

func statusUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute status [--as <agent-id>] [--control-repo <path>] [--loop-dir <path>]\n\n  --as is optional. With it set, the caller's last_seen is refreshed as a side effect\n  (the historical \"acting-agent\" mode). Without it, status prints a pool overview only.", err)
}

func readRegistrations(cfg *loop.Config) (map[string]*loop.Registration, error) {
	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		// Fresh pool with no agents registered yet: that's a valid state
		// for a read-only pool-overview, not an error.
		if os.IsNotExist(err) {
			return map[string]*loop.Registration{}, nil
		}
		return nil, fmt.Errorf("read agents dir: %w", err)
	}

	regs := make(map[string]*loop.Registration)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "README.md" || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") {
			continue
		}
		path := filepath.Join(cfg.AgentsDir(), name)
		reg, err := loop.ReadRegistration(path)
		if err != nil {
			return nil, err
		}
		regs[reg.AgentID] = reg
	}
	return regs, nil
}

func printStatus(w io.Writer, cfg *loop.Config, regs map[string]*loop.Registration, now time.Time) {
	fmt.Fprintf(w, "control_repo: %s%s\n", cfg.ControlRepo, formatOriginSuffix(cfg.ControlRepoOrigin))
	fmt.Fprintf(w, "loop_dir:     %s%s\n", cfg.LoopDir, formatOriginSuffix(cfg.LoopDirOrigin))
	fmt.Fprintf(w, "vendor:       %s\n", cfg.Vendor)
	for _, shadowed := range cfg.ShadowedPointers {
		fmt.Fprintf(w, "  (shadowed pointer: %s)\n", shadowed)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATUS\tINBOX\tLAST_SEEN\tAGE\tHOST\tWAKE")
	for _, id := range loop.RegistrationsByAgentID(regs) {
		reg := regs[id]
		inboxDepth := countInbox(cfg.AgentInboxDir(id))
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			reg.AgentID,
			reg.Status,
			inboxDepth,
			formatMaybeTime(reg.LastSeen),
			formatAge(now, reg.LastSeen),
			formatDash(reg.Host),
			formatWake(reg.WakeMethod, reg.WakeTarget),
		)
	}
	_ = tw.Flush()
}

func countInbox(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") && !strings.HasPrefix(entry.Name(), ".tmp_") {
			count++
		}
	}
	return count
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func formatMaybeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatAge(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	return age.Round(time.Second).String()
}

// formatOriginSuffix renders the discovery-origin annotation appended to
// control_repo / loop_dir lines in status output. Empty origin (e.g., legacy
// callers that don't set it) renders nothing. See AGENTCHUTE.md §4.
func formatOriginSuffix(origin string) string {
	if origin == "" {
		return ""
	}
	return "   (via " + origin + ")"
}

func formatDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func formatWake(method, target string) string {
	method = strings.TrimSpace(method)
	target = strings.TrimSpace(target)
	if method == "" && target == "" {
		return "-"
	}
	if method == "" {
		return target
	}
	if target == "" {
		return method
	}
	return method + ":" + target
}
