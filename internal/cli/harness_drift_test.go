package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// ---------- H5: Guarded follows the binary ----------

func TestWrapperGuardedForBinary(t *testing.T) {
	gemini, _ := wrapperSpecForName("gemini")
	grok, _ := wrapperSpecForName("grok")
	codex, _ := wrapperSpecForName("codex")
	rows := []struct {
		spec   wrapperSpec
		binary string
		want   bool
		reason bool
	}{
		{gemini, "/usr/local/bin/gemini", true, false},
		{gemini, "gemini-cli", true, false},
		{gemini, "/Users/alex/.local/bin/agy", false, true},
		{gemini, "agy", false, true},
		{grok, "/Users/alex/.grok/bin/grok", false, false},
		{codex, "codex", true, false},
	}
	for _, row := range rows {
		got, reason := row.spec.guardedFor(row.binary)
		if got != row.want || (reason != "") != row.reason {
			t.Fatalf("%s + %s: guarded=%v reason=%q, want guarded=%v reason=%v", row.spec.Key, row.binary, got, reason, row.want, row.reason)
		}
		if reason != "" && (!strings.Contains(reason, "agy") || !strings.Contains(reason, "UNGUARDED")) {
			t.Fatalf("reason must name the binary and say unguarded: %q", reason)
		}
	}
}

// `ac serve gemini` resolving to agy: the child gets no AGENTCHUTE_GUARD,
// stderr says which binary and why, and the gemini template is NOT written.
func TestServeAgyLaunchesUnguardedWithoutGeminiTemplate(t *testing.T) {
	root := setupShortRunFixture(t)
	envPath := filepath.Join(root, "child-env.txt")
	wrapper := filepath.Join(root, "agy")
	mustWrite(t, wrapper, []byte("#!/bin/sh\nenv | grep -E '^(AGENTCHUTE_GUARD|AGENTCHUTE_AGENT_ID)=' > "+shellQuote(envPath)+"\n"))
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	var serveErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			serveErr = cmdServe([]string{"--as", "gemini-cli", "--control-repo", root, "--loop-dir", filepath.Join(root, ".agentchute", "loop"), "--interval", "5", "--idle-grace", "100ms", "--", wrapper})
		})
	})
	if serveErr != nil {
		t.Fatalf("serve: %v", serveErr)
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("wrapper did not run: %v", err)
	}
	if strings.Contains(string(got), "AGENTCHUTE_GUARD=") {
		t.Fatalf("agy child is armed:\n%s", got)
	}
	if !strings.Contains(string(got), "AGENTCHUTE_AGENT_ID=gemini-cli") {
		t.Fatalf("child env missing the id:\n%s", got)
	}
	if !strings.Contains(stderr, "resolved to agy") || !strings.Contains(stderr, "UNGUARDED") {
		t.Fatalf("stderr did not say which binary and why:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("gemini template written for agy: stat err = %v", err)
	}
}

// ---------- H3a: codex hook trust ----------

const trustFixtureHooks = `{"hooks":{"SessionStart":[{"matcher":"startup|resume|clear","hooks":[{"type":"command","command":"a","statusMessage":"s"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"b"},{"type":"command","command":"c"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"d","timeout":30}]}],"Stop":[{"hooks":[{"type":"command","command":"e","timeout":30}]}]}}`

// writeCodexTrustConfig writes a codex config.toml carrying the given trust
// tables: key -> trusted_hash value ("" writes the header with no hash).
func writeCodexTrustConfig(t *testing.T, dir string, tables map[string]string) string {
	t.Helper()
	keys := make([]string, 0, len(tables))
	for k := range tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("model = \"x\"\n\n[hooks.state]\n\n")
	for _, k := range keys {
		b.WriteString("[hooks.state.\"" + k + "\"]\n")
		if tables[k] != "" {
			b.WriteString("trusted_hash = \"" + tables[k] + "\"\n")
		}
		b.WriteString("\n")
	}
	path := filepath.Join(dir, "config.toml")
	mustWrite(t, path, []byte(b.String()))
	return path
}

// realTrust returns key -> the hash codex would record for hooksPath as it is
// on disk now.
func realTrust(t *testing.T, hooksPath string) map[string]string {
	t.Helper()
	entries, err := codexExpectedTrust(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		out[e.Key] = e.Hash
	}
	return out
}

