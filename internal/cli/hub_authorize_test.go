package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func setupHubAuthorizeTest(t *testing.T) (home, pool, executable, key string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	pool = filepath.Join(t.TempDir(), "pool")
	if err := os.MkdirAll(filepath.Join(pool, ".agentchute", "loop", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool, "AGENTCHUTE.md"), []byte("# test pool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable = filepath.Join(home, "bin", "agentchute")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("test binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	oldExecutable := hubAuthorizeExecutable
	oldLink := hubAuthorizeLink
	hubAuthorizeExecutable = func() (string, error) { return executable, nil }
	hubAuthorizeLink = os.Link
	t.Cleanup(func() {
		hubAuthorizeExecutable = oldExecutable
		hubAuthorizeLink = oldLink
	})
	key = "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("test-key-one")) + " input-comment"
	return home, pool, executable, key
}

func readAuthorizeTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHubAuthorizeWritesExactForcedLineAndIsIdempotent(t *testing.T) {
	home, pool, executable, key := setupHubAuthorizeTest(t)
	var output bytes.Buffer
	opts := hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key}
	if err := runHubAuthorize(opts, &output); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(pool, ".agentchute", "loop", "state", "pool.id")
	identity := string(readAuthorizeTestFile(t, identityPath))
	if !poolIDPattern.MatchString(identity) {
		t.Fatalf("pool.id = %q", identity)
	}
	poolID := strings.TrimSpace(identity)
	resolvedPool, err := filepath.EvalSymlinks(pool)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(filepath.Clean(resolvedPool)))
	if expectedID := hex.EncodeToString(digest[:])[:12]; poolID != expectedID {
		t.Fatalf("pool id = %s, want %s", poolID, expectedID)
	}
	marker := hubKeyMarker("codex-tiny", poolID)
	expected := fmt.Sprintf("restrict,command=\"%s hub session --agent codex-tiny --pool %s --pool-id %s\" %s %s\n", executable, pool, poolID, strings.Join(strings.Fields(key)[:2], " "), marker)
	authorizedPath := filepath.Join(home, ".ssh", "authorized_keys")
	if got := string(readAuthorizeTestFile(t, authorizedPath)); got != expected {
		t.Fatalf("authorized_keys:\n%s\nwant:\n%s", got, expected)
	}
	if info, err := os.Stat(identityPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pool.id mode = %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(home, ".ssh")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode = %v, %v", info, err)
	}
	if info, err := os.Stat(authorizedPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_keys mode = %v, %v", info, err)
	}

	output.Reset()
	if err := runHubAuthorize(opts, &output); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(readAuthorizeTestFile(t, authorizedPath)), marker); got != 1 {
		t.Fatalf("marker count = %d", got)
	}
	if !strings.Contains(output.String(), "already present") {
		t.Fatalf("idempotent output = %q", output.String())
	}

	output.Reset()
	if err := runHubAuthorize(hubAuthorizeOptions{List: true}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "PASS "+marker) {
		t.Fatalf("list output = %q", output.String())
	}
}

func TestHubAuthorizeDuplicateReplaceAndRevoke(t *testing.T) {
	home, pool, _, key := setupHubAuthorizeTest(t)
	authorizedPath := filepath.Join(home, ".ssh", "authorized_keys")
	writeAuthorizeFixture(t, authorizedPath, "ssh-ed25519 dW5yZWxhdGVk operator-key\n", 0o600)
	opts := hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key}
	if err := runHubAuthorize(opts, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), readAuthorizeTestFile(t, authorizedPath)...)
	key2 := "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("test-key-two"))
	err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key2}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "One key = one agent id") || !strings.Contains(err.Error(), "--replace-key") || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("duplicate error = %v", err)
	}
	if after := readAuthorizeTestFile(t, authorizedPath); !bytes.Equal(before, after) {
		t.Fatal("duplicate refusal changed authorized_keys")
	}

	var output bytes.Buffer
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key2, ReplaceKey: true}, &output); err != nil {
		t.Fatal(err)
	}
	if got := string(readAuthorizeTestFile(t, authorizedPath)); strings.Contains(got, strings.Fields(key)[1]) || !strings.Contains(got, strings.Fields(key2)[1]) {
		t.Fatalf("replacement content = %q", got)
	}
	if !strings.Contains(output.String(), "replaced") {
		t.Fatalf("replace output = %q", output.String())
	}

	output.Reset()
	if err := runHubAuthorize(hubAuthorizeOptions{Revoke: "codex-tiny", Pool: pool}, &output); err != nil {
		t.Fatal(err)
	}
	if got := string(readAuthorizeTestFile(t, authorizedPath)); strings.Contains(got, "agentchute:codex-tiny:") {
		t.Fatalf("revoked marker remains: %q", got)
	} else if !strings.Contains(got, "operator-key") {
		t.Fatalf("revoke removed unrelated key: %q", got)
	}
	if !strings.Contains(output.String(), "1 line(s) removed") {
		t.Fatalf("revoke output = %q", output.String())
	}
}

