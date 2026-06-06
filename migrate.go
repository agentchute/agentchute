package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// legacyNamespace is the pre-v0.2.2 dotdir namespace agentchute used before the
// rename to .agentchute. Installs from that era leave a .rehumanlabs/loop behind.
// Because loop.Discover scans every .<name>/loop, a stranded legacy loop either
// collides with the canonical .agentchute/loop ("multiple vendor loop
// directories") or silently runs the agent under vendor=rehumanlabs. setup/init
// consolidate it into the canonical namespace.
const legacyNamespace = "rehumanlabs"

// dirExists reports whether path is an existing directory (symlinks resolved).
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// pathExists reports whether anything exists at path (without following links).
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// isScaffoldFile reports whether name is a loop scaffold artifact rather than
// real coordination state. Empty subdirs, READMEs, and *.example.md templates
// are scaffolding; registrations, inbox messages, archive/malformed entries,
// and runtime state files are not.
func isScaffoldFile(name string) bool {
	return name == "README.md" || strings.HasSuffix(name, ".example.md")
}

// countLoopState returns the number of real (non-scaffold) files under loopDir.
// A fresh scaffold (only empty dirs and/or README/example files) returns 0.
func countLoopState(loopDir string) (int, error) {
	count := 0
	err := filepath.WalkDir(loopDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || isScaffoldFile(d.Name()) {
			return nil
		}
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// loopHasState reports whether loopDir holds live coordination state.
func loopHasState(loopDir string) (bool, error) {
	n, err := countLoopState(loopDir)
	return n > 0, err
}

// planLegacyMigration inspects root for a legacy .rehumanlabs/loop and returns
// an initAction that consolidates it into the canonical .<namespace>/loop for
// the safe cases. It returns (nil, "", nil) when there is nothing to migrate
// (idempotent), and an error for the unsafe both-live case where an automatic
// merge could lose data or fabricate obligations — the operator resolves that
// manually.
//
// The second return value is the rel loop path the migration will remove (e.g.
// ".rehumanlabs/loop"); the caller excludes it from the ambiguity guard so the
// pre-migration coexistence does not trip the "multiple loop namespaces" refusal.
//
// Scope is the control repo only (no $HOME scan), and symlinked namespace/loop
// paths are rejected the same way init scaffolding rejects them.
func planLegacyMigration(root, namespace string) (*initAction, string, error) {
	if namespace == legacyNamespace {
		return nil, "", nil // canonical IS the legacy name; nothing to do
	}

	legacyDir := filepath.Join(root, "."+legacyNamespace)
	legacyLoop := filepath.Join(legacyDir, "loop")
	if !dirExists(legacyLoop) {
		return nil, "", nil // idempotent: already migrated / never present
	}

	canonDir := filepath.Join(root, "."+namespace)
	canonLoop := filepath.Join(canonDir, "loop")
	legacyRel := filepath.ToSlash(filepath.Join("."+legacyNamespace, "loop"))

	// Refuse to operate through symlinked namespace/loop paths.
	for _, p := range []string{legacyDir, legacyLoop, canonDir, canonLoop} {
		if err := rejectSymlinkAncestor(p); err != nil {
			return nil, "", err
		}
	}

	// Case A: canonical loop absent — straight move, no possibility of collision.
	if !dirExists(canonLoop) {
		if !pathExists(canonDir) {
			// Whole canonical namespace absent → atomic rename of the dotdir,
			// preserving registrations, inboxes, archive, ledger and state.
			return &initAction{
				Target: "." + legacyNamespace,
				Action: "migrate legacy namespace",
				Detail: fmt.Sprintf("rename .%s -> .%s (legacy bus state preserved)", legacyNamespace, namespace),
				Apply: func() error {
					return os.Rename(legacyDir, canonDir)
				},
			}, legacyRel, nil
		}
		// Canonical dotdir exists but has no loop → move just the loop in.
		return &initAction{
			Target: legacyRel,
			Action: "migrate legacy loop",
			Detail: fmt.Sprintf("move .%s/loop -> .%s/loop", legacyNamespace, namespace),
			Apply: func() error {
				if err := loop.EnsurePrivateDir(canonDir); err != nil {
					return err
				}
				return os.Rename(legacyLoop, canonLoop)
			},
		}, legacyRel, nil
	}

	// Canonical loop exists: classify by which side holds live state.
	legacyLive, err := loopHasState(legacyLoop)
	if err != nil {
		return nil, "", err
	}
	canonLive, err := loopHasState(canonLoop)
	if err != nil {
		return nil, "", err
	}

	switch {
	case !legacyLive:
		// Case B: legacy is scaffold-only → move it aside. Back up the LOOP path
		// (not the whole .rehumanlabs dotdir): a ".rehumanlabs.<backup>/loop"
		// would still be auto-discovered as a vendor loop; ".rehumanlabs/loop.<backup>"
		// is not (its basename is no longer "loop").
		backup := setupBackupPath(legacyLoop)
		return &initAction{
			Target: legacyRel,
			Action: "retire legacy scaffold",
			Detail: fmt.Sprintf("move empty .%s/loop aside to %s", legacyNamespace, filepath.Base(backup)),
			Apply: func() error {
				return os.Rename(legacyLoop, backup)
			},
		}, legacyRel, nil

	case !canonLive:
		// Case C: canonical is scaffold-only but legacy has real state → back up
		// the empty canonical loop, then move the legacy loop into place.
		backup := setupBackupPath(canonLoop)
		return &initAction{
			Target: legacyRel,
			Action: "migrate legacy loop",
			Detail: fmt.Sprintf("back up empty .%s/loop to %s, then move .%s/loop -> .%s/loop", namespace, filepath.Base(backup), legacyNamespace, namespace),
			Apply: func() error {
				if err := os.Rename(canonLoop, backup); err != nil {
					return err
				}
				return os.Rename(legacyLoop, canonLoop)
			},
		}, legacyRel, nil

	default:
		// Case D: both sides hold live state → refuse. Collisions in
		// agents/inbox/state carry semantics; an automatic merge could lose mail
		// or fabricate obligations. The operator moves one aside and re-runs.
		legacyN, _ := countLoopState(legacyLoop)
		canonN, _ := countLoopState(canonLoop)
		return nil, "", fmt.Errorf(
			"both .%s/loop (%d state file(s)) and .%s/loop (%d state file(s)) hold live coordination state; refusing to auto-merge because collisions in agents/inbox/state carry semantics. Move one aside manually (e.g. `mv .%s/loop .%s/loop.backup`) and re-run setup/init, or merge by hand",
			legacyNamespace, legacyN, namespace, canonN, legacyNamespace, legacyNamespace)
	}
}
