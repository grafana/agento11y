// Package npmspec reads the npm specs and package.json files the agent
// launchers find in host config, where a plugin entry is either a package
// spec or a path to a checkout.
package npmspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Name returns the package name portion of an npm spec, stripping the
// trailing `@<version>` segment if present. Scoped packages start with
// `@scope/...`; the leading `@` (index 0) is part of the name, not a version
// separator, so only the LAST `@` after index 0 counts.
func Name(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return spec
	}
	return spec[:at]
}

// Version returns the pinned version of an npm spec, e.g. "0.6.0" from
// "@grafana/agento11y-opencode@0.6.0". A dist tag is returned as-is, and a
// spec with no version yields "". Callers strip any `npm:` prefix first.
func Version(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return ""
	}
	return spec[at+1:]
}

// ReadPackageJSON returns the name and version the package.json in dir
// declares. Any IO or parse failure means they cannot be confirmed — ok is
// false rather than an error, since these files belong to the host agent or to
// a checkout we do not own. A package.json with no `version` reports an empty
// version with ok true. A dir that is not a directory reports ok false.
func ReadPackageJSON(dir string) (name, version string, ok bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", "", false
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", "", false
	}
	return pkg.Name, pkg.Version, true
}
