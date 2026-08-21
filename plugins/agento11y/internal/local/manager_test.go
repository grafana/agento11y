package local

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStop_RemovesStatusWhenProcessGone(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveStatus(dir, Status{PID: 0, Port: 1, Endpoint: "http://127.0.0.1:1"}))
	stopped, err := Stop(dir)
	require.NoError(t, err)
	assert.False(t, stopped)
	_, err = os.Stat(filepath.Join(dir, StatusFile))
	assert.True(t, os.IsNotExist(err), "stale status file should be removed")
}

// TestSaveStatus_ConcurrentReadersSeeWholeFile pins down the atomic write.
// LoadStatus takes no lock, so an in-place write truncates the file under
// readers and they fail with "unexpected end of JSON input" - which is how
// TestEnsureRunning_ConcurrentCallersSpawnOnce used to flake.
func TestSaveStatus_ConcurrentReadersSeeWholeFile(t *testing.T) {
	dir := t.TempDir()
	want := Status{
		PID:       os.Getpid(),
		Port:      8765,
		Endpoint:  "http://127.0.0.1:8765",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	require.NoError(t, SaveStatus(dir, want))

	const writes = 500
	writing := make(chan struct{})
	writeErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(writing)
		for range writes {
			if err := SaveStatus(dir, want); err != nil {
				writeErr <- err
				return
			}
		}
	})

	var readErr error
	partial := 0
	for done := false; !done; {
		select {
		case <-writing:
			done = true
		default:
		}
		s, err := LoadStatus(dir)
		switch {
		case err != nil:
			if readErr == nil {
				readErr = err
			}
		case s == nil || *s != want:
			partial++
		}
	}
	wg.Wait()

	select {
	case err := <-writeErr:
		require.NoError(t, err)
	default:
	}
	require.NoError(t, readErr, "LoadStatus read a status file mid-write")
	assert.Zero(t, partial, "LoadStatus returned a status that was never written")
}

const (
	daemonHelperEnv = "AGENTO11Y_TEST_LOCAL_DAEMON_HELPER"
	daemonHelperDir = "AGENTO11Y_TEST_LOCAL_DAEMON_DIR"
	idleHelperEnv   = "AGENTO11Y_TEST_IDLE_HELPER"
)

