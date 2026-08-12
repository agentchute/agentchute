package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// guard.go — `agentchute guard --pre-tool-use` (v2.5 plan A7, C25; scope
// narrowed by the guard-latch-livelock fix, docs/decisions/
// agentchute-v2.5-implementation-plan.md's C25 row is now historical — see
// AGENTCHUTE.md §15 for the current scope): the PreToolUse-family hook entry
// that denies a short, best-effort SUBSET of tool invocations while THIS
// session holds unacked claimed mail (per-session guard latch,
// loop/guard.go) — specifically, the causal path between "mail claimed" and
// "mail committed" (destroying the claim, or disabling the handler that
// commits it). It is NOT a general scope-expansion guard: every wake cue
// mandates checking inbox at turn start, which arms the latch, so a guard
// that also denied pushes/tags/releases denied exactly the action an
// implementer's turn exists to perform (three lanes livelocked on it the
// same day: claude-code x2, sonnet on PR #110, codex on PR #111). Alex's
// ruling: mail-integrity-only guard, accepting that a guarded lane is no
// longer mechanically protected against scope-expanding actions — that
// control becomes prose (§15's sender-routing rule) and routing judgment,
// same as it always was for unguarded lanes. Defense-in-depth only, honestly
// framed: C25 is case-insensitive substring matching against the tool's own
// command text — an injected instruction can alias around it (e.g. quoting,
// path tricks). It is not a hard security boundary and must never be
// presented as one.
//
// Fails OPEN (allows) whenever it cannot cleanly resolve an armed session or
// this agent's id: a misconfigured or partially-wired guard must never
// itself wedge a serve lane (decision §9 rev 2.3, grok P2).

// guardDenyReason is the fixed decision text emitted on every deny,
// regardless of which deny-list entry matched.
const guardDenyReason = "claimed mail is not yet committed; commands that could destroy it or disable the end-of-turn handler are denied until turn-end runs (agentchute §15 guard)"

