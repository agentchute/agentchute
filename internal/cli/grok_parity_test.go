package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Grok parity, runner/shim surface. The grok CLI has no repo hook system
// (no settings.json/hooks.json, no SessionStart lifecycle), so its first-class
// wake path is the launcher shim that routes through `agentchute serve`. These
// tests pin grok as a known setup wrapper and a shimmable wrapper, while
// asserting it is correctly treated as hookless.

func TestShimsInstallGrok(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			// The wrapper-agnostic `ac` dispatcher routes grok via `ac serve grok`;
			// there is no per-wrapper grok shim.
			return cmdShims([]string{"install", "--dir", filepath.Join(root, "bin")})
		}); err != nil {
			t.Fatalf("shims install grok: %v", err)
		}
	})
	if _, err := os.Stat(filepath.Join(root, "bin", "ac")); err != nil {
		t.Fatalf("ac dispatcher not installed: %v", err)
	}
}

func TestShimsAllIncludesGrok(t *testing.T) {
	specs, err := selectShimSpecs("all")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range specs {
		if s.Name == "ac-grok" {
			found = true
			if s.AgentID != "grok" || s.Vendor != "xai" {
				t.Errorf("grok shim spec = %+v, want AgentID=grok Vendor=xai", s)
			}
		}
	}
	if !found {
		t.Error("`shims install --wrapper all` does not include grok")
	}
}

func TestSetupWrappersAcceptsGrok(t *testing.T) {
	root := t.TempDir()
	// Put a fake grok on PATH so detection can find it.
	bin := filepath.Join(root, "realbin")
	mustMkdir(t, bin)
	grokPath := filepath.Join(bin, "grok")
	mustWrite(t, grokPath, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(grokPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	wrappers, paths, err := resolveSetupWrappers("grok", filepath.Join(root, "shimdir"))
	if err != nil {
		t.Fatalf("resolveSetupWrappers(grok): %v", err)
	}
	if len(wrappers) != 1 || wrappers[0] != "grok" {
		t.Fatalf("wrappers = %v, want [grok]", wrappers)
	}
	if paths["grok"] == "" {
		t.Errorf("grok not detected on PATH; paths=%v", paths)
	}
}

func TestSetupWrappersAllDetectsGrok(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "realbin")
	mustMkdir(t, bin)
	grokPath := filepath.Join(bin, "grok")
	mustWrite(t, grokPath, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(grokPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	detected := detectSetupWrappers(filepath.Join(root, "shimdir"))
	var found bool
	for _, w := range detected {
		if w == "grok" {
			found = true
		}
	}
	if !found {
		t.Errorf("`--wrappers all` detection missed grok on PATH; detected=%v", detected)
	}
}

// Grok is a known wrapper but hookless: hookWrapperByName must report it has no
// hook template, so applySetup skips hook install and doctor never blocker-fails
// a grok agent for a hook it cannot have.
func TestGrokIsKnownButHookless(t *testing.T) {
	if !wrapperIsKnownForSetup("grok") {
		t.Error("grok should be a known setup wrapper")
	}
	if _, ok := hookWrapperByName("grok"); ok {
		t.Error("grok must NOT have a hook template (grok CLI has no repo hook system)")
	}
	if _, ok := hookWrapperForAgent("grok"); ok {
		t.Error("doctor must not expect a hook for a grok agent")
	}
}

// Runner-mode setup with grok selected installs the `ac` dispatcher and does
// NOT attempt to write a grok hook file (there is no grok hook template).
func TestSetupRunnerInstallsGrokShimNoHook(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	realGrok := filepath.Join(realDir, "grok")
	mustWrite(t, realGrok, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(realGrok, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdSetup([]string{
				"--wake", "runner",
				"--wrappers", "grok",
				"--profile", profile,
				"--yes",
			})
		}); err != nil {
			t.Fatalf("cmdSetup grok runner: %v", err)
		}
	})

	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", "ac")); err != nil {
		t.Errorf("ac dispatcher not installed by setup: %v", err)
	}
	// No grok hook file should exist anywhere — grok has no hook template.
	for _, p := range []string{".grok/settings.json", ".grok/hooks.json"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			t.Errorf("setup wrote a grok hook file %s; grok has no hook system", p)
		}
	}
}