func TestLocalDaemonHelperProcess(t *testing.T) {
	if os.Getenv(daemonHelperEnv) != "1" {
		return
	}
	if err := Serve(context.Background(), os.Getenv(daemonHelperDir), 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIdleHelperProcess(t *testing.T) {
	if os.Getenv(idleHelperEnv) != "1" {
		return
	}
	time.Sleep(time.Minute)
}

func TestLocalDaemonLifecycle(t *testing.T) {
	dir := t.TempDir()
	helper := copyTestExecutable(t)
	cmd := exec.Command(helper, "-test.run=^TestLocalDaemonHelperProcess$", "local", "serve")
	cmd.Env = append(os.Environ(), daemonHelperEnv+"=1", daemonHelperDir+"="+dir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	trustDaemonHelperProcess(t, cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = cmd.Process.Kill()
		<-done
	})

	var first *Status
	require.Eventually(t, func() bool {
		var err error
		first, err = IsRunning(dir)
		return err == nil && first != nil
	}, 5*time.Second, 20*time.Millisecond, "helper daemon published a healthy endpoint")
	require.Equal(t, cmd.Process.Pid, first.PID)

	reused, err := EnsureRunning(context.Background(), dir, nil)
	require.NoError(t, err)
	require.Equal(t, first, reused)

	started := time.Now()
	stopped, err := Stop(dir)
	require.NoError(t, err)
	require.True(t, stopped)
	require.Less(t, time.Since(started), 3*time.Second)
	select {
	case err := <-done:
		require.NoError(t, err)
		waited = true
	case <-time.After(3 * time.Second):
		t.Fatal("helper daemon process did not exit")
	}
	require.False(t, pidAlive(first.PID))
	_, err = os.Stat(filepath.Join(dir, StatusFile))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func copyTestExecutable(t *testing.T) string {
	t.Helper()
	sourcePath, err := os.Executable()
	require.NoError(t, err)
	source, err := os.Open(sourcePath)
	require.NoError(t, err)
	defer func() { _ = source.Close() }()

	name := "agento11y"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	destinationPath := filepath.Join(t.TempDir(), name)
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	require.NoError(t, copyErr)
	require.NoError(t, closeErr)
	return destinationPath
}

// TestListenLocal covers both halves of the port-fallback contract:
// when the preferred port is free we get exactly that port (no kernel
// random); when it's held we bump to the next free slot. Each row
// discovers a port via net.Listen(":0") so we never assume any
// specific port is free on the test host.
func TestListenLocal(t *testing.T) {
	cases := []struct {
		name           string
		holdDuringCall bool // when true, hold preferred during listenLocal
		wantBumped     bool // when true, returned port must be > preferred
	}{
		{name: "preferred free, returns exactly that port", holdDuringCall: false, wantBumped: false},
		{name: "preferred taken, bumps to next free port", holdDuringCall: true, wantBumped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Discover a port we know is free right now by binding to
			// :0, then either hold it (forcing a bump) or release it
			// (expecting an exact match).
			probe, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			preferred := probe.Addr().(*net.TCPAddr).Port
			if tc.holdDuringCall {
				defer probe.Close()
			} else {
				_ = probe.Close()
			}

			listener, err := listenLocal(preferred)
			if err != nil {
				if !tc.holdDuringCall {
					// Tiny race window between Close and the next Listen.
					t.Skipf("port %d was retaken between probe and bind: %v", preferred, err)
				}
				t.Fatalf("listenLocal: %v", err)
			}
			defer listener.Close()
			got := listener.Addr().(*net.TCPAddr).Port

			switch {
			case tc.wantBumped && got == preferred:
				t.Fatalf("got blocked port %d; should have bumped", got)
			case tc.wantBumped && (got <= preferred || got > preferred+maxPortBumps):
				t.Fatalf("got %d, want in (%d, %d]", got, preferred, preferred+maxPortBumps)
			case !tc.wantBumped && got != preferred:
				t.Fatalf("got %d, want preferred %d", got, preferred)
			}
		})
	}
}

// looksLikeTestBinary is what stops startDaemon from re-executing a test binary
// as the daemon, which would fork the suite into more daemons.
func TestLooksLikeTestBinary(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{path: "/var/folders/x/go-build123/b001/entry.test", want: true},
		{path: "/var/folders/x/go-build123/b001/local.test.exe", want: true},
		{path: "/usr/local/bin/agento11y", want: false},
		{path: "/usr/local/bin/agento11y.exe", want: false},
		// `go run` compiles to "main" in the build cache.
		{path: "/Users/x/Library/Caches/go-build/ab/cd-d/main", want: false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := looksLikeTestBinary(tc.path); got != tc.want {
				t.Errorf("looksLikeTestBinary(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestEnsureRunning_ConcurrentCallersSpawnOnce drives several
// EnsureRunning goroutines at the same empty state dir. A healthy
// daemon is simulated by an httptest server so endpointAlive returns
// true after the first SaveStatus; the file lock makes the callers converge
// on the status that the first caller writes.
func TestEnsureRunning_ConcurrentCallersSpawnOnce(t *testing.T) {
	dir := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	colon := strings.LastIndex(host, ":")
	port, _ := strconv.Atoi(host[colon+1:])

	var spawns atomic.Int32
	restore := SetStartDaemonForTesting(func(_ context.Context, dir string, _ *log.Logger) (*Status, error) {
		spawns.Add(1)
		// Yield a moment so the next goroutine has time to wait on the
		// file lock; without this, the first caller may finish before the
		// others even contend.
		time.Sleep(20 * time.Millisecond)
		s := Status{
			PID:       os.Getpid(),
			Port:      port,
			Endpoint:  ts.URL,
			StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		_ = SaveStatus(dir, s)
		return &s, nil
	})
	defer restore()

	const callers = 8
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			if _, err := EnsureRunning(context.Background(), dir, nil); err != nil {
				t.Errorf("EnsureRunning: %v", err)
			}
		})
	}
	wg.Wait()

	if got := spawns.Load(); got != 1 {
		t.Errorf("spawns = %d, want 1 (file lock should serialise EnsureRunning)", got)
	}
}

// testLogger routes a daemon's diagnostics into the test's output, so a
// failing test reports what the daemon was doing. Call it from the test
// goroutine: it registers the cleanup that stops the writer, and t.Log
// panics once the test and its cleanups are done.
func testLogger(t *testing.T) *log.Logger {
	w := &testLogWriter{t: t}
	t.Cleanup(w.stop)
	return log.New(w, "", 0)
}

type testLogWriter struct {
	t *testing.T

	mu      sync.Mutex
	stopped bool
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

// stop drops later lines. A goroutine the test did not join can still hold
// the logger, and writing from one after the test ends takes the whole run
// down.
func (w *testLogWriter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
}
