package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// ---------- S3: settings files are merged, never replaced ----------

// projectClaudeSettings is a project's own .claude/settings.json: its own
// permissions (deny rules included), env, MCP server, model and a hook of its
// own — plus an out-of-date agentchute Stop hook and two allow rules earlier
// agentchute templates installed and this one retires.
const projectClaudeSettings = `{
  "env": {"FOO": "1"},
  "permissions": {
    "allow": ["Bash(make:*)", "Bash(agentchute send:*)", "Bash(go test:*)"],
    "deny": ["Bash(rm:*)", "Read(./.env)"],
    "defaultMode": "acceptEdits"
  },
  "mcpServers": {"x": {"command": "x-server"}},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "./scripts/lint-hook.sh"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "${AGENTCHUTE_BIN:-agentchute} turn-end"}]}]
  },
  "model": "opus"
}
`

func claudeHook(t *testing.T) hookWrapper {
	t.Helper()
	w, ok := hookWrapperByName("claude-code")
	if !ok {
		t.Fatal("claude-code hook wrapper missing")
	}
	return w
}

func decodeSettings(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("settings do not decode: %v\n%s", err, data)
	}
	return v
}

func stringList(v any) []string {
	var out []string
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}

func hasString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func TestHookMergeKeepsTheProjectsSettings(t *testing.T) {
	root := hooksRepoFixture(t)
	path := filepath.Join(root, ".claude", "settings.json")
	mustWrite(t, path, []byte(projectClaudeSettings))
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code", "--force"})
		})
		if err != nil {
			t.Fatalf("install --force: %v", err)
		}
		if !strings.Contains(out, "merged agentchute's entries") {
			t.Fatalf("install did not report a merge:\n%s", out)
		}
	})
	got := mustRead(t, path)
	v := decodeSettings(t, got)

	perms := v["permissions"].(map[string]any)
	deny := stringList(perms["deny"])
	for _, rule := range []string{"Bash(rm:*)", "Read(./.env)", "Bash(go * -exec *)", "Bash(git * --output=*)"} {
		if !hasString(deny, rule) {
			t.Errorf("deny lost %q: %q", rule, deny)
		}
	}
	allow := stringList(perms["allow"])
	if !hasString(allow, "Bash(make:*)") || !hasString(allow, "Bash(go test ./...)") {
		t.Errorf("allow lost the project's rule or missed the template's: %q", allow)
	}
	for _, retired := range []string{"Bash(agentchute send:*)", "Bash(go test:*)"} {
		if hasString(allow, retired) {
			t.Errorf("retired template rule %q kept: %q", retired, allow)
		}
	}
	if perms["defaultMode"] != "acceptEdits" || v["model"] != "opus" {
		t.Errorf("project scalars lost: defaultMode=%v model=%v", perms["defaultMode"], v["model"])
	}
	if env := v["env"].(map[string]any); env["FOO"] != "1" {
		t.Errorf("env lost: %v", env)
	}
	if _, ok := v["mcpServers"].(map[string]any)["x"]; !ok {
		t.Errorf("mcpServers lost: %v", v["mcpServers"])
	}
	if !strings.Contains(string(got), "./scripts/lint-hook.sh") {
		t.Errorf("the project's own hook was dropped:\n%s", got)
	}
	// Document order is the project's: a reviewer diffing the file sees only
	// agentchute's changes.
	order := []string{`"env"`, `"permissions"`, `"mcpServers"`, `"hooks"`, `"model"`}
	last := -1
	for _, key := range order {
		i := strings.Index(string(got), key)
		if i <= last {
			t.Fatalf("top-level key order changed at %s:\n%s", key, got)
		}
		last = i
	}
	plan, err := planHookFile(claudeHook(t), root)
	if err != nil || plan.State != hookFileCurrent {
		t.Fatalf("after the merge the plan is %v (%v), want current", plan.State, err)
	}
	if bak := mustRead(t, mustOneHookBackup(t, path)); string(bak) != projectClaudeSettings {
		t.Fatalf("backup is not the project's original file:\n%s", bak)
	}

	// Idempotent: a second run changes nothing and keeps no second backup.
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code", "--force"})
		})
		if err != nil || !strings.Contains(out, "already current") {
			t.Fatalf("second install: err=%v out=%s", err, out)
		}
	})
	mustOneHookBackup(t, path)
}

// setup's compatibility pass merges the same way: project rules survive.
func TestSetupResyncMergesIntoProjectSettings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".claude", "settings.json")
	mustWrite(t, path, []byte(projectClaudeSettings))
	refreshed, err := refreshHookCompatibility(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 1 || refreshed[0] != "claude-code" {
		t.Fatalf("refreshed = %v", refreshed)
	}
	deny := stringList(decodeSettings(t, mustRead(t, path))["permissions"].(map[string]any)["deny"])
	if !hasString(deny, "Bash(rm:*)") || !hasString(deny, "Read(./.env)") {
		t.Fatalf("setup dropped the project's deny rules: %q", deny)
	}
}

