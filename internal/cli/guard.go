package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
// `check` is NOT on the deny list (mail-flow decision 2026-09-17, item B).
// It once was: a second check while this session's latch was armed was
// denied here and by check's own self-denial, and since every gate phase
// blocks on unread mail, mail landing mid-turn forced the lane to end its
// turn just to read it. A re-check cannot reach either thing this guard
// protects — it never archives and never clears the latch; it arms it — so
// the deny bought nothing. Every check replays uncommitted residue as
// REDELIVERED (op.Claim), because a set latch is not proof that every
// claimed message was displayed (check_latch_residue_test.go).
//
// One rule is NOT latch-scoped: a codex thread whose working directory is
// outside the control repo cannot run agentchute bus commands at all
// (evaluateCodexThreadCwd). codex's memory consolidation runs such a thread
// inside the lane's own process, with the lane's identity, and one sent a
// review verdict its lane never wrote (codex_memories.go).
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
var guardPipelineDenySubstrings = append([]string{
	"curl",
	"wget",
	"rm -rf",
}, guardHookConfigPaths...)

func init() {
	// Every hook file agentchute installs is a guarded path: derived from
	// hookWrappers, so adding a wrapper cannot leave its file unguarded.
	for _, w := range hookWrappers {
		dest := strings.ToLower(filepathToSlash(w.Dest))
		found := false
		for _, p := range guardHookConfigPaths {
			if p == dest {
				found = true
				break
			}
		}
		if !found {
			guardHookConfigPaths = append(guardHookConfigPaths, dest)
			guardPipelineDenySubstrings = append(guardPipelineDenySubstrings, dest)
		}
	}
}

func filepathToSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// guardHookConfigPaths are the files — and directories, with a trailing "/" —
// that configure a harness's hooks, so a write there can disable the
// end-of-turn handler: each vendor's project file, the local and user files
// that outrank or extend it (opus-xhigh S5: settings.local.json can set
// disableAllHooks; codex's config.toml holds inline [hooks] and the trust
// table; grok and Antigravity keep hooks in their own trees). Shell text is
// matched against them by substring (guardPipelineDenySubstrings); a file
// tool's target path by guardHookConfigPath.
var guardHookConfigPaths = []string{
	".claude/settings.json",
	".claude/settings.local.json",
	".codex/hooks.json",
	".codex/config.toml",
	".gemini/settings.json",
	".gemini/config/hooks.json",
	".gemini/antigravity-cli/settings.json",
	".agents/hooks.json",
	".grok/hooks/",
	".grok/config.toml",
}

