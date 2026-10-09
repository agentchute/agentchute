package cli

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// fakeCodexHelp is a `codex --help` text in 0.162's layout: the Commands rows
// are indented by exactly two spaces, options by more.
func fakeCodexHelp(disable, features, noDaemon bool) string {
	var b strings.Builder
	b.WriteString("Usage: codex [OPTIONS] [PROMPT]\n\nCommands:\n  exec              Run Codex non-interactively\n")
	if features {
		b.WriteString("  features          Inspect feature flags\n")
	}
	b.WriteString("\nOptions:\n  -c, --config <key=value>\n          Override a configuration value\n      --enable <FEATURE>\n          Enable a feature (repeatable). Equivalent to `-c features.<name>=true`\n")
	if disable {
		b.WriteString("      --disable <FEATURE>\n          Disable a feature (repeatable). Equivalent to `-c features.<name>=false`\n")
	}
	if noDaemon {
		b.WriteString("      --no-daemon\n          Run without the shared background server\n")
	}
	return b.String()
}

const (
	fakeFeaturesOff     = "external_agent_memory_import             under development  false\nmemories                                 stable             false\n"
	fakeFeaturesOn      = "external_agent_memory_import             under development  false\nmemories                                 stable             true\n"
	fakeFeaturesNoMem   = "apps                                     stable             true\n"
	fakeUnknownMemories = "Error: Unknown feature flag: memories\n"
)

func TestEnsureCodexMemoriesOff_Argv(t *testing.T) {
	codex := codexSpec(t)
	claude := claudeSpec(t)
	rows := []struct {
		name  string
		spec  wrapperSpec
		args  []string
		probe func(string) bool
		want  []string
	}{
		{name: "bare codex gains the pair as a top-level option", spec: codex,
			args: []string{"/opt/homebrew/bin/codex"}, probe: probeYes,
			want: []string{"/opt/homebrew/bin/codex", "--disable", "memories"}},
		{name: "pair lands before the resume subcommand, after argv0", spec: codex,
			args: []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "resume"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "--dangerously-bypass-approvals-and-sandbox", "resume"}},
		{name: "pair lands before exec too", spec: codex,
			args: []string{"codex", "exec", "hello"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "exec", "hello"}},
		{name: "operator passed --disable memories: not duplicated", spec: codex,
			args: []string{"codex", "--disable", "memories", "resume"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "resume"}},
		{name: "operator passed --disable=memories: not duplicated", spec: codex,
			args: []string{"codex", "--disable=memories"}, probe: probeYes,
			want: []string{"codex", "--disable=memories"}},
		{name: "operator chose --enable memories: the operator wins", spec: codex,
			args: []string{"codex", "--enable", "memories"}, probe: probeYes,
			want: []string{"codex", "--enable", "memories"}},
		{name: "operator chose --enable=memories after the subcommand: still wins", spec: codex,
			args: []string{"codex", "resume", "--enable=memories"}, probe: probeYes,
			want: []string{"codex", "resume", "--enable=memories"}},
		{name: "-c features.memories=true: the operator wins", spec: codex,
			args: []string{"codex", "-c", "features.memories=true"}, probe: probeYes,
			want: []string{"codex", "-c", "features.memories=true"}},
		{name: "--config features.memories=false: not duplicated", spec: codex,
			args: []string{"codex", "--config", "features.memories=false"}, probe: probeYes,
			want: []string{"codex", "--config", "features.memories=false"}},
		{name: "--config=features.memories=true", spec: codex,
			args: []string{"codex", "--config=features.memories=true"}, probe: probeYes,
			want: []string{"codex", "--config=features.memories=true"}},
		{name: "-cfeatures.memories=true (attached short value)", spec: codex,
			args: []string{"codex", "-cfeatures.memories=true"}, probe: probeYes,
			want: []string{"codex", "-cfeatures.memories=true"}},
		{name: "-c=features.memories=true", spec: codex,
			args: []string{"codex", "-c=features.memories=true"}, probe: probeYes,
			want: []string{"codex", "-c=features.memories=true"}},
		{name: "-c with spaces around the key", spec: codex,
			args: []string{"codex", "-c", " features.memories =true"}, probe: probeYes,
			want: []string{"codex", "-c", " features.memories =true"}},
		{name: "-c inline features table naming memories", spec: codex,
			args: []string{"codex", "-c", "features={memories=true}"}, probe: probeYes,
			want: []string{"codex", "-c", "features={memories=true}"}},
		{name: "another feature disabled: memories still turned off", spec: codex,
			args: []string{"codex", "--disable", "apps"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "--disable", "apps"}},
		{name: "another -c override: memories still turned off", spec: codex,
			args: []string{"codex", "-c", "model=o3"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "-c", "model=o3"}},
		{name: "a value that merely reads memories is not a choice", spec: codex,
			args: []string{"codex", "-c", "model=memories"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "-c", "model=memories"}},
		{name: "--enable memories after -- is prompt text, not a choice", spec: codex,
			args: []string{"codex", "--", "--enable", "memories"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "--", "--enable", "memories"}},
		{name: "daemon-only form queue: skipped", spec: codex,
			args: []string{"codex", "queue", "list"}, probe: probeYes,
			want: []string{"codex", "queue", "list"}},
		{name: "daemon-only form agents: skipped", spec: codex,
			args: []string{"codex", "agents"}, probe: probeYes,
			want: []string{"codex", "agents"}},
		{name: "--remote: skipped", spec: codex,
			args: []string{"codex", "--remote", "resume"}, probe: probeYes,
			want: []string{"codex", "--remote", "resume"}},
		{name: "a prompt that merely contains the word agents is not the subcommand", spec: codex,
			args: []string{"codex", "list my agents"}, probe: probeYes,
			want: []string{"codex", "--disable", "memories", "list my agents"}},
		{name: "probe says no: argv untouched", spec: codex,
			args: []string{"codex", "resume"}, probe: probeNo,
			want: []string{"codex", "resume"}},
		{name: "other wrappers untouched even when the probe would say yes", spec: claude,
			args: []string{"claude", "--model", "sonnet"}, probe: probeYes,
			want: []string{"claude", "--model", "sonnet"}},
		{name: "unknown wrapper untouched", spec: wrapperSpec{},
			args: []string{"something", "else"}, probe: probeYes,
			want: []string{"something", "else"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			probed := false
			probe := func(bin string) bool {
				probed = true
				if bin != row.args[0] {
					t.Errorf("probe ran against %q, want argv0 %q", bin, row.args[0])
				}
				return row.probe(bin)
			}
			orig := append([]string(nil), row.args...)
			got := ensureCodexMemoriesOff(row.spec, row.args, probe)
			if strings.Join(got, "\x00") != strings.Join(row.want, "\x00") {
				t.Fatalf("argv = %q, want %q", got, row.want)
			}
			if strings.Join(row.args, "\x00") != strings.Join(orig, "\x00") {
				t.Fatalf("input argv mutated: %q", row.args)
			}
			wantProbe := row.spec.Key == "codex" && !codexMemoriesExplicit(row.args[1:]) && !codexNoDaemonIncompatible(row.args[1:])
			if probed != wantProbe {
				t.Fatalf("probe ran = %v, want %v (no probe when the operator chose, the form is daemon-only, or the wrapper is not codex)", probed, wantProbe)
			}
		})
	}
}

