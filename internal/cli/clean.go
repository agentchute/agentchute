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

// clean.go — the v2.5 manual clean command (implementation plan slice A4,
// C20; decision §4/§12.13). Handles the two human-triggered, explicit
// destructive operations the soft-state registration redesign (the lazy
// sweep, loop/sweep.go) deliberately does NOT do on its own: pruning an
// asker's own expired reply obligations, and deleting an abandoned peer's
// mailbox. Sweep removes ONLY the registration row and NEVER touches inboxes
// or `.claimed` residue (§1) — this command is the explicit counterpart for
// the mail itself.
//
// Plan-then-apply, same discipline as setup_clean.go's clean-all audit:
// without --yes this only PRINTS what would happen and mutates nothing; with
// --yes it applies, RE-CHECKING every guard immediately before the
// destructive step (TOCTOU) and failing closed if anything no longer passes.

func cmdClean(args []string) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, controlRepo, loopDir, mailboxTarget string
	var owed, apply, jsonOut bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID); required for --owed")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&owed, "owed", false, "prune this agent's own expired reply obligations")
	fs.StringVar(&mailboxTarget, "mailbox", "", "delete an abandoned peer's mailbox (agent id); refuses on any live signal")
	fs.BoolVar(&apply, "yes", false, "apply the plan (default: print what would happen and change nothing)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")

	if err := fs.Parse(args); err != nil {
		return cleanUsage(err)
	}
	if fs.NArg() != 0 {
		return cleanUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if !owed && strings.TrimSpace(mailboxTarget) == "" {
		return cleanUsage(fmt.Errorf("one of --owed or --mailbox <id> is required"))
	}
	if owed && strings.TrimSpace(mailboxTarget) != "" {
		return cleanUsage(fmt.Errorf("--owed and --mailbox are mutually exclusive"))
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

	if owed {
		// Destructive command: require EXPLICIT identity (--as or the env
		// var), never the contextual-guess fallback resolveAgentID otherwise
		// falls through to (review nit — B4 deletes that fallback fleet-wide
		// soon, but this command shouldn't guess whose obligations to prune
		// in the meantime).
		if strings.TrimSpace(agentID) == "" && strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")) == "" {
			return cleanUsage(fmt.Errorf("--owed requires an explicit identity: pass --as <id> or set AGENTCHUTE_AGENT_ID"))
		}
		agentID, err = resolveAgentID(agentID, vendor, cfg)
		if err != nil {
			return err
		}
		if err := loop.ValidateAgentID(agentID); err != nil {
			return err
		}
		return cmdCleanOwed(cfg, agentID, apply, jsonOut, now)
	}

	if err := loop.ValidateAgentID(mailboxTarget); err != nil {
		return fmt.Errorf("--mailbox: %w", err)
	}
	return cmdCleanMailbox(cfg, mailboxTarget, apply, jsonOut, now)
}

// cleanOwedResult is the --owed plan/apply outcome. Pruned lists the SAME
// entries (by ref) whether planning or applying; Applied distinguishes
// whether they were actually removed from the ledger.
type cleanOwedResult struct {
	Agent   string   `json:"agent"`
	Pruned  []string `json:"pruned"`
	Applied bool     `json:"applied"`
}

// cmdCleanOwed prunes (or, without --yes, plans pruning) asker's own expired
// `.owed` entries. Runs under WithAgentLock(asker) so a concurrent
// RecordOwed/ClearOwed (from the agent's own `check`/`send`) cannot race the
// read-modify-write.
func cmdCleanOwed(cfg *loop.Config, agentID string, apply, jsonOut bool, now time.Time) error {
	result := cleanOwedResult{Agent: agentID, Pruned: []string{}}
	err := loop.WithAgentLock(cfg, agentID, func() error {
		ledger, err := loop.LoadOwedLedger(cfg, agentID)
		if err != nil {
			return fmt.Errorf("load owed ledger: %w", err)
		}
		expired := ledger.ExpiredOwed(now)
		// Keyed by MsgID, not counted: RecordOwed's own API is idempotent per
		// key and can never create two entries with the same (to,from,seq),
		// so this can't observe a real duplicate through normal use. A
		// hand-edited ledger with a duplicate key would have BOTH instances
		// pruned together if either is expired — not reachable via the API,
		// so left as a documented limitation rather than a count-based fix.
		expiredKeys := make(map[loop.MsgID]bool, len(expired))
		for _, e := range expired {
			result.Pruned = append(result.Pruned, e.Key().RefString())
			expiredKeys[e.Key()] = true
		}
		if !apply || len(expired) == 0 {
			return nil
		}
		kept := make([]loop.OwedEntry, 0, len(ledger.Owed))
		for _, e := range ledger.Owed {
			if expiredKeys[e.Key()] {
				continue
			}
			kept = append(kept, e)
		}
		ledger.Owed = kept
		return loop.SaveOwedLedger(cfg, agentID, ledger)
	})
	if err != nil {
		return err
	}
	result.Applied = apply && len(result.Pruned) > 0

	if jsonOut {
		return emitCleanJSON(result)
	}
	emitCleanOwedText(result, apply)
	return nil
}

