package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	_ "embed"

	"github.com/agentchute/agentchute/internal/loop"
)

// enrollmentMarkerRE matches both begin and end markers of any version, so
// counting + version extraction work uniformly. Captures: 1=version, 2=kind.
var enrollmentMarkerRE = regexp.MustCompile(`<!-- agentchute-enrollment v(\d+) (begin|end) -->`)

// AGENTCHUTE.md content is embedded at build time so `init` writes the spec
// version pinned to the binary. AGENTCHUTE.md lives at the repo root next to
// this file, so go:embed resolves relative to here.
//
//go:embed AGENTCHUTE.md
var embeddedSpecContent string

const (
	enrollmentVersion       = 16
	gitignoreVersion        = 2
	gitignoreBeginV1        = "# agentchute-gitignore v2 begin"
	gitignoreEndV1          = "# agentchute-gitignore v2 end"
	defaultNamespace        = "agentchute"
	specRecognitionSentinel = "# AGENTCHUTE.md"
)

// enrollmentWrapperTemplate is rendered into CLAUDE.md / CODEX.md / GEMINI.md
// with concrete default identity / vendor values inlined per wrapper. Single source of
// truth: templates/enrollment/wrapper.md at repo root (so the dev repo's
// wrapper files and the embedded init payload stay in sync; CI lint catches
// drift).
//
//go:embed templates/enrollment/wrapper.md
var enrollmentWrapperTemplate string

// enrollmentAgentsTemplate is rendered into AGENTS.md: the wrapper table
// replaces per-tool inlined values so it works as a universal index. Single
// source of truth: templates/enrollment/agents.md at repo root.
//
//go:embed templates/enrollment/agents.md
var enrollmentAgentsTemplate string

// gitignoreStanzaTemplate is appended to .gitignore in a git worktree. NAMESPACE
// is substituted with the fixed "agentchute" loop namespace.
const gitignoreStanzaTemplate = `# agentchute-gitignore v2 begin
.{{NAMESPACE}}/loop/agents/*.md
!.{{NAMESPACE}}/loop/agents/*.example.md
!.{{NAMESPACE}}/loop/agents/README.md
.{{NAMESPACE}}/loop/inbox/
.{{NAMESPACE}}/loop/archive/
.{{NAMESPACE}}/loop/malformed/
.{{NAMESPACE}}/loop/state/
.{{NAMESPACE}}/loop/scratch-*
.{{NAMESPACE}}/loop/watchdog.log
# agentchute-gitignore v2 end
`

// wrapperTarget describes one of CLAUDE.md / CODEX.md / GEMINI.md — auto-
// discovered agent files we render with concrete --as / --vendor values.
type wrapperTarget struct {
	Filename string
	AsID     string
	Vendor   string
}

var wrapperTargets = []wrapperTarget{
	{"CLAUDE.md", "claude-code", "anthropic"},
	{"CODEX.md", "codex", "openai"},
	{"GEMINI.md", "gemini-cli", "google"},
	{"GROK.md", "grok", "xai"},
}

// initAction represents one row in the plan: what we want to do to one path,
// with a short label for the plan output and a thunk that does the work.
type initAction struct {
	Target string
	Action string // human-readable label: "write", "create v1", "prepend v1", "replace v1 drift", "skip", "mkdir 0700", "chmod 0700", "append v1", "skip (not in git)", "warn", "fail"
	Detail string
	Apply  func() error // nil if action is a pure no-op (e.g., skip)
}

