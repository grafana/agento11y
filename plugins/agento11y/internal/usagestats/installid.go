package usagestats

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// installIDFileName holds the per-installation id. Deleting the file resets
// it, which drops correlation without disabling reporting.
const installIDFileName = "install-id"

// InstallIDPath returns the file backing the id, or "" when no durable state
// directory is known — an id under TMPDIR resets on reboot and would inflate
// installation counts.
func InstallIDPath() string {
	root, durable := xdg.AppStateRootDurable()
	if !durable {
		return ""
	}
	return filepath.Join(root, installIDFileName)
}

// InstallID returns the per-installation id and whether it is durable.
//
// It identifies an installation, not a person: a fresh UUID with nothing
// derived from hardware, account, hostname, or user. persisted=false means a
// throwaway id, which the receiver must exclude from installation counts.
func InstallID() (id string, persisted bool) {
	return installID(InstallIDPath())
}

func installID(path string) (string, bool) {
	if path == "" {
		return uuid.NewString(), false
	}
	if existing, ok := readInstallID(path); ok {
		return existing, true
	}
	fresh := uuid.NewString()
	if err := writeInstallID(path, fresh); err != nil {
		// Report the throwaway id rather than dropping the event.
		return fresh, false
	}
	return fresh, true
}

// readInstallID returns a stored id, or ok=false when the file is missing,
// unreadable, or not a UUID.
//
// Validated rather than trusted: a truncated file would otherwise become a
// correlator shared by every install with the same corruption, silently
// merging unrelated machines into one.
func readInstallID(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	candidate := strings.TrimSpace(string(raw))
	if _, err := uuid.Parse(candidate); err != nil {
		return "", false
	}
	return candidate, true
}

// writeInstallID stores id owner-only: not a secret, but the one correlator
// in the event.
func writeInstallID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id+"\n"), 0o600)
}
