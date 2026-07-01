package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// setup_wipe.go — the DESTRUCTIVE `agentchute setup --reset --wipe-state` phase.
//
// This wipes ONLY the loop RUNTIME state (inbox/archive/malformed/live contents,
// scratch dirs, root runtime logs, live agent registrations, and per-agent state)
// after stopping local pool processes and proving the bus is not live. It is the
// v0.7.0 -> v2 transition tool. Everything here is guarded HARD: it never
// RemoveAll's the loop dir itself, never follows a symlink, never wipes a
// non-canonical/overridden loop, and refuses outright if a live local process or
// a fresh foreign-host presence/serve claim is detected.
//
// Scaffold preservation is by an explicit ALLOWLIST/NAME (README.md,
// *.example.md, setup.json) — NOT by git-tracking, because an install may not be
// in git.

// wipeProcessWait is the bounded wait the wipe gives a SIGTERM'd local poller/
// runner to exit before it re-verifies liveness. Longer than the soft reset's
// 500ms because a destructive wipe must not race a still-shutting-down process.
// Package var so tests can shrink it.
var wipeProcessWait = 3 * time.Second

// wipeRecreateDirs are the loop subdirectories that must exist (0700) after a
// wipe. inbox/archive/malformed/live have their CONTENTS removed; agents/state
// keep their allowlisted scaffold. All six are (re)created to guarantee a usable
// post-wipe loop.
var wipeRecreateDirs = []string{"inbox", "archive", "malformed", "live", "agents", "state"}

// wipeLegacyNamespaceDotdirs is the EXPLICIT allowlist of pre-`.agentchute`
// namespace dotdirs whose `<dotdir>/loop` may be removed by --wipe-state. Only
// `.rehumanlabs` is concretely documented in this project's history (the original
// brand namespace, removed in v0.2.2). Any OTHER dotdir that contains an
// agentchute-loop sentinel is reported as "manual cleanup", never deleted.
// Keep this list TIGHT: every entry is still subjected to the full per-candidate
// guard (basename==loop, no symlink, sentinel present, under the control repo).
var wipeLegacyNamespaceDotdirs = []string{
	".rehumanlabs",
}

// wipeCategory is one grouped set of delete targets under a single parent
// (e.g. all of inbox/). Preserved records the scaffold entries deliberately KEPT
// in that parent, for the plan printout.
type wipeCategory struct {
	Name      string
	Parent    string
	Targets   []string
	Preserved []string
}

// wipePlan is the fully-computed, pure (no-mutation) delete plan. Computing it
// reads the tree; nothing is removed until executeWipePlan runs.
type wipePlan struct {
	LoopDir       string
	ControlRepo   string
	Categories    []wipeCategory
	LegacyDirs    []string // verified legacy <dotdir>/loop dirs to RemoveAll
	ManualCleanup []string // legacy-looking dotdirs reported, NOT deleted
}

// ---------- path guards ----------

