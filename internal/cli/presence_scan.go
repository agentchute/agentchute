package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// UnenrolledProcess is one wrapper-shaped presence in this pool that has no
// matching live registration. It is the operator answer to "who is running here
// but never enrolled?" — the read-only slice of the presence daemon (WI-E4).
type UnenrolledProcess struct {
	Kind       string // herdr | tmux | runner-socket | process
	Hint       string // identity hint (herdr name / tmux pane / agent id / pid)
	Cwd        string // the process working dir that mapped to this pool
	Suggestion string // how to enroll it
}

// ---- enumeration seams ----
//
// Each presence source is enumerated through a package-level function var so
// tests can inject deterministic fakes (and so the scan never shells out during
// unrelated unit tests). Production wires the defaults below. Every default is
// best-effort: on a missing tool or any enumeration error it returns nil and the
// source is skipped — the scan must never fail just because one source is
// unavailable.

type herdrPresenceEntry struct {
	Name string
	Cwd  string
}

type tmuxPresenceEntry struct {
	PaneID string
	Cwd    string
}

type runnerPresenceEntry struct {
	AgentID string
	Target  string // unix:<socket> (for the hint)
	Cwd     string
}

type processPresenceEntry struct {
	PID     int
	Command string
	Cwd     string
}

var (
	listHerdrAgents   = defaultListHerdrAgents
	listTmuxPanes     = defaultListTmuxPanes
	listRunnerSockets = defaultListRunnerSockets
	listProcesses     = defaultListProcesses
	// processParentPID resolves a pid's parent pid (macOS/Linux `ps -p <pid> -o
	// ppid=`). Package var so the runner-child suppression below is unit-testable
	// without real processes. Returns 0 on any failure or for pid<=1.
	processParentPID = defaultProcessParentPID
)

// processAncestryDepthLimit bounds the ppid walk so a pathological/cyclic
// process table can never spin the read-only presence scan.
const processAncestryDepthLimit = 32

// processAncestryHasEnrolledRunner reports whether any ANCESTOR (walking ppid
// from pid's parent upward) is an enrolled runner for this pool — defined as a
// LIVE, SAME-USER process whose cmdline is an `agentchute serve` for THIS pool.
// (Recorded runner.json pids are deliberately NOT trusted on their own: a stale
// pid can be reused.) A vendor wrapper launched by the runner (agentchute serve ->
// codex -> node -> vendor codex) is therefore NOT a raw, unenrolled bypass — it
// is the runner's child. Best-effort + cheap: bounded depth, cycle-guarded, and
// only invoked for an in-pool wrapper process.
func processAncestryHasEnrolledRunner(pid int, cfg *loop.Config) bool {
	if pid <= 0 {
		return false
	}
	seen := map[int]bool{pid: true}
	cur := pid
	for depth := 0; depth < processAncestryDepthLimit; depth++ {
		parent := processParentPID(cur)
		if parent <= 1 || seen[parent] {
			return false
		}
		seen[parent] = true
		// Revalidate the ancestor DIRECTLY — never trust a recorded runner.json
		// pid (it can be stale or reused as an unrelated process). The ancestor
		// counts only if it is a LIVE, SAME-USER process whose cmdline is an
		// `agentchute serve` for THIS pool.
		if setupProcessAlive(parent) && processSameUser(parent) &&
			setupCommandMatchesRunnerPool(setupProcessCommandLine(parent), cfg) {
			return true
		}
		cur = parent
	}
	return false
}

// processOwnerUID is a seam (overridable in tests) returning a pid's owner uid.
var processOwnerUID = defaultProcessOwnerUID

func defaultProcessOwnerUID(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "uid=").Output()
	if err != nil {
		return 0, false
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return uid, true
}

// processSameUser reports whether pid is owned by the current user. Fails closed
// (returns false) when ownership cannot be determined.
func processSameUser(pid int) bool {
	uid, ok := processOwnerUID(pid)
	return ok && uid == os.Getuid()
}

