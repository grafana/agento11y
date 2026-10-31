package local

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchForwardPostureValidation(t *testing.T) {
	const status = `{"enabled":true,"mode":"metadata_only","generations":true,"otlp":false,"hooks":false}`
	wrap := func(status string) string { return `{"forwardStatus":` + status + `}` }
	cases := []struct{ name, body string }{
		{"missing status", `{}`},
		{"null status", wrap(`null`)},
		{"array status", wrap(`[]`)},
		{"string status", wrap(`"off"`)},
		{"malformed JSON", `{"forwardStatus":`},
		{"trailing JSON", wrap(status) + `{}`},
		{"trailing text", wrap(status) + `secret`},
		{"unknown mode", wrap(strings.Replace(status, `metadata_only`, `unknown`, 1))},
		{"enabled off mode", wrap(strings.Replace(status, `metadata_only`, `off`, 1))},
		{"disabled full mode", wrap(`{"enabled":false,"mode":"full","generations":false,"otlp":false,"hooks":false}`)},
		{"boolean as string", wrap(strings.Replace(status, `"enabled":true`, `"enabled":"true"`, 1))},
		{"destinations object", wrap(strings.TrimSuffix(status, `}`) + `,"destinations":{}}`)},
		{"destinations null", wrap(strings.TrimSuffix(status, `}`) + `,"destinations":null}`)},
		{"incomplete destination", wrap(strings.TrimSuffix(status, `}`) + `,"destinations":[{"name":"extra"}]}`)},
		{"legs array", wrap(strings.TrimSuffix(status, `}`) + `,"legs":[]}`)},
		{"null leg", wrap(strings.TrimSuffix(status, `}`) + `,"legs":{"traces":null}}`)},
		{"bad counter", wrap(strings.TrimSuffix(status, `}`) + `,"legs":{"traces":{"attempted":"1"}}}`)},
		{"incomplete counters", wrap(strings.TrimSuffix(status, `}`) + `,"legs":{"traces":{"attempted":1}}}`)},
		{"failures object", wrap(strings.TrimSuffix(status, `}`) + `,"failures":{}}`)},
		{"incomplete failure", wrap(strings.TrimSuffix(status, `}`) + `,"failures":[{}]}`)},
	}
	for _, key := range []string{"enabled", "mode", "generations", "otlp", "hooks"} {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(status), &fields))
		delete(fields, key)
		data, err := json.Marshal(fields)
		require.NoError(t, err)
		cases = append(cases, struct{ name, body string }{"missing " + key, wrap(string(data))})
		fields[key] = json.RawMessage(`null`)
		data, err = json.Marshal(fields)
		require.NoError(t, err)
		cases = append(cases, struct{ name, body string }{"null " + key, wrap(string(data))})
	}
	const destination = `{"name":"archive","endpoint":"","otlpEndpoint":"https://archive.example","httpGenerations":false,"otlp":true,"revision":1}`
	for _, key := range []string{"name", "endpoint", "otlpEndpoint", "httpGenerations", "otlp", "revision"} {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(destination), &fields))
		delete(fields, key)
		data, err := json.Marshal(fields)
		require.NoError(t, err)
		cases = append(cases, struct{ name, body string }{"destination missing " + key, wrap(strings.TrimSuffix(status, `}`) + `,"destinations":[` + string(data) + `]}`)})
	}
	for _, malformed := range []struct{ name, destination string }{
		{"null destination", `null`},
		{"bad revision", strings.Replace(destination, `"revision":1`, `"revision":"1"`, 1)},
		{"bad endpoint", strings.Replace(destination, `"endpoint":""`, `"endpoint":12`, 1)},
		{"bad destination legs", strings.TrimSuffix(destination, `}`) + `,"legs":{"traces":[]}}`},
		{"destination missing counters", strings.TrimSuffix(destination, `}`) + `,"legs":{"traces":{"lastSuccessAt":"success"}}}`},
		{"bad destination failures", strings.TrimSuffix(destination, `}`) + `,"failures":[null]}`},
	} {
		cases = append(cases, struct{ name, body string }{malformed.name, wrap(strings.TrimSuffix(status, `}`) + `,"destinations":[` + malformed.destination + `]}`)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/config", r.URL.Path)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer ts.Close()
			posture, err := FetchForwardPosture(context.Background(), ts.URL)
			require.Error(t, err)
			assert.Equal(t, ForwardPosture{}, posture, "invalid posture must be unknown, not off")
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestFetchForwardPostureSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		count        int
	}{
		{"legacy off", `{"enabled":false,"mode":"off","generations":false,"otlp":false,"hooks":false}`, 0},
		{"legacy enabled", `{"enabled":true,"mode":"full","generations":true,"otlp":true,"hooks":true,"legs":{"traces":{"lastSuccessAt":"2026-01-01T00:00:00Z"}}}`, 1},
		{"extra only", `{"enabled":true,"mode":"metadata_only","generations":true,"otlp":true,"hooks":false,"destinations":[{"name":"archive","endpoint":"","otlpEndpoint":"https://archive.example/otlp","httpGenerations":false,"otlp":true,"revision":7,"legs":{"traces":{"attempted":4,"succeeded":1,"failed":2,"dropped":1,"lastSuccessAt":"success","lastFailureAt":"failure","lastFailureDetail":"timeout"}},"failures":[{"at":"failure","label":"traces","detail":"timeout","destination":"archive"}]}],"forwardingPath":"/config/forwarding.toml","forwardingError":"invalid extra config","failures":[{"at":"failure","label":"traces","detail":"timeout","destination":"archive"}]}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"forwardStatus":%s,"localGuards":{"rules":2}} `, tc.status)
			}))
			defer ts.Close()
			posture, err := FetchForwardPosture(context.Background(), ts.URL)
			require.NoError(t, err)
			assert.Equal(t, tc.count, posture.DestinationCount)
			assert.Equal(t, 2, posture.LocalRules)
			if tc.name == "extra only" {
				require.Len(t, posture.Destinations, 1)
				assert.False(t, posture.Hooks)
				assert.Empty(t, posture.Legs, "top-level legs describe primary, not extras")
				assert.Equal(t, uint64(7), posture.Destinations[0].Revision)
				assert.Equal(t, uint64(4), posture.Destinations[0].Legs["traces"].Attempted)
				assert.Equal(t, "timeout", posture.Destinations[0].Legs["traces"].LastFailureDetail)
				assert.Equal(t, "archive", posture.Failures[0].Destination)
				assert.Equal(t, "/config/forwarding.toml", posture.ForwardingPath)
				assert.NotEmpty(t, posture.ForwardingError)
			}
		})
	}
}

