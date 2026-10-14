package opencode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectHost(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		probeErr error
		want     hostGeneration
		// wantErr is a substring of the *VersionError; "" means supported.
		wantErr      string
		wantReported string
	}{
		// OpenCode 1 prints the bare version (yargs); OpenCode 2 prints
		// "<name> v<version>" (effect/cli).
		{name: "oldest v1", out: "1.3.13\n", want: generationV1},
		{name: "current v1", out: "1.18.29\n", want: generationV1},
		{name: "v1 with a v prefix", out: "v1.4.0", want: generationV1},
		{name: "oldest v2", out: "opencode v2.0.22\n", want: generationV2},
		{name: "later v2", out: "opencode v2.11.3\n", want: generationV2},
		{name: "renamed v2 binary", out: "opencode2 v2.0.29\n", want: generationV2},
		{name: "colored v2", out: "\x1b[1mopencode\x1b[0m \x1b[2mv\x1b[0m\x1b[1m2.0.25\x1b[0m\n", want: generationV2},

		{name: "v1 before 1.3.13", out: "1.3.12\n", wantErr: "OpenCode 1.3.12 is older than 1.3.13", wantReported: "1.3.12"},
		{name: "manual-config-only v1", out: "1.2.16\n", wantErr: "older than 1.3.13", wantReported: "1.2.16"},
		{name: "snapshot zero version", out: "0.0.0\n", wantErr: "OpenCode 0.0.0 is older than 1.3.13"},
		{name: "v2 before 2.0.22", out: "opencode v2.0.21\n", wantErr: "OpenCode 2.0.21 is older than 2.0.22", wantReported: "2.0.21"},
		{name: "future major", out: "opencode v3.0.0\n", wantErr: "OpenCode 3.0.0 is newer than this agento11y release supports", wantReported: "3.0.0"},
		{name: "v2 prerelease", out: "opencode v2.0.30-beta.2\n", wantErr: "OpenCode 2.0.30-beta.2 is not a stable release", wantReported: "2.0.30-beta.2"},
		{name: "v1 snapshot", out: "0.0.0-202601011200\n", wantErr: "is not a stable release"},
		{name: "build metadata", out: "1.18.29+abc\n", wantErr: "is not a stable release"},
		{name: "four components", out: "2.0.25.1\n", wantErr: "is not a stable release"},
		{name: "source build", out: "opencode vlocal\n", wantErr: `printed "opencode vlocal"`},
		{name: "major and minor only", out: "opencode v2.0\n", wantErr: "cannot tell which OpenCode release is installed"},
		{name: "empty output", out: "", wantErr: `printed ""`},
		{name: "probe failure", probeErr: errors.New("exit status 1: boom"), wantErr: "`opencode --version` failed: exit status 1: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withVersionOutput(t, tc.out, tc.probeErr)
			got, err := detectHost(context.Background(), "/usr/local/bin/opencode")
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}
			var versionErr *VersionError
			require.ErrorAs(t, err, &versionErr)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, generationUnknown, got)
			if tc.wantReported != "" {
				assert.Equal(t, tc.wantReported, versionErr.Reported)
			}
			// Every refusal names what is supported or how to get past it.
			assert.Contains(t, err.Error(), "agento11y")
		})
	}
}

// The probe is bounded: a hung `opencode --version` must not hold the launch.
func TestDetectHost_BoundsTheProbe(t *testing.T) {
	prev := versionOutput
	t.Cleanup(func() { versionOutput = prev })
	var deadline time.Time
	versionOutput = func(ctx context.Context, bin string) ([]byte, error) {
		assert.Equal(t, "/usr/local/bin/opencode", bin)
		var ok bool
		deadline, ok = ctx.Deadline()
		require.True(t, ok, "the probe context must carry a deadline")
		return []byte("1.18.29\n"), nil
	}
	start := time.Now()
	_, err := detectHost(context.Background(), "/usr/local/bin/opencode")
	require.NoError(t, err)
	assert.WithinDuration(t, start.Add(versionProbeTimeout), deadline, time.Second)
}