func TestHookMergeGeminiSettingsKeepsTheProjectsKeys(t *testing.T) {
	root := hooksRepoFixture(t)
	path := filepath.Join(root, ".gemini", "settings.json")
	mustWrite(t, path, []byte(`{"theme": "dark", "mcpServers": {"y": {"command": "y"}}, "hooks": {"BeforeAgent": [{"matcher": "*", "hooks": [{"type": "command", "command": "agentchute poller ensure"}]}]}}`))
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "gemini-cli", "--force"})
		}); err != nil {
			t.Fatal(err)
		}
	})
	got := mustRead(t, path)
	v := decodeSettings(t, got)
	if v["theme"] != "dark" || v["mcpServers"] == nil {
		t.Fatalf("project keys lost:\n%s", got)
	}
	if strings.Contains(string(got), "poller ensure") || !strings.Contains(string(got), "turn-end --gemini-hook AfterAgent") {
		t.Fatalf("agentchute's gemini hooks were not replaced by the template's:\n%s", got)
	}
}

// A settings file agentchute cannot read is never overwritten.
func TestHookMergeNeverOverwritesAnUnreadableSettingsFile(t *testing.T) {
	for _, row := range []struct{ name, content string }{
		{"invalid JSON", `{"permissions": {"deny": ["Bash(rm:*)"]`},
		{"duplicate key", `{"model": "a", "model": "b"}`},
		{"top level not an object", `["Bash(rm:*)"]`},
		{"hooks not an object", `{"hooks": []}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := hooksRepoFixture(t)
			path := filepath.Join(root, ".claude", "settings.json")
			mustWrite(t, path, []byte(row.content))
			withCwd(t, root, func() {
				_, err := captureStdout(t, func() error {
					return cmdHooks([]string{"install", "--wrapper", "claude-code", "--force"})
				})
				if err == nil || !strings.Contains(err.Error(), "never overwrites") {
					t.Fatalf("install --force over an unreadable file: err = %v", err)
				}
			})
			if got := mustRead(t, path); string(got) != row.content {
				t.Fatalf("file changed:\n%s", got)
			}
			mustNoHookBackup(t, path)
		})
	}
}

func TestHookMergeKeepsTheFileMode(t *testing.T) {
	root := hooksRepoFixture(t)
	path := filepath.Join(root, ".claude", "settings.json")
	mustWrite(t, path, []byte(projectClaudeSettings))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code", "--force"})
		}); err != nil {
			t.Fatal(err)
		}
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want the project's 0644", info.Mode().Perm())
	}
}

// Backups are never overwritten: two in the same second get distinct names.
func TestHookBackupsNeverOverwriteEachOther(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "settings.json")
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	a, err := writeHookBackup(dest, []byte("one"), now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := writeHookBackup(dest, []byte("two"), now)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasSuffix(a, "20261008T120000Z") || !strings.HasSuffix(b, "20261008T120000Z-2") {
		t.Fatalf("backups = %q, %q", a, b)
	}
	if string(mustRead(t, a)) != "one" || string(mustRead(t, b)) != "two" {
		t.Fatal("a backup was overwritten")
	}
}

// serve never rewrites an existing settings file. A current one — the
// project's keys plus agentchute's part matching — is accepted as is; an
// unreadable one is refused; on a remote lane whose control repo is only its
// working directory (no AGENTCHUTE.md), nothing is written at all.
func TestServeHookPreparation(t *testing.T) {
	merged := func(t *testing.T, root string) []byte {
		t.Helper()
		tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := mergeSettingsHookFile([]byte(projectClaudeSettings), tmpl)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, row := range []struct {
		name       string
		spec       bool // AGENTCHUTE.md present
		content    func(t *testing.T, root string) []byte
		wantErr    string // substring; "" = no error
		wantNote   bool
		wantCreate bool
	}{
		{name: "missing file, real control repo: created", spec: true, wantCreate: true},
		{name: "merged current project file: accepted untouched", spec: true, content: merged},
		{name: "stale file: refused with the repair command", spec: true, content: func(*testing.T, string) []byte { return []byte(projectClaudeSettings) }, wantErr: "agentchute setup"},
		{name: "unreadable file: refused", spec: true, content: func(*testing.T, string) []byte { return []byte("{") }, wantErr: "never rewrites"},
		{name: "cwd fallback, missing file: nothing written, unguarded", wantNote: true},
		{name: "cwd fallback, stale file: untouched, unguarded", content: func(*testing.T, string) []byte { return []byte(projectClaudeSettings) }, wantNote: true},
		{name: "cwd fallback, current file: guarded", content: merged},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			if row.spec {
				mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
			}
			path := filepath.Join(root, ".claude", "settings.json")
			var before []byte
			if row.content != nil {
				before = row.content(t, root)
				mustWrite(t, path, before)
			}
			note, err := prepareServeHook(root, "claude-code")
			if row.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), row.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, row.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (note != "") != row.wantNote || (row.wantNote && !strings.Contains(note, "UNGUARDED")) {
				t.Fatalf("unguarded note = %q, want present=%v", note, row.wantNote)
			}
			got, rerr := os.ReadFile(path)
			switch {
			case row.wantCreate:
				if rerr != nil {
					t.Fatalf("hook file not created: %v", rerr)
				}
			case before == nil:
				if !os.IsNotExist(rerr) {
					t.Fatalf("serve wrote %s where it must write nothing", path)
				}
			default:
				if string(got) != string(before) {
					t.Fatalf("serve rewrote an existing settings file:\n%s", got)
				}
			}
			mustNoHookBackup(t, path)
		})
	}
}

// ---------- disableAllHooks ----------

func TestClaudeHooksDisabledFollowsSettingsPrecedence(t *testing.T) {
	user := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", user)
	for _, row := range []struct {
		name                 string
		user, project, local string
		args                 []string
		wantOff              bool
		wantWhere            string
	}{
		{name: "nothing set"},
		{name: "project", project: `{"disableAllHooks": true}`, wantOff: true, wantWhere: "settings.json"},
		{name: "local outranks project", project: `{"disableAllHooks": true}`, local: `{"disableAllHooks": false}`, wantWhere: "settings.local.json"},
		{name: "local alone", local: `{"disableAllHooks": true}`, wantOff: true, wantWhere: "settings.local.json"},
		{name: "user, when the project is silent", user: `{"disableAllHooks": true}`, wantOff: true, wantWhere: user},
		{name: "project false outranks user true", user: `{"disableAllHooks": true}`, project: `{"disableAllHooks": false}`},
		{name: "--settings inline outranks the files", project: `{"disableAllHooks": false}`, args: []string{"--model", "x", "--settings", `{"disableAllHooks": true}`}, wantOff: true, wantWhere: "--settings"},
		{name: "--settings after -- is prompt text", args: []string{"--", "--settings", `{"disableAllHooks": true}`}},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, content string) {
				if content == "" {
					_ = os.Remove(path)
					return
				}
				mustWrite(t, path, []byte(content))
			}
			write(filepath.Join(user, "settings.json"), row.user)
			write(filepath.Join(root, ".claude", "settings.json"), row.project)
			write(filepath.Join(root, ".claude", "settings.local.json"), row.local)
			where, off := claudeHooksDisabled(root, row.args)
			if off != row.wantOff || (row.wantWhere != "" && !strings.Contains(where, row.wantWhere)) {
				t.Fatalf("claudeHooksDisabled = (%q, %v), want off=%v from %q", where, off, row.wantOff, row.wantWhere)
			}
		})
	}
}

// serve launches a claude lane UNGUARDED when its hooks are switched off: a
// latch armed then would be one no Stop hook clears.
func TestServeLaunchesClaudeUnguardedWhenHooksAreDisabled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := setupShortRunFixture(t)
	envPath := filepath.Join(root, "guard-env")
	wrapper := filepath.Join(root, "claude")
	mustWrite(t, wrapper, []byte("#!/bin/sh\nprintf '%s' \"${AGENTCHUTE_GUARD-unset}\" > "+shellQuote(envPath)+"\n"))
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteCanonicalHook(t, root, "claude-code")
	mustWrite(t, filepath.Join(root, ".claude", "settings.local.json"), []byte(`{"disableAllHooks": true}`))
	var stderr string
	withCwd(t, root, func() {
		var err error
		_, stderr, err = captureStdoutStderr(t, func() error {
			return cmdServe([]string{
				"--as", "claude-code",
				"--control-repo", root,
				"--loop-dir", filepath.Join(root, ".agentchute", "loop"),
				"--interval", "5",
				"--idle-grace", "100ms",
				"--", wrapper,
			})
		})
		if err != nil {
			t.Fatalf("cmdServe: %v", err)
		}
	})
	if got := string(mustRead(t, envPath)); got != "unset" {
		t.Fatalf("AGENTCHUTE_GUARD in the wrapper = %q, want unset (unguarded)", got)
	}
	if !strings.Contains(stderr, "disableAllHooks") || !strings.Contains(stderr, "UNGUARDED") {
		t.Fatalf("serve did not say why it launched unguarded:\n%s", stderr)
	}
}

// ---------- S4: the Claude template's permission rules ----------

// The template's rules are pinned: no prefix rule that runs an arbitrary
// program (`go test -exec`, `go build -toolexec`, `go vet -vettool`), no
// `git --output` writes, and no auto-approved `agentchute send`, whose
// --body-file reads any file and delivers it (opus-xhigh S4).
func TestClaudeTemplatePermissionsArePinned(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	perms := decodeSettings(t, tmpl)["permissions"].(map[string]any)
	wantAllow := []string{
		"Bash(agentchute check:*)", "Bash(agentchute ack:*)", "Bash(agentchute gate:*)",
		"Bash(agentchute turn-end:*)", "Bash(agentchute boot:*)", "Bash(agentchute doctor:*)",
		"Bash(agentchute status:*)", "Bash(agentchute pending:*)", "Bash(agentchute identity:*)",
		"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
		"Bash(git fetch:*)", "Bash(git add:*)", "Bash(git commit:*)", "Bash(git worktree:*)",
		"Bash(go test ./...)", "Bash(go build ./...)", "Bash(go vet ./...)", "Bash(gofmt:*)",
		"Bash(gh pr view:*)", "Bash(gh pr list:*)", "Bash(gh pr diff:*)", "Bash(gh pr checks:*)",
		"Bash(gh run view:*)", "Bash(gh run list:*)", "Bash(gh run watch:*)",
	}
	if got := stringList(perms["allow"]); strings.Join(got, "\n") != strings.Join(wantAllow, "\n") {
		t.Fatalf("allow rules drifted:\n%s", strings.Join(got, "\n"))
	}
	wantDeny := []string{
		"Bash(go * -exec *)",
		"Bash(go * -exec=*)",
		"Bash(go * --exec *)",
		"Bash(go * --exec=*)",
		"Bash(go * -toolexec *)",
		"Bash(go * -toolexec=*)",
		"Bash(go * --toolexec *)",
		"Bash(go * --toolexec=*)",
		"Bash(go * -vettool *)",
		"Bash(go * -vettool=*)",
		"Bash(go * --vettool *)",
		"Bash(go * --vettool=*)",
		"Bash(git * --output *)",
		"Bash(git * --output=*)",
		"Bash(git * --upload-pack*)",
		"Bash(git * --receive-pack*)",
	}
	if got := stringList(perms["deny"]); strings.Join(got, "\n") != strings.Join(wantDeny, "\n") {
		t.Fatalf("deny rules drifted:\n%s", strings.Join(got, "\n"))
	}
	repoCopy := mustRead(t, filepath.Join(repoRootForTests(), ".claude", "settings.json"))
	if string(repoCopy) != string(tmpl) {
		t.Fatal("the repo's own .claude/settings.json differs from examples/hooks/claude-code")
	}
}

// ---------- S5: every tool, judged by command text AND write target ----------

func TestGuardJudgesEveryToolByCommandAndTarget(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	_ = root
	if err := loop.SetGuardLatch(cfg, "bob", "tok-s5"); err != nil {
		t.Fatal(err)
	}
	home := "/home/u"
	rows := []struct {
		name string
		body string
		deny bool
	}{
		{"Bash rm -rf", `{"tool_name":"Bash","tool_input":{"command":"rm -rf x"}}`, true},
		{"PowerShell writes a hook file", `{"tool_name":"PowerShell","tool_input":{"command":"Set-Content .claude/settings.json '{}'"}}`, true},
		{"Monitor runs ack", `{"tool_name":"Monitor","tool_input":{"command":"agentchute ack --as bob"}}`, true},
		{"Write to settings.local.json", `{"tool_name":"Write","tool_input":{"file_path":"/repo/.claude/settings.local.json","content":"{\"disableAllHooks\": true}"}}`, true},
		{"Write elsewhere: content is not a command", `{"tool_name":"Write","tool_input":{"file_path":"/repo/notes.md","content":"rm -rf x; agentchute ack; curl y"}}`, false},
		{"Edit the project settings", `{"tool_name":"Edit","tool_input":{"file_path":"/repo/.claude/settings.json","old_string":"a","new_string":"b"}}`, true},
		{"Edit prose that names a hook file", `{"tool_name":"Edit","tool_input":{"file_path":"/repo/docs.md","old_string":"x","new_string":"edit .claude/settings.json by hand"}}`, false},
		{"NotebookEdit onto a hook file", `{"tool_name":"NotebookEdit","tool_input":{"notebook_path":"/repo/.codex/hooks.json","new_source":"x"}}`, true},
		{"MCP file tool with its own path key", `{"tool_name":"mcp__fs__write_file","tool_input":{"path":"` + home + `/.codex/config.toml","content":"x"}}`, true},
		{"Antigravity write_to_file", `{"toolCall":{"name":"write_to_file","args":{"TargetFile":"/repo/.agents/hooks.json","CodeContent":"{}"}}}`, true},
		{"Antigravity run_command ack", `{"toolCall":{"name":"run_command","args":{"CommandLine":"agentchute ack","Cwd":"/repo"}}}`, true},
		{"Antigravity run_command elsewhere", `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls","Cwd":"/repo/.agents"}}}`, false},
		{"grok camelCase", `{"toolName":"bash","toolInput":{"command":"agentchute turn-end"}}`, true},
		{"codex write_stdin types ack into an open shell", `{"tool_name":"write_stdin","tool_input":{"session_id":3,"chars":"agentchute ack\n"}}`, true},
		{"codex exec_command with a workdir", `{"tool_name":"functions.exec_command","tool_input":{"cmd":"ls","workdir":"/repo"}}`, false},
		{"Gemini write_file onto its settings", `{"tool_name":"write_file","tool_input":{"file_path":"/repo/.gemini/settings.json","content":"{}"}}`, true},
		{"a file in grok's hooks directory", `{"tool_name":"Write","tool_input":{"file_path":"` + home + `/.grok/hooks/stop.json","content":"{}"}}`, true},
		{"apply_patch body mentions ack", `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: docs.md\n+run agentchute ack\n*** End Patch\n"}}`, false},
		{"apply_patch targets a hook file", `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Add File: .claude/settings.local.json\n+{}\n*** End Patch\n"}}`, true},
		{"Read is never a write", `{"tool_name":"Read","tool_input":{"file_path":"/repo/notes.md"}}`, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			d := evaluateGuardDecisionFor(cfg, "bob", "tok-s5", parseGuardToolUse([]byte(row.body)))
			if d.Allowed == row.deny {
				t.Fatalf("allowed = %v, want deny=%v for %s", d.Allowed, row.deny, row.body)
			}
		})
	}
}

