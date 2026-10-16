package launcher

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecFnIsOnlyCalledByThisPackage asserts that no launcher invokes its
// execFn seam directly: the handoff goes through Exec, which is the one place
// that prepends argv[0] and wraps the failure as "exec <name>".
//
// Each launcher keeps its own `execFn = launcher.DefaultExec` package var as a
// test seam, so the declarations are expected and this does not look for them.
// What it forbids is a *call*. Three launchers (copilot, vibe, kiro) used to
// reproduce Exec's body inline, which meant a change to the handoff reached
// four of seven launchers and silently skipped the rest. A new launcher that
// copies one of those bodies would reintroduce the split, and nothing else in
// the suite would notice.
func TestExecFnIsOnlyCalledByThisPackage(t *testing.T) {
	var offenders []string
	for _, f := range productionFiles(t) {
		if f.inLauncher {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "execFn" {
				return true
			}
			offenders = append(offenders, f.position(call.Pos()))
			return true
		})
	}

	if len(offenders) > 0 {
		t.Errorf("execFn is called directly at %v; route the handoff through launcher.Exec(execFn, bin, name, args, env) so every launcher shares one exec path", offenders)
	}
}

// TestExecFnDefaultsToDefaultExec asserts that every launcher's seam starts at
// the platform handoff. The adapter tests replace execFn, so a launcher left on
// syscall.Exec passes all of them and fails only when a Windows user runs it.
func TestExecFnDefaultsToDefaultExec(t *testing.T) {
	var seams int
	var offenders []string
	for _, f := range productionFiles(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if isSelector(n, "syscall", "Exec") && f.rel != filepath.Join("launcher", "handoff_unix.go") {
					offenders = append(offenders, f.position(n.Pos())+" uses syscall.Exec")
				}
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if name.Name != "execFn" {
						continue
					}
					seams++
					if i >= len(n.Values) {
						offenders = append(offenders, f.position(name.Pos())+" declares execFn without a default")
						continue
					}
					if sel, ok := n.Values[i].(*ast.SelectorExpr); !ok || !isSelector(sel, "launcher", "DefaultExec") {
						offenders = append(offenders, f.position(name.Pos())+" does not default execFn to launcher.DefaultExec")
					}
				}
			}
			return true
		})
	}

	if seams == 0 {
		t.Fatal("found no execFn seams; the walk no longer reaches the launchers")
	}
	if len(offenders) > 0 {
		t.Errorf("launcher handoffs must start at launcher.DefaultExec: %v", offenders)
	}
}

type productionFile struct {
	rel        string
	inLauncher bool
	fset       *token.FileSet
	file       *ast.File
}

func (f productionFile) position(pos token.Pos) string {
	p := f.fset.Position(pos)
	return fmt.Sprintf("%s:%d:%d", f.rel, p.Line, p.Column)
}

// productionFiles parses every non-test Go file under the module's internal/
// directory, regardless of build tags.
func productionFiles(t *testing.T) []productionFile {
	t.Helper()
	root := moduleInternalDir(t)

	var files []productionFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, productionFile{
			rel:        rel,
			inLauncher: filepath.Dir(path) == filepath.Join(root, "launcher"),
			fset:       fset,
			file:       file,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

func isSelector(sel *ast.SelectorExpr, pkg, name string) bool {
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg && sel.Sel.Name == name
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