// validateWipePaths is the FIRST gate: it proves controlRepo + loopDir are safe
// to operate on before any scan or deletion. Any violation is a HARD error.
func validateWipePaths(controlRepo, loopDir string) error {
	if strings.TrimSpace(controlRepo) == "" {
		return fmt.Errorf("empty control repo")
	}
	if strings.TrimSpace(loopDir) == "" {
		return fmt.Errorf("empty loop dir")
	}
	cr, err := filepath.Abs(filepath.Clean(controlRepo))
	if err != nil {
		return fmt.Errorf("resolve control repo %q: %w", controlRepo, err)
	}
	ld, err := filepath.Abs(filepath.Clean(loopDir))
	if err != nil {
		return fmt.Errorf("resolve loop dir %q: %w", loopDir, err)
	}
	switch ld {
	case "", "/", ".":
		return fmt.Errorf("refusing to wipe degenerate loop dir %q", ld)
	}
	if ld == cr {
		return fmt.Errorf("refusing: loop dir %q equals the control repo", ld)
	}
	if ld == filepath.Dir(ld) {
		return fmt.Errorf("refusing: loop dir %q is a filesystem root", ld)
	}
	// The loop parent (<repo>/.agentchute) must never itself be the wipe target.
	canonical := filepath.Join(cr, fixedDotDirName, loopDirBaseName)
	if ld == filepath.Dir(canonical) {
		return fmt.Errorf("refusing: loop dir %q is the loop parent", ld)
	}
	// CANONICAL-ONLY: the loop MUST be exactly <controlRepo>/.agentchute/loop. An
	// AGENTCHUTE_LOOP_DIR override pointing elsewhere fails closed — we never wipe
	// an operator's bespoke loop location.
	if ld != canonical {
		return fmt.Errorf("loop dir %q is not the canonical %q; refusing to wipe a non-canonical or AGENTCHUTE_LOOP_DIR-overridden loop", ld, canonical)
	}
	// Reject symlinked control repo, .agentchute, and loop — a symlink anywhere in
	// this chain could redirect deletion outside the project.
	for _, p := range []string{cr, filepath.Join(cr, fixedDotDirName), ld} {
		if err := rejectWipeSymlinkDir(p, true); err != nil {
			return err
		}
	}
	// Reject symlinked top-level runtime dirs. They are recreated/scanned, so a
	// symlinked one would let us traverse out of the loop.
	for _, sub := range wipeRecreateDirs {
		if err := rejectWipeSymlinkDir(filepath.Join(ld, sub), false); err != nil {
			return err
		}
	}
	return nil
}

// rejectWipeSymlinkDir lstat's p (never following) and fails if it is a symlink
// or (when mustExist) absent / not a directory. When mustExist is false an
// absent entry is fine (the runtime dir simply doesn't exist yet).
func rejectWipeSymlinkDir(p string, mustExist bool) error {
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			if mustExist {
				return fmt.Errorf("%s does not exist", p)
			}
			return nil
		}
		return fmt.Errorf("stat %s: %w", p, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to wipe through it", p)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory; refusing", p)
	}
	return nil
}

const (
	fixedDotDirName = ".agentchute"
	loopDirBaseName = "loop"
)

