package weavertest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryDependencyLayouts(t *testing.T) {
	tests := []struct {
		name     string
		versions string
		manifest string
		want     string
	}{
		{
			name:     "legacy versions env and local manifest dependency",
			versions: "SEMCONV_VERSION=v1.43.0\n",
			manifest: "dependencies:\n  - schema_url: https://opentelemetry.io/schemas/1.43.0\n    registry_path: ./.build/sc-upstream-filtered\n",
			want:     "v1.43.0",
		},
		{
			name:     "current manifest URL dependency",
			versions: "WEAVER_VERSION=v0.25.1\n",
			manifest: "dependencies:\n  - schema_url: https://opentelemetry.io/schemas/1.44.0\n    registry_path: https://github.com/open-telemetry/semantic-conventions.git@v1.44.0[model]\n",
			want:     "v1.44.0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "model"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "versions.env"), []byte(test.versions), 0o644); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(root, "model", "manifest.yaml")
			if err := os.WriteFile(manifest, []byte(test.manifest), 0o644); err != nil {
				t.Fatal(err)
			}

			version, err := upstreamVersion(root)
			if err != nil {
				t.Fatalf("upstreamVersion: %v", err)
			}
			if version != test.want {
				t.Fatalf("upstreamVersion = %q, want %q", version, test.want)
			}

			filtered := filepath.Join(root, ".build", "filtered")
			if err := rewriteManifestDependency(root, filtered); err != nil {
				t.Fatalf("rewriteManifestDependency: %v", err)
			}
			contents, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			wantPath, err := filepath.Abs(filtered)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(contents); !strings.Contains(got, "registry_path: "+filepath.ToSlash(wantPath)) {
				t.Fatalf("rewritten manifest =\n%s\nwant registry path %s", got, wantPath)
			}
		})
	}
}

func TestProcessDefinitionImports(t *testing.T) {
	for _, test := range []struct {
		name       string
		spans      string
		wantImport bool
	}{
		{name: "legacy registry", spans: "groups:\n  - id: gen_ai.execute_tool.internal\n"},
		{name: "command execution kind", spans: "span_refinements:\n  - id: gen_ai.execute_tool.command.internal\n    attributes:\n      - ref: process.exit.code\n", wantImport: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "model", "gen-ai"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "model", "gen-ai", "spans.yaml"), []byte(test.spans), 0o644); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := importProcessDefinitions(root); err != nil {
					t.Fatal(err)
				}
			}
			contents, err := os.ReadFile(filepath.Join(root, "model", "weavertest-process.yaml"))
			if !test.wantImport {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy registry gained process imports: %q, %v", contents, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "file_format: definition/2\nimports:\n  attribute_groups:\n    - registry.process\n" {
				t.Fatalf("process imports = %q", contents)
			}
		})
	}
	t.Run("missing model fails", func(t *testing.T) {
		if err := importProcessDefinitions(t.TempDir()); err == nil {
			t.Fatal("missing spans accepted")
		}
	})
}

func TestRegistryProvisionStamp(t *testing.T) {
	root := t.TempDir()
	for _, stamp := range []string{"", "v1\n", "v2\n", registryStampContents} {
		if err := os.WriteFile(filepath.Join(root, ".provisioned"), []byte(stamp), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, want := isProvisioned(root), stamp == "v3\n"; got != want {
			t.Errorf("stamp %q: provisioned = %v, want %v", stamp, got, want)
		}
	}
	if isProvisioned(t.TempDir()) {
		t.Error("missing stamp accepted")
	}
}

func TestEmbeddedInputsIncludeCoverageTemplates(t *testing.T) {
	files, _, err := loadEmbeddedInputs()
	if err != nil {
		t.Fatalf("loadEmbeddedInputs: %v", err)
	}
	seen := map[string]bool{}
	for _, file := range files {
		seen[file.name] = true
	}
	for _, name := range []string{
		"templates/coverage-model/weaver.yaml",
		"templates/coverage-model/coverage-model.json.j2",
	} {
		if !seen[name] {
			t.Errorf("embedded input %s is missing", name)
		}
	}
}
