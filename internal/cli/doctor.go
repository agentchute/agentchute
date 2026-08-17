package cli

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
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

	// Any invocation form followed by its subcommand token (capture 1).
	// Flags (leading `-`) never match the token class; (?m) so a bare
	// invocation at the start of a joined-body line still anchors. The
	// bare-form guard mirrors hookBareAgentchuteRE.
	hookSubcmdTokenRE = regexp.MustCompile(`(?m)(?:\$\{AGENTCHUTE_BIN:-agentchute\}|\$AGENTCHUTE_BIN|(?:^|[^A-Za-z0-9_/\-{])agentchute)[ \t]+([a-z][a-z0-9-]*)`)
)

const (
	staleTempFilePrefix = ".tmp_"
	staleTempFileAge    = time.Hour
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
//   - SKIP:    check is not applicable in this context (the setup wake mode
//     does not include the runner; --as not provided so agent-specific check
//     skipped).
//   - OK:      check passed.
func cmdDoctor(args []string) error {
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

	runningExecutable, _ := os.Executable()
	opts := doctorOptions{
		Now:               time.Now().UTC(),
		PathEnv:           os.Getenv("PATH"),
		RunningExecutable: runningExecutable,
		RunningVersion:    version,
	}
	if gs, err := readSetupGlobalState(); err == nil {
		opts.GlobalState = &gs
	}
	if ps, err := readSetupPoolState(cfg); err == nil {
		opts.PoolState = &ps
	}

	if agentID != "" {
		agentID, err = resolveAgentID(agentID, cfg)
		if err != nil {
			return err
		}
	}

	var report doctorReport
	if cfg.Remote != nil {
		report = runRemoteDoctorChecks(cfg, agentID, opts.Now)
	} else {
		report = runDoctorChecks(cfg, agentID, opts)
	}

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

func runRemoteDoctorChecks(cfg *loop.Config, agentID string, now time.Time) doctorReport {
	report := doctorReport{Agent: agentID}
	add := func(check doctorCheck) {
		report.Checks = append(report.Checks, check)
		switch check.Severity {
		case severityBlocker:
			report.Blockers++
		case severityWarn:
			report.Warnings++
		}
	}
	hubCfg, err := hubclient.ReadHubConfig(cfg.Remote.HubID)
	if err != nil {
		add(doctorCheck{Name: "hub_config", Severity: severityBlocker, Message: err.Error()})
		return report
	}
	joined := strings.Join(hubCfg.JoinedAs, ", ")
	add(doctorCheck{Name: "hub_config", Severity: severityOK, Message: fmt.Sprintf("%s, joined as %s", hubCfg.URL, joined)})
	if agentID == "" && len(hubCfg.JoinedAs) > 0 {
		agentID = hubCfg.JoinedAs[0]
		report.Agent = agentID
	}
	key := filepath.Join(cfg.Remote.HubDir, "keys", agentID+"_ed25519")
	keyInfo, keyErr := os.Lstat(key)
	if keyErr != nil {
		add(doctorCheck{Name: "hub_key", Severity: severityBlocker, Message: fmt.Sprintf("key unavailable at %s: %v", key, keyErr)})
	} else if keyInfo.Mode()&os.ModeSymlink == 0 {
		add(doctorCheck{Name: "hub_key", Severity: severityBlocker, Message: fmt.Sprintf("%s is not the active-key symlink", key)})
	} else if target, err := filepath.EvalSymlinks(key); err != nil {
		add(doctorCheck{Name: "hub_key", Severity: severityBlocker, Message: fmt.Sprintf("active key target invalid: %v", err)})
	} else if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		add(doctorCheck{Name: "hub_key", Severity: severityBlocker, Message: fmt.Sprintf("active key target must be a 0600 file: %s", target)})
	} else {
		add(doctorCheck{Name: "hub_key", Severity: severityOK, Message: fmt.Sprintf("%s -> %s, 0600", key, filepath.Base(target))})
	}
	if cached, age, err := hubclient.ConnectFailureCached(cfg.Remote, now); err == nil && cached {
		add(doctorCheck{Name: "hub_negative_cache", Severity: severityWarn, Message: fmt.Sprintf("hooks are suppressing repeat connects after E_CONNECT %s ago", age.Round(time.Second))})
	}
	if agentID == "" {
		add(doctorCheck{Name: "hub_connect", Severity: severityBlocker, Message: "no joined identity available for the hub probe"})
		return report
	}
	started := time.Now()
	session, err := openRemoteOneShot(cfg, agentID)
	if err != nil {
		code := hubclient.ErrorCode(err)
		if code == "" {
			code = "E_CONNECT"
		}
		add(doctorCheck{Name: "hub_connect", Severity: severityBlocker, Message: fmt.Sprintf("%s: %v", code, err)})
		return report
	}
	hello := session.Hello()
	_ = session.Close()
	add(doctorCheck{Name: "hub_connect", Severity: severityOK, Message: fmt.Sprintf("rtt %s", time.Since(started).Round(time.Millisecond))})
	if hello.Agent != agentID {
		add(doctorCheck{Name: "hub_identity", Severity: severityBlocker, Message: fmt.Sprintf("key pinned to %s, acting as %s", hello.Agent, agentID)})
	} else {
		add(doctorCheck{Name: "hub_identity", Severity: severityOK, Message: fmt.Sprintf("key pinned to %s; protocol %s v%d; hub binary %s", hello.Agent, hubwire.Protocol, hello.V, hello.HubBin)})
	}
	if hello.Pool != hubCfg.Pool || hello.Pool12 != hubCfg.Pool12 {
		add(doctorCheck{Name: "hub_pool", Severity: severityBlocker, Message: fmt.Sprintf("E_POOL_MISMATCH: joined %s/%s, hub reports %s/%s", hubCfg.Pool, hubCfg.Pool12, hello.Pool, hello.Pool12)})
	} else if !hello.Writable {
		add(doctorCheck{Name: "hub_pool", Severity: severityBlocker, Message: "hub pool is not writable"})
	} else {
		offset := hello.HubTime.Sub(time.Now().UTC()).Round(100 * time.Millisecond)
		add(doctorCheck{Name: "hub_pool", Severity: severityOK, Message: fmt.Sprintf("writable; hub time offset %s", offset)})
	}
	if envID := strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")); envID != "" {
		if mapped, ok := hubCfg.Names[envID]; ok && mapped != envID {
			add(doctorCheck{Name: "hub_identity_env", Severity: severityWarn, Message: fmt.Sprintf("AGENTCHUTE_AGENT_ID=%s is a local name on this machine; every command resolves it to %q (this hub's names map). Unset it, or export the full id, if that is not what you want.", envID, mapped)})
		}
	}
	return report
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
	Now               time.Time
	PathEnv           string
	GlobalState       *setupGlobalState
	PoolState         *setupPoolState
	RunningExecutable string
	RunningVersion    string
	ReadBinaryVersion func(string) (string, error)
}