// initPlan is the full set of actions plus the rendered content blocks the
// user will see in the plan output before the confirmation prompt.
type initPlan struct {
	Root             string
	Namespace        string
	InGit            bool
	Actions          []initAction
	EnrollmentBlocks map[string]string // filename -> rendered block (shown to user)
	GitignoreStanza  string            // empty if not applicable
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var dryRun, yes bool
	fs.BoolVar(&dryRun, "dry-run", false, "print plan and exit without making changes")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt and apply the plan")

	if err := fs.Parse(args); err != nil {
		return initUsage(err)
	}
	if fs.NArg() != 0 {
		return initUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	root, inGit, err := resolveInitRoot()
	if err != nil {
		return fmt.Errorf("resolve init root: %w", err)
	}

	// Vendor namespacing was removed (simple-again): the loop dotdir is always
	// the fixed .agentchute/loop. No --namespace override.
	plan, err := computeInitPlan(root, defaultNamespace, inGit)
	if err != nil {
		return err
	}

	printInitPlan(os.Stdout, plan)

	if dryRun {
		// Per codex: if both --dry-run and --yes are given, dry-run wins.
		fmt.Println("\n(dry-run; no changes made)")
		return nil
	}

	if !planHasMutations(plan) {
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

	for _, action := range plan.Actions {
		if action.Apply == nil {
			continue
		}
		if err := action.Apply(); err != nil {
			return fmt.Errorf("%s: %w", action.Target, err)
		}
	}

	applied := 0
	for _, a := range plan.Actions {
		if a.Apply != nil {
			applied++
		}
	}
	fmt.Printf("\napplied %d change(s).\n", applied)
	return nil
}

func initUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute init [--dry-run] [--yes]", err)
}

// resolveInitRoot prefers `git rev-parse --show-toplevel` when inside a git
// worktree (per codex: don't silently init a subdir); falls back to cwd
// otherwise. The boolean signals whether we're in a git worktree, which the
// .gitignore handling needs.
func resolveInitRoot() (string, bool, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err == nil {
		path := strings.TrimSpace(string(out))
		if path != "" {
			return path, true, nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	return cwd, false, nil
}

func planHasMutations(plan initPlan) bool {
	for _, a := range plan.Actions {
		if a.Apply != nil {
			return true
		}
	}
	return false
}

// computeInitPlan walks every target and produces the action list. No
// filesystem mutations happen here — only reads and stat calls so the user
// sees the full plan before any prompt.
func computeInitPlan(root, namespace string, inGit bool) (initPlan, error) {
	plan := initPlan{
		Root:             root,
		Namespace:        namespace,
		InGit:            inGit,
		EnrollmentBlocks: make(map[string]string),
	}

	// 1. AGENTCHUTE.md
	specAction, err := planSpecFile(root)
	if err != nil {
		return plan, err
	}
	plan.Actions = append(plan.Actions, specAction)

	// 2. Wrapper enrollment files (CLAUDE.md / CODEX.md / GEMINI.md)
	for _, target := range wrapperTargets {
		rendered := renderWrapperBlock(target.AsID, target.Vendor)
		plan.EnrollmentBlocks[target.Filename] = rendered
		action, err := planEnrollmentFile(filepath.Join(root, target.Filename), rendered)
		if err != nil {
			return plan, err
		}
		plan.Actions = append(plan.Actions, action)
	}

	// 2b. AGENTS.md (table variant)
	rendered := enrollmentAgentsTemplate
	plan.EnrollmentBlocks["AGENTS.md"] = rendered
	action, err := planEnrollmentFile(filepath.Join(root, "AGENTS.md"), rendered)
	if err != nil {
		return plan, err
	}
	plan.Actions = append(plan.Actions, action)

	// 3. .gitignore (only in git worktree)
	plan.GitignoreStanza = renderGitignoreStanza(namespace)
	giAction, err := planGitignore(root, inGit, plan.GitignoreStanza)
	if err != nil {
		return plan, err
	}
	plan.Actions = append(plan.Actions, giAction)

	// 4. Loop directories. We validate the namespace and loop root themselves
	// first: a symlinked parent dir is unsafe because os.MkdirAll on a missing
	// subdir would follow the symlink and create files outside the project.
	loopRoot := filepath.Join(root, "."+namespace, "loop")
	for _, ancestor := range []string{filepath.Join(root, "."+namespace), loopRoot} {
		if err := rejectSymlinkAncestor(ancestor); err != nil {
			return plan, err
		}
	}
	for _, sub := range []string{"agents", "inbox", "archive", "malformed"} {
		dir := filepath.Join(loopRoot, sub)
		rel := filepath.Join("."+namespace, "loop", sub)
		dirAction, err := planLoopDir(dir, rel)
		if err != nil {
			return plan, err
		}
		plan.Actions = append(plan.Actions, dirAction)
	}

	return plan, nil
}

// rejectSymlinkAncestor verifies that path, if it exists, is a real directory
// (not a symlink, not a file). A symlinked namespace or loop dir would let a
// later os.MkdirAll on a missing subdir create the subdir outside the project.
func rejectSymlinkAncestor(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // missing is fine; we'll create it as a real dir.
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; agentchute init refuses to scaffold under a symlinked parent — replace or remove it manually", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	return nil
}

// planSpecFile decides what to do with AGENTCHUTE.md. Missing -> write
// embedded. Recognizable spec (sentinel header found) -> skip. Anything
// else -> fail: the enrollment block references §5 in the spec, so a
// non-agentchute AGENTCHUTE.md would silently break the contract.
func planSpecFile(root string) (initAction, error) {
	path := filepath.Join(root, "AGENTCHUTE.md")
	rel := "AGENTCHUTE.md"

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return initAction{
				Target: rel,
				Action: "write",
				Detail: fmt.Sprintf("new file (%d bytes)", len(embeddedSpecContent)),
				Apply: func() error {
					return os.WriteFile(path, []byte(embeddedSpecContent), 0o644)
				},
			}, nil
		}
		return initAction{}, fmt.Errorf("read AGENTCHUTE.md: %w", err)
	}
	if strings.Contains(string(data), specRecognitionSentinel) {
		return initAction{
			Target: rel,
			Action: "skip",
			Detail: "recognizable agentchute spec already present",
		}, nil
	}
	return initAction{}, fmt.Errorf("%s exists but does not look like an agentchute spec (missing %q header); refusing to overwrite", rel, specRecognitionSentinel)
}

// planEnrollmentFile applies the decision table for wrapper files: missing,
// no marker, marker-current, marker-older, marker-newer, malformed/multiple.
func planEnrollmentFile(path, rendered string) (initAction, error) {
	rel := filepath.Base(path)

	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return initAction{
				Target: rel,
				Action: fmt.Sprintf("create v%d", enrollmentVersion),
				Detail: fmt.Sprintf("new file (%d bytes)", len(rendered)),
				Apply: func() error {
					return os.WriteFile(path, []byte(rendered), 0o644)
				},
			}, nil
		}
		return initAction{}, fmt.Errorf("read %s: %w", rel, err)
	}

	matches := enrollmentMarkerRE.FindAllStringSubmatchIndex(string(existing), -1)
	if len(matches) == 0 {
		// No marker — prepend block at top of file.
		updated := rendered + "\n" + string(existing)
		return initAction{
			Target: rel,
			Action: fmt.Sprintf("prepend v%d", enrollmentVersion),
			Detail: "existing file, no marker found",
			Apply: func() error {
				return os.WriteFile(path, []byte(updated), 0o644)
			},
		}, nil
	}
	if len(matches) > 2 {
		return initAction{}, fmt.Errorf("%s has multiple agentchute-enrollment markers; refusing to guess", rel)
	}
	if len(matches) == 1 {
		return initAction{}, fmt.Errorf("%s has a malformed agentchute-enrollment marker (only one of begin/end present); fix or remove manually", rel)
	}
	// Two markers: must be (begin, end) of same version in that order.
	beginVersion, beginKind := matchInfo(string(existing), matches[0])
	endVersion, endKind := matchInfo(string(existing), matches[1])
	if beginKind != "begin" || endKind != "end" || beginVersion != endVersion {
		return initAction{}, fmt.Errorf("%s has a malformed agentchute-enrollment marker pair (got %s v%d then %s v%d); fix or remove manually", rel, beginKind, beginVersion, endKind, endVersion)
	}
	version := beginVersion
	blockStart := matches[0][0]
	endLineEnd := matches[1][1]
	// Include trailing newline if present.
	if endLineEnd < len(existing) && existing[endLineEnd] == '\n' {
		endLineEnd++
	}
	blockEnd := endLineEnd

	if version > enrollmentVersion {
		return initAction{
			Target: rel,
			Action: "skip (warn)",
			Detail: fmt.Sprintf("file has newer agentchute-enrollment v%d; leaving alone — upgrade agentchute to manage", version),
		}, nil
	}

	currentBlock := (string(existing))[blockStart:blockEnd]

	if version < enrollmentVersion {
		updated := (string(existing))[:blockStart] + rendered + (string(existing))[blockEnd:]
		return initAction{
			Target: rel,
			Action: fmt.Sprintf("replace v%d→v%d", version, enrollmentVersion),
			Detail: "existing file, older marker",
			Apply: func() error {
				return os.WriteFile(path, []byte(updated), 0o644)
			},
		}, nil
	}

	// version == enrollmentVersion
	if strings.TrimRight(currentBlock, "\n") == strings.TrimRight(rendered, "\n") {
		return initAction{
			Target: rel,
			Action: "skip",
			Detail: fmt.Sprintf("v%d already current", enrollmentVersion),
		}, nil
	}

	// Same version, content drift — replace per codex.
	updated := (string(existing))[:blockStart] + rendered + (string(existing))[blockEnd:]
	return initAction{
		Target: rel,
		Action: fmt.Sprintf("replace v%d drift", enrollmentVersion),
		Detail: fmt.Sprintf("marked region differs from canonical v%d", enrollmentVersion),
		Apply: func() error {
			return os.WriteFile(path, []byte(updated), 0o644)
		},
	}, nil
}

