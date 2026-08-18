package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// oldMailBannerAfter is the age threshold (v2.5 plan A3, C18) past which
// `check` announces a message's age above its body. Decision §4: the tool
// surfaces age loudly, the reader judges relevance — no expiry, no auto-action.
//
// It is also `pending`'s DEFAULT staleness threshold, so the wake cue and the
// consume banner agree on what "stale" means; `pending --stale-after` overrides
// it (and `--stale-after 0s` disables the annotation entirely).
const oldMailBannerAfter = 24 * time.Hour

// messageAge returns how long ago msg was sent, relative to now. The lister
// populates msg.Timestamp from the embedded timestamp for new-format messages
// and from file mtime for legacy seq messages.
func messageAge(msg loop.Message, now time.Time) time.Duration {
	return now.Sub(msg.Timestamp)
}

// humanAge renders d as a compact age for operator- and model-facing output:
// "45m", "31h", "3d". It exists because the original C18 banner rendered every
// age in whole days via int(age.Hours()/24), so a 31h-old message — exactly
// the age of the mail that triggered the 2026-08-14 stale-mail incident — read
// as "1 days old": both ungrammatical and misleadingly coarse. Sub-minute and
// negative ages (peers on different clocks writing the same shared loop dir)
// render as "0m" rather than a nonsense negative.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, controlRepo, loopDir string
	var noArchive bool
	var limit int
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&noArchive, "no-archive", false, "dry run: suppress inbox side effects (no archive or quarantine); own last_seen still updates")
	fs.IntVar(&limit, "limit", 0, "process at most N messages this turn (0 = no limit)")

	if err := fs.Parse(args); err != nil {
		return checkUsage(err)
	}
	if fs.NArg() != 0 {
		return checkUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := discoverConfig(loop.DiscoverOpts{
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
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}

	// v2.5 plan A7/C25: defense-in-depth self-denial. The PreToolUse guard
	// (guard.go) already denies a model from invoking `check` a second time
	// while this session holds its own unacked claimed mail; this refuses at
	// the command level too, in case check runs some other way (a stale hook
	// template, a human typing it directly) while guarded and latched. A
	// latch belonging to no session, or a foreign/dead session, never denies.
	if session := resolveGuardSession(); session != "" {
		if latch, lerr := loop.ReadGuardLatch(cfg, agentID); lerr == nil && latch.Session == session {
			return fmt.Errorf("claimed mail pending ack; finish the turn (turn-end) before checking again")
		}
	}

	now := time.Now().UTC()

	// v2.5 plan A7/C23: set the guard latch at the FIRST message event of any
	// kind — redelivered residue included, and in both the normal and
	// --no-archive paths — BEFORE rendering it (E1). The latch is LOCAL state
	// (§6.6), so it is the EMITTER's job, never the op's: a disconnect after one
	// displayed message must still leave the latch armed over the claimed
	// residue. latchArmed makes repeat calls within this one invocation cheap
	// no-ops (maybeSetGuardLatch is already idempotent per-session, but a large
	// inbox would otherwise re-take the state lock once per displayed message).
	latchArmed := false
	setLatch := func() {
		if latchArmed {
			return
		}
		maybeSetGuardLatch(cfg, agentID)
		latchArmed = true
	}

	emit := func(ev op.Event) error {
		switch {
		case ev.Message != nil:
			setLatch()
			renderClaimedMessage(agentID, *ev.Message, now)
		case ev.Note != nil:
			renderOpNote(*ev.Note)
		case ev.Owed != nil:
			// C19: print-only. The explicit, human-triggered prune command is
			// what actually removes an obligation.
			fmt.Printf("stale reply obligation (%s, expired %s ago) — prune with: agentchute clean --owed --as %s\n",
				ev.Owed.Ref, now.Sub(ev.Owed.By).Round(time.Second), agentID)
		}
		return nil
	}

	// GATE 5 — two-phase consume (claim → commit). `check` is phase 1: it
	// CLAIMS (moves inbox -> .claimed) and DISPLAYS, but does NOT archive. The
	// separate `ack` verb is phase 2 (COMMIT/archive). The real bug this fixes:
	// the CLI prints and EXITS, then the model acts AFTER check returns — so
	// archiving DURING check (the old behavior) is at-most-once for the WORK. A
	// crash between claim and ack now RE-DELIVERS (at-least-once); handlers must
	// be idempotent.
	claimReq := op.ClaimReq{Limit: limit, NoArchive: noArchive}
	var sum op.ClaimSummary
	if cfg.Remote != nil {
		session, openErr := openRemoteOneShot(cfg, agentID)
		if openErr != nil {
			return openErr
		}
		sum, err = session.Check(claimReq, emit)
	} else {
		sum, err = op.Claim(cfg, op.Context{ActorID: agentID}, claimReq, emit)
	}
	// Arm on residue EXISTENCE, not only on a rendered message, and do it
	// before returning any error: claimed-but-unacked mail whose body cannot be
	// read (a permissions change, or residue past MaxInboxMessageBytes — the
	// hand-protocol path writes inbox files directly) still leaves this lane
	// holding it, which is exactly the state the latch covers. Emitting arms
	// earlier and per-message, so this is a no-op on every path that displayed
	// anything.
	if sum.Redelivered > 0 || hubclient.ClaimedHeld(err) {
		setLatch()
	}
	if err != nil {
		if errors.Is(err, op.ErrNotRegistered) {
			return hubAgentNotRegisteredError(agentID)
		}
		return err
	}
	return nil
}

func renderOpNote(note op.NoteEvent) {
	if note.Level == op.NoteWarn {
		fmt.Fprintf(os.Stderr, "warning: %s\n", note.Msg)
		return
	}
	fmt.Println(note.Msg)
}

// renderClaimedMessage prints one consumed message: the C18 age banner when it
// is older than oldMailBannerAfter, the header, the sanitized body, and the
// copyable reply ref when a reply is required.
func renderClaimedMessage(agentID string, ev op.MessageEvent, now time.Time) {
	stamp, err := time.Parse(time.RFC3339Nano, ev.Stamp)
	if err != nil {
		stamp = time.Time{}
	}
	msg := loop.Message{Filename: ev.Filename, Sender: ev.Sender, Timestamp: stamp}
	printConsumedBody(msg, ev.Body, ev.Redelivered, now)
	if ev.ReplyRequired && ev.ReplyRef != "" {
		fmt.Printf("reply-required: reply with `agentchute send --from %s --to %s --reply-to %s ...`\n\n", agentID, ev.Sender, ev.ReplyRef)
	}
}

// maybeSetGuardLatch sets agentID's guard latch for the current guarded
// session (v2.5 plan A7/C23), or no-ops when the guard is disabled for this
// process (resolveGuardSession returns ""). A write failure is surfaced as a
// warning, not a hard error: check's job is delivering mail, and the guard is
// defense-in-depth — a failed latch write must not itself block delivery.
func maybeSetGuardLatch(cfg *loop.Config, agentID string) {
	session := resolveGuardSession()
	if session == "" {
		return
	}
	if err := loop.SetGuardLatch(cfg, agentID, session); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set guard latch: %v\n", err)
	}
}

