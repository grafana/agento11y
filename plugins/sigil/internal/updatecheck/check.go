// Package updatecheck rate-limits launcher plugin refreshes.
package updatecheck

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/xdg"
)

const appName = "sigil"

var (
	stateRoot = func() string { return xdg.StateRoot(appName) }
	now       = time.Now
)

// ShouldRun reports whether agent's refresh should run.
func ShouldRun(agent string, ttl time.Duration, sigilVersion string) bool {
	if Disabled() {
		return false
	}
	if ttl <= 0 {
		return true
	}
	path := stampPath(agent)
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	if strings.TrimSpace(string(data)) != normalizeVersion(sigilVersion) {
		return true
	}
	return now().Sub(info.ModTime()) >= ttl
}

// Record marks agent's refresh as attempted. Errors are ignored so update
// bookkeeping cannot block launching the wrapped CLI.
func Record(agent, sigilVersion string) {
	path := stampPath(agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(normalizeVersion(sigilVersion)), 0o600); err != nil {
		return
	}
	t := now()
	_ = os.Chtimes(path, t, t)
}

// Disabled reports whether SIGIL_AUTO_UPDATE opts out of periodic refreshes.
func Disabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SIGIL_AUTO_UPDATE"))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

func stampPath(agent string) string {
	return filepath.Join(stateRoot(), "update-checks", agent+".stamp")
}

func normalizeVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "dev"
	}
	return v
}
