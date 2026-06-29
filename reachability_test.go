package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// withFakeHerdrList installs a fake `herdr` that supports the resolve-by-NAME
// path WI-E2 requires:
//   - `agent list`  → the given name→pane bindings (the robust resolver).
//   - `agent rename <pane> <name>` → appends "<pane> <name>" to renameLog.
//   - `agent get <name>` → ALWAYS agent_not_found, so a test proves the resolver
//     used `agent list` (match on the `name` field), not `agent get <name>`
//     (which fails when the herdr handle differs from the bound name).
func withFakeHerdrList(t *testing.T, renameLog string, bindings map[string]string) {
	t.Helper()
	old := herdrProbeBinary
	var items []string
	for name, pane := range bindings {
		items = append(items, fmt.Sprintf(`{"name":"%s","pane_id":"%s"}`, name, pane))
	}
	listJSON := fmt.Sprintf(`{"result":{"agents":[%s]}}`, strings.Join(items, ","))
	path := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\n" +
		"sub=\"$2\"\n" +
		"case \"$sub\" in\n" +
		"  list) printf '%s\\n' '" + listJSON + "' ; exit 0 ;;\n" +
		"  rename) printf '%s %s\\n' \"$3\" \"$4\" >> '" + renameLog + "' ; exit 0 ;;\n" +
		"  get) printf '{\"error\":{\"code\":\"agent_not_found\"}}\\n' ; exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrProbeBinary = path
	t.Cleanup(func() { herdrProbeBinary = old })
}

func withFakeHerdrWakeByName(t *testing.T, bindings map[string]string) string {
	t.Helper()
	old := herdrProbeBinary
	var items []string
	for name, pane := range bindings {
		items = append(items, fmt.Sprintf(`{"name":"%s","pane_id":"%s"}`, name, pane))
	}
	listJSON := fmt.Sprintf(`{"result":{"agents":[%s]}}`, strings.Join(items, ","))
	dir := t.TempDir()
	path := filepath.Join(dir, "herdr")
	logPath := filepath.Join(dir, "herdr.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\t' \"$@\" >> " + shellQuote(logPath) + "\n" +
		"printf '\\n' >> " + shellQuote(logPath) + "\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = list ]; then printf '%s\\n' " + shellQuote(listJSON) + "; exit 0; fi\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = get ]; then printf '{\"error\":{\"code\":\"agent_not_found\"}}\\n'; exit 0; fi\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = send ]; then exit 0; fi\n" +
		"if [ \"$1\" = pane ] && [ \"$2\" = send-keys ]; then exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrProbeBinary = path
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { herdrProbeBinary = old })
	return logPath
}