// guardPipelineDenySubstrings are matched case-insensitively against the
// tool's command text (tool_name + tool_input's string fields, joined).
// Plain substring match, not argv parsing — documented best-effort. Renamed
// from guardDenySubstrings (guard-latch-livelock fix): this is no longer a
// general high-blast-radius list — `git push`, `git tag`, `gh release`, `gh
// pr merge`, `ssh`, `scp` were cut (pure subtraction; Alex's ruling covers
// exactly this) because none of them can touch claimed-but-uncommitted mail
// or the end-of-turn handler, so denying them only produced the livelock
// with no mail-integrity benefit. `curl`/`wget` stay: `curl … | sh` moves an
// arbitrary payload off the command line and can reach `turn-end`/`rm
// -rf`/hook-config rewrites with no denied token of its own. The agentchute
// subcommands themselves are NOT here: they need word-bounded,
// binary-token-aware matching (guardAgentchuteSubcmdRE below), not a literal
// substring — see that regex's doc comment.
var guardPipelineDenySubstrings = []string{
	"curl",
	"wget",
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

// guardCommandDenied reports whether toolCmd matches guardAgentchuteSubcmdRE
// or any guardPipelineDenySubstrings entry, case-insensitive. A direct,
// single-command `agentchute send`/`ac send` invocation is a known data sink:
// its argument text is not shell syntax, so deny-list words in a quoted body
// are inert. The exception fails closed on compound or expandable shell syntax.
func guardCommandDenied(toolCmd string) bool {
	if candidate, inert := guardDirectSendInvocation(toolCmd); candidate {
		return !inert
	}
	lower := strings.ToLower(toolCmd)
	normalized := guardDispatchPrefixRE.ReplaceAllString(lower, "")
	if guardAgentchuteSubcmdRE.MatchString(normalized) {
		return true
	}
	for _, pattern := range guardPipelineDenySubstrings {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// guardDirectSendInvocation recognizes only the literal send binaries this
// repo teaches, optionally preceded by the tool-name text parseGuardToolCommand
// adds. It returns candidate=true for a send prefix even when later shell
// syntax is unsafe, so a compound send is denied rather than falling through
// to the best-effort substring list.
func guardDirectSendInvocation(toolCmd string) (candidate, inert bool) {
	cmd := strings.TrimSpace(toolCmd)
	for _, prefix := range []string{"Bash ", "functions.exec_command "} {
		if strings.HasPrefix(cmd, prefix) {
			cmd = strings.TrimSpace(strings.TrimPrefix(cmd, prefix))
			break
		}
	}

	words, inert := guardInertShellWords(cmd)
	candidate = len(words) >= 2 && (words[0] == "agentchute" || words[0] == "ac") && words[1] == "send"
	return candidate, inert
}

// guardInertShellWords tokenizes the small shell subset needed to recognize a
// direct send. Quotes and escapes may make argument text inert; executable
// syntax (operators, substitutions, redirections, comments, or malformed
// quoting) rejects the exception. This is deliberately not a general shell
// parser and never strips a quoted or heredoc body before the deny checks.
//
// The invariant this tokenizer exists to hold: text that reads as literal
// data to one layer is live syntax to another. guardCommandDenied decides on
// toolCmd text, but that text is also handed to a DIFFERENT shell — the one
// that actually executes the Bash/exec_command call — which interprets and
// expands it before the command runs. A double-quoted send body is inert
// only to the extent that the executing shell also treats it as inert; where
// the two disagree, the executing shell wins, after this function has
// already said yes. The double-quote branch below rejects a backtick or
// dollar-paren for exactly this reason. It originally stopped there and
// missed a bare $VAR or ${VAR}: also live to the executing shell, but not
// command substitution, so it slipped through. That let a quoted send body
// carry a literal reference to AGENTCHUTE_SERVE_TOKEN which the executing
// shell would expand to the real token value before the body was ever sent
// — the guard's own exception turned into an exfiltration path. A future
// editor adding a case here must ask "is there a layer downstream that
// interprets this differently than this tokenizer does?", not just
// pattern-match against the rows already under test. The same failure class
// has bitten this bus at a different boundary: composing a message body as
// an unquoted heredoc (`<<EOF` instead of `<<'EOF'`) let the composing shell
// evaluate literal backticks and dollar-parens in the message prose before
// the body was ever sent, silently blanking text with no visible error.
// guardIdentityEnvRefLen reports the length of a `$AGENTCHUTE_AGENT_ID` or
// `${AGENTCHUTE_AGENT_ID}` reference starting at s[0] (which must be '$'), or
// 0 if s starts with anything else — including a LONGER variable name that
// merely begins with the identity var's name. This is the one parameter
// expansion the inert-send exception tolerates, because the enrollment docs
// mandate exactly this spelling on every command (`--as/--from
// "$AGENTCHUTE_AGENT_ID"`): rejecting it made the guard deny the send form
// the docs themselves teach, re-creating the livelock the direct-send
// exception exists to prevent (sonnet, report-v2 session, 2026-08-12).
// Downstream-interpretation check (this file's standing question): the
// executing shell expands it to the serve-pinned agent id — public roster
// text carried in every envelope header, never a secret — and POSIX expansion
// does not re-parse the result for operators, so the expanded value can only
// ever be argument data to the one send. Everything else `$`-shaped,
// AGENTCHUTE_SERVE_TOKEN above all, stays rejected.
func guardIdentityEnvRefLen(s string) int {
	const name = "AGENTCHUTE_AGENT_ID"
	if strings.HasPrefix(s, "${"+name+"}") {
		return len(name) + 3
	}
	if !strings.HasPrefix(s, "$"+name) {
		return 0
	}
	rest := s[len(name)+1:]
	if rest != "" {
		if c := rest[0]; c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return 0
		}
	}
	return len(name) + 1
}

func guardInertShellWords(cmd string) ([]string, bool) {
	words := make([]string, 0, 4)
	var word strings.Builder
	inWord := false

	finishWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	reject := func() ([]string, bool) {
		finishWord()
		return words, false
	}

	for i := 0; i < len(cmd); {
		switch c := cmd[i]; {
		case c == ' ' || c == '\t':
			finishWord()
			i++
		case c == '\n' || c == '\r':
			return reject()
		case strings.ContainsRune(";&|<>(){}#`", rune(c)):
			return reject()
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			return reject()
		case c == '$' && guardIdentityEnvRefLen(cmd[i:]) > 0:
			n := guardIdentityEnvRefLen(cmd[i:])
			inWord = true
			word.WriteString(cmd[i : i+n])
			i += n
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '\'':
			inWord = true
			i += 2
			closed := false
			for i < len(cmd) {
				if cmd[i] == '\'' {
					closed = true
					i++
					break
				}
				if cmd[i] == '\\' {
					if i+1 >= len(cmd) || cmd[i+1] == '\n' || cmd[i+1] == '\r' {
						return reject()
					}
					i++
				}
				word.WriteByte(cmd[i])
				i++
			}
			if !closed {
				return reject()
			}
		case c == '$':
			return reject()
		case c == '\'':
			inWord = true
			i++
			start := i
			for i < len(cmd) && cmd[i] != '\'' {
				i++
			}
			if i >= len(cmd) {
				return reject()
			}
			word.WriteString(cmd[start:i])
			i++
		case c == '"':
			inWord = true
			i++
			closed := false
			for i < len(cmd) {
				if cmd[i] == '"' {
					closed = true
					i++
					break
				}
				if cmd[i] == '$' {
					n := guardIdentityEnvRefLen(cmd[i:])
					if n == 0 {
						return reject()
					}
					word.WriteString(cmd[i : i+n])
					i += n
					continue
				}
				if cmd[i] == '`' {
					return reject()
				}
				if cmd[i] == '\\' {
					if i+1 >= len(cmd) || cmd[i+1] == '\n' || cmd[i+1] == '\r' {
						return reject()
					}
					i++
				}
				word.WriteByte(cmd[i])
				i++
			}
			if !closed {
				return reject()
			}
		case c == '\\':
			if i+1 >= len(cmd) || cmd[i+1] == '\n' || cmd[i+1] == '\r' {
				return reject()
			}
			inWord = true
			word.WriteByte(cmd[i+1])
			i += 2
		default:
			inWord = true
			word.WriteByte(c)
			i++
		}
	}
	finishWord()
	return words, true
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

PreToolUse-family hook entry (v2.5 plan A7): denies a short, best-effort
SUBSET of tool invocations — the causal path between claiming mail and
committing it — while this session holds claimed-but-unacked mail. NOT a
general scope-expansion guard (see AGENTCHUTE.md §15). Defense-in-depth only
(best-effort substring matching, not a hard security boundary) — allows
everything when the guard is not armed for this process (no serve session,
or the wrapper's hooks cannot clear the latch).

Flags:
  --pre-tool-use        required marker for this mode
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --codex-hook <event>  emit codex's decision shape (PreToolUse)
  --gemini-hook <event> emit Gemini's decision shape (BeforeTool)
`)
}