// installCodexFakes replaces both codex probes for one test and records what
// the features probe was asked.
type codexFakeCalls struct {
	help     []string
	features [][]string
	dirs     []string
}

func installCodexFakes(t *testing.T, help string, helpErr error, features string, featuresErr error) *codexFakeCalls {
	t.Helper()
	restoreHelp, restoreFeatures := codexHelpOutput, codexFeaturesOutput
	t.Cleanup(func() { codexHelpOutput, codexFeaturesOutput = restoreHelp, restoreFeatures })
	calls := &codexFakeCalls{}
	codexHelpOutput = func(bin string, _ []string) (string, error) {
		calls.help = append(calls.help, bin)
		return help, helpErr
	}
	codexFeaturesOutput = func(bin string, _ []string, dir string, args []string) (string, error) {
		calls.features = append(calls.features, append([]string(nil), args...))
		calls.dirs = append(calls.dirs, dir)
		return features, featuresErr
	}
	return calls
}

func TestProbeCodexMemoriesOff(t *testing.T) {
	exit1 := errors.New("exit status 1")
	rows := []struct {
		name         string
		help         string
		helpErr      error
		features     string
		featuresErr  error
		want         codexMemoriesProbe
		wantFeatures bool
	}{
		{name: "takes the flag and reports the feature off", help: fakeCodexHelp(true, true, true), features: fakeFeaturesOff,
			want: codexMemoriesOffWorks, wantFeatures: true},
		{name: "help fails", helpErr: errors.New("exec: not found"),
			want: codexMemoriesUnknown},
		{name: "no --disable: no feature flags at all", help: fakeCodexHelp(false, true, true),
			want: codexMemoriesAbsent},
		{name: "no features command: never run `features list` (an older codex would read it as a prompt)", help: fakeCodexHelp(true, false, true),
			want: codexMemoriesAbsent},
		{name: "codex predates the memories feature: Unknown feature flag", help: fakeCodexHelp(true, true, true), features: fakeUnknownMemories, featuresErr: exit1,
			want: codexMemoriesAbsent, wantFeatures: true},
		{name: "features list fails another way", help: fakeCodexHelp(true, true, true), features: "boom\n", featuresErr: exit1,
			want: codexMemoriesUnknown, wantFeatures: true},
		{name: "no memories row", help: fakeCodexHelp(true, true, true), features: fakeFeaturesNoMem,
			want: codexMemoriesAbsent, wantFeatures: true},
		{name: "still on after --disable memories", help: fakeCodexHelp(true, true, true), features: fakeFeaturesOn,
			want: codexMemoriesUnknown, wantFeatures: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			calls := installCodexFakes(t, row.help, row.helpErr, row.features, row.featuresErr)
			got, why := probeCodexMemoriesOff("/bin/codex", nil, codexHelpMemo(nil))
			if got != row.want {
				t.Fatalf("probe = %v (%s), want %v", got, why, row.want)
			}
			if (got == codexMemoriesUnknown) != (why != "") {
				t.Fatalf("reason %q for result %v: only an unknown result carries one", why, got)
			}
			if ran := len(calls.features) > 0; ran != row.wantFeatures {
				t.Fatalf("features list ran = %v, want %v", ran, row.wantFeatures)
			}
			if row.wantFeatures && strings.Join(calls.features[0], " ") != "--disable memories" {
				t.Fatalf("features probe args = %q, want the exact pair serve inserts", calls.features[0])
			}
		})
	}
}

