package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// commandHandlers is the single source of truth for agentchute's subcommands.
// main()'s top-level switch and the `ac` dispatcher both resolve commands here,
// so "is this a known command?" can never drift from "what runs it".
var commandHandlers = map[string]func([]string) error{
	"init":         cmdInit,
	"prepare-pool": cmdPreparePool,
	"register":     cmdRegister,
	"boot":         cmdBoot,
	"gate":         cmdGate,
	"defer":        cmdDefer,
	"send":         cmdSend,
	"check":        cmdCheck,
	"ack":          cmdAck,
	"pending":      cmdPending,
	"run":          cmdRun,
	"setup":        cmdSetup,
	"update":       cmdUpdate,
	"self-check":   cmdSelfCheck,
	"self-poll":    cmdSelfPoll,
	"poller":       cmdPoller,
	"default-id":   cmdIdentity,
	"identity":     cmdIdentity,
	"shims":        cmdShims,
	"status":       cmdStatus,
	"doctor":       cmdDoctor,
	"watch":        cmdWatch,
	"presenced":    cmdPresenced,
	"hooks":        cmdHooks,
}

// globalValueFlags are the leading flags the `ac` dispatcher accepts BEFORE the
// subcommand and forwards to it; each consumes the following token as its value
// (unless given in --flag=value form). Bool/unknown leading flags are forwarded
// as a single token.
var globalValueFlags = map[string]bool{
	"--as":           true,
	"--vendor":       true,
	"--control-repo": true,
	"--loop-dir":     true,
}

// dispatchKind enumerates what `ac <args>` resolves to.
type dispatchKind int

const (
	dispatchCommand dispatchKind = iota // route to an agentchute subcommand
	dispatchRun                         // launch a wrapper under `run`
	dispatchHelp
)

// dispatchPlan is the pure result of parsing `ac` args — what to do, with no
// side effects. cmdDispatch executes it; tests assert it.
type dispatchPlan struct {
	Kind        dispatchKind
	Global      []string    // leading global flags (forwarded)
	Command     string      // Kind==dispatchCommand
	CommandArgs []string    // Kind==dispatchCommand (global + sub args)
	Wrapper     wrapperSpec // Kind==dispatchRun
	WrapperArgs []string    // Kind==dispatchRun (extra args after the wrapper token)
}

// splitLeadingGlobalFlags peels leading flag tokens (and the values of known
// value-flags) off the front; the first non-flag token is the subcommand.
func splitLeadingGlobalFlags(args []string) (global, rest []string) {
	i := 0
	for i < len(args) {
		tok := args[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			break // first non-flag token = subcommand
		}
		if tok == "-h" || tok == "--help" {
			break // help is a subcommand, not a global flag
		}
		if tok == "--" {
			i++ // explicit end-of-flags; subcommand follows
			break
		}
		global = append(global, tok)
		i++
		// `--flag value` form for known value-flags consumes the next token.
		if !strings.Contains(tok, "=") && globalValueFlags[tok] && i < len(args) {
			global = append(global, args[i])
			i++
		}
	}
	return global, args[i:]
}

