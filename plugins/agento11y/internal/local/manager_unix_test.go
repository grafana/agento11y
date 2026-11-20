//go:build !windows

package local

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStop_EndpointDeadButProcessAlive(t *testing.T) {
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
			cmd := exec.Command(os.Args[0], "-test.run=^TestIdleHelperProcess$")
			cmd.Env = append(os.Environ(), idleHelperEnv+"=1")
			require.NoError(t, cmd.Start())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			t.Cleanup(func() {
				if waited {
					return
				}
				_ = cmd.Process.Kill()
				_ = <-done
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
				// Stop killed the process, so Wait reports the signal.
				case _ = <-done:
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

func TestUnixDaemonProcessRechecksIdentityBeforeTerminate(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestIdleHelperProcess$")
	cmd.Env = append(os.Environ(), idleHelperEnv+"=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	cmdline := "/usr/local/bin/agento11y local serve"
	withProcessCommandLine(t, func(pid int) (string, error) {
		require.Equal(t, cmd.Process.Pid, pid)
		return cmdline, nil
	})
	proc, err := openDaemonProcess(Status{PID: cmd.Process.Pid})
	require.NoError(t, err)
	require.NotNil(t, proc)
	defer proc.close()

	cmdline = "sleep 60"
	require.NoError(t, proc.terminate())
	assert.True(t, pidAlive(cmd.Process.Pid))
}

func trustDaemonHelperProcess(t *testing.T, wantPID int) {
	withProcessCommandLine(t, func(pid int) (string, error) {
		require.Equal(t, wantPID, pid)
		return "/tmp/agento11y -test.run=^TestLocalDaemonHelperProcess$ local serve", nil
	})
}

func withProcessCommandLine(t *testing.T, fn func(int) (string, error)) {
	t.Helper()
	prev := processCommandLineFn
	t.Cleanup(func() { processCommandLineFn = prev })
	processCommandLineFn = fn
}

// TestStop_WaitsPastTheDrain stops a process that takes four seconds to honor
// SIGTERM, the way the daemon does when a slow request is in flight and it
// drains for drainTimeout first. Stop has to outlast that drain: before the
// wait exceeded it, this reported "did not exit within 3s" and exited 1 for a
// daemon that exited a moment later.
func TestStop_WaitsPastTheDrain(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh command unavailable")
	}
	dir := t.TempDir()
	// The shell prints "ready" once the trap is armed; signaling it before
	// that would hit the default action and end the process at once.
	cmd := exec.Command("sh", "-c", `trap 'sleep 4; exit 0' TERM; echo ready; while :; do sleep 0.1; done`)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = cmd.Process.Kill()
		_ = <-done
	})
	withProcessCommandLine(t, func(pid int) (string, error) {
		require.Equal(t, cmd.Process.Pid, pid)
		return "/usr/local/bin/agento11y local serve", nil
	})
	require.NoError(t, SaveStatus(dir, Status{PID: cmd.Process.Pid, Port: 1, Endpoint: "http://127.0.0.1:1"}))

	stopped, err := Stop(dir)
	require.NoError(t, err, "a daemon still draining after SIGTERM is within the stop wait")
	assert.True(t, stopped)
	select {
	// Stop killed the process, so Wait reports the signal.
	case _ = <-done:
		waited = true
	case <-time.After(time.Second):
		t.Fatal("process still running after Stop returned")
	}
	_, err = os.Stat(filepath.Join(dir, StatusFile))
	assert.True(t, os.IsNotExist(err), "status file should be removed once the daemon exits")
}

func TestServePublishesHealthBeforeMigrationCompletes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	require.NoError(t, storage.AppendGeneration(generationRecord{
		ReceivedAt:     "2026-08-20T08:00:00Z",
		GenerationID:   "g1",
		ConversationID: "conv-A",
		Generation:     []byte(`{"id":"g1","conversation_id":"conv-A"}`),
	}))

	// The first path in migration order blocks scanLatestGenerationRecords.
	// Health must answer while that scan is still waiting.
	blocker := filepath.Join(dir, ConversationsDir, "000-blocker.jsonl")
	require.NoError(t, syscall.Mkfifo(blocker, 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, dir, 0, log.New(os.Stderr, "", 0)) }()
	t.Cleanup(func() {
		if w, openErr := os.OpenFile(blocker, os.O_WRONLY|syscall.O_NONBLOCK, 0); openErr == nil {
			_ = w.Close()
		}
		cancel()
		require.NoError(t, <-served)
	})

	var status *Status
	require.Eventually(t, func() bool {
		status, err = LoadStatus(dir)
		return err == nil && status != nil && endpointAlive(status.Endpoint)
	}, 5*time.Second, 20*time.Millisecond, "daemon publishes status and health before migration finishes")

	// Opening the writer releases the parked migration scan. Cleanup tries the
	// same non-blockingly; OpenFile returns ENXIO when no scan is parked.
	w, err := os.OpenFile(blocker, os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())
}
