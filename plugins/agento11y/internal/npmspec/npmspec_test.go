package npmspec

import (
	"os"
	"path/filepath"
	"testing"
)

// Name and Version split the same spec on the same `@`, so every case checks
// both halves of that decision.
func TestNameAndVersion(t *testing.T) {
	cases := []struct {
		spec        string
		wantName    string
		wantVersion string
	}{
		{spec: "@grafana/agento11y-pi", wantName: "@grafana/agento11y-pi"},
		{spec: "@grafana/agento11y-pi@0.1.1", wantName: "@grafana/agento11y-pi", wantVersion: "0.1.1"},
		{spec: "@grafana/agento11y-pi@1.0.0-rc.3", wantName: "@grafana/agento11y-pi", wantVersion: "1.0.0-rc.3"},
		{spec: "@grafana/agento11y-pi@next", wantName: "@grafana/agento11y-pi", wantVersion: "next"},
		{spec: "@grafana/agento11y-pi-extra", wantName: "@grafana/agento11y-pi-extra"},
		{spec: "@grafana/agento11y-opencode", wantName: "@grafana/agento11y-opencode"},
		{spec: "@grafana/agento11y-opencode@0.6.0", wantName: "@grafana/agento11y-opencode", wantVersion: "0.6.0"},
		{spec: "@grafana/agento11y-opencode@next", wantName: "@grafana/agento11y-opencode", wantVersion: "next"},
		{spec: "pkg", wantName: "pkg"},
		{spec: "pkg@1.0.0", wantName: "pkg", wantVersion: "1.0.0"},
		{spec: "./local-plugin", wantName: "./local-plugin"},
		{spec: "/abs/path", wantName: "/abs/path"},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			if got := Name(tc.spec); got != tc.wantName {
				t.Errorf("Name(%q) = %q, want %q", tc.spec, got, tc.wantName)
			}
			if got := Version(tc.spec); got != tc.wantVersion {
				t.Errorf("Version(%q) = %q, want %q", tc.spec, got, tc.wantVersion)
			}
		})
	}
}

func TestReadPackageJSON(t *testing.T) {
	tests := []struct {
		name        string
		contents    string
		writeFile   bool
		fileInstead bool
		wantName    string
		wantVersion string
		wantOK      bool
	}{
		{
			name:        "name and version",
			contents:    `{"name":"@grafana/agento11y-pi","version":"0.2.0"}`,
			writeFile:   true,
			wantName:    "@grafana/agento11y-pi",
			wantVersion: "0.2.0",
			wantOK:      true,
		},
		{
			name:      "no version field",
			contents:  `{"name":"@grafana/agento11y-pi"}`,
			writeFile: true,
			wantName:  "@grafana/agento11y-pi",
			wantOK:    true,
		},
		{name: "malformed json", contents: `{`, writeFile: true},
		{name: "missing package.json"},
		{name: "path is a file, not a directory", fileInstead: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "pkg")
			if tc.fileInstead {
				if err := os.WriteFile(dir, []byte("not a dir"), 0o600); err != nil {
					t.Fatalf("write file: %v", err)
				}
			} else {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if tc.writeFile {
					if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(tc.contents), 0o600); err != nil {
						t.Fatalf("write package.json: %v", err)
					}
				}
			}

			name, version, ok := ReadPackageJSON(dir)
			if ok != tc.wantOK || name != tc.wantName || version != tc.wantVersion {
				t.Fatalf("ReadPackageJSON() = (%q, %q, %v), want (%q, %q, %v)",
					name, version, ok, tc.wantName, tc.wantVersion, tc.wantOK)
			}
		})
	}
}