// matchInfo extracts (version, kind) from one regex submatch produced by
// enrollmentMarkerRE.FindAllStringSubmatchIndex.
func matchInfo(content string, match []int) (int, string) {
	versionStr := content[match[2]:match[3]]
	kind := content[match[4]:match[5]]
	version := 0
	_, _ = fmt.Sscanf(versionStr, "%d", &version)
	return version, kind
}

// gitignoreMarkerRE mirrors enrollmentMarkerRE for the gitignore stanza so
// version handling is uniform: future-version detection, multi-block fail,
// malformed begin/end pair fail. (per codex: gitignore should be as strict
// as enrollment, not a loose prefix search.)
var gitignoreMarkerRE = regexp.MustCompile(`# agentchute-gitignore v(\d+) (begin|end)`)

// planGitignore decides .gitignore behavior. Outside git → skip with note.
// In git: missing → create. Marker handling parallels planEnrollmentFile.
func planGitignore(root string, inGit bool, stanza string) (initAction, error) {
	path := filepath.Join(root, ".gitignore")
	rel := ".gitignore"

	if !inGit {
		return initAction{
			Target: rel,
			Action: "skip",
			Detail: "not in a git worktree",
		}, nil
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			content := stanza
			return initAction{
				Target: rel,
				Action: fmt.Sprintf("create v%d", gitignoreVersion),
				Detail: fmt.Sprintf("new file (%d bytes)", len(content)),
				Apply: func() error {
					return os.WriteFile(path, []byte(content), 0o644)
				},
			}, nil
		}
		return initAction{}, fmt.Errorf("read .gitignore: %w", err)
	}

	matches := gitignoreMarkerRE.FindAllStringSubmatchIndex(string(existing), -1)
	if len(matches) == 0 {
		// No marker — append stanza.
		prefix := string(existing)
		if !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		updated := prefix + stanza
		return initAction{
			Target: rel,
			Action: fmt.Sprintf("append v%d", gitignoreVersion),
			Detail: "existing file, no marker found",
			Apply: func() error {
				return os.WriteFile(path, []byte(updated), 0o644)
			},
		}, nil
	}
	if len(matches) > 2 {
		return initAction{}, fmt.Errorf(".gitignore has multiple agentchute-gitignore markers; refusing to guess")
	}
	if len(matches) == 1 {
		return initAction{}, fmt.Errorf(".gitignore has a malformed agentchute-gitignore marker (only one of begin/end present); fix or remove manually")
	}
	beginVersion, beginKind := matchInfo(string(existing), matches[0])
	endVersion, endKind := matchInfo(string(existing), matches[1])
	if beginKind != "begin" || endKind != "end" || beginVersion != endVersion {
		return initAction{}, fmt.Errorf(".gitignore has a malformed agentchute-gitignore marker pair (got %s v%d then %s v%d)", beginKind, beginVersion, endKind, endVersion)
	}

	blockStart := matches[0][0]
	blockEnd := matches[1][1]
	if blockEnd < len(existing) && existing[blockEnd] == '\n' {
		blockEnd++
	}
	currentStanza := string(existing)[blockStart:blockEnd]

	if beginVersion > gitignoreVersion {
		return initAction{
			Target: rel,
			Action: "skip (warn)",
			Detail: fmt.Sprintf(".gitignore has newer agentchute-gitignore v%d; upgrade agentchute to manage", beginVersion),
		}, nil
	}
	if beginVersion < gitignoreVersion {
		updated := string(existing)[:blockStart] + stanza + string(existing)[blockEnd:]
		return initAction{
			Target: rel,
			Action: fmt.Sprintf("replace v%d→v%d", beginVersion, gitignoreVersion),
			Detail: "existing file, older marker",
			Apply: func() error {
				return os.WriteFile(path, []byte(updated), 0o644)
			},
		}, nil
	}
	if strings.TrimRight(currentStanza, "\n") == strings.TrimRight(stanza, "\n") {
		return initAction{
			Target: rel,
			Action: "skip",
			Detail: fmt.Sprintf("v%d already current", gitignoreVersion),
		}, nil
	}
	updated := string(existing)[:blockStart] + stanza + string(existing)[blockEnd:]
	return initAction{
		Target: rel,
		Action: fmt.Sprintf("replace v%d drift", gitignoreVersion),
		Detail: "marked stanza differs from canonical",
		Apply: func() error {
			return os.WriteFile(path, []byte(updated), 0o644)
		},
	}, nil
}

