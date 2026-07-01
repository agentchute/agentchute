package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// preparePoolTarget aggregates the plan for one target folder.
type preparePoolTarget struct {
	Target          string       // absolute path to the target folder
	NotGit          bool         // surfaced as a warning per codex's round-1 ask
	PointerAction   initAction   // write/replace/skip/error for .agentchute-control-repo
	WrapperActions  []initAction // CLAUDE.md / CODEX.md / GEMINI.md / AGENTS.md
	GitignoreAction *initAction  // populated only when --update-gitignore is set
}

// preparePoolPlan is the full multi-target preparation plan.
type preparePoolPlan struct {
	ControlRepo      string
	LoopDir          string
	Vendor           string
	UpdateGitignore  bool
	ReplacePointer   bool
	Targets          []preparePoolTarget
	EnrollmentBlocks map[string]string // filename -> rendered block, shown to user
	GitignoreStanza  string            // shared stanza text when --update-gitignore
}

// repoListFlag is reused from register.go.

func cmdPreparePool(args []string) error {
	fs := flag.NewFlagSet("prepare-pool", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var controlRepoFlag string
	var dryRun, yes, replacePointer, updateGitignore bool
	var targets repoListFlag

	fs.Var(&targets, "target", "target folder to prepare as a pool participant (repeatable)")
	fs.StringVar(&controlRepoFlag, "control-repo", "", "control repo the targets will point at (or AGENTCHUTE_CONTROL_REPO)")
	fs.BoolVar(&dryRun, "dry-run", false, "print plan and exit without making changes")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt and apply the plan")
	fs.BoolVar(&replacePointer, "replace-pointer", false, "overwrite an existing .agentchute-control-repo pointer that points elsewhere")
	fs.BoolVar(&updateGitignore, "update-gitignore", false, "append the agentchute gitignore stanza at each target (default off)")

	if err := fs.Parse(args); err != nil {
		return preparePoolUsage(err)
	}
	if fs.NArg() != 0 {
		return preparePoolUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if len(targets) == 0 {
		return preparePoolUsage(fmt.Errorf("at least one --target is required"))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepoFlag,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return fmt.Errorf("resolve control repo: %w", err)
	}

	plan, err := computePreparePoolPlan(cfg, targets, replacePointer, updateGitignore)
	if err != nil {
		return err
	}

	printPreparePoolPlan(os.Stdout, plan)

	if dryRun {
		fmt.Println("\n(dry-run; no changes made)")
		return nil
	}

	if !preparePoolHasMutations(plan) {
		fmt.Println("\nnothing to do.")
		return nil
	}

	if !yes {
		ok, err := promptConfirm(os.Stdin, os.Stdout, "\nApply all changes? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil
		}
	}

	applied, err := applyPreparePoolPlan(plan)
	if err != nil {
		return err
	}
	fmt.Printf("\napplied %d change(s) across %d target(s).\n", applied, len(plan.Targets))
	return nil
}

func preparePoolUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute prepare-pool --target <folder> [--target <folder>...] [--control-repo <path>] [--replace-pointer] [--update-gitignore] [--dry-run] [--yes]", err)
}

// computePreparePoolPlan builds the plan without doing any I/O writes. It
// validates every target up-front (codex's "preflight before any write"
// requirement); any hard error here aborts before a single byte is written.
func computePreparePoolPlan(cfg *loop.Config, targetPaths []string, replacePointer, updateGitignore bool) (preparePoolPlan, error) {
	plan := preparePoolPlan{
		ControlRepo:      cfg.ControlRepo,
		LoopDir:          cfg.LoopDir,
		Vendor:           cfg.Vendor,
		ReplacePointer:   replacePointer,
		UpdateGitignore:  updateGitignore,
		EnrollmentBlocks: make(map[string]string),
		GitignoreStanza:  renderGitignoreStanza(cfg.Vendor),
	}
	for _, wt := range wrapperTargets {
		plan.EnrollmentBlocks[wt.Filename] = renderWrapperBlock(wt.AsID, wt.Vendor)
	}

	// Render the AGENTS.md block once; reused per-target.
	agentsRendered := strings.ReplaceAll(enrollmentAgentsTemplate, "\r\n", "\n")
	plan.EnrollmentBlocks["AGENTS.md"] = agentsRendered

	for _, raw := range targetPaths {
		t, err := computeTargetPlan(cfg, raw, replacePointer, updateGitignore, plan.GitignoreStanza)
		if err != nil {
			return preparePoolPlan{}, fmt.Errorf("target %q: %w", raw, err)
		}
		plan.Targets = append(plan.Targets, t)
	}
	return plan, nil
}

// computeTargetPlan builds the per-target plan, validating the target up-front.
// Hard errors at this stage abort the entire prepare-pool run before any write.
func computeTargetPlan(cfg *loop.Config, raw string, replacePointer, updateGitignore bool, gitignoreStanza string) (preparePoolTarget, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return preparePoolTarget{}, fmt.Errorf("abs: %w", err)
	}
	// Lstat first so symlinked targets are rejected before we follow them.
	// Codex's POOL-2 ask: prepare-pool's other filesystem ops are cautious
	// about symlinks (loop dirs, registrations), so writes targeted via
	// symlink would land at the symlink's destination, not at the operator's
	// intended target. Reject explicitly.
	info, err := os.Lstat(abs)
	if err != nil {
		return preparePoolTarget{}, fmt.Errorf("lstat: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return preparePoolTarget{}, fmt.Errorf("target is a symlink; refusing to follow (resolve the symlink yourself and pass the canonical path)")
	}
	if !info.IsDir() {
		return preparePoolTarget{}, fmt.Errorf("not a directory")
	}

	// Hard error if target IS the control repo itself.
	absControl, _ := filepath.Abs(cfg.ControlRepo)
	if abs == absControl {
		return preparePoolTarget{}, fmt.Errorf("target is the control repo itself; use `agentchute init` to manage the control repo, not prepare-pool")
	}

	t := preparePoolTarget{Target: abs}

	// Hard error if target already has its own vendor loop directory (it's an
	// active control repo, not a participant). Suggest cleanup.
	if hasAnyVendorLoopDir(abs) {
		return t, fmt.Errorf("target already contains a vendor loop directory (looks like its own control repo); refusing to convert it into a participant. Remove the loop directory or pick a different target")
	}

	// Plan the pointer file.
	pointerAction, err := planPointerFile(abs, cfg.ControlRepo, replacePointer)
	if err != nil {
		return preparePoolTarget{}, err
	}
	t.PointerAction = pointerAction

	// Plan the wrapper files (reuse init's per-file planner).
	for _, wt := range wrapperTargets {
		rendered := renderWrapperBlock(wt.AsID, wt.Vendor)
		action, err := planEnrollmentFile(filepath.Join(abs, wt.Filename), rendered)
		if err != nil {
			return preparePoolTarget{}, fmt.Errorf("plan %s: %w", wt.Filename, err)
		}
		t.WrapperActions = append(t.WrapperActions, action)
	}
	// AGENTS.md uses the agents-template (with the wrapper-to-id table).
	agentsRendered := strings.ReplaceAll(enrollmentAgentsTemplate, "\r\n", "\n")
	agentsAction, err := planEnrollmentFile(filepath.Join(abs, "AGENTS.md"), agentsRendered)
	if err != nil {
		return preparePoolTarget{}, fmt.Errorf("plan AGENTS.md: %w", err)
	}
	t.WrapperActions = append(t.WrapperActions, agentsAction)

	// Gitignore plan only when explicitly requested.
	if updateGitignore {
		// Detect whether target is a git repo for the gitignore planner.
		inGit := isGitRepo(abs)
		t.NotGit = !inGit
		gitignoreAction, err := planGitignore(abs, inGit, gitignoreStanza)
		if err != nil {
			return preparePoolTarget{}, fmt.Errorf("plan gitignore: %w", err)
		}
		t.GitignoreAction = &gitignoreAction
	} else if !isGitRepo(abs) {
		t.NotGit = true
	}

	return t, nil
}

// planPointerFile decides what to do with .agentchute-control-repo at the
// target. Cases:
//
//   - File missing: write a new pointer (relative path preferred).
//   - File exists, same content: skip (idempotent no-op).
//   - File exists, different content, --replace-pointer NOT set: hard error
//     (refuse to silently redirect an existing participant).
//   - File exists, different content, --replace-pointer SET: replace with
//     "replace pointer" action showing old → new diff.
//   - File exists, malformed (multi-path / empty): hard error regardless of
//     --replace-pointer — operator must fix the existing file by hand.
func planPointerFile(target, controlRepo string, replacePointer bool) (initAction, error) {
	pointerPath := filepath.Join(target, loop.PointerFileName)
	desiredPath := bestPointerPath(target, controlRepo)
	desiredContent := desiredPath + "\n"

	existing, err := os.ReadFile(pointerPath)
	if os.IsNotExist(err) {
		return initAction{
			Target: pointerPath,
			Action: "write pointer",
			Detail: fmt.Sprintf("-> %s", desiredPath),
			Apply: func() error {
				return atomicWritePointer(pointerPath, desiredContent)
			},
		}, nil
	}
	if err != nil {
		return initAction{}, fmt.Errorf("read existing pointer: %w", err)
	}

	parsed, perr := loop.ParsePointerFile(string(existing))
	if perr != nil {
		return initAction{}, fmt.Errorf("existing pointer at %s is malformed: %w; fix it by hand before re-running", pointerPath, perr)
	}

	// Codex's POOL-1 review: compare RESOLVED targets, not raw text. A
	// pointer that spells the same control repo differently (absolute vs
	// relative, different number of `..` segments) is NOT a conflict.
	desiredResolved, dRErr := resolveAndNormalize(target, desiredPath)
	if dRErr != nil {
		// Desired target is fresh; this shouldn't normally fail. If it does,
		// surface the error.
		return initAction{}, fmt.Errorf("resolve desired pointer target %q: %w", desiredPath, dRErr)
	}
	existingResolved, eRErr := resolveAndNormalize(target, parsed)

	if eRErr == nil && existingResolved == desiredResolved {
		// Same control repo. Distinguish "same text" (true no-op) from
		// "different spelling, same target" (which we MAY want to rewrite to
		// the preferred spelling — but only under --replace-pointer, never
		// silently).
		if strings.TrimSpace(parsed) == strings.TrimSpace(desiredPath) {
			return initAction{
				Target: pointerPath,
				Action: "skip pointer",
				Detail: fmt.Sprintf("already points at %s", desiredPath),
			}, nil
		}
		if !replacePointer {
			return initAction{
				Target: pointerPath,
				Action: "skip pointer",
				Detail: fmt.Sprintf("equivalent target (%s ≡ %s); pass --replace-pointer to normalize spelling", parsed, desiredPath),
			}, nil
		}
		return initAction{
			Target: pointerPath,
			Action: "replace pointer",
			Detail: fmt.Sprintf("normalize spelling: %s -> %s (same control repo)", parsed, desiredPath),
			Apply: func() error {
				return atomicWritePointer(pointerPath, desiredContent)
			},
		}, nil
	}

	// Resolved targets differ, or existing pointer's target can't be resolved
	// (broken pointer). Both are conflict cases requiring --replace-pointer.
	if !replacePointer {
		if eRErr != nil {
			return initAction{}, fmt.Errorf("existing pointer at %s is broken (target %q does not resolve: %v); pass --replace-pointer to redirect", pointerPath, parsed, eRErr)
		}
		return initAction{}, fmt.Errorf("existing pointer at %s points at %q (resolves to %s; would redirect to %s); pass --replace-pointer to override", pointerPath, parsed, existingResolved, desiredResolved)
	}
	return initAction{
		Target: pointerPath,
		Action: "replace pointer",
		Detail: fmt.Sprintf("%s -> %s", parsed, desiredPath),
		Apply: func() error {
			return atomicWritePointer(pointerPath, desiredContent)
		},
	}, nil
}

// resolveAndNormalize resolves rawPath against pointerDir (relative paths
// resolve from pointerDir; absolutes are kept) and returns a cleaned
// absolute path suitable for equivalence comparison. The path must exist
// — equivalent to a stat check on the resolved location.
func resolveAndNormalize(pointerDir, rawPath string) (string, error) {
	raw := strings.TrimSpace(rawPath)
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(pointerDir, raw)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s: not a directory", abs)
	}
	// EvalSymlinks for consistent comparison across /tmp ↔ /private/tmp on
	// macOS and any other symlink-mediated locations.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real), nil
	}
	return filepath.Clean(abs), nil
}

