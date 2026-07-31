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

// guardDenyReason is the fixed decision text emitted on every deny,
// regardless of which deny-list entry matched (C25's exact wording).
const guardDenyReason = "claimed mail is pending ack; scope-expanding tools and ack/check are denied until the end-of-turn handler archives it (agentchute §9 guard)"

// guardDenySubstrings are matched case-insensitively against the tool's
// command text (tool_name + tool_input's string fields, joined). Plain
// substring match, not argv parsing — documented best-effort (C25). The
// agentchute subcommands themselves are NOT here: they need word-bounded,
// binary-token-aware matching (guardAgentchuteSubcmdRE below), not a literal
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
// guardAgentchuteSubcmdRE untouched (claude-code review, PR #89: proven as a
// live bypass that self-cleared the latch mid-turn and disarmed the entire
// deny list for the rest of the turn).
var guardDispatchPrefixRE = regexp.MustCompile(`\bdispatch\b(?:[ \t]+--shim-dir(?:=\S+|[ \t]+\S+))?(?:[ \t]+--)?[ \t]+`)

// guardAgentchuteSubcmdRE matches any of the sensitive agentchute
// subcommands (C25) regardless of which spelling of the binary invoked
// them: the templated `${AGENTCHUTE_BIN:-agentchute}` form, a bare
// `$AGENTCHUTE_BIN`, the literal `agentchute` binary name, or the `ac`
// dispatcher this repo's own hooks/docs teach as the normal way to invoke
// it. Word-bounded (`\b`), not a plain substring, so it is immune to extra
// whitespace between the binary token and the subcommand — the plain
// substring form this replaced missed `ac turn-end`, `$AGENTCHUTE_BIN
// turn-end`, and even doubled-space `agentchute  turn-end` (claude-code
// review, PR #89; doctor.go:38's hookCheckSubcmdRE is the existing precedent
// in this codebase for matching the binary token robustly instead of a bare
// substring). Apply AFTER guardDispatchPrefixRE strips any dispatch layer.
// The `\b` word-boundary is scoped to ONLY the bare-word alternatives
// (agentchute|ac): a leading `\b` applied uniformly across the whole
// alternation fails to match the $-prefixed forms at all, since `$` is a
// non-word character and a boundary can never hold between two non-word
// characters (e.g. string-start immediately followed by `$`) — caught by
// this file's own test suite once both forms were exercised together.
var guardAgentchuteSubcmdRE = regexp.MustCompile(`(?:\$\{agentchute_bin:-agentchute\}|\$agentchute_bin|\b(?:agentchute|ac)\b)[ \t]+(?:ack|check|turn-end|update|setup|clean)\b`)

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
	var preToolUse, clearStale bool
	var olderThan time.Duration
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&preToolUse, "pre-tool-use", false, "evaluate a PreToolUse-family hook decision from stdin JSON")
	fs.StringVar(&codexHook, "codex-hook", "", "emit codex's PreToolUse-equivalent decision JSON")
	fs.StringVar(&geminiHook, "gemini-hook", "", "emit Gemini's BeforeTool-family decision JSON")
	fs.BoolVar(&clearStale, "clear-stale", false, "recovery: force-clear this agent's guard latch if it is at least --older-than old, regardless of session")
	fs.DurationVar(&olderThan, "older-than", defaultGuardStaleThreshold, "age threshold for --clear-stale")

	if err := fs.Parse(args); err != nil {
		return guardUsage(err)
	}
	if fs.NArg() != 0 {
		return guardUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	if clearStale {
		if preToolUse {
			return guardUsage(fmt.Errorf("--clear-stale and --pre-tool-use are mutually exclusive"))
		}
		return cmdGuardClearStale(agentID, controlRepo, loopDir, olderThan)
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

// defaultGuardStaleThreshold is how old a guard latch must be before
// --clear-stale will touch it. Long enough that no legitimate single turn
// is plausibly still holding it (matches StaleRegThreshold's own "this is
// clearly abandoned" reasoning in gate.go), short enough that a genuinely
// wedged lane (the mixed hook-trust state — PreToolUse active, Stop
// independently disabled — that leaves NEITHER the automatic nor the manual
// turn-end recovery path usable) doesn't stay stuck indefinitely.
const defaultGuardStaleThreshold = 30 * time.Minute

// cmdGuardClearStale resolves identity the same minimal way cmdGuard's
// --pre-tool-use path does (env/--as only, no contextual-default guessing —
// this is a recovery tool, not a registration path) and force-clears a
// stale latch via loop.ClearStaleGuardLatch. See that function's doc
// comment for the full safety reasoning (age, not session identity, is the
// authorization here).
func cmdGuardClearStale(agentIDFlag, controlRepo, loopDir string, olderThan time.Duration) error {
	id := strings.TrimSpace(agentIDFlag)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID"))
	}
	if id == "" {
		return fmt.Errorf("agentchute guard --clear-stale: no agent id (pass --as or set $AGENTCHUTE_AGENT_ID)")
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

	cleared, found, age, err := loop.ClearStaleGuardLatch(cfg, id, olderThan, time.Now().UTC())
	if err != nil {
		return err
	}
	if !found {
		fmt.Println("(no guard latch to clear)")
		return nil
	}
	if !cleared {
		return fmt.Errorf("guard latch is only %s old (< --older-than %s); refusing — this may be an active turn, not an abandoned one", age.Round(time.Second), olderThan)
	}
	fmt.Printf("cleared stale guard latch for %s (age %s)\n", id, age.Round(time.Second))
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

// evaluateGuardDecision applies the C25 deny list against toolCmd, but ONLY
// when this agent's guard latch is currently held by `session` (C23): a
// latch that is absent, unreadable/corrupt, or belongs to a different
// (foreign/dead) session never triggers a deny (loop.ReadGuardLatch's own
// doc comment covers the foreign-latch case; this function fails open on any
// read error rather than propagate it, since a corrupt latch file must never
// become a way to wedge a lane shut).
func evaluateGuardDecision(cfg *loop.Config, agentID, session, toolCmd string) guardDecision {
	latch, err := loop.ReadGuardLatch(cfg, agentID)
	if err != nil {
		return guardDecision{Allowed: true}
	}
	if latch.Session != session {
		return guardDecision{Allowed: true}
	}
	if guardCommandDenied(toolCmd) {
		return guardDecision{Allowed: false, Reason: guardDenyReason}
	}
	return guardDecision{Allowed: true}
}

// guardCommandDenied reports whether toolCmd matches any C25 deny-list
// entry, case-insensitive substring match.
func guardCommandDenied(toolCmd string) bool {
	lower := strings.ToLower(toolCmd)
	normalized := guardDispatchPrefixRE.ReplaceAllString(lower, "")
	if guardAgentchuteSubcmdRE.MatchString(normalized) {
		return true
	}
	for _, pattern := range guardDenySubstrings {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
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
       agentchute guard --clear-stale [--older-than <dur>] [flags]

PreToolUse-family hook entry (v2.5 plan A7/C25): denies a documented list of
high-blast-radius tool invocations while this session holds claimed-but-unacked
mail. Defense-in-depth only (best-effort substring matching, not a hard
security boundary) — allows everything when the guard is not armed for this
process (no serve session, or the wrapper's hooks cannot clear the latch).

--clear-stale is the recovery path for a lane wedged by a mixed hook-trust
state (a vendor's PreToolUse guard hook active while its Stop hook — which
would normally run turn-end — is independently disabled or failing): in that
state the active guard also denies a direct turn-end invocation, since
turn-end is deliberately deny-listed to prevent a same-turn self-unlock
bypass. --clear-stale force-clears the latch by AGE instead of session
identity — a latch at least --older-than old (default 30m) is presumed
abandoned regardless of which session set it. A latch younger than the
threshold is left untouched and the command refuses (exit 1), since it might
be an active turn.

Flags:
  --pre-tool-use        evaluate a PreToolUse decision from stdin JSON (default mode)
  --clear-stale         recovery: force-clear a stale guard latch by age
  --older-than <dur>    age threshold for --clear-stale (default 30m)
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --codex-hook <event>  emit codex's decision shape (PreToolUse)
  --gemini-hook <event> emit Gemini's decision shape (BeforeTool)
`)
}
