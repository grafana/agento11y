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
// package may import the standard library, go.opentelemetry.io/otel/*, its
// own packages, and the shared skill URI scrubber. The scrubber is checked too
// and must move with this package if it moves to upstream contrib.
//
// This test holds the package's own tests to the same rule. A test that
// reached for agento11y would show the package needs it, and independence from
// agento11y is the property pinned here.
func TestImportBoundary(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	checkImports := func(path string, entry fs.DirEntry, err error) error {
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
			t.Errorf("%s imports %q; only the standard library, go.opentelemetry.io/otel/*, this package, and the shared skill URI scrubber are allowed", path, imported)
		}
		return nil
	}
	for _, root := range []string{".", "../internal/skilluri"} {
		if err := filepath.WalkDir(root, checkImports); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

const modulePath = "github.com/grafana/agento11y/go/otelgenai"

func allowedImport(path string) bool {
	if path == "go.opentelemetry.io/otel" || strings.HasPrefix(path, "go.opentelemetry.io/otel/") {
		return true
	}
	if path == modulePath || strings.HasPrefix(path, modulePath+"/") || path == "github.com/grafana/agento11y/go/internal/skilluri" {
		return true
	}
	// A standard-library path's first element never contains a dot.
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
