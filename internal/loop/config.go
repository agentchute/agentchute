package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	specFileName = "AGENTCHUTE.md"
	loopDirName  = "loop"
)

var ErrNoControlRepo = errors.New("no agentchute control repo")

func IsNoControlRepo(err error) bool {
	return errors.Is(err, ErrNoControlRepo)
}

// Config is the resolved agentchute control location.
type Config struct {
	ControlRepo string // absolute repo root containing AGENTCHUTE.md
	LoopDir     string // absolute .<vendor>/loop directory
	Vendor      string // vendor namespace, without leading dot

	// ControlRepoOrigin records which step of the discovery cascade resolved
	// ControlRepo, for visibility in `status` and `register` output. Values:
	// "flag", "env", "pointer:<file>", "cwd".
	ControlRepoOrigin string

	// LoopDirOrigin records which step resolved LoopDir. Values: "flag",
	// "env", "auto" (single vendor loop dir under ControlRepo).
	LoopDirOrigin string

	// ShadowedPointers lists any pointer files that were skipped during
	// discovery because a nearer pointer won. Informational; surfaced in
	// `status` for diagnostics.
	ShadowedPointers []string
}

// DiscoverOpts makes discovery deterministic in tests and command wrappers.
type DiscoverOpts struct {
	ControlRepoFlag string // --control-repo
	LoopDirFlag     string // --loop-dir
	Cwd             string // caller's working directory
	EnvControlRepo  string // AGENTCHUTE_CONTROL_REPO
	EnvLoopDir      string // AGENTCHUTE_LOOP_DIR
}

// Discover resolves the control repo and loop directory.
//
// Control repo discovery follows the AGENTCHUTE.md §4 cascade, in this order:
//  1. --control-repo flag (explicit)
//  2. AGENTCHUTE_CONTROL_REPO env var
//  3. .agentchute-control-repo pointer file (cwd or any ancestor; nearest wins)
//  4. Walk up from cwd looking for AGENTCHUTE.md + vendor loop dir
//
// First hit wins. The resulting Config records which step won via
// ControlRepoOrigin for visibility.
func Discover(opts DiscoverOpts) (*Config, error) {
	controlRepo, origin, shadowed, err := discoverControlRepo(opts)
	if err != nil {
		return nil, err
	}

	loopDir, loopOrigin, err := discoverLoopDir(controlRepo, opts)
	if err != nil {
		return nil, err
	}

	vendor, err := vendorFromLoopDir(loopDir)
	if err != nil {
		return nil, err
	}

	return &Config{
		ControlRepo:       controlRepo,
		LoopDir:           loopDir,
		Vendor:            vendor,
		ControlRepoOrigin: origin,
		LoopDirOrigin:     loopOrigin,
		ShadowedPointers:  shadowed,
	}, nil
}

// AgentRegistrationPath returns the live registration path for agentID.
func (c *Config) AgentRegistrationPath(agentID string) string {
	return filepath.Join(c.LoopDir, "agents", agentID+".md")
}

// AgentsDir returns the live registration directory.
func (c *Config) AgentsDir() string {
	return filepath.Join(c.LoopDir, "agents")
}

// AgentInboxDir returns the inbox directory for agentID.
func (c *Config) AgentInboxDir(agentID string) string {
	return filepath.Join(c.LoopDir, "inbox", agentID)
}

// AgentClaimedDir returns the two-phase-consume CLAIM directory for agentID, a
// dot-prefixed child of the inbox so the normal inbox lister never descends into
// it. `check` moves a message here (phase 1, CLAIM) under its CANONICAL name (no
// archive timestamp); `ack` archives it (phase 2, COMMIT). A crash between the
// two re-delivers the residue (at-least-once). Same owner-only 0700 posture as
// the inbox; created lazily via ensurePrivateDir on the first claim.
func (c *Config) AgentClaimedDir(agentID string) string {
	return filepath.Join(c.AgentInboxDir(agentID), ".claimed")
}

// ArchiveDir returns the consumed-message archive directory.
func (c *Config) ArchiveDir() string {
	return filepath.Join(c.LoopDir, "archive")
}

// MalformedDir returns the quarantine directory for files that violate the
// AGENTCHUTE.md §6.1 reference filename encoding or §6.4 reference
// frontmatter shape — used by §11 protocol enforcement.
func (c *Config) MalformedDir() string {
	return filepath.Join(c.LoopDir, "malformed")
}

// AgentStateDir returns the per-agent local-state directory at
// <loop>/state/<agent>. Used for recipient-owned state files such as the
// pending-reply ledger (§6.4); kept out of inbox/archive/malformed because
// it's not part of the wire protocol — peers never read another agent's
// state dir.
func (c *Config) AgentStateDir(agentID string) string {
	return filepath.Join(c.LoopDir, "state", agentID)
}

// PendingRepliesPath returns the per-agent pending-replies ledger path
// (§6.4). Recipient-owned; updated by `check` (on archive
// of a reply_required message), `send --reply-to`, and `defer`.
func (c *Config) PendingRepliesPath(agentID string) string {
	return filepath.Join(c.AgentStateDir(agentID), "pending-replies.json")
}

