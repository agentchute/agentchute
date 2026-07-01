package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// setup_clean.go — the v0.8.8 clean-all AUDIT + guarded removal, layered on top
// of the --wipe-state loop wipe (setup_wipe.go). The wipe clears loop RUNTIME
// state; clean-all additionally audits STALE INSTALL ARTIFACTS that accumulate
// across upgrades — agentchute binary backups, orphaned run/poller processes,
// and a PATH that shadows the installed binary.
//
// This is the HIGHEST-RISK surface (destructive file removal), so it is
// conservative by construction:
//   - The audit planner (computeCleanPlan) is PURE: it classifies every candidate
//     as remove|report and MUTATES NOTHING. It is the unit-tested core.
//   - Only OWNED REGULAR FILES (no symlink, current user) UNDER an ALLOWLISTED
//     ROOT (install dir, control repo, canonical loop dir) are ever classified
//     `remove`. Everything else — backups outside those roots, symlinks,
//     foreign-owned files, orphaned processes, PATH shadows — is REPORT ONLY.
//   - clean-all NEVER kills a process and NEVER edits PATH; those are report-only.
//   - The apply step (applyCleanPlan) re-checks every guard immediately before
//     each unlink (TOCTOU) and FAILS CLOSED on anything that no longer passes.
//   - It runs only inside the already-guarded --wipe-state phase, AFTER the
//     live-bus refusal and a single operator confirm, so a live bus is never
//     cleaned and removal honors --yes / the interactive confirm.

type cleanAction string

const (
	cleanRemove cleanAction = "remove"
	cleanReport cleanAction = "report"
)

type cleanClass string

const (
	cleanClassBinaryBackup  cleanClass = "binary-backup"
	cleanClassOrphanProcess cleanClass = "orphan-process"
	cleanClassPathShadow    cleanClass = "path-shadow"
)

// cleanItem is one classified clean-all candidate. PURE data produced by
// computeCleanPlan; nothing is acted on until applyCleanPlan.
type cleanItem struct {
	Class  cleanClass
	Action cleanAction
	Path   string // file path (binary-backup / path-shadow); "" for process items
	PID    int    // process id (orphan-process); 0 otherwise
	Reason string
}

// cleanPlan is the fully-classified, pure (no-mutation) clean-all audit.
type cleanPlan struct {
	AllowedRoots []string
	Items        []cleanItem
}

// RemoveItems returns the subset classified for auto-removal.
func (p cleanPlan) RemoveItems() []cleanItem {
	var out []cleanItem
	for _, it := range p.Items {
		if it.Action == cleanRemove {
			out = append(out, it)
		}
	}
	return out
}

// cleanProcess is a live `agentchute run`/`agentchute poller run` process for
// THIS pool, already cmdline-matched by the FIXED matcher (Task 1).
type cleanProcess struct {
	PID  int
	Kind string // "runner" | "poller"
}

// cleanAuditInputs are the resolved, side-effect-free inputs to the PURE
// clean-all audit planner. The production caller (resolveCleanInputs) derives
// these from the environment; tests construct them directly so computeCleanPlan
// is exercised without touching the host.
type cleanAuditInputs struct {
	AllowedRoots []string // roots under which auto-remove is permitted
	ScanDirs     []string // dirs scanned for backup-named files
	CurrentUID   int      // os.Getuid()

	PoolProcesses []cleanProcess // live run/poller for this pool
	BoundPIDs     map[int]bool   // pids bound by a CURRENT runner.json/poller heartbeat

	PathResolved    []string // agentchute paths on PATH in order (type -a agentchute)
	InstalledBinary string   // canonical installed agentchute path (os.Executable)
}

// ---------- name + path matchers ----------

// isAgentchuteBackupName reports whether basename is a stale agentchute binary
// backup: `agentchute.pre-*` (the update/install pre-swap snapshots) or
// `agentchute.*.bak`. The live binary is named exactly `agentchute` (no dot) and
// install.sh's staging file is `.agentchute.tmp.<pid>` (leading dot) — neither
// matches, so a live or in-flight binary is never classified a backup.
func isAgentchuteBackupName(name string) bool {
	if !strings.HasPrefix(name, "agentchute.") {
		return false
	}
	if strings.HasPrefix(name, "agentchute.pre-") {
		return true
	}
	return strings.HasSuffix(name, ".bak")
}