// runDoctorChecks executes the canonical check sequence and returns a
// fully-populated report.
func runDoctorChecks(cfg *loop.Config, agentID string, opts doctorOptions) doctorReport {
	checks := []doctorCheck{
		checkLoopDirScaffold(cfg),
		checkSpecFreshness(cfg),
		checkProtocolVersions(cfg),
		checkStaleTempFiles(cfg, opts.Now),
		checkBinaryOnPath(),
		checkBinaryIdentity(opts),
		checkHookFilePresence(cfg, agentID),
		checkHookContentSanity(cfg),
		checkWrapperShadowing(cfg, agentID, opts),
	}
	if agentID != "" {
		checks = append(checks,
			checkSelfRegistration(cfg, agentID),
			checkRegistrationFreshness(cfg, agentID, opts.Now),
			checkInboxState(cfg, agentID),
			checkGuardLatchAge(cfg, agentID, opts.Now),
		)
	} else {
		checks = append(checks, doctorCheck{
			Name:     "agent_specific_checks",
			Severity: severitySkip,
			Message:  "no --as / $AGENTCHUTE_AGENT_ID; skipped per-agent checks (registration freshness, inbox state)",
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

func checkSpecFreshness(cfg *loop.Config) doctorCheck {
	const name = "spec_freshness"
	if cfg == nil || cfg.ControlRepo == "" {
		return doctorCheck{Name: name, Severity: severitySkip, Message: "control repo unavailable; skipping AGENTCHUTE.md freshness check"}
	}
	if embeddedSpecContent == "" {
		return doctorCheck{Name: name, Severity: severitySkip, Message: "embedded AGENTCHUTE.md unavailable; skipping spec freshness check"}
	}
	path := filepath.Join(cfg.ControlRepo, "AGENTCHUTE.md")
	onDisk, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doctorCheck{Name: name, Severity: severitySkip, Message: "AGENTCHUTE.md missing; discovery/scaffold checks own this"}
		}
		return doctorCheck{Name: name, Severity: severityWarn, Message: fmt.Sprintf("AGENTCHUTE.md unreadable at %s: %v", path, err)}
	}
	embedded := []byte(embeddedSpecContent)
	if bytes.Equal(onDisk, embedded) {
		return doctorCheck{Name: name, Severity: severityOK, Message: "AGENTCHUTE.md matches embedded spec"}
	}
	// planSpecFile (init.go) now version-compares and auto-replaces AGENTCHUTE.md
	// on every setup/update resync, same as the enrollment blocks — so the disk
	// copy differing is only ever a WARN needing a resync, unless it is marked
	// newer than this binary knows about (deliberately future-dated), which
	// planSpecFile leaves alone on purpose. Tell the operator which case this is
	// rather than a generic "this may be expected" hedge.
	if match := specMarkerRE.FindStringSubmatch(string(onDisk)); match != nil {
		if diskVersion, err := strconv.Atoi(match[1]); err == nil && diskVersion > specVersion {
			return doctorCheck{
				Name:     name,
				Severity: severityWarn,
				Message: fmt.Sprintf("AGENTCHUTE.md is marked v%d, newer than this binary's embedded v%d (disk sha256=%s, embedded sha256=%s); this is left alone on purpose — upgrade agentchute to manage it",
					diskVersion, specVersion, shortSHA256(onDisk), shortSHA256(embedded)),
			}
		}
	}
	return doctorCheck{
		Name:     name,
		Severity: severityWarn,
		Message: fmt.Sprintf("AGENTCHUTE.md differs from this binary's embedded spec (disk sha256=%s, embedded sha256=%s); stale — run `agentchute setup --yes` or `agentchute update` to refresh it automatically",
			shortSHA256(onDisk), shortSHA256(embedded)),
	}
}

func shortSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])[:12]
}

