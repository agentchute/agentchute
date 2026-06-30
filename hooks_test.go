package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hooks install is the v0.2.1 operator-facing fix for the "LLM forgets
// to run boot at session start" failure mode. Tests pin the contract:
// idempotent re-install, refuse-on-divergence (default), --force with
// backup, --dry-run no-op, --scope repo|user respected, unknown wrapper
// rejection.

// hooksRepoFixture sets up the minimum control-repo scaffold the
// hooks-install code path needs (AGENTCHUTE.md spec marker + a loop
// dir). Otherwise loop.Discover refuses to anchor the scope root.
func hooksRepoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	return root
}

func TestHooksInstallWritesRepoScope(t *testing.T) {
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		})
		if err != nil {
			t.Fatalf("install err = %v", err)
		}
		path := filepath.Join(root, ".claude", "settings.json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist after install: %v", path, err)
		}
		// File mode should be owner-only (0600). Parent dir 0700.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("hook file mode = %v, want 0600", info.Mode().Perm())
		}
		parentInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if parentInfo.Mode().Perm() != 0o700 {
			t.Errorf("hook parent dir mode = %v, want 0700", parentInfo.Mode().Perm())
		}
	})
}

func TestHooksInstallIdempotent(t *testing.T) {
	// Same-content re-install must not error, must not rewrite, must report
	// "already current". This is the LLM-friendly "you can run this any
	// time" property gemini emphasized in the v6 enrollment block.
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		})
		if err != nil {
			t.Errorf("second install err = %v (idempotent re-run should not error)", err)
		}
		if !strings.Contains(out, "already current") {
			t.Errorf("second install missing 'already current' message:\n%s", out)
		}
	})
}

