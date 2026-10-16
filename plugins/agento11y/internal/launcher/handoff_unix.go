//go:build !windows

package launcher

import "syscall"

// DefaultExec replaces the launcher process with the target agent on Unix.
var DefaultExec ExecFunc = syscall.Exec