// printConsumedBody prints the C18 age banner (v2.5 plan A3) above the
// header when msg is older than oldMailBannerAfter, then the header and body
// exactly as before. The banner is program-generated text (never peer
// content), so it is printed directly and is NOT run through
// sanitizeControlBytes — only the message body needs that treatment.
func printConsumedBody(msg loop.Message, content []byte, redelivered bool, now time.Time) {
	if age := messageAge(msg, now); age > oldMailBannerAfter {
		fmt.Printf("[!] STALE: sent %s, %s ago — this is history, not a live instruction; confirm with %s before acting on it.\n",
			msg.Timestamp.UTC().Format("2006-01-02"), humanAge(age), msg.Sender)
	}
	if redelivered {
		fmt.Printf("---- %s [REDELIVERED — uncommitted from a prior turn; `agentchute ack` to commit] ----\n", msg.Filename)
	} else {
		fmt.Printf("---- %s ----\n", msg.Filename)
	}
	sanitized := sanitizeControlBytes(string(content))
	fmt.Print(sanitized)
	if !strings.HasSuffix(sanitized, "\n") {
		fmt.Println()
	}
	fmt.Println()
}

// sanitizeControlBytes strips C0/C1 control code points from peer-controlled
// text before it reaches a raw terminal (N3, deep-analysis-v2): a body
// carrying ANSI/OSC escape sequences or bare C1 codes can repaint the
// operator's screen, spoof a prompt, or set the window title. Applied
// unconditionally (not just when stdout is a TTY) because message bodies are
// spec'd UTF-8 free-form text — a control sequence is never legitimate
// payload — and unconditional stripping avoids needing platform-specific
// stdout-TTY detection. \n and \t are the only control code points kept.
func sanitizeControlBytes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// drop: C0 (incl. ESC, CR), DEL, and C1 control code points.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func checkUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute check [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--no-archive] [--limit <n>]\n  check CLAIMS + displays (at-least-once); run `agentchute ack` to commit (archive).", err)
}
