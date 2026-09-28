package local

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyToolPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{path: "main.go", want: "go"},
		{path: "src/app.tsx", want: "typescript"},
		{path: "pkg/app.py", want: "python"},
		{path: "src/Main.java", want: "java"},
		{path: "scripts/build.sh", want: "shell"},
		{path: "web/app.css", want: "css"},
		{path: "README.md", want: "documentation"},
		{path: "LICENSE", want: "documentation"},
		{path: "package.json", want: "configuration"},
		{path: "config/app.yaml", want: "configuration"},
		{path: "Dockerfile", want: "configuration"},
		{path: "go.mod", want: "configuration"},
		{path: "requirements.txt", want: "configuration"},
		{path: ".env.local", want: "configuration"},
		{path: "src/", want: ""},
		{path: "src", want: ""},
		{path: "plugins/agento11y", want: ""},
		{path: "**/*.go", want: ""},
		{path: "https://example.com/main.go", want: ""},
		{path: "notes.bin", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyToolPath(normalizeToolPath(tc.path)))
		})
	}
}

func TestFilePathsFromToolInput(t *testing.T) {
	t.Run("reads nested edit paths and ignores file contents", func(t *testing.T) {
		raw := json.RawMessage(`{"file_path":"main.go","old_string":"file_path: ignored.py","edits":[{"path":"web/app.tsx"},{"path":"main.go"}]}`)
		assert.ElementsMatch(t, []string{"main.go", "web/app.tsx"}, filePathsFromToolInput("Edit", raw))
	})

	t.Run("decodes protobuf base64 input", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString([]byte(`{"file_path":"lib/main.py"}`))
		raw, err := json.Marshal(encoded)
		require.NoError(t, err)
		assert.Equal(t, []string{"lib/main.py"}, filePathsFromToolInput("Read", raw))
	})

	t.Run("drops grep and glob search roots", func(t *testing.T) {
		grep, err := json.Marshal(map[string]any{"path": "plugins/agento11y", "pattern": "foo"})
		require.NoError(t, err)
		assert.Empty(t, filePathsFromToolInput("Grep", grep))
		glob, err := json.Marshal(map[string]any{"path": "src", "glob": "*.go"})
		require.NoError(t, err)
		assert.Empty(t, filePathsFromToolInput("Glob", glob))

		file, err := json.Marshal(map[string]any{"path": "src/main.go"})
		require.NoError(t, err)
		assert.Equal(t, []string{"src/main.go"}, filePathsFromToolInput("Grep", file))
	})

	t.Run("reads codex apply_patch markers", func(t *testing.T) {
		patch := "*** Begin Patch\n*** Update File: cmd/main.go\n@@\n-old\n+new\n*** Add File: docs/README.md\n+hello\n*** End Patch"
		raw, err := json.Marshal(map[string]any{"command": patch})
		require.NoError(t, err)
		assert.Equal(t, []string{"cmd/main.go", "docs/README.md"}, filePathsFromToolInput("apply_patch", raw))
		assert.Empty(t, filePathsFromToolInput("Bash", raw))
	})
}

func TestSplitInt64LargestRemainder(t *testing.T) {
	assert.Equal(t, []int64{51, 50}, splitInt64(101, []int{1, 1}))
	assert.Equal(t, []int64{2, 1, 0}, splitInt64(3, []int{2, 1, 0}))
	assert.Equal(t, []int64{0, 0}, splitInt64(0, []int{1, 1}))
}