// pathUnderRoot reports whether target is at or below root with no ".." escape.
func pathUnderRoot(target, root string) bool {
	target = filepath.Clean(target)
	root = filepath.Clean(root)
	if root == "" || target == "" {
		return false
	}
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// pathUnderAnyAllowedRoot returns the first allowlisted root containing target
// (abs-resolved) and true, or "" and false when target is outside every root.
func pathUnderAnyAllowedRoot(target string, roots []string) (string, bool) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", false
	}
	for _, r := range roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		ra, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if pathUnderRoot(abs, ra) {
			return ra, true
		}
	}
	return "", false
}

// ---------- pure audit planner ----------

// computeCleanPlan is the PURE clean-all audit: it scans for stale install
// artifacts and classifies each as remove|report. It MUTATES NOTHING (it reads
// the filesystem and the supplied process/PATH facts only). The classification
// is the testable core; applyCleanPlan acts on the result under the same guards.
func computeCleanPlan(in cleanAuditInputs) cleanPlan {
	plan := cleanPlan{}

	// Allowlisted roots: abs, de-duplicated, sorted.
	rootSeen := map[string]bool{}
	for _, r := range in.AllowedRoots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil || rootSeen[abs] {
			continue
		}
		rootSeen[abs] = true
		plan.AllowedRoots = append(plan.AllowedRoots, abs)
	}
	sort.Strings(plan.AllowedRoots)

	// ---- stale binary backups ----
	fileSeen := map[string]bool{}
	for _, dir := range in.ScanDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a missing/unreadable scan dir is not a clean-all failure
		}
		for _, e := range entries {
			if !isAgentchuteBackupName(e.Name()) {
				continue
			}
			abs, err := filepath.Abs(filepath.Join(dir, e.Name()))
			if err != nil || fileSeen[abs] {
				continue
			}
			fileSeen[abs] = true
			plan.Items = append(plan.Items, classifyBackup(abs, plan.AllowedRoots, in.CurrentUID))
		}
	}

	// ---- orphaned run/poller processes (REPORT ONLY) ----
	for _, p := range in.PoolProcesses {
		if p.PID <= 0 || in.BoundPIDs[p.PID] {
			continue
		}
		plan.Items = append(plan.Items, cleanItem{
			Class:  cleanClassOrphanProcess,
			Action: cleanReport,
			PID:    p.PID,
			Reason: fmt.Sprintf("orphaned agentchute %s process (pid=%d) for this pool with no current runner/poller state; stop it manually — clean-all never kills a process", p.Kind, p.PID),
		})
	}

	// ---- PATH shadow (REPORT ONLY) ----
	if shadow := cleanPathShadowItem(in.PathResolved, in.InstalledBinary); shadow != nil {
		plan.Items = append(plan.Items, *shadow)
	}

	sort.SliceStable(plan.Items, func(i, j int) bool {
		if plan.Items[i].Class != plan.Items[j].Class {
			return plan.Items[i].Class < plan.Items[j].Class
		}
		if plan.Items[i].Path != plan.Items[j].Path {
			return plan.Items[i].Path < plan.Items[j].Path
		}
		return plan.Items[i].PID < plan.Items[j].PID
	})
	return plan
}

// classifyBackup applies the per-file guards (allowlisted root, no symlink,
// regular file, current-user owner) and returns the classified item. ANY failed
// guard downgrades to report-only — auto-removal requires EVERY guard to pass.
func classifyBackup(abs string, allowedRoots []string, currentUID int) cleanItem {
	it := cleanItem{Class: cleanClassBinaryBackup, Path: abs}
	root, under := pathUnderAnyAllowedRoot(abs, allowedRoots)
	if !under {
		it.Action = cleanReport
		it.Reason = "stale agentchute binary backup OUTSIDE the allowlisted roots (install dir / control repo / loop dir); manual cleanup only"
		return it
	}
	info, err := os.Lstat(abs) // Lstat: never follow a symlink here.
	if err != nil {
		it.Action = cleanReport
		it.Reason = fmt.Sprintf("stale agentchute binary backup could not be inspected (%v); not auto-removed", err)
		return it
	}
	if info.Mode()&os.ModeSymlink != 0 {
		it.Action = cleanReport
		it.Reason = "stale agentchute binary backup is a SYMLINK; not auto-removed (a link could redirect deletion out of the allowlisted root)"
		return it
	}
	if !info.Mode().IsRegular() {
		it.Action = cleanReport
		it.Reason = "stale agentchute binary backup is not a regular file; not auto-removed"
		return it
	}
	if uid, ok := fileOwnerUID(info); !ok || uid != currentUID {
		it.Action = cleanReport
		it.Reason = fmt.Sprintf("stale agentchute binary backup is not owned by the current user (uid=%d); not auto-removed", currentUID)
		return it
	}
	it.Action = cleanRemove
	it.Reason = fmt.Sprintf("stale agentchute binary backup under %s; remove", root)
	return it
}

