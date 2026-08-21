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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
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
		if !addrInUse(err) {
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
	data, err := readShared(filepath.Join(dir, StatusFile))
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
//
// The bytes go to a temp file in dir and are renamed into place, so a reader
// never sees a half-written file. Writing in place would truncate first, and
// LoadStatus takes no lock: EnsureRunning probes the status before acquiring
// the daemon lock, and `local status` reads it at any time, so those readers
// would fail on an empty file while a daemon records itself.
func SaveStatus(dir string, s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, StatusFile, data)
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
// launches (or `agento11y local start`) cannot spawn duplicate daemons.
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

// startDaemon launches `agento11y local serve` as a detached child process.
// The parent waits for the child to write its status file, then returns
// the recorded endpoint. The child detaches through daemonSysProcAttr so it
// survives the parent exiting.
func startDaemon(ctx context.Context, dir string, logger *log.Logger) (*Status, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve agento11y binary: %w", err)
	}
	// os.Executable() is the test binary when a test reaches this function. A
	// test binary ignores the "local serve" arguments and runs its whole suite
	// again, and that suite starts another daemon, so the processes multiply.
	// Tests must install a stub with SetStartDaemonForTesting.
	if looksLikeTestBinary(bin) {
		return nil, fmt.Errorf("refusing to start the daemon from test binary %s: stub it with local.SetStartDaemonForTesting", filepath.Base(bin))
	}

	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cmd := exec.Command(bin, "local", "serve")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = daemonSysProcAttr()
	// Inherit env so SIGIL_DEBUG and XDG_* flow through.
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	// Close the log handle in this process; the child has its own copy.
	_ = logFile.Close()

	// Wait up to ~5s for the child to write its status file.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		}
		if s, err := IsRunning(dir); err == nil && s != nil {
			if logger != nil {
				logger.Printf("local: daemon started pid=%d port=%d", s.PID, s.Port)
			}
			return s, nil
		}
		// Check the child exited prematurely so we don't block forever.
		if childExited(cmd.Process.Pid) {
			body, _ := os.ReadFile(logPath)
			return nil, fmt.Errorf("daemon exited prematurely: %s", strings.TrimSpace(string(body)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("daemon did not become ready within 5s")
}

// SetStartDaemonForTesting replaces the daemon launcher with fn for the
// remainder of the test binary's life (callers should restore the prior
// value via t.Cleanup).
func SetStartDaemonForTesting(fn func(ctx context.Context, dir string, logger *log.Logger) (*Status, error)) (restore func()) {
	prev := startDaemonFn
	startDaemonFn = fn
	return func() { startDaemonFn = prev }
}

// Stop asks the recorded daemon to exit after verifying its identity. It
// returns (false, nil) if no matching process is running. A dead health
// endpoint does not prevent termination because the daemon may be wedged.
// Identity and timeout errors leave the status file in place.
func Stop(dir string) (bool, error) {
	s, err := LoadStatus(dir)
	if err != nil {
		return false, err
	}
	if s == nil {
		return false, nil
	}
	proc, err := openDaemonProcess(*s)
	if err != nil {
		return false, fmt.Errorf("identify recorded daemon pid %d: %w", s.PID, err)
	}
	if proc == nil {
		_ = RemoveStatus(dir)
		return false, nil
	}
	defer proc.close()

	deadline := time.Now().Add(3 * time.Second)
	shutdownErr := requestShutdown(s.Endpoint)
	if shutdownErr == nil {
		// Give the HTTP server time to drain before falling back to the
		// platform process termination call.
		graceDeadline := time.Now().Add(time.Second)
		for time.Now().Before(graceDeadline) {
			if !proc.alive() {
				_ = RemoveStatus(dir)
				return true, nil
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !proc.alive() {
		_ = RemoveStatus(dir)
		return true, nil
	}
	if err := proc.terminate(); err != nil {
		if shutdownErr != nil {
			return false, fmt.Errorf("request graceful shutdown: %v; terminate process: %w", shutdownErr, err)
		}
		return false, err
	}
	// Poll for the daemon to exit. The deadline covers both the graceful
	// request and the platform termination fallback.
	for time.Now().Before(deadline) {
		if !proc.alive() {
			_ = RemoveStatus(dir)
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, fmt.Errorf("daemon (pid %d) did not exit within 3s", s.PID)
}

func requestShutdown(endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("no daemon endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/api/v1/shutdown", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST %s returned HTTP %d", req.URL, resp.StatusCode)
	}
	return nil
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
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	srv := NewServer(storage, logger, dotenv.FilePath())
	srv.SetShutdown(stopServe)
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
	// A history import exports through this same address, so the server has to
	// know it. The port is only settled once the listener exists.
	srv.SetLocalEndpoint(status.Endpoint)
	// The summary cache starts empty, so without this the viewer decodes the
	// whole store one request at a time after a restart.
	srv.WarmSummariesOnFirstRead(serveCtx)
	if err := SaveStatus(dir, status); err != nil {
		_ = listener.Close()
		return fmt.Errorf("save status: %w", err)
	}
	defer func() { _ = RemoveStatus(dir) }()

	if logger != nil {
		logger.Printf("local: serving on %s (dir=%s)", status.Endpoint, dir)
	}

	// The pass starts only after the status file exists: the process that
	// spawned this daemon waits 5 seconds for that file, and stamping a large
	// store takes longer. On the way out the pass stops at the next file, so
	// waiting for it costs one file scan.
	repairCtx, cancelRepair := context.WithCancel(serveCtx)
	repairDone := make(chan struct{})
	go func() {
		defer close(repairDone)
		repairStoreOnStartup(repairCtx, storage, srv.hub)
	}()
	defer func() {
		cancelRepair()
		<-repairDone
	}()

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
	case <-serveCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// Close the event hub first so open SSE streams return immediately
		// instead of holding the shutdown deadline open.
		srv.Close()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		return err
	}
}

// looksLikeTestBinary reports whether path is a Go test binary. `go test` names
// them "<pkg>.test", and "<pkg>.test.exe" on Windows.
func looksLikeTestBinary(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(filepath.Base(path), ".exe"), ".test")
}

// imageIsDaemon reports whether an executable path names an agento11y
// binary. Release builds and `go build` name the binary "agento11y" (or the
// legacy "sigil"); `go run` compiles it to "main" in the build cache. Accept
// all three, otherwise a daemon started under the other name or from a dev
// build fails this check, Stop deletes the status file, and the daemon is
// orphaned on its port.
//
// Windows paths are case-insensitive and end in ".exe", so the comparison
// lowercases the name and removes that extension on every platform.
func imageIsDaemon(exe string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(exe)), ".exe")
	return strings.HasPrefix(base, "agento11y") || strings.HasPrefix(base, "sigil") || base == "main"
}

// ForwardPosture is what a running daemon would send to Cloud right now, for
// callers outside the viewer. The launcher prints a privacy claim before
// handing the terminal to the agent, and only the daemon knows the resolved
// answer: it reads config.env ahead of its own environment, so the launcher's
// process environment can disagree with it.
type ForwardPosture struct {
	// Enabled reports whether captured sessions are forwarded at all.
	Enabled bool
	// Mode is the content level of the forwarded copy (forwardMode*).
	Mode string
	// Hooks reports whether guard evaluation is relayed to Cloud, which sends
	// the content being evaluated regardless of Mode. See handleHookEvaluate.
	Hooks bool
}

// FetchForwardPosture asks a running daemon what it would forward. A non-nil
// error means the daemon could not be reached or did not answer in time, in
// which case the caller must not claim any particular posture, and should say
// why: the hedged wording it falls back to is otherwise indistinguishable from
// a daemon that reported "nothing is forwarded".
func FetchForwardPosture(ctx context.Context, endpoint string) (ForwardPosture, error) {
	if strings.TrimSpace(endpoint) == "" {
		return ForwardPosture{}, errors.New("no daemon endpoint")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/api/v1/config", nil)
	if err != nil {
		return ForwardPosture{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ForwardPosture{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return ForwardPosture{}, fmt.Errorf("GET %s returned HTTP %d", req.URL, resp.StatusCode)
	}
	var body configResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return ForwardPosture{}, fmt.Errorf("decode config from %s: %w", req.URL, err)
	}
	return ForwardPosture{
		Enabled: body.ForwardStatus.Enabled,
		Mode:    body.ForwardStatus.Mode,
		Hooks:   body.ForwardStatus.Hooks,
	}, nil
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
