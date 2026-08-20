//go:build windows

package local

import (
	"fmt"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestAddrInUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "address in use", err: windows.WSAEADDRINUSE, want: true},
		{name: "wrapped address in use", err: fmt.Errorf("listen: %w", windows.WSAEADDRINUSE), want: true},
		{name: "access denied", err: syscall.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := addrInUse(tc.err); got != tc.want {
				t.Fatalf("addrInUse(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
