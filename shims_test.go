package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyShimScript renders a pre-dispatcher per-wrapper `ac-*`/same-name shim
// (the format the removed production renderShimScript emitted). Kept as a
// test-only fixture generator so cleanup tests can plant legacy shims and assert
// setup removes them; production no longer generates per-wrapper shims.
func legacyShimScript(agentchuteBin, shimDir, name string) string {
	return fmt.Sprintf(`#!/bin/sh
# agentchute shim v1
AGENTCHUTE_BIN=${AGENTCHUTE_BIN:-%s}
exec "$AGENTCHUTE_BIN" shims exec --name %s --shim-dir %s -- "$@"
`, shellQuote(agentchuteBin), shellQuote(name), shellQuote(shimDir))
}

func TestShimsInstallWritesDispatcher(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--quiet"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ac")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dispatcher: %v", err)
	}
	if !strings.Contains(string(data), "dispatch --shim-dir") {
		t.Fatalf("dispatcher does not exec `agentchute dispatch`:\n%s", data)
	}
	if !strings.Contains(string(data), "AGENTCHUTE_BIN=") {
		t.Fatalf("dispatcher missing AGENTCHUTE_BIN override:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("dispatcher is not executable: mode %v", info.Mode().Perm())
	}
	// The dispatcher replaces, and never co-installs, the legacy ac-* launchers.
	for _, name := range []string{"ac-claude", "ac-codex", "ac-gemini", "ac-grok"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy launcher %s should not be installed: %v", name, err)
		}
	}
}

// The legacy --wrapper/--aliases no-op flags were removed; passing them now
// errors. Plain `shims install` still installs only the wrapper-agnostic `ac`
// dispatcher.
func TestShimsInstallRejectsRemovedLegacyFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := cmdShims([]string{"install", "--dir", dir, "--aliases", "--quiet"}); err == nil {
		t.Fatal("shims install --aliases should now error (flag removed)")
	}
	if err := cmdShims([]string{"install", "--dir", dir, "--wrapper", "codex", "--quiet"}); err == nil {
		t.Fatal("shims install --wrapper should now error (flag removed)")
	}
	if err := cmdShims([]string{"install", "--dir", dir, "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ac")); err != nil {
		t.Fatalf("ac dispatcher should be installed: %v", err)
	}
	for _, name := range []string{"ac-codex", "codex", "ac-gemini", "gemini"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should not be installed: %v", name, err)
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
	spec, ok := wrapperSpecForName("ac-codex")
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
		t.Fatal("expected existing dispatcher error")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error should mention --force: %v", err)
	}
	// --force makes the re-install idempotent (content stable).
	before := mustRead(t, filepath.Join(dir, "ac"))
	if err := cmdShims([]string{"install", "--dir", dir, "--force", "--quiet"}); err != nil {
		t.Fatalf("--force re-install should succeed: %v", err)
	}
	after := mustRead(t, filepath.Join(dir, "ac"))
	if string(before) != string(after) {
		t.Fatalf("dispatcher content changed across idempotent --force install:\n%s\n---\n%s", before, after)
	}
}

