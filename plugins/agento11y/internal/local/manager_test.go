//go:build !windows

package local

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// TestServe_StampsStoreAfterPublishingStatus covers the order the daemon
// starts things in: the status file the spawning process waits 5 seconds
// for appears while the modification-time pass is still reading the store,
// and the store repairs itself once the pass gets through it.
//
// A named pipe among the conversation files holds the pass: opening one
// for reading returns only once this test opens the write end, so the pass
// cannot reach the marker until the test lets it.
func TestServe_StampsStoreAfterPublishingStatus(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	activity := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)
	seedConversation(t, storage, "conv-A", activity)
	blocker := filepath.Join(dir, ConversationsDir, "conv-blocker.jsonl")
	require.NoError(t, syscall.Mkfifo(blocker, 0o600))

	// The pass reports what it stamped and what it could not, and this test
	// can only fail by the pass not finishing, so keep its log.
	logger := testLogger(t)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, dir, 0, logger) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-served)
	})
	// Serve waits for the pass on the way out, so a failed assertion below
	// must not leave it parked on the pipe. Opening the write end without
	// blocking fails with ENXIO when the pass is not reading, which is the
	// case where nothing needs freeing.
	t.Cleanup(func() {
		if w, err := os.OpenFile(blocker, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})

	// IsRunning is what the spawning process polls: the status file plus an
	// endpoint that answers.
	require.Eventually(t, func() bool {
		s, err := IsRunning(dir)
		return err == nil && s != nil
	}, 5*time.Second, 20*time.Millisecond, "daemon published its status and serves")

	meta, err := readStoreMeta(dir)
	require.NoError(t, err)
	require.False(t, meta.MtimeStamped, "the daemon serves while the pass is still reading")

	// Let the pass through the pipe: an empty file carries no activity, so
	// it is scanned and left alone.
	w, err := os.OpenFile(blocker, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.Eventually(t, func() bool {
		meta, err := readStoreMeta(dir)
		return err == nil && meta.MtimeStamped
	}, 5*time.Second, 20*time.Millisecond, "store marked as stamped")

	info, err := os.Stat(filepath.Join(dir, ConversationsDir, "conv-A.jsonl"))
	require.NoError(t, err)
	assert.WithinDuration(t, activity, info.ModTime(), stampTolerance)
}

func TestStop_EndpointDeadButProcessAlive(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep command unavailable")
	}
	for _, tc := range []struct {
		name         string
		cmdline      string
		cmdlineError error
		liveEndpoint bool
		wantStop     bool
		wantAlive    bool
		wantErr      string
		wantStatus   bool
	}{
		{name: "signals daemon-looking process", cmdline: "/usr/local/bin/sigil local serve", wantStop: true},
		{name: "signals agento11y daemon", cmdline: "/usr/local/bin/agento11y local serve", wantStop: true},
		{name: "signals agento11y.exe daemon", cmdline: "/usr/local/bin/agento11y.exe local serve", wantStop: true},
		{name: "signals daemon-looking process with spaces in path", cmdline: "/tmp/Sigil Dev/sigil local serve", wantStop: true},
		{name: "signals go run dev daemon", cmdline: "/Users/x/Library/Caches/go-build/ab/cd-d/main local serve", wantStop: true},
		{name: "does not signal unrelated live pid", cmdline: "sleep 60", wantAlive: true},
		{name: "does not signal main without local serve suffix", cmdline: "/tmp/main serve", wantAlive: true},
		{name: "does not signal non-sigil binary with local serve suffix", cmdline: "/tmp/python local serve", wantAlive: true},
		{name: "does not signal unrelated live pid with healthy endpoint", cmdline: "sleep 60", liveEndpoint: true, wantAlive: true},
		{name: "keeps status when pid identity cannot be checked", cmdlineError: errors.New("ps failed"), wantAlive: true, wantErr: "identify recorded daemon", wantStatus: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("sleep", "60")
			require.NoError(t, cmd.Start())
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

			withProcessCommandLine(t, func(pid int) (string, error) {
				require.Equal(t, cmd.Process.Pid, pid)
				return tc.cmdline, tc.cmdlineError
			})

			endpoint := "http://127.0.0.1:1"
			if tc.liveEndpoint {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(ts.Close)
				endpoint = ts.URL
			}
			require.NoError(t, SaveStatus(dir, Status{PID: cmd.Process.Pid, Port: 1, Endpoint: endpoint}))
			stopped, err := Stop(dir)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantStop, stopped)
			if tc.wantAlive {
				assert.True(t, pidAlive(cmd.Process.Pid))
			} else {
				select {
				case <-done:
					waited = true
				case <-time.After(time.Second):
					t.Fatal("daemon process still running after Stop returned")
				}
			}
			_, err = os.Stat(filepath.Join(dir, StatusFile))
			if tc.wantStatus {
				assert.NoError(t, err, "status file should remain when daemon identity cannot be checked")
			} else {
				assert.True(t, os.IsNotExist(err), "status file should be removed")
			}
		})
	}
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

// TestEnsureRunning_ConcurrentCallersSpawnOnce drives several
// EnsureRunning goroutines at the same empty state dir. A healthy
// daemon is simulated by an httptest server so endpointAlive returns
// true after the first SaveStatus; the flock-guarded recheck inside
// EnsureRunning ensures only the first caller spawns the daemon and
// the rest converge on the saved status.
func withProcessCommandLine(t *testing.T, fn func(int) (string, error)) {
	t.Helper()
	prev := processCommandLineFn
	t.Cleanup(func() { processCommandLineFn = prev })
	processCommandLineFn = fn
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
		// flock; without this, the first caller may finish before the
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
		t.Errorf("spawns = %d, want 1 (flock should serialise EnsureRunning)", got)
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