func TestHooksInstallRefusesDivergentWithoutForce(t *testing.T) {
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
		// Modify the installed file.
		path := filepath.Join(root, ".claude", "settings.json")
		if err := os.WriteFile(path, []byte(`{"modified": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		})
		if err == nil {
			t.Fatal("expected refuse on divergent existing file without --force")
		}
		if !strings.Contains(err.Error(), "--force") {
			t.Errorf("error missing --force hint: %v", err)
		}
	})
}

func TestHooksInstallForceBacksUp(t *testing.T) {
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, ".claude", "settings.json")
		original := []byte(`{"modified": true}`)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code", "--force"})
		}); err != nil {
			t.Fatalf("force install err = %v", err)
		}
		// .bak file holds the pre-overwrite content; main file is the canonical template.
		bak, err := os.ReadFile(path + ".bak")
		if err != nil {
			t.Fatalf("backup missing: %v", err)
		}
		if string(bak) != string(original) {
			t.Errorf("backup content = %q, want %q", bak, original)
		}
	})
}

func TestHooksInstallDryRunWritesNothing(t *testing.T) {
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code", "--dry-run"})
		})
		if err != nil {
			t.Fatalf("dry-run err = %v", err)
		}
		if !strings.Contains(out, "dry-run") {
			t.Errorf("dry-run output missing 'dry-run' label:\n%s", out)
		}
		// No file should have been written.
		path := filepath.Join(root, ".claude", "settings.json")
		if _, err := os.Stat(path); err == nil {
			t.Errorf("dry-run wrote %s; should be a no-op", path)
		}
	})
}

func TestHooksInstallAllWrappers(t *testing.T) {
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "all"})
		}); err != nil {
			t.Fatalf("install all err = %v", err)
		}
		for _, want := range []string{
			filepath.Join(".claude", "settings.json"),
			filepath.Join(".codex", "hooks.json"),
			filepath.Join(".gemini", "settings.json"),
		} {
			path := filepath.Join(root, want)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("expected %s after --wrapper all: %v", path, err)
			}
		}
	})
}

func TestHooksInstallUnknownWrapperRejected(t *testing.T) {
	root := hooksRepoFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "ferret"})
		})
		if err == nil {
			t.Fatal("expected error for unknown wrapper")
		}
		if !strings.Contains(err.Error(), "not recognized") {
			t.Errorf("error missing 'not recognized': %v", err)
		}
	})
}

func TestHooksInstallUserScope(t *testing.T) {
	// --scope user writes to $HOME-relative paths. We point HOME at a
	// temp dir for the test so we don't touch the developer's actual
	// ~/.claude/.
	root := t.TempDir()
	t.Setenv("HOME", root)
	if _, err := captureStdout(t, func() error {
		return cmdHooks([]string{"install", "--wrapper", "claude-code", "--scope", "user"})
	}); err != nil {
		t.Fatalf("user-scope install err = %v", err)
	}
	path := filepath.Join(root, ".claude", "settings.json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s under HOME after --scope user: %v", path, err)
	}
}

// Codex re-review #1 (2026-05-20): with --wrapper defaulted to "all",
// the bare `agentchute hooks install` from the v6 enrollment block is
// executable. Without this default the enrollment instruction is broken.
func TestHooksInstallDefaultsToAll(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install"})
		}); err != nil {
			t.Fatalf("bare `hooks install` should default to --wrapper all; got %v", err)
		}
		for _, want := range []string{
			filepath.Join(".claude", "settings.json"),
			filepath.Join(".codex", "hooks.json"),
			filepath.Join(".gemini", "settings.json"),
		} {
			if _, err := os.Stat(filepath.Join(root, want)); err != nil {
				t.Errorf("missing %s after default install: %v", want, err)
			}
		}
	})
}

// Codex re-review #2: --scope repo must anchor at the control-repo
// root, not cwd. Otherwise running hooks install from a subdir writes
// hook files the wrapper-at-repo-root never sees.
func TestHooksInstallRepoScopeAnchorsAtControlRepoRoot(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	subdir := filepath.Join(root, "internal", "deep", "nested")
	mustMkdir(t, subdir)
	withCwd(t, subdir, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		}); err != nil {
			t.Fatalf("install from subdir err = %v", err)
		}
	})
	rootPath := filepath.Join(root, ".claude", "settings.json")
	subPath := filepath.Join(subdir, ".claude", "settings.json")
	if _, err := os.Stat(rootPath); err != nil {
		t.Errorf("expected hook at control-repo root %s: %v", rootPath, err)
	}
	if _, err := os.Stat(subPath); err == nil {
		t.Errorf("hook should NOT land in subdir %s (--scope repo must anchor at the repo root)", subPath)
	}
}

// Codex re-review #3: MkdirAll only chmods directories it creates.
// An existing 0755 parent stays 0755 unless we explicitly chmod.
// The 0700 invariant must hold regardless of pre-existing parent mode.
func TestHooksInstallTightensExistingParentDirPerms(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	// Pre-create the .claude dir at 0755 (the default a user might have).
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdHooks([]string{"install", "--wrapper", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
	})
	info, err := os.Stat(filepath.Join(root, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("existing parent dir mode = %v after install, want 0700", info.Mode().Perm())
	}
}

// Codex re-review #5: `hooks install -h` was wrapping flag.ErrHelp in
// hooksUsage, which exited 1 with the top-level help. Should preserve
// the sentinel and emit install-specific help with exit 0.
func TestHooksInstallHelpFlagExitsZero(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return cmdHooks([]string{"install", "-h"})
	})
	if err != nil {
		t.Errorf("hooks install -h should exit 0; got %v", err)
	}
	if !strings.Contains(out, "Usage: agentchute hooks install") {
		t.Errorf("help output missing install-specific usage:\n%s", out)
	}
	if !strings.Contains(out, "--wrapper") {
		t.Errorf("help output missing --wrapper flag mention:\n%s", out)
	}
}

func TestHooksInstallEmbeddedTemplatesPresent(t *testing.T) {
	// Sanity check: the embed.FS actually has all three wrapper payloads.
	// Catches the dotfile-embedding gotcha (//go:embed needs the `all:`
	// prefix to include hidden directories like .claude/).
	want := map[string]bool{
		"examples/hooks/claude-code/.claude/settings.json": false,
		"examples/hooks/codex/.codex/hooks.json":           false,
		"examples/hooks/gemini/.gemini/settings.json":      false,
	}
	if err := fs.WalkDir(hooksFS, "examples/hooks", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			want[path] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for k, found := range want {
		if !found {
			t.Errorf("embedded template missing: %s", k)
		}
	}
}
