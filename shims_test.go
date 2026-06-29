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
	for _, name := range []string{"ac-claude", "ac-codex", "ac-gemini", "ac-grok"} {
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
	mustWrite(t, filepath.Join(shimDir, "ac-codex"), []byte("#!/bin/sh\nexit 99\n"))
	mustWrite(t, filepath.Join(realDir, "codex"), []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(filepath.Join(shimDir, "ac-codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(realDir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)
	spec, ok := shimSpecForName("ac-codex")
	if !ok {
		t.Fatal("missing ac-codex shim spec")
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

func TestShimsInstallCanLimitWrappers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--wrapper", "codex", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ac-codex")); err != nil {
		t.Fatalf("ac-codex shim missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codex")); !os.IsNotExist(err) {
		t.Fatalf("same-name codex alias should not be installed by default: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ac-claude")); !os.IsNotExist(err) {
		t.Fatalf("ac-claude shim should not be installed: %v", err)
	}
}

func TestShimsInstallWrapperAgentIDInstallsNamespacedOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--wrapper", "gemini-cli", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ac-gemini")); err != nil {
		t.Fatalf("ac-gemini shim missing: %v", err)
	}
	for _, name := range []string{"gemini", "gemini-cli"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s alias should not be installed by default: %v", name, err)
		}
	}
}

func TestShimsInstallAliasesOptIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--wrapper", "gemini-cli", "--aliases", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ac-gemini", "gemini", "gemini-cli"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s shim missing: %v", name, err)
		}
	}
}

// WI-E3 (gemini input): `agy` is the actual gemini-cli binary name on PATH, so
// the launcher must recognize it as the gemini-cli wrapper / ac-gemini.
func TestShims_AgyResolvesToGeminiWrapper(t *testing.T) {
	spec, ok := shimSpecForName("agy")
	if !ok {
		t.Fatal("shimSpecForName(\"agy\") not recognized; want the gemini-cli wrapper")
	}
	if spec.Name != "ac-gemini" || spec.AgentID != "gemini-cli" {
		t.Fatalf("agy resolved to %s/%s, want ac-gemini/gemini-cli", spec.Name, spec.AgentID)
	}
	// agy must also be a valid --wrapper alias selector and a real-binary candidate.
	selected, err := selectShimSpecs("agy")
	if err != nil {
		t.Fatalf("selectShimSpecs(\"agy\"): %v", err)
	}
	if len(selected) != 1 || selected[0].Name != "ac-gemini" {
		t.Fatalf("selectShimSpecs(\"agy\") = %v, want [ac-gemini]", selected)
	}
	foundCandidate := false
	for _, c := range spec.Candidates {
		if c == "agy" {
			foundCandidate = true
		}
	}
	if !foundCandidate {
		t.Fatalf("ac-gemini candidates %v missing agy (real binary cannot be resolved)", spec.Candidates)
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
		err := cmdShims([]string{"exec", "--name", "ac-codex", "--shim-dir", shimDir})
		if err == nil {
			t.Fatal("expected hard discovery error")
		}
		if !strings.Contains(err.Error(), "shim discovery failed") {
			t.Fatalf("error should mention shim discovery: %v", err)
		}
	})
}