// wipePathApproved is the per-target defense-in-depth check applied immediately
// before each RemoveAll: the target must resolve under loopDir with no ".." and
// its first path component must be an approved runtime dir or a recognized
// loop-root leftover name. Legacy <dotdir>/loop dirs are NOT covered here (they
// live outside loopDir and are guarded separately at plan time).
func wipePathApproved(loopDir, target string) bool {
	rel, err := filepath.Rel(loopDir, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	switch parts[0] {
	case "inbox", "archive", "malformed", "live", "agents", "state":
		return true
	}
	if len(parts) == 1 && isWipeRootLeftoverName(parts[0]) {
		return true
	}
	return false
}

// isWipeRootLeftoverName reports whether a loop-ROOT entry is a known runtime
// leftover safe to delete: scratch dirs, the watchdog/poller/runner logs, and
// stray sockets/pid files. Everything else at the loop root (README.md,
// .gitignore, the runtime subdirs themselves) is preserved.
func isWipeRootLeftoverName(name string) bool {
	if strings.HasPrefix(name, "scratch-") {
		return true
	}
	switch name {
	case "watchdog.log", "poller.log", "runner.log":
		return true
	}
	return strings.HasSuffix(name, ".sock") || strings.HasSuffix(name, ".pid")
}

// ---------- plan computation (pure) ----------

// computeWipePlan scans the loop tree and builds the categorized delete plan.
// It MUTATES NOTHING. A stat/permission error (other than not-exist) is a hard
// failure here so the caller aborts BEFORE any deletion begins.
func computeWipePlan(cfg *loop.Config, controlRepo string) (wipePlan, error) {
	loopDir := cfg.LoopDir
	plan := wipePlan{LoopDir: loopDir, ControlRepo: controlRepo}

	// Full-contents categories: every entry under these dirs is runtime.
	for _, name := range []string{"inbox", "archive", "malformed", "live"} {
		cat, err := wipeContentsCategory(loopDir, name)
		if err != nil {
			return wipePlan{}, err
		}
		plan.Categories = append(plan.Categories, cat)
	}
	agentsCat, err := wipeAgentsCategory(loopDir)
	if err != nil {
		return wipePlan{}, err
	}
	plan.Categories = append(plan.Categories, agentsCat)

	stateCat, err := wipeStateCategory(loopDir)
	if err != nil {
		return wipePlan{}, err
	}
	plan.Categories = append(plan.Categories, stateCat)

	rootCat, err := wipeRootLeftoverCategory(loopDir)
	if err != nil {
		return wipePlan{}, err
	}
	plan.Categories = append(plan.Categories, rootCat)

	legacy, manual, err := scanLegacyNamespaces(controlRepo)
	if err != nil {
		return wipePlan{}, err
	}
	plan.LegacyDirs = legacy
	plan.ManualCleanup = manual
	return plan, nil
}

func wipeReadDir(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	return entries, nil
}

// wipeContentsCategory targets EVERY entry under <loop>/<name> (e.g. inbox/<id>
// subdirs including their .claimed/). The parent dir is kept and recreated later.
func wipeContentsCategory(loopDir, name string) (wipeCategory, error) {
	dir := filepath.Join(loopDir, name)
	entries, err := wipeReadDir(dir)
	if err != nil {
		return wipeCategory{}, err
	}
	cat := wipeCategory{Name: name, Parent: dir}
	for _, e := range entries {
		cat.Targets = append(cat.Targets, filepath.Join(dir, e.Name()))
	}
	sort.Strings(cat.Targets)
	return cat, nil
}

// wipeAgentsCategory targets live registrations (agents/*.md) EXCEPT the
// committed scaffold (README.md, *.example.md). Non-.md files and subdirs are
// preserved.
func wipeAgentsCategory(loopDir string) (wipeCategory, error) {
	dir := filepath.Join(loopDir, "agents")
	entries, err := wipeReadDir(dir)
	if err != nil {
		return wipeCategory{}, err
	}
	cat := wipeCategory{Name: "agents", Parent: dir}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			cat.Preserved = append(cat.Preserved, name+"/")
			continue
		}
		if !strings.HasSuffix(name, ".md") || name == "README.md" || strings.HasSuffix(name, ".example.md") {
			cat.Preserved = append(cat.Preserved, name)
			continue
		}
		cat.Targets = append(cat.Targets, filepath.Join(dir, name))
	}
	sort.Strings(cat.Targets)
	sort.Strings(cat.Preserved)
	return cat, nil
}

// wipeStateCategory targets every state/ entry EXCEPT setup.json (the pool setup
// state applySetup writes; deleting it would break future `agentchute update`/
// resync).
func wipeStateCategory(loopDir string) (wipeCategory, error) {
	dir := filepath.Join(loopDir, "state")
	entries, err := wipeReadDir(dir)
	if err != nil {
		return wipeCategory{}, err
	}
	cat := wipeCategory{Name: "state", Parent: dir}
	for _, e := range entries {
		name := e.Name()
		if name == "setup.json" {
			cat.Preserved = append(cat.Preserved, name)
			continue
		}
		cat.Targets = append(cat.Targets, filepath.Join(dir, name))
	}
	sort.Strings(cat.Targets)
	sort.Strings(cat.Preserved)
	return cat, nil
}

// wipeRootLeftoverCategory targets scratch-* dirs and runtime logs/sockets at the
// loop ROOT. It skips the runtime subdirs (handled by their own categories) and
// the scaffold (README.md/.gitignore/AGENTCHUTE.md); any OTHER unrecognized
// root entry is preserved (and reported), never deleted.
func wipeRootLeftoverCategory(loopDir string) (wipeCategory, error) {
	entries, err := wipeReadDir(loopDir)
	if err != nil {
		return wipeCategory{}, err
	}
	cat := wipeCategory{Name: "loop-root", Parent: loopDir}
	for _, e := range entries {
		name := e.Name()
		switch name {
		case "inbox", "archive", "malformed", "live", "agents", "state",
			"README.md", ".gitignore", "AGENTCHUTE.md":
			continue
		}
		if isWipeRootLeftoverName(name) {
			cat.Targets = append(cat.Targets, filepath.Join(loopDir, name))
		} else {
			cat.Preserved = append(cat.Preserved, name)
		}
	}
	sort.Strings(cat.Targets)
	sort.Strings(cat.Preserved)
	return cat, nil
}