// cleanPathShadowItem reports (never removes/edits) a PATH that resolves
// `agentchute` to a different binary than the installed one. Returns nil when
// PATH is clean (the install resolves first) or the facts are unavailable.
func cleanPathShadowItem(resolved []string, installed string) *cleanItem {
	installed = strings.TrimSpace(installed)
	if installed == "" || len(resolved) == 0 {
		return nil
	}
	first := strings.TrimSpace(resolved[0])
	if first == "" || samePath(first, installed) {
		return nil
	}
	return &cleanItem{
		Class:  cleanClassPathShadow,
		Action: cleanReport,
		Path:   first,
		Reason: fmt.Sprintf("PATH resolves `agentchute` to %s, which shadows the installed %s; fix PATH order so the install dir precedes it — clean-all never edits PATH", first, installed),
	}
}

// ---------- guarded apply ----------

// applyCleanPlan removes the remove-classified binary backups, RE-CHECKING every
// guard immediately before each unlink (TOCTOU) and FAILING CLOSED on anything
// that no longer passes. dryRun lists what WOULD be removed and mutates nothing.
// It never touches report-only items (orphan processes, PATH shadows, out-of-root
// / unowned / symlinked backups). Returns the removed paths (or, in dry-run, the
// paths that would be removed) and a non-nil error if any removal failed/refused.
func applyCleanPlan(plan cleanPlan, currentUID int, dryRun bool) ([]string, error) {
	var done, failed []string
	for _, it := range plan.Items {
		if it.Class != cleanClassBinaryBackup || it.Action != cleanRemove {
			continue
		}
		// TOCTOU re-check against the SAME allowlisted roots + owner the plan used.
		if recheck := classifyBackup(it.Path, plan.AllowedRoots, currentUID); recheck.Action != cleanRemove {
			failed = append(failed, fmt.Sprintf("%s (guard re-check failed: %s)", it.Path, recheck.Reason))
			continue
		}
		if dryRun {
			done = append(done, it.Path)
			continue
		}
		// os.Remove (never RemoveAll): the re-check proved a regular file; this can
		// only unlink a single file, never recurse a directory.
		if err := os.Remove(it.Path); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", it.Path, err))
			continue
		}
		done = append(done, it.Path)
	}
	sort.Strings(done)
	if len(failed) > 0 {
		return done, fmt.Errorf("clean-all: %d backup removal(s) failed/refused: %s", len(failed), strings.Join(failed, "; "))
	}
	return done, nil
}

// ---------- plan printing ----------

func printCleanPlan(w io.Writer, plan cleanPlan) {
	if len(plan.Items) == 0 {
		fmt.Fprintln(w, "clean-all audit: no stale install artifacts found.")
		return
	}
	removeN := len(plan.RemoveItems())
	fmt.Fprintf(w, "clean-all audit: %d auto-removable, %d report-only\n", removeN, len(plan.Items)-removeN)
	for _, it := range plan.Items {
		tag := "report"
		if it.Action == cleanRemove {
			tag = "REMOVE"
		}
		target := it.Path
		if target == "" && it.PID > 0 {
			target = fmt.Sprintf("pid=%d", it.PID)
		}
		fmt.Fprintf(w, "  [%s] %s: %s — %s\n", tag, it.Class, target, it.Reason)
	}
}

// ---------- input resolution (production) ----------

