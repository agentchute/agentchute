package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// guard.go — `agentchute guard --pre-tool-use` (v2.5 plan A7, C25): the
// PreToolUse-family hook entry that denies a narrow, documented list of
// high-blast-radius tool invocations while THIS session holds unacked
// claimed mail (per-session guard latch, loop/guard.go). Defense-in-depth
// only, honestly framed (plan §15 / grok P3): C25 is case-insensitive
// substring matching against the tool's own command text — an injected
// instruction can alias around it (e.g. quoting, path tricks). It is not a
// hard security boundary and must never be presented as one.
//
// Fails OPEN (allows) whenever it cannot cleanly resolve an armed session or
// this agent's id: a misconfigured or partially-wired guard must never
// itself wedge a serve lane (decision §9 rev 2.3, grok P2).

// guardDenyReason is the fixed decision text emitted on a deny caused by this
// session's own guard latch (C25's exact wording; applies to both mailbox
// and scope-expanding commands when the latch itself is the reason).
const guardDenyReason = "claimed mail is pending ack; scope-expanding tools and ack/check are denied until the end-of-turn handler archives it (agentchute §9 guard)"

// guardRecoveredDenyReason is the deny text for a scope-expanding command
// denied by the SESSION-STICKY recovered mark rather than the latch itself
// (mixed hook-trust recovery — see cmdGuardRecover's doc comment). Distinct
// wording so a human/model sees WHY: not "mail is pending", but "you already
// recovered mailbox access this session and traded away scope-expanding
// capability for the rest of it."
const guardRecoveredDenyReason = "this session ran `guard --recover` earlier; scope-expanding tools stay denied for the rest of the session — relaunch (a fresh serve session) to restore them (agentchute §9 guard)"

// guardDenySubstrings are matched case-insensitively against the tool's
// command text (tool_name + tool_input's string fields, joined). Plain
// substring match, not argv parsing — documented best-effort (C25). The
// agentchute subcommands themselves are NOT here: they need word-bounded,
// binary-token-aware matching (guardMailboxSubcmdRE / guardScopeExpandingSubcmdRE
// below), not a literal
// substring — see that regex's doc comment.
var guardDenySubstrings = []string{
	"git push",
	"git tag",
	"gh release",
	"gh pr merge",
	"curl",
	"wget",
	"ssh",
	"scp",
	"rm -rf",
	".claude/settings.json",
	".codex/hooks.json",
	".gemini/settings.json",
}

// guardDispatchPrefixRE strips a `dispatch [--shim-dir[= |] <path>] [--] `
// layer that may sit between the agentchute binary token and the real
// subcommand: the installed `ac` dispatcher script execs exactly
// `agentchute dispatch --shim-dir <dir> -- "$@"` (dispatch.go's
// splitDispatchContext — --shim-dir and the `--` sentinel are both
// optional). Without stripping this, "agentchute dispatch -- turn-end" —
// literally what `ac turn-end` expands to — would not contain "agentchute"
// and "turn-end" as adjacent tokens and would slip past
// guardMailboxSubcmdRE/guardScopeExpandingSubcmdRE untouched (claude-code
// review, PR #89: proven as a
// live bypass that self-cleared the latch mid-turn and disarmed the entire
// deny list for the rest of the turn).
var guardDispatchPrefixRE = regexp.MustCompile(`\bdispatch\b(?:[ \t]+--shim-dir(?:=\S+|[ \t]+\S+))?(?:[ \t]+--)?[ \t]+`)

// guardBinaryTokenAlt is the shared alternation matching any spelling of the
// agentchute binary invocation: the templated `${AGENTCHUTE_BIN:-agentchute}`
// form, a bare `$AGENTCHUTE_BIN`, the literal `agentchute` binary name, or
// the `ac` dispatcher this repo's own hooks/docs teach as the normal way to
// invoke it. Word-bounded (`\b`) on the bare-word alternatives ONLY: a
// leading `\b` applied uniformly across the whole alternation fails to match
// the $-prefixed forms at all, since `$` is a non-word character and a
// boundary can never hold between two non-word characters (e.g.
// string-start immediately followed by `$`) — caught by this file's own
// test suite once both forms were exercised together.
const guardBinaryTokenAlt = `(?:\$\{agentchute_bin:-agentchute\}|\$agentchute_bin|\b(?:agentchute|ac)\b)`

