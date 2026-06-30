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
		{"serve alias for run", []string{"serve", "claude"}, "claude", nil, []string{}},
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
