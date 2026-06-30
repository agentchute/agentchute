package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Hook content scan patterns. Each captures one of the three documented
// invocation forms for the agentchute binary inside a hook command string.
//
// Forms:
//   - bare       — `agentchute <subcmd>`. Requires `agentchute` on PATH.
//   - templated  — `${AGENTCHUTE_BIN:-agentchute} <subcmd>`. Requires AGENTCHUTE_BIN
//     to resolve OR `agentchute` on PATH as fallback.
//   - env-only   — `$AGENTCHUTE_BIN <subcmd>`. Requires AGENTCHUTE_BIN only.
//
// All three forms count as offenders if the subcommand is `check` —
// `check` archives and quarantines, regardless of how the binary is
// resolved. (codex review on bff226c: the prior check only matched the
// bare form.)
var (
	// Any agentchute invocation form followed by `check` as a subcommand.
	hookCheckSubcmdRE = regexp.MustCompile(`(?:\$\{AGENTCHUTE_BIN:-agentchute\}|\$AGENTCHUTE_BIN|agentchute)[ \t]+check\b`)

	// Bare `agentchute <word>` not preceded by a path char or by `-` (which
	// would mean it's the inside of `${AGENTCHUTE_BIN:-agentchute}`).
	hookBareAgentchuteRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_/\-{])agentchute[ \t]+[a-z]`)

	// Templated form. Anchors the presence of the override-aware shape.
	hookTemplatedRE = regexp.MustCompile(`\$\{AGENTCHUTE_BIN:-agentchute\}[ \t]+[a-z]`)

	// Env-only form. Distinct from templated because absent
	// AGENTCHUTE_BIN there is no fallback.
	hookEnvOnlyRE = regexp.MustCompile(`\$AGENTCHUTE_BIN[ \t]+[a-z]`)
)

// cmdDoctor is the diagnostic aggregator. Walks an
// ordered list of checks; each check returns a severity-tagged result.
// Doctor diagnoses and exits nonzero on blockers; `gate` / `boot` own the
// lifecycle blocking surface during normal wrapper operation.
//
// Severity rules (codex brainstorm note):
//   - BLOCKER: integration is unsafe or broken; exit nonzero so CI/operator
//     scripts can fail fast (missing scaffold, unreadable registration,
//     bare `check` in a hook, binary unresolvable for declared hook template).
//   - WARN:    operational signal; surface but do not fail (stale reg,
//     unread mail, /tmp binary, hook file absent for installed wrapper).
//   - SKIP:    check is not applicable in this context (cross-host wake
//     target; --as not provided so agent-specific check skipped).
//   - OK:      check passed.
func cmdDoctor(args []string) error {
	// v0.2: --generate-service emits launchd/systemd/script artifacts and
	// returns; it does not run the diagnostic checks. Routed up front so
	// the normal flag parser doesn't choke on the generator-only flags.
	for _, a := range args {
		if a == "--generate-service" || strings.HasPrefix(a, "--generate-service=") {
			return handleGenerateService(args)
		}
	}

	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, controlRepo, loopDir string
	var jsonOut bool
	fs.StringVar(&agentID, "as", "", "agent id to diagnose; optional (or $AGENTCHUTE_AGENT_ID). When omitted, agent-specific checks are SKIPped.")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")

	if err := fs.Parse(args); err != nil {
		return doctorUsage(err)
	}
	if fs.NArg() != 0 {
		return doctorUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID != "" {
		if err := loop.ValidateAgentID(agentID); err != nil {
			return err
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		// Discovery failure is itself diagnostic: emit a single BLOCKER and
		// exit nonzero. Without cfg we can't run any of the other checks.
		report := doctorReport{
			Agent:    agentID,
			Checks:   []doctorCheck{{Name: "discover", Severity: severityBlocker, Message: err.Error()}},
			Blockers: 1,
		}
		if jsonOut {
			if emitErr := emitDoctorJSON(report); emitErr != nil {
				return emitErr
			}
		} else {
			emitDoctorText(report)
		}
		// Per the doctor contract (codex review on bff226c): any BLOCKER
		// must exit nonzero regardless of output mode.
		return errBlocked
	}

	opts := doctorOptions{
		Now:     time.Now().UTC(),
		PathEnv: os.Getenv("PATH"),
	}
	if gs, err := readSetupGlobalState(); err == nil {
		opts.GlobalState = &gs
	}
	if ps, err := readSetupPoolState(cfg); err == nil {
		opts.PoolState = &ps
	}

	report := runDoctorChecks(cfg, agentID, opts)

	if jsonOut {
		if err := emitDoctorJSON(report); err != nil {
			return err
		}
	} else {
		emitDoctorText(report)
	}
	if report.Blockers > 0 {
		return errBlocked
	}
	return nil
}

const (
	severityBlocker = "BLOCKER"
	severityWarn    = "WARN"
	severityOK      = "OK"
	severitySkip    = "SKIP"
)

type doctorCheck struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type doctorReport struct {
	Agent    string        `json:"agent,omitempty"`
	Checks   []doctorCheck `json:"checks"`
	Blockers int           `json:"blockers"`
	Warnings int           `json:"warnings"`
}

type doctorOptions struct {
	Now         time.Time
	PathEnv     string
	GlobalState *setupGlobalState
	PoolState   *setupPoolState
}

// runDoctorChecks executes the canonical check sequence and returns a
// fully-populated report.
func runDoctorChecks(cfg *loop.Config, agentID string, opts doctorOptions) doctorReport {
	checks := []doctorCheck{
		checkLoopDirScaffold(cfg),
		checkBinaryOnPath(),
		checkHookFilePresence(cfg, agentID),
		checkHookContentSanity(cfg),
		checkWrapperShadowing(cfg, agentID, opts),
		checkUnenrolledPresence(cfg),
		checkLaunchProvenance(cfg, agentID, opts),
	}
	if agentID != "" {
		checks = append(checks,
			checkSelfRegistration(cfg, agentID),
			checkRegistrationFreshness(cfg, agentID, opts.Now),
			checkInboxState(cfg, agentID),
			checkLedgerState(cfg, agentID),
		)
	} else {
		checks = append(checks, doctorCheck{
			Name:     "agent_specific_checks",
			Severity: severitySkip,
			Message:  "no --as / $AGENTCHUTE_AGENT_ID; skipped per-agent checks (registration freshness, inbox state, ledger state, wake target validity)",
		})
	}

	report := doctorReport{Agent: agentID, Checks: checks}
	for _, c := range checks {
		switch c.Severity {
		case severityBlocker:
			report.Blockers++
		case severityWarn:
			report.Warnings++
		}
	}
	return report
}

// ---------- individual checks ----------

func checkLoopDirScaffold(cfg *loop.Config) doctorCheck {
	type expected struct {
		path string
		mode os.FileMode
	}
	for _, e := range []expected{
		{cfg.AgentsDir(), 0o700},
		{filepath.Join(cfg.LoopDir, "inbox"), 0o700},
		{cfg.ArchiveDir(), 0o700},
		{cfg.MalformedDir(), 0o700},
	} {
		info, err := os.Stat(e.path)
		if err != nil {
			if os.IsNotExist(err) {
				// archive + malformed are created lazily on first use; only
				// agents + inbox are required upfront. Inbox is the parent
				// dir; per-agent dirs land at register time.
				if e.path == cfg.ArchiveDir() || e.path == cfg.MalformedDir() {
					continue
				}
				return doctorCheck{
					Name:     "loop_dir_scaffold",
					Severity: severityBlocker,
					Message:  fmt.Sprintf("required directory missing: %s — run `agentchute init`", e.path),
				}
			}
			return doctorCheck{
				Name:     "loop_dir_scaffold",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("stat %s: %v", e.path, err),
			}
		}
		if !info.IsDir() {
			return doctorCheck{
				Name:     "loop_dir_scaffold",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("%s exists but is not a directory", e.path),
			}
		}
	}
	return doctorCheck{Name: "loop_dir_scaffold", Severity: severityOK, Message: "agents/, inbox/ present with correct shape"}
}

func checkBinaryOnPath() doctorCheck {
	// AGENTCHUTE_BIN takes precedence; hook templates use ${AGENTCHUTE_BIN:-agentchute}.
	if envBin := strings.TrimSpace(os.Getenv("AGENTCHUTE_BIN")); envBin != "" {
		if reason := executableFileProblem(envBin); reason != "" {
			return doctorCheck{
				Name:     "binary_on_path",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("AGENTCHUTE_BIN=%s %s; hook templates will fail to launch the binary", envBin, reason),
			}
		}
		return doctorCheck{
			Name:     "binary_on_path",
			Severity: severityOK,
			Message:  fmt.Sprintf("AGENTCHUTE_BIN=%s is an executable file; hook templates will resolve", envBin),
		}
	}
	resolved, err := exec.LookPath("agentchute")
	if err != nil {
		return doctorCheck{
			Name:     "binary_on_path",
			Severity: severityWarn,
			Message:  "agentchute is not on PATH and AGENTCHUTE_BIN is unset; hook templates that reference bare `agentchute` will fail unless you set AGENTCHUTE_BIN in the wrapper-launching environment",
		}
	}
	// Non-canonical /tmp/ location is operational debt, not a blocker.
	if strings.HasPrefix(resolved, "/tmp/") || strings.HasPrefix(resolved, "/var/tmp/") {
		return doctorCheck{
			Name:     "binary_on_path",
			Severity: severityWarn,
			Message:  fmt.Sprintf("agentchute resolves to %s (transient location); consider installing to a stable PATH entry or setting AGENTCHUTE_BIN", resolved),
		}
	}
	return doctorCheck{
		Name:     "binary_on_path",
		Severity: severityOK,
		Message:  fmt.Sprintf("agentchute resolves to %s", resolved),
	}
}

// checkWrapperShadowing verifies the single `ac` dispatcher (v0.8.8) resolves
// from the shim dir ahead of the system `ac` (/usr/sbin/ac, the accounting
// command). It is OK when `ac` resolves from $shim_dir AND $shim_dir precedes any
// other dir with an `ac` on PATH; WARN when a non-shim-dir `ac` shadows it or the
// shim dir is absent from PATH. The check is reported as `ac_dispatcher`.
func checkWrapperShadowing(cfg *loop.Config, agentID string, opts doctorOptions) doctorCheck {
	const name = "ac_dispatcher"

	wake := ""
	if opts.PoolState != nil && opts.PoolState.Wake != "" {
		wake = opts.PoolState.Wake
	} else if opts.GlobalState != nil && opts.GlobalState.Wake != "" {
		wake = opts.GlobalState.Wake
	}

	if wake == "" {
		return doctorCheck{Name: name, Severity: severitySkip, Message: "agentchute setup not run; skipping ac dispatcher check"}
	}

	shimDir := ""
	if opts.GlobalState != nil {
		shimDir = opts.GlobalState.ShimDir
	}
	if shimDir == "" {
		home, _ := os.UserHomeDir()
		shimDir = filepath.Join(home, ".agentchute", "bin")
	}

	// Set-aware: the lifecycle-hook skip applies only when runner is NOT among
	// the wake paths (runner requires the dispatcher on PATH). A legacy
	// tmux/herdr-only set — single or combined ("tmux,herdr") — relies on the
	// hookable wrapper's lifecycle hook, so the dispatcher is optional there.
	if !setupNeedsShims(wake) && (wakeSetContains(wake, setupWakeTmux) || wakeSetContains(wake, setupWakeHerdr)) {
		if _, hookable := hookWrapperForAgent(agentID); hookable {
			return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("%s wake uses lifecycle hooks for this wrapper; ac dispatcher is optional", wake)}
		}
	}

	pathEnv := opts.PathEnv
	if pathEnv == "" {
		pathEnv = os.Getenv("PATH")
	}

	if !pathContains(shimDir, pathEnv) {
		return doctorCheck{
			Name:     name,
			Severity: severityWarn,
			Message:  fmt.Sprintf("shim dir %s is not on PATH; add it or rerun setup", shimDir),
		}
	}
	if pathResolvesToDir(shimDir, pathEnv, []string{"ac"}) {
		return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("ac dispatcher resolves from %s", shimDir)}
	}
	return doctorCheck{
		Name:     name,
		Severity: severityWarn,
		Message:  fmt.Sprintf("the system `ac` shadows the agentchute dispatcher; ensure %s precedes /usr/sbin on PATH (open a new shell or `hash -r`)", shimDir),
	}
}

// checkUnenrolledPresence is the WI-E1 read-only presence check. It runs
// scanUnenrolledWrappers and reports OK when nothing is present-but-unenrolled,
// or WARN listing the offenders. It is ADVISORY ONLY — never a BLOCKER: a raw
// wrapper that skipped enrollment is an operator signal, not a reason to fail a
// gate. The scan performs ZERO writes (it never repairs a registration; that is
// WI-E4's job).
func checkUnenrolledPresence(cfg *loop.Config) doctorCheck {
	found, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		return doctorCheck{Name: "unenrolled_presence", Severity: severitySkip, Message: fmt.Sprintf("presence scan unavailable: %v", err)}
	}
	if len(found) == 0 {
		return doctorCheck{Name: "unenrolled_presence", Severity: severityOK, Message: "no unenrolled wrappers detected in this pool"}
	}
	parts := make([]string, 0, len(found))
	for _, p := range found {
		parts = append(parts, fmt.Sprintf("%s:%s", p.Kind, p.Hint))
	}
	return doctorCheck{
		Name:     "unenrolled_presence",
		Severity: severityWarn,
		Message:  fmt.Sprintf("%d wrapper(s) present in this pool but not enrolled: %s — enroll via their `ac-<wrapper>` launcher or `agentchute boot --as <id>`", len(found), strings.Join(parts, ", ")),
	}
}

// checkLaunchProvenance is the WI-E3 detect-and-warn launch-bypass check. When
// the runner wake path IS configured (setup installed the ac-* launchers and the
// expected launch is `ac-<wrapper>` -> runner), it WARNS — never BLOCKS — if a
// wrapper is running raw:
//
//   - the agent's registration records launched_by=manual or has no provenance
//     (a hand/raw launch, not via the runner), OR
//   - a real wrapper binary shadows the launcher shim earlier on PATH
//     (pathIsPrioritized==false while the shim dir IS on PATH).
//
// This is ADVISORY by design (codex guardrail): it NEVER returns a BLOCKER, it
// does NOT flip the runner default (runner stays opt-in), and it installs no
// same-name shadowing — it only points the operator at `ac-<wrapper>`. Managed
// enrollments (runner/hook/poller) and non-runner setups do not warn.
func checkLaunchProvenance(cfg *loop.Config, agentID string, opts doctorOptions) doctorCheck {
	const name = "launch_provenance"

	wake := ""
	if opts.PoolState != nil && opts.PoolState.Wake != "" {
		wake = opts.PoolState.Wake
	} else if opts.GlobalState != nil && opts.GlobalState.Wake != "" {
		wake = opts.GlobalState.Wake
	}
	if wake == "" {
		return doctorCheck{Name: name, Severity: severitySkip, Message: "agentchute setup not run; launch-bypass check not applicable"}
	}
	if !setupNeedsShims(wake) {
		return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("%s wake does not include the runner; raw-launch bypass only applies to runner setups", wake)}
	}

	var reasons []string
	// Provenance: the agent enrolled raw (manual / no provenance) rather than via
	// the runner. Managed provenance (runner/hook/poller) is fine. The old
	// per-wrapper-shim shadow check is obsolete under the `ac` dispatcher — launch
	// is `ac run <wrapper>`, and `ac`'s own PATH precedence is the ac_dispatcher check.
	if agentID != "" {
		if reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err == nil {
			switch strings.TrimSpace(reg.LaunchedBy) {
			case loop.LaunchedByRunner, loop.LaunchedByHook, loop.LaunchedByPoller:
				// Managed enrollment — not a raw bypass.
			default: // "" (legacy/unknown) or "manual"
				reasons = append(reasons, fmt.Sprintf("registration launched_by=%q indicates a raw launch (not routed through the runner)", firstNonEmpty(reg.LaunchedBy, "unset")))
			}
		}
	}

	if len(reasons) == 0 {
		return doctorCheck{Name: name, Severity: severityOK, Message: "no raw-launch bypass detected; this lane routes through the runner"}
	}
	return doctorCheck{
		Name:     name,
		Severity: severityWarn,
		Message:  fmt.Sprintf("%s — relaunch via `%s` to route through the runner (advisory only; the runner stays opt-in and is never auto-activated)", strings.Join(reasons, "; "), acRunHintForAgent(agentID)),
	}
}

// acRunHintForAgent renders the canonical launch command for an agent id, e.g.
// "ac run codex". Falls back to a generic hint for an unrecognized id.
func acRunHintForAgent(agentID string) string {
	for _, spec := range wrapperSpecs {
		if spec.AgentID == agentID {
			return "ac run " + spec.Key
		}
	}
	return "ac run <wrapper>"
}

func shimNamesForAgent(agentID string) []string {
	agentID = strings.TrimSpace(agentID)
	if agentID != "" {
		for _, spec := range wrapperSpecs {
			// Match contextual ids (codex-agentchute) to their canonical shim,
			// not just exact base ids.
			if registrationMatchesCanonical(agentID, spec.AgentID) {
				return []string{spec.Name}
			}
		}
	}
	names := make([]string, 0, len(wrapperSpecs))
	for _, spec := range wrapperSpecs {
		names = append(names, spec.Name)
	}
	return names
}

// hookFile maps a wrapper to the conventional template location relative
// to the control repo. checkHookFilePresence walks this list to surface
// which wrappers are wired up vs. relying on plain-text fallback.
var hookFiles = []struct {
	wrapper string
	path    []string // relative to control repo
}{
	{"claude-code", []string{".claude", "settings.json"}},
	{"codex", []string{".codex", "hooks.json"}},
	{"gemini-cli", []string{".gemini", "settings.json"}},
}

func checkHookFilePresence(cfg *loop.Config, agentID string) doctorCheck {
	present := []string{}
	presentSet := map[string]bool{}
	for _, h := range hookFiles {
		full := filepath.Join(append([]string{cfg.ControlRepo}, h.path...)...)
		if _, err := os.Stat(full); err == nil {
			present = append(present, h.wrapper)
			presentSet[h.wrapper] = true
		}
	}
	if wrapper, ok := hookWrapperForAgent(agentID); ok {
		if !presentSet[wrapper] {
			return doctorCheck{
				Name:     "hook_file_presence",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("acting wrapper hook for %s is missing; run `agentchute hooks install --wrapper %s`", agentID, wrapper),
			}
		}
		if drift := actingHookDrift(cfg, wrapper); drift != "" {
			return doctorCheck{
				Name:     "hook_file_presence",
				Severity: severityBlocker,
				Message:  drift,
			}
		}
	}
	if len(present) == 0 {
		return doctorCheck{
			Name:     "hook_file_presence",
			Severity: severityWarn,
			Message:  "no wrapper hook templates installed in this control repo; copy from examples/hooks/<wrapper>/ to wire up SessionStart/UserPromptSubmit/Stop automation",
		}
	}
	return doctorCheck{
		Name:     "hook_file_presence",
		Severity: severityOK,
		Message:  fmt.Sprintf("hook templates installed for: %s", strings.Join(present, ", ")),
	}
}

func actingHookDrift(cfg *loop.Config, wrapper string) string {
	for _, h := range hookWrappers {
		if h.Name != wrapper {
			continue
		}
		full := filepath.Join(cfg.ControlRepo, h.Dest)
		installed, err := os.ReadFile(full)
		if err != nil {
			return fmt.Sprintf("acting wrapper hook for %s is unreadable at %s: %v", wrapper, full, err)
		}
		canonical, err := hooksFS.ReadFile(h.Src)
		if err != nil {
			return fmt.Sprintf("canonical hook template for %s is unreadable: %v", wrapper, err)
		}
		if !bytes.Equal(installed, canonical) {
			return fmt.Sprintf("acting wrapper hook for %s differs from the canonical template; run `agentchute hooks install --wrapper %s --force`", wrapper, wrapper)
		}
	}
	return ""
}

// hookWrapperForAgent resolves an agent id to its canonical hookable wrapper.
// Real setups enroll with contextual ids (e.g. codex-agentchute), so match by
// canonical base — exact or "<base>-" prefix — not exact base id only.
// Hookless wrappers (grok) are intentionally absent.
func hookWrapperForAgent(agentID string) (string, bool) {
	agentID = strings.TrimSpace(agentID)
	for _, w := range setupWrappers {
		if w.Hookable && registrationMatchesCanonical(agentID, w.Name) {
			return w.Name, true
		}
	}
	return "", false
}

// checkHookContentSanity scans installed hook templates per-occurrence
// instead of per-file: each agentchute invocation form is analyzed
// independently so mixed templated + bare references in one file are
// caught (codex review on bff226c). Two BLOCKER classes:
//
//  1. Any `check` subcommand in a hook — bare, templated, or env-only.
//     `check` archives and quarantines regardless of how the binary
//     resolved, so the silent-drain risk doesn't depend on which form
//     was used.
//  2. A binary-resolution gap: a bare `agentchute ...` reference with
//     no PATH resolution, a templated `${AGENTCHUTE_BIN:-agentchute} ...`
//     reference with neither AGENTCHUTE_BIN set nor PATH fallback, or a
//     `$AGENTCHUTE_BIN ...` reference with no AGENTCHUTE_BIN.
func checkHookContentSanity(cfg *loop.Config) doctorCheck {
	binOnPath := isAgentchuteOnPath()
	envBinValid := isAgentchuteBinValid()

	var checkOffenders []string
	var resolutionOffenders []string

	for _, h := range hookFiles {
		full := filepath.Join(append([]string{cfg.ControlRepo}, h.path...)...)
		data, err := os.ReadFile(full)
		if err != nil {
			continue // absence is handled by checkHookFilePresence
		}
		body := string(data)

		if hookCheckSubcmdRE.MatchString(body) {
			checkOffenders = append(checkOffenders, h.wrapper)
		}

		hasBare := hookBareAgentchuteRE.MatchString(body)
		hasTemplated := hookTemplatedRE.MatchString(body)
		hasEnvOnly := hookEnvOnlyRE.MatchString(body)

		// Each form's resolution is checked independently. A mixed file
		// with one bare + one templated invocation will be flagged if
		// either form can't resolve in this environment.
		switch {
		case hasBare && !binOnPath:
			resolutionOffenders = append(resolutionOffenders, h.wrapper+" (bare `agentchute` needs PATH)")
		case hasTemplated && !envBinValid && !binOnPath:
			resolutionOffenders = append(resolutionOffenders, h.wrapper+" (templated `${AGENTCHUTE_BIN:-agentchute}` needs AGENTCHUTE_BIN or PATH)")
		case hasEnvOnly && !envBinValid:
			resolutionOffenders = append(resolutionOffenders, h.wrapper+" (`$AGENTCHUTE_BIN` reference needs AGENTCHUTE_BIN set)")
		}
	}
	if len(checkOffenders) > 0 {
		return doctorCheck{
			Name:     "hook_content_sanity",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("hook file(s) invoke `agentchute check` (silent-drain risk; check archives and quarantines): %s — replace with `pending` or `boot --context-only`", strings.Join(checkOffenders, ", ")),
		}
	}
	if len(resolutionOffenders) > 0 {
		return doctorCheck{
			Name:     "hook_content_sanity",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("hook file(s) reference agentchute commands that cannot resolve in this environment: %s", strings.Join(resolutionOffenders, ", ")),
		}
	}
	return doctorCheck{Name: "hook_content_sanity", Severity: severityOK, Message: "no `check` subcommand in hooks and all references resolve"}
}

func isAgentchuteOnPath() bool {
	_, err := exec.LookPath("agentchute")
	return err == nil
}

func isAgentchuteBinValid() bool {
	envBin := strings.TrimSpace(os.Getenv("AGENTCHUTE_BIN"))
	if envBin == "" {
		return false
	}
	return executableFileProblem(envBin) == ""
}

// executableFileProblem returns a human-readable reason when `path` is
// NOT a regular file with at least one execute bit set, or "" when the
// path is launchable by the wrapper's exec call. Stricter than
// os.Stat because v0.1.2 shipped a check that incorrectly accepted
// directories (codex review on d73d4dd).
func executableFileProblem(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "does not exist"
		}
		return fmt.Sprintf("stat error: %v", err)
	}
	if info.IsDir() {
		return "is a directory, not a binary"
	}
	if !info.Mode().IsRegular() {
		return "is not a regular file"
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "is not executable (no exec bits)"
	}
	return ""
}

func checkSelfRegistration(cfg *loop.Config, agentID string) doctorCheck {
	regPath := cfg.AgentRegistrationPath(agentID)
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{
				Name:     "self_registration",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("no registration for %s — run `agentchute boot --as %s --vendor <vendor>`", agentID, agentID),
			}
		}
		return doctorCheck{
			Name:     "self_registration",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("registration unreadable at %s: %v", regPath, err),
		}
	}
	if reg.AgentID != agentID {
		return doctorCheck{
			Name:     "self_registration",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("registration file at %s reports agent_id=%q, expected %q", regPath, reg.AgentID, agentID),
		}
	}
	return doctorCheck{Name: "self_registration", Severity: severityOK, Message: fmt.Sprintf("registration valid: %s (%s)", reg.AgentID, reg.Vendor)}
}

// checkRegistrationFreshness reports presence freshness. GATE 3: the freshness
// SOURCE is the `.live` presence fact, not registration last_seen. The check
// name ("registration_freshness"), the StaleRegThreshold, the severities, and
// the "run `agentchute boot`" remediation are unchanged.
func checkRegistrationFreshness(cfg *loop.Config, agentID string, now time.Time) doctorCheck {
	if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err != nil {
		return doctorCheck{Name: "registration_freshness", Severity: severitySkip, Message: "registration unreadable (see self_registration)"}
	}
	liveSeen, present := loop.LiveLastSeen(cfg, agentID)
	if !present {
		// Registered but no `.live` published (never booted under this gate, or
		// presence expired) — surface as a warn with the same boot remediation.
		return doctorCheck{
			Name:     "registration_freshness",
			Severity: severityWarn,
			Message:  "no recent presence (`.live` absent); run `agentchute boot` to refresh",
		}
	}
	age := now.Sub(liveSeen)
	if age < 0 {
		age = 0 // future-dated (clock skew) reads as fresh.
	}
	if age > StaleRegThreshold {
		return doctorCheck{
			Name:     "registration_freshness",
			Severity: severityWarn,
			Message:  fmt.Sprintf("last_seen age %s exceeds %s threshold; run `agentchute boot` to refresh", age.Round(time.Second), StaleRegThreshold),
		}
	}
	return doctorCheck{Name: "registration_freshness", Severity: severityOK, Message: fmt.Sprintf("last_seen age %s within threshold", age.Round(time.Second))}
}

func checkInboxState(cfg *loop.Config, agentID string) doctorCheck {
	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		if errors.Is(err, loop.ErrInboxMissing) {
			return doctorCheck{
				Name:     "inbox_state",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("inbox directory missing for %s — run `agentchute boot --as %s --vendor <vendor>` (AGENTCHUTE.md §5.3)", agentID, agentID),
			}
		}
		return doctorCheck{Name: "inbox_state", Severity: severityWarn, Message: fmt.Sprintf("inbox list error: %v", err)}
	}
	// Gate 4 dual-read drain gauge: count legacy nonce-named files still present.
	// Drain is complete (and the legacy reader becomes removable in a future
	// release) when every live inbox reports zero. Computed from the already-
	// listed slice — no second filesystem scan.
	legacyNote := ""
	if n := loop.CountLegacyNonce(msgs); n > 0 {
		legacyNote = fmt.Sprintf(" (%d legacy nonce-named, pending drain — one-release migration window)", n)
	}
	if len(skipped) > 0 {
		return doctorCheck{
			Name:     "inbox_state",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%d unread + %d malformed file(s) in inbox%s; malformed files block `gate --before finish` until quarantined via `check`", len(msgs), len(skipped), legacyNote),
		}
	}
	if len(msgs) > 0 {
		return doctorCheck{
			Name:     "inbox_state",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%d unread direct message(s) in inbox%s", len(msgs), legacyNote),
		}
	}
	return doctorCheck{Name: "inbox_state", Severity: severityOK, Message: "inbox clear"}
}

func checkLedgerState(cfg *loop.Config, agentID string) doctorCheck {
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		return doctorCheck{Name: "ledger_state", Severity: severityBlocker, Message: fmt.Sprintf("pending-reply ledger unreadable: %v", err)}
	}
	pending := ledger.PendingEntries()
	if len(pending) > 0 {
		return doctorCheck{
			Name:     "ledger_state",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%d pending reply obligation(s); will block `gate --before finish` until cleared via `send --reply-to` or `defer`", len(pending)),
		}
	}
	return doctorCheck{Name: "ledger_state", Severity: severityOK, Message: "no pending reply obligations"}
}

// Simple-again Gate 6a (pull-only): checkWakeTargetValidity and
// checkRunnerSocketStaleness were removed. Both probed a recipient's wake
// endpoint for reachability — a push-era concern that no longer exists once
// senders stop poking. They depended on the deleted runner / tmux + herdr
// reachability helpers. Gate 6c then removed the registration wake fields
// entirely, so no doctor check reads them. The doctor framework and all other
// (subsystem-free) checks are unchanged.

// ---------- output ----------

func emitDoctorText(r doctorReport) {
	if r.Agent != "" {
		fmt.Printf("doctor: %s\n\n", r.Agent)
	} else {
		fmt.Printf("doctor: (no agent; pool-level checks only)\n\n")
	}
	for _, c := range r.Checks {
		marker := "  "
		switch c.Severity {
		case severityBlocker:
			marker = "✗ "
		case severityWarn:
			marker = "⚠ "
		case severityOK:
			marker = "✓ "
		case severitySkip:
			marker = "· "
		}
		fmt.Printf("%s[%s] %s — %s\n", marker, c.Severity, c.Name, c.Message)
	}
	fmt.Println()
	switch {
	case r.Blockers > 0:
		fmt.Printf("summary: %d blocker(s), %d warning(s); exit 1\n", r.Blockers, r.Warnings)
	case r.Warnings > 0:
		fmt.Printf("summary: clear of blockers; %d warning(s) for operator attention\n", r.Warnings)
	default:
		fmt.Println("summary: all checks passed")
	}
}

func emitDoctorJSON(r doctorReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func doctorUsage(err error) error {
	if err == flag.ErrHelp {
		return doctorHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, doctorHelp())
}

func doctorHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, doctorHelp())
}

func doctorHelp() string {
	return strings.TrimSpace(`
Usage: agentchute doctor [--as <id>] [--json]

Diagnostic aggregator. Runs an ordered set of checks against the local
loop directory, the calling environment, and (if --as is provided) the
named agent's registration / inbox / ledger / wake target / recipient
liveness. Reports each check with a severity (BLOCKER / WARN / OK / SKIP)
and exits nonzero when any BLOCKER is found.

Doctor diagnoses setup readiness. boot/gate own the blocking surface for
unread mail, pending replies, and recipient liveness during normal operation.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID); optional
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output

Service generator (v0.2):
  --generate-service <kind>  emit a unit/script for the preflighted-scheduler
                             pattern (round-3 synthesis tier 2). Kind is one of:
                             launchd | systemd-service | systemd-timer | script.
                             Generated schedulers call self-poll --heartbeat.
                             Doctor emits ONLY; install/load/start is the
                             operator's responsibility.
  --as <id>                  required with --generate-service
  --vendor <v>               wrapper vendor (inferred for claude-code / codex /
                             gemini-cli)
  --interval <n>             poll interval in seconds (default 30, min 5)
  --repo <path>              working directory for the service (default: cwd)
  --command <cmd>            override the full wrapper invocation (advanced)
  --out <path>               write to file (default: stdout, mode 0600)
`)
}
