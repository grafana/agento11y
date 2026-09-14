package local

import (
	"net/http"
	"os"
	"testing"

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
