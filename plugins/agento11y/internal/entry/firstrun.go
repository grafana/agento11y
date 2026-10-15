package entry

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats/capture"
)

const usageNoticeRevision = "1"
const usageNoticeURL = "https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/instrument-coding-agents/"

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
	if _, err := fmt.Fprintf(w, "agento11y sends anonymous CLI usage statistics to Grafana by default: the command used, its result, CLI and system details, and a random persistent installation ID. It does not send user or account IDs, argument values, or session content.\nTo turn reporting off, set AGENTO11Y_ANONYMOUS_USAGE_STATS=disabled in your shell or ~/.config/agento11y/config.env, or DO_NOT_TRACK=1 in your shell.\nLearn more: %s\n", usageNoticeURL); err != nil {
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
