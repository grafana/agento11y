package vibe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestLaunch_InstallsHookAndExecs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIBE_HOME", dir)

	// Stand-in for syscall.Exec: capture argv + env so we can assert
	// that the experimental-hooks env var is force-set and that vibe
	// would receive the forwarded args.
	type call struct {
		bin  string
		argv []string
		env  []string
	}
	var got call
	origLookPath, origExec := lookPath, execFn
	t.Cleanup(func() { lookPath, execFn = origLookPath, origExec })
	lookPath = func(string) (string, error) { return "/fake/vibe", nil }
	execFn = func(bin string, argv []string, env []string) error {
		got = call{bin: bin, argv: argv, env: env}
		return nil
	}

	var stderr bytes.Buffer
	logger := log.New(io.Discard, "", 0)
	err := Launch(context.Background(), []string{"--print", "hi"}, nil, nil, io.Discard, &stderr, logger, "test")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got.bin != "/fake/vibe" {
		t.Errorf("bin = %q, want /fake/vibe", got.bin)
	}
	wantArgv := []string{"/fake/vibe", "--print", "hi"}
	if len(got.argv) != len(wantArgv) {
		t.Fatalf("argv = %v, want %v", got.argv, wantArgv)
	}
	for i, v := range wantArgv {
		if got.argv[i] != v {
			t.Errorf("argv[%d] = %q, want %q", i, got.argv[i], v)
		}
	}

	if !hasEnv(got.env, "VIBE_ENABLE_EXPERIMENTAL_HOOKS=true") {
		t.Errorf("env missing VIBE_ENABLE_EXPERIMENTAL_HOOKS=true: %v", got.env)
	}

	// Hook should have been written under VIBE_HOME.
	if _, err := os.Stat(dir + "/hooks.toml"); err != nil {
		t.Errorf("hooks.toml not written: %v", err)
	}
	if !strings.Contains(stderr.String(), "installed Vibe hook") {
		t.Errorf("stderr = %q, want install message", stderr.String())
	}
}

func TestLaunch_VibeNotOnPath(t *testing.T) {
	origLookPath := lookPath
	t.Cleanup(func() { lookPath = origLookPath })
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	logger := log.New(io.Discard, "", 0)
	err := Launch(context.Background(), nil, nil, nil, io.Discard, io.Discard, logger, "v")
	if err == nil || !strings.Contains(err.Error(), "vibe CLI not found") {
		t.Errorf("Launch err = %v, want vibe CLI not found", err)
	}
}

func TestEnvWithExperimentalHooks_Replaces(t *testing.T) {
	in := []string{"PATH=/usr/bin", "VIBE_ENABLE_EXPERIMENTAL_HOOKS=false", "OTHER=1"}
	out := envWithExperimentalHooks(in)
	if !hasEnv(out, "VIBE_ENABLE_EXPERIMENTAL_HOOKS=true") {
		t.Errorf("env did not force true: %v", out)
	}
	for _, kv := range out {
		if kv == "VIBE_ENABLE_EXPERIMENTAL_HOOKS=false" {
			t.Errorf("stale false survived: %v", out)
		}
	}
}

func TestEnvWithExperimentalHooks_AppendsWhenMissing(t *testing.T) {
	in := []string{"PATH=/usr/bin"}
	out := envWithExperimentalHooks(in)
	if !hasEnv(out, "VIBE_ENABLE_EXPERIMENTAL_HOOKS=true") {
		t.Errorf("env did not append: %v", out)
	}
}

func hasEnv(env []string, want string) bool {
	return slices.Contains(env, want)
}
