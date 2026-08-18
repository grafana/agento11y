// Package buildversion resolves the version reported by the agento11y binary.
package buildversion

import (
	"runtime/debug"
	"strings"
)

const (
	developmentVersion  = "dev"
	shortRevisionLength = 7
)

// Resolve keeps a release version supplied through ldflags. For unstamped
// builds, it reads the module version or Git revision that Go records in the
// binary.
func Resolve(stamped string) string {
	if stamped != developmentVersion {
		return stamped
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return stamped
	}
	return fromBuildInfo(stamped, info)
}

func fromBuildInfo(stamped string, info *debug.BuildInfo) string {
	if stamped != developmentVersion {
		return stamped
	}

	if moduleVersion := info.Main.Version; moduleVersion != "" && moduleVersion != "(devel)" {
		if revision := pseudoVersionRevision(moduleVersion); revision != "" {
			return formatDevelopmentVersion(revision, false)
		}
		return moduleVersion
	}

	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return stamped
	}
	return formatDevelopmentVersion(revision, modified)
}

func formatDevelopmentVersion(revision string, modified bool) string {
	revision = revision[:min(len(revision), shortRevisionLength)]
	version := developmentVersion + "-" + revision
	if modified {
		version += "-dirty"
	}
	return version
}

// pseudoVersionRevision extracts the revision from a Go pseudo-version. A
// pseudo-version ends in a 14-digit UTC timestamp and a 12-character revision.
func pseudoVersionRevision(version string) string {
	version = strings.TrimSuffix(version, "+incompatible")
	separator := strings.LastIndexByte(version, '-')
	if separator < 0 {
		return ""
	}

	revision := version[separator+1:]
	if len(revision) != 12 || strings.Trim(revision, "0123456789abcdef") != "" {
		return ""
	}

	prefix := version[:separator]
	if len(prefix) < 14 {
		return ""
	}
	timestamp := prefix[len(prefix)-14:]
	if strings.Trim(timestamp, "0123456789") != "" {
		return ""
	}
	if timestampStart := len(prefix) - len(timestamp); timestampStart > 0 {
		separator := prefix[timestampStart-1]
		if separator != '-' && separator != '.' {
			return ""
		}
	}
	return revision
}
