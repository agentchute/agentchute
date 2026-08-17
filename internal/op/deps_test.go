package op

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

const loopPkg = "github.com/agentchute/agentchute/internal/loop"

// The dependency direction is the whole reason every wrapped helper MOVED here
// instead of being called in place (B1/B2): an op that called back into
// internal/cli would close a cycle, and one that reached into a wire or
// transport package would put framing concerns inside the seam. This is the
// mechanical check that keeps that stated rule true.
//
// Implemented with go/parser over the package's own files — stdlib only, no new
// dependency and no build-system assumptions.
//
// Mutation-tested rather than assumed: adding a BLANK import of another
// internal package to internal/op fails this test with the right message. The
// blank form is the one that actually exercises the parser, since a normal
// unused import fails at compile time and never reaches the test.
func TestOpImportsOnlyStdlibAndLoop(t *testing.T) {
	for file, imports := range packageImports(t, ".", true) {
		for _, imp := range imports {
			// An import whose first path element carries no dot is standard
			// library ("os", "path/filepath"); anything else is a module.
			if !strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
				continue
			}
			if imp == loopPkg {
				continue
			}
			t.Fatalf("%s imports %q: internal/op may import only the standard library and %s", file, imp, loopPkg)
		}
	}
}

// The other direction, stated as a rule and now enforced: internal/loop keeps
// the primitives and must never learn about the seam above it.
func TestLoopDoesNotImportOp(t *testing.T) {
	for file, imports := range packageImports(t, "../loop", false) {
		for _, imp := range imports {
			if strings.HasSuffix(imp, "/internal/op") {
				t.Fatalf("%s imports %q: the dependency direction is one-way", file, imp)
			}
		}
	}
}

// packageImports maps file name -> import paths for every .go file in dir,
// including tests when withTests is set.
//
// internal/op's own files are checked WITH tests: an op test importing
// internal/cli happens to be a compile error today (these are in-package tests,
// so it would close a cycle), but a future internal/hubwire is not — no cycle,
// so nothing but this check would catch a test reaching down into the layer the
// seam exists to stay above (opus-xhigh, PR #148 gate).
func packageImports(t *testing.T, dir string, withTests bool) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return withTests || !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[name] = append(out[name], path)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no Go files parsed under %s — the check would pass vacuously", dir)
	}
	return out
}