func TestHubAuthorizeMarkerIsScopedByPool(t *testing.T) {
	home, poolA, _, key := setupHubAuthorizeTest(t)
	poolB := filepath.Join(t.TempDir(), "pool-b")
	if err := os.MkdirAll(filepath.Join(poolB, ".agentchute", "loop", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolB, "AGENTCHUTE.md"), []byte("# pool b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []string{poolA, poolB} {
		if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runHubAuthorize(hubAuthorizeOptions{Revoke: "codex", Pool: poolA}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	content := string(readAuthorizeTestFile(t, filepath.Join(home, ".ssh", "authorized_keys")))
	poolAID := strings.TrimSpace(string(readAuthorizeTestFile(t, filepath.Join(poolA, ".agentchute", "loop", "state", "pool.id"))))
	poolBID := strings.TrimSpace(string(readAuthorizeTestFile(t, filepath.Join(poolB, ".agentchute", "loop", "state", "pool.id"))))
	if strings.Contains(content, hubKeyMarker("codex", poolAID)) || !strings.Contains(content, hubKeyMarker("codex", poolBID)) {
		t.Fatalf("authorized_keys after pool-scoped revoke = %q", content)
	}
}

func TestHubAuthorizeRevokePrintsLiveSessionCutCommand(t *testing.T) {
	_, pool, _, key := setupHubAuthorizeTest(t)
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex-tiny", Pool: pool, Key: key}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cfg := &loop.Config{ControlRepo: pool, LoopDir: filepath.Join(pool, ".agentchute", "loop")}
	lease, err := loop.AcquireServeLease(cfg, "codex-tiny")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })
	var output bytes.Buffer
	if err := runHubAuthorize(hubAuthorizeOptions{Revoke: "codex-tiny", Pool: pool}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), fmt.Sprintf("kill %d", os.Getpid())) {
		t.Fatalf("revoke output = %q", output.String())
	}
}

func TestHubAuthorizeRejectsMalformedPoolIdentityWithoutKeyWrite(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"oversized":        func(t *testing.T, path string) { writeAuthorizeFixture(t, path, strings.Repeat("a", 65), 0o600) },
		"embedded-newline": func(t *testing.T, path string) { writeAuthorizeFixture(t, path, "012345\n6789ab\n", 0o600) },
		"quote":            func(t *testing.T, path string) { writeAuthorizeFixture(t, path, `0123456789a"\n`, 0o600) },
		"dollar":           func(t *testing.T, path string) { writeAuthorizeFixture(t, path, "01234567$()\n", 0o600) },
		"whitespace":       func(t *testing.T, path string) { writeAuthorizeFixture(t, path, "0123456789a \n", 0o600) },
		"wrong-length":     func(t *testing.T, path string) { writeAuthorizeFixture(t, path, "0123456789a\n", 0o600) },
		"wrong-charset":    func(t *testing.T, path string) { writeAuthorizeFixture(t, path, "0123456789aG\n", 0o600) },
		"wrong-mode":       func(t *testing.T, path string) { writeAuthorizeFixture(t, path, "0123456789ab\n", 0o644) },
		"symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "identity")
			writeAuthorizeFixture(t, target, "0123456789ab\n", 0o600)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			home, pool, _, key := setupHubAuthorizeTest(t)
			identityPath := filepath.Join(pool, ".agentchute", "loop", "state", "pool.id")
			plant(t, identityPath)
			authorizedPath := filepath.Join(home, ".ssh", "authorized_keys")
			writeAuthorizeFixture(t, authorizedPath, "ssh-ed25519 sentinel unrelated\n", 0o600)
			before := readAuthorizeTestFile(t, authorizedPath)
			err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "Nothing was written to authorized_keys") {
				t.Fatalf("error = %v", err)
			}
			if after := readAuthorizeTestFile(t, authorizedPath); !bytes.Equal(before, after) {
				t.Fatal("malformed pool identity changed authorized_keys")
			}
		})
	}
}