// guardMailboxSubcmdRE matches the "mailbox" subcommands (C25/mixed
// hook-trust recovery): ack/check/turn-end, the two-phase-consume and
// end-of-turn commands whose sole purpose is committing or reading THIS
// agent's own mail. These deny ONLY while this session's OWN latch is set;
// `guard --recover` restores them unconditionally (there is nothing
// dangerous about an agent reading/committing its own mail) — unlike the
// scope-expanding set below, which stays denied by the session-sticky
// recovered mark even after a recover.
var guardMailboxSubcmdRE = regexp.MustCompile(guardBinaryTokenAlt + `[ \t]+(?:ack|check|turn-end)\b`)

// guardScopeExpandingSubcmdRE matches the remaining denied agentchute
// subcommands: update (replaces the binary), setup (rewrites hook/PATH
// wiring — could disable the guard's own future enforcement), and clean
// (prunes persistent state). Classified as scope-expanding, not mailbox:
// each can tamper with the binary/hook/state surface itself, which is
// exactly the class of action `guard --recover` must NOT restore.
var guardScopeExpandingSubcmdRE = regexp.MustCompile(guardBinaryTokenAlt + `[ \t]+(?:update|setup|clean)\b`)

// resolveGuardSession returns the session key guard operations should latch
// against, or "" if the guard is disabled for this process (C22). The guard
// arms only when BOTH AGENTCHUTE_SERVE_TOKEN is non-empty AND
// AGENTCHUTE_GUARD=1 — serve exports the GUARD bit only for wrappers whose
// installed hooks can actually clear the latch (turn-end). Absent either,
// every guard operation (set/check/deny) must no-op: arming a latch nothing
// can clear converts the guard into a permanent jam, not a security control
// (grok P2 — the serve-launched-grok case).
func resolveGuardSession() string {
	token := strings.TrimSpace(os.Getenv("AGENTCHUTE_SERVE_TOKEN"))
	if token == "" || os.Getenv("AGENTCHUTE_GUARD") != "1" {
		return ""
	}
	return token
}

// guardDecision is the cross-vendor result of one PreToolUse-family
// evaluation.
type guardDecision struct {
	Allowed bool
	Reason  string
}

