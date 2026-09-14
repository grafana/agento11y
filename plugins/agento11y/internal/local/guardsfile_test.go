package local

import (
	"net/http"
	"os"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
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