// planLoopDir ensures dir exists at 0700; existing dirs at other modes get a
// chmod action so "nothing to do" only fires when both content and perms are
// already correct (per codex). Returns error for unsafe states (symlinks,
// existing non-dir) so the command aborts at plan time.
func planLoopDir(dir, rel string) (initAction, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return initAction{
				Target: rel,
				Action: "mkdir 0700",
				Detail: "new directory",
				Apply: func() error {
					return loop.EnsurePrivateDir(dir)
				},
			}, nil
		}
		return initAction{}, fmt.Errorf("%s: lstat: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return initAction{}, fmt.Errorf("%s: symlink not allowed for loop dirs; remove it manually", rel)
	}
	if !info.IsDir() {
		return initAction{}, fmt.Errorf("%s: exists and is not a directory", rel)
	}
	if info.Mode().Perm() != 0o700 {
		return initAction{
			Target: rel,
			Action: "chmod 0700",
			Detail: fmt.Sprintf("existing dir at mode %o", info.Mode().Perm()),
			Apply: func() error {
				return os.Chmod(dir, 0o700)
			},
		}, nil
	}
	return initAction{
		Target: rel,
		Action: "skip",
		Detail: "already exists at 0700",
	}, nil
}

// renderWrapperBlock substitutes {{AGENT_ID}} / {{VENDOR}} in the wrapper
// template. {{AS}} is kept as a legacy alias so older templates still render.
func renderWrapperBlock(asID, vendor string) string {
	out := strings.ReplaceAll(enrollmentWrapperTemplate, "{{AGENT_ID}}", asID)
	out = strings.ReplaceAll(out, "{{AS}}", asID)
	out = strings.ReplaceAll(out, "{{VENDOR}}", vendor)
	return out
}