// cmdGuard implements `agentchute guard --pre-tool-use`. Read-only: it never
// lists inboxes, archives, or takes any lock beyond the single latch read on
// the (rare) armed-and-latched path.
func cmdGuard(args []string) error {
	fs := flag.NewFlagSet("guard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, controlRepo, loopDir, codexHook, geminiHook string
	var preToolUse, doRecover bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&preToolUse, "pre-tool-use", false, "evaluate a PreToolUse-family hook decision from stdin JSON")
	fs.StringVar(&codexHook, "codex-hook", "", "emit codex's PreToolUse-equivalent decision JSON")
	fs.StringVar(&geminiHook, "gemini-hook", "", "emit Gemini's BeforeTool-family decision JSON")
	fs.BoolVar(&doRecover, "recover", false, "mixed hook-trust recovery: restore mailbox access (ack/check/turn-end), trading away scope-expanding tools for the rest of this session")

	if err := fs.Parse(args); err != nil {
		return guardUsage(err)
	}
	if fs.NArg() != 0 {
		return guardUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	if doRecover {
		if preToolUse {
			return guardUsage(fmt.Errorf("--recover and --pre-tool-use are mutually exclusive"))
		}
		return cmdGuardRecover(agentID, controlRepo, loopDir)
	}
	if !preToolUse {
		return guardUsage(fmt.Errorf("--pre-tool-use is required"))
	}

	stdinBody, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	toolCmd := parseGuardToolCommand(stdinBody)

	decision := evaluateGuardInvocation(agentID, controlRepo, loopDir, toolCmd)

	switch {
	case codexHook == "PreToolUse":
		return emitCodexGuardDecision(decision)
	case geminiHook == "BeforeTool":
		return emitGeminiGuardDecision(decision)
	default:
		return emitClaudeGuardDecision(decision)
	}
}

// cmdGuardRecover implements `agentchute guard --recover`: the mixed
// hook-trust recovery path (codex review round 3 finding #1; grok's A1
// attack on an earlier same-turn-unlock-prone draft; claude-code review
// round 4 ruling). It is NOT reachable through the deny list (neither
// guardMailboxSubcmdRE nor guardScopeExpandingSubcmdRE list "guard" as a
// sensitive subcommand).
//
// Design: recovering the mailbox and recovering scope-expanding capability
// are NOT the same act. --recover grants ONLY the former:
//  1. Requires an ARMED session (resolveGuardSession() != "") — grok's J-R6:
//     hookless/hand-run lanes (no serve token, or no AGENTCHUTE_GUARD bit)
//     get no new state or ceremony; it's a pure no-op.
//  2. Archives THIS session's own claimed mail using the exact same
//     own-latch-only predicate turn-end step 1 uses (archiveAllClaimed) —
//     NEVER a foreign/dead latch's residue, preserving the P1 guarantee.
//  3. Clears the OWN latch (loop.ClearGuardLatch, session-matched — same
//     semantics as turn-end step 2, not age-based).
//  4. Sets a durable, SESSION-STICKY "recovered" mark (loop.
//     SetGuardRecoveredMark) that keeps scope-expanding tools denied for
//     the REST of this session, cleared only by a session boundary (a new,
//     different serve token from a fresh launch) — nothing a model can
//     invoke clears it, not turn-end, not ack, not check, not a second
//     recover. This is what makes recovering safe: an injected "recover,
//     then push" instruction gains the mailbox and nothing else, because
//     evaluateGuardDecision's split predicate (below) checks the recovered
//     mark independently of the latch for scope-expanding commands.
//
// An earlier draft tried to gate the mark's clearance on "only a genuine
// end-of-turn hook run" — grok's A1 attack showed that signal is forgeable
// by construction (a model tool call can replicate any flags the real hook
// uses), so recover→turn-end (now allowed, since mailbox is unconditionally
// restored)→[whatever cleared the mark]→push would fully unlock. Tying
// clearance to the serve token instead means the ONLY way to clear it is an
// actual new `ac serve` launch, which mints a fresh token no Bash tool call
// can fabricate.
func cmdGuardRecover(agentIDFlag, controlRepo, loopDir string) error {
	session := resolveGuardSession()
	if session == "" {
		fmt.Println("(guard is disarmed for this process; nothing to recover)")
		return nil
	}
	id := strings.TrimSpace(agentIDFlag)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID"))
	}
	if id == "" {
		return fmt.Errorf("agentchute guard --recover: no agent id (pass --as or set $AGENTCHUTE_AGENT_ID)")
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
	if err := loop.ValidateAgentID(id); err != nil {
		return err
	}

	now := time.Now().UTC()
	latch, lerr := loop.ReadGuardLatch(cfg, id)
	ownLatch := lerr == nil && latch.Session == session

	var acked []ackItem
	if ownLatch {
		acked, err = archiveAllClaimed(cfg, id, now)
		if err != nil {
			return err
		}
	}
	if err := loop.ClearGuardLatch(cfg, id, session); err != nil {
		return fmt.Errorf("clear guard latch: %w", err)
	}
	if err := loop.SetGuardRecoveredMark(cfg, id, session); err != nil {
		return fmt.Errorf("set guard recovered mark: %w", err)
	}

	fmt.Printf("recovered mailbox access for %s (archived %d message(s)); scope-expanding tools stay denied for the rest of this session — relaunch to restore full capability\n", id, len(acked))
	return nil
}

// evaluateGuardInvocation is cmdGuard's testable core: resolves the session,
// the agent id, and (only when both resolve) this agent's latch, then
// applies the deny list. Every failure-to-resolve path allows — see the
// guard.go doc comment on why fail-open is the only safe default here.
func evaluateGuardInvocation(agentIDFlag, controlRepo, loopDir, toolCmd string) guardDecision {
	session := resolveGuardSession()
	if session == "" {
		return guardDecision{Allowed: true}
	}

	id := strings.TrimSpace(agentIDFlag)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID"))
	}
	if id == "" {
		// Cannot resolve whose latch to check. Guard is armed (a serve session
		// is active) but the id this process would need is missing — a
		// misconfiguration, not a signal to block. Fail open (hint only, no
		// stdout noise that would corrupt hook JSON parsing).
		fmt.Fprintln(os.Stderr, "agentchute guard: AGENTCHUTE_AGENT_ID not set; cannot resolve guard latch, allowing (hint: guard only applies inside an `ac serve` session)")
		return guardDecision{Allowed: true}
	}
	if err := loop.ValidateAgentID(id); err != nil {
		return guardDecision{Allowed: true}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return guardDecision{Allowed: true}
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return guardDecision{Allowed: true}
	}

	return evaluateGuardDecision(cfg, id, session, toolCmd)
}

// guardCmdCategory classifies a tool invocation for the split deny
// predicate (mixed hook-trust recovery): mailbox commands deny only on this
// session's own latch; scope-expanding commands deny on the latch OR the
// session-sticky recovered mark (see cmdGuardRecover's doc comment).
type guardCmdCategory int