func checkProtocolVersions(cfg *loop.Config) doctorCheck {
	regs, errs := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	if len(errs) > 0 {
		return doctorCheck{
			Name:     "protocol_version",
			Severity: severityWarn,
			Message:  fmt.Sprintf("registration protocol-version scan skipped %d unreadable registration(s)", len(errs)),
		}
	}

	regsMap := make(map[string]*loop.Registration)
	for _, reg := range regs {
		regsMap[reg.AgentID] = reg
	}

	var warnings []string
	for _, id := range loop.RegistrationsByAgentID(regsMap) {
		if warning := protocolVersionWarning(regsMap[id]); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) == 0 {
		return doctorCheck{Name: "protocol_version", Severity: severityOK, Message: fmt.Sprintf("no explicit protocol-version mismatches; expected %s", protocolVersionLabel(loop.CurrentProtocolVersion))}
	}
	return doctorCheck{
		Name:     "protocol_version",
		Severity: severityWarn,
		Message:  strings.Join(warnings, "; "),
	}
}

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

type staleTempFile struct {
	path string
	age  time.Duration
}

func checkStaleTempFiles(cfg *loop.Config, now time.Time) doctorCheck {
	stale, err := findStaleTempFiles(cfg, now, staleTempFileAge)
	if err != nil {
		return doctorCheck{Name: "stale_temp_files", Severity: severityWarn, Message: fmt.Sprintf("stale temp scan error: %v", err)}
	}
	if len(stale) == 0 {
		return doctorCheck{Name: "stale_temp_files", Severity: severityOK, Message: "no stale .tmp_* files found"}
	}
	return doctorCheck{
		Name:     "stale_temp_files",
		Severity: severityWarn,
		Message:  fmt.Sprintf("%d stale .tmp_* file(s) older than %s: %s", len(stale), staleTempFileAge, formatStaleTempFiles(cfg, stale)),
	}
}

