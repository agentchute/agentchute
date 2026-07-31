package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// agentchuteBinPrefix is the exact prefix every hook-config command line uses
// to invoke the CLI (respecting an AGENTCHUTE_BIN override). A command not
// starting with this prefix is a wrapper-native invocation (e.g. a plain
// shell builtin) and is not an agentchute subcommand at all.
const agentchuteBinPrefix = "${AGENTCHUTE_BIN:-agentchute} "

// verbFromHookCommand extracts the agentchute subcommand a hook command
// string invokes, if any.
func verbFromHookCommand(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, agentchuteBinPrefix)
	if !ok {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// allHookCommands returns every "command" string across every event in a
// hook config document, regardless of event name (unlike
// hookCommandsForEvent, which is scoped to one named event).
func allHookCommands(t *testing.T, label string, data []byte) []string {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: parse hook config: %v", label, err)
	}
	var cmds []string
	for _, groups := range doc.Hooks {
		for _, group := range groups {
			for _, h := range group.Hooks {
				cmds = append(cmds, h.Command)
			}
		}
	}
	return cmds
}

// TestNoTrackedHookConfigInvokesARemovedSubcommand asserts, by parsing and
// checking against the live commandHandlers dispatch table (not by grep
// discipline), that every agentchute invocation in every hook config this
// repo ships or dogfoods names a real subcommand. codex PR #98 review,
// finding 3: this repo's own tracked .claude/settings.json still called
// `poller ensure` after the poller verb was deleted — the embedded
// examples/hooks templates were resynced, the repo's own live config was not.
// This test exists so the next deletion slice cannot repeat that miss.
func TestNoTrackedHookConfigInvokesARemovedSubcommand(t *testing.T) {
	type source struct {
		label string
		data  []byte
	}
	var sources []source

	for _, h := range hookWrappers {
		embedded, err := fs.ReadFile(hooksFS, h.Src)
		if err != nil {
			t.Fatalf("read embedded template %s: %v", h.Src, err)
		}
		sources = append(sources, source{label: "embedded:" + h.Src, data: embedded})

		tracked, err := os.ReadFile(h.Dest)
		if err != nil {
			if os.IsNotExist(err) {
				continue // this wrapper's config isn't dogfooded at the repo root
			}
			t.Fatalf("read tracked %s: %v", h.Dest, err)
		}
		sources = append(sources, source{label: "tracked:" + h.Dest, data: tracked})
	}

	for _, src := range sources {
		for _, cmd := range allHookCommands(t, src.label, src.data) {
			verb, ok := verbFromHookCommand(cmd)
			if !ok {
				continue
			}
			if _, known := commandHandlers[verb]; !known {
				t.Errorf("%s: invokes removed/unknown subcommand %q: %q", src.label, verb, cmd)
			}
		}
	}
}