func TestLanguageMixAllocatesByDistinctFiles(t *testing.T) {
	s := newStorage(t)
	goInput, err := json.Marshal(map[string]any{"file_path": "main.go", "old_string": "package main"})
	require.NoError(t, err)
	pyInput, err := json.Marshal(map[string]any{"path": "app.py"})
	require.NoError(t, err)
	docInput, err := json.Marshal(map[string]any{"file_path": "README.md"})
	require.NoError(t, err)
	unknownInput, err := json.Marshal(map[string]any{"file_path": "blob.bin"})
	require.NoError(t, err)
	rustInput, err := json.Marshal(map[string]any{"file_path": "lib.rs"})
	require.NoError(t, err)

	writeGen(t, s, "conv-mix", "outside", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "costly"},
		StartedAt: mustParse(t, "2026-05-21T09:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 1000},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Read", InputJSON: rustInput}},
		}}},
	}, "2026-05-21T09:00:00Z")
	writeGen(t, s, "conv-mix", "inside", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "costly"},
		StartedAt: mustParse(t, "2026-05-21T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 101, OutputTokens: 10},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Read", InputJSON: goInput}},
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Read", InputJSON: goInput}},
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Write", InputJSON: pyInput}},
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Read", InputJSON: unknownInput}},
		}}},
	}, "2026-05-21T10:00:00Z")
	writeGen(t, s, "conv-doc", "doc", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "costly"},
		StartedAt: mustParse(t, "2026-05-21T10:10:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 40},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Read", InputJSON: docInput}},
		}}},
	}, "2026-05-21T10:10:00Z")
	writeGen(t, s, "conv-bare", "bare", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "costly"},
		StartedAt: mustParse(t, "2026-05-21T10:20:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 7},
	}, "2026-05-21T10:20:00Z")

	_, _, aggregate, err := s.ConversationMetrics(ConversationListOptions{
		Since:  mustParse(t, "2026-05-21T10:00:00Z"),
		Before: mustParse(t, "2026-05-21T11:00:00Z"),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, aggregate.LanguageSessions)
	byID := map[string]LanguageAggregate{}
	for _, row := range aggregate.LanguageRows {
		byID[row.ID] = row
	}
	require.Len(t, byID, 3)
	assert.Equal(t, LanguageAggregate{
		ID:       "go",
		Name:     "Go",
		Sessions: 1,
		Files:    1,
		TokenBuckets: TokenBuckets{
			FreshInput: 34,
			Output:     4,
		},
		TokenBucketsByModel: map[string]TokenBuckets{
			"costly": {FreshInput: 34, Output: 4},
		},
	}, byID["go"])
	assert.Equal(t, LanguageAggregate{
		ID:       "python",
		Name:     "Python",
		Sessions: 1,
		Files:    1,
		TokenBuckets: TokenBuckets{
			FreshInput: 34,
			Output:     3,
		},
		TokenBucketsByModel: map[string]TokenBuckets{
			"costly": {FreshInput: 34, Output: 3},
		},
	}, byID["python"])
	assert.Equal(t, 1, byID["documentation"].Sessions)
	assert.Equal(t, int64(40), byID["documentation"].TokenBuckets.FreshInput)
	assert.Equal(t, LanguageAggregate{
		ID:       languageSharedID,
		Name:     languageSharedName,
		Sessions: 2,
		Files:    1,
		TokenBuckets: TokenBuckets{
			FreshInput: 40,
			Output:     3,
		},
		TokenBucketsByModel: map[string]TokenBuckets{
			"costly": {FreshInput: 40, Output: 3},
		},
	}, aggregate.LanguageShared)

	var fresh, output int64
	for _, row := range aggregate.LanguageRows {
		fresh += row.TokenBuckets.FreshInput
		output += row.TokenBuckets.Output
	}
	fresh += aggregate.LanguageShared.TokenBuckets.FreshInput
	output += aggregate.LanguageShared.TokenBuckets.Output
	assert.Equal(t, aggregate.TokenBuckets.FreshInput, fresh)
	assert.Equal(t, aggregate.TokenBuckets.Output, output)
	assert.NotContains(t, byID, "rust")

	rows, _, _, err := s.ConversationMetrics(ConversationListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	encodedRow, err := json.Marshal(rows[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encodedRow), "language_files")
	encoded, err := json.Marshal(aggregate)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"language_rows"`)
}