// ---------- legacy namespace handling ----------

// scanLegacyNamespaces inspects the control repo's dotdirs for pre-`.agentchute`
// loops. An allowlisted dotdir (wipeLegacyNamespaceDotdirs) whose <dotdir>/loop
// passes every per-candidate guard is returned for deletion; any OTHER dotdir
// that merely LOOKS like an agentchute loop is returned as a manual-cleanup
// report and never deleted.
func scanLegacyNamespaces(controlRepo string) (legacy []string, manual []string, err error) {
	entries, rerr := os.ReadDir(controlRepo)
	if rerr != nil {
		return nil, nil, fmt.Errorf("scan control repo %q: %w", controlRepo, rerr)
	}
	allow := map[string]bool{}
	for _, d := range wipeLegacyNamespaceDotdirs {
		allow[d] = true
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".") || name == fixedDotDirName {
			continue
		}
		// DirEntry.IsDir() is false for a symlink-to-dir (no following), so a
		// symlinked dotdir is naturally excluded from the dir scan below.
		if !e.IsDir() {
			continue
		}
		loopCandidate := filepath.Join(controlRepo, name, loopDirBaseName)
		if !looksLikeAgentchuteLoop(loopCandidate) {
			continue
		}
		if !allow[name] {
			manual = append(manual, fmt.Sprintf("%s (unknown legacy namespace with an agentchute loop; manual cleanup)", filepath.Join(name, loopDirBaseName)))
			continue
		}
		if reason := validateLegacyCandidate(controlRepo, loopCandidate); reason != "" {
			manual = append(manual, fmt.Sprintf("%s (%s)", filepath.Join(name, loopDirBaseName), reason))
			continue
		}
		legacy = append(legacy, loopCandidate)
	}
	sort.Strings(legacy)
	sort.Strings(manual)
	return legacy, manual, nil
}

// validateLegacyCandidate runs the full per-candidate guard. Returns "" when the
// candidate is safe to RemoveAll, or a human reason when it must be skipped.
func validateLegacyCandidate(controlRepo, loopCandidate string) string {
	abs, err := filepath.Abs(filepath.Clean(loopCandidate))
	if err != nil {
		return "unresolvable path"
	}
	rel, err := filepath.Rel(controlRepo, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "not under the control repo"
	}
	if filepath.Base(abs) != loopDirBaseName {
		return "basename is not \"loop\""
	}
	parentDot := filepath.Base(filepath.Dir(abs))
	if parentDot == fixedDotDirName {
		return "parent is the canonical .agentchute"
	}
	// No symlink on the dotdir or the loop dir itself.
	if err := rejectWipeSymlinkDir(filepath.Dir(abs), true); err != nil {
		return "namespace dir is a symlink or not a directory"
	}
	if err := rejectWipeSymlinkDir(abs, true); err != nil {
		return "loop dir is a symlink or not a directory"
	}
	if !looksLikeAgentchuteLoop(abs) {
		return "missing agentchute loop sentinel"
	}
	return ""
}

// looksLikeAgentchuteLoop is the legacy-loop sentinel: the dir must contain an
// agents/ AND an inbox/ subdir AND a README.md whose text names agentchute. This
// is deliberately strict so an unrelated `.something/loop` is never mistaken for
// a coordination loop.
func looksLikeAgentchuteLoop(dir string) bool {
	if !isRealDir(filepath.Join(dir, "agents")) || !isRealDir(filepath.Join(dir, "inbox")) {
		return false
	}
	data, err := readFileLimited(filepath.Join(dir, "README.md"), 64<<10)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "agentchute")
}

func isRealDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir()
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// ---------- execution ----------

