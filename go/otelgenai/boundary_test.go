package otelgenai_test

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestImportBoundary pins that the util stays usable outside agento11y: the
// package may import the standard library, go.opentelemetry.io/otel/*, and its
// own packages, and nothing else. That is also the set an upstream contrib
// package is allowed to depend on.
//
// This test holds the package's own tests to the same rule. A test that
// reached for agento11y would show the package needs it, and independence from
// agento11y is the property pinned here.
func TestImportBoundary(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == "." {
				return nil
			}
			_, statErr := os.Stat(filepath.Join(path, "go.mod"))
			switch {
			case statErr == nil:
				return filepath.SkipDir
			case !errors.Is(statErr, os.ErrNotExist):
				return statErr
			default:
				return nil
			}
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, filepath.Clean(path), nil, parser.ImportsOnly)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return fmt.Errorf("%s: unquote import %s: %w", path, spec.Path.Value, unquoteErr)
			}
			if allowedImport(imported) {
				continue
			}
			t.Errorf("%s imports %q; only the standard library, go.opentelemetry.io/otel/*, and this package are allowed", path, imported)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk package: %v", err)
	}
}

const modulePath = "github.com/grafana/agento11y/go/otelgenai"

func allowedImport(path string) bool {
	if path == "go.opentelemetry.io/otel" || strings.HasPrefix(path, "go.opentelemetry.io/otel/") {
		return true
	}
	if path == modulePath || strings.HasPrefix(path, modulePath+"/") {
		return true
	}
	// A standard-library path's first element never contains a dot.
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
