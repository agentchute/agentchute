package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// run_runtime_constructor_test.go makes the runner's constructor MANDATORY
// rather than conventional.
//
// newRunnerRuntime exists because the lease has to reach two places at once —
// the runtime handle the shutdown path releases, and the op.Channel that adopts
// it — and as two independent lines in a struct literal they desynchronized:
// production released nothing and leaked serve.claim on every clean exit, while
// the whole suite stayed green. The constructor takes the lease once so the pair
// cannot come apart.
//
// But a constructor only helps if it is the sole way to build the struct.
// TestNewRunnerRuntimeWiresOneLeaseToBothHolders proves the wiring; it cannot
// prove that runWrapper (or a future construction site) actually goes through
// it. This closes that residual gap the way internal/op/deps_test.go closes the
// dependency-direction rule: a source assertion, stdlib only, no goroutines, no
// PTY, no behavior (opus-xhigh, PR #148 gate).
//
// Scoped to serve.go deliberately: newPollTestRuntime builds its own literal in
// run_test.go, which is a named exception file this merge may not touch twice.
func TestRunnerRuntimeIsOnlyBuiltByItsConstructor(t *testing.T) {
	const (
		file = "serve.go"
		ctor = "newRunnerRuntime"
	)
	// Located from this test's own source path: the package's TestMain changes
	// the working directory, so a bare relative name would not resolve.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	path := filepath.Join(filepath.Dir(self), file)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	found := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "runnerRuntime" {
				return true
			}
			found++
			if fn.Name.Name != ctor {
				t.Errorf("%s:%d: runnerRuntime built directly in %s; build it through %s, "+
					"which wires the lease to BOTH the runtime handle and the channel from one value "+
					"(a literal that sets one and not the other leaked serve.claim on every clean exit)",
					file, fset.Position(lit.Pos()).Line, fn.Name.Name, ctor)
			}
			return true
		})
	}

	// A check that found nothing would pass vacuously — e.g. if the struct were
	// renamed, or the file moved.
	if found == 0 {
		t.Fatalf("no runnerRuntime literal found in %s; this check would pass vacuously", file)
	}
}
