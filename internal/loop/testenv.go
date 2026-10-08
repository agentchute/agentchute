// Test-environment guard (opus-xhigh C4): keeps test binaries away from a
// live agentchute pool.
//
// A lane launched by `agentchute serve` carries its pool, identity and fence
// token in AGENTCHUTE_* variables. A bare `go test ./internal/cli -run X`
// typed in that lane inherits them, and on 2026-08-12 one such run kicked the
// whole fleet. tools/test.sh strips them, but only for runs that go through
// it. Every package whose tests resolve a pool calls MustStripTestEnv from
// TestMain, so the strip holds for any way the tests are started. It lives
// here, not in a test-only package, because internal/op may import only the
// standard library and this package.
//
// Test helpers that re-execute the test binary signal their child mode with
// ACTEST_* variables, never AGENTCHUTE_*: those would be stripped.
package loop

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnvPrefix is the namespace of every agentchute environment variable.
const EnvPrefix = "AGENTCHUTE_"

// StripTestEnv unsets every AGENTCHUTE_* variable in this process, then
// reports any that is still visible.
func StripTestEnv() error { return stripTestEnv(os.Environ, os.Unsetenv) }

func stripTestEnv(environ func() []string, unset func(string) error) error {
	for _, name := range agentchuteNames(environ()) {
		_ = unset(name)
	}
	if left := agentchuteNames(environ()); len(left) > 0 {
		return fmt.Errorf("refusing to run tests: %s still set after stripping; a test must never see a live serve's pool, identity or fence token", strings.Join(left, ", "))
	}
	return nil
}

func agentchuteNames(env []string) []string {
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, EnvPrefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// MustStripTestEnv is for TestMain: it strips, and exits the test binary with
// status 3 and the reason on stderr when a variable survives.
func MustStripTestEnv() {
	if err := StripTestEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "test environment:", err)
		os.Exit(3)
	}
}