// bestPointerPath returns the path string to embed in the pointer file.
// Prefers a relative path when target and control repo share a *meaningful*
// common ancestor (the common multi-repo case); otherwise absolute. A
// meaningful ancestor is one above the filesystem root — sibling repos
// under /Users/alex/code/ share /Users/alex/code/; unrelated trees like
// /tmp/test and /Users/alex/code share only /, which is too coarse to be
// a "project".
func bestPointerPath(target, controlRepo string) string {
	tAbs, err1 := filepath.Abs(target)
	cAbs, err2 := filepath.Abs(controlRepo)
	if err1 != nil || err2 != nil {
		return controlRepo
	}
	common := commonAncestor(tAbs, cAbs)
	// A common ancestor at filesystem root means these paths aren't really
	// in the same project tree. Use absolute.
	if common == "" || common == "/" || common == string(filepath.Separator) {
		return cAbs
	}
	rel, err := filepath.Rel(target, controlRepo)
	if err != nil {
		return controlRepo
	}
	return rel
}

// commonAncestor returns the longest directory prefix shared by a and b.
// Returns "" if there's no shared prefix (different roots on Windows). On
// POSIX returns at minimum "/" when both paths are absolute.
func commonAncestor(a, b string) string {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	aParts := strings.Split(a, string(filepath.Separator))
	bParts := strings.Split(b, string(filepath.Separator))
	i := 0
	for i < len(aParts) && i < len(bParts) && aParts[i] == bParts[i] {
		i++
	}
	if i == 0 {
		return ""
	}
	common := strings.Join(aParts[:i], string(filepath.Separator))
	if common == "" {
		// Both started with separator; the shared root IS the separator.
		return string(filepath.Separator)
	}
	return common
}

