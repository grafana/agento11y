package local

// daemonProcess keeps Stop tied to the process whose identity it checked.
// An operating system can reuse a PID after graceful shutdown starts.
type daemonProcess interface {
	alive() bool
	terminate() error
	close()
}
