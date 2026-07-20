// Package cli wires the sigil binary's logger and panic recovery. Logging
// is gated on a debug env key so hooks default to silent; failures to open
// the log file fall back to /dev/null because hooks must not surface
// anything to stderr/stdout that the agent might misinterpret.
package cli

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// InitLogger returns a logger that writes to the per-app log file when the
// branded DEBUG family (AGENTO11Y_DEBUG, SIGIL_DEBUG fallback) is truthy,
// and /dev/null otherwise.
//
// agentName is woven into the line prefix (`<app>[<agent>]: `) so log
// entries from concurrently-running agents stay distinguishable in the
// shared log file. Pass "" to omit the agent tag.
//
// The log file lives at xdg.LogFilePath(appName); the directory is created
// if missing. Any open failure falls back silently to io.Discard.
func InitLogger(appName, agentName string) *log.Logger {
	prefix := appName + ": "
	if agentName != "" {
		prefix = appName + "[" + agentName + "]: "
	}
	logger := log.New(io.Discard, prefix, log.Ltime)
	if !envconfig.ParseBool(envconfig.Getenv("DEBUG")) {
		return logger
	}
	path := xdg.LogFilePath(appName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return logger
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return logger
	}
	return log.New(f, prefix, log.Ldate|log.Ltime|log.Lmicroseconds)
}

// RecoverAndLog catches a panic in a deferred call and logs it. The
// process always exits 0 — hooks must never crash their agent.
func RecoverAndLog(logger *log.Logger) {
	if r := recover(); r != nil {
		if logger != nil {
			logger.Printf("dispatch: panic: %v", r)
		}
	}
}
