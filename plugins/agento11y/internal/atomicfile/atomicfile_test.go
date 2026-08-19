package atomicfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteIfChangedCreatesFileAndParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	wrote, err := WriteIfChanged(path, []byte("{}\n"), 0o644)
	require.NoError(t, err)
	assert.True(t, wrote)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.True(t, dir.IsDir(), "the missing parent must be created")
}

func TestWriteIfChangedSkipsMatchingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))

	wrote, err := WriteIfChanged(path, []byte("same"), 0o644)
	require.NoError(t, err)
	assert.False(t, wrote)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"a skipped write must not touch the file at all")
}

func TestWriteIfChangedAppliesModeToReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	wrote, err := WriteIfChanged(path, []byte("new"), 0o600)
	require.NoError(t, err)
	assert.True(t, wrote)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteIfChangedLeavesNoTempFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	wrote, err := WriteIfChanged(path, []byte("new"), 0o644)
	require.Error(t, err)
	assert.False(t, wrote)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file may survive the failure")
	assert.Equal(t, "config.json", entries[0].Name())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old", string(data), "the original file must be untouched")
}
