package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// updateGitHubBase is the release host. The update path reuses the same release
// artifacts as install.sh (agentchute_<ver>_<os>_<arch>.tar.gz + checksums.txt)
// but performs the download/verify/replace in pure Go — it never fetches or
// executes a remote shell script. Variable so tests can point it at a local
// httptest server.
var updateGitHubBase = "https://github.com/agentchute/agentchute"

// updateMaxAsset caps the archive we will buffer/verify and the extracted
// member size (defense against a hostile or corrupt release asset). Variable so
// tests can lower it.
var updateMaxAsset int64 = 64 << 20 // 64 MiB

var versionTagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}

func updateUsage(err error) error {
	if err == flag.ErrHelp {
		return fmt.Errorf("%w\n%s", flag.ErrHelp, updateHelp())
	}
	return fmt.Errorf("%w\n\n%s", err, updateHelp())
}

func updateHelp() string {
	return strings.TrimSpace(`
Usage:
  agentchute update [--version <tag>] [--dry-run] [--no-resync]

Updates an existing install in one step: self-updates the binary to the target
release, then re-runs ` + "`setup`" + ` with this control repo's saved wake mode and
wrappers so hooks, enrollment blocks, and shims re-sync to the new version.

This clears live registrations; you MUST restart every active agent afterward.

Pass --no-resync to swap ONLY the binary and skip the setup re-sync: live
registrations are preserved and no bus reset happens. Use it for a pure binary
refresh, or for a pool created by an older binary / via ` + "`init`" + ` that has no
saved setup state to replay.

Flags:
  --version <tag>   release tag to install (default: latest release)
  --dry-run         print the plan and exit; no mutation
  --no-resync       swap the binary only; skip the setup re-sync / bus reset
  --control-repo    control repo to re-sync (default: env or current pool)`)
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var targetTag, controlRepo, loopDir string
	var dryRun, noResync bool
	fs.StringVar(&targetTag, "version", "", "release tag to install (default: latest)")
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan and exit; no mutation")
	fs.BoolVar(&noResync, "no-resync", false, "swap the binary only; skip the setup re-sync / bus reset")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	if err := fs.Parse(args); err != nil {
		return updateUsage(err)
	}
	if fs.NArg() != 0 {
		return updateUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	// 1. Discover the control repo. The setup re-sync replays the saved
	// wake/wrappers, so it REQUIRES saved pool state — but --no-resync skips the
	// re-sync entirely (binary-only update), so it neither needs nor reads that
	// state and never resets the bus.
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
		return fmt.Errorf("discover control repo: %w", err)
	}

	var setupArgs []string
	var storedWake, wrappersArg string
	if !noResync {
		pool, err := readSetupPoolState(cfg)
		if err != nil {
			return fmt.Errorf("no saved setup state at %s — run `agentchute setup` in this repo before updating, or pass --no-resync to swap only the binary", filepath.Join(cfg.LoopDir, "state", "setup.json"))
		}
		storedWake = strings.TrimSpace(pool.Wake)
		if storedWake == "" {
			return fmt.Errorf("saved setup state is missing the wake mode; run `agentchute setup` to re-establish before updating, or pass --no-resync to swap only the binary")
		}
		// An empty wrapper list is the valid `--wrappers none` mode, not missing state.
		wrappersArg = "none"
		if w := compactStrings(pool.Wrappers); len(w) > 0 {
			wrappersArg = strings.Join(w, ",")
		}
		// Global state carries install-wide knobs (shim dir, profile) so the re-sync
		// replays them faithfully instead of reverting to defaults.
		global, _ := readSetupGlobalState()

		// The exact setup re-sync, replaying the saved install config.
		setupArgs = []string{"setup", "--control-repo", cfg.ControlRepo, "--wake", storedWake, "--wrappers", wrappersArg, "--yes"}
		if pool.Aliases {
			setupArgs = append(setupArgs, "--aliases")
		}
		if sd := strings.TrimSpace(global.ShimDir); sd != "" {
			setupArgs = append(setupArgs, "--shim-dir", sd)
		}
		if global.NoProfile {
			setupArgs = append(setupArgs, "--no-profile")
		} else if p := strings.TrimSpace(global.Profile); p != "" {
			setupArgs = append(setupArgs, "--profile", p)
		}
	}

	// 2. Resolve the real binary we will replace — refuse a shim BEFORE any
	// network work. Writability is probed later (apply only) so --dry-run never
	// touches the install dir.
	target, err := resolveUpdateTarget()
	if err != nil {
		return err
	}

	// 3. Resolve the target version.
	targetTag = strings.TrimSpace(targetTag)
	if targetTag == "" {
		targetTag, err = resolveLatestVersion()
		if err != nil {
			return fmt.Errorf("resolve latest release: %w", err)
		}
	}
	if !versionTagRE.MatchString(targetTag) {
		return fmt.Errorf("invalid version tag %q (expected vMAJOR.MINOR.PATCH)", targetTag)
	}
	current := "v" + version // ldflag-injected; "vdev" for local builds
	bare := strings.TrimPrefix(targetTag, "v")
	asset := fmt.Sprintf("agentchute_%s_%s_%s.tar.gz", bare, runtime.GOOS, runtime.GOARCH)

	setupCmd := strings.Join(setupArgs, " ")
	active := activeAgentIDs(cfg)

	if dryRun {
		fmt.Println("agentchute update (dry-run)")
		fmt.Printf("  current:       %s\n", current)
		fmt.Printf("  target:        %s\n", targetTag)
		fmt.Printf("  binary:        %s\n", target)
		fmt.Printf("  asset:         %s\n", asset)
		if noResync {
			fmt.Println("  re-sync:       skipped (--no-resync); binary swap only")
			fmt.Println("  reset:         skipped (--no-resync); live registrations preserved")
		} else {
			fmt.Printf("  re-sync:       %s\n", setupCmd)
			fmt.Printf("  reset:         would stop local agentchute runtimes and clear %d live agent registration(s)\n", len(active))
		}
		printActiveAgents(active)
		fmt.Println("(dry-run; no changes made)")
		return nil
	}

	// Apply path only: confirm the install dir is writable before downloading.
	if err := ensureWritableDir(target); err != nil {
		return err
	}

	if versionIsOlder(bare, strings.TrimPrefix(current, "v")) {
		fmt.Fprintf(os.Stderr, "WARNING: downgrading %s -> %s; this may revert enrollment templates and break compatibility with newer peers.\n", current, targetTag)
	}

	// 4. Download + verify + extract into a temp file in the SAME directory as
	// the target (so the final swap is an atomic same-filesystem rename). Any
	// failure here leaves the installed binary untouched.
	tmpBin, err := downloadVerifyExtract(targetTag, asset, target)
	if err != nil {
		return err
	}
	defer os.Remove(tmpBin)

	// --no-resync is a pure binary refresh: no bus reset, no restart needed.
	if !noResync {
		printRestartWarning(targetTag, active, false)
	}

	// 5. Atomic replace + best-effort parent-dir fsync for crash durability.
	if err := os.Rename(tmpBin, target); err != nil {
		return fmt.Errorf("replace binary %s: %w", target, err)
	}
	syncDir(filepath.Dir(target))
	fmt.Printf("Updated agentchute %s -> %s (%s)\n", current, targetTag, target)

	if noResync {
		// Binary-only update: skip the destructive setup re-sync entirely. Live
		// registrations and wake infrastructure are left untouched.
		fmt.Println("(--no-resync: skipped setup re-sync; live registrations preserved)")
		return nil
	}

	// 6. Re-exec the NEW binary's setup so it writes the new version's
	// templates/hooks/shims, not the old binary's.
	if err := updateRunResync(target, setupArgs, cfg.ControlRepo); err != nil {
		fmt.Fprintf(os.Stderr, "\nWARNING: binary updated to %s but `setup` re-sync FAILED: %v\n", targetTag, err)
		fmt.Fprintf(os.Stderr, "Finish the re-sync manually from this repo:\n  %s %s\n", target, setupCmd)
		return errors.New("setup re-sync after update failed (see warning above)")
	}

	// 7. Final restart warning.
	printRestartWarning(targetTag, active, true)
	return nil
}

