package login

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

// TestParseGcxStacks pins what login reads out of `gcx config list-contexts
// -o json`: the current context leads, hosts repeated across contexts appear
// once, and everything unusable — a signed-out gcx, a server login could not
// parse, output from a version that changed the shape — resolves to no stacks
// so the prompt falls back to its own question.
func TestParseGcxStacks(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "current context first",
			out: `{"contexts":[
				{"current":false,"name":"alpha","server":"https://alpha.grafana.net"},
				{"current":true,"name":"beta","server":"https://beta.grafana.net"},
				{"current":false,"name":"gamma","server":"https://gamma.grafana.net"}]}`,
			want: []string{"https://beta.grafana.net", "https://alpha.grafana.net", "https://gamma.grafana.net"},
		},
		{
			name: "gcx order kept when the current context is already first",
			out: `{"contexts":[
				{"current":true,"name":"alpha","server":"https://alpha.grafana.net"},
				{"current":false,"name":"beta","server":"https://beta.grafana.net"}]}`,
			want: []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
		},
		{
			name: "no current context",
			out: `{"contexts":[
				{"current":false,"name":"alpha","server":"https://alpha.grafana.net"}]}`,
			want: []string{"https://alpha.grafana.net"},
		},
		{
			// A token context and an OAuth context for one stack are two gcx
			// contexts and one Grafana host.
			name: "one host shared by several contexts",
			out: `{"contexts":[
				{"current":false,"name":"prod-token","server":"https://alpha.grafana.net"},
				{"current":true,"name":"prod-oauth","server":"https://alpha.grafana.net"},
				{"current":false,"name":"dev","server":"https://beta.grafana-dev.net"}]}`,
			want: []string{"https://alpha.grafana.net", "https://beta.grafana-dev.net"},
		},
		{
			name: "host case and trailing slash normalised",
			out:  `{"contexts":[{"current":false,"name":"alpha","server":"https://Alpha.Grafana.NET/"}]}`,
			want: []string{"https://alpha.grafana.net"},
		},
		{
			// A Grafana on this machine is not a Grafana Cloud URL, so it is not
			// suggested. Typing one still works: only this list drops it.
			name: "a local Grafana is dropped",
			out:  `{"contexts":[{"current":true,"name":"local","server":"http://localhost:3000"}]}`,
			want: nil,
		},
		{
			name: "every spelling of this machine is dropped",
			out: `{"contexts":[
				{"current":false,"name":"a","server":"http://localhost:3000"},
				{"current":false,"name":"b","server":"http://127.0.0.1:3000"},
				{"current":false,"name":"c","server":"http://[::1]:3000"},
				{"current":false,"name":"d","server":"http://LocalHost:3000"},
				{"current":true,"name":"cloud","server":"https://alpha.grafana.net"}]}`,
			want: []string{"https://alpha.grafana.net"},
		},
		{
			// The dropped context was the current one, so the rest keep gcx's order.
			name: "a local Grafana as the current context",
			out: `{"contexts":[
				{"current":false,"name":"alpha","server":"https://alpha.grafana.net"},
				{"current":true,"name":"local","server":"http://localhost:3000"},
				{"current":false,"name":"beta","server":"https://beta.grafana.net"}]}`,
			want: []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
		},
		{
			// What a gcx that was never logged in prints.
			name: "no stack configured: a default context with no server",
			out:  `{"contexts":[{"current":true,"name":"default"}]}`,
			want: nil,
		},
		{
			name: "explicit null server",
			out:  `{"contexts":[{"current":true,"name":"default","server":null}]}`,
			want: nil,
		},
		{
			name: "server login cannot parse is skipped",
			out: `{"contexts":[
				{"current":false,"name":"broken","server":"::not a url::"},
				{"current":true,"name":"alpha","server":"https://alpha.grafana.net"}]}`,
			want: []string{"https://alpha.grafana.net"},
		},
		{"empty context list", `{"contexts":[]}`, nil},
		{"no contexts key", `{}`, nil},
		{"not JSON", "CURRENT  NAME  GRAFANA SERVER\n", nil},
		{"empty output", "", nil},
		{
			// gcx reports a failure as JSON on stdout with a non-zero exit, so
			// readGcxStacks drops it before this. Pin the shape anyway.
			name: "gcx error document",
			out:  `{"type":"gcx.error","error":{"summary":"File not found","exitCode":1}}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseGcxStacks([]byte(tc.out)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseGcxStacks() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadGcxStacksWithoutGcx pins that a machine with no gcx on PATH reports
// no stacks instead of running anything.
func TestReadGcxStacksWithoutGcx(t *testing.T) {
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	called := ""
	lookPath = func(file string) (string, error) {
		called = file
		return "", exec.ErrNotFound
	}

	if got := readGcxStacks(context.Background()); got != nil {
		t.Errorf("readGcxStacks() = %v, want nil", got)
	}
	if called != gcxBinary {
		t.Errorf("looked up %q, want %q", called, gcxBinary)
	}
}

// TestReadGcxStacksIgnoresAFailingGcx pins that a gcx which exits non-zero
// (an unreadable --config, a version without the subcommand) reports no stacks
// rather than an error the caller would have to handle: the stack question
// works without the list.
func TestReadGcxStacksIgnoresAFailingGcx(t *testing.T) {
	bin, err := exec.LookPath("false")
	if err != nil {
		t.Skipf("no false binary: %v", err)
	}
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	lookPath = func(string) (string, error) { return bin, nil }

	if got := readGcxStacks(context.Background()); got != nil {
		t.Errorf("readGcxStacks() = %v, want nil", got)
	}
}

// TestReadGcxStacksHonoursACancelledContext pins that the lookup does not
// outlive the login run it belongs to.
func TestReadGcxStacksHonoursACancelledContext(t *testing.T) {
	bin, err := exec.LookPath("echo")
	if err != nil {
		t.Skipf("no echo binary: %v", err)
	}
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	lookPath = func(string) (string, error) { return bin, nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := readGcxStacks(ctx); got != nil {
		t.Errorf("readGcxStacks() = %v, want nil", got)
	}
}

// TestGcxStacksDefaultsToTheReader pins that the package var the prompt calls
// is wired to the real lookup, so a test stubbing it cannot hide a production
// path that was never connected.
func TestGcxStacksDefaultsToTheReader(t *testing.T) {
	if gcxStacks == nil {
		t.Fatal("gcxStacks is nil")
	}
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	sentinel := errors.New("lookPath called")
	called := false
	lookPath = func(string) (string, error) {
		called = true
		return "", sentinel
	}

	gcxStacks(context.Background())

	if !called {
		t.Error("gcxStacks did not reach readGcxStacks")
	}
}
