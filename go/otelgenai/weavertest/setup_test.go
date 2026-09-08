package weavertest

import (
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
