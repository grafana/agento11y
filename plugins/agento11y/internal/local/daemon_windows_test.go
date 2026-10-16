//go:build windows

package local

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Windows pairs the image name with process creation time because it cannot
// read another process's command line safely.
func TestCreatedForStatus(t *testing.T) {
	started := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		created   time.Time
		startedAt string
		want      bool
		wantErr   bool
	}{
		{
			name:    "a process created just before its status file is the daemon",
			created: started.Add(-200 * time.Millisecond),
			want:    true,
		},
		{
			name:    "a cold start between creation and status file still matches",
			created: started.Add(-90 * time.Second),
			want:    true,
		},
		{
			name:    "one second after the status file is within clock slack",
			created: started.Add(time.Second),
			want:    true,
		},
		{
			name:    "a process created long before the status file is a recycled pid",
			created: started.Add(-10 * time.Minute),
		},
		{
			name:    "a process created after the status file is a recycled pid",
			created: started.Add(time.Minute),
		},
		{
			name:      "a status file with no usable timestamp cannot identify the process",
			created:   started.Add(-10 * time.Minute),
			startedAt: "whenever",
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startedAt := tc.startedAt
			if startedAt == "" {
				startedAt = started.Format(time.RFC3339Nano)
			}
			got, err := createdForStatus(tc.created, startedAt)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Windows image paths use backslashes and compare without case sensitivity.
func TestImageIsDaemonWindowsPaths(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{path: `C:\Program Files\agento11y\agento11y.exe`, want: true},
		{path: `C:\Users\x\AppData\Local\agento11y\SIGIL.EXE`, want: true},
		// `go run` compiles to "main" in the build cache.
		{path: `C:\Users\x\AppData\Local\Temp\go-build123\b001\main.exe`, want: true},
		{path: `C:\Windows\System32\notepad.exe`, want: false},
		{path: `C:\Python312\python.exe`, want: false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, imageIsDaemon(tc.path))
		})
	}
}

// A live process with another image name is not the recorded daemon.
func TestOpenDaemonProcessRejectsUnrelatedProcess(t *testing.T) {
	self := Status{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	}
	proc, err := openDaemonProcess(self)
	require.NoError(t, err)
	assert.Nil(t, proc, "a test binary is not the recorded daemon")
}

// The spawning process keeps the child's handle, so childExited answers from
// the handle's signalled state. Only a PID with no process behind it counts
// as an exit: a false exit report abandons a running daemon and lets the
// caller start a second one on the same store.
func TestChildExited(t *testing.T) {
	helper := copyTestExecutable(t)
	cmd := exec.Command(helper, "-test.run=^TestIdleHelperProcess$")
	cmd.Env = append(os.Environ(), idleHelperEnv+"=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	assert.False(t, childExited(cmd.Process.Pid), "the helper is still running")

	require.NoError(t, cmd.Process.Kill())
	// No Wait: startDaemon does not reap either, and holding the handle keeps
	// the PID from being reused while the check runs.
	assert.Eventually(t, func() bool { return childExited(cmd.Process.Pid) }, 5*time.Second, 10*time.Millisecond)

	// Windows hands out PIDs in multiples of four, so an odd one belongs to no
	// process and OpenProcess rejects it as an invalid parameter.
	assert.True(t, childExited(0x7FFFFFF1))
}

func TestWindowsDaemonProcessKeepsOriginalHandleAfterExit(t *testing.T) {
	helper := copyTestExecutable(t)
	cmd := exec.Command(helper, "-test.run=^TestIdleHelperProcess$")
	cmd.Env = append(os.Environ(), idleHelperEnv+"=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	proc, err := openDaemonProcess(Status{
		PID:       cmd.Process.Pid,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	require.NotNil(t, proc)
	defer proc.close()

	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())
	assert.False(t, proc.alive())
	assert.NoError(t, proc.terminate())
}
