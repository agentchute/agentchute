package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// claudeHooksDisabled reports where Claude Code's `disableAllHooks` resolves
// to true for a session in root, if it does. "Claude Code reads the value
// left after settings precedence applies" (code.claude.com/docs/en/hooks), so
// the first source that sets the key decides: the launch line's --settings (an
// inline JSON object or a file), then <root>/.claude/settings.local.json, then
// <root>/.claude/settings.json, then the user settings ($CLAUDE_CONFIG_DIR, or
// ~/.claude). Managed policy settings are not read.
//
// A merged settings file can keep agentchute's hooks byte-for-byte current
// while this one key switches all of them off: serve would arm a latch no
// Stop hook clears, and the guard would never run (opus-xhigh S3/S5 review
// of the merge).
func claudeHooksDisabled(root string, wrapperArgs []string) (where string, disabled bool) {
	type source struct {
		name string
		read func() ([]byte, error)
	}
	var sources []source
	if v, ok := claudeSettingsFlag(wrapperArgs); ok {
		if strings.HasPrefix(strings.TrimSpace(v), "{") {
			sources = append(sources, source{"the launch line's --settings", func() ([]byte, error) { return []byte(v), nil }})
		} else {
			sources = append(sources, source{v + " (--settings)", func() ([]byte, error) { return os.ReadFile(v) }})
		}
	}
	files := []string{
		filepath.Join(root, ".claude", "settings.local.json"),
		filepath.Join(root, ".claude", "settings.json"),
	}
	if user := claudeUserSettingsPath(); user != "" {
		files = append(files, user)
	}
	for _, path := range files {
		path := path
		sources = append(sources, source{path, func() ([]byte, error) { return os.ReadFile(path) }})
	}
	for _, src := range sources {
		data, err := src.read()
		if err != nil {
			continue
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(data, &top) != nil {
			continue
		}
		raw, ok := top["disableAllHooks"]
		if !ok {
			continue
		}
		var v bool
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		return src.name, v
	}
	return "", false
}

// claudeSettingsFlag returns the value of a `--settings` option on a claude
// launch line (`--settings v` or `--settings=v`), before any `--`.
func claudeSettingsFlag(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "--settings" && i+1 < len(args) {
			return args[i+1], true
		}
		if v, ok := strings.CutPrefix(a, "--settings="); ok {
			return v, true
		}
	}
	return "", false
}

// claudeUserSettingsPath is Claude Code's user settings file:
// $CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json.
func claudeUserSettingsPath() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}