// The reconstructed hash is codex's: sha256 over compact key-sorted JSON of
// {event_name, matcher?, hooks:[{type,command,async:false,timeout|600,statusMessage?}]}.
// Pinned by two values computed from that definition so a drift in the
// serializer (key order, defaults) is caught here, not in a live lane.
func TestCodexHookTrustHashShape(t *testing.T) {
	matcher := "startup|resume|clear"
	got := codexHookTrustHash("SessionStart", &matcher, map[string]any{"type": "command", "command": "a", "statusMessage": "s"})
	want := "sha256:" + sha256Hex(`{"event_name":"session_start","hooks":[{"async":false,"command":"a","statusMessage":"s","timeout":600,"type":"command"}],"matcher":"startup|resume|clear"}`)
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	got = codexHookTrustHash("PreToolUse", nil, map[string]any{"type": "command", "command": "d", "timeout": float64(30)})
	want = "sha256:" + sha256Hex(`{"event_name":"pre_tool_use","hooks":[{"async":false,"command":"d","timeout":30,"type":"command"}]}`)
	if got != want {
		t.Fatalf("hash (no matcher, explicit timeout) = %s, want %s", got, want)
	}
}

func TestCodexHookTrustMissing(t *testing.T) {
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, "hooks-under-test.json")
	mustWrite(t, hooksPath, []byte(trustFixtureHooks))
	all := realTrust(t, hooksPath)
	key := func(pos string) string { return hooksPath + ":" + pos }
	clone := func() map[string]string {
		m := map[string]string{}
		for k, v := range all {
			m[k] = v
		}
		return m
	}

	if missing, err := codexHookTrustMissing(writeCodexTrustConfig(t, dir, all), hooksPath); err != nil || len(missing) != 0 {
		t.Fatalf("all trusted with the real hashes: missing=%v err=%v", missing, err)
	}

	noStop := clone()
	delete(noStop, key("stop:0:0"))
	missing, err := codexHookTrustMissing(writeCodexTrustConfig(t, dir, noStop), hooksPath)
	if err != nil || strings.Join(missing, ",") != "stop:0:0" {
		t.Fatalf("stop untrusted: missing=%v err=%v", missing, err)
	}

	// A table header with no trusted_hash is not trust (codex gate on #213).
	headerOnly := clone()
	headerOnly[key("pre_tool_use:0:0")] = ""
	missing, err = codexHookTrustMissing(writeCodexTrustConfig(t, dir, headerOnly), hooksPath)
	if err != nil || strings.Join(missing, ",") != "pre_tool_use:0:0" {
		t.Fatalf("header without hash: missing=%v err=%v", missing, err)
	}

	// A malformed hash is not trust either.
	malformed := clone()
	malformed[key("user_prompt_submit:0:1")] = "sha256:0"
	missing, err = codexHookTrustMissing(writeCodexTrustConfig(t, dir, malformed), hooksPath)
	if err != nil || strings.Join(missing, ",") != "user_prompt_submit:0:1" {
		t.Fatalf("malformed hash: missing=%v err=%v", missing, err)
	}

	// A changed command at a trusted position: the recorded hash is for the
	// OLD content, so the position reads as modified (codex skips it too).
	cfg := writeCodexTrustConfig(t, dir, all)
	mustWrite(t, hooksPath, []byte(strings.Replace(trustFixtureHooks, `"command":"e"`, `"command":"e --changed"`, 1)))
	missing, err = codexHookTrustMissing(cfg, hooksPath)
	if err != nil || strings.Join(missing, ",") != "stop:0:0 (modified)" {
		t.Fatalf("changed command at a trusted position: missing=%v err=%v", missing, err)
	}
	mustWrite(t, hooksPath, []byte(trustFixtureHooks))

	// A different root's entries do not count for this file.
	foreign := map[string]string{}
	for k, v := range all {
		foreign[strings.Replace(k, hooksPath, "/elsewhere/hooks-elsewhere.json", 1)] = v
	}
	missing, err = codexHookTrustMissing(writeCodexTrustConfig(t, dir, foreign), hooksPath)
	if err != nil || len(missing) != len(all) {
		t.Fatalf("foreign root: missing=%v err=%v", missing, err)
	}

	if _, err := codexHookTrustMissing(filepath.Join(dir, "absent.toml"), hooksPath); err == nil {
		t.Fatal("unreadable config must be an error (treated as untrusted)")
	}
	if got := codexHookEventKey("UserPromptSubmit"); got != "user_prompt_submit" {
		t.Fatalf("event key = %q", got)
	}
}

