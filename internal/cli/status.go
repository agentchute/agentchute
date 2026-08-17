package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
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
	agentID, err = resolveAgentID(agentID, cfg)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	// Read errors arrive as warn notes DURING the read, so they land on stderr
	// before the table exactly as they always have — position included.
	emit := func(ev op.Event) error {
		if ev.Note != nil && ev.Note.Level == op.NoteWarn {
			fmt.Fprintf(os.Stderr, "warning: %s\n", ev.Note.Msg)
		}
		return nil
	}
	var resp op.StatusResp
	if cfg.Remote != nil {
		session, openErr := openRemoteOneShot(cfg, agentID)
		if openErr != nil {
			return openErr
		}
		resp, err = session.Status(emit)
	} else {
		resp, err = op.Status(cfg, op.Context{ActorID: agentID}, op.StatusReq{}, emit)
	}
	if err != nil {
		if errors.Is(err, op.ErrNotRegistered) {
			return fmt.Errorf("agent %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", agentID, agentID)
		}
		return err
	}

	if cfg.Remote != nil {
		printRemoteStatus(os.Stdout, cfg, resp)
	} else {
		printStatus(os.Stdout, cfg, registrationsOf(resp.Agents), now)
	}
	return nil
}

// registrationsOf rebuilds the map printStatus takes from the seam's status
// rows. The registrations are PARTIAL by design — only the four fields
// printStatus, registrationStatusLabel and protocolVersionWarning actually read
// — and never escape cmdStatus. The rows also carry InboxDepth and Status,
// which the local renderer re-derives itself; WI-4.5 is where it switches onto
// them, because a remote client can neither stat the hub's inboxes nor read its
// claims.
func registrationsOf(agents []op.StatusAgent) map[string]*loop.Registration {
	regs := make(map[string]*loop.Registration, len(agents))
	for _, a := range agents {
		regs[a.AgentID] = &loop.Registration{
			AgentID:         a.AgentID,
			LastSeen:        a.LastSeen,
			Host:            a.Host,
			ProtocolVersion: a.ProtocolVersion,
		}
	}
	return regs
}

func statusUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute status --as <agent-id> [--control-repo <path>] [--loop-dir <path>]", err)
}

func printStatus(w io.Writer, cfg *loop.Config, regs map[string]*loop.Registration, now time.Time) {
	printStatusHeader(w, cfg)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Pull-only (Gate 6c): registrations carry no wake state. There is no
	// WAKE / REACHABLE / CACHED column; LAST_SEEN/AGE come from the
	// registration's own heartbeat (v2.5 plan B5 — `.live` is deleted).
	fmt.Fprintln(tw, "AGENT\tSTATUS\tINBOX\tLAST_SEEN\tAGE\tHOST\tPROTO")
	for _, id := range loop.RegistrationsByAgentID(regs) {
		reg := regs[id]
		inboxDepth := countInbox(cfg.AgentInboxDir(id))
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			reg.AgentID,
			registrationStatusLabel(cfg, reg, now),
			inboxDepth,
			formatMaybeTime(reg.LastSeen),
			formatAge(now, reg.LastSeen),
			formatDash(reg.Host),
			formatProtocolVersion(reg.ProtocolVersion),
		)
	}
	_ = tw.Flush()

	printProtocolWarnings(w, regs)
}

func printStatusHeader(w io.Writer, cfg *loop.Config) {
	controlRepo := cfg.ControlRepo
	if cfg.Remote != nil {
		controlRepo = cfg.Remote.URL
	}
	fmt.Fprintf(w, "control_repo: %s%s\n", controlRepo, formatOriginSuffix(cfg.ControlRepoOrigin))
	fmt.Fprintf(w, "loop_dir:     %s%s\n", cfg.LoopDir, formatOriginSuffix(cfg.LoopDirOrigin))
	if cfg.Remote != nil {
		fmt.Fprintln(w, "  (local shadow: this process's own loop dir, not the hub's)")
	}
	fmt.Fprintf(w, "vendor:       %s\n", cfg.Vendor)
	for _, shadowed := range cfg.ShadowedPointers {
		fmt.Fprintf(w, "  (shadowed pointer: %s)\n", shadowed)
	}
	fmt.Fprintln(w)
}

func printRemoteStatus(w io.Writer, cfg *loop.Config, resp op.StatusResp) {
	printStatusHeader(w, cfg)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATUS\tINBOX\tLAST_SEEN\tAGE\tHOST\tPROTO")
	regs := make(map[string]*loop.Registration, len(resp.Agents))
	for _, agent := range resp.Agents {
		regs[agent.AgentID] = &loop.Registration{AgentID: agent.AgentID, LastSeen: agent.LastSeen, Host: agent.Host, ProtocolVersion: agent.ProtocolVersion}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			agent.AgentID, agent.Status, agent.InboxDepth,
			formatMaybeTime(agent.LastSeen), formatAge(resp.Now, agent.LastSeen),
			formatDash(agent.Host), formatProtocolVersion(agent.ProtocolVersion),
		)
	}
	_ = tw.Flush()
	printProtocolWarnings(w, regs)
	if resp.Truncated {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.")
	}
}

func printProtocolWarnings(w io.Writer, regs map[string]*loop.Registration) {
	var warnings []string
	for _, id := range loop.RegistrationsByAgentID(regs) {
		if warning := protocolVersionWarning(regs[id]); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "PROTOCOL WARNINGS:")
		for _, warning := range warnings {
			fmt.Fprintf(w, "- %s\n", warning)
		}
	}
}

// registrationStatusLabel derives a display STATUS for a registration row
// (v2.5 plan B5: the .live-published Status field is gone). A live serve
// claim is the strongest signal — it means a fresh lease immunizes an
// old-looking row (C12: never call a row stale-would-sweep when a claim
// still owns it, since the sweep itself would back off too) — checked first,
// mirroring SweepStaleRegistrations' own claimProvablyDead ordering. Absent
// that, the row's own age vs the pool's stale_after decides fresh vs stale.
// Single hyphenated token (no embedded spaces) so simple whitespace-based
// column parsing of this output — including this package's own
// statusColumnValue test helper — never misaligns a later column.
func registrationStatusLabel(cfg *loop.Config, reg *loop.Registration, now time.Time) string {
	if claim, err := loop.ReadServeClaim(cfg, reg.AgentID); err == nil && !loop.ClaimIsStale(claim, now) {
		return "lease-held"
	}
	age := now.Sub(reg.LastSeen)
	if age < 0 {
		age = 0
	}
	if age > loop.StaleAfter(cfg) {
		return "stale-would-sweep"
	}
	return "fresh"
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

func formatProtocolVersion(version int) string {
	switch version {
	case 0:
		return "legacy"
	case loop.CurrentProtocolVersion:
		return protocolVersionLabel(version)
	default:
		return protocolVersionLabel(version) + "!"
	}
}

func protocolVersionLabel(version int) string {
	if version == 3 {
		return "v2.5"
	}
	return fmt.Sprintf("v%d", version)
}

func protocolVersionWarning(reg *loop.Registration) string {
	if reg == nil || reg.ProtocolVersion == 0 || reg.ProtocolVersion == loop.CurrentProtocolVersion {
		return ""
	}
	return fmt.Sprintf("%s reports protocol %s; expected %s — update and restart every lane before resuming sends", reg.AgentID, protocolVersionLabel(reg.ProtocolVersion), protocolVersionLabel(loop.CurrentProtocolVersion))
}
