// Package updatecheck rate-limits launcher plugin refreshes.
package updatecheck

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

var (
	stateRoot = xdg.AppStateRoot
	now       = time.Now
)

// ShouldRun reports whether agent's refresh should run.
func ShouldRun(agent string, ttl time.Duration, binaryVersion string) bool {
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
	if strings.TrimSpace(string(data)) != normalizeVersion(binaryVersion) {
		return true
	}
	return now().Sub(info.ModTime()) >= ttl
}

// Record marks agent's refresh as attempted. Errors are ignored so update
// bookkeeping cannot block launching the wrapped CLI.
func Record(agent, binaryVersion string) {
	path := stampPath(agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(normalizeVersion(binaryVersion)), 0o600); err != nil {
		return
	}
	t := now()
	_ = os.Chtimes(path, t, t)
}

// Disabled reports whether AGENTO11Y_AUTO_UPDATE (SIGIL_ fallback) opts out
// of periodic refreshes.
func Disabled() bool {
	switch strings.ToLower(envconfig.Getenv("AUTO_UPDATE")) {
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
