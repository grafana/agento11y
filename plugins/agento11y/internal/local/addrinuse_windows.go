//go:build windows

package local

import (
	"errors"

	"golang.org/x/sys/windows"
)

func addrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}