// hasAnyVendorLoopDir returns true if the directory contains at least one
// `.<vendor>/loop/` subdirectory — i.e., it's already acting as a control
// repo and shouldn't be made a participant.
func hasAnyVendorLoopDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ".") || name == "." || name == ".." {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, name, "loop")); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// isGitRepo checks whether dir contains a .git entry (file or dir).
// Submodules use a .git file pointing at the parent's .git/modules/... dir.
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// atomicWritePointer writes the pointer file via temp+rename so a crash mid-
// write never leaves a partial pointer file at the canonical name.
func atomicWritePointer(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp_"+filepath.Base(path)+"_")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// preflightTargetsWritable creates a sentinel temp file in each target dir
// and immediately deletes it. Surfaces permission / mount / readonly-fs
// errors before the first real write. Errors at this stage abort the whole
// prepare-pool run; partial-success is worse than zero-success here.
//
// POOL-3 visibility (codex's POOL-2 follow-up ask): walk ALL targets and
// collect every unwritable one before returning, so multi-target runs
// report the full set of failures up front rather than failing on the
// first one and forcing the operator to fix-and-retry per failure.
func preflightTargetsWritable(plan preparePoolPlan) error {
	var failures []string
	for _, t := range plan.Targets {
		f, err := os.CreateTemp(t.Target, ".agentchute-preflight-")
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s: %v", t.Target, err))
			continue
		}
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}
	if len(failures) > 0 {
		return fmt.Errorf("preflight: %d target(s) not writable:\n%s",
			len(failures), strings.Join(failures, "\n"))
	}
	return nil
}