// guardApplyPatchTargetRE captures the file paths an `apply_patch` body
// touches. codex fires PreToolUse for apply_patch with the WHOLE patch in
// tool_input.command, so matching it like a shell command denied every doc
// edit whose diff text merely mentioned `agentchute ack` (opus-xhigh H3c).
// Only the targets can touch a hook config file; the diff body cannot run
// anything.
var guardApplyPatchTargetRE = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$|^\*\*\* Move to: (.+)$`)

// guardApplyPatchTargets returns the paths a patch adds, updates, deletes or
// moves to, one per line of command text; the diff body is dropped.
func guardApplyPatchTargets(patch string) []string {
	var out []string
	for _, m := range guardApplyPatchTargetRE.FindAllStringSubmatch(patch, -1) {
		for _, g := range m[1:] {
			if g = strings.TrimSpace(g); g != "" {
				// Normalize before comparing: `.codex/./hooks.json` and
				// `.claude/sub/../settings.json` name the protected files
				// (codex gate on #213).
				out = append(out, filepath.ToSlash(filepath.Clean(g)))
			}
		}
	}
	return out
}

// guardHookConfigPath reports whether a file tool's target names a hook
// config file or a path inside a hook config directory: the entry itself or a
// longer path ending in it (`/repo/.codex/hooks.json`, `~/.codex/config.toml`,
// `sub/.grok/hooks/x.json`), after cleaning `.` and `..` segments.
func guardHookConfigPath(target string) bool {
	return guardHookConfigPathAt(target, "")
}

func guardHookConfigPathAt(target, cwd string) bool {
	if cwd != "" && !filepath.IsAbs(target) {
		target = filepath.Join(cwd, target)
	}
	if guardHookConfigPathText(target) {
		return true
	}
	// Judge where the write really lands too: a symlinked directory
	// (`/repo/cfg -> /repo/.claude`) or a symlinked file names a hook config
	// file only once resolved. The file may not exist yet, so its directory
	// is resolved and the name re-joined.
	if resolved, ok := guardResolveTarget(target); ok && resolved != target {
		return guardHookConfigPathText(resolved)
	}
	return false
}

// guardResolveTarget resolves the symlinks in a path-shaped string: the whole
// path when it exists, else its directory plus the final name. Strings that
// cannot be a path (a line break or more than PATH_MAX bytes) are not resolved.
// Callers supply actual target fields, so a single-component relative name
// must be resolved too: it can itself be a symlink.
func guardResolveTarget(target string) (string, bool) {
	if target == "" || len(target) > 4096 || strings.ContainsAny(target, "\n\r\x00") {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, true
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, filepath.Base(target)), true
}

// guardHookConfigPathText is the spelling-only match: the cleaned path equals
// a hook config path or ends in one; a directory entry matches below it.
func guardHookConfigPathText(target string) bool {
	t := strings.ToLower(filepath.ToSlash(filepath.Clean(target)))
	for _, p := range guardHookConfigPaths {
		if dir, isDir := strings.CutSuffix(p, "/"); isDir {
			if strings.HasPrefix(t, dir+"/") || strings.Contains(t, "/"+dir+"/") {
				return true
			}
			continue
		}
		if t == p || strings.HasSuffix(t, "/"+p) {
			return true
		}
	}
	return false
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
// `check` is deliberately absent (see the file header): a compound that
// pairs it with any listed token is still denied whole by that token.
var guardAgentchuteSubcmdRE = regexp.MustCompile(`(?:\$\{agentchute_bin:-agentchute\}|\$agentchute_bin|\b(?:agentchute|ac)\b)[ \t]+(ack|turn-end|update|setup|clean)\b`)

// guardStaleOwedHintCommand is the command `check` tells a lane to run when it
// holds a stale reply obligation. It lives here, next to the deny rule that has
// to permit it, because the two drifting apart is the whole of #174 — the hint
// named a command that was denied at the moment it was printed.
func guardStaleOwedHintCommand(agentID string) string {
	return "agentchute clean --owed --as " + agentID
}

// guardFlagRE matches a flag by name in either spelling Go's flag package
// accepts. `--mailbox` and `-mailbox` are the same flag, so a check that only
// knows the double-dash form is a check with a one-character bypass.
func guardFlagRE(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[ \t])--?` + name + `\b`)
}

var (
	guardOwedFlagRE    = guardFlagRE("owed")
	guardMailboxFlagRE = guardFlagRE("mailbox")
)

