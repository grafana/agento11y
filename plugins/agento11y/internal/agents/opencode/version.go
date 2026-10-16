package opencode

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
)

// hostGeneration is the OpenCode release line. See the package doc for how
// the lines differ.
type hostGeneration int

const (
	// generationUnknown means no version was read. Only Status accepts it,
	// because doctor reports install state without OpenCode on PATH.
	generationUnknown hostGeneration = iota
	generationV1
	generationV2
)

// hostVersion is a stable OpenCode release number.
type hostVersion struct{ major, minor, patch int }

func (v hostVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

func (v hostVersion) less(o hostVersion) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

var (
	// minV1 is the oldest 1.x release the launcher drives: the oldest tagged
	// release whose loader accepts the package's dual v1/v2 server export.
	// Older releases (1.2.16) still load the package's root export when it is
	// added to the config by hand, but agento11y opencode does not install into
	// them.
	minV1 = hostVersion{major: 1, minor: 3, patch: 13}
	// minV2 is the oldest 2.x release the launcher drives (the release in
	// issue #823). The launcher depends on its `opencode plugin add` and on
	// --standalone for the root, run, and mini commands.
	minV2 = hostVersion{major: 2, minor: 0, patch: 22}
)

const supportedVersions = "OpenCode 1.3.13 or a later 1.x release, or 2.0.22 or a later 2.x release"

// versionProbeTimeout caps `opencode --version`. OpenCode 2 is a compiled Bun
// binary whose cold start can take a few seconds, notably on Windows, but the
// probe sits in front of a session the user is waiting for.
const versionProbeTimeout = 10 * time.Second

// versionOutput runs `opencode --version`. Test seam.
var versionOutput = func(ctx context.Context, bin string) ([]byte, error) {
	return launcher.Output(ctx, bin, "--version")
}

// VersionError reports an OpenCode release the launcher refuses to drive.
// Prepare, Install, and Status return it before they change anything.
type VersionError struct {
	// Reported is the version text OpenCode printed, or "" when the probe
	// itself failed.
	Reported string
	reason   string
}

func (e *VersionError) Error() string { return e.reason }

var (
	// ansiEscape strips color codes. OpenCode 2 renders `--version` through
	// effect/cli, whose formatter can color "<name> v<version>".
	ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// versionToken matches one whitespace-separated token of `--version`
	// output: "1.3.13" from OpenCode 1 (yargs), "v2.0.25" from OpenCode 2. A
	// non-empty fourth group is a prerelease, build, or extra component.
	versionToken = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(.*)$`)
)

// detectHost returns the release line of bin, or a *VersionError for any
// release outside the supported ranges.
func detectHost(ctx context.Context, bin string) (hostGeneration, error) {
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	out, err := versionOutput(ctx, bin)
	if err != nil {
		return generationUnknown, &VersionError{reason: fmt.Sprintf(
			"cannot tell which OpenCode release is installed: `opencode --version` failed: %v. agento11y opencode supports %s",
			err, supportedVersions)}
	}
	v, err := parseHostVersion(string(out))
	if err != nil {
		return generationUnknown, err
	}
	return classifyHostVersion(v)
}

// parseHostVersion reads the release number from `opencode --version` output.
// Only a plain MAJOR.MINOR.PATCH is a release; anything after it (a
// prerelease or build suffix) is rejected, as is output without a number,
// such as the "local" version of a source build.
func parseHostVersion(out string) (hostVersion, error) {
	clean := strings.TrimSpace(ansiEscape.ReplaceAllString(out, ""))
	for field := range strings.FieldsSeq(clean) {
		m := versionToken.FindStringSubmatch(field)
		if m == nil {
			continue
		}
		reported := strings.TrimPrefix(field, "v")
		if m[4] != "" {
			return hostVersion{}, &VersionError{Reported: reported, reason: fmt.Sprintf(
				"OpenCode %s is not a stable release. agento11y opencode supports %s; run opencode directly to use this build without agento11y capture",
				reported, supportedVersions)}
		}
		var parts [3]int
		for i := range parts {
			n, err := strconv.Atoi(m[i+1])
			if err != nil {
				return hostVersion{}, unreadableVersion(clean)
			}
			parts[i] = n
		}
		return hostVersion{major: parts[0], minor: parts[1], patch: parts[2]}, nil
	}
	return hostVersion{}, unreadableVersion(clean)
}

func unreadableVersion(out string) error {
	return &VersionError{Reported: out, reason: fmt.Sprintf(
		"cannot tell which OpenCode release is installed: `opencode --version` printed %q. agento11y opencode supports %s",
		out, supportedVersions)}
}

func classifyHostVersion(v hostVersion) (hostGeneration, error) {
	switch {
	case v.major == 1 && !v.less(minV1):
		return generationV1, nil
	case v.major == 2 && !v.less(minV2):
		return generationV2, nil
	case v.major > 2:
		return generationUnknown, &VersionError{Reported: v.String(), reason: fmt.Sprintf(
			"OpenCode %s is newer than this agento11y release supports (%s). Upgrade agento11y, or run opencode directly without agento11y capture",
			v, supportedVersions)}
	case v.major == 2:
		return generationUnknown, &VersionError{Reported: v.String(), reason: fmt.Sprintf(
			"OpenCode %s is older than 2.0.22, the first 2.x release agento11y opencode supports. Upgrade OpenCode, or run opencode directly without agento11y capture",
			v)}
	default:
		return generationUnknown, &VersionError{Reported: v.String(), reason: fmt.Sprintf(
			"OpenCode %s is older than 1.3.13. agento11y opencode supports %s; upgrade OpenCode, or run opencode directly without agento11y capture",
			v, supportedVersions)}
	}
}