// The template routes every Claude tool through the guard: MCP servers and
// any future file tool included (codex, Gemini and Antigravity templates
// already do).
func TestClaudeTemplateGuardsEveryTool(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(tmpl, &v); err != nil {
		t.Fatal(err)
	}
	groups := v.Hooks["PreToolUse"]
	if len(groups) != 1 || groups[0].Matcher != "*" || !strings.Contains(groups[0].Hooks[0].Command, "guard --pre-tool-use") {
		t.Fatalf("PreToolUse = %+v, want one group matching every tool (\"*\") running the guard", groups)
	}
}

// ---------- C3: a repeated, unchanged Stop block is let through ----------

// stopInput makes turn-end's hook input source return one JSON body.
func stopInput(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stop.json")
	mustWrite(t, path, []byte(body))
	restore := hookStdin
	t.Cleanup(func() { hookStdin = restore })
	hookStdin = func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
}

func TestTurnEndLetsARepeatedUnchangedBlockThrough(t *testing.T) {
	for _, mode := range []string{"claude", "codex"} {
		t.Run(mode, func(t *testing.T) {
			root, cfg := setupConsumeFixture(t)
			withCwd(t, root, func() {
				clearGuardEnv(t)
				t.Setenv("AGENTCHUTE_RUNNER_PID", "")
				deliver := func(body string) {
					if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", body}); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"--as", "bob", "--json"}
				if mode == "codex" {
					args = []string{"--as", "bob", "--codex-hook", "Stop"}
				}
				stop := func(active bool) (stdout, stderr string, err error) {
					// The lane's cwd is in the input, as codex sends it: the
					// foreign-thread check and stop_hook_active read ONE input.
					stopInput(t, fmt.Sprintf(`{"hook_event_name":"Stop","cwd":%q,"session_id":"s1","stop_hook_active":%v}`, root, active))
					return captureStdoutStderr(t, func() error { return cmdTurnEnd(args) })
				}
				blocks := func(stdout string, err error) bool {
					if mode == "codex" {
						return err == nil && strings.Contains(stdout, `"decision":"block"`)
					}
					return err == errBlocked
				}

				deliver("one")
				out, _, err := stop(false)
				if !blocks(out, err) {
					t.Fatalf("first Stop with unread mail did not block: err=%v out=%s", err, out)
				}
				out, stderr, err := stop(true)
				if err != nil || blocks(out, err) || !strings.Contains(out, `"systemMessage"`) || !strings.Contains(out, "finish gate still blocked: ") {
					t.Fatalf("repeated Stop with unchanged reasons: err=%v out=%s stderr=%s", err, out, stderr)
				}
				deliver("two")
				out, _, err = stop(true)
				if !blocks(out, err) {
					t.Fatalf("a Stop whose reasons CHANGED (more mail) was let through: err=%v out=%s", err, out)
				}
				out, _, err = stop(true)
				if err != nil || blocks(out, err) {
					t.Fatalf("the changed reasons, repeated unchanged, still block: err=%v out=%s", err, out)
				}
				if _, err := os.Stat(turnEndLastBlockFile(cfg, "bob")); err != nil {
					t.Fatalf("no record of the last block: %v", err)
				}
			})
		})
	}
}

// A clear gate forgets the last block; a text-mode (hand-run) turn-end never reads stdin.
func TestTurnEndForgetsTheLastBlockWhenClearAndHandRunsReadNoStdin(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_RUNNER_PID", "")
		writeTurnEndLastBlock(cfg, "bob", "agentchute gate --before finish: stale")
		restore := hookStdin
		t.Cleanup(func() { hookStdin = restore })
		hookStdin = func() *os.File {
			t.Error("a text-mode turn-end read hook stdin")
			return os.Stdin
		}
		// Text mode is the hand-run form: it never reads stdin.
		if _, err := captureStdout(t, func() error { return cmdTurnEnd([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("clear turn-end: %v", err)
		}
		if _, err := os.Stat(turnEndLastBlockFile(cfg, "bob")); !os.IsNotExist(err) {
			t.Fatalf("a clear gate kept the last-block record: %v", err)
		}
	})
}

// ---------- C4: a planted AGENTCHUTE_* never reaches a test ----------

// The test binary re-executed with a live pool's variables planted: TestMain
// strips them before the probe below runs, so it sees none.
func TestTestMainStripsAPlantedPool(t *testing.T) {
	if os.Getenv("ACTEST_C4_PROBE") == "1" {
		return // the probe child's own copy of this test
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run", "^TestC4ProbeSeesNoAgentchuteVariable$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"ACTEST_C4_PROBE=1",
		"AGENTCHUTE_CONTROL_REPO=/Users/someone/live-pool",
		"AGENTCHUTE_LOOP_DIR=/Users/someone/live-pool/.agentchute/loop",
		"AGENTCHUTE_AGENT_ID=codex",
		"AGENTCHUTE_SERVE_TOKEN=live-token",
	)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("probe child: err=%v\n%s", err, out)
	}
}

func TestC4ProbeSeesNoAgentchuteVariable(t *testing.T) {
	if os.Getenv("ACTEST_C4_PROBE") != "1" {
		t.Skip("probe child only")
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AGENTCHUTE_") {
			t.Fatalf("a planted variable reached a test: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

// stop_hook_active is also true when ANOTHER Stop hook blocked. A block
// recorded in an earlier turn (self-check ran since) or in another session
// must never let a first block through.
func TestTurnEndRepeatedBlockIsScopedToTheTurnAndSession(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		t.Setenv("AGENTCHUTE_RUNNER_PID", "")
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "one"}); err != nil {
			t.Fatal(err)
		}
		stop := func(session string, active bool) error {
			stopInput(t, fmt.Sprintf(`{"hook_event_name":"Stop","session_id":%q,"stop_hook_active":%v}`, session, active))
			_, _, err := captureStdoutStderr(t, func() error { return cmdTurnEnd([]string{"--as", "bob", "--json"}) })
			return err
		}
		if err := stop("s1", false); err != errBlocked {
			t.Fatalf("first block: %v", err)
		}
		if err := stop("s2", true); err != errBlocked {
			t.Fatalf("another session's active Stop was let through: %v", err)
		}
		if err := stop("s2", false); err != errBlocked {
			t.Fatal("s2 first block")
		}
		if _, err := captureStdout(t, func() error { return cmdSelfCheck([]string{"--as", "bob", "--vendor", "test", "--quiet"}) }); err != nil {
			t.Fatalf("self-check: %v", err)
		}
		if _, err := os.Stat(turnEndLastBlockFile(cfg, "bob")); !os.IsNotExist(err) {
			t.Fatalf("self-check (a new turn) kept the last-block record: %v", err)
		}
		if err := stop("s2", true); err != errBlocked {
			t.Fatalf("after a new turn, an active Stop (another hook's block) was let through: %v", err)
		}
	})
}