func emitCleanOwedText(r cleanOwedResult, apply bool) {
	if len(r.Pruned) == 0 {
		fmt.Println("(no expired reply obligations)")
		return
	}
	verb := "would prune"
	if apply {
		verb = "pruned"
	}
	fmt.Printf("%s %d expired reply obligation(s) for %s:\n", verb, len(r.Pruned), r.Agent)
	for _, ref := range r.Pruned {
		fmt.Printf("  %s\n", ref)
	}
}

// cleanMailboxResult is the --mailbox plan/apply outcome.
type cleanMailboxResult struct {
	Target   string `json:"target"`
	InboxDir string `json:"inbox_dir"`
	Refused  string `json:"refused,omitempty"`
	Applied  bool   `json:"applied"`
}

// afterCleanMailboxFirstCheckHook is a test-only seam; see its call site in
// cmdCleanMailbox. nil in production.
var afterCleanMailboxFirstCheckHook func()

// cmdCleanMailbox deletes (or, without --yes, plans deleting) an abandoned
// peer's entire inbox tree (including `.claimed` residue, which lives under
// it — AgentClaimedDir is a child of AgentInboxDir). Refuses unless the
// target's registration row is absent AND its serve lease is absent or
// stale; the apply path re-runs that exact guard immediately before the
// removal (TOCTOU), matching applyCleanPlan's discipline in setup_clean.go.
func cmdCleanMailbox(cfg *loop.Config, target string, apply, jsonOut bool, now time.Time) error {
	inboxDir := cfg.AgentInboxDir(target)
	result := cleanMailboxResult{Target: target, InboxDir: inboxDir}

	if refusal := mailboxCleanRefusal(cfg, target, now); refusal != "" {
		result.Refused = refusal
		if jsonOut {
			if jerr := emitCleanJSON(result); jerr != nil {
				return jerr
			}
		} else {
			fmt.Printf("refused: %s\n", refusal)
		}
		return fmt.Errorf("clean --mailbox %s: %s", target, refusal)
	}

	if !apply {
		if jsonOut {
			return emitCleanJSON(result)
		}
		fmt.Printf("plan: would remove mailbox for %q: %s\n", target, inboxDir)
		return nil
	}

	// Test seam: fires after the first (plan-time) refusal check has PASSED,
	// before the apply-path re-check below. Lets a test inject a live signal
	// into exactly the TOCTOU window this re-check exists to close, so a
	// test can prove the RE-CHECK (not just the first check, which by then
	// has already passed) is what refuses. nil in production.
	if afterCleanMailboxFirstCheckHook != nil {
		afterCleanMailboxFirstCheckHook()
	}

	// Re-check the guard AND perform the removal under the SAME
	// WithAgentLock(target) critical section (review fix): publishRegistrationOnce
	// (register.go) and AcquireServeLease's reclaim path already serialize on
	// this exact per-agent lock, so a concurrent `boot`/`serve` for `target`
	// that would recreate its registration is FORCED to wait until this
	// whole recheck+remove completes — closing the window where the plan
	// above (run before --yes takes any lock) goes stale between the
	// unlocked recheck and the removal. (AcquireServeLease's FRESH-acquire
	// fast path is intentionally unlocked — see lease.go — so this does not
	// independently guard against a brand-new lease appearing mid-window;
	// mailboxCleanRefusal's own serve-claim read still catches it as long as
	// the claim file exists by the time the re-check runs.)
	//
	// Side effect, noted per review: WithAgentLock's ensurePrivateDir creates
	// state/<target>/ even for a target that never had a registration at
	// all (a pure typo). Harmless here — an empty directory for an id
	// already established (by the FIRST refusal check, before any lock) to
	// have no live registration or claim.
	var removed bool
	err := loop.WithAgentLock(cfg, target, func() error {
		if refusal := mailboxCleanRefusal(cfg, target, now); refusal != "" {
			result.Refused = refusal
			return fmt.Errorf("clean --mailbox %s: guard re-check failed: %s", target, refusal)
		}
		if _, statErr := os.Lstat(inboxDir); statErr != nil {
			if os.IsNotExist(statErr) {
				return nil // nothing to remove; not an error, not a removal either.
			}
			return fmt.Errorf("stat mailbox %s: %w", target, statErr)
		}
		if err := os.RemoveAll(inboxDir); err != nil {
			return fmt.Errorf("remove mailbox %s: %w", target, err)
		}
		removed = true
		return nil
	})
	if err != nil {
		// Emit whatever result state accumulated (Refused set for a guard
		// failure, empty for a stat/remove error) before returning — every
		// failure path reports structured output in --json mode, matching
		// the plan-mode and first-refusal paths above (review nit: the
		// original RemoveAll-failure path emitted no JSON at all).
		if jsonOut {
			if jerr := emitCleanJSON(result); jerr != nil {
				return jerr
			}
		} else if result.Refused != "" {
			fmt.Printf("refused (guard re-check failed): %s\n", result.Refused)
		}
		return err
	}

	result.Applied = removed
	if jsonOut {
		return emitCleanJSON(result)
	}
	if !removed {
		fmt.Printf("no mailbox exists for %q: %s\n", target, inboxDir)
		return nil
	}
	fmt.Printf("removed mailbox for %q: %s\n", target, inboxDir)
	return nil
}

