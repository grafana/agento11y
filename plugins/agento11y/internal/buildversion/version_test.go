package buildversion

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	tests := []struct {
		name    string
		stamped string
		info    debug.BuildInfo
		want    string
	}{
		{
			name:    "release stamp wins",
			stamped: "v0.30.0",
			info: debug.BuildInfo{
				Main:     debug.Module{Version: "v0.29.0"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}},
			},
			want: "v0.30.0",
		},
		{
			name:    "tagged module",
			stamped: "dev",
			info:    debug.BuildInfo{Main: debug.Module{Version: "v0.29.0"}},
			want:    "v0.29.0",
		},
		{
			name:    "module prerelease",
			stamped: "dev",
			info:    debug.BuildInfo{Main: debug.Module{Version: "v0.30.0-rc.1"}},
			want:    "v0.30.0-rc.1",
		},
		{
			name:    "module pseudo-version",
			stamped: "dev",
			info:    debug.BuildInfo{Main: debug.Module{Version: "v0.30.1-0.20260818160951-60b2ab8650b5"}},
			want:    "dev-60b2ab8",
		},
		{
			name:    "module pseudo-version with incompatible suffix",
			stamped: "dev",
			info:    debug.BuildInfo{Main: debug.Module{Version: "v2.0.1-0.20260818160951-60b2ab8650b5+incompatible"}},
			want:    "dev-60b2ab8",
		},
		{
			name:    "local checkout",
			stamped: "dev",
			info: debug.BuildInfo{
				Main:     debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "60b2ab8650b5b3ca35884e309ed201ee039d5b00"}},
			},
			want: "dev-60b2ab8",
		},
		{
			name:    "modified local checkout",
			stamped: "dev",
			info: debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.modified", Value: "true"},
					{Key: "vcs.revision", Value: "60b2ab8650b5b3ca35884e309ed201ee039d5b00"},
				},
			},
			want: "dev-60b2ab8-dirty",
		},
		{
			name:    "short revision",
			stamped: "dev",
			info: debug.BuildInfo{
				Main:     debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "60b2"}},
			},
			want: "dev-60b2",
		},
		{
			name:    "no build metadata",
			stamped: "dev",
			info:    debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want:    "dev",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fromBuildInfo(tc.stamped, &tc.info); got != tc.want {
				t.Fatalf("fromBuildInfo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPseudoVersionRevisionRejectsNonPseudoVersions(t *testing.T) {
	for _, version := range []string{
		"v0.30.0",
		"v0.30.0-rc.1",
		"v0.30.0-20260818160951-not-a-commit",
		"v0.30.0-not-a-timestamp-60b2ab8650b5",
		"v0.30.0-x20260818160951-60b2ab8650b5",
	} {
		t.Run(version, func(t *testing.T) {
			if got := pseudoVersionRevision(version); got != "" {
				t.Fatalf("pseudoVersionRevision() = %q, want empty", got)
			}
		})
	}
}