// Both launch forms get both flags from ONE `codex --help` run: `ac serve
// codex ...` (dispatch → serve) and a hand-typed `agentchute serve -- codex`.
func TestServeWrapperArgs_AddsDisableMemoriesWithNoDaemon(t *testing.T) {
	calls := installCodexFakes(t, fakeCodexHelp(true, true, true), nil, fakeFeaturesOff, nil)

	plan, err := parseDispatch([]string{"serve", "codex", "--dangerously-bypass-approvals-and-sandbox", "resume"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &loop.Config{ControlRepo: "/repo", LoopDir: "/repo/.agentchute/loop"}
	runArgs := buildDispatchRunArgs("/bin/agentchute", plan.Wrapper.Vendor, nil, cfg, append([]string{"/opt/homebrew/bin/codex"}, plan.WrapperArgs...))
	sep := -1
	for i, a := range runArgs {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("no -- separator in %q", runArgs)
	}
	got, spec := serveWrapperArgs(runArgs[sep+1:], nil)
	want := []string{"/opt/homebrew/bin/codex", "--disable", "memories", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "resume"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") || spec.Key != "codex" {
		t.Fatalf("dispatch→serve argv = %q (spec %q), want %q", got, spec.Key, want)
	}
	if len(calls.help) != 1 || len(calls.features) != 1 {
		t.Fatalf("help ran %d times, features %d; want one each (help shared by both probes)", len(calls.help), len(calls.features))
	}

	got, _ = serveWrapperArgs([]string{"codex", "resume"}, nil)
	want = []string{"codex", "--disable", "memories", "--no-daemon", "resume"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("hand-typed serve argv = %q, want %q", got, want)
	}
}

func TestApplyCodexLaunchArgsWarnsOnlyWhenTheMemoriesProbeFails(t *testing.T) {
	for _, row := range []struct {
		name, features string
		featuresErr    error
		wantWarn       bool
		wantArgv       []string
	}{
		{name: "probe fails: warn, launch without the pair", features: "boom\n", featuresErr: errors.New("exit status 1"), wantWarn: true,
			wantArgv: []string{"codex", "--no-daemon"}},
		{name: "no memories feature: nothing to say", features: fakeUnknownMemories, featuresErr: errors.New("exit status 1"),
			wantArgv: []string{"codex", "--no-daemon"}},
		{name: "works: pair inserted, no warning", features: fakeFeaturesOff,
			wantArgv: []string{"codex", "--disable", "memories", "--no-daemon"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			installCodexFakes(t, fakeCodexHelp(true, true, true), nil, row.features, row.featuresErr)
			var got []string
			stderr := captureStderr(t, func() { got = applyCodexLaunchArgs([]string{"codex"}, nil) })
			if strings.Join(got, "\x00") != strings.Join(row.wantArgv, "\x00") {
				t.Fatalf("argv = %q, want %q", got, row.wantArgv)
			}
			if warned := strings.Contains(stderr, "--disable memories"); warned != row.wantWarn {
				t.Fatalf("memories warning = %v, want %v; stderr:\n%s", warned, row.wantWarn, stderr)
			}
		})
	}
}

// The real probes against a real (fake) codex binary, through serve's own
// launch path: what codex is actually run with, and what serve launches.
func TestServeLaunchesCodexWithMemoriesOff(t *testing.T) {
	const (
		featuresOff     = "off"
		featuresUnknown = "unknown"
	)
	for _, row := range []struct {
		name         string
		features     bool   // help lists the features command
		featuresMode string // what `--disable memories features list` does
		extra        []string
		wantLaunched []string
		wantProbed   bool
	}{
		{name: "current codex: memories turned off", features: true, featuresMode: featuresOff,
			wantLaunched: []string{"--disable", "memories", "--no-daemon"}, wantProbed: true},
		{name: "operator keeps memories on with --enable memories", features: true, featuresMode: featuresOff,
			extra:        []string{"--enable", "memories"},
			wantLaunched: []string{"--no-daemon", "--enable", "memories"}},
		{name: "codex without the memories feature still launches", features: true, featuresMode: featuresUnknown,
			wantLaunched: []string{"--no-daemon"}, wantProbed: true},
		{name: "codex without a features command is never run with `features list`", features: false,
			wantLaunched: []string{"--no-daemon"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := setupShortRunFixture(t)
			invocations := filepath.Join(root, "invocations")
			launched := filepath.Join(root, "launched")
			helpText := fakeCodexHelp(true, row.features, true)
			featuresBranch := `  echo "memories                                 stable             false"; exit 0`
			if row.featuresMode == featuresUnknown {
				featuresBranch = `  echo "Error: Unknown feature flag: memories" >&2; exit 1`
			}
			script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
if [ "$1" = "--help" ]; then
  cat <<'HELP'
%sHELP
  exit 0
fi
if [ "$*" = "--disable memories features list" ]; then
%s
fi
for a in "$@"; do printf '%%s\n' "$a"; done > %s
`, shellQuote(invocations), helpText, featuresBranch, shellQuote(launched))
			wrapper := filepath.Join(root, "codex")
			mustWrite(t, wrapper, []byte(script))
			if err := os.Chmod(wrapper, 0o755); err != nil {
				t.Fatal(err)
			}
			withCwd(t, root, func() {
				args := append([]string{
					"--as", "codex",
					"--control-repo", root,
					"--loop-dir", filepath.Join(root, ".agentchute", "loop"),
					"--interval", "5",
					"--idle-grace", "100ms",
					"--", wrapper,
				}, row.extra...)
				if err := cmdServe(args); err != nil {
					t.Fatalf("cmdServe err = %v", err)
				}
			})
			got := strings.Fields(string(mustRead(t, launched)))
			if strings.Join(got, " ") != strings.Join(row.wantLaunched, " ") {
				t.Fatalf("serve launched codex with %q, want %q", got, row.wantLaunched)
			}
			runs := string(mustRead(t, invocations))
			if probed := strings.Contains(runs, "--disable memories features list"); probed != row.wantProbed {
				t.Fatalf("features probe ran = %v, want %v; invocations:\n%s", probed, row.wantProbed, runs)
			}
			for _, line := range strings.Split(strings.TrimSpace(runs), "\n") {
				if strings.Contains(line, "features") && line != "--disable memories features list" {
					t.Fatalf("codex was run with %q: only the exact probe may name `features`", line)
				}
			}
		})
	}
}

// ---------- guard: codex threads outside the control repo ----------

// runCodexGuardHook drives `agentchute guard --pre-tool-use` with one hook
// input and reports the decision. args selects the vendor emitter.
func runCodexGuardHook(t *testing.T, args []string, input map[string]any) (denied bool, reason string) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	// runGuardHook, not withStdin: an earlier serve row's input copier still
	// reads os.Stdin, so swapping it here is a data race.
	out, err := captureStdout(t, func() error { return runGuardHook(args, strings.NewReader(string(body))) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) == "" {
		return false, ""
	}
	var resp struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("guard output %q: %v", out, err)
	}
	return resp.HookSpecificOutput.PermissionDecision == "deny", resp.HookSpecificOutput.PermissionDecisionReason
}

func codexShellInput(cwd, command string) map[string]any {
	in := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "s",
		"turn_id":         "t",
		"tool_name":       "functions.exec_command",
		"tool_input":      map[string]any{"cmd": command},
	}
	if cwd != "" {
		in["cwd"] = cwd
	}
	return in
}

// A codex thread whose working directory is outside the control repo cannot
// run bus commands: codex's memory consolidation runs such a thread inside the
// lane's process with the lane's env. No latch is involved: the hidden send on
// 2026-10-08 needed none.
func TestCodexGuardRefusesBusCommandsFromAThreadOutsideTheControlRepo(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	memories := filepath.Join(t.TempDir(), ".codex", "memories")
	worktree := filepath.Join(root, ".tmp", "worktrees", "x")
	homeMemories := filepath.Join(root, ".codex", "memories")       // #222: a pool at $HOME
	customMemories := filepath.Join(root, "codex-home", "memories") // #222: $CODEX_HOME inside the pool
	for _, d := range []string{memories, worktree, homeMemories, customMemories} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	// A symlink INSIDE the repo that leads out of it, to the memory dir.
	escape := filepath.Join(root, "memories-link")
	if err := os.Symlink(memories, escape); err != nil {
		t.Fatal(err)
	}
	withInput := func(tool string, input map[string]any) func(cwd string) map[string]any {
		return func(cwd string) map[string]any {
			return map[string]any{"hook_event_name": "PreToolUse", "cwd": cwd, "tool_name": tool, "tool_input": input}
		}
	}
	codexHook := []string{"--pre-tool-use", "--codex-hook", "PreToolUse"}
	rows := []struct {
		name    string
		args    []string
		cwd     string
		cmd     string
		armed   bool
		wantDen bool
		input   func(cwd string) map[string]any // nil: a shell call running cmd
		env     map[string]string
	}{
		{name: "memory thread in a pool at $HOME", args: codexHook, cwd: homeMemories, cmd: "agentchute send --body x", armed: true, wantDen: true, env: map[string]string{"HOME": root, "CODEX_HOME": ""}},
		{name: "lane thread in a pool at $HOME", args: codexHook, cwd: root, cmd: "agentchute send --body x", armed: true, env: map[string]string{"HOME": root, "CODEX_HOME": ""}},
		{name: "memory thread under a custom CODEX_HOME in the pool", args: codexHook, cwd: customMemories, cmd: "agentchute send --body x", armed: true, wantDen: true, env: map[string]string{"CODEX_HOME": filepath.Join(root, "codex-home")}},
		{name: "write_stdin types the command into an open shell", args: codexHook, cwd: memories, armed: true, wantDen: true,
			input: withInput("write_stdin", map[string]any{"session_id": 7, "chars": "agentchute send --from bob --to alice --body SHIP\n"})},
		{name: "a code cell carries the command under its own key", args: codexHook, cwd: memories, armed: true, wantDen: true,
			input: withInput("exec", map[string]any{"source": "await tools.exec_command({cmd: 'agentchute send --body x'})"})},
		{name: "an argv array under an unknown key", args: codexHook, cwd: memories, armed: true, wantDen: true,
			input: withInput("functions.exec_command", map[string]any{"argv": []any{"sh", "-c", "agentchute check --as bob"}})},
		{name: "apply_patch writes a file and runs nothing", args: codexHook, cwd: memories, armed: true,
			input: withInput("apply_patch", map[string]any{"command": "*** Begin Patch\n*** Add File: notes.md\n+run agentchute send --body-file reply.md\n*** End Patch\n"})},
		{name: "a symlink inside the repo that leads out of it", args: codexHook, cwd: escape, cmd: "agentchute send --body x", armed: true, wantDen: true},
		{name: "memory thread sends", args: codexHook, cwd: memories, cmd: "agentchute send --from bob --to alice --body SHIP", armed: true, wantDen: true},
		{name: "memory thread checks", args: codexHook, cwd: memories, cmd: "agentchute check --as bob", armed: true, wantDen: true},
		{name: "memory thread acks", args: codexHook, cwd: memories, cmd: "agentchute ack --as bob", armed: true, wantDen: true},
		{name: "memory thread turn-end", args: codexHook, cwd: memories, cmd: "agentchute turn-end", armed: true, wantDen: true},
		{name: "memory thread clean --owed", args: codexHook, cwd: memories, cmd: "agentchute clean --owed --as bob", armed: true, wantDen: true},
		{name: "memory thread setup", args: codexHook, cwd: memories, cmd: "agentchute setup --wake runner --yes", armed: true, wantDen: true},
		{name: "memory thread update", args: codexHook, cwd: memories, cmd: "agentchute update", armed: true, wantDen: true},
		{name: "ac dispatcher spelling", args: codexHook, cwd: memories, cmd: "ac send --from bob --to alice --body x", armed: true, wantDen: true},
		{name: "templated binary spelling, quoted", args: codexHook, cwd: memories, cmd: `"${AGENTCHUTE_BIN:-agentchute}" send --body x`, armed: true, wantDen: true},
		{name: "dispatch layer", args: codexHook, cwd: memories, cmd: "agentchute dispatch --shim-dir /x -- send --body x", armed: true, wantDen: true},
		{name: "absolute binary path inside a compound", args: codexHook, cwd: memories, cmd: "go test ./... && /usr/local/bin/agentchute send --from bob --to alice --body ok", armed: true, wantDen: true},
		{name: "quoted subcommand", args: codexHook, cwd: memories, cmd: "agentchute 'send' --body x", armed: true, wantDen: true},
		{name: "double-quoted subcommand", args: codexHook, cwd: memories, cmd: `agentchute "send" --body x`, armed: true, wantDen: true},
		{name: "quotes inside the binary name", args: codexHook, cwd: memories, cmd: "agent''chute send --body x", armed: true, wantDen: true},
		{name: "backslash inside the subcommand", args: codexHook, cwd: memories, cmd: `agentchute s\end --body x`, armed: true, wantDen: true},
		{name: "line continuation between binary and subcommand", args: codexHook, cwd: memories, cmd: "agentchute \\\n  send --body x", armed: true, wantDen: true},
		{name: "braced variable without a default", args: codexHook, cwd: memories, cmd: "${AGENTCHUTE_BIN} send --body x", armed: true, wantDen: true},
		{name: "binary through a shell variable", args: codexHook, cwd: memories, cmd: "x=agentchute; $x send --body x", armed: true, wantDen: true},
		{name: "upper case (case-insensitive file system)", args: codexHook, cwd: memories, cmd: "AGENTCHUTE SEND --body x", armed: true, wantDen: true},
		{name: "errs toward deny: a grep for the words", args: codexHook, cwd: memories, cmd: `grep -r "agentchute send" .`, armed: true, wantDen: true},
		{name: "lane thread with a quoted subcommand is still its own business", args: codexHook, cwd: root, cmd: "agentchute 'send' --body x", armed: true},
		{name: "memory thread reads status: not a bus command", args: codexHook, cwd: memories, cmd: "agentchute status", armed: true},
		{name: "memory thread runs other tools", args: codexHook, cwd: memories, cmd: "ls -la", armed: true},
		{name: "lane thread in the control repo sends", args: codexHook, cwd: root, cmd: "agentchute send --from bob --to alice --body ok", armed: true},
		{name: "lane thread in a worktree under the repo sends", args: codexHook, cwd: worktree, cmd: "agentchute send --from bob --to alice --body ok", armed: true},
		{name: "lane thread through a symlink to the repo sends", args: codexHook, cwd: link, cmd: "agentchute send --from bob --to alice --body ok", armed: true},
		{name: "cwd absent: fail open", args: codexHook, cwd: "", cmd: "agentchute send --body x", armed: true},
		{name: "cwd relative: fail open", args: codexHook, cwd: "memories", cmd: "agentchute send --body x", armed: true},
		{name: "cwd does not exist: fail open", args: codexHook, cwd: filepath.Join(memories, "gone"), cmd: "agentchute send --body x", armed: true},
		{name: "not a guarded serve lane: fail open", args: codexHook, cwd: memories, cmd: "agentchute send --body x"},
		{name: "Claude hook input: cwd follows cd, never foreign", args: []string{"--pre-tool-use"}, cwd: memories, cmd: "agentchute send --body x", armed: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			withCwd(t, root, func() {
				clearGuardEnv(t)
				t.Setenv("AGENTCHUTE_RUNNER_PID", "")
				t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
				t.Setenv("AGENTCHUTE_CONTROL_REPO", root)
				t.Setenv("AGENTCHUTE_LOOP_DIR", cfg.LoopDir)
				for k, v := range row.env {
					t.Setenv(k, v)
				}
				if row.armed {
					armGuard(t, "tok-memories")
				}
				in := codexShellInput(row.cwd, row.cmd)
				if row.input != nil {
					in = row.input(row.cwd)
				}
				if len(row.args) == 1 {
					in["tool_name"] = "Bash"
					in["tool_input"] = map[string]any{"command": row.cmd}
				}
				denied, reason := runCodexGuardHook(t, row.args, in)
				if denied != row.wantDen {
					t.Fatalf("denied = %v (%s), want %v", denied, reason, row.wantDen)
				}
				if denied && (!strings.Contains(reason, "outside the control repo") || !strings.Contains(reason, row.cwd)) {
					t.Fatalf("deny reason does not name the thread's directory: %q", reason)
				}
				if _, err := loop.ReadGuardLatch(cfg, "bob"); err == nil {
					t.Fatal("a latch exists: these rows must hold with no claimed mail at all")
				}
			})
		})
	}
}

// ---------- doctor: codex_memories ----------

func TestDoctorCodexMemories(t *testing.T) {
	const servePID = 4242
	lane := func(argv ...string) []laneProcess {
		return []laneProcess{{PID: 777, Argv: argv}}
	}
	rows := []struct {
		name         string
		children     []laneProcess
		listErr      error
		help         string
		features     string
		featuresErr  error
		wantSeverity string
		wantContains []string
		wantFeatures []string // nil: features list must not run
	}{
		{name: "launched with --disable memories", children: lane("codex", "--disable", "memories", "--no-daemon"),
			wantSeverity: severityOK, wantContains: []string{"--disable memories"}},
		{name: "launched without it and codex reports memories on", children: lane("/x/codex", "--no-daemon", "-c", "model=o3"),
			help: fakeCodexHelp(true, true, true), features: fakeFeaturesOn,
			wantSeverity: severityWarn, wantContains: []string{"memories are ON", "777", "ac serve codex", "--disable memories"},
			wantFeatures: []string{"-c", "model=o3"}},
		{name: "launched without it but codex reports memories off", children: lane("codex", "--no-daemon"),
			help: fakeCodexHelp(true, true, true), features: fakeFeaturesOff,
			wantSeverity: severityOK, wantContains: []string{"off"}, wantFeatures: []string{}},
		{name: "operator kept them on with --enable memories", children: lane("codex", "--no-daemon", "--enable", "memories"),
			help: fakeCodexHelp(true, true, true), features: fakeFeaturesOn,
			wantSeverity: severityWarn, wantContains: []string{"memories are ON"}, wantFeatures: []string{"--enable", "memories"}},
		{name: "--disable then --enable: enable wins, so ask codex", children: lane("codex", "--disable", "memories", "--enable=memories"),
			help: fakeCodexHelp(true, true, true), features: fakeFeaturesOn,
			wantSeverity: severityWarn, wantFeatures: []string{"--disable", "memories", "--enable=memories"}},
		{name: "codex has no memories feature", children: lane("codex"),
			help: fakeCodexHelp(true, true, true), features: fakeUnknownMemories, featuresErr: errors.New("exit status 1"),
			wantSeverity: severityOK, wantContains: []string{"no memories feature"}, wantFeatures: []string{}},
		{name: "features list fails another way", children: lane("codex"),
			help: fakeCodexHelp(true, true, true), features: "boom", featuresErr: errors.New("exit status 2"),
			wantSeverity: severityWarn, wantContains: []string{"could not read", "exit status 2"}, wantFeatures: []string{}},
		{name: "codex without a features command: never run it", children: lane("codex"),
			help: fakeCodexHelp(true, false, true), wantSeverity: severitySkip, wantContains: []string{"no `features` command"}},
		{name: "the lane runs claude, not codex", children: lane("claude", "--model", "fable"),
			wantSeverity: severitySkip, wantContains: []string{"not running codex"}},
		{name: "process table unreadable", listErr: errors.New("ps: boom"),
			wantSeverity: severitySkip, wantContains: []string{"ps: boom"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			root, cfg := setupConsumeFixture(t)
			calls := installCodexFakes(t, row.help, nil, row.features, row.featuresErr)
			restore := listServeChildren
			t.Cleanup(func() { listServeChildren = restore })
			listServeChildren = func(pid int) ([]laneProcess, error) {
				if pid != servePID {
					t.Errorf("listed children of pid %d, want the runner pid %d", pid, servePID)
				}
				return row.children, row.listErr
			}
			t.Setenv("AGENTCHUTE_AGENT_ID", "codex")
			t.Setenv("AGENTCHUTE_RUNNER_PID", fmt.Sprint(servePID))
			c := checkCodexMemories(cfg, "codex")
			if c.Name != "codex_memories" || c.Severity != row.wantSeverity {
				t.Fatalf("check = %+v, want severity %s", c, row.wantSeverity)
			}
			for _, s := range row.wantContains {
				if !strings.Contains(c.Message, s) {
					t.Fatalf("message %q lacks %q", c.Message, s)
				}
			}
			if row.wantFeatures == nil {
				if len(calls.features) != 0 {
					t.Fatalf("features list ran with %q; it must not", calls.features)
				}
				return
			}
			if len(calls.features) != 1 {
				t.Fatalf("features list ran %d times, want once", len(calls.features))
			}
			if strings.Join(calls.features[0], "\x00") != strings.Join(row.wantFeatures, "\x00") {
				t.Fatalf("features list got %q, want the lane's own feature options %q", calls.features[0], row.wantFeatures)
			}
			if calls.dirs[0] != root {
				t.Fatalf("features list ran in %q, want the control repo %q", calls.dirs[0], root)
			}
		})
	}
}

// Outside the lane, doctor finds serve through the local claim — only when
// the claim was written on this host.
func TestDoctorCodexMemoriesFindsServeThroughTheLocalClaim(t *testing.T) {
	for _, row := range []struct {
		name       string
		host       string
		wantListed bool
	}{
		{name: "claim from this host", host: localHostname(), wantListed: true},
		{name: "claim from another host", host: "elsewhere.example"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, cfg := setupConsumeFixture(t)
			dir := cfg.AgentStateDir("codex")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			claim := loop.ServeClaim{ID: "codex", Host: row.host, PID: 31337, ServeToken: "t", StartedAt: time.Now(), LastSeen: time.Now()}
			data, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(dir, "serve.claim"), data)
			t.Setenv("AGENTCHUTE_AGENT_ID", "someone-else")
			t.Setenv("AGENTCHUTE_RUNNER_PID", "")
			restore := listServeChildren
			t.Cleanup(func() { listServeChildren = restore })
			listed := 0
			listServeChildren = func(pid int) ([]laneProcess, error) {
				listed = pid
				return nil, nil
			}
			c := checkCodexMemories(cfg, "codex")
			if (listed == 31337) != row.wantListed || c.Severity != severitySkip {
				t.Fatalf("listed pid %d, check %+v; want listed=%v and SKIP", listed, c, row.wantListed)
			}
		})
	}
}

// ---------- exact argv readers ----------

func TestParseProcArgs2Argv(t *testing.T) {
	var buf []byte
	buf = binary.NativeEndian.AppendUint32(buf, 4)
	buf = append(buf, "/Users/a b/.local/bin/codex\x00\x00\x00\x00"...)
	buf = append(buf, "/Users/a b/.local/bin/codex\x00--disable\x00memories\x00a prompt with spaces\x00"...)
	buf = append(buf, "HOME=/Users/a b\x00AGENTCHUTE_SERVE_TOKEN=x\x00\x00"...)
	argv, err := parseProcArgs2Argv(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Users/a b/.local/bin/codex", "--disable", "memories", "a prompt with spaces"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	short := binary.NativeEndian.AppendUint32(nil, 3)
	short = append(short, "/bin/codex\x00codex\x00"...)
	if _, err := parseProcArgs2Argv(short); err == nil {
		t.Fatal("a buffer with fewer strings than argc was accepted")
	}
	if _, err := parseProcArgs2Argv([]byte{1}); err == nil {
		t.Fatal("truncated buffer accepted")
	}
	if got := splitNULArgv([]byte("codex\x00--disable\x00memories\x00")); strings.Join(got, " ") != "codex --disable memories" {
		t.Fatalf("cmdline split = %q", got)
	}
	if got := splitNULArgv(nil); got != nil {
		t.Fatalf("empty cmdline = %q, want nil", got)
	}
}

// The real reader against a child of this test: exact argv, spaces kept.
func TestReadProcessArgv_Child(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no exact argv source on " + runtime.GOOS)
	}
	if os.Getenv("AGENTCHUTE_TEST_ARGV_CHILD") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run", "^TestReadProcessArgv_Child$", "-test.v=false", "--", "--disable", "memories", "a prompt with spaces")
	cmd.Env = append(os.Environ(), "AGENTCHUTE_TEST_ARGV_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var argv []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		argv, err = readProcessArgv(cmd.Process.Pid)
		if err == nil && len(argv) == 8 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv[len(argv)-3:], "\x00"); got != "--disable\x00memories\x00a prompt with spaces" {
		t.Fatalf("argv tail = %q", argv)
	}
}

func TestCodexFeatureArgs(t *testing.T) {
	for _, row := range []struct {
		args, want []string
	}{
		{args: []string{"--no-daemon", "-c", "model=o3", "--enable", "memories", "resume"}, want: []string{"-c", "model=o3", "--enable", "memories"}},
		{args: []string{"--config=a=b", "-cfeatures.x=1", "-c=y=2", "--disable=apps", "-p", "prof"}, want: []string{"--config=a=b", "-c", "features.x=1", "-c", "y=2", "--disable=apps"}},
		{args: []string{"--", "-c", "x=1"}, want: nil},
		{args: []string{"-c"}, want: nil},
	} {
		if got := codexFeatureArgs(row.args); strings.Join(got, "\x00") != strings.Join(row.want, "\x00") {
			t.Errorf("codexFeatureArgs(%q) = %q, want %q", row.args, got, row.want)
		}
	}
}

// ---------- codex lifecycle hooks from a foreign thread ----------

// codexHookFeed makes the codex lifecycle hooks read one input with cwd
// ("" omits the field).
func codexHookFeed(t *testing.T, event, cwd string) {
	t.Helper()
	in := map[string]any{"hook_event_name": event, "session_id": "s", "turn_id": "t"}
	if cwd != "" {
		in["cwd"] = cwd
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hook.json")
	mustWrite(t, path, body)
	restore := codexHookStdin
	t.Cleanup(func() { codexHookStdin = restore })
	codexHookStdin = func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
}

// The memory thread's SessionStart, UserPromptSubmit and Stop reach the lane's
// hooks too. From a cwd outside the control repo each does nothing; from the
// lane's cwd, or with no cwd, each does its job (claude-code and grok reviews
// of #217: turn-end archived the lane's claimed mail, pending injected the
// unread count, boot registered).
func TestCodexLifecycleHooksIgnoreAThreadOutsideTheControlRepo(t *testing.T) {
	memories := filepath.Join(t.TempDir(), ".codex", "memories")
	if err := os.MkdirAll(memories, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{"memory thread", "lane", "no cwd", "memory thread in a pool at $HOME", "lane in a pool at $HOME", "memory thread under a custom CODEX_HOME"} {
		t.Run(where, func(t *testing.T) {
			root, cfg := setupConsumeFixture(t)
			inPool := filepath.Join(root, ".codex", "memories")
			if err := os.MkdirAll(inPool, 0o755); err != nil {
				t.Fatal(err)
			}
			cwd := map[string]string{"memory thread": memories, "lane": root, "no cwd": "", "memory thread in a pool at $HOME": inPool, "lane in a pool at $HOME": root, "memory thread under a custom CODEX_HOME": inPool}[where]
			foreign := strings.HasPrefix(where, "memory thread")
			if strings.HasSuffix(where, "at $HOME") {
				t.Setenv("HOME", root)
				t.Setenv("CODEX_HOME", "")
			} else if strings.HasSuffix(where, "CODEX_HOME") {
				t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
			}
			withCwd(t, root, func() {
				clearGuardEnv(t)
				t.Setenv("AGENTCHUTE_RUNNER_PID", "")
				t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
				t.Setenv("AGENTCHUTE_CONTROL_REPO", root)
				t.Setenv("AGENTCHUTE_LOOP_DIR", cfg.LoopDir)

				// pending (UserPromptSubmit): unread mail → context, or nothing.
				if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "one"}); err != nil {
					t.Fatal(err)
				}
				codexHookFeed(t, "UserPromptSubmit", cwd)
				out, err := captureStdout(t, func() error {
					return cmdPending([]string{"--as", "bob", "--codex-hook", "UserPromptSubmit"})
				})
				if err != nil {
					t.Fatalf("pending: %v", err)
				}
				if foreign != (strings.TrimSpace(out) == "") {
					t.Fatalf("pending output = %q, want empty only for the memory thread", out)
				}

				// turn-end (Stop): the lane's claimed mail is archived, or untouched.
				if _, err := checkAs(t, "bob"); err != nil {
					t.Fatal(err)
				}
				codexHookFeed(t, "Stop", cwd)
				out, err = captureStdout(t, func() error {
					return cmdTurnEnd([]string{"--as", "bob", "--codex-hook", "Stop"})
				})
				if err != nil {
					t.Fatalf("turn-end: %v", err)
				}
				claimed, err := os.ReadDir(cfg.AgentClaimedDir("bob"))
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if foreign != (len(claimed) == 1) {
					t.Fatalf("claimed after Stop = %d, want the claim kept only for the memory thread", len(claimed))
				}
				if foreign && out != "" {
					t.Fatalf("turn-end printed %q for the memory thread", out)
				}

				// boot (SessionStart): the registration is written, or not.
				reg := cfg.AgentRegistrationPath("bob")
				if err := os.Remove(reg); err != nil {
					t.Fatal(err)
				}
				codexHookFeed(t, "SessionStart", cwd)
				out, err = captureStdout(t, func() error {
					return cmdBoot([]string{"--as", "bob", "--vendor", "test", "--codex-hook", "SessionStart"})
				})
				if err != nil && !errors.Is(err, errBlocked) {
					t.Fatalf("boot: %v", err)
				}
				_, statErr := os.Stat(reg)
				if foreign != os.IsNotExist(statErr) {
					t.Fatalf("registration after SessionStart: stat err = %v, want it written except for the memory thread", statErr)
				}
				if foreign && out != "" {
					t.Fatalf("boot printed %q for the memory thread", out)
				}
			})
		})
	}
}