func preparePoolHasMutations(plan preparePoolPlan) bool {
	for _, t := range plan.Targets {
		if t.PointerAction.Apply != nil {
			return true
		}
		for _, w := range t.WrapperActions {
			if w.Apply != nil {
				return true
			}
		}
		if t.GitignoreAction != nil && t.GitignoreAction.Apply != nil {
			return true
		}
	}
	return false
}

// applyPreparePoolPlan runs Apply funcs across all targets. Runs the
// preflight writability check first so permission/mount errors abort
// before any write. Then per-action best-effort: on per-action failure,
// log to stderr and continue (codex's preferred degradation; rollback for
// N targets isn't worth the complexity once preflight has cleared the way).
func applyPreparePoolPlan(plan preparePoolPlan) (int, error) {
	if err := preflightTargetsWritable(plan); err != nil {
		return 0, err
	}
	applied := 0
	for _, t := range plan.Targets {
		if t.PointerAction.Apply != nil {
			if err := t.PointerAction.Apply(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", t.PointerAction.Target, err)
			} else {
				applied++
			}
		}
		for _, w := range t.WrapperActions {
			if w.Apply == nil {
				continue
			}
			if err := w.Apply(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", w.Target, err)
				continue
			}
			applied++
		}
		if t.GitignoreAction != nil && t.GitignoreAction.Apply != nil {
			if err := t.GitignoreAction.Apply(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", t.GitignoreAction.Target, err)
			} else {
				applied++
			}
		}
	}
	return applied, nil
}

