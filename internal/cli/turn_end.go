package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// cmdTurnEnd is the ordered end-of-turn handler (v2.5 plan A7, C24): ONE
// process replaces the separate `self-check` + `ack --quiet` +
// `gate --before finish` end-of-turn hook entries. Codex's docs say same-event
// hooks run CONCURRENTLY, so "self-check entry stays first" was never
// enforceable as template ordering on every vendor — folding all four steps
// into one process is the only way to guarantee the order.
//
// Strict in-process order, steps 0 and 3 run regardless of step 1's
// condition:
//
//  0. registration self-repair — identical logic to `self-check`
//     (selfRepairRegistration), so the row exists and last_seen/.live are
//     fresh before the gate evaluates.
//  1. archive `.claimed` UNLESS a DIFFERENT session's latch says otherwise
//     (C23). Only a latch that both (a) reads back successfully and (b)
//     names a foreign/dead session withholds the commit — that residue is
//     left for `check`'s own redelivery banner. No latch at all (guard
//     disabled, hand-run/unguarded session, or nothing claimed yet) always
//     archives, matching `ack`'s pre-A7 unconditional-commit contract; a
//     latch that fails to read (absent OR corrupt) is treated the same way,
//     never as a reason to withhold the commit.
//  2. clear the own-session guard latch (no-op if none/foreign).
//  3. evaluate + emit the finish gate via the EXACT SAME contracts as
//     `agentchute gate --before finish` (gate.go): default text, --json, and
//     --codex-hook Stop (silent on clear, block-JSON exit-0 on block).
//
// Recovery property (and its known limit): turn-end has NO self-denial of
// its own (unlike ack) — its only possible denial is the PreToolUse
// guard hook itself. When NEITHER that hook NOR the Stop hook is firing at
// all (e.g. a hook-trust rollout window on a vendor that gates project-local
// hook changes per-command), a lane armed by `check` is still recoverable:
// the guard that would deny a direct `turn-end` invocation also isn't
// running, so nothing stops it (ack's own self-denial error text names it
// as the fix for exactly this reason — TestGuardArmedWithoutHooksEverFiringStillRecoversViaTurnEnd;
// check itself never self-denies, so a lane can always re-read what it holds).
//
// KNOWN GAP (codex review, PR #89 round 3, finding #1 — NOT fixed): a MIXED
// state where the PreToolUse guard is active but Stop is independently
// disabled/failing is not recoverable this way — the active guard denies a
// model's own attempt to run `turn-end` (it is deliberately deny-listed, so a
// same-turn instruction can't clear its own latch and disarm the rest of the
// deny list for the remainder of the turn). Removing turn-end from the deny
// list would close this gap but reopen that exact bypass, which several
// review rounds have independently protected; kept deny-listed on the
// judgment that a same-turn security bypass is worse than a narrow, human-
// recoverable (delete state/<id>/guard.latch) hook-rollout edge. Flagged for
// Alex/reviewers as an open design question, not silently accepted.
func cmdTurnEnd(args []string) error {
	fs := flag.NewFlagSet("turn-end", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, bio, controlRepo, loopDir, codexHook, geminiHook, agyHook string
	var jsonOut bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai, local)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
	fs.StringVar(&bio, "bio", "", "short self-description for the registration body")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.StringVar(&codexHook, "codex-hook", "", "codex hook JSON shape for the named event (Stop)")
	fs.StringVar(&geminiHook, "gemini-hook", "", "Gemini CLI hook JSON shape for the named event (AfterAgent)")
	fs.StringVar(&agyHook, "agy-hook", "", "Antigravity CLI hook JSON shape for the named event (Stop)")

	if err := fs.Parse(args); err != nil {
		return turnEndUsage(err)
	}
	if fs.NArg() != 0 {
		return turnEndUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if err := requireRunnerAncestry("turn-end"); err != nil {
		return err
	}
	// Hook stdin is read only in a hook mode: a hand-run turn-end must never
	// wait on a terminal.
	// Read once: the codex foreign-thread check and stop_hook_active both
	// come from the same input. --json is the Claude Code Stop hook's mode
	// (the template's command has always been `turn-end --json`, so an
	// installed older binary keeps working against a newer template): its
	// stdin is read too, never when it is a terminal.
	var hookBody []byte
	if jsonOut || codexHook == "Stop" || geminiHook == "AfterAgent" {
		hookBody = readHookStdin(hookStdin())
	}
	var hookIn turnEndHookInput
	if len(hookBody) > 0 {
		_ = json.Unmarshal(hookBody, &hookIn)
	}

	opts := registerOpts{Host: host, Bio: bio, ServeToken: os.Getenv("AGENTCHUTE_SERVE_TOKEN")}
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
	if codexHook == "Stop" {
		// A codex thread outside the control repo (memory consolidation's
		// hidden thread) gets nothing from this hook: no registration write, no archive of the lane's claimed mail, no latch change, no gate verdict.
		if _, foreign := codexHookForeignCwd(cfg, hookBody); foreign {
			return nil
		}
	}

	now := time.Now().UTC()

	// STEP 0. Best-effort: a registration WRITE failure (e.g. --vendor was
	// never passed — C26 ships turn-end's hook entries env-identity-only —
	// and this id doesn't prefix-match a canonical wrapper base closely
	// enough for resolveAgentVendor to backfill one from an existing row)
	// must not itself abort steps 1-3, which still need to commit THIS
	// session's own claimed mail and clear its latch regardless (claude-code
	// review, PR #89: the live roster id "sonnet" is exactly such an id, and
	// the old unconditional-abort-on-error fully wedged it). Only a genuine
	// identity-resolution failure — no id could be determined at all, so
	// resolvedID comes back empty — leaves nothing usable to proceed with.
	resolvedID, _, repairErr := selfRepairRegistration(cfg, &opts, agentID, vendor, now)
	if resolvedID == "" {
		return repairErr
	}
	agentID = resolvedID
	if repairErr != nil {
		fmt.Fprintf(os.Stderr, "warning: registration self-repair failed (continuing so this session's own claimed mail still commits): %v\n", repairErr)
	}

	// STEP 1: archive .claimed UNLESS THIS invocation is itself guard-armed
	// (session != "") AND a latch exists, reads back successfully, AND
	// belongs to a DIFFERENT session (the gemini crash / dead-latch case:
	// preserve that residue for check's own redelivery banner). Guard
	// disabled for this process (no serve token, or a hand-run session the
	// hooks never armed) always archives UNCONDITIONALLY, regardless of any
	// latch a PAST guarded session may have left behind — codex review, PR
	// #89 round 3: comparing a stale latch's session against session=="" made
	// EVERY unguarded/no-token turn-end call after a crashed guarded run
	// treat that stale latch as "foreign", withholding the commit forever
	// (step 2 also never clears it when session==""), reintroducing exactly
	// the same-vs-hand-run divergence A7 promised never to have. A latch
	// that fails to read at all (absent OR corrupt) is likewise never a
	// reason to withhold the commit within the armed branch (findings #1 and
	// #4 from round 2).
	session := resolveGuardSession()
	archive := true
	if session != "" {
		if latch, lerr := loop.ReadGuardLatch(cfg, agentID); lerr == nil && latch.Session != session {
			archive = false
		}
	}

	var acked []ackItem
	if archive {
		acked, _, err = ackClaimed(cfg, agentID)
		if err != nil {
			if cfg.Remote != nil && hubclient.ErrorCode(err) == "E_CONNECT" {
				return hubTurnEndConnectError()
			}
			return err
		}
	}

	// STEP 2: clear own-session latch. No-op if none/foreign/corrupt
	// (ClearGuardLatch fails open on a read it cannot make sense of).
	if session != "" {
		if err := loop.ClearGuardLatch(cfg, agentID, session); err != nil {
			return fmt.Errorf("clear guard latch: %w", err)
		}
	}

	// STEP 3.
	gateReq := op.GateReq{Phase: gatePhaseFinish}
	var status op.GateResp
	if cfg.Remote != nil {
		gateSession, openErr := openRemoteOneShot(cfg, agentID)
		if openErr != nil {
			return openErr
		}
		status, err = gateSession.Gate(gateReq)
	} else {
		status, err = op.Gate(cfg, op.Context{ActorID: agentID}, gateReq)
	}
	if err != nil {
		return err
	}

	// C3 (opus-xhigh): a Stop our own block caused (stop_hook_active) that
	// finds the SAME reasons means the agent tried and could not clear them.
	// Blocking again only re-prompts it until the harness's own continuation
	// cap overrides us silently; allow the stop and say so instead.
	stillBlocked := ""
	if status.Blocked && (jsonOut || codexHook == "Stop") {
		// The record names the session too, and self-check (every turn's
		// UserPromptSubmit) clears it: stop_hook_active is also true when
		// ANOTHER Stop hook blocked, and a block from an earlier turn or
		// session must never let this turn's first block through.
		reasons := gateBlockedReasonLine(status)
		record := hookIn.session() + "\n" + reasons
		if hookIn.active() && readTurnEndLastBlock(cfg, agentID) == record {
			stillBlocked = "finish gate still blocked: " + reasons
		} else {
			writeTurnEndLastBlock(cfg, agentID, record)
		}
	} else if !status.Blocked {
		clearTurnEndLastBlock(cfg, agentID)
	}

	if codexHook == "Stop" {
		if stillBlocked != "" {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"systemMessage": stillBlocked})
		}
		// Identical contract to gate.go's own codex Stop path: silent + exit 0
		// on clear, block-JSON + exit 0 on block (codex reads the JSON, not
		// the exit code, for this event).
		return emitGateCodexStop(status)
	}
	if geminiHook == "AfterAgent" {
		return emitTurnEndGeminiAfterAgent(status, hookIn.active())
	}
	if agyHook == "Stop" {
		return emitTurnEndAgyStop(status)
	}
	if jsonOut {
		if err := emitTurnEndJSON(status, acked, stillBlocked); err != nil {
			return err
		}
	} else {
		emitTurnEndText(status, acked)
	}
	if stillBlocked != "" {
		// Exit 0: Claude Code stops, and shows systemMessage to the user.
		fmt.Fprintln(os.Stderr, stillBlocked)
		return nil
	}

	emitGateBlockedStderr(status)
	if status.Blocked {
		return errBlocked
	}
	return nil
}

