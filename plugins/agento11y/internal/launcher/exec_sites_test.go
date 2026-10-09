package launcher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecFnIsOnlyCalledByThisPackage asserts that no launcher invokes its
// execFn seam directly: the execve handoff goes through Exec, which is the one
// place that prepends argv[0] and wraps the failure as "exec <name>".
//
// Each launcher keeps its own `execFn = syscall.Exec` package var as a test
// seam, so the declarations are expected and this does not look for them. What
// it forbids is a *call*. Three launchers (copilot, vibe, kiro) used to
// reproduce Exec's body inline, which meant a change to the handoff reached
// four of seven launchers and silently skipped the rest. A new launcher that
// copies one of those bodies would reintroduce the split, and nothing else in
// the suite would notice.
func TestExecFnIsOnlyCalledByThisPackage(t *testing.T) {
	root := moduleInternalDir(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// This package is the one legitimate caller.
		if filepath.Dir(path) == filepath.Join(root, "launcher") {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "execFn" {
				return true
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			offenders = append(offenders, rel+":"+fset.Position(call.Pos()).String()[len(path)+1:])
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(offenders) > 0 {
		t.Errorf("execFn is called directly at %v; route the handoff through launcher.Exec(execFn, bin, name, args, env) so every launcher shares one exec path", offenders)
	}
}

// moduleInternalDir returns the absolute path of the module's internal/
// directory. The test runs with its own package dir as the working directory,
// so internal/ is the parent of internal/launcher.
func moduleInternalDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Dir(wd)
	if filepath.Base(root) != "internal" {
		t.Fatalf("expected to run from internal/launcher, got %s", wd)
	}
	return root
}