func TestIsAgentchuteDispatcher(t *testing.T) {
	dir := t.TempDir()

	ours := filepath.Join(dir, "ac")
	mustWrite(t, ours, []byte(renderDispatcherScript("/bin/agentchute", dir)))
	if ok, err := isAgentchuteDispatcher(ours); err != nil || !ok {
		t.Fatalf("our dispatcher: got (%v,%v), want (true,nil)", ok, err)
	}

	foreign := filepath.Join(dir, "foreign")
	mustWrite(t, foreign, []byte("#!/bin/sh\necho hi\n"))
	if ok, err := isAgentchuteDispatcher(foreign); err != nil || ok {
		t.Fatalf("foreign file: got (%v,%v), want (false,nil)", ok, err)
	}

	if ok, err := isAgentchuteDispatcher(filepath.Join(dir, "missing")); err != nil || ok {
		t.Fatalf("missing file: got (%v,%v), want (false,nil)", ok, err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(ours, link); err != nil {
		t.Fatal(err)
	}
	if ok, err := isAgentchuteDispatcher(link); err != nil || ok {
		t.Fatalf("symlink (even to our dispatcher): got (%v,%v), want (false,nil)", ok, err)
	}
}

func TestInstallDispatcherRefusesForeignAndSymlink(t *testing.T) {
	// Foreign regular file at <dir>/ac: refuse, never clobber, never with --force.
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "ac")
	mustWrite(t, foreign, []byte("#!/bin/sh\n# user's own ac\nexit 0\n"))
	original := mustRead(t, foreign)
	for _, force := range []bool{false, true} {
		err := installDispatcher(dir, "/bin/agentchute", force)
		if err == nil || !strings.Contains(err.Error(), "refusing to overwrite non-agentchute ac") {
			t.Fatalf("force=%v: expected refusal error, got %v", force, err)
		}
	}
	if string(mustRead(t, foreign)) != string(original) {
		t.Fatal("foreign ac was modified; collision guard breached")
	}

	// Symlink at <dir2>/ac: refuse to follow/replace even with --force.
	dir2 := filepath.Join(t.TempDir(), "bin2")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real-ac")
	mustWrite(t, target, []byte("#!/bin/sh\nexit 0\n"))
	link := filepath.Join(dir2, "ac")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := installDispatcher(dir2, "/bin/agentchute", true); err == nil || !strings.Contains(err.Error(), "refusing to overwrite non-agentchute ac") {
		t.Fatalf("symlink: expected refusal error, got %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink ac was replaced; collision guard breached (mode=%v err=%v)", fi.Mode(), err)
	}
}

// installDispatcher writes ONLY into the requested dir — never the system
// /usr/sbin/ac accounting command.
func TestInstallDispatcherWritesIntoRequestedDirOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := installDispatcher(dir, "/bin/agentchute", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ac")); err != nil {
		t.Fatalf("dispatcher not written to requested dir: %v", err)
	}
	// Sanity: the target path is inside dir, not /usr/sbin.
	if strings.HasPrefix(filepath.Join(dir, "ac"), "/usr/sbin") {
		t.Fatal("dispatcher path resolved under /usr/sbin")
	}
}

func TestRemoveLegacyWrapperShims(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Marker-bearing agentchute shims (a namespaced launcher + a same-name alias).
	mustWrite(t, filepath.Join(dir, "ac-codex"), []byte(legacyShimScript("/bin/agentchute", dir, "ac-codex")))
	mustWrite(t, filepath.Join(dir, "codex"), []byte(legacyShimScript("/bin/agentchute", dir, "codex")))
	// A user-owned same-name file WITHOUT the marker: must be left untouched.
	userClaude := []byte("#!/bin/sh\n# my own claude wrapper\nexec /opt/claude \"$@\"\n")
	mustWrite(t, filepath.Join(dir, "claude"), userClaude)
	// The dispatcher itself must never be removed by the legacy sweep.
	mustWrite(t, filepath.Join(dir, "ac"), []byte(renderDispatcherScript("/bin/agentchute", dir)))

	removed, err := removeLegacyWrapperShims(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range removed {
		got[n] = true
	}
	if !got["ac-codex"] || !got["codex"] {
		t.Fatalf("expected ac-codex and codex removed, got %v", removed)
	}
	if got["claude"] {
		t.Fatalf("user-owned claude must NOT be in removed set: %v", removed)
	}
	for _, name := range []string{"ac-codex", "codex"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed: %v", name, err)
		}
	}
	if string(mustRead(t, filepath.Join(dir, "claude"))) != string(userClaude) {
		t.Fatal("user-owned claude file was modified/removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "ac")); err != nil {
		t.Fatalf("dispatcher must survive the legacy sweep: %v", err)
	}
}

// WI-E3 (gemini input): `agy` is the actual gemini-cli binary name on PATH, so
// the launcher must recognize it as the gemini-cli wrapper / ac-gemini.
func TestShims_AgyResolvesToGeminiWrapper(t *testing.T) {
	spec, ok := wrapperSpecForName("agy")
	if !ok {
		t.Fatal("wrapperSpecForName(\"agy\") not recognized; want the gemini-cli wrapper")
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