func findStaleTempFiles(cfg *loop.Config, now time.Time, olderThan time.Duration) ([]staleTempFile, error) {
	var stale []staleTempFile
	scanDir := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), staleTempFilePrefix) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			age := now.Sub(info.ModTime())
			if age > olderThan {
				stale = append(stale, staleTempFile{path: filepath.Join(dir, entry.Name()), age: age})
			}
		}
		return nil
	}
	scanChildDirs := func(parent string) error {
		children, err := os.ReadDir(parent)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			if err := scanDir(filepath.Join(parent, child.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if err := scanChildDirs(filepath.Join(cfg.LoopDir, "inbox")); err != nil {
		return nil, err
	}
	if err := scanChildDirs(filepath.Join(cfg.LoopDir, "state")); err != nil {
		return nil, err
	}
	if err := scanDir(cfg.AgentsDir()); err != nil {
		return nil, err
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].path < stale[j].path })
	return stale, nil
}

func formatStaleTempFiles(cfg *loop.Config, files []staleTempFile) string {
	const maxShown = 5
	parts := make([]string, 0, minInt(len(files), maxShown))
	for i, f := range files {
		if i >= maxShown {
			break
		}
		path := f.path
		if rel, err := filepath.Rel(cfg.ControlRepo, f.path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
		parts = append(parts, fmt.Sprintf("%s (%s old)", path, f.age.Round(time.Minute)))
	}
	if len(files) > maxShown {
		parts = append(parts, fmt.Sprintf("... %d more", len(files)-maxShown))
	}
	return strings.Join(parts, ", ")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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

type binaryIdentity struct {
	label   string
	path    string
	version string
}

// checkBinaryIdentity verifies that the running binary, the generated ac
// dispatcher's pinned binary, and bare agentchute on PATH report one version.
// Missing/unreadable sources remain owned by the existing binary_on_path and
// ac_dispatcher checks; a proven version mismatch is the only new blocker.
func checkBinaryIdentity(opts doctorOptions) doctorCheck {
	const name = "binary_identity"
	if strings.TrimSpace(opts.RunningExecutable) == "" {
		return doctorCheck{Name: name, Severity: severitySkip, Message: "running executable unavailable; skipping binary version parity"}
	}

	readVersion := opts.ReadBinaryVersion
	if readVersion == nil {
		readVersion = readAgentchuteBuildVersion
	}
	runningVersion := normalizeBinaryVersion(opts.RunningVersion)
	if runningVersion == "" {
		var err error
		runningVersion, err = readVersion(opts.RunningExecutable)
		if err != nil {
			return doctorCheck{Name: name, Severity: severitySkip, Message: fmt.Sprintf("cannot read running binary version: %v", err)}
		}
		runningVersion = normalizeBinaryVersion(runningVersion)
	}

	identities := []binaryIdentity{{label: "running", path: opts.RunningExecutable, version: runningVersion}}
	addIdentity := func(label, path string) {
		if strings.TrimSpace(path) == "" || executableFileProblem(path) != "" {
			return
		}
		candidateVersion := runningVersion
		if !samePath(path, opts.RunningExecutable) {
			var err error
			candidateVersion, err = readVersion(path)
			if err != nil {
				return
			}
			candidateVersion = normalizeBinaryVersion(candidateVersion)
		}
		if candidateVersion != "" {
			identities = append(identities, binaryIdentity{label: label, path: path, version: candidateVersion})
		}
	}

	if target, err := dispatcherBinaryTarget(opts); err == nil {
		addIdentity("dispatcher", target)
	}
	if pathBinary, err := resolveExecutableOnPath("agentchute", opts.PathEnv); err == nil {
		addIdentity("PATH", pathBinary)
	}

	parts := make([]string, 0, len(identities))
	mismatch := false
	for _, identity := range identities {
		parts = append(parts, fmt.Sprintf("%s=%s (%s)", identity.label, identity.version, identity.path))
		if identity.version != runningVersion {
			mismatch = true
		}
	}
	if mismatch {
		return doctorCheck{
			Name:     name,
			Severity: severityBlocker,
			Message:  fmt.Sprintf("agentchute binary version mismatch: %s; rerun `agentchute setup` with the intended binary and fix PATH before launching wrappers", strings.Join(parts, ", ")),
		}
	}
	if len(identities) < 2 {
		return doctorCheck{Name: name, Severity: severitySkip, Message: "only the running agentchute binary version could be inspected; existing path/dispatcher checks own missing sources"}
	}
	return doctorCheck{Name: name, Severity: severityOK, Message: fmt.Sprintf("agentchute binary versions agree: %s", strings.Join(parts, ", "))}
}

func dispatcherBinaryTarget(opts doctorOptions) (string, error) {
	if opts.GlobalState == nil || strings.TrimSpace(opts.GlobalState.ShimDir) == "" {
		return "", errors.New("dispatcher shim directory unavailable")
	}
	data, err := os.ReadFile(filepath.Join(opts.GlobalState.ShimDir, "ac"))
	if err != nil {
		return "", err
	}
	const prefix = "AGENTCHUTE_BIN=${AGENTCHUTE_BIN:-"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
			continue
		}
		return unquoteDispatcherValue(strings.TrimSuffix(strings.TrimPrefix(line, prefix), "}"))
	}
	return "", errors.New("dispatcher binary assignment not found")
}