func TestHubAuthorizeValidatesPoolIdentityAfterLosingCreateRace(t *testing.T) {
	home, pool, _, key := setupHubAuthorizeTest(t)
	identityPath := filepath.Join(pool, ".agentchute", "loop", "state", "pool.id")
	authorizedPath := filepath.Join(home, ".ssh", "authorized_keys")
	writeAuthorizeFixture(t, authorizedPath, "ssh-ed25519 dW5yZWxhdGVk operator-key\n", 0o600)
	before := readAuthorizeTestFile(t, authorizedPath)
	hubAuthorizeLink = func(_, newPath string) error {
		writeAuthorizeFixture(t, newPath, "corrupt-winner\n", 0o600)
		return &os.LinkError{Op: "link", New: newPath, Err: fs.ErrExist}
	}
	err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "Nothing was written to authorized_keys") {
		t.Fatalf("loser re-read error = %v", err)
	}
	if after := readAuthorizeTestFile(t, authorizedPath); !bytes.Equal(before, after) {
		t.Fatal("loser re-read failure changed authorized_keys")
	}
	if got := string(readAuthorizeTestFile(t, identityPath)); got != "corrupt-winner\n" {
		t.Fatalf("winner pool.id = %q", got)
	}
}

func writeAuthorizeFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestHubAuthorizeRejectsUnsafePathsAndKeys(t *testing.T) {
	_, _, _, key := setupHubAuthorizeTest(t)
	for _, pool := range []string{
		"/tmp/pool space", "/tmp/pool;touch", `/tmp/pool"quoted`, "/tmp/pool'quoted",
		"/tmp/pool$(id)", "/tmp/pool`id`", `/tmp/pool\slash`, "/tmp/pool&other", "/tmp/pool|other",
	} {
		err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "outside the safe set") {
			t.Fatalf("pool %q error = %v", pool, err)
		}
	}
	for _, key := range []string{"", "ssh-ed25519 not*base64", "SSH-ED25519 AAAA", "ssh-ed25519 ==="} {
		_, pool, _, _ := setupHubAuthorizeTest(t)
		err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "--key") {
			t.Fatalf("key %q error = %v", key, err)
		}
	}
}

func TestHubAuthorizeAliasesReuseOnePoolIdentity(t *testing.T) {
	home, pool, _, key := setupHubAuthorizeTest(t)
	aliasRoot := t.TempDir()
	aliasA := filepath.Join(aliasRoot, "pool-a")
	aliasB := filepath.Join(aliasRoot, "pool-b")
	if err := os.Symlink(pool, aliasA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pool, aliasB); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, tc := range []struct{ agent, path string }{{"codex-a", aliasA}, {"codex-b", aliasB}} {
		wg.Add(1)
		go func(agent, path string) {
			defer wg.Done()
			errs <- runHubAuthorize(hubAuthorizeOptions{Agent: agent, Pool: path, Key: key}, &bytes.Buffer{})
		}(tc.agent, tc.path)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	poolID := strings.TrimSpace(string(readAuthorizeTestFile(t, filepath.Join(pool, ".agentchute", "loop", "state", "pool.id"))))
	content := string(readAuthorizeTestFile(t, filepath.Join(home, ".ssh", "authorized_keys")))
	if strings.Count(content, ":"+poolID) != 2 {
		t.Fatalf("authorized_keys = %q; want two markers for pool %s", content, poolID)
	}
}

func TestHubAuthorizeListFailsBrokenBinaryAndPermissions(t *testing.T) {
	home, pool, executable, key := setupHubAuthorizeTest(t)
	if err := runHubAuthorize(hubAuthorizeOptions{Agent: "codex", Pool: pool, Key: key}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, ".ssh", "authorized_keys"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runHubAuthorize(hubAuthorizeOptions{List: true}, &output)
	if err == nil || !strings.Contains(output.String(), "FAIL agentchute:codex:") || !strings.Contains(output.String(), "binary does not exist or is not executable") || !strings.Contains(output.String(), "authorized_keys must be mode 0600") {
		t.Fatalf("list output = %q, err = %v", output.String(), err)
	}
}

func TestHubAuthorizeListFailsWrongSSHDirectoryModeWithoutKeyFile(t *testing.T) {
	home, _, _, _ := setupHubAuthorizeTest(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runHubAuthorize(hubAuthorizeOptions{List: true}, &output)
	if err == nil || !strings.Contains(err.Error(), "set .ssh to 0700") {
		t.Fatalf("list output = %q, err = %v", output.String(), err)
	}
}

func TestHubAuthorizeCommandDispatch(t *testing.T) {
	_, pool, _, key := setupHubAuthorizeTest(t)
	stdout, _, err := captureStdoutStderr(t, func() error {
		return cmdHub([]string{"authorize", "--agent", "codex", "--pool", pool, "--key", key})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "authorized: codex -> pool") {
		t.Fatalf("stdout = %q", stdout)
	}
}
