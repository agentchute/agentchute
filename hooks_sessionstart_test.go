package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// At SessionStart, boot already registers/refreshes the agent (it runs
// performRegister even in --context-only / --codex-hook mode). A redundant
// self-check in the same SessionStart block doubles the registration write and
// is the engine of the contextual-identity duplicate race: two writes resolve
// the same base before either is visible. The fix removes self-check from
// SessionStart (boot owns it there) while keeping it on the per-turn hook
// (UserPromptSubmit / BeforeAgent), where no boot runs and last_seen/wake still
// need active reconciliation.
//
// This pins the contract against the embedded templates `hooks install` ships.
func TestHookTemplatesSessionStartHasNoRedundantSelfCheck(t *testing.T) {
	cases := []struct {
		wrapper      string
		path         string
		sessionStart string // event key holding the startup commands
		turnEvent    string // event key holding the per-turn commands
	}{
		{"claude-code", "examples/hooks/claude-code/.claude/settings.json", "SessionStart", "UserPromptSubmit"},
		{"codex", "examples/hooks/codex/.codex/hooks.json", "SessionStart", "UserPromptSubmit"},
		{"gemini-cli", "examples/hooks/gemini/.gemini/settings.json", "SessionStart", "BeforeAgent"},
	}

	for _, c := range cases {
		data, err := hooksFS.ReadFile(c.path)
		if err != nil {
			t.Errorf("%s: read embedded template: %v", c.wrapper, err)
			continue
		}
		cmds := hookCommandsForEvent(t, data, c.sessionStart)
		if len(cmds) == 0 {
			t.Errorf("%s: no %s commands found", c.wrapper, c.sessionStart)
			continue
		}
		for _, cmd := range cmds {
			if strings.Contains(cmd, "self-check") {
				t.Errorf("%s: %s still runs self-check (%q); boot already registers at session start — remove to avoid the duplicate-identity race",
					c.wrapper, c.sessionStart, cmd)
			}
		}
		var sawBoot bool
		for _, cmd := range cmds {
			if strings.Contains(cmd, " boot ") || strings.HasSuffix(cmd, " boot") {
				sawBoot = true
			}
		}
		if !sawBoot {
			t.Errorf("%s: %s no longer runs boot; it must own session-start registration", c.wrapper, c.sessionStart)
		}

		// self-check must remain on the per-turn hook, where no boot runs.
		turn := hookCommandsForEvent(t, data, c.turnEvent)
		var sawTurnSelfCheck bool
		for _, cmd := range turn {
			if strings.Contains(cmd, "self-check") {
				sawTurnSelfCheck = true
			}
		}
		if !sawTurnSelfCheck {
			t.Errorf("%s: %s dropped self-check; per-turn wake reconciliation still needs it", c.wrapper, c.turnEvent)
		}
	}
}

func TestHookTemplatesRunSelfCheckBeforeFinishGate(t *testing.T) {
	cases := []struct {
		wrapper string
		path    string
		event   string
	}{
		{"claude-code", "examples/hooks/claude-code/.claude/settings.json", "Stop"},
		{"codex", "examples/hooks/codex/.codex/hooks.json", "Stop"},
		{"gemini-cli", "examples/hooks/gemini/.gemini/settings.json", "BeforeAgent"},
	}

	for _, c := range cases {
		data, err := hooksFS.ReadFile(c.path)
		if err != nil {
			t.Errorf("%s: read embedded template: %v", c.wrapper, err)
			continue
		}
		cmds := hookCommandsForEvent(t, data, c.event)
		selfCheckAt, gateAt := -1, -1
		for i, cmd := range cmds {
			switch {
			case strings.Contains(cmd, "self-check"):
				if selfCheckAt == -1 {
					selfCheckAt = i
				}
			case strings.Contains(cmd, " gate "):
				if gateAt == -1 {
					gateAt = i
				}
			}
		}
		if gateAt == -1 {
			t.Errorf("%s: %s has no finish gate", c.wrapper, c.event)
			continue
		}
		if selfCheckAt == -1 {
			t.Errorf("%s: %s has no self-check before finish gate", c.wrapper, c.event)
			continue
		}
		if selfCheckAt > gateAt {
			t.Errorf("%s: %s self-check must run before gate; commands=%v", c.wrapper, c.event, cmds)
		}
	}
}

func TestGeminiHookTemplateUsesBeforeAgentJSONGate(t *testing.T) {
	data, err := hooksFS.ReadFile("examples/hooks/gemini/.gemini/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	cmds := hookCommandsForEvent(t, data, "BeforeAgent")
	var gateCmd string
	for _, cmd := range cmds {
		if strings.Contains(cmd, " gate ") {
			gateCmd = cmd
			break
		}
	}
	if gateCmd == "" {
		t.Fatal("Gemini BeforeAgent hook has no gate command")
	}
	for _, want := range []string{"--before finish", "--json"} {
		if !strings.Contains(gateCmd, want) {
			t.Fatalf("Gemini gate command missing %q: %q", want, gateCmd)
		}
	}
	for _, stale := range []string{"--gemini-hook", "AfterAgent"} {
		if strings.Contains(gateCmd, stale) {
			t.Fatalf("Gemini shipped hook must use BeforeAgent + --json, not %s: %q", stale, gateCmd)
		}
	}
}

// hookCommandsForEvent extracts every hook command string for one event key
// from a wrapper hook config. The claude/codex/gemini configs share the shape
// {"hooks": {"<event>": [ {"hooks": [ {"command": "..."} ]} ]}}.
func hookCommandsForEvent(t *testing.T, data []byte, event string) []string {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse hook config: %v", err)
	}
	var cmds []string
	for _, group := range doc.Hooks[event] {
		for _, h := range group.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	return cmds
}
