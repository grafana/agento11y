//go:build windows

package local

import (
	"errors"

	"golang.org/x/sys/windows"
)

func addrInUse(err error) bool {
	// Excluded ports and exclusive binds can return WSAEACCES instead.
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}