// mailboxCleanRefusal returns a non-empty refusal reason unless target's
// registration row is ABSENT and its serve lease is ABSENT or STALE — the
// two live signals that mean a lane may still own this mailbox. An
// unreadable (not merely absent) registration or claim fails CLOSED (refuse)
// rather than guessing.
func mailboxCleanRefusal(cfg *loop.Config, target string, now time.Time) string {
	if _, err := os.Stat(cfg.AgentRegistrationPath(target)); err == nil {
		return fmt.Sprintf("%q has a registration row (not abandoned)", target)
	} else if !os.IsNotExist(err) {
		return fmt.Sprintf("could not check %q's registration: %v", target, err)
	}

	claim, err := loop.ReadServeClaim(cfg, target)
	if err == nil {
		if !loop.ClaimIsStale(claim, now) {
			return fmt.Sprintf("%q has a fresh serve lease (a live process may still own this id)", target)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Sprintf("could not check %q's serve lease: %v", target, err)
	}

	return ""
}

func emitCleanJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cleanUsage(err error) error {
	if err == flag.ErrHelp {
		return cleanHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, cleanHelp())
}

func cleanHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, cleanHelp())
}

func cleanHelp() string {
	return strings.TrimSpace(`
Usage: agentchute clean --owed [--as <id>] [--yes] [--json]
       agentchute clean --mailbox <id> [--yes] [--json]

Manual, human-triggered cleanup for the two things the lazy sweep
deliberately never touches on its own: an asker's own expired reply
obligations, and an abandoned peer's mailbox. Without --yes, prints a plan
and changes nothing; --yes applies it, re-checking every guard immediately
before the destructive step.

Modes (exactly one):
  --owed              prune this agent's own expired .owed entries
  --mailbox <id>      delete <id>'s entire inbox tree (incl. .claimed);
                       refuses unless <id> has no registration row AND no
                       fresh serve lease

Flags:
  --as <id>           agent id (or $AGENTCHUTE_AGENT_ID); required for --owed
  --vendor <vendor>   vendor or origin (anthropic, openai, google, xai)
  --control-repo <p>  control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>      loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --yes               apply the plan (default: plan only, no mutation)
  --json              structured JSON output
`)
}
