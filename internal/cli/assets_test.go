package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain injects the build-time assets that Main normally supplies (the
// embeds live in the root main package and never run under `go test`), and
// restores the repo-root working directory the package's tests assumed before
// the package moved from the repo root into internal/cli.
//
// Both are required for a byte-identical test environment:
//   - Asset vars (spec/templates/hooks) are read from the repo-root sources so
//     handlers and the drift/injection crown-jewel tests see the same content
//     the prod embed would produce.
//   - Chdir to the repo root so tests that read repo-root files via a
//     cwd-relative path (e.g. TestTemplatesMatchRepoWrappers reading
//     "CLAUDE.md") resolve exactly as they did at the old package location.
//     Tests that chdir themselves capture/restore os.Getwd() dynamically, so
//     this baseline is safe.
//
// The repo root is derived from this file's compile-time path via
// runtime.Caller, NOT from the working directory. That keeps setup correct even
// when the test binary is re-exec'd as a subprocess helper with a different cwd
// (e.g. TestActiveSessionSelfCheckHelperProcess), which runs TestMain too.
func TestMain(m *testing.M) {
	root := repoRootForTests()

	mustRead := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			panic("cli test setup: read asset " + rel + ": " + err.Error())
		}
		return string(b)
	}

	embeddedSpecContent = mustRead("AGENTCHUTE.md")
	enrollmentWrapperTemplate = mustRead(filepath.Join("templates", "enrollment", "wrapper.md"))
	enrollmentAgentsTemplate = mustRead(filepath.Join("templates", "enrollment", "agents.md"))
	// Prod embeds `//go:embed all:examples/hooks`, so paths read via hooksFS are
	// rooted at the repo (e.g. "examples/hooks/..."). os.DirFS(root) with an
	// absolute root exposes the identical layout and stays valid across chdirs.
	hooksFS = os.DirFS(root)

	if err := os.Chdir(root); err != nil {
		panic("cli test setup: chdir to repo root: " + err.Error())
	}

	os.Exit(m.Run())
}

// repoRootForTests returns the absolute repo root, computed from this source
// file's location (internal/cli/assets_test.go) so it is independent of the
// process working directory.
func repoRootForTests() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("cli test setup: runtime.Caller failed to locate assets_test.go")
	}
	// thisFile = <repo>/internal/cli/assets_test.go → up three dirs = <repo>.
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}
