package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

type setupRuntimeResetResult struct {
	Agents       []string
	Pollers      []string
	Runners      []string
	RuntimeFiles []string
	HerdrNames   []string
	Warnings     []string
}

var setupProcessCommandLine = processCommandLine
var setupProcessAlive = processAlive
var setupSignalProcess = signalProcess

func resetSetupRuntimeState(root string, cfg *loop.Config, wrappers []string) setupRuntimeResetResult {
	agentIDs, warnings := setupResetAgentIDs(root, cfg, wrappers)
	result := setupRuntimeResetResult{Agents: agentIDs, Warnings: warnings}
	for _, agentID := range agentIDs {
		if stopped, warning := stopSetupPoller(cfg, agentID); warning != "" {
			result.Warnings = append(result.Warnings, warning)
		} else if stopped {
			result.Pollers = append(result.Pollers, agentID)
		}
		if stopped, warning := stopSetupRunner(cfg, agentID); warning != "" {
			result.Warnings = append(result.Warnings, warning)
		} else if stopped {
			result.Runners = append(result.Runners, agentID)
		}
		for _, path := range setupRuntimeStatePaths(cfg, agentID) {
			if err := os.Remove(path); err == nil {
				result.RuntimeFiles = append(result.RuntimeFiles, filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path))
			} else if err != nil && !os.IsNotExist(err) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("remove runtime state %s: %v", path, err))
			}
		}
		// Pull-only (Gate 6c): herdr is retired as a wake transport and a
		// registration carries no herdr name binding, so there is no herdr-name
		// binding to clear on reset. The HerdrNames result field stays for output
		// stability but is no longer populated.
	}
	sort.Strings(result.Pollers)
	sort.Strings(result.Runners)
	sort.Strings(result.RuntimeFiles)
	sort.Strings(result.HerdrNames)
	sort.Strings(result.Warnings)
	return result
}

