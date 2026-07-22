package agento11y

import (
	"runtime/debug"
	"testing"
)

func TestVersionFromBuildInfo(t *testing.T) {
	cases := []struct {
		name        string
		deps        []*debug.Module
		wantVersion string
		wantOK      bool
	}{
		{
			name: "released dependency",
			deps: []*debug.Module{
				{Path: "github.com/other/dep", Version: "v1.2.3"},
				{Path: sdkModulePath, Version: "v0.15.0"},
			},
			wantVersion: "0.15.0",
			wantOK:      true,
		},
		{
			name: "missing dependency",
			deps: []*debug.Module{
				{Path: "github.com/other/dep", Version: "v1.2.3"},
			},
			wantOK: false,
		},
		{
			name: "replaced dependency",
			deps: []*debug.Module{
				{
					Path:    sdkModulePath,
					Version: "v0.15.0",
					Replace: &debug.Module{Path: "../go", Version: "(devel)"},
				},
			},
			wantOK: false,
		},
		{
			name: "devel version",
			deps: []*debug.Module{
				{Path: sdkModulePath, Version: "(devel)"},
			},
			wantOK: false,
		},
		{
			name:   "no dependencies",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := versionFromBuildInfo(&debug.BuildInfo{Deps: tc.deps})
			if ok != tc.wantOK {
				t.Fatalf("versionFromBuildInfo ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.wantVersion {
				t.Fatalf("versionFromBuildInfo version = %q, want %q", got, tc.wantVersion)
			}
		})
	}
}

func TestVersionFallsBackInRepo(t *testing.T) {
	// In-repo tests run with the SDK as the main module, so build information
	// has no self-dependency and the fallback applies.
	if Version != versionFallback {
		t.Fatalf("Version = %q, want %q", Version, versionFallback)
	}
	if want := "agento11y-sdk-go/" + versionFallback; UserAgent() != want {
		t.Fatalf("UserAgent() = %q, want %q", UserAgent(), want)
	}
}
