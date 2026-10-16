//go:build !windows

package local

import (
	"errors"
	"syscall"
)

func addrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