func printPreparePoolPlan(w io.Writer, plan preparePoolPlan) {
	fmt.Fprintf(w, "agentchute prepare-pool plan\n")
	fmt.Fprintf(w, "  control_repo: %s\n", plan.ControlRepo)
	fmt.Fprintf(w, "  loop_dir:     %s\n", plan.LoopDir)
	fmt.Fprintf(w, "  vendor:       %s\n", plan.Vendor)
	fmt.Fprintf(w, "  targets:      %d\n", len(plan.Targets))
	if plan.UpdateGitignore {
		fmt.Fprintf(w, "  gitignore:    will update at each target\n")
	} else {
		fmt.Fprintf(w, "  gitignore:    not updating (use --update-gitignore to enable)\n")
	}

	// Stable target ordering for deterministic output.
	sortedTargets := append([]preparePoolTarget(nil), plan.Targets...)
	sort.Slice(sortedTargets, func(i, j int) bool { return sortedTargets[i].Target < sortedTargets[j].Target })

	for _, t := range sortedTargets {
		fmt.Fprintf(w, "\n=== %s ===\n", t.Target)
		if t.NotGit && !plan.UpdateGitignore {
			fmt.Fprintf(w, "  (note: target is not a git repo; pointer + wrapper files won't be auto-tracked)\n")
		}
		if t.NotGit && plan.UpdateGitignore {
			fmt.Fprintf(w, "  (note: target is not a git repo but --update-gitignore was requested; .gitignore will be created/appended anyway)\n")
		}
		fmt.Fprintf(w, "  pointer:   %s   %s\n", t.PointerAction.Action, t.PointerAction.Detail)
		for _, w2 := range t.WrapperActions {
			fmt.Fprintf(w, "  wrapper:   %s %s   %s\n", filepath.Base(w2.Target), w2.Action, w2.Detail)
		}
		if t.GitignoreAction != nil {
			fmt.Fprintf(w, "  gitignore: %s   %s\n", t.GitignoreAction.Action, t.GitignoreAction.Detail)
		}
	}
}
