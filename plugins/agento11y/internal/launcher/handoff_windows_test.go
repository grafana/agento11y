//go:build windows

package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	handoffHelperExitCode = "AGENTO11Y_TEST_HANDOFF_EXIT_CODE"
	handoffBatchHelper    = "AGENTO11Y_TEST_HANDOFF_BATCH_HELPER"
)

func TestDefaultExecReturnsChildExitCode(t *testing.T) {
	for _, code := range []int{0, 42} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			stdout, restoreStdout := captureProcessFile(t, &os.Stdout)
			stderr, restoreStderr := captureProcessFile(t, &os.Stderr)

			env := append(os.Environ(), handoffHelperExitCode+"="+strconv.Itoa(code))
			err := DefaultExec(os.Args[0], []string{os.Args[0], "-test.run=TestDefaultExecHelper"}, env)
			signal.Reset(os.Interrupt)
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
	want := []string{"plain", "foo&bar", "%PATH%", `say "hello"`, "space arg", `trailing\`, `caret^bang!`, `pipe|redirect<in>out`}

	stdout, restoreStdout := captureProcessFile(t, &os.Stdout)
	env := append(os.Environ(), handoffBatchHelper+"=1")
	err = DefaultExec(shim, append([]string{shim}, want...), env)
	signal.Reset(os.Interrupt)
	restoreStdout()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal([]byte(stdout()), &got); err != nil {
		t.Fatalf("decode helper arguments: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestDefaultExecBatchHelper(t *testing.T) {
	if os.Getenv(handoffBatchHelper) != "1" {
		return
	}
	body, err := json.Marshal(os.Args[2:])
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.Write(body)
	// Returning would let the test framework print its own PASS line into
	// the stdout the parent decodes as JSON.
	os.Exit(0)
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