// safeRemove deletes path WITHOUT ever following a symlink: a symlink or
// non-directory entry is unlinked (os.Remove); a real directory is recursively
// removed. An absent path is a no-op.
func safeRemove(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// executeWipePlan performs the deletions. Each runtime target is re-checked with
// wipePathApproved (defense-in-depth) before removal. If deletion starts and
// then fails, it collects the remaining/failed paths and returns a non-zero
// error naming them.
func executeWipePlan(plan wipePlan) error {
	var failed []string
	del := func(path string) {
		if !wipePathApproved(plan.LoopDir, path) {
			failed = append(failed, fmt.Sprintf("%s (rejected: not under an approved runtime dir)", path))
			return
		}
		if err := safeRemove(path); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", path, err))
		}
	}
	for _, cat := range plan.Categories {
		for _, t := range cat.Targets {
			del(t)
		}
	}
	for _, d := range plan.LegacyDirs {
		// TOCTOU re-check: re-validate the legacy candidate right before removal.
		if reason := validateLegacyCandidate(plan.ControlRepo, d); reason != "" {
			failed = append(failed, fmt.Sprintf("%s (legacy re-check failed: %s)", d, reason))
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", d, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("deletion failed for %d target(s); remaining: %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// recreateWipeDirs (re)creates the expected loop subdirs at 0700.
func recreateWipeDirs(loopDir string) error {
	for _, sub := range wipeRecreateDirs {
		if err := loop.EnsurePrivateDir(filepath.Join(loopDir, sub)); err != nil {
			return fmt.Errorf("recreate %s: %w", sub, err)
		}
	}
	return nil
}

// rescanWipeLeftovers re-scans for runtime entries that REappeared after the
// wipe — evidence that a process recreated state mid-wipe. Returns the offending
// relative paths (empty == clean).
func rescanWipeLeftovers(loopDir string) []string {
	var leftovers []string
	for _, name := range []string{"inbox", "archive", "malformed", "live"} {
		entries, _ := os.ReadDir(filepath.Join(loopDir, name))
		for _, e := range entries {
			leftovers = append(leftovers, filepath.Join(name, e.Name()))
		}
	}
	stateEntries, _ := os.ReadDir(filepath.Join(loopDir, "state"))
	for _, e := range stateEntries {
		if e.Name() == "setup.json" {
			continue
		}
		leftovers = append(leftovers, filepath.Join("state", e.Name()))
	}
	sort.Strings(leftovers)
	return leftovers
}

// ---------- live-bus refusal ----------

// scanWipeLiveSignals is the READ-ONLY live-bus detector. It returns refusal
// reasons (empty == safe to wipe) for: a still-alive local pool poller/runner,
// an ambiguous local process recorded for this pool whose cmdline can't be
// verified (FAIL CLOSED), an active local wrapper session, and any FRESH
// presence (.live) or serve.claim owned by ANOTHER HOST.
func scanWipeLiveSignals(cfg *loop.Config, agentIDs []string) []string {
	var reasons []string
	now := time.Now().UTC()
	localHost := strings.TrimSpace(localHostname())

	for _, id := range agentIDs {
		if hb, err := loop.LoadPollerHeartbeat(cfg, id); err == nil {
			if setupLocalHost(hb.Host) && hb.PID > 0 && setupProcessAlive(hb.PID) {
				cmdline := setupProcessCommandLine(hb.PID)
				if setupCommandMatches(cmdline, id, "poller run", cfg) {
					reasons = append(reasons, fmt.Sprintf("live poller for %s (pid=%d) is still running; stop it before wiping", id, hb.PID))
				} else {
					reasons = append(reasons, fmt.Sprintf("ambiguous live process pid=%d recorded as poller for %s (cmdline did not match this pool); refusing (fail closed)", hb.PID, id))
				}
			}
		}
		if st, err := loop.LoadRunnerState(cfg, id); err == nil {
			if setupLocalHost(st.Host) && st.RunnerPID > 0 && setupProcessAlive(st.RunnerPID) {
				cmdline := setupProcessCommandLine(st.RunnerPID)
				// Runner attribution: runner.json binds this pid to <id>, the pid is
				// alive, and the cmdline is an `agentchute serve` for THIS pool. A runner
				// has NO --as (contextual id), so we must NOT require the agent id in the
				// cmdline — doing so reported every live runner as ambiguous. The poller
				// case below keeps the agent-id check (pollers DO carry --as).
				if setupCommandMatchesRunnerPool(cmdline, cfg) {
					reasons = append(reasons, fmt.Sprintf("live runner for %s (pid=%d) is still running; stop it before wiping", id, st.RunnerPID))
				} else {
					reasons = append(reasons, fmt.Sprintf("ambiguous live process pid=%d recorded as runner for %s (cmdline did not match this pool); refusing (fail closed)", st.RunnerPID, id))
				}
			}
		}
		if sess, err := loop.LoadActiveSession(cfg, id); err == nil {
			if alive, reason := activeSessionAliveAtWithReason(sess, now); alive {
				reasons = append(reasons, fmt.Sprintf("active wrapper session for %s (%s); close or restart it before wiping", id, reason))
			}
		}
	}

	reasons = append(reasons, scanWipeLivePresence(cfg, localHost, now)...)
	reasons = append(reasons, scanForeignWipeServeClaims(cfg, localHost, now)...)
	sort.Strings(reasons)
	return reasons
}

// scanWipeLivePresence refuses on any FRESH .live presence fact that signals a
// genuinely-live agent on this bus: a fresh fact owned by ANOTHER HOST (a shared
// cross-host bus must not be wiped from one host), OR a fresh local fact whose
// PID is still alive (a live agent — possibly outside the configured pool). A
// fresh-but-dead local fact (e.g. a pool agent we just stopped, whose .live is
// still inside the freshness window) does NOT block the wipe, because its PID is
// no longer alive.
func scanWipeLivePresence(cfg *loop.Config, localHost string, now time.Time) []string {
	var out []string
	entries, _ := os.ReadDir(filepath.Join(cfg.LoopDir, "live"))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".live") {
			continue
		}
		id := strings.TrimSuffix(name, ".live")
		live, err := loop.ReadLive(cfg, id)
		if err != nil {
			continue
		}
		age := now.Sub(live.LastSeen.UTC())
		if age < 0 {
			age = 0
		}
		if age >= loop.LiveWindow() {
			continue
		}
		h := strings.TrimSpace(live.Host)
		if h != "" && localHost != "" && h != localHost {
			out = append(out, fmt.Sprintf("fresh presence for %s on another host %q; refusing to wipe a shared live bus", id, h))
			continue
		}
		if (h == "" || localHost == "" || h == localHost) && live.PID > 0 && setupProcessAlive(live.PID) {
			out = append(out, fmt.Sprintf("fresh presence for %s from a live local agent (pid=%d); stop it before wiping", id, live.PID))
		}
	}
	return out
}

// scanForeignWipeServeClaims refuses on any FRESH serve.claim held by another
// host (the lease owner of an id lives elsewhere).
func scanForeignWipeServeClaims(cfg *loop.Config, localHost string, now time.Time) []string {
	var out []string
	entries, _ := os.ReadDir(filepath.Join(cfg.LoopDir, "state"))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if loop.ValidateAgentID(id) != nil {
			continue
		}
		claim, err := loop.ReadServeClaim(cfg, id)
		if err != nil {
			continue
		}
		if loop.ClaimIsStale(claim, now) {
			continue
		}
		h := strings.TrimSpace(claim.Host)
		if h != "" && localHost != "" && h != localHost {
			out = append(out, fmt.Sprintf("fresh serve claim for %s held by another host %q; refusing to wipe a shared live bus", id, h))
		}
	}
	return out
}