// defaultProcessParentPID resolves the parent pid via `ps -p <pid> -o ppid=`.
// Read-only; returns 0 when ps is unavailable, the process is gone, or the pid
// is the init/swapper (<=1).
func defaultProcessParentPID(pid int) int {
	if pid <= 1 {
		return 0
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
	if err != nil {
		return 0
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || ppid < 0 {
		return 0
	}
	return ppid
}

// presenceProbeTimeout bounds the per-socket runner liveness ping done while
// enumerating present runner sockets. Short so a dead socket can't stall a
// read-only diagnostic.
const presenceProbeTimeout = 500 * time.Millisecond

// scanUnenrolledWrappers enumerates wrapper presences on this host (herdr
// agents, tmux panes, runner sockets, raw wrapper processes) whose working dir
// maps — via loop.Discover, by cwd alone — to THIS pool's control repo and that
// have NO matching live registration, returning one UnenrolledProcess per hit.
//
// It is STRICTLY READ-ONLY: it reads registrations + per-agent local state and
// enumerates host state, but never creates or repairs a registration (that is
// WI-E4's job). The only error it returns is a failure to read the agents
// directory; every enumerator error is swallowed so a missing herdr/tmux/ps
// degrades to "that source reported nothing."
func scanUnenrolledWrappers(cfg *loop.Config) ([]UnenrolledProcess, error) {
	if cfg == nil {
		return nil, nil
	}

	regs, err := readRegistrations(cfg)
	if err != nil {
		return nil, err
	}

	// Pull-only (Gate 6c): registrations carry no wake target, so a herdr/tmux
	// presence can no longer be matched to a registration BY wake target. herdr
	// agents map to an enrolled lane only when the bound name is itself a
	// registered agent id; tmux panes have no registration link at all, so an
	// in-pool pane always surfaces as present-but-not-enrolled.
	agentIDs := map[string]bool{}
	enrolledPIDs := map[int]bool{}
	for id := range regs {
		agentIDs[id] = true
		// Best-effort: a wrapper PID recorded in a live agent's session/runner
		// state is "accounted for" — a raw process with that PID is NOT a bypass.
		if sess, e := loop.LoadActiveSession(cfg, id); e == nil && sess.PID > 0 {
			enrolledPIDs[sess.PID] = true
		}
		if rs, e := loop.LoadRunnerState(cfg, id); e == nil {
			if rs.RunnerPID > 0 {
				enrolledPIDs[rs.RunnerPID] = true
			}
			if rs.ChildPID > 0 {
				enrolledPIDs[rs.ChildPID] = true
			}
		}
	}

	var out []UnenrolledProcess

	// Enumeration + in-pool cwd gate for the two high-confidence-capable
	// sources (herdr panes + runner sockets). The per-source CREATE exclusion of
	// already-registered ids is applied below; the enumeration + gating itself
	// lives in enumerateInPoolHerdrRunnerPresences.
	hrPresences := enumerateInPoolHerdrRunnerPresences(cfg)

	// herdr agents: registrations carry no wake target, so a herdr pane counts as
	// enrolled only when its bound name matches a registered agent id outright.
	for _, p := range hrPresences {
		if p.Kind != "herdr" {
			continue
		}
		name := p.Hint
		if agentIDs[name] {
			continue
		}
		out = append(out, UnenrolledProcess{
			Kind:       "herdr",
			Hint:       name,
			Cwd:        p.Cwd,
			Suggestion: fmt.Sprintf("herdr agent %q is in this pool but not enrolled; relaunch via the `ac` dispatcher (`ac serve <wrapper>`) or run `agentchute boot --as %s`", name, name),
		})
	}

	// tmux panes: detected by enumerating panes and their cwds (`tmux list-panes`).
	// A pane id has no registration link, so every in-pool pane surfaces as
	// present-but-not-enrolled.
	for _, p := range listTmuxSafe() {
		pane := strings.TrimSpace(p.PaneID)
		if pane == "" {
			continue
		}
		if !cwdMapsToPool(cfg, p.Cwd) {
			continue
		}
		out = append(out, UnenrolledProcess{
			Kind:       "tmux",
			Hint:       pane,
			Cwd:        p.Cwd,
			Suggestion: fmt.Sprintf("tmux pane %s is in this pool but not enrolled; relaunch via the `ac` dispatcher (`ac serve <wrapper>`) or run `agentchute boot --as <id>`", pane),
		})
	}

	// runner sockets: a live runner socket under this pool's loop dir whose agent
	// id has no registration. These are in-pool by construction (the socket lives
	// inside cfg.LoopDir), so no cwd mapping is needed.
	for _, p := range hrPresences {
		if p.Kind != "runner-socket" {
			continue
		}
		id := p.Hint
		if agentIDs[id] {
			continue
		}
		out = append(out, UnenrolledProcess{
			Kind:       "runner-socket",
			Hint:       id,
			Cwd:        p.Cwd,
			Suggestion: fmt.Sprintf("a live agentchute-run socket for %q has no registration; run `agentchute boot --as %s` to enroll it", id, id),
		})
	}

	// raw wrapper processes: a known wrapper binary running in this pool whose PID
	// is not recorded in any live agent's session/runner state.
	for _, pr := range listProcessSafe() {
		if !isKnownWrapperProcess(pr.Command) {
			continue
		}
		if pr.PID > 0 && enrolledPIDs[pr.PID] {
			continue
		}
		if !cwdMapsToPool(cfg, pr.Cwd) {
			continue
		}
		// Suppress runner children: a wrapper whose ancestry includes an enrolled
		// runner for this pool was launched BY the runner (agentchute serve -> wrapper
		// -> ... -> vendor binary); it is not a raw, unenrolled launch. Checked last
		// so the ppid walk only runs for an in-pool wrapper process (cheapest).
		if pr.PID > 0 && processAncestryHasEnrolledRunner(pr.PID, cfg) {
			continue
		}
		out = append(out, UnenrolledProcess{
			Kind:       "process",
			Hint:       fmt.Sprintf("pid %d (%s)", pr.PID, filepath.Base(strings.TrimSpace(pr.Command))),
			Cwd:        pr.Cwd,
			Suggestion: "wrapper running raw with no agentchute enrollment; relaunch via the `ac` dispatcher (`ac serve <wrapper>`) or run `agentchute boot --as <id>`",
		})
	}

	return out, nil
}

// enumerateInPoolHerdrRunnerPresences enumerates the two presence sources that
// can ever reach HIGH confidence — herdr panes and runner sockets — that belong
// to THIS pool, as raw (Kind/Hint/Cwd) UnenrolledProcess values. It applies ONLY
// the enumeration + in-pool cwd gate:
//
//   - herdr: each live pane's name (trimmed, non-empty) whose foreground cwd
//     maps to this pool via cwdMapsToPool;
//   - runner sockets: each answering socket's agent id (trimmed, non-empty),
//     in-pool by construction (the socket lives under cfg.LoopDir), so no cwd
//     gate is applied.
//
// It deliberately does NOT apply caller-specific filtering: it does NOT exclude
// already-registered ids (scanUnenrolledWrappers does that for its CREATE
// candidates). No Suggestion is set; a caller that needs one builds it itself.
// herdr entries are emitted before runner entries to preserve caller ordering.
// STRICTLY READ-ONLY.
func enumerateInPoolHerdrRunnerPresences(cfg *loop.Config) []UnenrolledProcess {
	var out []UnenrolledProcess
	for _, a := range listHerdrSafe() {
		name := strings.TrimSpace(a.Name)
		if name == "" || !cwdMapsToPool(cfg, a.Cwd) {
			continue
		}
		out = append(out, UnenrolledProcess{Kind: "herdr", Hint: name, Cwd: a.Cwd})
	}
	for _, s := range listRunnerSafe(cfg) {
		id := strings.TrimSpace(s.AgentID)
		if id == "" {
			continue
		}
		out = append(out, UnenrolledProcess{Kind: "runner-socket", Hint: id, Cwd: s.Cwd})
	}
	return out
}

// cwdMapsToPool reports whether cwd resolves — by cwd alone (the enumerated
// process's working dir, never this process's env) — to the same control repo as
// cfg. A discovery failure (no control repo for that cwd) means "not in pool."
func cwdMapsToPool(cfg *loop.Config, cwd string) bool {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cfg == nil {
		return false
	}
	other, err := loop.Discover(loop.DiscoverOpts{Cwd: cwd})
	if err != nil {
		return false
	}
	return filepath.Clean(other.ControlRepo) == filepath.Clean(cfg.ControlRepo)
}

// ---- recover-guarded seam wrappers ----
//
// A buggy (or test) enumerator must never crash a read-only diagnostic, so each
// seam call is shielded with a recover.

func listHerdrSafe() (r []herdrPresenceEntry) {
	defer func() { _ = recover() }()
	if listHerdrAgents != nil {
		r = listHerdrAgents()
	}
	return
}

func listTmuxSafe() (r []tmuxPresenceEntry) {
	defer func() { _ = recover() }()
	if listTmuxPanes != nil {
		r = listTmuxPanes()
	}
	return
}

func listRunnerSafe(cfg *loop.Config) (r []runnerPresenceEntry) {
	defer func() { _ = recover() }()
	if listRunnerSockets != nil {
		r = listRunnerSockets(cfg)
	}
	return
}

func listProcessSafe() (r []processPresenceEntry) {
	defer func() { _ = recover() }()
	if listProcesses != nil {
		r = listProcesses()
	}
	return
}

// ---- default enumerators (best-effort; nil on any failure) ----

// defaultListHerdrAgents enumerates herdr agents via `herdr agent list`,
// preferring foreground_cwd over cwd. Read-only; returns nil when herdr is
// absent or the call fails.
func defaultListHerdrAgents() []herdrPresenceEntry {
	if !herdrAvailable() {
		return nil
	}
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
	}
	if json.Unmarshal(out, &resp) != nil {
		return nil
	}
	var entries []herdrPresenceEntry
	for _, a := range resp.Result.Agents {
		name := strings.TrimSpace(a.Name)
		cwd := strings.TrimSpace(a.ForegroundCwd)
		if cwd == "" {
			cwd = strings.TrimSpace(a.Cwd)
		}
		if name == "" || cwd == "" {
			continue
		}
		entries = append(entries, herdrPresenceEntry{Name: name, Cwd: cwd})
	}
	return entries
}

// defaultListTmuxPanes enumerates every tmux pane and its current path via
// `tmux list-panes -a`. Read-only; nil when tmux is absent or the call fails.
func defaultListTmuxPanes() []tmuxPresenceEntry {
	if _, err := exec.LookPath(tmuxProbeBinary); err != nil {
		return nil
	}
	out, err := exec.Command(tmuxProbeBinary, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}").Output()
	if err != nil {
		return nil
	}
	var entries []tmuxPresenceEntry
	for _, line := range strings.Split(string(out), "\n") {
		pane, cwd, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		pane = strings.TrimSpace(pane)
		cwd = strings.TrimSpace(cwd)
		if pane == "" || cwd == "" {
			continue
		}
		entries = append(entries, tmuxPresenceEntry{PaneID: pane, Cwd: cwd})
	}
	return entries
}

// defaultListRunnerSockets enumerates live agentchute-run runners under this
// pool's loop dir. Simple-again Gate 6b (pull-only): the runner no longer owns a
// receive socket, so a live runner is detected from its runner STATE file
// (state/<agent>/runner.json) — same-host, status != offline, and a live runner
// pid — instead of a socket ping. A stale state file (dead pid / offline) is not
// a present process.
func defaultListRunnerSockets(cfg *loop.Config) []runnerPresenceEntry {
	if cfg == nil {
		return nil
	}
	stateDir := filepath.Join(cfg.LoopDir, "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}
	localHost := strings.TrimSpace(localHostname())
	var out []runnerPresenceEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		agentID := e.Name()
		if loop.ValidateAgentID(agentID) != nil {
			continue
		}
		st, err := loop.LoadRunnerState(cfg, agentID)
		if err != nil {
			continue
		}
		if localHost != "" && strings.TrimSpace(st.Host) != "" && st.Host != localHost {
			continue // runner pid liveness is only provable same-host.
		}
		if strings.EqualFold(strings.TrimSpace(st.Status), "offline") {
			continue
		}
		if st.RunnerPID <= 0 || !processAlive(st.RunnerPID) {
			continue
		}
		out = append(out, runnerPresenceEntry{AgentID: agentID, Cwd: cfg.ControlRepo})
	}
	return out
}

// defaultListProcesses enumerates known wrapper processes via `ps` and resolves
// each one's working dir. Read-only; nil when `ps` is unavailable. Only known
// wrapper binaries are cwd-resolved (a handful at most), so the per-process cwd
// lookup stays cheap.
func defaultListProcesses() []processPresenceEntry {
	if _, err := exec.LookPath("ps"); err != nil {
		return nil
	}
	out, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	var entries []processPresenceEntry
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		cmd := strings.Join(fields[1:], " ")
		if !isKnownWrapperProcess(cmd) {
			continue
		}
		cwd := processCwd(pid)
		if cwd == "" {
			continue
		}
		entries = append(entries, processPresenceEntry{PID: pid, Command: cmd, Cwd: cwd})
	}
	return entries
}

// processCwd resolves a process's working directory best-effort: the /proc
// symlink on Linux, then `lsof` on macOS/BSD. Returns "" when neither works
// (e.g. Windows, or the process exited). No-op safe on every platform.
func processCwd(pid int) string {
	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		return strings.TrimSpace(link)
	}
	if _, err := exec.LookPath("lsof"); err == nil {
		out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "n") {
					return strings.TrimSpace(strings.TrimPrefix(line, "n"))
				}
			}
		}
	}
	return ""
}