// turnEndJSON is turn-end's --json shape: the same gateStatus fields
// `agentchute gate --json` emits, plus the archive commit this call made.
type turnEndJSON struct {
	gateStatus
	Archived []ackItem `json:"archived,omitempty"`
	// SystemMessage is Claude Code's common hook output field ("shown to the
	// user"): set only when a repeated, unchanged block is let through.
	SystemMessage string `json:"systemMessage,omitempty"`
}

func emitTurnEndJSON(status gateStatus, acked []ackItem, systemMessage string) error {
	out := turnEndJSON{gateStatus: status, Archived: acked, SystemMessage: systemMessage}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func emitTurnEndText(status gateStatus, acked []ackItem) {
	if len(acked) == 0 {
		fmt.Println("(nothing to archive)")
	} else {
		for _, a := range acked {
			fmt.Printf("acked %s -> %s\n", a.Filename, a.ArchivePath)
		}
	}
	emitGateText(status)
}

func turnEndUsage(err error) error {
	if err == flag.ErrHelp {
		return turnEndHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, turnEndHelp())
}

func turnEndHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, turnEndHelp())
}

func turnEndHelp() string {
	return strings.TrimSpace(`
Usage: agentchute turn-end --as <id> --vendor <vendor> [flags]

Ordered end-of-turn handler (v2.5 plan A7/C24): registration self-repair, then
archives THIS session's own claimed mail (only if its guard latch is set),
clears that latch, then evaluates + emits the finish gate — replacing the
separate self-check + ack --quiet + gate --before finish hook entries (whose
relative order codex's concurrent hook execution made unenforceable).

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin (anthropic, openai, google, xai, local)
  --host <name>         host (defaults to OS hostname)
  --bio <text>          short self-description
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output; also the Claude Code Stop hook
                        mode: reads the hook input when stdin is not a
                        terminal, and a Stop our own block caused that finds
                        the same reasons is let through with a "finish gate
                        still blocked" systemMessage
  --codex-hook <event>  codex hook JSON shape (Stop)
  --gemini-hook <event> Gemini CLI hook JSON shape (AfterAgent)
  --agy-hook <event>    Antigravity CLI hook JSON shape (Stop)
`)
}

