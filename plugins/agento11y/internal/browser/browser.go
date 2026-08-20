// Package browser opens URLs in the user's browser.
package browser

import (
	"os/exec"
	"runtime"
)

// Open starts the operating system URL handler for target. It returns after
// the process starts and does not report later handler failures.
func Open(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child in the background so long-running callers do not leave a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}