// guardCleanOwedExempt reports whether every sensitive subcommand in this
// command text is `clean`, scoped to `--owed`.
//
// #174, a deadlock the guard created and could not get out of. `check` arms the
// latch and prints, in the same breath, "prune with: agentchute clean --owed" —
// which the latch then denied. turn-end clears the latch at the END of the turn
// and the next turn opens with another injected `check`, so a lane never had a
// moment where it both knew about the obligation and was allowed to act on it.
// Two lanes hit it independently in one session and neither could clear it.
//
// The deny was protecting nothing there. This guard exists for the claimed-mail
// pipeline; `clean` has exactly two modes and they are mutually exclusive.
// `--mailbox` DELETES a peer's inbox and stays denied. `--owed` prunes this
// agent's own expired reply obligations, which are asker-owned, non-blocking,
// and not mail.
//
// Three properties, each of which is a row:
//
//   - It is scoped to the whole COMMAND TEXT, not to one occurrence. A compound
//     that also runs `clean --mailbox` is denied whole, or `--owed` becomes a
//     prefix that launders whatever follows it.
//   - It is clean-only. An `--owed` flag next to `ack` exempts nothing; that
//     subcommand still matches and still denies. (`check` is no longer a
//     sensitive subcommand at all, so it has nothing to launder.)
//   - It exempts this rule only. The pipeline substrings (curl, rm -rf, hook
//     config writes) are checked afterwards and are unaffected.
func guardCleanOwedExempt(normalized string) bool {
	matches := guardAgentchuteSubcmdRE.FindAllStringSubmatch(normalized, -1)
	if len(matches) == 0 {
		return false
	}
	for _, match := range matches {
		if match[1] != "clean" {
			return false
		}
	}
	return guardOwedFlagRE.MatchString(normalized) && !guardMailboxFlagRE.MatchString(normalized)
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
	return runGuardHook(args, os.Stdin)
}

// runGuardHook is cmdGuard with the hook input passed in, so tests drive it
// without swapping the process-wide os.Stdin (serve's input copier reads that
// variable from a goroutine that outlives cmdServe).
func runGuardHook(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("guard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, controlRepo, loopDir, codexHook, geminiHook, agyHook string
	var preToolUse bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&preToolUse, "pre-tool-use", false, "evaluate a PreToolUse-family hook decision from stdin JSON")
	fs.StringVar(&codexHook, "codex-hook", "", "emit codex's PreToolUse-equivalent decision JSON")
	fs.StringVar(&geminiHook, "gemini-hook", "", "emit Gemini's BeforeTool-family decision JSON")
	fs.StringVar(&agyHook, "agy-hook", "", "emit Antigravity CLI's PreToolUse decision JSON")

	if err := fs.Parse(args); err != nil {
		return guardUsage(err)
	}
	if !preToolUse {
		return guardUsage(fmt.Errorf("--pre-tool-use is required"))
	}
	if fs.NArg() != 0 {
		return guardUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	stdinBody, readErr := io.ReadAll(io.LimitReader(stdin, guardMaxInputBytes+1))
	use := parseGuardToolUse(stdinBody)
	if readErr != nil {
		use.Invalid = true
	}

	// A foreign runner env fails open like every other unresolvable guard state:
	// the latch it would match belongs to another lane, not this process. The
	// stderr warning is the only signal a lane gets that its hooks run on
	// someone else's env.
	decision := guardDecision{Allowed: true}
	if warnRunnerAncestry("guard") {
		if codexHook == "PreToolUse" {
			// Latched or not: a hidden codex thread is a hazard at any time.
			toolName, inputText := guardAllInputStrings(stdinBody)
			decision = evaluateCodexThreadCwd(controlRepo, loopDir, parseGuardHookCwd(stdinBody), toolName, inputText)
		}
		if decision.Allowed {
			decision = evaluateGuardToolUse(agentID, controlRepo, loopDir, use)
		}
	}

	switch {
	case codexHook == "PreToolUse":
		return emitCodexGuardDecision(decision)
	case geminiHook == "BeforeTool":
		return emitGeminiGuardDecision(decision)
	case agyHook == "PreToolUse":
		return emitAgyGuardDecision(decision)
	default:
		return emitClaudeGuardDecision(decision)
	}
}

// evaluateGuardInvocation is the shell-text form of evaluateGuardToolUse,
// kept for callers that hold a command line.
func evaluateGuardInvocation(agentIDFlag, controlRepo, loopDir, toolCmd string) guardDecision {
	return evaluateGuardToolUse(agentIDFlag, controlRepo, loopDir, guardToolUse{Text: toolCmd})
}

// evaluateGuardToolUse is cmdGuard's testable core: resolves the session,
// the agent id, and (only when both resolve) this agent's latch, then
// applies the deny list. Every failure-to-resolve path allows — see the
// guard.go doc comment on why fail-open is the only safe default here.
func evaluateGuardToolUse(agentIDFlag, controlRepo, loopDir string, use guardToolUse) guardDecision {
	session := resolveGuardSession()
	if session == "" {
		return guardDecision{Allowed: true}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return guardDecision{Allowed: true}
	}
	cfg, err := discoverConfig(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return guardDecision{Allowed: true}
	}
	id, err := resolveAgentID(agentIDFlag, cfg)
	if err != nil {
		if strings.TrimSpace(agentIDFlag) == "" && strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")) == "" {
			// Cannot resolve whose latch to check. Guard is armed (a serve
			// session is active) but identity is missing. Fail open (hint only,
			// no stdout noise that would corrupt hook JSON parsing).
			fmt.Fprintln(os.Stderr, "agentchute guard: AGENTCHUTE_AGENT_ID not set; cannot resolve guard latch, allowing (hint: guard only applies inside an `ac serve` session)")
		}
		return guardDecision{Allowed: true}
	}

	return evaluateGuardDecisionFor(cfg, id, session, use)
}