// updateRunResync re-execs the freshly-swapped binary's `setup` so it writes the
// NEW version's templates/hooks/shims and performs the bus reset. It is a package
// var so tests can assert the --no-resync path never invokes it. The apply path
// runs this only when --no-resync is absent.
var updateRunResync = func(target string, setupArgs []string, controlRepo string) error {
	setup := exec.Command(target, setupArgs...)
	setup.Stdout = os.Stdout
	setup.Stderr = os.Stderr
	setup.Stdin = os.Stdin
	setup.Dir = controlRepo
	return setup.Run()
}

// resolveUpdateTargetForTest, when non-empty, overrides the running-binary
// lookup so tests can point the swap at a fake install dir without depending on
// the location/permissions of the test binary. Empty in production.
var resolveUpdateTargetForTest string

// resolveUpdateTarget returns the path of the real agentchute binary to
// replace, refusing wrapper shims and non-writable locations.
func resolveUpdateTarget() (string, error) {
	if resolveUpdateTargetForTest != "" {
		return resolveBinaryTarget(resolveUpdateTargetForTest)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	return resolveBinaryTarget(exe)
}

func resolveBinaryTarget(exe string) (string, error) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve binary path %q: %w", exe, err)
	}
	// Refuse a shell shim (the ac-* launchers and the shim dispatcher are
	// scripts beginning with a shebang). The real binary is a native
	// executable. This is read-only so it is safe for --dry-run.
	if looksLikeShimScript(real) {
		return "", fmt.Errorf("running via a launcher shim (%s), not the real binary; run `agentchute update` from the installed binary or pass its path on PATH", real)
	}
	return real, nil
}

