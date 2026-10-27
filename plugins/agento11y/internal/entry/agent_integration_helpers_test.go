package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateCodexToolResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		callID  string
		content any
		isError bool
		wantErr string
	}{
		{name: "object result", callID: fakeToolCallID, content: []byte(`{"output":"hello-from-fake\n"}`)},
		{name: "string result", callID: fakeToolCallID, content: []byte(`"hello-from-fake\n"`)},
		{name: "missing content", callID: fakeToolCallID, wantErr: "missing or invalid content_json"},
		{name: "invalid content", callID: fakeToolCallID, content: []byte(fakeToolOutput), wantErr: "missing or invalid content_json"},
		{name: "invalid base64", callID: fakeToolCallID, content: "%", wantErr: "decode Codex generation"},
		{name: "wrong output", callID: fakeToolCallID, content: []byte(`{"output":"wrong"}`), wantErr: "want \"hello-from-fake\""},
		{name: "null content", callID: fakeToolCallID, content: []byte(`null`), wantErr: "want \"hello-from-fake\""},
		{name: "failed result", callID: fakeToolCallID, content: []byte(`"hello-from-fake"`), isError: true, wantErr: "reports an error"},
		{name: "unrelated result", callID: "another-call", content: []byte(`"hello-from-fake"`), wantErr: "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			generation, err := json.Marshal(map[string]any{
				"input": []any{map[string]any{"parts": []any{
					map[string]any{"text": fakeToolOutput},
					map[string]any{"tool_result": map[string]any{
						"tool_call_id": tc.callID,
						"content_json": tc.content,
						"is_error":     tc.isError,
					}},
				}}},
				"output": []any{map[string]any{"parts": []any{
					map[string]any{"tool_call": map[string]any{
						"id": fakeToolCallID, "input_json": []byte(`{"cmd":"echo hello-from-fake"}`),
					}},
				}}},
			})
			require.NoError(t, err)
			err = validateCodexToolResult([]goldenExport{{Generations: []json.RawMessage{generation}}})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestNewAgentCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		exitCode int
		cancel   bool
	}{
		{name: "success", role: "success"},
		{name: "failure", role: "failure", exitCode: 7},
		{name: "cancel descendants", role: "launcher", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			argv := []string{os.Args[0], "-test.run=^TestAgentCommandProcess$", "--", tc.role}
			var listener *net.TCPListener
			if tc.cancel {
				var err error
				listener, err = net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				require.NoError(t, listener.SetDeadline(time.Now().Add(10*time.Second)))
				argv = append(argv, listener.Addr().String())
			}
			cmd := newAgentCommand(ctx, t.TempDir(), argv)
			cmd.WaitDelay = 100 * time.Millisecond
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				cancel()
				if cmd.ProcessState == nil {
					_ = cmd.Wait()
				}
			})
			var connection net.Conn
			if tc.cancel {
				var err error
				connection, err = listener.Accept()
				require.NoError(t, err)
				t.Cleanup(func() { _ = connection.Close() })
				cancel()
			}
			err := cmd.Wait()
			switch {
			case tc.cancel:
				require.Error(t, err, "output: %s", output.String())
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, err = connection.Read(make([]byte, 1))
				require.Error(t, err, "descendant connection must close after cancellation")
				require.False(t, os.IsTimeout(err), "descendant connection remained open after cancellation")
			case tc.exitCode != 0:
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, "output: %s", output.String())
				require.Equal(t, tc.exitCode, exitErr.ExitCode())
			default:
				require.NoError(t, err, "output: %s", output.String())
			}
		})
	}
}

func TestAgentCommandProcess(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	switch args[0] {
	case "success":
		os.Exit(0)
	case "failure":
		os.Exit(7)
	case "launcher":
		cmd := exec.Command(os.Args[0], "-test.run=^TestAgentCommandProcess$", "--", "child", args[1])
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		require.NoError(t, cmd.Run())
	case "child":
		connection, err := net.DialTimeout("tcp4", args[1], 5*time.Second)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, connection)
		_ = connection.Close()
	default:
		t.Fatalf("unknown helper role %q", args[0])
	}
}

func TestLoopbackServerCleanup(t *testing.T) {
	var server *httptest.Server
	t.Cleanup(func() {
		if server != nil {
			server.Close()
		}
	})
	t.Run("server", func(t *testing.T) {
		server = newLoopbackServer(t, http.NotFoundHandler())
	})
	if server == nil {
		t.Skip("loopback listen unavailable")
	}
	connection, err := net.DialTimeout("tcp4", server.Listener.Addr().String(), time.Second)
	if err == nil {
		_ = connection.Close()
		t.Fatal("loopback listener survived its test cleanup")
	}
}