// resolveCleanInputs derives the clean-all audit inputs from the live
// environment: the install dir (dir of the running binary), the configured
// control repo + canonical loop dir as the ONLY allowlisted roots, the scan dirs
// (those roots plus common backup locations whose out-of-root hits become
// report-only), the current uid, this pool's live run/poller processes vs. their
// bound state pids, and the PATH resolution of `agentchute`. Side-effect-free.
func resolveCleanInputs(cfg *loop.Config, controlRepo string, agentIDs []string) cleanAuditInputs {
	in := cleanAuditInputs{CurrentUID: os.Getuid()}

	installDir := ""
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			in.InstalledBinary = abs
			installDir = filepath.Dir(abs)
		}
	}

	// Allowlisted roots: install dir, control repo, canonical loop dir ONLY.
	if installDir != "" {
		in.AllowedRoots = append(in.AllowedRoots, installDir)
	}
	if strings.TrimSpace(controlRepo) != "" {
		in.AllowedRoots = append(in.AllowedRoots, controlRepo)
	}
	if cfg != nil && strings.TrimSpace(cfg.LoopDir) != "" {
		in.AllowedRoots = append(in.AllowedRoots, cfg.LoopDir)
	}

	// Scan dirs: the allowlisted file roots plus common backup locations. A
	// backup found in a scan dir that is NOT under an allowlisted root is
	// classified REPORT ONLY by the planner (e.g. ~/.local/bin when the binary
	// was installed elsewhere).
	if installDir != "" {
		in.ScanDirs = append(in.ScanDirs, installDir)
	}
	if strings.TrimSpace(controlRepo) != "" {
		in.ScanDirs = append(in.ScanDirs, controlRepo)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		in.ScanDirs = append(in.ScanDirs, filepath.Join(home, ".local", "bin"))
	}

	// Orphan processes: matched pool processes whose pid is NOT bound by a
	// current runner.json/poller heartbeat.
	bound := map[int]bool{}
	for _, id := range agentIDs {
		if hb, err := loop.LoadPollerHeartbeat(cfg, id); err == nil && hb.PID > 0 {
			bound[hb.PID] = true
		}
		if st, err := loop.LoadRunnerState(cfg, id); err == nil && st.RunnerPID > 0 {
			bound[st.RunnerPID] = true
		}
	}
	in.BoundPIDs = bound
	in.PoolProcesses = listPoolAgentchuteProcesses(cfg)

	in.PathResolved = resolveAgentchutePathEntries()
	return in
}

// (The compute/print/apply steps are orchestrated inline by setupRunWipeState and
// printWipeStateDryRun so the clean-all shares the wipe's single destructive
// confirm and post-confirm live-bus rescan.)

// listPoolAgentchuteProcesses enumerates live `agentchute run`/`poller run`
// processes whose cmdline matches THIS pool (the FIXED matcher from Task 1).
// Package var so the wipe orchestration stays testable without shelling out.
var listPoolAgentchuteProcesses = defaultListPoolAgentchuteProcesses

func defaultListPoolAgentchuteProcesses(cfg *loop.Config) []cleanProcess {
	if cfg == nil {
		return nil
	}
	if _, err := exec.LookPath("ps"); err != nil {
		return nil
	}
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var procs []cleanProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		cmd := strings.Join(fields[1:], " ")
		switch {
		case setupCommandMatchesRunnerPool(cmd, cfg):
			procs = append(procs, cleanProcess{PID: pid, Kind: "runner"})
		case setupCommandMatchesPool(cmd, "poller run", cfg):
			procs = append(procs, cleanProcess{PID: pid, Kind: "poller"})
		}
	}
	return procs
}

// resolveAgentchutePathEntries returns every `agentchute` resolvable on $PATH,
// in PATH order, symlink-resolved and de-duplicated (the `type -a agentchute`
// equivalent in pure Go; no shell). Read-only.
func resolveAgentchutePathEntries() []string {
	pathEnv := os.Getenv("PATH")
	if strings.TrimSpace(pathEnv) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(pathEnv) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		cand := filepath.Join(dir, "agentchute")
		info, err := os.Stat(cand)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		resolved := cand
		if rp, err := filepath.EvalSymlinks(cand); err == nil {
			resolved = rp
		}
		if abs, err := filepath.Abs(resolved); err == nil {
			resolved = abs
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}