// ensureWritableDir confirms the install directory accepts a temp file, so the
// apply path fails before downloading rather than after. It creates and removes
// a probe file, so it runs ONLY on apply — never on --dry-run.
func ensureWritableDir(target string) error {
	dir := filepath.Dir(target)
	probe, err := os.CreateTemp(dir, ".agentchute-update-probe-*")
	if err != nil {
		return fmt.Errorf("install dir %s is not writable (%v); re-run with write access (e.g. sudo) or reinstall via install.sh", dir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

func looksLikeShimScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 2)
	n, _ := io.ReadFull(f, buf)
	return n == 2 && buf[0] == '#' && buf[1] == '!'
}

// resolveLatestVersion follows GitHub's /releases/latest redirect and returns
// the resolved tag (e.g. "v0.6.0"), mirroring install.sh without JSON.
func resolveLatestVersion() (string, error) {
	resp, err := updateHTTPClient().Get(updateGitHubBase + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	final := resp.Request.URL.Path // expected: .../releases/tag/vX.Y.Z
	marker := "/releases/tag/"
	i := strings.LastIndex(final, marker)
	if i < 0 {
		return "", fmt.Errorf("unexpected redirect target %q (not a /releases/tag/ URL)", resp.Request.URL.String())
	}
	tag := final[i+len(marker):]
	if !versionTagRE.MatchString(tag) {
		return "", fmt.Errorf("could not parse a release tag from %q", resp.Request.URL.String())
	}
	return tag, nil
}

// downloadVerifyExtract downloads the release archive + checksums.txt, verifies
// the archive's SHA-256 against the exact filename entry, extracts only the
// `agentchute` member (rejecting any other or traversing path), and writes it
// to a temp file alongside target. Returns the temp path and the file mode to
// apply. The installed binary is never touched by this function.
func downloadVerifyExtract(tag, asset, target string) (string, error) {
	base := updateGitHubBase + "/releases/download/" + tag + "/"
	expected, err := fetchChecksum(base+"checksums.txt", asset)
	if err != nil {
		return "", err
	}

	resp, err := updateHTTPClient().Get(base + asset)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", asset, resp.StatusCode)
	}

	archive, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxAsset+1))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	if int64(len(archive)) > updateMaxAsset {
		return "", fmt.Errorf("release asset %s exceeds %d bytes", asset, updateMaxAsset)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != expected {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s — aborting, binary untouched", asset, got, expected)
	}

	return extractAgentchute(archive, target)
}

