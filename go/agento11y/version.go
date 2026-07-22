package agento11y

import (
	"runtime/debug"
	"strings"
)

// sdkModulePath is the module path consumers depend on; its version in the
// consumer's module graph is the published SDK version.
const sdkModulePath = "github.com/grafana/agento11y/go"

// versionFallback is reported when the SDK version cannot be resolved from
// build information, e.g. in-repo tests, replaced modules, or (devel) builds.
const versionFallback = "0.0.0+unknown"

// Version is the SDK version stamped into the default generation-export
// User-Agent (see UserAgent). Resolved from the consumer's module graph via
// runtime/debug.ReadBuildInfo; "0.0.0+unknown" when dependency metadata is
// unavailable.
var Version = sdkVersion()

func sdkVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v, ok := versionFromBuildInfo(info); ok {
			return v
		}
	}
	return versionFallback
}

// versionFromBuildInfo returns the SDK module's version from build
// information, without the "v" prefix. It reports false when the dependency
// is missing, replaced, or has no released version.
func versionFromBuildInfo(info *debug.BuildInfo) (string, bool) {
	for _, dep := range info.Deps {
		if dep.Path != sdkModulePath {
			continue
		}
		if dep.Replace != nil || dep.Version == "" || dep.Version == "(devel)" {
			return "", false
		}
		return strings.TrimPrefix(dep.Version, "v"), true
	}
	return "", false
}
