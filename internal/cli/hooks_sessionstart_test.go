package cli

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

// At SessionStart, boot already registers/refreshes the agent (it runs
// performRegister even in --context-only / --codex-hook mode). A redundant
// self-check in the same SessionStart block doubles the registration write and
// can race a second write for the same explicit identity. The fix removes
// self-check from
// SessionStart (boot owns it there) while keeping it on the per-turn hook,
// where no boot runs and last_seen still needs active reconciliation —
// claude/codex do this via a standalone `self-check` entry
// on UserPromptSubmit (untouched by A7/A8); gemini does it via `turn-end`'s
// step 0 (v2.5 plan A7/C24), since self-check folded into turn-end there —
// BeforeAgent has no separate per-turn event from its end-of-turn one.
//
// This pins the contract against the embedded templates `hooks install` ships.
func TestHookTemplatesSessionStartHasNoRedundantSelfCheck(t *testing.T) {
	cases := []struct {
		wrapper           string
		path              string
		sessionStart      string // event key holding the startup commands
		turnEvent         string // event key holding the per-turn commands
		turnSelfRepairCmd string // substring the per-turn event must run to reconcile last_seen
	}{
		{"claude-code", "examples/hooks/claude-code/.claude/settings.json", "SessionStart", "UserPromptSubmit", "self-check"},
		{"codex", "examples/hooks/codex/.codex/hooks.json", "SessionStart", "UserPromptSubmit", "self-check"},
		{"gemini-cli", "examples/hooks/gemini/.gemini/settings.json", "SessionStart", "BeforeAgent", "self-check"},
		{"agy", "examples/hooks/agy/.agents/hooks.json", "PreInvocation", "PreInvocation", "boot"},
	}

	for _, c := range cases {
		data, err := fs.ReadFile(hooksFS, c.path)
		if err != nil {
			t.Errorf("%s: read embedded template: %v", c.wrapper, err)
			continue
		}
		if strings.Contains(string(data), "--vendor") {
			t.Errorf("%s: hook template still passes --vendor; identity must come from AGENTCHUTE_AGENT_ID", c.wrapper)
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

		// The per-turn hook must still reconcile last_seen, whether via
		// a standalone self-check (claude/codex) or turn-end's step 0 (gemini).
		turn := hookCommandsForEvent(t, data, c.turnEvent)
		var sawSelfRepair bool
		for _, cmd := range turn {
			if strings.Contains(cmd, c.turnSelfRepairCmd) {
				sawSelfRepair = true
			}
		}
		if !sawSelfRepair {
			t.Errorf("%s: %s dropped %s; per-turn last_seen reconciliation still needs it", c.wrapper, c.turnEvent, c.turnSelfRepairCmd)
		}
	}
}

// TestGeminiHookTemplateUsesAfterAgentTurnEnd pins the H6 fix (opus-xhigh
// review 2026-10-08): Gemini CLI has an AfterAgent event
// (geminicli.com/docs/hooks/reference), so the end-of-turn commit runs
// THERE — `turn-end --gemini-hook AfterAgent`, silent on clear and
// `{"decision":"deny",...}` exit 0 on block — not on BeforeAgent, where it
// committed turn N's mail only when turn N+1's prompt arrived (never, if the
// inbox stayed empty) and exited 2 into a prompt-erasing abort. BeforeAgent
// keeps only self-check and the pending context emitter.
func TestGeminiHookTemplateUsesAfterAgentTurnEnd(t *testing.T) {
	data, err := fs.ReadFile(hooksFS, "examples/hooks/gemini/.gemini/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	after := hookCommandsForEvent(t, data, "AfterAgent")
	var turnEndCmd string
	for _, cmd := range after {
		if strings.Contains(cmd, "turn-end") {
			turnEndCmd = cmd
		}
	}
	if turnEndCmd == "" {
		t.Fatalf("Gemini AfterAgent hook has no turn-end command: %q", after)
	}
	if !strings.Contains(turnEndCmd, "--gemini-hook AfterAgent") || strings.Contains(turnEndCmd, "--json") {
		t.Fatalf("Gemini turn-end must use the AfterAgent envelope, not --json: %q", turnEndCmd)
	}
	before := hookCommandsForEvent(t, data, "BeforeAgent")
	var sawSelfCheck, sawPending bool
	for _, cmd := range before {
		if strings.Contains(cmd, "turn-end") || strings.Contains(cmd, " gate ") {
			t.Fatalf("Gemini BeforeAgent must not commit or gate (that is AfterAgent's job): %q", cmd)
		}
		sawSelfCheck = sawSelfCheck || strings.Contains(cmd, "self-check")
		sawPending = sawPending || strings.Contains(cmd, "pending --gemini-hook BeforeAgent")
	}
	if !sawSelfCheck || !sawPending {
		t.Fatalf("Gemini BeforeAgent must run self-check and the BeforeAgent pending emitter: %q", before)
	}
	start := hookCommandsForEvent(t, data, "SessionStart")
	if len(start) != 1 || !strings.Contains(start[0], "boot --gemini-hook SessionStart") {
		t.Fatalf("Gemini SessionStart must run boot with the SessionStart emitter: %q", start)
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
	if len(doc.Hooks) == 0 {
		// Antigravity's shape: {"<hookName>": {"enabled": …, "<Event>": [
		// {"command": …} | {"matcher": …, "hooks": [{"command": …}]} ]}}.
		var named map[string]map[string]json.RawMessage
		if err := json.Unmarshal(data, &named); err != nil {
			t.Fatalf("parse named hook config: %v", err)
		}
		for _, events := range named {
			raw, ok := events[event]
			if !ok {
				continue
			}
			var handlers []struct {
				Command string `json:"command"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			}
			if err := json.Unmarshal(raw, &handlers); err != nil {
				t.Fatalf("parse %s handlers: %v", event, err)
			}
			for _, h := range handlers {
				if h.Command != "" {
					cmds = append(cmds, h.Command)
				}
				for _, inner := range h.Hooks {
					cmds = append(cmds, inner.Command)
				}
			}
		}
	}
	return cmds
}