// guardBusAfterBinaryRE matches an agentchute binary token — the name, a
// path ending in it, the `ac` dispatcher, `$AGENTCHUTE_BIN` in any expansion
// form — followed ANYWHERE later by a bus subcommand word. Applied to text
// guardBusCommand has already normalized.
var guardBusAfterBinaryRE = regexp.MustCompile(`(?s)\b(?:agentchute_bin|agentchute|ac)\b.*?\b(send|check|ack|turn-end|clean|setup|update)\b`)

// guardBusQuoting is what the executing shell removes from a word before
// running it: quotes, backslash escapes and line continuations. Dropping them
// here closes spellings such as `agentchute 'send'`, `agent""chute send` and
// `agentchute \<newline> send`; a backtick becomes a word break.
var guardBusQuoting = strings.NewReplacer("\\\n", "", `"`, "", "'", "", `\`, "", "`", " ")

// guardBusCommand reports whether toolCmd may run an agentchute bus command.
// It decides only for a codex thread OUTSIDE the control repo, where no bus
// command is legitimate, so it errs toward yes: quoting is dropped the way the
// shell drops it, and the subcommand may come anywhere after the binary token
// (a variable, flags or another command between). An apply_patch runs
// nothing. Still text matching, not a shell: a name assembled at run time
// (`$(printf agent%s chute) send`) is not seen.
func guardBusCommand(text string) bool {
	normalized := guardBusQuoting.Replace(strings.ToLower(text))
	return guardBusAfterBinaryRE.MatchString(normalized)
}

// guardAllInputStrings returns the tool name and every string anywhere in the
// hook's tool input (nested objects and arrays included, keys in order), one
// per line: what the foreign-thread rule matches. Unlike
// parseGuardToolCommand it knows no tool's argument names, so a command a
// tool carries under any key is seen — codex's write_stdin `chars` typed
// into an already-open shell, a code cell's source.
func guardAllInputStrings(body []byte) (toolName, text string) {
	var in struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return "", ""
	}
	var parts []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			parts = append(parts, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k])
			}
		}
	}
	var input any
	if len(in.ToolInput) > 0 && json.Unmarshal(in.ToolInput, &input) == nil {
		walk(input)
	}
	return in.ToolName, strings.Join(parts, "\n")
}

// parseGuardHookCwd returns the hook input's `cwd` (codex's PreToolUse input
// requires it: pre-tool-use.command.input), or "" when it is absent,
// malformed or not absolute.
func parseGuardHookCwd(body []byte) string {
	var in struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return ""
	}
	cwd := strings.TrimSpace(in.Cwd)
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return cwd
}

