package hermes

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

func TestInstallUsesHermesPluginManager(t *testing.T) {
	originalLookPath, originalRunSteps := lookPath, runSteps
	t.Cleanup(func() { lookPath, runSteps = originalLookPath, originalRunSteps })
	lookPath = func(name string) (string, error) {
		if name != "hermes" {
			t.Fatalf("lookPath(%q)", name)
		}
		return "/usr/local/bin/hermes", nil
	}
	var got [][]string
	runSteps = func(_ context.Context, bin string, _ io.Writer, steps [][]string) error {
		if bin != "/usr/local/bin/hermes" {
			t.Fatalf("bin = %q", bin)
		}
		got = steps
		return nil
	}
	changed, err := Install(context.Background(), io.Discard, log.Default())
	if err != nil || !changed {
		t.Fatalf("Install() = %v, %v", changed, err)
	}
	want := [][]string{{"plugins", "install", PluginSource, "--force"}, {"plugins", "enable", PluginName}}
	if len(got) != len(want) {
		t.Fatalf("steps = %#v", got)
	}
	for i := range want {
		if !equalStrings(got[i], want[i]) {
			t.Fatalf("step %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestInstallHonorsLocalPluginSource(t *testing.T) {
	originalLookPath, originalRunSteps := lookPath, runSteps
	t.Cleanup(func() { lookPath, runSteps = originalLookPath, originalRunSteps })
	t.Setenv("AGENTO11Y_HERMES_PLUGIN_SOURCE", "file:///tmp/agento11y#plugins/hermes")
	lookPath = func(string) (string, error) { return "/usr/local/bin/hermes", nil }
	var got [][]string
	runSteps = func(_ context.Context, _ string, _ io.Writer, steps [][]string) error { got = steps; return nil }
	_, _ = Install(context.Background(), io.Discard, log.Default())
	if got[0][2] != "file:///tmp/agento11y#plugins/hermes" {
		t.Fatalf("plugin source = %q", got[0][2])
	}
}

func TestLaunchExecutesHermesWithLocalEnvironment(t *testing.T) {
	originalLookPath, originalExecFn := lookPath, execFn
	t.Cleanup(func() { lookPath, execFn = originalLookPath, originalExecFn })
	lookPath = func(string) (string, error) { return "/usr/local/bin/hermes", nil }
	var gotArgs, gotEnv []string
	execFn = func(_ string, argv, env []string) error { gotArgs, gotEnv = argv, env; return errors.New("stop") }
	err := Launch(context.Background(), []string{"chat"}, &local.LaunchEnv{Endpoint: "http://127.0.0.1:8765", OTLPEndpoint: "http://127.0.0.1:8765/otlp"}, nil, nil, nil, nil, "")
	if err == nil {
		t.Fatal("Launch() succeeded, want exec error")
	}
	if !equalStrings(gotArgs, []string{"/usr/local/bin/hermes", "chat"}) {
		t.Fatalf("argv = %#v", gotArgs)
	}
	if !contains(gotEnv, "AGENTO11Y_ENDPOINT=http://127.0.0.1:8765") {
		t.Fatalf("local endpoint missing from environment")
	}
	if !contains(gotEnv, "AGENTO11Y_PROTOCOL=http") {
		t.Fatalf("local HTTP protocol missing from environment")
	}
}

func TestInstallReturnsMissingHost(t *testing.T) {
	originalLookPath := lookPath
	t.Cleanup(func() { lookPath = originalLookPath })
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	_, err := Install(context.Background(), io.Discard, log.Default())
	if !errors.Is(err, ErrCLINotFound) {
		t.Fatalf("Install error = %v", err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