func setupResetAgentIDs(root string, cfg *loop.Config, wrappers []string) ([]string, []string) {
	ids := map[string]bool{}
	var warnings []string
	for _, id := range setupRegistrationAgentIDs(cfg) {
		ids[id] = true
	}
	for _, id := range setupStateAgentIDs(cfg) {
		ids[id] = true
	}
	for _, id := range setupExpectedContextualAgentIDs(root, wrappers) {
		ids[id] = true
	}
	for _, id := range setupHerdrAgentIDsForRepo(root) {
		ids[id] = true
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		if err := loop.ValidateAgentID(id); err != nil {
			warnings = append(warnings, fmt.Sprintf("skip invalid reset agent id %q: %v", id, err))
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, warnings
}

func setupRegistrationAgentIDs(cfg *loop.Config) []string {
	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		return nil
	}
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") || name == "README.md" {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		if err := loop.ValidateAgentID(id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func setupStateAgentIDs(cfg *loop.Config) []string {
	entries, err := os.ReadDir(filepath.Join(cfg.LoopDir, "state"))
	if err != nil {
		return nil
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := loop.ValidateAgentID(id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func setupExpectedContextualAgentIDs(root string, wrappers []string) []string {
	if len(wrappers) == 0 {
		wrappers = setupWrapperNames()
	}
	slug := getFolderSlug(root)
	ids := make([]string, 0, len(wrappers))
	seen := map[string]bool{}
	for _, wrapper := range wrappers {
		canon := canonicalAgentIDForVendor(wrapper)
		if canon == "" {
			continue
		}
		id := canon + "-" + slug
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids
}

func setupHerdrAgentIDsForRepo(root string) []string {
	if !herdrAvailable() {
		return nil
	}
	allowed := setupAllowedHerdrCanonicalIDs()
	out, err := exec.Command(herdrProbeBinary, "agent", "list").Output()
	if err != nil {
		return nil
	}
	var resp struct {
		Result struct {
			Agents []struct {
				Name          string `json:"name"`
				Cwd           string `json:"cwd"`
				ForegroundCwd string `json:"foreground_cwd"`
			} `json:"agents"`
		} `json:"result"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &resp) != nil || resp.Error != nil {
		return nil
	}
	var ids []string
	for _, agent := range resp.Result.Agents {
		name := strings.TrimSpace(agent.Name)
		if name == "" || !setupPathMatchesRoot(agent.Cwd, root) && !setupPathMatchesRoot(agent.ForegroundCwd, root) {
			continue
		}
		if !setupAgentMatchesCanonical(name, allowed) {
			continue
		}
		if err := loop.ValidateAgentID(name); err == nil {
			ids = append(ids, name)
		}
	}
	return ids
}

func setupAllowedHerdrCanonicalIDs() []string {
	var out []string
	seen := map[string]bool{}
	for _, wrapper := range setupWrapperNames() {
		canon := canonicalAgentIDForVendor(wrapper)
		if canon == "" || seen[canon] {
			continue
		}
		out = append(out, canon)
		seen[canon] = true
	}
	return out
}

func setupAgentMatchesCanonical(agentID string, allowed []string) bool {
	for _, canon := range allowed {
		if registrationMatchesCanonical(agentID, canon) {
			return true
		}
	}
	return false
}

func setupRuntimeStatePaths(cfg *loop.Config, agentID string) []string {
	return []string{
		cfg.PollerHeartbeatPath(agentID),
		cfg.ActiveSessionPath(agentID),
		cfg.RunnerStatePath(agentID),
		cfg.RunnerSocketPath(agentID),
	}
}

func stopSetupPoller(cfg *loop.Config, agentID string) (bool, string) {
	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		return false, ""
	}
	if !setupLocalHost(hb.Host) || hb.PID <= 0 || !setupProcessAlive(hb.PID) {
		return false, ""
	}
	cmdline := setupProcessCommandLine(hb.PID)
	if !setupCommandMatches(cmdline, agentID, "poller run", cfg) {
		return false, fmt.Sprintf("not stopping poller for %s pid=%d; process command did not match this agentchute pool", agentID, hb.PID)
	}
	if err := setupSignalProcess(hb.PID, syscall.SIGTERM); err != nil {
		return false, fmt.Sprintf("stop poller for %s pid=%d: %v", agentID, hb.PID, err)
	}
	waitSetupProcessExit(hb.PID, 500*time.Millisecond)
	return true, ""
}

func stopSetupRunner(cfg *loop.Config, agentID string) (bool, string) {
	st, err := loop.LoadRunnerState(cfg, agentID)
	if err != nil {
		return false, ""
	}
	if !setupLocalHost(st.Host) {
		return false, ""
	}
	if st.RunnerPID <= 0 || !setupProcessAlive(st.RunnerPID) {
		return false, ""
	}
	cmdline := setupProcessCommandLine(st.RunnerPID)
	// Runner attribution = the runner.json pid->id binding (loaded above) +
	// setupProcessAlive (checked above) + this being an `agentchute run` for THIS
	// pool. A runner is launched WITHOUT --as (contextual id), so its cmdline never
	// carries the agent id; requiring it here was a false-negative that left every
	// live runner un-stopped. The state file binds pid->id; the cmdline only proves
	// the pool. Simple-again Gate 6b (pull-only): the runner owns no receive socket,
	// so SIGTERM is the stop — its signal handler marks the registration offline and
	// releases its serve lease on exit.
	if !setupCommandMatchesRunnerPool(cmdline, cfg) {
		return false, fmt.Sprintf("not stopping runner for %s pid=%d; process command did not match this agentchute pool", agentID, st.RunnerPID)
	}
	if err := setupSignalProcess(st.RunnerPID, syscall.SIGTERM); err != nil {
		return false, fmt.Sprintf("stop runner for %s pid=%d: %v", agentID, st.RunnerPID, err)
	}
	waitSetupProcessExit(st.RunnerPID, 500*time.Millisecond)
	return true, ""
}

func waitSetupProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !setupProcessAlive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func setupCommandMatches(cmdline, agentID, subcommand string, cfg *loop.Config) bool {
	if !setupCommandMatchesPool(cmdline, subcommand, cfg) {
		return false
	}
	return setupCommandHasAgentID(cmdline, agentID)
}

// setupCommandMatchesRunnerPool attributes a live RUNNER to THIS pool. Unlike a
// poller (which is launched with --as <id> and so carries its agent id in the
// cmdline), a runner is launched with the CONTEXTUAL id — it has NO --as — so its
// cmdline never contains the agent id. The pid->id binding therefore comes from
// the runner.json state file (state/<id>/runner.json recorded this pid for <id>),
// and the cmdline only needs to prove the process is an `agentchute run` for THIS
// pool (its --control-repo/--loop-dir resolves to this pool). Requiring the agent
// id in a runner cmdline is the false-negative this fixes: every live runner was
// reported "ambiguous ... cmdline did not match this pool; refusing (fail closed)".
// A foreign pool or non-agentchute process still fails this check (still ambiguous,
// still fail-closed).
func setupCommandMatchesRunnerPool(cmdline string, cfg *loop.Config) bool {
	return setupCommandMatchesPool(cmdline, "run", cfg)
}

func setupCommandMatchesPool(cmdline, subcommand string, cfg *loop.Config) bool {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return false
	}
	lower := strings.ToLower(cmdline)
	normalized := " " + strings.Join(strings.Fields(lower), " ") + " "
	switch subcommand {
	case "poller run":
		if !strings.Contains(normalized, " agentchute poller run ") && !strings.Contains(normalized, "/agentchute poller run ") {
			return false
		}
	case "run":
		if !strings.Contains(normalized, " agentchute run ") && !strings.Contains(normalized, "/agentchute run ") {
			return false
		}
	default:
		return false
	}
	// EXACT --control-repo/--loop-dir VALUE match (not substring): a foreign
	// `--control-repo /tmp/repo2` must NOT match this pool's `/tmp/repo`. For
	// runners this is the SOLE pool proof (their cmdline carries no agent id), and
	// it gates a SIGTERM, so loose substring matching here is unsafe.
	if setupPathsEquivalent(setupCommandFlagValue(cmdline, "--control-repo"), cfg.ControlRepo) ||
		setupPathsEquivalent(setupCommandFlagValue(cmdline, "--loop-dir"), cfg.LoopDir) {
		return true
	}
	return false
}

// setupCommandFlagValue returns the value of `flag` in a process cmdline,
// handling both `--flag value` and `--flag=value` forms; "" if absent.
func setupCommandFlagValue(cmdline, flag string) string {
	fields := strings.Fields(cmdline)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
		if strings.HasPrefix(f, flag+"=") {
			return strings.TrimPrefix(f, flag+"=")
		}
	}
	return ""
}

// setupPathCandidates returns the equivalence set for a path: itself, its abs
// form, EvalSymlinks resolution, and the /private/var <-> /var twins (macOS).
func setupPathCandidates(path string) map[string]bool {
	path = strings.TrimSpace(path)
	out := map[string]bool{}
	if path == "" {
		return out
	}
	out[path] = true
	if abs, err := filepath.Abs(path); err == nil {
		out[abs] = true
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			out[r] = true
		}
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		out[r] = true
	}
	for c := range mapsClone(out) {
		if strings.HasPrefix(c, "/private/var/") {
			out["/var/"+strings.TrimPrefix(c, "/private/var/")] = true
		}
		if strings.HasPrefix(c, "/var/") {
			out["/private/var/"+strings.TrimPrefix(c, "/var/")] = true
		}
	}
	return out
}

// setupPathsEquivalent reports whether two paths refer to the same location,
// comparing whole normalized paths exactly (no substring/prefix matching).
func setupPathsEquivalent(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	ca := setupPathCandidates(a)
	for c := range setupPathCandidates(b) {
		if c != "" && ca[c] {
			return true
		}
	}
	return false
}

func setupCommandHasAgentID(cmdline, agentID string) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false
	}
	fields := strings.Fields(cmdline)
	for i, field := range fields {
		switch {
		case field == agentID:
			return true
		case field == "--as="+agentID:
			return true
		case field == "--agent-id="+agentID:
			return true
		case strings.HasPrefix(field, "AGENTCHUTE_AGENT_ID=") && strings.TrimPrefix(field, "AGENTCHUTE_AGENT_ID=") == agentID:
			return true
		case (field == "--as" || field == "--agent-id") && i+1 < len(fields) && fields[i+1] == agentID:
			return true
		}
	}
	return false
}

func mapsClone(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func processCommandLine(pid int) string {
	if pid <= 0 {
		return ""
	}
	// -ww disables ps's column-width truncation so a very long runner/poller
	// cmdline (long control-repo/loop-dir paths) is returned in full; a truncated
	// cmdline could drop the pool path and make a real match look ambiguous.
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func signalProcess(pid int, sig os.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

func setupLocalHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return true
	}
	local := strings.TrimSpace(localHostname())
	return local == "" || host == local
}

func setupPathMatchesRoot(path, root string) bool {
	path = strings.TrimSpace(path)
	root = strings.TrimSpace(root)
	if path == "" || root == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	return samePath(absPath, absRoot)
}