// serve arms the guard for codex only when every hook position is trusted
// with the hash of the installed content.
func TestServeCodexGuardFollowsHookTrust(t *testing.T) {
	for _, row := range []struct {
		name      string
		mutate    func(map[string]string, string)
		wantGuard bool
		wantWarn  string
	}{
		{"all trusted: guarded", func(map[string]string, string) {}, true, ""},
		{"one position untrusted: unguarded with warning", func(m map[string]string, k string) { delete(m, k) }, false, "pre_tool_use:0:0"},
		{"header without hash: unguarded", func(m map[string]string, k string) { m[k] = "" }, false, "pre_tool_use:0:0"},
		{"stale hash (modified content): unguarded", func(m map[string]string, k string) { m[k] = "sha256:" + strings.Repeat("ab", 32) }, false, "pre_tool_use:0:0 (modified)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := setupShortRunFixture(t)
			envPath := filepath.Join(root, "child-env.txt")
			wrapper := filepath.Join(root, "codex")
			mustWrite(t, wrapper, []byte("#!/bin/sh\ncase \"${1-}\" in --help) exit 0;; esac\nenv | grep -E '^AGENTCHUTE_GUARD=' > "+shellQuote(envPath)+"; true\n"))
			if err := os.Chmod(wrapper, 0o755); err != nil {
				t.Fatal(err)
			}
			// Install the shipped template first (serve's refresh is then a
			// no-op) and record the hashes codex would for that content.
			installed := filepath.Join(root, ".codex", "hooks.json")
			template, err := fs.ReadFile(hooksFS, "examples/hooks/codex/.codex/hooks.json")
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, installed, template)
			tables := realTrust(t, installed)
			row.mutate(tables, installed+":pre_tool_use:0:0")
			codexHome := t.TempDir()
			writeCodexTrustConfig(t, codexHome, tables)
			t.Setenv("CODEX_HOME", codexHome)

			var serveErr error
			stderr := captureStderr(t, func() {
				withCwd(t, root, func() {
					serveErr = cmdServe([]string{"--as", "codex", "--control-repo", root, "--loop-dir", filepath.Join(root, ".agentchute", "loop"), "--interval", "5", "--idle-grace", "100ms", "--", wrapper})
				})
			})
			if serveErr != nil {
				t.Fatalf("serve: %v", serveErr)
			}
			got, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatalf("wrapper did not run: %v", err)
			}
			if armed := strings.Contains(string(got), "AGENTCHUTE_GUARD=1"); armed != row.wantGuard {
				t.Fatalf("guard armed=%v, want %v; env:\n%s\nstderr:\n%s", armed, row.wantGuard, got, stderr)
			}
			if row.wantWarn != "" && (!strings.Contains(stderr, "not trusted every agentchute hook") || !strings.Contains(stderr, row.wantWarn)) {
				t.Fatalf("stderr did not name %q:\n%s", row.wantWarn, stderr)
			}
			if row.wantGuard && strings.Contains(stderr, "UNGUARDED") {
				t.Fatalf("spurious warning:\n%s", stderr)
			}
		})
	}
}

// The expected keys for the shipped codex template, by position: the test
// that fails if someone inserts a hook in the middle (see hookWrappers).
func TestCodexTemplateTrustPositions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shipped.json")
	data, err := fs.ReadFile(hooksFS, "examples/hooks/codex/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, data)
	keys, err := codexExpectedTrustKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range keys {
		keys[i] = strings.TrimPrefix(keys[i], path+":")
	}
	want := "pre_tool_use:0:0,session_start:0:0,stop:0:0,user_prompt_submit:0:0,user_prompt_submit:0:1"
	if got := strings.Join(keys, ","); got != want {
		t.Fatalf("template trust positions changed:\n got %s\nwant %s\n(append new hooks at the end of their group; existing positions are codex trust keys)", got, want)
	}
}

// ---------- H3c: apply_patch matches targets only ----------

