package usagestats

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// installIDFileName holds the per-installation id. Deleting the file resets
// it, which drops correlation without disabling reporting.
const installIDFileName = "install-id"

// InstallIDPath returns the file backing the id, or "" when no durable state
// directory is known — an id under TMPDIR would reset on reboot.
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
// throwaway id that will differ on the next run.
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
	err := createInstallID(path, fresh)
	switch {
	case err == nil:
		return fresh, true
	case !errors.Is(err, fs.ErrExist):
		// Report a throwaway id rather than dropping the event.
		return fresh, false
	}

	// The file appeared between the read and the create, so another process is
	// initialising it concurrently. Adopt its id: two first runs that each
	// reported persisted with a different id would look like two installations.
	if existing, ok := awaitInstallID(path); ok {
		return existing, true
	}

	// The file exists but never became a valid id, so it is corrupt rather
	// than contended. Replace it, or a damaged file would make every future
	// run report a throwaway id.
	if err := replaceInstallID(path, fresh); err != nil {
		return fresh, false
	}
	return fresh, true
}

// readInstallID returns a stored id, or ok=false when the file is missing,
// unreadable, or not a UUID. Validated rather than trusted: a truncated file
// would otherwise be shared by every install with the same corruption.
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

// createInstallID writes id to path and fails with fs.ErrExist if the file is
// already there. O_EXCL makes the claim atomic, so concurrent first runs
// cannot each conclude that they created the file.
func createInstallID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Owner-only: not a secret, but the one correlator in the event.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(id + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// awaitInstallID re-reads a file another process is in the middle of creating.
// The winner claims the path before writing to it, so for a moment the loser
// can see it empty.
func awaitInstallID(path string) (string, bool) {
	for range 50 {
		if id, ok := readInstallID(path); ok {
			return id, true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", false
}

// replaceInstallID overwrites a corrupt file through a temporary file and a
// rename, so a reader sees either the old contents or the new ones.
func replaceInstallID(path, id string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".install-id-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()

	if _, err := temp.WriteString(id + "\n"); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