func TestStop_RemovesStatusWhenProcessGone(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveStatus(dir, Status{PID: 0, Port: 1, Endpoint: "http://127.0.0.1:1"}))
	stopped, err := Stop(dir)
	require.NoError(t, err)
	assert.False(t, stopped)
	_, err = os.Stat(filepath.Join(dir, StatusFile))
	assert.True(t, os.IsNotExist(err), "stale status file should be removed")
}

// TestSaveStatus_ConcurrentReadersSeeWholeFile pins down the atomic write.
// LoadStatus takes no lock, so an in-place write truncates the file under
// readers and they fail with "unexpected end of JSON input" - which is how
// TestEnsureRunning_ConcurrentCallersSpawnOnce used to flake.
func TestSaveStatus_ConcurrentReadersSeeWholeFile(t *testing.T) {
	dir := t.TempDir()
	want := Status{
		PID:       os.Getpid(),
		Port:      8765,
		Endpoint:  "http://127.0.0.1:8765",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	require.NoError(t, SaveStatus(dir, want))

	const writes = 500
	writing := make(chan struct{})
	writeErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(writing)
		for range writes {
			if err := SaveStatus(dir, want); err != nil {
				writeErr <- err
				return
			}
		}
	})

	var readErr error
	partial := 0
	for done := false; !done; {
		select {
		case <-writing:
			done = true
		default:
		}
		s, err := LoadStatus(dir)
		switch {
		case err != nil:
			if readErr == nil {
				readErr = err
			}
		case s == nil || *s != want:
			partial++
		}
	}
	wg.Wait()

	select {
	case err := <-writeErr:
		require.NoError(t, err)
	default:
	}
	require.NoError(t, readErr, "LoadStatus read a status file mid-write")
	assert.Zero(t, partial, "LoadStatus returned a status that was never written")
}

