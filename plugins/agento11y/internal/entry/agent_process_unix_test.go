//go:build !windows

package entry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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

// stopAgentProcessGroup kills what an exited agent left running in its
// process group and waits until the group is empty. Codex starts a sync of
// its curated plugin marketplace on every run and exits without waiting for
// it, so its `git fetch` and `git reset --hard` keep writing under
// $CODEX_HOME/.tmp while the test's TempDir cleanup removes that directory.
func stopAgentProcessGroup(cmd *exec.Cmd) error {
	group := -cmd.Process.Pid
	if err := syscall.Kill(group, syscall.SIGKILL); err != nil {
		if groupGone(err) {
			return nil
		}
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); groupRunning(-group); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("agent process group %d still running after SIGKILL", -group)
		}
	}
	return nil
}

// groupRunning reports whether the group has a member that is not a zombie.
// The leftovers are orphans, so PID 1 reaps them, and in a container whose
// PID 1 is not an init it never does: on Linux kill(-pgid, 0) keeps
// succeeding for processes that have already exited.
func groupRunning(pgid int) bool {
	if err := syscall.Kill(-pgid, 0); err != nil {
		return !groupGone(err)
	}
	if runtime.GOOS != "linux" {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if state, pgrp, ok := procStat(pid); ok && pgrp == pgid && state != 'Z' {
			return true
		}
	}
	return false
}

// groupGone reports whether a kill of a whole group failed because nothing
// in it is running. Darwin returns EPERM, not ESRCH, for a group whose
// members are all zombies.
func groupGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || runtime.GOOS == "darwin" && errors.Is(err, syscall.EPERM)
}

// procStat reads a Linux process's state and process group. It reports false
// where there is no /proc/<pid>/stat, which is everywhere but Linux.
func procStat(pid int) (state byte, pgrp int, ok bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, false
	}
	// The command name in parentheses can hold spaces and ')', so the fields
	// after it are read from the last ')': state, ppid, pgrp.
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	if len(fields) < 3 {
		return 0, 0, false
	}
	pgrp, err = strconv.Atoi(fields[2])
	return fields[0][0], pgrp, err == nil
}
