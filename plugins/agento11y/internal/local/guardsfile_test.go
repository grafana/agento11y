package local

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file the daemon cannot parse must not be rewritten from an empty ruleset
// when the Local tab toggles a pack. Custom rules live only in that file.
func TestServer_Guards_MalformedFilePutDoesNotRewrite(t *testing.T) {
	original := "[[rules]\nrule_id = "
	s, guardsPath, _ := newGuardsServer(t)
	require.NoError(t, os.WriteFile(guardsPath, []byte(original), 0o600))

	resp := doReq(t, s, http.MethodPut, "/api/v1/guards", `{"packs":{"destructive":true}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	got, err := os.ReadFile(guardsPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
}

func TestServer_Guards_DisableSucceedsWhenFileMalformed(t *testing.T) {
	original := "[[rules]\nrule_id = "
	s, guardsPath, configPath := newGuardsServer(t)
	require.NoError(t, os.WriteFile(guardsPath, []byte(original), 0o600))
	require.NoError(t, os.WriteFile(configPath, []byte("AGENTO11Y_GUARDS_ENABLED=true\nSIGIL_GUARDS_ENABLED=true\n"), 0o600))

	resp := doReq(t, s, http.MethodPut, "/api/v1/guards", `{"enabled":false}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got guardsFileResponse
	decodeJSON(t, resp.Body, &got)
	assert.False(t, got.Enabled)

	env := dotenv.LoadDotenv(configPath, nil)
	assert.Equal(t, "false", env["AGENTO11Y_GUARDS_ENABLED"])
	assert.Equal(t, "false", env["SIGIL_GUARDS_ENABLED"])

	data, err := os.ReadFile(guardsPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

func TestServer_Guards_PackTogglePreservesCustomExtraFields(t *testing.T) {
	original := `
[[rules]]
rule_id = "block.rm"
phase = "postflight"
action_on_fail = "deny"
short_circuit = true
evaluator_ids = ["ev-1"]
tool_filter.blocked_names = ["Bash(*rm -rf*)"]
`
	s, guardsPath, _ := newGuardsServer(t)
	require.NoError(t, os.WriteFile(guardsPath, []byte(original), 0o600))

	resp := doReq(t, s, http.MethodPut, "/api/v1/guards", `{"packs":{"git":true}}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := os.ReadFile(guardsPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "short_circuit = true")
	assert.Contains(t, string(got), `evaluator_ids = ["ev-1"]`)
	assert.Contains(t, string(got), `rule_id = "block.rm"`)
	assert.Contains(t, string(got), `rule_id = "pack.git"`)
}

func TestServer_Guards_MalformedPackPutLeavesEnabledUnchanged(t *testing.T) {
	s, guardsPath, configPath := newGuardsServer(t)
	require.NoError(t, os.WriteFile(guardsPath, []byte("[[rules]\nrule_id = "), 0o600))
	require.NoError(t, os.WriteFile(configPath, []byte("AGENTO11Y_GUARDS_ENABLED=true\nSIGIL_GUARDS_ENABLED=true\n"), 0o600))

	resp := doReq(t, s, http.MethodPut, "/api/v1/guards", `{"packs":{"git":true}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	env := dotenv.LoadDotenv(configPath, nil)
	assert.Equal(t, "true", env["AGENTO11Y_GUARDS_ENABLED"])
}

func TestServer_Guards_ConcurrentUpdates(t *testing.T) {
	for _, tc := range []struct {
		name, first, second         string
		firstEnabled, secondEnabled bool
		firstCount, secondCount     int
	}{
		{"disable_then_enable", `{"enabled":false}`, `{"enabled":true,"packs":{"git":true}}`, false, true, 0, 1},
		{"enable_then_disable", `{"enabled":true,"packs":{"git":true}}`, `{"enabled":false}`, true, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newGuardsServer(t)
			await := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("guard/config request did not finish")
				}
			}
			first := &pausedGuardsResponse{ResponseRecorder: httptest.NewRecorder(), reached: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(first.release) })
			firstDone := make(chan struct{})
			firstReq := newLocalRequest(http.MethodPut, "/api/v1/guards", strings.NewReader(tc.first))
			firstReq.Header.Set("Content-Type", "application/json")
			go func() { s.ServeHTTP(first, firstReq); close(firstDone) }()
			await(first.reached)
			if s.configMu.TryLock() {
				s.configMu.Unlock()
				t.Error("guard PUT released configMu before its response")
			}
			if s.guardsMu.TryLock() {
				s.guardsMu.Unlock()
				t.Error("guard PUT released guardsMu before its response")
			}

			second := httptest.NewRecorder()
			secondRead := make(chan struct{})
			secondDone := make(chan struct{})
			secondReq := newLocalRequest(http.MethodPut, "/api/v1/guards", &guardsBodyReadSignal{Reader: strings.NewReader(tc.second), done: secondRead})
			secondReq.Header.Set("Content-Type", "application/json")
			go func() { s.ServeHTTP(second, secondReq); close(secondDone) }()
			patch := httptest.NewRecorder()
			patchRead := make(chan struct{})
			patchDone := make(chan struct{})
			patchReq := newLocalRequest(http.MethodPatch, "/api/v1/config", &guardsBodyReadSignal{Reader: strings.NewReader(`{"theme":"dark"}`), done: patchRead})
			patchReq.Header.Set("Content-Type", "application/json")
			go func() { s.ServeHTTP(patch, patchReq); close(patchDone) }()
			await(secondRead)
			await(patchRead)
			release.Do(func() { close(first.release) })
			await(firstDone)
			await(secondDone)
			await(patchDone)

			for _, result := range []struct {
				rr      *httptest.ResponseRecorder
				enabled bool
				count   int
			}{{first.ResponseRecorder, tc.firstEnabled, tc.firstCount}, {second, tc.secondEnabled, tc.secondCount}} {
				require.Equal(t, http.StatusOK, result.rr.Code, result.rr.Body.String())
				var got guardsFileResponse
				require.NoError(t, json.Unmarshal(result.rr.Body.Bytes(), &got))
				assert.Equal(t, result.enabled, got.Enabled)
				assert.Equal(t, result.count, got.Enforcing)
				assert.Len(t, got.Rules, result.count)
			}
			require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
			var cfg configResponse
			require.NoError(t, json.Unmarshal(patch.Body.Bytes(), &cfg))
			if cfg.LocalGuards.Enabled {
				assert.Equal(t, 1, cfg.LocalGuards.Rules)
			} else {
				assert.Zero(t, cfg.LocalGuards.Rules)
			}
			final := doReq(t, s, http.MethodGet, "/api/v1/guards", "")
			defer final.Body.Close()
			var got guardsFileResponse
			decodeJSON(t, final.Body, &got)
			assert.Equal(t, tc.secondEnabled, got.Enabled)
			assert.Equal(t, tc.secondCount, got.Enforcing)
			assert.Len(t, got.Rules, tc.secondCount)
		})
	}
}

type pausedGuardsResponse struct {
	*httptest.ResponseRecorder
	reached, release chan struct{}
}

func (w *pausedGuardsResponse) WriteHeader(status int) {
	close(w.reached)
	<-w.release
	w.ResponseRecorder.WriteHeader(status)
}

type guardsBodyReadSignal struct {
	io.Reader
	done chan struct{}
	once sync.Once
}

func (r *guardsBodyReadSignal) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.once.Do(func() { close(r.done) })
	}
	return n, err
}