const (
	guardCmdNeither guardCmdCategory = iota
	guardCmdMailbox
	guardCmdScope
)

// classifyGuardCommand reports which C25 deny-list category toolCmd matches,
// case-insensitive, after stripping any dispatch-prefix layer.
func classifyGuardCommand(toolCmd string) guardCmdCategory {
	lower := strings.ToLower(toolCmd)
	normalized := guardDispatchPrefixRE.ReplaceAllString(lower, "")
	if guardMailboxSubcmdRE.MatchString(normalized) {
		return guardCmdMailbox
	}
	if guardScopeExpandingSubcmdRE.MatchString(normalized) {
		return guardCmdScope
	}
	for _, pattern := range guardDenySubstrings {
		if strings.Contains(lower, pattern) {
			return guardCmdScope
		}
	}
	return guardCmdNeither
}

// evaluateGuardDecision applies the C25 deny predicate against toolCmd.
// Mailbox commands (ack/check/turn-end) deny ONLY while this session's own
// guard latch is set (C23): a latch that is absent, unreadable/corrupt, or
// belongs to a different (foreign/dead) session never triggers a mailbox
// deny (loop.ReadGuardLatch's own doc comment covers the foreign-latch
// case; this function fails open on any latch read error rather than
// propagate it, since a corrupt latch file must never become a way to
// wedge a lane shut). Scope-expanding commands ALSO deny when this
// session's recovered mark is set (mixed hook-trust recovery — grok's A1
// attack on an earlier draft is exactly why this check is independent of
// the latch: `guard --recover` clears the latch unconditionally, so scope
// commands need their OWN, non-model-clearable signal).
func evaluateGuardDecision(cfg *loop.Config, agentID, session, toolCmd string) guardDecision {
	latch, lerr := loop.ReadGuardLatch(cfg, agentID)
	ownLatch := lerr == nil && latch.Session == session

	switch classifyGuardCommand(toolCmd) {
	case guardCmdMailbox:
		if ownLatch {
			return guardDecision{Allowed: false, Reason: guardDenyReason}
		}
	case guardCmdScope:
		if ownLatch {
			return guardDecision{Allowed: false, Reason: guardDenyReason}
		}
		if guardRecoveredForSession(cfg, agentID, session) {
			return guardDecision{Allowed: false, Reason: guardRecoveredDenyReason}
		}
	}
	return guardDecision{Allowed: true}
}

// guardRecoveredForSession reports whether id's session-sticky recovered
// mark applies to `session` — i.e. THIS session already ran
// `guard --recover` and has not since been relaunched (a relaunch mints a
// new, different serve token, making any prior mark inert — same "dead
// latch" pattern the guard latch itself uses). A mark that fails to read at
// all (corrupt/unreadable) fails CLOSED here — the opposite of the latch's
// own fail-open posture — deliberately: this signal's whole job is
// preventing regained scope-expanding capability, so a corrupted file must
// never become a way to regain it.
func guardRecoveredForSession(cfg *loop.Config, id, session string) bool {
	mark, err := loop.ReadGuardRecoveredMark(cfg, id)
	if err != nil {
		return !os.IsNotExist(err) // absent => never recovered; anything else (corrupt) => fail closed.
	}
	return mark.Session == session
}

// guardHookInput is the tolerant shape of a PreToolUse-family hook's stdin
// JSON. Only the fields this guard consults are bound; everything else is
// ignored.
type guardHookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// parseGuardToolCommand extracts the best-effort command text to match
// against the deny list. Never errors: a missing/malformed body yields "",
// which matches nothing — a denial must be POSITIVELY matched from real
// input, never inferred from a parse failure (fail open, not fail deny).
func parseGuardToolCommand(body []byte) string {
	var in guardHookInput
	if err := json.Unmarshal(body, &in); err != nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if in.ToolName != "" {
		parts = append(parts, in.ToolName)
	}
	if len(in.ToolInput) > 0 {
		var asMap map[string]any
		if err := json.Unmarshal(in.ToolInput, &asMap); err == nil {
			for _, key := range []string{"command", "cmd"} {
				if s, ok := asMap[key].(string); ok {
					parts = append(parts, s)
				}
			}
			if arr, ok := asMap["args"].([]any); ok {
				for _, item := range arr {
					if s, ok := item.(string); ok {
						parts = append(parts, s)
					}
				}
			}
		}
	}
	return strings.Join(parts, " ")
}

