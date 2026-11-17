//go:build !windows

package entry

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentProcessStopsBackgroundDescendants(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	require.NoError(t, listener.SetDeadline(time.Now().Add(10*time.Second)))

	outcome := runAgentProcess(t.Context(), t.TempDir(), []string{
		os.Args[0], "-test.run=^TestAgentBackgroundProcess$", "--", "launcher", listener.Addr().String(),
	})
	require.NoError(t, outcome.check(""))
	connection, err := listener.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = connection.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "background child must exit even when its launcher succeeds")
}

func TestAgentBackgroundProcess(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if args[0] == "child" {
		connection, err := net.DialTimeout("tcp4", args[1], 5*time.Second)
		require.NoError(t, err)
		_, err = os.Stdout.Write([]byte{1})
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, connection)
		_ = connection.Close()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentBackgroundProcess$", "--", "child", args[1])
	ready, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// Wait until the child connects, then exit without waiting for it.
	_, err = io.ReadFull(ready, make([]byte, 1))
	require.NoError(t, err)
	_ = ready.Close()
	require.NoError(t, cmd.Process.Release())
	os.Exit(0)
}

func cleanupAgentProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Codex can exit successfully while its marketplace Git clone is still running.
	err := cmd.Cancel()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func configureAgentProcess(cmd *exec.Cmd) {
	// The npm launcher and its native child must share the cancellation target.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
