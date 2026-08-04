package agento11y

import (
	"encoding/json"
	"strings"
	"testing"
	"unsafe"
)

func TestCloneGenerationDetachesCallerStorage(t *testing.T) {
	backing := "prefix:" + strings.Repeat("retained-value", 32) + ":suffix"
	value := backing[len("prefix:") : len(backing)-len(":suffix")]
	toolChoice := value
	bytesBacking := []byte(`prefix:{"type":"object"}:suffix`)
	schema := bytesBacking[len("prefix:") : len(bytesBacking)-len(":suffix")]

	cloned := cloneGeneration(Generation{
		Model:        ModelRef{Provider: value, Name: value},
		SystemPrompt: value,
		Input: []Message{{
			Name: value,
			Parts: []Part{{
				Kind: PartKindText,
				Text: value,
			}},
		}},
		Tools:      []ToolDefinition{{Name: value, Description: value, InputSchema: schema}},
		ToolChoice: &toolChoice,
		Tags:       map[string]string{"tag": value},
		Metadata:   map[string]any{value: value},
		Artifacts:  []Artifact{{Name: value, Payload: schema}},
	})

	assertStringDetached(t, cloned.Model.Name, value)
	assertStringDetached(t, cloned.SystemPrompt, value)
	assertStringDetached(t, cloned.Input[0].Name, value)
	assertStringDetached(t, cloned.Input[0].Parts[0].Text, value)
	assertStringDetached(t, cloned.Tools[0].Name, value)
	assertStringDetached(t, *cloned.ToolChoice, value)
	assertStringDetached(t, cloned.Tags["tag"], value)
	var metadataKey string
	for key := range cloned.Metadata {
		metadataKey = key
	}
	assertStringDetached(t, metadataKey, value)
	assertStringDetached(t, cloned.Metadata[metadataKey].(string), value)
	assertStringDetached(t, cloned.Artifacts[0].Name, value)

	bytesBacking[len("prefix:")] = '!'
	if !json.Valid(cloned.Tools[0].InputSchema) {
		t.Fatal("tool schema still shares caller byte storage")
	}
	if !json.Valid(cloned.Artifacts[0].Payload) {
		t.Fatal("artifact payload still shares caller byte storage")
	}
}

func TestMergeMetadataDetachesOverrideStrings(t *testing.T) {
	keyBacking := "prefix:" + strings.Repeat("metadata-key", 32) + ":suffix"
	key := keyBacking[len("prefix:") : len(keyBacking)-len(":suffix")]
	valueBacking := "prefix:" + strings.Repeat("metadata-value", 32) + ":suffix"
	value := valueBacking[len("prefix:") : len(valueBacking)-len(":suffix")]

	merged := mergeMetadata(nil, map[string]any{key: value})
	var mergedKey string
	for candidate := range merged {
		mergedKey = candidate
	}

	assertStringDetached(t, mergedKey, key)
	assertStringDetached(t, merged[mergedKey].(string), value)
}

func assertStringDetached(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("cloned string = %q, want %q", got, want)
	}
	if got != "" && unsafe.StringData(got) == unsafe.StringData(want) {
		t.Fatal("cloned string still shares caller backing storage")
	}
}

func TestCloneGenerationClonesMediaParts(t *testing.T) {
	original := Generation{
		Input: []Message{
			{
				Role: RoleUser,
				Parts: []Part{
					MediaPart(Media{
						Kind:     "image",
						URL:      "data:image/png;base64,abc123",
						MIMEType: "image/png",
						Name:     "weather-map.png",
					}),
				},
			},
		},
	}

	cloned := cloneGeneration(original)

	if cloned.Input[0].Parts[0].Media == nil {
		t.Fatal("expected cloned media part to keep media payload")
	}
	if got := cloned.Input[0].Parts[0].Media.URL; got != "data:image/png;base64,abc123" {
		t.Fatalf("unexpected cloned media URL: %q", got)
	}

	original.Input[0].Parts[0].Media.URL = "changed"
	if got := cloned.Input[0].Parts[0].Media.URL; got != "data:image/png;base64,abc123" {
		t.Fatalf("cloned media changed after mutating original: %q", got)
	}
}
