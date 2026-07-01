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
		{"serve by key", []string{"serve", "codex"}, "codex", nil, []string{}},
		{"serve by alias agy", []string{"serve", "agy"}, "gemini", nil, []string{}},
		{"serve with global flag (spaced)", []string{"--as", "x", "serve", "codex"}, "codex", []string{"--as", "x"}, []string{}},
		{"serve with global flag (=)", []string{"--as=x", "serve", "gemini", "--flag"}, "gemini", []string{"--as=x"}, []string{"--flag"}},
		{"serve with control-repo + loop-dir globals", []string{"--control-repo", "R", "--loop-dir", "D", "serve", "codex"}, "codex", []string{"--control-repo", "R", "--loop-dir", "D"}, []string{}},
		// `run` is the deprecated alias — it must launch identically to `serve`.
		{"run alias by key", []string{"run", "codex"}, "codex", nil, []string{}},
		{"run alias by wrapper alias agy", []string{"run", "agy"}, "gemini", nil, []string{}},
		{"run alias with global flag", []string{"--as", "x", "run", "codex"}, "codex", []string{"--as", "x"}, []string{}},
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
		{"bare wrapper requires serve", []string{"claude"}, "use `ac serve claude`"},
		{"bare wrapper alias requires serve", []string{"agy"}, "use `ac serve agy`"},
		{"unknown subcommand", []string{"frobnicate"}, "unknown subcommand"},
		{"serve without wrapper", []string{"serve"}, "ac serve <wrapper>"},
		{"serve unknown wrapper", []string{"serve", "nope"}, "unknown wrapper"},
		{"run alias without wrapper", []string{"run"}, "ac serve <wrapper>"},
		{"run alias unknown wrapper", []string{"run", "nope"}, "unknown wrapper"},
		{"global flag then nothing", []string{"--as", "x"}, "expected a command"},
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
	// "serve" (and its deprecated alias "run") are both agentchute commands and the
	// dispatcher launch keyword; the dispatcher must treat `ac serve <wrapper>` /
	// `ac run <wrapper>` as a launch, not as cmdServe routed through the command path.
	for _, verb := range []string{"serve", "run"} {
		plan, err := parseDispatch([]string{verb, "grok"})
		if err != nil {
			t.Fatalf("ac %s grok: %v", verb, err)
		}
		if plan.Kind != dispatchRun || plan.Wrapper.Key != "grok" {
			t.Fatalf("ac %s grok => kind=%v key=%q; want launch of grok", verb, plan.Kind, plan.Wrapper.Key)
		}
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
	for _, name := range []string{"check", "send", "ack", "doctor", "setup", "serve", "run", "status", "gate", "boot"} {
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
		"/bin/agentchute", "serve", "--vendor", "openai", "--as", "reviewer",
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