func TestGuardApplyPatchMatchesTargetsOnly(t *testing.T) {
	const bodyMentionsAck = `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-old\n+run ` + "`agentchute ack`" + ` and then rm -rf nothing, curl nowhere\n*** End Patch\n"}}`
	const targetsHookFile = `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: .codex/hooks.json\n@@\n-a\n+b\n*** End Patch\n"}}`
	const movesOntoHookFile = `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: notes.md\n*** Move to: .claude/settings.json\n@@\n-a\n+b\n*** End Patch\n"}}`
	const bashStillMatchesBody = `{"tool_name":"Bash","tool_input":{"command":"echo hi && agentchute ack"}}`

	if cmd := parseGuardToolCommand([]byte(bodyMentionsAck)); guardCommandDenied(cmd) {
		t.Fatalf("a patch whose diff body mentions deny words was denied; command text = %q", cmd)
	}
	if cmd := parseGuardToolCommand([]byte(targetsHookFile)); !guardCommandDenied(cmd) {
		t.Fatalf("a patch targeting a hook config file was allowed; command text = %q", cmd)
	}
	if cmd := parseGuardToolCommand([]byte(movesOntoHookFile)); !guardCommandDenied(cmd) {
		t.Fatalf("a patch moving onto a hook config file was allowed; command text = %q", cmd)
	}
	if cmd := parseGuardToolCommand([]byte(bashStillMatchesBody)); !guardCommandDenied(cmd) {
		t.Fatalf("Bash command text is still matched whole; command text = %q", cmd)
	}
	if got := guardApplyPatchTargets("*** Add File: a/b.md\n*** Delete File: c.txt\nbody *** Update File: not-a-header\n"); strings.Join(got, ",") != "a/b.md,c.txt" {
		t.Fatalf("targets = %v", got)
	}

	// Targets are cleaned before the hook-path predicate (codex gate on #213):
	// dot segments, parent segments, absolute forms and a multi-file patch
	// with one bad target are all denied; look-alikes are not.
	patch := func(lines ...string) string {
		return `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n` + strings.Join(lines, `\n`) + `\n@@\n-a\n+b\n*** End Patch\n"}}`
	}
	for _, row := range []struct {
		name string
		body string
		deny bool
	}{
		{"dot segment", patch("*** Update File: .codex/./" + "hooks.json"), true},
		{"parent segment", patch("*** Update File: .claude/sub/../" + "settings.json"), true},
		{"agy dot segment", patch("*** Add File: .agents/./" + "hooks.json"), true},
		{"move onto via parent segment", patch("*** Update File: notes.md", "*** Move to: .codex/sub/../"+"hooks.json"), true},
		{"absolute path", patch("*** Delete File: /repo/" + ".codex/hooks.json"), true},
		{"multi-file with one bad target", patch("*** Update File: README.md", "*** Update File: docs/x.md", "*** Update File: sub/"+".claude/settings.json"), true},
		{"look-alike name", patch("*** Update File: docs/codex-hooks.json"), false},
		{"look-alike suffix", patch("*** Update File: notes/settings.json"), false},
	} {
		cmd := parseGuardToolCommand([]byte(row.body))
		if got := guardCommandDenied(cmd); got != row.deny {
			t.Fatalf("%s: denied=%v, want %v; command text = %q", row.name, got, row.deny, cmd)
		}
	}
}

// ---------- H4: grok is launched hookless ----------

func TestRunnerChildEnvDisablesGrokClaudeHookCompat(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "1") // an operator's shell default must not win
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}
	grok := runnerChildEnv(cfg, runnerOptions{AgentID: "grok", Vendor: "xai", WrapperArgs: []string{"/Users/alex/.grok/bin/grok"}}, "tok")
	if !has(grok, "GROK_CLAUDE_HOOKS_ENABLED=0") || has(grok, "GROK_CLAUDE_HOOKS_ENABLED=1") {
		t.Fatalf("grok child env does not disable Claude hook compat: %v", grok)
	}
	claude := runnerChildEnv(cfg, runnerOptions{AgentID: "claude-code", Vendor: "anthropic", Guarded: true, WrapperArgs: []string{"claude"}}, "tok")
	if has(claude, "GROK_CLAUDE_HOOKS_ENABLED=0") {
		t.Fatalf("non-grok child env carries the grok override: %v", claude)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------- codex gate finding 3: doctor agrees with the unguarded agy launch ----------

func TestDoctorSkipsHookPresenceForUnguardedAgy(t *testing.T) {
	geminiTemplate := filepath.Join(".gemini", "settings.json")
	for _, row := range []struct {
		name     string
		template string // "" = absent, otherwise written as the installed gemini file
	}{
		{"template absent", ""},
		{"template present but stale", "{\"hooks\":{}}"},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg := newDoctorCfg(t)
			if row.template != "" {
				mustWrite(t, filepath.Join(cfg.ControlRepo, geminiTemplate), []byte(row.template))
			}
			bin := t.TempDir()
			mustWrite(t, filepath.Join(bin, "agy"), []byte("#!/bin/sh\nexit 0\n"))
			if err := os.Chmod(filepath.Join(bin, "agy"), 0o755); err != nil {
				t.Fatal(err)
			}
			r := runDoctorChecks(cfg, "gemini-cli", doctorOptions{Now: time.Now().UTC(), PathEnv: bin})
			c := findCheck(t, r, "hook_file_presence")
			if c.Severity != severitySkip || !strings.Contains(c.Message, "agy") || !strings.Contains(c.Message, "unguarded") {
				t.Fatalf("hook_file_presence with agy resolved = %s: %s", c.Severity, c.Message)
			}
		})
	}
	// With a real gemini binary on PATH the template is still required.
	cfg := newDoctorCfg(t)
	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "gemini"), []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(filepath.Join(bin, "gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runDoctorChecks(cfg, "gemini-cli", doctorOptions{Now: time.Now().UTC(), PathEnv: bin})
	if c := findCheck(t, r, "hook_file_presence"); c.Severity != severityBlocker {
		t.Fatalf("gemini binary without its template must still block: %s %s", c.Severity, c.Message)
	}
}