func unquoteDispatcherValue(value string) (string, error) {
	if len(value) < 2 || value[0] != '\'' || value[len(value)-1] != '\'' {
		return "", fmt.Errorf("unsupported dispatcher binary quoting %q", value)
	}
	const escapedQuote = "'\\''"
	inner := value[1 : len(value)-1]
	marked := strings.ReplaceAll(inner, escapedQuote, "\x00")
	if strings.Contains(marked, "'") {
		return "", fmt.Errorf("unsupported dispatcher binary quoting %q", value)
	}
	return strings.ReplaceAll(marked, "\x00", "'"), nil
}

func resolveExecutableOnPath(name, pathEnv string) (string, error) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		candidate := filepath.Join(absDir, name)
		if executableFileProblem(candidate) == "" {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", name)
}

func readAgentchuteBuildVersion(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, setting := range info.Settings {
		if setting.Key != "-ldflags" {
			continue
		}
		if value := mainVersionFromLDFlags(setting.Value); value != "" {
			return value, nil
		}
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version, nil
	}
	return "dev", nil
}

func mainVersionFromLDFlags(flags string) string {
	fields := strings.Fields(strings.Trim(flags, "\""))
	for i := 0; i < len(fields); i++ {
		assignment := ""
		switch {
		case fields[i] == "-X" && i+1 < len(fields):
			i++
			assignment = fields[i]
		case strings.HasPrefix(fields[i], "-X="):
			assignment = strings.TrimPrefix(fields[i], "-X=")
		}
		assignment = strings.Trim(assignment, "\"'")
		if strings.HasPrefix(assignment, "main.version=") {
			return strings.TrimPrefix(assignment, "main.version=")
		}
	}
	return ""
}

func normalizeBinaryVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
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