// A command key's value is command text whatever its shape.
func TestGuardReadsACommandArray(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "tok-arr"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"tool_name":"shell","tool_input":{"command":["bash","-lc","agentchute ack --as bob"]}}`,
		`{"tool_name":"shell","tool_input":{"command":{"argv":["rm","-rf","x"]}}}`,
	} {
		if d := evaluateGuardDecisionFor(cfg, "bob", "tok-arr", parseGuardToolUse([]byte(body))); d.Allowed {
			t.Errorf("allowed: %s", body)
		}
	}
}

// ---------- C5: a spooled send is retried while latched ----------

// A local send that fails before delivery preserves its body in the sender's
// spool and prints `--body-file <spool>`. That exact command passes the latched
// guard, reads the spool (the one state/ directory a body may come from), and
// delivers. Another agent's spool and the sender's serve.claim stay refused.
func TestSpooledSendIsRetriedWhileLatched(t *testing.T) {
	root, cfg := setupSendFixture(t)
	body := "line one\nline two\n"
	var sendErr error
	withCwd(t, root, func() {
		sendErr = withRecipientRemovedAfterPreflight(t, cfg, "codex", func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body", body})
		})
	})
	if sendErr == nil {
		t.Fatal("expected a pre-delivery failure")
	}
	spool := onlySendSpool(t, cfg, "claude-code")
	retry := sendErr.Error()[strings.Index(sendErr.Error(), "retry with: ")+len("retry with: "):]
	retry = strings.TrimSpace(strings.SplitN(retry, "\n", 2)[0])
	if !strings.HasSuffix(retry, "--body-file "+shellQuote(canonicalTestPath(t, spool))) {
		t.Fatalf("retry line = %q, want the --body-file form", retry)
	}

	// The printed command, as a Bash tool call, while this session is latched.
	if err := loop.SetGuardLatch(cfg, "claude-code", "tok-c5"); err != nil {
		t.Fatal(err)
	}
	use := parseGuardToolUse([]byte(`{"tool_name":"Bash","tool_input":{"command":` + strconvQuote(retry) + `}}`))
	if d := evaluateGuardDecisionFor(cfg, "claude-code", "tok-c5", use); !d.Allowed {
		t.Fatalf("the printed retry is denied while latched: %q", retry)
	}

	if err := os.MkdirAll(cfg.AgentInboxDir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", spool}); err != nil {
			t.Fatalf("--body-file retry from the own spool: %v", err)
		}
	})
	if got := readMostRecentInboxMessage(t, cfg, "codex"); !strings.HasSuffix(got, "\n\n"+body) {
		t.Fatalf("retried message lost the body:\n%s", got)
	}

	// Still refused: someone else's spool, and the sender's own serve.claim.
	otherSpool := filepath.Join(cfg.AgentStateDir("codex"), "spool", "x.md")
	mustWrite(t, otherSpool, []byte("theirs"))
	claim := filepath.Join(cfg.AgentStateDir("claude-code"), "serve.claim")
	mustWrite(t, claim, []byte(`{"serve_token":"secret"}`))
	link := filepath.Join(cfg.AgentStateDir("claude-code"), "spool", "claim-link.md")
	if err := os.Symlink(claim, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{otherSpool, claim, link} {
		if _, err := readSendBodyFile(cfg, path, "claude-code"); err == nil || !strings.Contains(err.Error(), "state/ tree") {
			t.Errorf("--body-file %s was not refused: %v", path, err)
		}
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// An input past the read limit is denied while latched, never truncated into
// a parse failure and allowed; unlatched it is allowed like everything else.
func TestGuardDeniesAnOversizedInputWhileLatched(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	big := `{"tool_name":"Write","tool_input":{"file_path":"/repo/.claude/settings.json","content":"` + strings.Repeat("x", guardMaxInputBytes) + `"}}`
	use := parseGuardToolUse([]byte(big))
	if !use.Oversize {
		t.Fatal("oversized input not flagged")
	}
	if err := loop.SetGuardLatch(cfg, "bob", "tok-big"); err != nil {
		t.Fatal(err)
	}
	if d := evaluateGuardDecisionFor(cfg, "bob", "tok-big", use); d.Allowed {
		t.Fatal("an oversized input was allowed while latched")
	}
	if d := evaluateGuardDecisionFor(cfg, "bob", "tok-other", use); !d.Allowed {
		t.Fatal("an oversized input was denied without this session's latch")
	}
}

// Every hook file agentchute installs is a guarded write target.
func TestEveryInstalledHookFileIsGuarded(t *testing.T) {
	for _, w := range hookWrappers {
		if !guardHookConfigPath("/repo/" + w.Dest) {
			t.Errorf("%s's hook file %s is not a guarded path", w.Name, w.Dest)
		}
	}
}

// Antigravity's toolCall args stay command text in full, as before.
func TestGuardKeepsReadingEveryAntigravityArgAsCommandText(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "tok-agy"); err != nil {
		t.Fatal(err)
	}
	body := `{"toolCall":{"name":"run_command","args":{"CommandLine":"sh run.sh","Extra":"rm -rf x"}}}`
	if d := evaluateGuardDecisionFor(cfg, "bob", "tok-agy", parseGuardToolUse([]byte(body))); d.Allowed {
		t.Fatal("a denied word in a non-CommandLine Antigravity arg is no longer seen")
	}
}

// A write target is judged by where it really lands: a symlinked directory or
// file leading to a hook config file is that file.
func TestGuardResolvesSymlinkedWriteTargets(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	if err := loop.SetGuardLatch(cfg, "bob", "tok-ln"); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, ".claude", "settings.json"), []byte("{}"))
	if err := os.Symlink(filepath.Join(repo, ".claude"), filepath.Join(repo, "cfg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, ".claude", "settings.json"), filepath.Join(repo, "notes.json")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		filepath.Join(repo, "cfg", "settings.json"),       // through a symlinked directory
		filepath.Join(repo, "cfg", "settings.local.json"), // not yet existing, through it
		filepath.Join(repo, "notes.json"),                 // a symlinked file
	} {
		body := `{"tool_name":"Write","tool_input":{"file_path":` + strconvQuote(target) + `,"content":"{}"}}`
		if d := evaluateGuardDecisionFor(cfg, "bob", "tok-ln", parseGuardToolUse([]byte(body))); d.Allowed {
			t.Errorf("write through %s was allowed while latched", target)
		}
	}
	plain := `{"tool_name":"Write","tool_input":{"file_path":` + strconvQuote(filepath.Join(repo, "README.md")) + `,"content":"{}"}}`
	if d := evaluateGuardDecisionFor(cfg, "bob", "tok-ln", parseGuardToolUse([]byte(plain))); !d.Allowed {
		t.Fatal("an ordinary write was denied")
	}
}

// The spool exemption covers the spool's own files only: a hard link to
// serve.claim placed in the spool, under a spool-shaped name, stays refused,
// and so does a file there under another name.
func TestSpoolExemptionRefusesAHardLinkToTheClaim(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	state := cfg.AgentStateDir("bob")
	claim := filepath.Join(state, "serve.claim")
	mustWrite(t, claim, []byte(`{"serve_token":"secret"}`))
	spool := filepath.Join(state, "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(spool, "20261008T120000000000Z_to-alice.md")
	if err := os.Link(claim, hard); err != nil {
		t.Skip("hard links unsupported here: " + err.Error())
	}
	other := filepath.Join(spool, "notes.md")
	mustWrite(t, other, []byte("x"))
	for _, path := range []string{hard, other} {
		if _, err := readSendBodyFile(cfg, path, "bob"); err == nil || !strings.Contains(err.Error(), "state/ tree") {
			t.Errorf("--body-file %s was not refused: %v", path, err)
		}
	}
	real := filepath.Join(spool, "20261008T120000000001Z_to-alice.md")
	mustWrite(t, real, []byte("preserved body"))
	if body, err := readSendBodyFile(cfg, real, "bob"); err != nil || body != "preserved body" {
		t.Fatalf("a real spool file: body=%q err=%v", body, err)
	}
}

// A path swapped between the check and the open (a symlink re-pointed at
// serve.claim) is refused: what is read must be what was checked.
func TestBodyFileSwappedAfterTheCheckIsRefused(t *testing.T) {
	_, cfg := setupConsumeFixture(t)
	claim := filepath.Join(cfg.AgentStateDir("bob"), "serve.claim")
	mustWrite(t, claim, []byte(`{"serve_token":"secret"}`))
	dir := t.TempDir()
	harmless := filepath.Join(dir, "body.md")
	mustWrite(t, harmless, []byte("hello"))
	link := filepath.Join(dir, "reply.md")
	if err := os.Symlink(harmless, link); err != nil {
		t.Fatal(err)
	}
	restore := afterSendBodyFileCheck
	t.Cleanup(func() { afterSendBodyFileCheck = restore })
	afterSendBodyFileCheck = func(string) {
		_ = os.Remove(link)
		if err := os.Symlink(claim, link); err != nil {
			t.Error(err)
		}
	}
	body, err := readSendBodyFile(cfg, link, "bob")
	if err == nil || strings.Contains(body, "secret") {
		t.Fatalf("a body file swapped after the check was read: body=%q err=%v", body, err)
	}
}

// claudeBashRuleMatches is Claude Code's Bash(...) rule match for the rule
// shapes the template uses: `*` matches any run of characters, the pattern
// matches the whole command, and a trailing `:*` is a prefix match.
func claudeBashRuleMatches(rule, command string) bool {
	pattern := strings.TrimSuffix(strings.TrimPrefix(rule, "Bash("), ")")
	if prefix, ok := strings.CutSuffix(pattern, ":*"); ok {
		return command == prefix || strings.HasPrefix(command, prefix+" ")
	}
	re := "^" + strings.Join(func() []string {
		parts := strings.Split(pattern, "*")
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		return parts
	}(), ".*") + "$"
	return regexp.MustCompile(re).MatchString(command)
}

// Deny rules apply even in bypass-permissions mode, so they must hit only
// the flag that runs a program, never an ordinary command a lane types
// (claude-code review of #226).
func TestClaudeTemplateDenyRulesHitOnlyTheDangerousFlags(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	deny := stringList(decodeSettings(t, tmpl)["permissions"].(map[string]any)["deny"])
	denied := func(cmd string) bool {
		for _, rule := range deny {
			if claudeBashRuleMatches(rule, cmd) {
				return true
			}
		}
		return false
	}
	for _, cmd := range []string{
		"go test -exec sh ./...",
		"go test ./... -exec=/tmp/x",
		"go test --exec 'sh -c id' ./...",
		"go build -toolexec 'sh -c id' ./...",
		"go build -toolexec=/tmp/x ./...",
		"go vet -vettool=/tmp/x ./...",
		"go vet --vettool /tmp/x ./...",
		"git diff --output=/tmp/x",
		"git log -p --output /tmp/x",
		"git fetch --upload-pack='sh -c id' origin",
		"git fetch origin --upload-pack=/tmp/x",
	} {
		if !denied(cmd) {
			t.Errorf("not denied: %s", cmd)
		}
	}
	for _, cmd := range []string{
		"go build ./cmd/exec-server",
		"go test ./... -run 'TestFoo-exec'",
		"go test -run TestExec ./...",
		"go test ./internal/cli -run TestGuard -v",
		"go vet ./...",
		"git format-patch --output-directory out HEAD~2",
		"git diff --output-indicator-new=+",
		"git log --oneline -5",
		"git fetch origin",
		"git status",
	} {
		if denied(cmd) {
			t.Errorf("an ordinary command is denied: %s", cmd)
		}
	}
	// The matcher itself: prefix rules and globs behave as documented.
	if !claudeBashRuleMatches("Bash(git status:*)", "git status --short") || claudeBashRuleMatches("Bash(git status:*)", "git statusx") {
		t.Fatal("prefix-rule model is wrong")
	}
}

// The Claude template's hook COMMANDS stay ones every installed binary since
// v1.6.2 understands: a repo pull that lands before the binary update must
// not break a running lane's hooks (claude-code review of #226).
func TestClaudeTemplateCommandsRunOnOlderBinaries(t *testing.T) {
	tmpl, err := fs.ReadFile(hooksFS, claudeHook(t).Src)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(tmpl, &v); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop"} {
		for _, g := range v.Hooks[event] {
			for _, h := range g.Hooks {
				got = append(got, h.Command)
			}
		}
	}
	want := []string{
		"${AGENTCHUTE_BIN:-agentchute} boot --context-only",
		"${AGENTCHUTE_BIN:-agentchute} self-check --quiet",
		"${AGENTCHUTE_BIN:-agentchute} pending --claude-hook UserPromptSubmit",
		"${AGENTCHUTE_BIN:-agentchute} guard --pre-tool-use",
		"${AGENTCHUTE_BIN:-agentchute} turn-end --json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hook commands changed:\n%s", strings.Join(got, "\n"))
	}
}
