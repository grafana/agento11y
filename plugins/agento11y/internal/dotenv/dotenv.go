// Package dotenv loads KEY=value pairs from $XDG_CONFIG_HOME/<app>/config.env
// and writes them into the process environment where the OS env is empty.
//
// This lets hooks pick up branded credentials when the agent runs them under
// a stripped environment (e.g. Cursor's hook runtime, Codex's headless mode).
package dotenv

import (
	"bufio"
	"log"
	"os"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// FilePath returns the dotenv config path for an app:
// $XDG_CONFIG_HOME/<appName>/config.env (with sensible fallbacks).
func FilePath(appName string) string {
	return xdg.ConfigFilePath(appName, "config.env")
}

// HasCredentials reports whether the branded credentials are populated in the
// OS env under either spelling. Call after ApplyEnv so dotenv-supplied values
// are visible.
func HasCredentials() bool {
	return envconfig.Getenv("ENDPOINT") != "" &&
		envconfig.Getenv("AUTH_TENANT_ID") != "" &&
		envconfig.Getenv("AUTH_TOKEN") != ""
}

// ApplyEnv loads the dotenv file for appName and merges it into the process
// environment. Supported alias families resolve source-first, spelling-second:
//
//	shell AGENTO11Y_* > shell SIGIL_* > file AGENTO11Y_* > file SIGIL_*
//
// so a shell export always beats a config.env entry even across spellings.
// The winning value is materialized under BOTH names so downstream readers and
// child processes (including old binaries that only read SIGIL_*) observe one
// consistent value. Blank or whitespace-only values count as unset at every
// step. Keys outside the alias registry keep the old exact-key semantics:
// the file value is applied only where the OS env is empty. Returns the
// parsed dotenv map for callers that need to introspect (tests).
func ApplyEnv(appName string, logger *log.Logger) map[string]string {
	fileEnv := LoadDotenv(FilePath(appName), logger)

	aliasKeys := map[string]bool{}
	for _, suffix := range envconfig.AliasSuffixes {
		preferred := envconfig.PreferredKey(suffix)
		legacy := envconfig.LegacyKey(suffix)
		aliasKeys[preferred] = true
		aliasKeys[legacy] = true

		winner, found := "", false
		for _, candidate := range []string{
			strings.TrimSpace(os.Getenv(preferred)),
			strings.TrimSpace(os.Getenv(legacy)),
			strings.TrimSpace(fileEnv[preferred]),
			strings.TrimSpace(fileEnv[legacy]),
		} {
			if candidate != "" {
				winner, found = candidate, true
				break
			}
		}
		if found {
			envconfig.SetBothEnv(suffix, winner)
		}
	}

	for k, v := range fileEnv {
		if aliasKeys[k] {
			continue
		}
		if strings.TrimSpace(os.Getenv(k)) != "" {
			continue
		}
		_ = os.Setenv(k, v)
	}
	return fileEnv
}

// LoadDotenv parses the file at path. Missing files return an empty map
// silently; other errors are logged. Only keys matching AllowedDotenvKey
// are honoured — defense-in-depth so a stray entry can't override unrelated
// process state.
//
// Format:
//   - `KEY=value` one pair per line
//   - `# comment` lines and trailing ` # comment` on unquoted values
//   - optional single- or double-quoted values
//   - optional leading `export ` prefix
func LoadDotenv(path string, logger *log.Logger) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) && logger != nil {
			logger.Printf("dotenv: read %s: %v", path, err)
		}
		return out
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(line[len("export "):])
		}
		key, rest, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || !AllowedDotenvKey(key) {
			continue
		}
		value := parseDotenvValue(strings.TrimSpace(rest))
		if value != "" {
			out[key] = value
		}
	}
	if err := scanner.Err(); err != nil && logger != nil {
		logger.Printf("dotenv: scan %s: %v", path, err)
	}
	return out
}

// AllowedDotenvKey limits which keys the dotenv loader will copy into the
// process environment. AGENTO11Y_*, SIGIL_*, and a small set of OTEL_* vars
// are recognised.
func AllowedDotenvKey(key string) bool {
	if strings.HasPrefix(key, "AGENTO11Y_") || strings.HasPrefix(key, "SIGIL_") {
		return true
	}
	switch key {
	case "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_SERVICE_NAME":
		return true
	default:
		return false
	}
}

// parseDotenvValue strips surrounding quotes (if both ends match) and a
// trailing ` # comment` from an unquoted value. Quoted values keep their
// inner whitespace and any `#` characters verbatim — including on a quoted
// value followed by an inline comment like `KEY="x" # comment`.
func parseDotenvValue(v string) string {
	if len(v) >= 2 {
		first := v[0]
		if first == '"' || first == '\'' {
			if end := strings.IndexByte(v[1:], first); end >= 0 {
				return v[1 : 1+end]
			}
		}
	}
	if hashIdx := strings.Index(v, " #"); hashIdx >= 0 {
		v = strings.TrimRight(v[:hashIdx], " \t")
	}
	return v
}