func TestServer_Guards_PutResponseUsesWrittenSnapshot(t *testing.T) {
	rules, err := applyPackUpdates(nil, map[string]bool{"git": true})
	require.NoError(t, err)
	data, err := guardeval.EncodeRules(rules)
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		contents []byte
	}{
		{"concurrent_external_edit", []byte("")},
		{"followup_read_failure", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path, _ := newGuardsServer(t)
			if tc.contents != nil {
				require.NoError(t, os.WriteFile(path, tc.contents, 0o600))
			} else {
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			rr := httptest.NewRecorder()
			s.writeGuardsPutOKLocked(rr, rules, data)
			require.Equal(t, http.StatusOK, rr.Code)
			var got guardsFileResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
			assert.Equal(t, 1, got.Enforcing)
			require.Len(t, got.Rules, 1)
			assert.Equal(t, "pack.git", got.Rules[0].RuleID)
		})
	}
}

func TestServer_Guards_ConfigWriteFailureIsNotSuccess(t *testing.T) {
	s, _, configPath := newGuardsServer(t)
	require.NoError(t, os.Mkdir(configPath, 0o700))
	resp := doReq(t, s, http.MethodPut, "/api/v1/guards", `{"enabled":true,"packs":{"git":true}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "write config:")
}

func TestServer_Guards_PackWriteFailureLeavesEnabledUnchanged(t *testing.T) {
	s, guardsPath, configPath := newGuardsServer(t)
	require.NoError(t, os.WriteFile(configPath, []byte("AGENTO11Y_GUARDS_ENABLED=true\nSIGIL_GUARDS_ENABLED=true\n"), 0o600))
	require.NoError(t, os.RemoveAll(guardsPath))
	require.NoError(t, os.Mkdir(guardsPath, 0o700))

	resp := doReq(t, s, http.MethodPut, "/api/v1/guards", `{"enabled":false}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	env := dotenv.LoadDotenv(configPath, nil)
	assert.Equal(t, "true", env["AGENTO11Y_GUARDS_ENABLED"])
}
