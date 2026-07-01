package main

import (
	"strings"
	"testing"
)

func TestParseDispatch_Commands(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCmd  string
		wantArgs []string
	}{
		{"plain command", []string{"check"}, "check", []string{}},
		{"command with args", []string{"check", "--as", "x"}, "check", []string{"--as", "x"}},
		{"global flag forwarded to command", []string{"--as", "x", "check"}, "check", []string{"--as", "x"}},
		{"doctor", []string{"doctor", "--json"}, "doctor", []string{"doctor-tail-marker"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := parseDispatch(tc.args)
			if err != nil {
				t.Fatalf("parseDispatch(%v) error: %v", tc.args, err)
			}
			if plan.Kind != dispatchCommand {
				t.Fatalf("kind = %v, want dispatchCommand", plan.Kind)
			}
			if plan.Command != tc.wantCmd {
				t.Fatalf("command = %q, want %q", plan.Command, tc.wantCmd)
			}
		})
	}
}

func TestParseDispatch_GlobalFlagsForwarded(t *testing.T) {
	plan, err := parseDispatch([]string{"--as", "reviewer", "check", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != dispatchCommand || plan.Command != "check" {
		t.Fatalf("got kind=%v cmd=%q", plan.Kind, plan.Command)
	}
	want := []string{"--as", "reviewer", "--json"}
	if strings.Join(plan.CommandArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("CommandArgs = %v, want %v", plan.CommandArgs, want)
	}
}

func TestParseDispatch_Run(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantKey    string
		wantGlobal []string
		wantWArgs  []string
	}{
		{"run by key", []string{"run", "codex"}, "codex", nil, []string{}},
		{"run by alias agy", []string{"run", "agy"}, "gemini", nil, []string{}},
		{"run with global flag (spaced)", []string{"--as", "x", "run", "codex"}, "codex", []string{"--as", "x"}, []string{}},
		{"run with global flag (=)", []string{"--as=x", "run", "gemini", "--flag"}, "gemini", []string{"--as=x"}, []string{"--flag"}},
		{"run with control-repo + loop-dir globals", []string{"--control-repo", "R", "--loop-dir", "D", "run", "codex"}, "codex", []string{"--control-repo", "R", "--loop-dir", "D"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := parseDispatch(tc.args)
			if err != nil {
				t.Fatalf("parseDispatch(%v) error: %v", tc.args, err)
			}
			if plan.Kind != dispatchRun {
				t.Fatalf("kind = %v, want dispatchRun", plan.Kind)
			}
			if plan.Wrapper.Key != tc.wantKey {
				t.Fatalf("wrapper key = %q, want %q", plan.Wrapper.Key, tc.wantKey)
			}
			if strings.Join(plan.Global, " ") != strings.Join(tc.wantGlobal, " ") {
				t.Fatalf("global = %v, want %v", plan.Global, tc.wantGlobal)
			}
			if strings.Join(plan.WrapperArgs, " ") != strings.Join(tc.wantWArgs, " ") {
				t.Fatalf("wrapperArgs = %v, want %v", plan.WrapperArgs, tc.wantWArgs)
			}
		})
	}
}

func TestParseDispatch_Errors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"bare wrapper requires run", []string{"claude"}, "use `ac run claude`"},
		{"bare wrapper alias requires run", []string{"agy"}, "use `ac run agy`"},
		{"unknown subcommand", []string{"frobnicate"}, "unknown subcommand"},
		{"run without wrapper", []string{"run"}, "ac run <wrapper>"},
		{"run unknown wrapper", []string{"run", "nope"}, "unknown wrapper"},
		{"global flag then nothing", []string{"--as", "x"}, "expected a command"},
		{"serve is deferred, not a launch keyword", []string{"serve", "claude"}, "unknown subcommand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDispatch(tc.args)
			if err == nil {
				t.Fatalf("parseDispatch(%v) = nil error, want error containing %q", tc.args, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParseDispatch_CommandWinsOverWrapperName(t *testing.T) {
	// "run" is both an agentchute command and the dispatcher launch keyword;
	// the dispatcher must treat `ac run <wrapper>` as a launch, not cmdRun.
	plan, err := parseDispatch([]string{"run", "grok"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != dispatchRun || plan.Wrapper.Key != "grok" {
		t.Fatalf("ac run grok => kind=%v key=%q; want launch of grok", plan.Kind, plan.Wrapper.Key)
	}
}

func TestParseDispatch_Help(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"-h"}, {"help"}} {
		plan, err := parseDispatch(args)
		if err != nil {
			t.Fatalf("parseDispatch(%v) error: %v", args, err)
		}
		if plan.Kind != dispatchHelp {
			t.Fatalf("parseDispatch(%v) kind=%v, want help", args, plan.Kind)
		}
	}
}

func TestCommandHandlersCoverExpected(t *testing.T) {
	// Guard against accidental removal: the dispatcher's known-command set must
	// include the core operational commands.
	for _, name := range []string{"check", "send", "ack", "doctor", "setup", "run", "status", "gate", "boot"} {
		if commandHandlers[name] == nil {
			t.Errorf("commandHandlers missing %q", name)
		}
	}
}

func TestExtractGlobalFlag(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		flag      string
		wantVal   string
		wantRest  []string
		wantFound bool
	}{
		{"spaced form", []string{"--as", "x", "--control-repo", "R"}, "--control-repo", "R", []string{"--as", "x"}, true},
		{"equals form", []string{"--control-repo=R", "--as", "x"}, "--control-repo", "R", []string{"--as", "x"}, true},
		{"absent", []string{"--as", "x"}, "--loop-dir", "", []string{"--as", "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, rest, found := extractGlobalFlag(tc.args, tc.flag)
			if val != tc.wantVal || found != tc.wantFound {
				t.Fatalf("got (%q,%v), want (%q,%v)", val, found, tc.wantVal, tc.wantFound)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestSplitDispatchContext(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantShimDir string
		wantRest    []string
	}{
		{"spaced shim-dir then --", []string{"--shim-dir", "/d", "--", "run", "codex"}, "/d", []string{"run", "codex"}},
		{"equals shim-dir then --", []string{"--shim-dir=/d", "--", "check", "--as", "x"}, "/d", []string{"check", "--as", "x"}},
		{"shim-dir without trailing --", []string{"--shim-dir", "/d", "run", "codex"}, "/d", []string{"run", "codex"}},
		{"no shim-dir, leading --", []string{"--", "check"}, "", []string{"check"}},
		{"no shim-dir, no --", []string{"check", "--json"}, "", []string{"check", "--json"}},
		{"empty", []string{}, "", []string{}},
		{"user args preserved after --", []string{"--shim-dir", "/d", "--", "--as", "x", "run", "codex"}, "/d", []string{"--as", "x", "run", "codex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shimDir, rest := splitDispatchContext(tc.args)
			if shimDir != tc.wantShimDir {
				t.Fatalf("shimDir = %q, want %q", shimDir, tc.wantShimDir)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestSplitDispatchContext_ThenParseRoundTrip(t *testing.T) {
	// The exact argv the installed dispatcher script produces:
	// `agentchute dispatch --shim-dir <dir> -- <user args>`.
	shimDir, rest := splitDispatchContext([]string{"--shim-dir", "/shims", "--", "--as", "rev", "run", "codex"})
	if shimDir != "/shims" {
		t.Fatalf("shimDir = %q, want /shims", shimDir)
	}
	plan, err := parseDispatch(rest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != dispatchRun || plan.Wrapper.Key != "codex" {
		t.Fatalf("plan kind=%v key=%q, want run codex", plan.Kind, plan.Wrapper.Key)
	}
	if strings.Join(plan.Global, " ") != "--as rev" {
		t.Fatalf("global = %v, want [--as rev]", plan.Global)
	}
}

func TestBuildDispatchRunArgs_SingleAuthoritativePair(t *testing.T) {
	got := buildDispatchRunArgs("/bin/agentchute", "openai", []string{"--as", "reviewer"},
		"/repo", "/repo/.agentchute/loop", []string{"/usr/bin/codex", "resume"})
	want := []string{
		"/bin/agentchute", "run", "--vendor", "openai", "--as", "reviewer",
		"--control-repo", "/repo", "--loop-dir", "/repo/.agentchute/loop", "--shim-name", "ac", "--",
		"/usr/bin/codex", "resume",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("runArgs =\n  %v\nwant\n  %v", got, want)
	}
	// exactly one of each authoritative flag
	for _, f := range []string{"--control-repo", "--loop-dir", "--vendor"} {
		n := 0
		for _, a := range got {
			if a == f {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s appears %d times, want 1", f, n)
		}
	}
}