// PollerHeartbeatPath returns the per-agent poller heartbeat path. This is
// recipient-owned liveness state: senders never rely on it for delivery, but
// local lifecycle checks use it to prove that a non-pokable recipient has a
// self-poll loop running.
func (c *Config) PollerHeartbeatPath(agentID string) string {
	return filepath.Join(c.AgentStateDir(agentID), "poller.json")
}

// ActiveSessionPath returns the per-agent active wrapper heartbeat path. This
// is local lifecycle state written by hook-driven boot/self-check commands so
// gate can distinguish a live visible wrapper from an off-turn poller.
func (c *Config) ActiveSessionPath(agentID string) string {
	return filepath.Join(c.AgentStateDir(agentID), "session.json")
}

// RunnerStatePath returns the per-agent runner state path. This is
// recipient-owned local diagnostic/recovery state for agentchute's PTY runner;
// it is not part of the wire protocol.
func (c *Config) RunnerStatePath(agentID string) string {
	return filepath.Join(c.AgentStateDir(agentID), "runner.json")
}

// RunnerSocketPath returns the default local Unix socket path for the
// agentchute-run wake adapter. When the in-state path is too long for a Unix
// socket address (the sun_path limit is ~104 bytes on darwin/108 on linux), it
// falls back to a per-user temp directory; see runnerSocketTempPath.
func (c *Config) RunnerSocketPath(agentID string) string {
	inState := c.runnerSocketInStatePath(agentID)
	if len(inState) < 100 {
		return inState
	}
	return c.runnerSocketTempPath(agentID)
}

func (c *Config) runnerSocketInStatePath(agentID string) string {
	return filepath.Join(c.AgentStateDir(agentID), "runner.sock")
}

// runnerSocketTempDir is the per-user temp directory for runner sockets that
// don't fit in-state. Predictable but NOT shared: the uid suffix means a second
// local user cannot pre-create or squat the directory to intercept this user's
// sockets. See ensureOwnedRunnerSocketDir (build-tagged) for the bind-time
// ownership check.
func runnerSocketTempDir() string {
	return filepath.Join(os.TempDir(), "agentchute-run-"+currentUID())
}

func (c *Config) runnerSocketTempPath(agentID string) string {
	sum := sha256.Sum256([]byte(c.LoopDir + "\x00" + agentID))
	short := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(runnerSocketTempDir(), short+"-"+agentID+".sock")
}

// EnsureRunnerSocketDir creates the parent directory for socketPath and, when
// that directory is the per-user temp fallback, verifies it is owned by the
// current uid (defense against a shared-/tmp squatter). In-state socket dirs
// are created with the standard private-dir helper. Call this before binding a
// runner socket.
func (c *Config) EnsureRunnerSocketDir(socketPath string) error {
	dir := filepath.Dir(socketPath)
	if dir == runnerSocketTempDir() {
		return ensureOwnedRunnerSocketDir(dir)
	}
	return ensurePrivateDir(dir)
}

// WatchdogLogPath returns the watchdog log path.
func (c *Config) WatchdogLogPath() string {
	return filepath.Join(c.LoopDir, "watchdog.log")
}

func discoverControlRepo(opts DiscoverOpts) (controlRepo, origin string, shadowed []string, err error) {
	// 1. Explicit --control-repo flag wins.
	if strings.TrimSpace(opts.ControlRepoFlag) != "" {
		repo, err := validateExplicitControlRepo(opts.ControlRepoFlag)
		if err != nil {
			return "", "", nil, err
		}
		return repo, "flag", nil, nil
	}

	// 2. AGENTCHUTE_CONTROL_REPO env var.
	if strings.TrimSpace(opts.EnvControlRepo) != "" {
		repo, err := validateExplicitControlRepo(opts.EnvControlRepo)
		if err != nil {
			return "", "", nil, err
		}
		return repo, "env", nil, nil
	}

	// 3. .agentchute-control-repo pointer file (cwd or ancestor).
	if opts.Cwd != "" {
		ptr, perr := DiscoverPointer(opts.Cwd)
		if perr != nil {
			// Pointer discovery errors (malformed pointer, broken target) are
			// hard errors — the operator put a pointer there on purpose.
			return "", "", nil, fmt.Errorf("pointer file discovery: %w", perr)
		}
		if ptr != nil {
			if !fileExists(filepath.Join(ptr.ResolvedTarget, specFileName)) {
				return "", "", nil, fmt.Errorf("pointer %s -> %q: target does not contain %s",
					ptr.PointerFilePath, ptr.ResolvedTarget, specFileName)
			}
			if !hasVendorLoopDir(ptr.ResolvedTarget) {
				return "", "", nil, fmt.Errorf("pointer %s -> %q: target has no vendor loop directory",
					ptr.PointerFilePath, ptr.ResolvedTarget)
			}
			return ptr.ResolvedTarget, "pointer:" + ptr.PointerFilePath, ptr.Shadowed, nil
		}
	}

	// 4. Walk up from cwd looking for a control repo with a vendor loop dir.
	if opts.Cwd != "" {
		if repo, err := findControlRepo(opts.Cwd); err == nil {
			if hasVendorLoopDir(repo) {
				return repo, "cwd", nil, nil
			}
		}
	}

	return "", "", nil, fmt.Errorf("%w: no --control-repo flag, no AGENTCHUTE_CONTROL_REPO env, no %s pointer in cwd ancestors, and no AGENTCHUTE.md + vendor loop dir found walking up from %q",
		ErrNoControlRepo, PointerFileName, opts.Cwd)
}