const (
	daemonHelperEnv = "AGENTO11Y_TEST_LOCAL_DAEMON_HELPER"
	daemonHelperDir = "AGENTO11Y_TEST_LOCAL_DAEMON_DIR"
	idleHelperEnv   = "AGENTO11Y_TEST_IDLE_HELPER"
)

func TestLocalDaemonHelperProcess(t *testing.T) {
	if os.Getenv(daemonHelperEnv) != "1" {
		return
	}
	if err := Serve(context.Background(), os.Getenv(daemonHelperDir), 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIdleHelperProcess(t *testing.T) {
	if os.Getenv(idleHelperEnv) != "1" {
		return
	}
	time.Sleep(time.Minute)
}

func TestLocalDaemonLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name           string
		inFlightExport bool
	}{
		{name: "idle"},
		{name: "drains in-flight export", inFlightExport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := copyTestExecutable(t)
			cmd := exec.Command(helper, "-test.run=^TestLocalDaemonHelperProcess$", "local", "serve")
			cmd.Env = append(os.Environ(), daemonHelperEnv+"=1", daemonHelperDir+"="+dir)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			require.NoError(t, cmd.Start())
			trustDaemonHelperProcess(t, cmd.Process.Pid)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			t.Cleanup(func() {
				if waited {
					return
				}
				_ = cmd.Process.Kill()
				_ = <-done
			})

			var first *Status
			require.Eventually(t, func() bool {
				var err error
				first, err = IsRunning(dir)
				return err == nil && first != nil
			}, 5*time.Second, 20*time.Millisecond, "helper daemon published a healthy endpoint")
			require.Equal(t, cmd.Process.Pid, first.PID)

			reused, err := EnsureRunning(context.Background(), dir, nil)
			require.NoError(t, err)
			require.Equal(t, first, reused)

			const body = `{"generations":[{"id":"gen-drain","conversation_id":"conv-drain"}]}`
			var upload net.Conn
			var reader *bufio.Reader
			if tc.inFlightExport {
				host := strings.TrimPrefix(first.Endpoint, "http://")
				upload, err = net.DialTimeout("tcp", host, time.Second)
				require.NoError(t, err)
				defer upload.Close()
				require.NoError(t, upload.SetDeadline(time.Now().Add(10*time.Second)))
				_, err = fmt.Fprintf(upload, "POST /api/v1/generations:export HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", host, len(body))
				require.NoError(t, err)
				reader = bufio.NewReader(upload)
				resp, err := http.ReadResponse(reader, nil)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				// A 100 response means the handler is waiting for the export body.
				require.Equal(t, http.StatusContinue, resp.StatusCode)
			}

			stopDone := make(chan struct{})
			var stopped bool
			var stopErr error
			started := time.Now()
			go func() {
				defer close(stopDone)
				stopped, stopErr = Stop(dir)
			}()
			t.Cleanup(func() { <-stopDone })
			if tc.inFlightExport {
				select {
				case <-stopDone:
					t.Fatal("Stop returned before the in-flight export finished")
				case <-time.After(2 * time.Second):
				}
				_, err = io.WriteString(upload, body)
				require.NoError(t, err)
				resp, err := http.ReadResponse(reader, nil)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				var result generationsResponse
				decodeJSON(t, resp.Body, &result)
				require.Len(t, result.Results, 1)
				require.True(t, result.Results[0].Accepted)
				lines := readLines(t, filepath.Join(dir, ConversationsDir, "conv-drain.jsonl"))
				require.Len(t, lines, 1)
				require.Contains(t, lines[0], `"id":"gen-drain"`)
			}
			<-stopDone
			require.NoError(t, stopErr)
			require.True(t, stopped)
			require.Less(t, time.Since(started), stopTimeout)
			select {
			case err := <-done:
				waited = true
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("helper daemon process did not exit")
			}
			require.False(t, pidAlive(first.PID))
			_, err = os.Stat(filepath.Join(dir, StatusFile))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func copyTestExecutable(t *testing.T) string {
	t.Helper()
	sourcePath, err := os.Executable()
	require.NoError(t, err)
	source, err := os.Open(sourcePath)
	require.NoError(t, err)
	defer func() { _ = source.Close() }()

	name := "agento11y"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	destinationPath := filepath.Join(t.TempDir(), name)
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	require.NoError(t, copyErr)
	require.NoError(t, closeErr)
	return destinationPath
}

// TestListenLocal covers both halves of the port-fallback contract:
// when the preferred port is free we get exactly that port (no kernel
// random); when it's held we bump to the next free slot. Each row
// discovers a port via net.Listen(":0") so we never assume any
// specific port is free on the test host.
func TestListenLocal(t *testing.T) {
	cases := []struct {
		name           string
		holdDuringCall bool // when true, hold preferred during listenLocal
		wantBumped     bool // when true, returned port must be > preferred
	}{
		{name: "preferred free, returns exactly that port", holdDuringCall: false, wantBumped: false},
		{name: "preferred taken, bumps to next free port", holdDuringCall: true, wantBumped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Discover a port we know is free right now by binding to
			// :0, then either hold it (forcing a bump) or release it
			// (expecting an exact match).
			probe, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			preferred := probe.Addr().(*net.TCPAddr).Port
			if tc.holdDuringCall {
				defer probe.Close()
			} else {
				_ = probe.Close()
			}

			listener, err := listenLocal(preferred)
			if err != nil {
				if !tc.holdDuringCall {
					// Tiny race window between Close and the next Listen.
					t.Skipf("port %d was retaken between probe and bind: %v", preferred, err)
				}
				t.Fatalf("listenLocal: %v", err)
			}
			defer listener.Close()
			got := listener.Addr().(*net.TCPAddr).Port

			switch {
			case tc.wantBumped && got == preferred:
				t.Fatalf("got blocked port %d; should have bumped", got)
			case tc.wantBumped && (got <= preferred || got > preferred+maxPortBumps):
				t.Fatalf("got %d, want in (%d, %d]", got, preferred, preferred+maxPortBumps)
			case !tc.wantBumped && got != preferred:
				t.Fatalf("got %d, want preferred %d", got, preferred)
			}
		})
	}
}

// looksLikeTestBinary is what stops startDaemon from re-executing a test binary
// as the daemon, which would fork the suite into more daemons.
func TestLooksLikeTestBinary(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{path: "/var/folders/x/go-build123/b001/entry.test", want: true},
		{path: "/var/folders/x/go-build123/b001/local.test.exe", want: true},
		{path: "/usr/local/bin/agento11y", want: false},
		{path: "/usr/local/bin/agento11y.exe", want: false},
		// `go run` compiles to "main" in the build cache.
		{path: "/Users/x/Library/Caches/go-build/ab/cd-d/main", want: false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := looksLikeTestBinary(tc.path); got != tc.want {
				t.Errorf("looksLikeTestBinary(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestEnsureRunning_ConcurrentCallersSpawnOnce drives several
// EnsureRunning goroutines at the same empty state dir. A healthy
// daemon is simulated by an httptest server so endpointAlive returns
// true after the first SaveStatus; the file lock makes the callers converge
// on the status that the first caller writes.
func TestEnsureRunning_ConcurrentCallersSpawnOnce(t *testing.T) {
	dir := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	colon := strings.LastIndex(host, ":")
	port, _ := strconv.Atoi(host[colon+1:])

	var spawns atomic.Int32
	restore := SetStartDaemonForTesting(func(_ context.Context, dir string, _ *log.Logger) (*Status, error) {
		spawns.Add(1)
		// Yield a moment so the next goroutine has time to wait on the
		// file lock; without this, the first caller may finish before the
		// others even contend.
		time.Sleep(20 * time.Millisecond)
		s := Status{
			PID:       os.Getpid(),
			Port:      port,
			Endpoint:  ts.URL,
			StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		_ = SaveStatus(dir, s)
		return &s, nil
	})
	defer restore()

	const callers = 8
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			if _, err := EnsureRunning(context.Background(), dir, nil); err != nil {
				t.Errorf("EnsureRunning: %v", err)
			}
		})
	}
	wg.Wait()

	if got := spawns.Load(); got != 1 {
		t.Errorf("spawns = %d, want 1 (file lock should serialise EnsureRunning)", got)
	}
}