// wipeStopLocalProcesses SIGTERMs this pool's local pollers/runners (reusing the
// soft-reset stop helpers, which only signal a cmdline-verified process) and
// then waits up to wipeProcessWait for the recorded PIDs to exit. Returns the
// ids stopped and any warnings, for the plan printout.
func wipeStopLocalProcesses(cfg *loop.Config, agentIDs []string) (stopped, warnings []string) {
	for _, id := range agentIDs {
		if ok, warn := stopSetupPoller(cfg, id); warn != "" {
			warnings = append(warnings, warn)
		} else if ok {
			stopped = append(stopped, id+" (poller)")
		}
		if ok, warn := stopSetupRunner(cfg, id); warn != "" {
			warnings = append(warnings, warn)
		} else if ok {
			stopped = append(stopped, id+" (runner)")
		}
	}
	deadline := time.Now().Add(wipeProcessWait)
	for time.Now().Before(deadline) {
		if wipeRecordedPIDsDead(cfg, agentIDs) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	sort.Strings(stopped)
	sort.Strings(warnings)
	return stopped, warnings
}

// wipeRecordedPIDsDead reports whether every local poller/runner PID recorded
// for these ids is no longer alive.
func wipeRecordedPIDsDead(cfg *loop.Config, agentIDs []string) bool {
	for _, id := range agentIDs {
		if hb, err := loop.LoadPollerHeartbeat(cfg, id); err == nil {
			if setupLocalHost(hb.Host) && hb.PID > 0 && setupProcessAlive(hb.PID) {
				return false
			}
		}
		if st, err := loop.LoadRunnerState(cfg, id); err == nil {
			if setupLocalHost(st.Host) && st.RunnerPID > 0 && setupProcessAlive(st.RunnerPID) {
				return false
			}
		}
	}
	return true
}

// ---------- plan printing ----------

func printWipePlan(w io.Writer, plan wipePlan) {
	fmt.Fprintln(w, "wipe-state plan (DESTRUCTIVE):")
	fmt.Fprintf(w, "  control repo: %s\n", plan.ControlRepo)
	fmt.Fprintf(w, "  loop dir:     %s\n", plan.LoopDir)
	total := 0
	for _, cat := range plan.Categories {
		total += len(cat.Targets)
		if len(cat.Targets) == 0 && len(cat.Preserved) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s/ (%s): delete %d entr%s\n", cat.Name, cat.Parent, len(cat.Targets), plural(len(cat.Targets)))
		for _, t := range cat.Targets {
			fmt.Fprintf(w, "      - %s\n", filepath.Base(t))
		}
		if len(cat.Preserved) > 0 {
			fmt.Fprintf(w, "      preserved: %s\n", strings.Join(cat.Preserved, ", "))
		}
	}
	if len(plan.LegacyDirs) > 0 {
		fmt.Fprintf(w, "  legacy namespace loops (RemoveAll): %d\n", len(plan.LegacyDirs))
		for _, d := range plan.LegacyDirs {
			fmt.Fprintf(w, "      - %s\n", d)
		}
		total += len(plan.LegacyDirs)
	}
	if len(plan.ManualCleanup) > 0 {
		fmt.Fprintln(w, "  manual cleanup (NOT deleted):")
		for _, m := range plan.ManualCleanup {
			fmt.Fprintf(w, "      - %s\n", m)
		}
	}
	fmt.Fprintf(w, "  total delete targets: %d\n", total)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// ---------- orchestration ----------

// setupRunWipeState is the destructive phase invoked LAST in applySetup when
// --wipe-state is set. By this point every recoverable setup write (including
// state/setup.json) has landed, so this only removes runtime state. It is a
// package var so applySetup can call it and tests stay flexible.
var setupRunWipeState = func(root string, cfg *loop.Config, wrappers []string, opts setupOptions) error {
	if err := validateWipePaths(root, cfg.LoopDir); err != nil {
		return fmt.Errorf("wipe-state: %w", err)
	}

	agentIDs, idWarnings := setupResetAgentIDs(root, cfg, wrappers)
	for _, warn := range idWarnings {
		fmt.Printf("warning: wipe-state: %s\n", warn)
	}

	stopped, stopWarnings := wipeStopLocalProcesses(cfg, agentIDs)
	if len(stopped) > 0 {
		fmt.Printf("stopped %d local process(es): %s\n", len(stopped), strings.Join(stopped, ", "))
	}
	for _, warn := range stopWarnings {
		fmt.Printf("warning: wipe-state: %s\n", warn)
	}

	if reasons := scanWipeLiveSignals(cfg, agentIDs); len(reasons) > 0 {
		return fmt.Errorf("wipe-state: refusing to wipe a live bus:\n  - %s", strings.Join(reasons, "\n  - "))
	}

	plan, err := computeWipePlan(cfg, root)
	if err != nil {
		return fmt.Errorf("wipe-state: %w", err)
	}
	printWipePlan(os.Stdout, plan)

	// Clean-all (v0.8.8): compute + print the stale-install-artifact audit BEFORE
	// the single destructive confirm, so the operator approves the binary-backup
	// removals together with the loop wipe. The removals are APPLIED below, after
	// the confirm and the post-confirm live-bus rescan.
	cleanIn := resolveCleanInputs(cfg, root, agentIDs)
	cleanAudit := computeCleanPlan(cleanIn)
	printCleanPlan(os.Stdout, cleanAudit)

	if !opts.Yes {
		ok, err := promptSetupConfirm("\nWipe the runtime state above? This is DESTRUCTIVE and cannot be undone. [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("wipe-state: aborted by operator")
		}
	}

	// RESCAN live signals after the confirm window, before any deletion.
	if reasons := scanWipeLiveSignals(cfg, agentIDs); len(reasons) > 0 {
		return fmt.Errorf("wipe-state: refusing to wipe; a live signal appeared during confirmation:\n  - %s", strings.Join(reasons, "\n  - "))
	}

	if err := executeWipePlan(plan); err != nil {
		return fmt.Errorf("wipe-state: %w", err)
	}
	if err := recreateWipeDirs(cfg.LoopDir); err != nil {
		return fmt.Errorf("wipe-state: %w", err)
	}
	if leftovers := rescanWipeLeftovers(cfg.LoopDir); len(leftovers) > 0 {
		return fmt.Errorf("wipe-state: runtime files reappeared during the wipe (a live process may be writing): %s", strings.Join(leftovers, ", "))
	}

	// Apply the guarded clean-all removals (re-checks every guard before each
	// unlink; fails closed). Report-only items (orphans, PATH shadows, out-of-root
	// backups) were already printed above and are never acted on.
	if removed, err := applyCleanPlan(cleanAudit, cleanIn.CurrentUID, false); err != nil {
		return fmt.Errorf("wipe-state: %w", err)
	} else if len(removed) > 0 {
		fmt.Printf("clean-all: removed %d stale install artifact(s)\n", len(removed))
	}

	fmt.Println("wipe-state: runtime state wiped; preserved scaffold (README.md, *.example.md) and state/setup.json.")
	return nil
}

// printWipeStateDryRun prints the wipe plan for `setup --reset --wipe-state
// --dry-run`. It MUTATES NOTHING: it discovers the loop (best-effort — a fresh
// setup has no loop yet), validates the paths, prints the delete plan, and runs
// the READ-ONLY live-bus detector (without stopping anything).
func printWipeStateDryRun(w io.Writer, root string) {
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: root,
		Cwd:             root,
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		fmt.Fprintln(w, "\nwipe-state plan: no existing loop dir discovered yet; a fresh setup creates an empty loop, so there is nothing to wipe.")
		return
	}
	if err := validateWipePaths(root, cfg.LoopDir); err != nil {
		fmt.Fprintf(w, "\nwipe-state plan: cannot compute (%v)\n", err)
		return
	}
	plan, err := computeWipePlan(cfg, root)
	if err != nil {
		fmt.Fprintf(w, "\nwipe-state plan: cannot compute (%v)\n", err)
		return
	}
	fmt.Fprintln(w, "\n(the destructive wipe-state phase would run AFTER setup completes)")
	printWipePlan(w, plan)

	agentIDs, _ := setupResetAgentIDs(root, cfg, nil)
	if reasons := scanWipeLiveSignals(cfg, agentIDs); len(reasons) > 0 {
		fmt.Fprintln(w, "  live-bus check would REFUSE the wipe:")
		for _, r := range reasons {
			fmt.Fprintf(w, "      - %s\n", r)
		}
	} else {
		fmt.Fprintln(w, "  live-bus check: no live local or foreign-host signals detected (re-verified at apply time).")
	}

	// Clean-all audit (read-only report; the destructive removals run only on apply).
	printCleanPlan(w, computeCleanPlan(resolveCleanInputs(cfg, root, agentIDs)))
}