// validateExplicitControlRepo checks that a flag- or env-provided control
// repo exists, is a directory, and contains AGENTCHUTE.md. Used by the flag
// and env arms of the discovery cascade.
func validateExplicitControlRepo(candidate string) (string, error) {
	repo, err := absExistingDir(candidate)
	if err != nil {
		return "", fmt.Errorf("control repo %q: %w", candidate, err)
	}
	if !fileExists(filepath.Join(repo, specFileName)) {
		return "", fmt.Errorf("control repo %q does not contain %s", repo, specFileName)
	}
	return repo, nil
}

func hasVendorLoopDir(controlRepo string) bool {
	loopDirs, err := findVendorLoopDirs(controlRepo)
	return err == nil && len(loopDirs) > 0
}

func findControlRepo(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}

	for {
		if fileExists(filepath.Join(dir, specFileName)) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s not found from %q", specFileName, start)
		}
		dir = parent
	}
}

func discoverLoopDir(controlRepo string, opts DiscoverOpts) (string, string, error) {
	// 1. Explicit --loop-dir flag wins.
	if strings.TrimSpace(opts.LoopDirFlag) != "" {
		loop, err := resolveLoopDir(controlRepo, opts.LoopDirFlag)
		if err != nil {
			return "", "", err
		}
		return loop, "flag", nil
	}
	// 2. AGENTCHUTE_LOOP_DIR env var.
	if strings.TrimSpace(opts.EnvLoopDir) != "" {
		loop, err := resolveLoopDir(controlRepo, opts.EnvLoopDir)
		if err != nil {
			return "", "", err
		}
		return loop, "env", nil
	}

	// 3. Auto-discover the single vendor loop dir under controlRepo.
	loopDirs, err := findVendorLoopDirs(controlRepo)
	if err != nil {
		return "", "", err
	}
	switch len(loopDirs) {
	case 0:
		return "", "", fmt.Errorf("no vendor loop directories found under %q", controlRepo)
	case 1:
		return loopDirs[0], "auto", nil
	default:
		return "", "", fmt.Errorf("multiple vendor loop directories found under %q; set AGENTCHUTE_LOOP_DIR or pass --loop-dir", controlRepo)
	}
}

func resolveLoopDir(controlRepo, raw string) (string, error) {
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(controlRepo, raw)
	}
	dir, err := absExistingDir(raw)
	if err != nil {
		return "", fmt.Errorf("loop dir %q: %w", raw, err)
	}
	if filepath.Base(dir) != loopDirName {
		return "", fmt.Errorf("loop dir %q must end in %q", dir, loopDirName)
	}
	if _, err := vendorFromLoopDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func findVendorLoopDirs(controlRepo string) ([]string, error) {
	entries, err := os.ReadDir(controlRepo)
	if err != nil {
		return nil, err
	}

	var loopDirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, ".") || name == "." || name == ".." {
			continue
		}
		candidate := filepath.Join(controlRepo, name, loopDirName)
		if dirExists(candidate) {
			abs, err := filepath.Abs(candidate)
			if err != nil {
				return nil, err
			}
			loopDirs = append(loopDirs, abs)
		}
	}
	return loopDirs, nil
}

func vendorFromLoopDir(loopDir string) (string, error) {
	if filepath.Base(loopDir) != loopDirName {
		return "", fmt.Errorf("loop dir %q must end in %q", loopDir, loopDirName)
	}
	vendorDir := filepath.Base(filepath.Dir(loopDir))
	if !strings.HasPrefix(vendorDir, ".") || len(vendorDir) == 1 {
		return "", fmt.Errorf("loop dir %q must be under a vendor dotdir", loopDir)
	}
	return strings.TrimPrefix(vendorDir, "."), nil
}

func absExistingDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlink not allowed")
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return abs, nil
}

// EnsurePrivateDir creates path if needed and tightens existing directories to
// owner-only access. Live loop state can contain local paths and message text.
func EnsurePrivateDir(path string) error {
	return ensurePrivateDir(path)
}

// WithAgentLock runs fn while holding the exclusive per-agent file lock at
// <loop>/state/<agent>/.lock. Exported for package-main callers (the runner)
// that read-modify-write an agent's registration outside this package and must
// serialize against UpdateLastSeen / ledger writes. See the build-tagged
// withAgentLock for the no-nested-lock contract: a single call stack must never
// acquire this lock twice for the same agentID.
func WithAgentLock(cfg *Config, agentID string, fn func() error) error {
	return withAgentLock(cfg, agentID, fn)
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: symlink not allowed", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", path)
	}
	return os.Chmod(path, 0o700)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir()
}
