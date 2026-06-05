package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Status describes the running local receiver daemon.
type Status struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Endpoint  string `json:"endpoint"`
	StartedAt string `json:"started_at"`
}

// DefaultPort is the first port the daemon tries when no explicit
// port is requested. Picked to avoid clashes with the usual dev-tool
// crowd (3000/5000/8000/8080/9090/4317-4318/11434/…) while staying
// memorable. Bumps upward by 1 on conflict, see listenLocal.
const DefaultPort = 8765

// maxPortBumps caps the linear probe from DefaultPort upwards when
// the preferred port is taken. Beyond this we give up rather than
// scanning the whole ephemeral range — something is wrong if 32
// consecutive ports are all bound.
const maxPortBumps = 32

// listenLocal returns a listener on 127.0.0.1 at the preferred port,
// or at the next free port up to maxPortBumps slots above it. When
// preferred <= 0 the kernel picks any free port (legacy behaviour
// retained so tests/callers can still ask for ephemeral binding).
func listenLocal(preferred int) (net.Listener, error) {
	if preferred <= 0 {
		return net.Listen("tcp", "127.0.0.1:0")
	}
	var lastErr error
	for i := 0; i <= maxPortBumps; i++ {
		p := preferred + i
		if p > 65535 {
			break
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return l, nil
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			// Permission denied, IPv6 misconfig, etc. — don't keep
			// trying other ports, the next bind will fail the same way.
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free port in [%d, %d]: %w", preferred, preferred+maxPortBumps, lastErr)
}

// LoadStatus reads the persisted status file under dir. Returns
// (nil, nil) when no status file exists.
func LoadStatus(dir string) (*Status, error) {
	data, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}
	return &s, nil
}

// SaveStatus writes the daemon's status file with 0o600 permissions.
func SaveStatus(dir string, s Status) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, StatusFile)
	return os.WriteFile(path, data, 0o600)
}

// RemoveStatus deletes the status file. Missing-file errors are ignored.
func RemoveStatus(dir string) error {
	err := os.Remove(filepath.Join(dir, StatusFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsRunning probes the recorded daemon. Returns the status when the
// process exists and the HTTP endpoint responds, otherwise nil.
func IsRunning(dir string) (*Status, error) {
	s, err := LoadStatus(dir)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	if !pidAlive(s.PID) {
		return nil, nil
	}
	if !endpointAlive(s.Endpoint) {
		return nil, nil
	}
	return s, nil
}

// EnsureRunning returns the current daemon status, starting it if no
// healthy daemon is recorded. Concurrent callers are serialised by an
// exclusive flock on dir/LockFile so a race between two `--local`
// launches (or `sigil local start`) cannot spawn duplicate daemons.
func EnsureRunning(ctx context.Context, dir string, logger *log.Logger) (*Status, error) {
	if s, err := IsRunning(dir); err != nil {
		return nil, err
	} else if s != nil {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	lock, err := acquireDaemonLock(dir)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	// Re-check inside the lock — a concurrent caller may have just
	// finished spawning a healthy daemon while we were waiting.
	if s, err := IsRunning(dir); err != nil {
		return nil, err
	} else if s != nil {
		return s, nil
	}
	if s, err := LoadStatus(dir); err != nil {
		return nil, err
	} else if s != nil && pidAlive(s.PID) {
		if _, err := Stop(dir); err != nil {
			return nil, err
		}
	}

	// Stale or missing — clean up and start.
	_ = RemoveStatus(dir)
	return startDaemonFn(ctx, dir, logger)
}

// startDaemonFn is a test seam — production points at startDaemon,
// tests can swap in an in-process server.
var startDaemonFn = startDaemon

// processCommandLineFn is a test seam for identifying a recorded daemon PID.
var processCommandLineFn = processCommandLine

// SetStartDaemonForTesting replaces the daemon launcher with fn for the
// remainder of the test binary's life (callers should restore the prior
// value via t.Cleanup).
func SetStartDaemonForTesting(fn func(ctx context.Context, dir string, logger *log.Logger) (*Status, error)) (restore func()) {
	prev := startDaemonFn
	startDaemonFn = fn
	return func() { startDaemonFn = prev }
}

// Stop sends SIGTERM to the recorded daemon after verifying the live PID
// still looks like `sigil local serve`. Returns (false, nil) when no
// daemon is recorded, the recorded process is gone, or the live PID is
// not a sigil daemon. Endpoint health is not required: an alive process
// with a dead /healthz endpoint may be a wedged daemon, and leaving it
// alive lets a later start orphan it. Returns a non-nil error when the
// daemon identity cannot be checked or the daemon does not exit within
// the deadline; the status file is left in place so `status` and
// EnsureRunning still see the lingering daemon.
func Stop(dir string) (bool, error) {
	s, err := LoadStatus(dir)
	if err != nil {
		return false, err
	}
	if s == nil {
		return false, nil
	}
	if !pidAlive(s.PID) {
		_ = RemoveStatus(dir)
		return false, nil
	}
	ok, err := processLooksLikeDaemon(s.PID)
	if err != nil {
		return false, fmt.Errorf("identify recorded daemon pid %d: %w", s.PID, err)
	}
	if !ok {
		_ = RemoveStatus(dir)
		return false, nil
	}
	if err := terminateProcess(s.PID); err != nil {
		return false, err
	}
	// Poll for the daemon to exit. We don't own the child, so we cannot
	// wait(2); 3s is plenty for an HTTP server with no in-flight work.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(s.PID) {
			_ = RemoveStatus(dir)
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, fmt.Errorf("daemon (pid %d) did not exit within 3s", s.PID)
}

// Serve runs the local receiver synchronously. Listens on 127.0.0.1
// at port (or the next free slot above it if it's taken). port == 0
// asks the kernel for any free port — used by tests; in production the
// CLI passes DefaultPort so the daemon URL stays predictable across
// restarts. Writes the status file and blocks until ctx is done or a
// SIGTERM is received.
func Serve(ctx context.Context, dir string, port int, logger *log.Logger) error {
	storage, err := NewStorage(dir)
	if err != nil {
		return err
	}
	listener, err := listenLocal(port)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	srv := NewServer(storage, logger)
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	status := Status{
		PID:       os.Getpid(),
		Port:      actualPort,
		Endpoint:  fmt.Sprintf("http://127.0.0.1:%d", actualPort),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := SaveStatus(dir, status); err != nil {
		_ = listener.Close()
		return fmt.Errorf("save status: %w", err)
	}
	defer func() { _ = RemoveStatus(dir) }()

	if logger != nil {
		logger.Printf("local: serving on %s (dir=%s)", status.Endpoint, dir)
	}

	serveErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		return err
	}
}

func processLooksLikeDaemon(pid int) (bool, error) {
	cmdline, err := processCommandLineFn(pid)
	if err != nil {
		return false, err
	}
	cmdline = strings.TrimSpace(cmdline)
	const daemonArgs = " local serve"
	if !strings.HasSuffix(cmdline, daemonArgs) {
		return false, nil
	}
	exe := strings.TrimSpace(strings.TrimSuffix(cmdline, daemonArgs))
	if exe == "" {
		return false, nil
	}
	return strings.HasPrefix(filepath.Base(exe), "sigil"), nil
}

// endpointAlive returns true when GET <endpoint>/healthz responds within
// 500ms. /healthz is the JSON liveness probe; / now serves the viewer
// HTML and would be wasteful (and ambiguous) to fetch on every check.
func endpointAlive(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/healthz")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
