package agento11y

import (
	"strings"
	"testing"
)

func TestValidateGenerationRolePartCompatibility(t *testing.T) {
	base := Generation{
		Model: ModelRef{
			Provider: "anthropic",
			Name:     "claude-sonnet-4-5",
		},
		Input: []Message{
			{
				Role:  RoleAssistant,
				Parts: []Part{TextPart("ok")},
			},
		},
	}

	t.Run("tool call only assistant", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleUser,
			Parts: []Part{
				ToolCallPart(ToolCall{Name: "weather"}),
			},
		})

		if err := ValidateGeneration(g); err == nil {
			t.Fatalf("expected validation error")
		}
	})

	t.Run("tool result only tool", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleAssistant,
			Parts: []Part{
				ToolResultPart(ToolResult{ToolCallID: "toolu_1", Content: "sunny"}),
			},
		})

		if err := ValidateGeneration(g); err == nil {
			t.Fatalf("expected validation error")
		}
	})

	t.Run("tool result requires correlation key", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleTool,
			Parts: []Part{
				ToolResultPart(ToolResult{Content: "sunny"}),
			},
		})

		err := ValidateGeneration(g)
		if err == nil {
			t.Fatalf("expected validation error")
		}
		if !strings.Contains(err.Error(), "tool_result.tool_call_id or name is required") {
			t.Fatalf("expected correlation validation error, got %q", err.Error())
		}
	})

	t.Run("tool result allows name fallback without tool call id", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleTool,
			Parts: []Part{
				ToolResultPart(ToolResult{Name: "weather", Content: "sunny"}),
			},
		})

		if err := ValidateGeneration(g); err != nil {
			t.Fatalf("expected valid generation, got %v", err)
		}
	})

	t.Run("thinking only assistant", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleUser,
			Parts: []Part{
				ThinkingPart("private reasoning"),
			},
		})

		if err := ValidateGeneration(g); err == nil {
			t.Fatalf("expected validation error")
		}
	})

	t.Run("media requires url when content is captured", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleUser,
			Parts: []Part{
				MediaPart(Media{Kind: "image", MIMEType: "image/png"}),
			},
		})

		err := ValidateGeneration(g)
		if err == nil {
			t.Fatalf("expected validation error")
		}
		if !strings.Contains(err.Error(), "media.url is required") {
			t.Fatalf("expected media URL validation error, got %q", err.Error())
		}
	})

	t.Run("media is valid for user and assistant messages", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Input = append(g.Input, Message{
			Role: RoleUser,
			Parts: []Part{
				MediaPart(Media{Kind: "image", URL: "data:image/png;base64,abc123", MIMEType: "image/png"}),
			},
		})
		g.Output = []Message{
			{
				Role: RoleAssistant,
				Parts: []Part{
					MediaPart(Media{Kind: "image", URL: "data:image/png;base64,def456", MIMEType: "image/png"}),
				},
			},
		}

		if err := ValidateGeneration(g); err != nil {
			t.Fatalf("expected valid generation, got %v", err)
		}
	})

	t.Run("output path is reported", func(t *testing.T) {
		g := cloneGeneration(base)
		g.Output = []Message{
			{
				Role:  RoleUser,
				Parts: []Part{ThinkingPart("private reasoning")},
			},
		}

		err := ValidateGeneration(g)
		if err == nil {
			t.Fatalf("expected validation error")
		}
		if !strings.Contains(err.Error(), "generation.output[0]") {
			t.Fatalf("expected output validation path, got %q", err.Error())
		}
	})
}

func TestValidateGenerationAllowsConversationAndResponseFields(t *testing.T) {
	g := Generation{
		ConversationID: "conv-1",
		Model: ModelRef{
			Provider: "anthropic",
			Name:     "claude-sonnet-4-5",
		},
		ResponseID:    "resp-1",
		ResponseModel: "claude-sonnet-4-5-20260201",
		Input: []Message{
			{
				Role:  RoleUser,
				Parts: []Part{TextPart("hello")},
			},
		},
		Output: []Message{
			{
				Role:  RoleAssistant,
				Parts: []Part{TextPart("hi")},
			},
		},
	}

	if err := ValidateGeneration(g); err != nil {
		t.Fatalf("expected valid generation, got %v", err)
	}
}

func TestValidateStrippedGeneration(t *testing.T) {
	base := Generation{
		Model:    ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
		Metadata: map[string]any{metadataKeyContentCaptureMode: contentCaptureModeValueMetaOnly},
	}

	t.Run("stripped text and thinking parts are valid", func(t *testing.T) {
		g := base
		g.Input = []Message{{Role: RoleUser, Parts: []Part{{Kind: PartKindText}}}}
		g.Output = []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindThinking}}}}
		if err := ValidateGeneration(g); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("nil ToolCall still fails when stripped", func(t *testing.T) {
		g := base
		g.Output = []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindToolCall}}}}
		err := ValidateGeneration(g)
		if err == nil {
			t.Fatal("expected error for nil ToolCall even when stripped")
		}
		if !strings.Contains(err.Error(), "must set exactly one payload field") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("nil ToolResult still fails when stripped", func(t *testing.T) {
		g := base
		g.Input = []Message{{Role: RoleTool, Parts: []Part{{Kind: PartKindToolResult}}}}
		err := ValidateGeneration(g)
		if err == nil {
			t.Fatal("expected error for nil ToolResult even when stripped")
		}
		if !strings.Contains(err.Error(), "must set exactly one payload field") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("stripped media parts are valid without url", func(t *testing.T) {
		g := base
		g.Input = []Message{{Role: RoleUser, Parts: []Part{MediaPart(Media{Kind: "image", MIMEType: "image/png"})}}}
		if err := ValidateGeneration(g); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})
}

func TestValidateGenerationAllowsWhitespaceOnlyTextAndThinking(t *testing.T) {
	g := Generation{
		Model: ModelRef{
			Provider: "anthropic",
			Name:     "claude-sonnet-4-5",
		},
		Input: []Message{
			{
				Role:  RoleUser,
				Parts: []Part{TextPart("   ")},
			},
		},
		Output: []Message{
			{
				Role:  RoleAssistant,
				Parts: []Part{ThinkingPart(" \n\t ")},
			},
		},
	}

	if err := ValidateGeneration(g); err != nil {
		t.Fatalf("expected valid generation, got %v", err)
	}
}
