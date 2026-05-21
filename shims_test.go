package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShimsInstallWritesKnownWrapperLaunchers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--quiet"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "claude-code", "codex", "gemini", "gemini-cli"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read shim %s: %v", name, err)
		}
		if !strings.Contains(string(data), "shims exec") {
			t.Fatalf("shim %s does not route through shims exec:\n%s", name, data)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("shim %s is not executable: mode %v", name, info.Mode().Perm())
		}
	}
}

func TestResolveRealWrapperSkipsShimDirectory(t *testing.T) {
	shimDir := filepath.Join(t.TempDir(), "shim")
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(shimDir, "codex"), []byte("#!/bin/sh\nexit 99\n"))
	mustWrite(t, filepath.Join(realDir, "codex"), []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(filepath.Join(shimDir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(realDir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)
	spec, ok := shimSpecForName("codex")
	if !ok {
		t.Fatal("missing codex shim spec")
	}
	got, err := resolveRealWrapper(spec, shimDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realDir, "codex")
	if got != want {
		t.Fatalf("real wrapper = %s, want %s", got, want)
	}
}

func TestShimsInstallRefusesExistingWithoutForce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--quiet"}); err != nil {
		t.Fatal(err)
	}
	err := cmdShims([]string{"install", "--dir", dir, "--quiet"})
	if err == nil {
		t.Fatal("expected existing shim error")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error should mention --force: %v", err)
	}
}

func TestShimsExecRefusesHardDiscoveryError(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(t.TempDir(), "shim")
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(realDir, "codex")
	mustWrite(t, real, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(real, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, ".agentchute-control-repo"), []byte("/no/such/control/repo\n"))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	withCwd(t, root, func() {
		err := cmdShims([]string{"exec", "--name", "codex", "--shim-dir", shimDir})
		if err == nil {
			t.Fatal("expected hard discovery error")
		}
		if !strings.Contains(err.Error(), "shim discovery failed") {
			t.Fatalf("error should mention shim discovery: %v", err)
		}
	})
}
