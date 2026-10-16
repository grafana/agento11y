//go:build windows

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	handoffHelperExitCode = "AGENTO11Y_TEST_HANDOFF_EXIT_CODE"
	handoffBatchHelper    = "AGENTO11Y_TEST_HANDOFF_BATCH_HELPER"
	handoffInterruptRole  = "AGENTO11Y_TEST_HANDOFF_INTERRUPT_ROLE"
)

func TestDefaultExecReturnsChildExitCode(t *testing.T) {
	for _, code := range []int{0, 42} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			stdout, restoreStdout := captureProcessFile(t, &os.Stdout)
			stderr, restoreStderr := captureProcessFile(t, &os.Stderr)

			env := append(os.Environ(), handoffHelperExitCode+"="+strconv.Itoa(code))
			err := DefaultExec(os.Args[0], []string{os.Args[0], "-test.run=TestDefaultExecHelper"}, env)
			restoreStdout()
			restoreStderr()

			if code == 0 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else {
				var exitErr *ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("err = %v, want ExitError", err)
				}
				if exitErr.ExitCode() != code {
					t.Fatalf("exit code = %d, want %d", exitErr.ExitCode(), code)
				}
			}
			if !strings.Contains(stdout(), "child stdout") {
				t.Fatalf("stdout = %q, want child output", stdout())
			}
			if !strings.Contains(stderr(), "child stderr") {
				t.Fatalf("stderr = %q, want child output", stderr())
			}
		})
	}
}

func TestDefaultExecHelper(t *testing.T) {
	raw := os.Getenv(handoffHelperExitCode)
	if raw == "" {
		return
	}
	code, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(os.Stdout, "child stdout")
	_, _ = fmt.Fprint(os.Stderr, "child stderr")
	os.Exit(code)
}

// TestDefaultExecBatchShimPreservesArguments runs the round-trip arguments
// through a real cmd.exe and batch shim, the way npm installs agents. The
// helper reports its raw command line because Node.js parses it with the
// Microsoft C runtime rules, which Go's own os.Args parsing does not follow.
func TestDefaultExecBatchShimPreservesArguments(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.exe")
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.Create(helper)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	closeDestinationErr := destination.Close()
	closeSourceErr := source.Close()
	if copyErr != nil || closeDestinationErr != nil || closeSourceErr != nil {
		t.Fatalf("copy helper: copy=%v close destination=%v close source=%v", copyErr, closeDestinationErr, closeSourceErr)
	}

	shim := filepath.Join(dir, "agent shim.cmd")
	body := "@echo off\r\n\"%~dp0helper.exe\" -test.run=^TestDefaultExecBatchHelper$ %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// If cmd.exe ran the & in this argument as a command, the marker appears.
	marker := filepath.Join(dir, "injected")
	want := append(slices.Clone(batchRoundTripArgs), `x" & echo injected> "`+marker+`" & "`)

	stdout, restoreStdout := captureProcessFile(t, &os.Stdout)
	env := append(os.Environ(), handoffBatchHelper+"=1")
	err = DefaultExec(shim, append([]string{shim}, want...), env)
	restoreStdout()
	if err != nil {
		t.Fatal(err)
	}
	var commandLine string
	if err := json.Unmarshal([]byte(stdout()), &commandLine); err != nil {
		t.Fatalf("decode helper command line: %v", err)
	}
	got := parseMSVCArgs(commandLine)
	if len(got) < 2 || !slices.Equal(got[2:], want) {
		t.Fatalf("arguments from %s\n= %q\nwant %q", commandLine, got, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("cmd.exe ran a command from an argument")
	}
}

func TestDefaultExecBatchHelper(t *testing.T) {
	if os.Getenv(handoffBatchHelper) != "1" {
		return
	}
	body, err := json.Marshal(windows.UTF16PtrToString(windows.GetCommandLine()))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.Write(body)
	// Returning would let the test framework print its own PASS line into
	// the stdout the parent decodes as JSON.
	os.Exit(0)
}

// TestDefaultExecSurvivesConsoleInterrupt runs a launcher in a console of its
// own and has the agent raise Ctrl+Break on that console, which reaches both
// processes. The launcher must outlive the event and exit with the agent's
// code. A launcher that does not watch os.Interrupt dies with
// STATUS_CONTROL_C_EXIT instead. The runtime maps Ctrl+C and Ctrl+Break to the
// same signal; Ctrl+Break is used because an inherited console setting can
// disable Ctrl+C for every process on the runner.
func TestDefaultExecSurvivesConsoleInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDefaultExecInterruptLauncherHelper$")
	cmd.Env = append(os.Environ(), handoffInterruptRole+"=launcher")
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = 5 * time.Second
	// A separate console keeps the event away from this test process.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}

	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("launcher result = %v, want exit code 42 from the agent; output:\n%s", err, output.String())
	}
}

func TestDefaultExecInterruptLauncherHelper(t *testing.T) {
	if os.Getenv(handoffInterruptRole) != "launcher" {
		return
	}
	env := append(os.Environ(), handoffInterruptRole+"=agent")
	err := DefaultExec(os.Args[0], []string{os.Args[0], "-test.run=^TestDefaultExecInterruptAgentHelper$"}, env)
	var exitErr *ExitError
	switch {
	case err == nil:
		os.Exit(0)
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	default:
		_, _ = fmt.Fprintln(os.Stderr, "launcher:", err)
		os.Exit(99)
	}
}

func TestDefaultExecInterruptAgentHelper(t *testing.T) {
	if os.Getenv(handoffInterruptRole) != "agent" {
		return
	}
	received := make(chan os.Signal, 1)
	signal.Notify(received, os.Interrupt)
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agent: GenerateConsoleCtrlEvent:", err)
		os.Exit(98)
	}
	select {
	case <-received:
		os.Exit(42)
	case <-time.After(20 * time.Second):
		_, _ = fmt.Fprintln(os.Stderr, "agent: no interrupt arrived")
		os.Exit(97)
	}
}

func captureProcessFile(t *testing.T, target **os.File) (read func() string, restore func()) {
	t.Helper()
	original := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*target = w
	return func() string {
			body, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			_ = r.Close()
			return string(body)
		}, func() {
			_ = w.Close()
			*target = original
		}
}