// parseDispatch is the bounded, canonical-only `ac` parser (v0.8.8):
//   - known command          -> route to it
//   - run <wrapper> [args]    -> launch that wrapper
//   - bare known wrapper      -> ERROR "use ac run <wrapper>"
//   - unknown                 -> ERROR with suggestions
//
// It performs NO arbitrary-PATH-executable inference; a command name always
// wins over a same-named wrapper.
func parseDispatch(args []string) (dispatchPlan, error) {
	global, rest := splitLeadingGlobalFlags(args)
	if len(rest) == 0 {
		if len(global) > 0 {
			return dispatchPlan{}, fmt.Errorf("ac: expected a command or `run <wrapper>` after %s", strings.Join(global, " "))
		}
		return dispatchPlan{Kind: dispatchHelp}, nil
	}
	sub := rest[0]
	subArgs := rest[1:]

	switch sub {
	case "-h", "--help", "help":
		return dispatchPlan{Kind: dispatchHelp}, nil
	case "run", "serve":
		// `serve` accepted as a forward-compatible alias for `run` launches.
		if len(subArgs) == 0 {
			return dispatchPlan{}, fmt.Errorf("ac run <wrapper> — known wrappers: %s", strings.Join(knownWrapperTokens(), ", "))
		}
		token := subArgs[0]
		spec, ok := wrapperForToken(token)
		if !ok {
			return dispatchPlan{}, fmt.Errorf("ac run: unknown wrapper %q — known: %s", token, strings.Join(knownWrapperTokens(), ", "))
		}
		return dispatchPlan{Kind: dispatchRun, Global: global, Wrapper: spec, WrapperArgs: subArgs[1:]}, nil
	}

	// Command always wins over a same-named wrapper.
	if _, ok := commandHandlers[sub]; ok {
		cmdArgs := append(append([]string{}, global...), subArgs...)
		return dispatchPlan{Kind: dispatchCommand, Global: global, Command: sub, CommandArgs: cmdArgs}, nil
	}

	// A bare wrapper token is canonical-only: require `ac run <wrapper>`.
	if _, ok := wrapperForToken(sub); ok {
		return dispatchPlan{}, fmt.Errorf("`ac %s` launches a wrapper — use `ac run %s`", sub, sub)
	}

	return dispatchPlan{}, fmt.Errorf("ac: unknown subcommand %q\n  commands: %s\n  wrappers: use `ac run <wrapper>` (%s)",
		sub, strings.Join(commandNamesSorted(), ", "), strings.Join(knownWrapperTokens(), ", "))
}

// cmdDispatch is the `ac` front door. The installed `ac` script execs
// `agentchute dispatch -- "$@"` (wired in Gate 2).
func cmdDispatch(args []string) error {
	plan, err := parseDispatch(args)
	if err != nil {
		return err
	}
	switch plan.Kind {
	case dispatchHelp:
		fmt.Print(dispatchHelpText())
		return nil
	case dispatchCommand:
		return commandHandlers[plan.Command](plan.CommandArgs)
	case dispatchRun:
		return dispatchExecRun(plan)
	}
	return fmt.Errorf("ac: internal dispatch error")
}

// dispatchExecRun resolves the real wrapper binary and re-execs `agentchute run`
// for it, mirroring the generated-shim exec path (without an ac-* shim name).
func dispatchExecRun(plan dispatchPlan) error {
	realWrapper, err := resolveRealWrapper(plan.Wrapper, "")
	if err != nil {
		return err
	}
	wrapperArgs := append([]string{realWrapper}, plan.WrapperArgs...)
	if os.Getenv("AGENTCHUTE_SHIM_BYPASS") == "1" || os.Getenv("AGENTCHUTE_RUNNER") == "1" {
		return execReplace(realWrapper, wrapperArgs, os.Environ())
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		Cwd:            cwd,
		EnvControlRepo: os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:     os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		if loop.IsNoControlRepo(err) {
			return execReplace(realWrapper, wrapperArgs, os.Environ())
		}
		return fmt.Errorf("agentchute dispatch discovery failed from %s: %w", cwd, err)
	}
	agentchuteBin, err := os.Executable()
	if err != nil {
		return err
	}
	runArgs := []string{agentchuteBin, "run", "--vendor", plan.Wrapper.Vendor}
	runArgs = append(runArgs, plan.Global...)
	runArgs = append(runArgs,
		"--control-repo", cfg.ControlRepo,
		"--loop-dir", cfg.LoopDir,
		"--shim-name", "ac",
		"--",
	)
	runArgs = append(runArgs, wrapperArgs...)
	return execReplace(agentchuteBin, runArgs, os.Environ())
}

func commandNamesSorted() []string {
	out := make([]string, 0, len(commandHandlers))
	for name := range commandHandlers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func dispatchHelpText() string {
	return fmt.Sprintf(`ac — the agentchute launcher/dispatcher

Usage:
  ac run <wrapper> [args...]   launch a wrapper under the runner (%s)
  ac <command> [args...]       any agentchute command (%s)

Global flags may precede the subcommand: ac --as <id> run codex
`, strings.Join(knownWrapperTokens(), ", "), strings.Join(commandNamesSorted(), ", "))
}
