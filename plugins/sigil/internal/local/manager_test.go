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
		{name: "signals daemon-looking process with spaces in path", cmdline: "/tmp/Sigil Dev/sigil local serve", wantStop: true},
		{name: "does not signal unrelated live pid", cmdline: "sleep 60", wantAlive: true},
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