func shimNamesForAgent(agentID string) []string {
	agentID = strings.TrimSpace(agentID)
	if agentID != "" {
		for _, spec := range wrapperSpecs {
			// Match explicitly named ids (codex-review) to their canonical shim,
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

// Wrapper hook locations come from hookWrappers (hooks.go) — the same
// table `hooks install` writes from — so doctor can never check a
// different path than install wrote (two hand-maintained copies of the
// three paths drifted into being during v2; deleted with the v1.5.0
// cutover fix).

func checkHookFilePresence(cfg *loop.Config, agentID string) doctorCheck {
	present := []string{}
	presentSet := map[string]bool{}
	drifted := []string{}
	for _, h := range hookWrappers {
		full := filepath.Join(cfg.ControlRepo, filepath.FromSlash(h.Dest))
		installed, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return doctorCheck{
				Name:     "hook_file_presence",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("installed hook for %s is unreadable at %s: %v", h.Name, full, err),
			}
		}
		present = append(present, h.Name)
		presentSet[h.Name] = true
		canonical, err := fs.ReadFile(hooksFS, h.Src)
		if err != nil {
			return doctorCheck{
				Name:     "hook_file_presence",
				Severity: severityBlocker,
				Message:  fmt.Sprintf("canonical hook template for %s is unreadable: %v", h.Name, err),
			}
		}
		if !bytes.Equal(installed, canonical) {
			drifted = append(drifted, h.Name)
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
	}
	if len(drifted) > 0 {
		return doctorCheck{
			Name:     "hook_file_presence",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("installed hook template(s) differ from the canonical embed: %s; run `agentchute hooks install --wrapper all --scope repo --force`", strings.Join(drifted, ", ")),
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

// hookWrapperForAgent resolves an agent id to its canonical hookable wrapper.
// Explicit lane names may retain a canonical wrapper prefix (e.g. codex-review),
// so match by canonical base — exact or "<base>-" prefix — not exact base only.
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

// hookCommandsDoc models the shape shared by every wrapper's hook JSON file
// (claude .claude/settings.json, codex .codex/hooks.json, gemini
// .gemini/settings.json all nest {"hooks": {<Event>: [{"hooks":
// [{"command": ...}]}]}} identically, modulo event names and unrelated
// per-hook fields like "matcher"/"timeout"/"statusMessage").
type hookCommandsDoc struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// hookCommandBody extracts and joins every hook command string from a
// wrapper's hook JSON file, so checkHookContentSanity's regexes scan only
// actual hook invocations — not unrelated top-level keys like
// `permissions`. Without this, a `permissions.allow` entry that merely
// *names* an agentchute subcommand (e.g. `"Bash(agentchute check:*)"`) reads
// as if it were a hook literally invoking that subcommand (bug: #74 added
// such an entry and tripped a false BLOCKER). Returns an error if data
// isn't valid JSON; callers must NOT fall back to scanning the raw file
// body in that case — that would re-open the same false-positive (a
// permissions/other key naming a subcommand inside a malformed file would
// still match). Surface the parse failure as its own signal instead.
func hookCommandBody(data []byte) (string, error) {
	var doc hookCommandsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	var commands []string
	for _, entries := range doc.Hooks {
		for _, entry := range entries {
			for _, h := range entry.Hooks {
				commands = append(commands, h.Command)
			}
		}
	}
	return strings.Join(commands, "\n"), nil
}

// hookBodyUnknownSubcommands returns, in first-occurrence order, the
// distinct agentchute subcommand tokens referenced in a hook file's parsed
// command body (see hookCommandBody) that this binary does not recognize —
// the v1.5.0 cutover outage class (docs/decisions/agentchute-v150-cutover-
// incident-and-fix.md): a stale template invoking a removed subcommand dies
// with `unknown command`, and a failing UserPromptSubmit hook blocks the
// prompt entirely. Shared by doctor's hook_content_sanity BLOCKER and
// setup's post-resync hook-compatibility verification (update-fix-v2, docs/
// decisions/agentchute-update-fix-v2.md) so the two checks can never drift
// on what counts as "broken."
func hookBodyUnknownSubcommands(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range hookSubcmdTokenRE.FindAllStringSubmatch(body, -1) {
		tok := m[1]
		if _, known := commandHandlers[tok]; known || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
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
	var unknownOffenders []string
	var resolutionOffenders []string
	var invalidJSONFiles []string

	for _, h := range hookWrappers {
		full := filepath.Join(cfg.ControlRepo, filepath.FromSlash(h.Dest))
		data, err := os.ReadFile(full)
		if err != nil {
			continue // absence is handled by checkHookFilePresence
		}
		body, err := hookCommandBody(data)
		if err != nil {
			// Not valid JSON. Do NOT fall back to raw-body scanning here —
			// that would re-open the exact false-positive this function
			// exists to avoid (a permissions/other key merely naming a
			// subcommand would read as a hook invoking it). Surface the
			// parse failure as its own signal instead; a malformed hook
			// file won't fire correctly anyway, which is worth flagging.
			invalidJSONFiles = append(invalidJSONFiles, h.Name)
			continue
		}

		if hookCheckSubcmdRE.MatchString(body) {
			checkOffenders = append(checkOffenders, h.Name)
		}

		// A subcommand the running binary doesn't know is a stale template
		// from another binary version — the v1.5.0 cutover outage class
		// (docs/decisions/agentchute-v150-cutover-incident-and-fix.md):
		// every such hook dies with `unknown command`, and a failing
		// UserPromptSubmit hook blocks the prompt entirely.
		for _, tok := range hookBodyUnknownSubcommands(body) {
			unknownOffenders = append(unknownOffenders, h.Name+" (`"+tok+"`)")
		}

		hasBare := hookBareAgentchuteRE.MatchString(body)
		hasTemplated := hookTemplatedRE.MatchString(body)
		hasEnvOnly := hookEnvOnlyRE.MatchString(body)

		// Each form's resolution is checked independently. A mixed file
		// with one bare + one templated invocation will be flagged if
		// either form can't resolve in this environment.
		switch {
		case hasBare && !binOnPath:
			resolutionOffenders = append(resolutionOffenders, h.Name+" (bare `agentchute` needs PATH)")
		case hasTemplated && !envBinValid && !binOnPath:
			resolutionOffenders = append(resolutionOffenders, h.Name+" (templated `${AGENTCHUTE_BIN:-agentchute}` needs AGENTCHUTE_BIN or PATH)")
		case hasEnvOnly && !envBinValid:
			resolutionOffenders = append(resolutionOffenders, h.Name+" (`$AGENTCHUTE_BIN` reference needs AGENTCHUTE_BIN set)")
		}
	}
	if len(checkOffenders) > 0 {
		return doctorCheck{
			Name:     "hook_content_sanity",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("hook file(s) invoke `agentchute check` (silent-drain risk; check archives and quarantines): %s — replace with `pending` or `boot --context-only`", strings.Join(checkOffenders, ", ")),
		}
	}
	if len(unknownOffenders) > 0 {
		return doctorCheck{
			Name:     "hook_content_sanity",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("hook file(s) invoke unknown agentchute subcommand(s) — stale templates from another binary version: %s — run `agentchute hooks install --wrapper all --scope repo --force`", strings.Join(unknownOffenders, ", ")),
		}
	}
	if len(resolutionOffenders) > 0 {
		return doctorCheck{
			Name:     "hook_content_sanity",
			Severity: severityBlocker,
			Message:  fmt.Sprintf("hook file(s) reference agentchute commands that cannot resolve in this environment: %s", strings.Join(resolutionOffenders, ", ")),
		}
	}
	if len(invalidJSONFiles) > 0 {
		return doctorCheck{
			Name:     "hook_content_sanity",
			Severity: severityWarn,
			Message:  fmt.Sprintf("hook file(s) are not valid JSON, cannot verify hook command safety: %s — fix the JSON syntax", strings.Join(invalidJSONFiles, ", ")),
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

// checkRegistrationFreshness reports presence freshness. v2.5 plan B5: the
// freshness SOURCE is registration last_seen itself (`.live` is deleted). The
// check name ("registration_freshness"), StaleRegThreshold, and severities
// are unchanged; the remediation text now implements "report, never act" —
// doctor never sweeps or otherwise mutates state on the caller's behalf, it
// just names what will eventually happen on its own (a stale row re-registers
// at boot, or is swept lazily after stale_after).
func checkRegistrationFreshness(cfg *loop.Config, agentID string, now time.Time) doctorCheck {
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if err != nil {
		return doctorCheck{Name: "registration_freshness", Severity: severitySkip, Message: "registration unreadable (see self_registration)"}
	}
	age := now.Sub(reg.LastSeen)
	if age < 0 {
		age = 0 // future-dated (clock skew) reads as fresh.
	}
	if age > StaleRegThreshold {
		return doctorCheck{
			Name:     "registration_freshness",
			Severity: severityWarn,
			Message:  fmt.Sprintf("agent is stale (last_seen age %s exceeds %s threshold); it re-registers at boot (would be swept after %s)", age.Round(time.Second), StaleRegThreshold, loop.StaleAfter(cfg)),
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
	if len(skipped) > 0 {
		return doctorCheck{
			Name:     "inbox_state",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%d unread + %d malformed file(s) in inbox; malformed files block `gate --before finish` until quarantined via `check`", len(msgs), len(skipped)),
		}
	}
	if len(msgs) > 0 {
		return doctorCheck{
			Name:     "inbox_state",
			Severity: severityWarn,
			Message:  fmt.Sprintf("%d unread direct message(s) in inbox", len(msgs)),
		}
	}
	return doctorCheck{Name: "inbox_state", Severity: severityOK, Message: "inbox clear"}
}

// guardLatchStaleWarnThreshold is how old a guard latch (v2.5 A7) must be
// before doctor surfaces it as a WARN. Read-only diagnostic only — doctor
// never acts on this, it only reports (build-nothing ruling on the mixed
// hook-trust wedge: codex's finding that any recovery mechanism here is
// undermined by the latch being a same-UID-writable state file meant no new
// enforcement code ships, only this visibility). Longer than a single normal
// turn should plausibly take, short enough that a genuinely wedged lane
// (its end-of-turn hook not running `turn-end`) doesn't go unnoticed for
// long.
const guardLatchStaleWarnThreshold = 15 * time.Minute

// checkGuardLatchAge reports how long agentID's guard latch (if any) has
// been held. A latch older than guardLatchStaleWarnThreshold WARNs with the
// full remediation sequence (§15 AGENTCHUTE.md): repair the end-of-turn hook
// FIRST; only then does relaunching or deleting the latch become durable,
// rather than a temporary unwedge that re-latches on the very next check.
func checkGuardLatchAge(cfg *loop.Config, agentID string, now time.Time) doctorCheck {
	// PeekGuardLatch, not ReadGuardLatch: the latter takes WithAgentLock,
	// which creates state/<id>/ and its lock file as a side effect — a
	// write this strictly read-only diagnostic must never perform (codex
	// review, PR #89 round 5).
	latch, err := loop.PeekGuardLatch(cfg, agentID)
	if err != nil {
		if os.IsNotExist(err) {
			return doctorCheck{Name: "guard_latch_age", Severity: severityOK, Message: "no guard latch"}
		}
		return doctorCheck{
			Name:     "guard_latch_age",
			Severity: severityWarn,
			Message:  fmt.Sprintf("guard latch for %s is unreadable/corrupt (%v)", agentID, err),
		}
	}
	age := now.Sub(latch.SetAt)
	if age < 0 {
		age = 0 // future-dated (clock skew) reads as fresh.
	}
	if age < guardLatchStaleWarnThreshold {
		return doctorCheck{
			Name:     "guard_latch_age",
			Severity: severityOK,
			Message:  fmt.Sprintf("guard latch set %s ago (session %s)", age.Round(time.Second), latch.Session),
		}
	}
	return doctorCheck{
		Name:     "guard_latch_age",
		Severity: severityWarn,
		Message: fmt.Sprintf(
			"guard latch for %s has been held for %s (since %s) — if its Stop/end-of-turn hook is not running `turn-end` (a mixed hook-trust state: hook definitions untrusted after a change, individually disabled, or failing at runtime), repair that hook FIRST; only then does relaunching the lane or removing state/%s/guard.latch (and immediately running `agentchute turn-end`) become durable — doing either before the hook is fixed is a temporary unwedge that re-latches on the next check",
			agentID, age.Round(time.Second), latch.SetAt.UTC().Format(time.RFC3339), agentID,
		),
	}
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
named agent's registration / inbox / recipient liveness. Reports each
check with a severity (BLOCKER / WARN / OK / SKIP) and exits nonzero when
any BLOCKER is found.

Doctor diagnoses setup readiness. boot/gate own the blocking surface for
unread mail and recipient liveness during normal operation.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID); optional
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output
`)
}
