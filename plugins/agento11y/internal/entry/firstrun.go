package entry

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats/capture"
)

const usageNoticeRevision = "4"

// showUsageNotice prints once per revision before the first enabled report.
func showUsageNotice(w io.Writer) {
	if capture.Snapshot().Suppressed || usagestats.ResolveMode(usageStatsShell, usageStatsConfigValue) != usagestats.ModeEnabled {
		return
	}
	path := usagestats.InstallIDPath()
	if path != "" {
		path = filepath.Join(filepath.Dir(path), "usage-stats-notice")
	}
	if path != "" {
		if revision, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(revision)) == usageNoticeRevision {
			return
		}
	}
	if _, err := io.WriteString(w, "The agento11y CLI sends anonymous usage statistics about its own commands, never operations performed by a coding agent. "+
		"To turn reporting off, set AGENTO11Y_ANONYMOUS_USAGE_STATS=disabled "+
		"or DO_NOT_TRACK=1 in your shell.\n"); err != nil {
		return
	}
	if path != "" && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		file, err := os.CreateTemp(filepath.Dir(path), ".usage-stats-notice-*")
		if err != nil {
			return
		}
		defer os.Remove(file.Name())
		if _, err := file.WriteString(usageNoticeRevision + "\n"); err != nil {
			_ = file.Close()
			return
		}
		if file.Close() == nil {
			_ = os.Rename(file.Name(), path)
		}
	}
}
