package harbor

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	originalLookPath, originalExec := lookPath, execFn
	t.Cleanup(func() { lookPath, execFn = originalLookPath, originalExec })
	lookPath = func(string) (string, error) { return "/bin/agento11y-harbor", nil }
	var got []string
	execFn = func(_ string, argv, _ []string) error {
		got = argv
		return nil
	}

	if err := Run([]string{"export", "/tmp/job"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/agento11y-harbor", "export", "/tmp/job"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
}

func TestRunWrapsExecFailure(t *testing.T) {
	originalLookPath, originalExec := lookPath, execFn
	t.Cleanup(func() { lookPath, execFn = originalLookPath, originalExec })
	lookPath = func(string) (string, error) { return "/bin/agento11y-harbor", nil }
	execFn = func(string, []string, []string) error { return errors.New("permission denied") }

	err := Run(nil, &bytes.Buffer{})
	if err == nil || !strings.HasPrefix(err.Error(), "exec agento11y-harbor: ") {
		t.Fatalf("err = %v, want launcher.Exec wrapping", err)
	}
}

func TestRunMissingIntegration(t *testing.T) {
	original := lookPath
	t.Cleanup(func() { lookPath = original })
	lookPath = func(string) (string, error) { return "", errors.New("missing") }
	if err := Run(nil, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an installation error")
	}
}