// emitPreToolUseDenyJSON writes the canonical hookSpecificOutput
// permission-decision shape (C25's exact wording for Claude; confirmed by
// codex-agentchute on review of PR #89 as ITS current canonical shape too —
// `{"decision":"block","reason":...}` is codex's older compatibility form).
// Shared by every vendor emitter below, mirroring buildPendingContext /
// emitHookContextJSON's shared-body-plus-per-vendor-wrapper pattern
// (pending.go), so a future wrapper-specific field can diverge one emitter at
// a time without duplicating the JSON shape itself.
func emitPreToolUseDenyJSON(reason string) error {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

// emitClaudeGuardDecision emits Claude Code's PreToolUse permission-decision
// shape. Exit 0 either way — Claude reads `permissionDecision` from the JSON
// for PreToolUse, unlike Stop's exit-2 convention. On allow: no stdout,
// matching emitGateCodexStop / emitHookContextJSON's "silence means proceed"
// convention elsewhere in this codebase. This is also the DEFAULT shape
// (cmdGuard's fallback case).
func emitClaudeGuardDecision(d guardDecision) error {
	if d.Allowed {
		return nil
	}
	return emitPreToolUseDenyJSON(d.Reason)
}

// emitCodexGuardDecision: codex-agentchute confirmed on review of PR #89 that
// "PreToolUse" is the correct event name and that the canonical shape here is
// the SAME hookSpecificOutput/permissionDecision form as Claude's, not
// gate.go's older `{"decision":"block",...}` Stop convention (which codex
// documents as accepted-but-legacy).
func emitCodexGuardDecision(d guardDecision) error {
	if d.Allowed {
		return nil
	}
	return emitPreToolUseDenyJSON(d.Reason)
}

// emitGeminiGuardDecision uses gemini's OWN BeforeTool contract — a
// top-level `{"decision":"block","reason":"..."}` — NOT the nested
// hookSpecificOutput/permissionDecision shape Claude/codex's PreToolUse event
// uses. codex-agentchute confirmed on a second review pass (citing gemini's
// own hooks reference) that gemini's BeforeTool blocks with a top-level
// decision field, either "deny" or "block"; this codebase's only existing
// precedent for a top-level decision/reason shape is gate.go's codex Stop
// convention, which uses "block" — used here too, absent independent
// confirmation of which of the two gemini itself prefers. Round 1 of this
// PR incorrectly generalized codex's PreToolUse-specific shape answer to
// gemini as well (an different event, on an unrelated vendor) and made this
// emitter send Claude/codex's nested shape, which gemini's BeforeTool would
// not recognize — making the gemini guard inert. Reverted to a
// gemini-specific top-level shape.
func emitGeminiGuardDecision(d guardDecision) error {
	if d.Allowed {
		return nil
	}
	out := map[string]any{
		"decision": "block",
		"reason":   d.Reason,
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

func guardUsage(err error) error {
	if err == flag.ErrHelp {
		return guardHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, guardHelp())
}

func guardHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, guardHelp())
}

func guardHelp() string {
	return strings.TrimSpace(`
Usage: agentchute guard --pre-tool-use [flags]
       agentchute guard --recover [flags]

PreToolUse-family hook entry (v2.5 plan A7/C25): denies a documented list of
high-blast-radius tool invocations while this session holds claimed-but-unacked
mail. Defense-in-depth only (best-effort substring matching, not a hard
security boundary) — allows everything when the guard is not armed for this
process (no serve session, or the wrapper's hooks cannot clear the latch).

--recover is the mixed-hook-trust recovery path: a vendor's PreToolUse guard
hook active while its Stop hook (which would normally run turn-end) is
independently disabled or failing denies a direct turn-end invocation too
(turn-end is deliberately deny-listed to prevent a same-turn self-unlock
bypass), wedging the lane. --recover restores ONLY mailbox access
(ack/check/turn-end): it archives this session's own claimed mail, clears its
own latch, and sets a session-sticky mark that keeps scope-expanding tools
(pushes, releases, network fetches, deletions, update/setup/clean, hook-config
writes) denied for the REST of this session regardless of latch state.
Nothing a model can invoke clears that mark — only a session boundary (a
fresh ` + "`ac serve`" + ` launch, which mints a new, different serve token) does.
No-op when the guard is disarmed for this process.

Flags:
  --pre-tool-use        evaluate a PreToolUse decision from stdin JSON (default mode)
  --recover             mixed hook-trust recovery: restore mailbox access only
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --codex-hook <event>  emit codex's decision shape (PreToolUse)
  --gemini-hook <event> emit Gemini's decision shape (BeforeTool)
`)
}
