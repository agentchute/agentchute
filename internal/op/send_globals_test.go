package op

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSendHubPathHasNoMutablePackageState is the WI-3.6 check. It follows the
// local function call graph from Send and permits only the package's fixed
// error sentinels. Any new package variable reachable from the production send
// path fails, regardless of whether its name is exported.
func TestSendHubPathHasNoMutablePackageState(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	dir := filepath.Dir(self)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info os.FileInfo) bool {
		return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := pkgs["op"]
	funcs := map[string]*ast.FuncDecl{}
	vars := map[string]bool{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					funcs[decl.Name.Name] = decl
				}
			case *ast.GenDecl:
				if decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						vars[name.Name] = true
					}
				}
			}
		}
	}
	allowed := map[string]bool{
		"ErrNotRegistered": true, "ErrRecipientUnknown": true,
		"ErrRecipientUnreadable": true, "ErrFenced": true,
		"ErrLeaseHeld": true, "ErrRecipientStale": true,
		"ErrRecipientRacing": true, "ErrOrder": true,
	}
	visited := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		fn := funcs[name]
		if fn == nil || fn.Body == nil {
			return
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.Ident:
				if vars[node.Name] && !allowed[node.Name] {
					t.Errorf("op.Send reaches mutable package state %q", node.Name)
				}
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok && funcs[id.Name] != nil {
					walk(id.Name)
				}
			}
			return true
		})
	}
	walk("Send")
}