// turnEndHookInput is the slice of an end-of-turn hook's stdin turn-end
// reads: whether this run was caused by a block we returned. Claude Code,
// codex and Gemini CLI send stop_hook_active, grok stopHookActive.
type turnEndHookInput struct {
	StopHookActive      bool   `json:"stop_hook_active"`
	StopHookActiveCamel bool   `json:"stopHookActive"`
	SessionID           string `json:"session_id"`
	SessionIDCamel      string `json:"sessionId"`
}

func (in turnEndHookInput) session() string {
	if in.SessionID != "" {
		return in.SessionID
	}
	return in.SessionIDCamel
}

func (in turnEndHookInput) active() bool { return in.StopHookActive || in.StopHookActiveCamel }

// turnEndLastBlockFile holds the reasons of the last block turn-end returned
// in a hook mode, so a Stop it caused can tell whether anything changed.
func turnEndLastBlockFile(cfg *loop.Config, agentID string) string {
	return filepath.Join(cfg.AgentStateDir(agentID), "turn-end.last-block")
}

func readTurnEndLastBlock(cfg *loop.Config, agentID string) string {
	data, err := os.ReadFile(turnEndLastBlockFile(cfg, agentID))
	if err != nil {
		return ""
	}
	return string(data)
}

// writeTurnEndLastBlock records a block's reasons. Best effort: without the
// record the next Stop simply blocks again, as it always did.
func writeTurnEndLastBlock(cfg *loop.Config, agentID, reasons string) {
	path := turnEndLastBlockFile(cfg, agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(reasons), 0o600)
}

