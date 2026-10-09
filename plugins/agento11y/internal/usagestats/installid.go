package usagestats

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// installIDFileName holds the random per-installation id. Deleting the file
// resets the id, which is the documented way to opt out of correlation without
// opting out of reporting.
const installIDFileName = "install-id"

// InstallIDPath returns the file backing the installation id, or "" when no
// durable state directory is known. See xdg.AppStateRootDurable: a path under
// the OS temp directory is not durable, and an id stored there would reset on
// reboot and inflate installation counts.
func InstallIDPath() string {
	root, durable := xdg.AppStateRootDurable()
	if !durable {
		return ""
	}
	return filepath.Join(root, installIDFileName)
}

// InstallID returns the random per-installation id and whether it came from,
// or was written to, durable storage.
//
// It identifies an installation of agento11y, not a person: a fresh UUID with
// nothing derived from the hardware, the account, the hostname, or the user.
//
// persisted=false means this invocation used a throwaway id — no durable state
// directory, or a read or write that failed. The receiver needs to tell those
// apart from real installations, because otherwise a machine with an
// unwritable state directory counts as a new installation on every single
// invocation and swamps the installation count.
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
		// Report the throwaway id rather than failing: usage statistics must
		// never affect the command's outcome, and an id the receiver knows is
		// throwaway is more useful than no event.
		return fresh, false
	}
	return fresh, true
}

// readInstallID returns a stored id, or ok=false when the file is missing,
// unreadable, or does not hold a UUID.
//
// The content is validated rather than trusted. A truncated or hand-edited
// file would otherwise become a permanent correlator shared by every
// installation that suffered the same corruption, which is worse than a fresh
// id: it silently merges unrelated machines into one apparent installation.
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

// writeInstallID stores id with owner-only permissions. The id is not a
// secret, but it is the one correlator in the event, so it is not left
// world-readable either.
func writeInstallID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id+"\n"), 0o600)
}