// guardPathWithin reports whether path, with every symlink resolved, is dir
// or below it, by file identity walking up (so a symlinked or differently
// cased spelling of the repo counts, and a symlink inside the repo that
// leads out of it does not). A path or dir that cannot be resolved counts as
// within: fail open.
func guardPathWithin(path, dir string) bool {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return true
	}
	for d := resolved; ; {
		if info, err := os.Stat(d); err == nil && os.SameFile(info, dirInfo) {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// codexThreadCwdReasonFmt is the deny text for a bus command from a codex
// thread outside the control repo.
const codexThreadCwdReasonFmt = "this codex thread runs in %s, inside codex's home or outside the control repo %s, so agentchute send/check/ack/turn-end/clean/setup/update are refused from it: codex runs hidden threads (memory consolidation, in ~/.codex/memories) with this lane's identity. A lane's own threads run where serve launched it; if this is one, relaunch the lane from inside the control repo (agentchute §15 guard)"

// evaluateCodexThreadCwd refuses agentchute bus commands from a codex thread
// whose working directory (the hook input's `cwd`) is outside the lane's
// control repo, latched or not. codex's memory consolidation runs such a
// thread inside the lane's own process, with the lane's env, in
// ~/.codex/memories (codex_memories.go); the lane's own threads run where
// serve launched codex. Codex only: the hidden-thread hazard is codex's, and
// Claude Code's `cwd` follows the agent's `cd` ("the new directory after
// Claude runs `cd`", code.claude.com/docs/en/hooks), so a Claude lane working
// in its scratchpad is not foreign. Fails open
// when the cwd is absent, the guard session does not resolve, discovery
// fails, or the lane is remote: an ssh:// lane's local repo is itself derived
// from the working directory, so it cannot anchor this check (serve's
// `--disable memories` is the protection there).
func evaluateCodexThreadCwd(controlRepo, loopDir, hookCwd, toolName, inputText string) guardDecision {
	allow := guardDecision{Allowed: true}
	// apply_patch writes files and runs nothing.
	if hookCwd == "" || toolName == "apply_patch" || !guardBusCommand(inputText) || resolveGuardSession() == "" {
		return allow
	}
	cwd, err := os.Getwd()
	if err != nil {
		return allow
	}
	cfg, err := discoverConfig(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil || cfg.Remote != nil || cfg.ControlRepo == "" {
		return allow
	}
	if !codexHomeContains(hookCwd) && guardPathWithin(hookCwd, cfg.ControlRepo) {
		return allow
	}
	return guardDecision{Allowed: false, Reason: fmt.Sprintf(codexThreadCwdReasonFmt, hookCwd, cfg.ControlRepo)}
}

// evaluateGuardDecision is the shell-text form of evaluateGuardDecisionFor.
func evaluateGuardDecision(cfg *loop.Config, agentID, session, toolCmd string) guardDecision {
	return evaluateGuardDecisionFor(cfg, agentID, session, guardToolUse{Text: toolCmd})
}

// evaluateGuardDecisionFor applies the C25 deny list to one tool use, but ONLY
// when this agent's guard latch is currently held by `session` (C23): a
// latch that is absent, unreadable/corrupt, or belongs to a different
// (foreign/dead) session never triggers a deny (loop.ReadGuardLatch's own
// doc comment covers the foreign-latch case; this function fails open on any
// read error rather than propagate it, since a corrupt latch file must never
// become a way to wedge a lane shut).
func evaluateGuardDecisionFor(cfg *loop.Config, agentID, session string, use guardToolUse) guardDecision {
	latch, err := loop.ReadGuardLatch(cfg, agentID)
	if err != nil {
		return guardDecision{Allowed: true}
	}
	if latch.Session != session {
		return guardDecision{Allowed: true}
	}
	if use.denied() {
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
	// Enforce the work budget before any whole-input scan, including the send
	// exemption and case folding. Oversized input must cost constant time.
	if len(toolCmd) > guardHubBudget {
		return true
	}
	if candidate, inert := guardDirectSendInvocation(toolCmd); candidate {
		return !inert
	}
	lower := strings.ToLower(toolCmd)
	normalized := guardDispatchPrefixRE.ReplaceAllString(lower, "")
	if guardAgentchuteSubcmdRE.MatchString(normalized) && !guardCleanOwedExempt(normalized) {
		return true
	}
	if guardHubInvocation(guardStripToolName(toolCmd)) {
		return true
	}
	for _, pattern := range guardPipelineDenySubstrings {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// guardStripToolName drops the tool-name word parseGuardToolUse puts in
// front of the command text, so the hub rule sees the command itself in
// command position. It drops whatever that word is: a list of known tool names
// failed open for every tool not on it (background security review).
func guardStripToolName(lower string) string {
	trimmed := strings.TrimSpace(lower)
	if i := strings.IndexAny(trimmed, " \t\n"); i >= 0 {
		return strings.TrimSpace(trimmed[i:])
	}
	return ""
}

// guardDirectSendInvocation recognizes only the literal send binaries this
// repo teaches, optionally preceded by the tool-name text parseGuardToolUse
// adds. It returns candidate=true for a send prefix even when later shell
// syntax is unsafe, so a compound send is denied rather than falling through
// to the best-effort substring list.
func guardDirectSendInvocation(toolCmd string) (candidate, inert bool) {
	cmd := strings.TrimSpace(toolCmd)
	// Tool names each wrapper prepends: Claude's Bash, codex's
	// functions.exec_command, Gemini CLI's run_shell_command, Antigravity's
	// run_command (opus-xhigh H6: a latched inert `send --body` under Gemini
	// was denied because the tool name was never stripped).
	for _, prefix := range []string{"Bash ", "functions.exec_command ", "run_shell_command ", "run_command "} {
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
	Cwd       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	// Grok's camelCase spelling of the same two fields
	// (~/.grok/docs/user-guide/10-hooks.md "Input"; opus-xhigh S5).
	ToolNameCamel  string          `json:"toolName"`
	ToolInputCamel json.RawMessage `json:"toolInput"`
	// ToolCall is Antigravity's camelCase shape: {"name": "run_command",
	// "args": {"CommandLine": "...", ...}} (https://antigravity.google/docs/hooks/).
	ToolCall *struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

// guardToolUse separates command fields from the target fields of recognized
// file-writing tools. Read/search tools and document content are never write
// targets. Unknown MCP schemas need an explicit mapping, not a guess based on
// every string they happen to receive. apply_patch uses its own grammar.
type guardToolUse struct {
	Text     string
	Paths    []string
	Cwd      string
	Oversize bool // the input exceeded guardMaxInputBytes and was not judged
	Invalid  bool // non-empty input could not be decoded completely
}

func (u guardToolUse) denied() bool {
	if u.Oversize || u.Invalid {
		return true
	}
	if u.Text != "" && guardCommandDenied(u.Text) {
		return true
	}
	for _, p := range u.Paths {
		if guardHookConfigPathAt(p, u.Cwd) {
			return true
		}
	}
	return false
}

// guardMaxInputBytes bounds the hook input the guard reads. An input past it
// cannot be judged, so it is denied while latched (guardToolUse.Oversize)
// instead of failing to parse and being allowed: a write of a large file onto
// a hook config path must not pass by being large.
const guardMaxInputBytes = 32 << 20

// guardCommandKeys are the input fields that carry a command line, compared
// case-insensitively.
var guardCommandKeys = map[string]bool{"command": true, "cmd": true, "chars": true, "commandline": true, "args": true}

// parseGuardToolUse extracts what to judge from a hook's stdin JSON. Never
// errors: an absent body yields an empty use. A malformed non-empty body is
// denied while latched, so truncation cannot silently bypass the rules. Claude Code,
// codex and Gemini send snake_case `tool_name`/`tool_input`, grok camelCase
// `toolName`/`toolInput`, Antigravity `toolCall{name,args}`.
func parseGuardToolUse(body []byte) guardToolUse {
	if len(body) > guardMaxInputBytes {
		return guardToolUse{Oversize: true}
	}
	var in guardHookInput
	if err := json.Unmarshal(body, &in); err != nil {
		return guardToolUse{Invalid: len(body) != 0}
	}
	name, input := in.ToolName, in.ToolInput
	if name == "" {
		name = in.ToolNameCamel
	}
	if len(input) == 0 {
		input = in.ToolInputCamel
	}
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return guardToolUse{Invalid: true}
		}
	}
	agyArgs := false
	if in.ToolCall != nil {
		if name == "" {
			name = in.ToolCall.Name
		}
		if args == nil {
			args = in.ToolCall.Args
			agyArgs = true
		}
	}
	tool := guardToolBaseName(name)
	cwd := in.Cwd
	for _, key := range []string{"cwd", "workdir", "Cwd"} {
		if dir, ok := args[key].(string); ok && dir != "" {
			if cwd != "" && !filepath.IsAbs(dir) {
				dir = filepath.Join(cwd, dir)
			}
			cwd = dir
			break
		}
	}
	if tool == "apply_patch" {
		var targets []string
		for _, key := range []string{"command", "cmd", "patch", "input"} {
			if s, ok := args[key].(string); ok {
				targets = append(targets, guardApplyPatchTargets(s)...)
			}
		}
		return guardToolUse{Paths: targets, Cwd: cwd}
	}
	if keys := guardWriteTargetKeys(tool); keys != nil {
		var targets []string
		for _, key := range keys {
			if target, ok := args[key].(string); ok && target != "" {
				targets = append(targets, target)
			}
		}
		return guardToolUse{Paths: targets, Cwd: cwd}
	}
	text := make([]string, 0, 2)
	if name != "" {
		text = append(text, name)
	}
	// Every string under a command key is command text, whatever the
	// value's shape: a string, codex's older shell tool's argv array
	// ("command": ["bash", "-lc", "…"]), or a nested object.
	var collect func(v any)
	collect = func(v any) {
		switch x := v.(type) {
		case string:
			text = append(text, x)
		case []any:
			for _, e := range x {
				collect(e)
			}
		case map[string]any:
			for _, k := range sortedKeys(x) {
				collect(x[k])
			}
		}
	}
	for _, k := range sortedKeys(args) {
		if guardCommandKeys[strings.ToLower(k)] {
			collect(args[k])
			continue
		}
		if agyArgs && tool == "run_command" {
			// Preserve the command runner's extra command arguments, without
			// interpreting file content or read-tool paths as shell text.
			collect(args[k])
		}
	}
	return guardToolUse{Text: strings.Join(text, " "), Cwd: cwd}
}

func guardToolBaseName(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(name)
}

func guardWriteTargetKeys(tool string) []string {
	switch tool {
	case "write", "edit", "multiedit":
		return []string{"file_path"}
	case "notebookedit":
		return []string{"notebook_path", "file_path"}
	case "write_file", "edit_file", "append_file":
		return []string{"path", "file_path"}
	case "write_to_file", "replace_file_content", "multi_replace_file_content":
		return []string{"TargetFile"}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
		// "deny" is the documented value; "block" is its alias
		// (geminicli.com/docs/hooks/reference).
		"decision": "deny",
		"reason":   d.Reason,
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

// emitAgyGuardDecision is Antigravity's PreToolUse shape: `decision` is
// REQUIRED on every response (allow|deny|ask|force_ask|deny_unless_prior_grant),
// so a no-objection answer is emitted explicitly — as `ask`, never `allow`
// — unlike the other vendors' silent allow (https://antigravity.google/docs/hooks/).
func emitAgyGuardDecision(d guardDecision) error {
	// "ask" is the ordinary approval path (auto-approve rules and prior
	// grants still apply; "force_ask" is what forces a prompt); "allow" would
	// auto-approve every tool call for a mail-integrity guard that has no
	// opinion (codex gate on #214).
	out := map[string]any{"decision": "ask"}
	if !d.Allowed {
		out = map[string]any{"decision": "deny", "reason": d.Reason}
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
  --agy-hook <event>    emit Antigravity CLI's decision shape (PreToolUse)
`)
}