func clearTurnEndLastBlock(cfg *loop.Config, agentID string) {
	_ = os.Remove(turnEndLastBlockFile(cfg, agentID))
}

// emitTurnEndGeminiAfterAgent is the Gemini CLI end-of-turn contract: silent
// and exit 0 on clear; `{"decision":"deny","reason":…}` with exit 0 on block —
// the documented AfterAgent deny (geminicli.com/docs/hooks/reference#afteragent),
// which rejects the response and retries with the reason; exit 2 is the
// stderr spelling of the same rejection and is not used. A second AfterAgent
// raised by our own deny (stop_hook_active) is never denied again, so a lane
// that cannot clear the gate is not spun in retries.
func emitTurnEndGeminiAfterAgent(s gateStatus, stopHookActive bool) error {
	if !s.Blocked {
		return nil
	}
	if stopHookActive {
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(map[string]any{"decision": "deny", "reason": gateBlockedReasonLine(s)})
}

// emitTurnEndAgyStop is Antigravity's Stop contract: `decision` is required;
// `"continue"` keeps the agent running and injects `reason` as a system
// message, any other value allows the stop (https://antigravity.google/docs/hooks/).
// Clear → `{"decision":"stop"}`; blocked → continue with the gate's reason.
func emitTurnEndAgyStop(s gateStatus) error {
	out := map[string]any{"decision": "stop"}
	if s.Blocked {
		out = map[string]any{"decision": "continue", "reason": gateBlockedReasonLine(s)}
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}
