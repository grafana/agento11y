package vibe

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
)

func TestHookTypesForVersion(t *testing.T) {
	// Real `vibe --version` output is "<prog> <version>". Anything we cannot
	// read a major.minor out of reports ok=false and falls back to the current
	// set, which the caller logs.
	tests := []struct {
		name   string
		out    string
		want   hookTypeSet
		wantOK bool
	}{
		{name: "the release that renamed the types", out: "vibe 2.21.0\n", want: currentHookTypes, wantOK: true},
		{name: "later release", out: "vibe 2.24.2\n", want: currentHookTypes, wantOK: true},
		{name: "next major", out: "vibe 3.0.0\n", want: currentHookTypes, wantOK: true},
		{name: "last release before the rename", out: "vibe 2.20.0\n", want: preRenameHookTypes, wantOK: true},
		{name: "older minor", out: "vibe 2.15.0\n", want: preRenameHookTypes, wantOK: true},
		{name: "older major", out: "vibe 1.3.5\n", want: preRenameHookTypes, wantOK: true},
		// Only major.minor is compared, so a suffix rides along. A 2.21
		// pre-release is treated as renamed: it is closer to 2.21.0 than to
		// 2.20.x, and guessing current is the documented fallback anyway.
		{name: "pre-release of the rename", out: "vibe 2.21.0rc1\n", want: currentHookTypes, wantOK: true},
		{name: "dev build before the rename", out: "vibe 2.20.1.dev0+g1234abc\n", want: preRenameHookTypes, wantOK: true},
		// argparse prints the executable's own name, which is not always
		// "vibe".
		{name: "renamed executable", out: "vibe-acp 2.20.0\n", want: preRenameHookTypes, wantOK: true},
		{name: "no version in output", out: "vibe\n", want: currentHookTypes},
		{name: "major only", out: "vibe 2\n", want: currentHookTypes},
		{name: "empty output", out: "", want: currentHookTypes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := hookTypesForVersion(tt.out)
			if got != tt.want {
				t.Errorf("types = %+v, want %+v", got, tt.want)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestHookTypesFor(t *testing.T) {
	// Every path that cannot answer has to land on the current types, because
	// a pre-2.21.0 vibe is the rarer case: its hooks also needed
	// VIBE_ENABLE_EXPERIMENTAL_HOOKS, so its users opted in by hand.
	tests := []struct {
		name    string
		onPath  bool
		out     string
		err     error
		want    hookTypeSet
		wantLog string
	}{
		{name: "reads the installed version", onPath: true, out: "vibe 2.20.0\n", want: preRenameHookTypes},
		{name: "reads a current version", onPath: true, out: "vibe 2.24.2\n", want: currentHookTypes},
		// doctor probes install state with no vibe installed at all.
		{name: "vibe not on PATH", want: currentHookTypes},
		{name: "probe fails", onPath: true, err: errors.New("exit status 1"), want: currentHookTypes, wantLog: "exit status 1"},
		{name: "unparsable output", onPath: true, out: "vibe (unknown)\n", want: currentHookTypes, wantLog: "cannot parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origLookPath, origOutput := lookPath, vibeVersionOutput
			t.Cleanup(func() { lookPath, vibeVersionOutput = origLookPath, origOutput })
			lookPath = func(string) (string, error) {
				if !tt.onPath {
					return "", errors.New("not found")
				}
				return "/fake/vibe", nil
			}
			vibeVersionOutput = func(context.Context, string) ([]byte, error) {
				return []byte(tt.out), tt.err
			}

			var logged bytes.Buffer
			got := hookTypesFor(context.Background(), log.New(&logged, "", 0))
			if got != tt.want {
				t.Errorf("types = %+v, want %+v", got, tt.want)
			}
			if tt.wantLog == "" {
				if logged.Len() != 0 {
					t.Errorf("logged %q, want nothing", logged.String())
				}
			} else if !strings.Contains(logged.String(), tt.wantLog) {
				t.Errorf("logged %q, want it to mention %q", logged.String(), tt.wantLog)
			}
		})
	}
}

func TestHookTypesFor_ToleratesNilLogger(t *testing.T) {
	// Status has no logger to pass, and its probe still has to report.
	origLookPath, origOutput := lookPath, vibeVersionOutput
	t.Cleanup(func() { lookPath, vibeVersionOutput = origLookPath, origOutput })
	lookPath = func(string) (string, error) { return "/fake/vibe", nil }
	vibeVersionOutput = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("boom")
	}

	if got := hookTypesFor(context.Background(), nil); got != currentHookTypes {
		t.Errorf("types = %+v, want %+v", got, currentHookTypes)
	}
}
