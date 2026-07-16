package sigil

import "testing"

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