func reachTestCfg(t *testing.T, root string) *loop.Config {
	t.Helper()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
	if err := loop.EnsurePrivateDir(cfg.AgentsDir()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeHerdrReg(t *testing.T, cfg *loop.Config, agentID, target string) {
	t.Helper()
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "examplecorp",
		ControlRepo: cfg.ControlRepo,
		Host:        localHostname(),
		WakeMethod:  "herdr",
		WakeTarget:  target,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}
}

func renameLogContents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// A stale herdr binding (name maps to a DIFFERENT pane) plus HERDR_PANE_ID
// context must self-repair: reprove re-binds our pane to the stable name and
// records ReachableAt — breaking the herdr circular deadlock without an inbound
// wake.
func TestReproveAndRebind_HerdrRebindWithPaneContext(t *testing.T) {
	root := t.TempDir()
	cfg := reachTestCfg(t, root)
	renameLog := filepath.Join(t.TempDir(), "rename.log")
	// The stable name currently maps to a DIFFERENT pane (stale binding).
	withFakeHerdrList(t, renameLog, map[string]string{"claude-code-agentchute": "w9:pOther"})
	setupHerdrEnv(t, "w3:p7") // our pane is w3:p7

	writeHerdrReg(t, cfg, "claude-code-agentchute", "claude-code-agentchute")

	rebound, err := reproveAndRebindOwnWake(cfg, "claude-code-agentchute")
	if err != nil {
		t.Fatalf("reproveAndRebindOwnWake err = %v", err)
	}
	if !rebound {
		t.Fatal("rebound = false, want true (stale binding + pane context should re-bind)")
	}
	if got := renameLogContents(t, renameLog); !strings.Contains(got, "w3:p7 claude-code-agentchute") {
		t.Fatalf("rename log = %q, want a rebind of our pane to the stable name", got)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("claude-code-agentchute"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.ReachableAt == nil {
		t.Fatal("ReachableAt not written after a successful rebind")
	}
	if reg.ReachabilityMethod != "herdr" || reg.ReachabilityTarget != "claude-code-agentchute" {
		t.Fatalf("reachability endpoint = %s/%s, want herdr/claude-code-agentchute", reg.ReachabilityMethod, reg.ReachabilityTarget)
	}
	if reg.ReachabilityError != "" {
		t.Fatalf("ReachabilityError = %q, want empty on success", reg.ReachabilityError)
	}
}

// Without binding context (no HERDR_PANE_ID), a context-less reprove may PROBE
// and record the reachability fact, but MUST NOT re-bind. A stale/unbound name
// records an error, leaves ReachableAt absent, and never calls `agent rename`.
func TestReprove_NoPaneContext_ProbesButNoRebind(t *testing.T) {
	root := t.TempDir()
	cfg := reachTestCfg(t, root)
	renameLog := filepath.Join(t.TempDir(), "rename.log")
	// The stable name is NOT bound to any live pane.
	withFakeHerdrList(t, renameLog, map[string]string{})
	// No HERDR_PANE_ID: a context-less probe.
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("TMUX_PANE", "")

	writeHerdrReg(t, cfg, "codex-agentchute", "codex-agentchute")

	rebound, err := reproveAndRebindOwnWake(cfg, "codex-agentchute")
	if err != nil {
		t.Fatalf("reproveAndRebindOwnWake err = %v", err)
	}
	if rebound {
		t.Fatal("rebound = true, want false (no pane context must NOT re-bind)")
	}
	if got := renameLogContents(t, renameLog); got != "" {
		t.Fatalf("rename log = %q, want empty (no rebind without pane context)", got)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex-agentchute"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.ReachableAt != nil {
		t.Fatal("ReachableAt written for an unreachable name with no rebind; want absent")
	}
	if reg.ReachabilityError == "" {
		t.Fatal("ReachabilityError not recorded for a failed probe")
	}
}

func TestReproveReselectsTmuxWhenHerdrPrimaryUnreachable(t *testing.T) {
	root := t.TempDir()
	cfg := reachTestCfg(t, root)
	renameLog := filepath.Join(t.TempDir(), "rename.log")
	withFakeHerdrList(t, renameLog, map[string]string{})
	withFakeTmuxTargets(t, "%7")
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("TMUX_PANE", "%7")

	writeHerdrReg(t, cfg, "codex-agentchute", "codex-agentchute")

	rebound, err := reproveAndRebindOwnWake(cfg, "codex-agentchute")
	if err != nil {
		t.Fatalf("reproveAndRebindOwnWake err = %v", err)
	}
	if !rebound {
		t.Fatal("rebound = false, want true (unreachable herdr primary should reselect live tmux pane)")
	}
	if got := renameLogContents(t, renameLog); got != "" {
		t.Fatalf("rename log = %q, want empty (no herdr rebind without pane context)", got)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex-agentchute"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.WakeMethod != "tmux" || reg.WakeTarget != "%7" {
		t.Fatalf("wake = %s/%s, want tmux/%%7", reg.WakeMethod, reg.WakeTarget)
	}
	if reg.ReachableAt == nil {
		t.Fatal("ReachableAt not written after selecting live tmux primary")
	}
	if reg.ReachabilityMethod != "tmux" || reg.ReachabilityTarget != "%7" {
		t.Fatalf("reachability endpoint = %s/%s, want tmux/%%7", reg.ReachabilityMethod, reg.ReachabilityTarget)
	}
	if reg.ReachabilityError != "" {
		t.Fatalf("ReachabilityError = %q, want empty after selecting live tmux primary", reg.ReachabilityError)
	}
}

// The herdr handle can differ from the bound name (gemini's handle "agy" vs name
// "gemini-cli-agentchute"), so `herdr agent get <name>` returns agent_not_found.
// reprove MUST resolve via `herdr agent list` + match on the `name` field, so a
// correctly-bound-to-us name is found WITHOUT a (spurious) rebind.
func TestReproveHerdr_ResolvesByNameWhenHandleDiffers(t *testing.T) {
	root := t.TempDir()
	cfg := reachTestCfg(t, root)
	renameLog := filepath.Join(t.TempDir(), "rename.log")
	// `agent list` knows the name → OUR pane; `agent get <name>` (fake) always
	// returns not_found (handle "agy" ≠ name).
	withFakeHerdrList(t, renameLog, map[string]string{"gemini-cli-agentchute": "w3:p7"})
	setupHerdrEnv(t, "w3:p7") // our pane is w3:p7 — the name already maps to us

	writeHerdrReg(t, cfg, "gemini-cli-agentchute", "gemini-cli-agentchute")

	rebound, err := reproveAndRebindOwnWake(cfg, "gemini-cli-agentchute")
	if err != nil {
		t.Fatalf("reproveAndRebindOwnWake err = %v", err)
	}
	if rebound {
		t.Fatal("rebound = true; a name already mapping to us (resolved via list) must NOT rebind — proves get-by-name was used")
	}
	if got := renameLogContents(t, renameLog); got != "" {
		t.Fatalf("rename log = %q, want empty (no rebind needed)", got)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("gemini-cli-agentchute"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.ReachableAt == nil {
		t.Fatal("ReachableAt not written; list-match should have proven reachability")
	}
	if reg.ReachabilityError != "" {
		t.Fatalf("ReachabilityError = %q, want empty (resolved cleanly via list)", reg.ReachabilityError)
	}
}

// The wake path must use the same name-match resolver as reachability. If the
// herdr handle differs from the bound name, `agent get <name>` returns not_found
// while `agent list` still proves the stable wake target and pane.
func TestPokeHerdr_ResolvesByNameWhenHandleDiffers(t *testing.T) {
	logPath := withFakeHerdrWakeByName(t, map[string]string{"gemini-cli-agentchute": "w3:p7"})

	if err := loop.PokeHerdrTargetContext(context.Background(), "gemini-cli-agentchute"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "agent\tlist\t") {
		t.Fatalf("herdr wake did not resolve via `agent list`:\n%s", got)
	}
	if strings.Contains(got, "agent\tget\tgemini-cli-agentchute\t") {
		t.Fatalf("herdr wake fell back to `agent get` for a bound name:\n%s", got)
	}
	if !strings.Contains(got, "agent\tsend\tgemini-cli-agentchute\t[agentchute:herdr] check inbox\t") {
		t.Fatalf("herdr wake did not send the prompt by stable name:\n%s", got)
	}
	if !strings.Contains(got, "pane\tsend-keys\tw3:p7\tEnter\t") {
		t.Fatalf("herdr wake did not submit Enter to the pane resolved by name:\n%s", got)
	}
}