// renderGitignoreStanza substitutes {{NAMESPACE}} in the gitignore template.
func renderGitignoreStanza(namespace string) string {
	return strings.ReplaceAll(gitignoreStanzaTemplate, "{{NAMESPACE}}", namespace)
}

// printInitPlan renders the plan as a tab-aligned table plus the literal
// content of every block/stanza that will be written. Sorted action order
// matches what the apply loop will do.
func printInitPlan(w io.Writer, plan initPlan) {
	fmt.Fprintln(w, "agentchute init")
	fmt.Fprintln(w)
	if plan.InGit {
		fmt.Fprintf(w, "initializing in: %s (git toplevel)\n", plan.Root)
	} else {
		fmt.Fprintf(w, "initializing in: %s\n", plan.Root)
	}
	fmt.Fprintf(w, "namespace:       %s (loop dir: .%s/loop/)\n", plan.Namespace, plan.Namespace)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "planned changes:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, a := range plan.Actions {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Target, a.Action, a.Detail)
	}
	_ = tw.Flush()

	// Show literal content user must consent to.
	if len(plan.EnrollmentBlocks) > 0 {
		fmt.Fprintln(w)
		// Render in stable order so output is predictable.
		filenames := make([]string, 0, len(plan.EnrollmentBlocks))
		for k := range plan.EnrollmentBlocks {
			filenames = append(filenames, k)
		}
		sort.Strings(filenames)
		for _, fn := range filenames {
			fmt.Fprintf(w, "--- ENROLLMENT v%d block for %s ---\n", enrollmentVersion, fn)
			fmt.Fprint(w, plan.EnrollmentBlocks[fn])
			fmt.Fprintln(w, "--- end block ---")
		}
	}
	if plan.InGit && plan.GitignoreStanza != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "--- .gitignore stanza ---")
		fmt.Fprint(w, plan.GitignoreStanza)
		fmt.Fprintln(w, "--- end stanza ---")
	}
}

func promptConfirm(stdin io.Reader, stdout io.Writer, prompt string) (bool, error) {
	fmt.Fprint(stdout, prompt)
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}
