package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
// substring match, not argv parsing — documented best-effort (C25).
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
	"agentchute ack",
	"agentchute check",
	"agentchute turn-end",
	"agentchute update",
	"agentchute setup",
	"agentchute clean",
}

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
	var preToolUse bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&preToolUse, "pre-tool-use", false, "evaluate a PreToolUse-family hook decision from stdin JSON")
	fs.StringVar(&codexHook, "codex-hook", "", "emit codex's PreToolUse-equivalent decision JSON")
	fs.StringVar(&geminiHook, "gemini-hook", "", "emit Gemini's BeforeTool-family decision JSON")

	if err := fs.Parse(args); err != nil {
		return guardUsage(err)
	}
	if !preToolUse {
		return guardUsage(fmt.Errorf("--pre-tool-use is required"))
	}
	if fs.NArg() != 0 {
		return guardUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
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

// emitClaudeGuardDecision emits Claude Code's PreToolUse permission-decision
// hookSpecificOutput shape (C25's exact wording). Exit 0 either way — Claude
// reads `permissionDecision` from the JSON for PreToolUse, unlike Stop's
// exit-2 convention. On allow: no stdout, matching emitGateCodexStop /
// emitHookContextJSON's "silence means proceed" convention elsewhere in this
// codebase. This is also the DEFAULT shape (cmdGuard's fallback case) since
// it is the only vendor shape this plan gives verbatim.
func emitClaudeGuardDecision(d guardDecision) error {
	if d.Allowed {
		return nil
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": d.Reason,
		},
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

// emitCodexGuardDecision mirrors gate.go's emitGateCodexStop shape (codex's
// established block/allow decision convention in this codebase): on deny,
// `{"decision":"block","reason":"..."}` to stdout, exit 0; on allow, no
// stdout. codex-cli's actual PreToolUse-equivalent hook surface has not been
// independently verified against codex's own hook docs from this session —
// flagged for codex-agentchute's review (see PR body).
func emitCodexGuardDecision(d guardDecision) error {
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

// emitGeminiGuardDecision uses the same block/reason shape as
// emitCodexGuardDecision, absent any established gemini-specific deny
// convention in this codebase (pending.go's gemini emitter only covers
// additionalContext injection, not a permission decision). Flagged as a
// best-effort judgment call for review.
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

PreToolUse-family hook entry (v2.5 plan A7/C25): denies a documented list of
high-blast-radius tool invocations while this session holds claimed-but-unacked
mail. Defense-in-depth only (best-effort substring matching, not a hard
security boundary) — allows everything when the guard is not armed for this
process (no serve session, or the wrapper's hooks cannot clear the latch).

Flags:
  --pre-tool-use        required marker for this mode
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --codex-hook <event>  emit codex's decision shape (PreToolUse)
  --gemini-hook <event> emit Gemini's decision shape (BeforeTool)
`)
}
