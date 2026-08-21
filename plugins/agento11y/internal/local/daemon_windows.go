//go:build windows

package local

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// ReceiverSupported reports whether this platform can run the local
// capture daemon.
func ReceiverSupported() bool { return true }

// daemonStartupWindow is how long before its status file a daemon may have
// been created and still count as the process that wrote it. The creation
// time comes from the kernel; the recorded start is what the daemon itself
// formatted after opening its store and binding its listener, so the two
// never agree exactly, and a cold start behind a virus scanner can spend
// seconds between them. The window only has to be tight enough that a PID
// reused by another agento11y process is not mistaken for this one.
const daemonStartupWindow = 2 * time.Minute

// creationTimeSlack absorbs a creation time that reads later than the
// recorded start. Both timestamps come from the system clock, but a process
// creation time is a FILETIME the kernel stamps at its own resolution, and
// the clock can be re-synced between the two reads.
const creationTimeSlack = 5 * time.Second

// daemonSysProcAttr detaches the daemon from the process that spawns it.
// DETACHED_PROCESS keeps it off this process's console, so closing the
// terminal does not close the daemon, and CREATE_NEW_PROCESS_GROUP keeps
// console Ctrl events sent to the launcher's group away from it.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// childExited reports whether the daemon this process spawned has already
// exited. The spawning process still holds the child's process handle, so
// the PID stays valid until this process drops it and cannot have been
// reused underneath this check.
func childExited(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// A PID with no process behind it answers ERROR_INVALID_PARAMETER.
		// Any other refusal, a denied open above all, says nothing about the
		// child, and reporting an exit that did not happen would leave the
		// detached daemon running while the caller starts a second one on
		// the same store.
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event == uint32(windows.WAIT_OBJECT_0)
}

// pidAlive reports whether a process with the given PID is still running. A
// process handle is signalled once the process exits, so a handle that
// times out on a zero-length wait is a process still running, and one that
// returns immediately is a process that exited but whose handles are not
// all closed yet.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

type windowsDaemonProcess struct {
	pid    int
	handle windows.Handle
}

func openDaemonProcess(status Status) (daemonProcess, error) {
	if status.PID <= 0 {
		return nil, nil
	}
	const access = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE | windows.PROCESS_TERMINATE
	h, err := windows.OpenProcess(access, false, uint32(status.PID))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open process %d: %w", status.PID, err)
	}
	matches, err := processHandleMatchesDaemon(h, status)
	if err != nil || !matches {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &windowsDaemonProcess{pid: status.PID, handle: h}, nil
}

func (p *windowsDaemonProcess) alive() bool {
	if p.handle == 0 {
		return false
	}
	event, err := windows.WaitForSingleObject(p.handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

func (p *windowsDaemonProcess) terminate() error {
	if !p.alive() {
		return nil
	}
	if err := windows.TerminateProcess(p.handle, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) && !p.alive() {
			return nil
		}
		return fmt.Errorf("terminate process %d: %w", p.pid, err)
	}
	return nil
}

func (p *windowsDaemonProcess) close() {
	if p.handle != 0 {
		_ = windows.CloseHandle(p.handle)
		p.handle = 0
	}
}

// processHandleMatchesDaemon checks whether an open process handle refers to
// the daemon that wrote s. Windows does not expose another process's command
// line through the query-information API, so the check uses its image path and
// creation time.
func processHandleMatchesDaemon(h windows.Handle, s Status) (bool, error) {
	image, err := processImagePath(h)
	if err != nil {
		return false, err
	}
	if !imageIsDaemon(image) {
		return false, nil
	}
	created, err := processCreationTime(h)
	if err != nil {
		return false, err
	}
	return createdForStatus(created, s.StartedAt)
}

// createdForStatus reports whether a process created at created could be the
// daemon that recorded startedAt.
func createdForStatus(created time.Time, startedAt string) (bool, error) {
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return false, fmt.Errorf("parse daemon start time: %w", err)
	}
	age := started.Sub(created)
	return age >= -creationTimeSlack && age <= daemonStartupWindow, nil
}

// processImagePath returns the full path of the executable a process is
// running.
func processImagePath(h windows.Handle) (string, error) {
	// An image behind an extended-length path is longer than MAX_PATH, so
	// grow the buffer up to the 32K ceiling Windows allows instead of
	// failing on the first short read.
	for n := windows.MAX_PATH; ; n = min(n*2, 32768) {
		buf := make([]uint16, n)
		size := uint32(len(buf))
		err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size)
		switch {
		case err == nil:
			return windows.UTF16ToString(buf[:size]), nil
		case errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) && n < 32768:
			continue
		default:
			return "", fmt.Errorf("query process image name: %w", err)
		}
	}
}

// processCreationTime returns when a process was created, in UTC.
func processCreationTime(h windows.Handle) (time.Time, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, fmt.Errorf("read process times: %w", err)
	}
	return time.Unix(0, creation.Nanoseconds()).UTC(), nil
}