// fetchChecksum returns the lowercase hex SHA-256 recorded for asset in a
// checksums.txt body, requiring an exact filename match.
func fetchChecksum(url, asset string) (string, error) {
	resp, err := updateHTTPClient().Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksums.txt: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// checksums.txt entries may carry a leading "*" binary marker.
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			h := strings.ToLower(fields[0])
			if len(h) != 64 {
				return "", fmt.Errorf("malformed checksum for %s", asset)
			}
			if _, err := hex.DecodeString(h); err != nil {
				return "", fmt.Errorf("non-hex checksum for %s", asset)
			}
			return h, nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s in checksums.txt", asset)
}

func extractAgentchute(archive []byte, target string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", fmt.Errorf("gunzip release asset: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read release archive: %w", err)
		}
		// Only the exact top-level `agentchute` regular file is accepted. Any
		// path separator, traversal, or other name is rejected.
		if hdr.Name != "agentchute" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > updateMaxAsset {
			return "", fmt.Errorf("agentchute member is %d bytes, exceeds the %d-byte cap", hdr.Size, updateMaxAsset)
		}
		tmp, err := os.CreateTemp(filepath.Dir(target), ".agentchute-update-*")
		if err != nil {
			return "", fmt.Errorf("create temp binary: %w", err)
		}
		fail := func(format string, a ...any) (string, error) {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf(format, a...)
		}
		// Copy exactly the declared size and verify the full member was written
		// — never install a truncated binary.
		written, err := io.Copy(tmp, tr)
		if err != nil {
			return fail("extract agentchute: %w", err)
		}
		if written != hdr.Size {
			return fail("short extract: wrote %d of %d bytes", written, hdr.Size)
		}
		mode := os.FileMode(0o755)
		if hdr.Mode != 0 {
			mode = os.FileMode(hdr.Mode).Perm() | 0o100
		}
		// chmod BEFORE the final fsync so the durable file already carries its
		// exec bits.
		if err := tmp.Chmod(mode); err != nil {
			return fail("chmod new binary: %w", err)
		}
		if err := tmp.Sync(); err != nil {
			return fail("sync new binary: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("close new binary: %w", err)
		}
		return tmp.Name(), nil
	}
	return "", fmt.Errorf("release archive did not contain an `agentchute` binary")
}

// syncDir best-effort fsyncs a directory so a rename is durable across a crash.
// Not supported everywhere; failures are ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

// activeAgentIDs returns the agent ids of live registrations on this host in
// this pool, for the restart warning and dry-run plan.
func activeAgentIDs(cfg *loop.Config) []string {
	regs, _ := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	localHost, _ := os.Hostname()
	localHost = strings.TrimSpace(localHost)
	var ids []string
	for _, reg := range regs {
		if reg.Status == loop.StatusOffline {
			continue
		}
		if localHost != "" && strings.TrimSpace(reg.Host) != "" && reg.Host != localHost {
			continue
		}
		ids = append(ids, reg.AgentID)
	}
	return ids
}

func printActiveAgents(ids []string) {
	if len(ids) == 0 {
		fmt.Println("  active agents: none")
		return
	}
	fmt.Printf("  active agents: %s\n", strings.Join(ids, ", "))
}

func printRestartWarning(tag string, active []string, done bool) {
	bar := strings.Repeat("=", 70)
	fmt.Fprintln(os.Stderr, bar)
	if done {
		fmt.Fprintf(os.Stderr, "agentchute updated to %s and re-ran setup.\n\n", tag)
	} else {
		fmt.Fprintf(os.Stderr, "agentchute is updating to %s and will re-run setup.\n\n", tag)
	}
	fmt.Fprintln(os.Stderr, "setup stops local agentchute pollers/runners, clears live registrations,")
	fmt.Fprintln(os.Stderr, "and releases repo Herdr names where possible. Until each wrapper restarts")
	fmt.Fprintln(os.Stderr, "and re-enrolls, peers CANNOT wake it.")
	if len(active) > 0 {
		fmt.Fprintf(os.Stderr, "\nRESTART every active agent now (%d): %s\n", len(active), strings.Join(active, ", "))
	}
	fmt.Fprintln(os.Stderr, bar)
}

func compactStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// versionIsOlder reports whether bare semver a is strictly older than b. Any
// unparseable input (e.g. "dev") returns false so we never block on it.
func versionIsOlder(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseSemver(s string) ([3]int, bool) {
	var out [3]int
	core := s
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
