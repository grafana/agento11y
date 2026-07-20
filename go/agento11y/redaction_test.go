package agento11y

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
)

func TestSecretRedactionSanitizerRedactsAssistantAndToolOutputByDefault(t *testing.T) {
	client, _, _ := newTestClient(t, Config{
		GenerationSanitizer: NewSecretRedactionSanitizer(SecretRedactionOptions{}),
	})

	secretToken := "glc_abcdefghijklmnopqrstuvwxyz1234"
	envSecret := "DATABASE_PASSWORD=hunter2secret123"
	bearerToken := strings.Repeat("a", 30)
	historicBearer := strings.Repeat("h", 30)
	historicEnv := "API_TOKEN=historicvalue9876"

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{
		Input: []Message{
			{Role: RoleUser, Parts: []Part{{Kind: PartKindText, Text: "user pasted " + secretToken}}},
			{Role: RoleAssistant, Parts: []Part{
				{Kind: PartKindText, Text: "previous turn mentioned " + secretToken},
				{Kind: PartKindToolCall, ToolCall: &ToolCall{
					ID:        "prev-call",
					Name:      "bash",
					InputJSON: json.RawMessage(`{"header":"Bearer ` + historicBearer + `"}`),
				}},
			}},
			{Role: RoleTool, Parts: []Part{
				{Kind: PartKindToolResult, ToolResult: &ToolResult{
					ToolCallID: "prev-call",
					Name:       "bash",
					Content:    "previous output " + historicEnv,
				}},
			}},
		},
		Output: []Message{
			{Role: RoleAssistant, Parts: []Part{
				{Kind: PartKindText, Text: "assistant saw " + secretToken},
				{Kind: PartKindThinking, Thinking: "thinking about " + secretToken},
				{Kind: PartKindToolCall, ToolCall: &ToolCall{
					ID:        "call-1",
					Name:      "bash",
					InputJSON: json.RawMessage(`{"header":"Bearer ` + bearerToken + `","env":"` + envSecret + `"}`),
				}},
			}},
			{Role: RoleTool, Parts: []Part{
				{Kind: PartKindToolResult, ToolResult: &ToolResult{
					ToolCallID: "call-1",
					Name:       "bash",
					Content:    "output " + envSecret,
				}},
			}},
		},
		Usage: TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, nil)
	rec.End()

	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}

	gen := rec.lastGeneration
	if !strings.Contains(gen.Input[0].Parts[0].Text, "glc_") {
		t.Errorf("user input was redacted; expected unchanged. got %q", gen.Input[0].Parts[0].Text)
	}
	if strings.Contains(gen.Input[1].Parts[0].Text, "glc_") {
		t.Errorf("historic assistant text not redacted: %q", gen.Input[1].Parts[0].Text)
	}
	historicToolCall := string(gen.Input[1].Parts[1].ToolCall.InputJSON)
	if strings.Contains(historicToolCall, "Bearer "+historicBearer) {
		t.Errorf("historic tool-call bearer not redacted: %q", historicToolCall)
	}
	historicToolResult := gen.Input[2].Parts[0].ToolResult.Content
	if strings.Contains(historicToolResult, "historicvalue9876") {
		t.Errorf("historic tool-result env secret not redacted: %q", historicToolResult)
	}
	if strings.Contains(gen.Output[0].Parts[0].Text, "glc_") {
		t.Errorf("assistant text not redacted: %q", gen.Output[0].Parts[0].Text)
	}
	if !strings.Contains(gen.Output[0].Parts[0].Text, "[REDACTED:grafana-cloud-token]") {
		t.Errorf("assistant text missing redaction marker: %q", gen.Output[0].Parts[0].Text)
	}
	if strings.Contains(gen.Output[0].Parts[1].Thinking, "glc_") {
		t.Errorf("thinking not redacted: %q", gen.Output[0].Parts[1].Thinking)
	}
	toolCallInput := string(gen.Output[0].Parts[2].ToolCall.InputJSON)
	if strings.Contains(toolCallInput, "hunter2secret123") {
		t.Errorf("tool-call env secret not redacted: %q", toolCallInput)
	}
	if strings.Contains(toolCallInput, "Bearer "+bearerToken) {
		t.Errorf("tool-call bearer not redacted: %q", toolCallInput)
	}
	if !strings.Contains(toolCallInput, "[REDACTED:") {
		t.Errorf("tool-call missing redaction marker: %q", toolCallInput)
	}
	toolResult := gen.Output[1].Parts[0].ToolResult.Content
	if strings.Contains(toolResult, "hunter2secret123") {
		t.Errorf("tool-result env secret not redacted: %q", toolResult)
	}
	if !strings.Contains(toolResult, "[REDACTED:env-secret-value]") {
		t.Errorf("tool-result missing env-secret-value marker: %q", toolResult)
	}
}

func TestSecretRedactionSanitizerEmailToggle(t *testing.T) {
	const text = "Send me an email to example@example.com"

	cases := []struct {
		name         string
		opts         SecretRedactionOptions
		wantMarker   bool
		wantPreserve bool
	}{
		{name: "default redacts email", opts: SecretRedactionOptions{}, wantMarker: true},
		{name: "disable preserves email", opts: SecretRedactionOptions{RedactEmailAddresses: boolPtr(false)}, wantPreserve: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sanitizer := NewSecretRedactionSanitizer(tc.opts)
			got := sanitizer(Generation{
				Output: []Message{{
					Role:  RoleAssistant,
					Parts: []Part{{Kind: PartKindText, Text: text}},
				}},
			}).Output[0].Parts[0].Text

			if tc.wantPreserve {
				if got != text {
					t.Errorf("email should be preserved, got %q", got)
				}
				return
			}
			if strings.Contains(got, "example@example.com") {
				t.Errorf("email not redacted: %q", got)
			}
			if tc.wantMarker && !strings.Contains(got, "[REDACTED:email]") {
				t.Errorf("email marker missing: %q", got)
			}
		})
	}
}

func TestSecretRedactionSanitizerInputRedactionByRole(t *testing.T) {
	secretToken := "glc_abcdefghijklmnopqrstuvwxyz1234"
	envSecret := "DATABASE_PASSWORD=hunter2secret123"
	bearerToken := strings.Repeat("a", 30)

	build := func() Generation {
		return Generation{
			ID:    "gen-1",
			Mode:  GenerationModeSync,
			Model: ModelRef{Provider: "openai", Name: "gpt-5"},
			Input: []Message{
				{Role: RoleUser, Parts: []Part{{Kind: PartKindText, Text: "user pasted " + secretToken}}},
				{Role: RoleAssistant, Parts: []Part{
					{Kind: PartKindText, Text: "assistant response with " + secretToken},
					{Kind: PartKindToolCall, ToolCall: &ToolCall{
						ID:        "call-1",
						Name:      "bash",
						InputJSON: json.RawMessage(`{"header":"Bearer ` + bearerToken + `"}`),
					}},
				}},
				{Role: RoleTool, Parts: []Part{
					{Kind: PartKindToolResult, ToolResult: &ToolResult{
						ToolCallID: "call-1",
						Name:       "bash",
						Content:    "output " + envSecret,
					}},
				}},
			},
		}
	}

	cases := []struct {
		name             string
		opts             SecretRedactionOptions
		env              map[string]string
		wantUserRedacted bool
	}{
		{name: "default preserves user only", opts: SecretRedactionOptions{}, wantUserRedacted: false},
		{name: "opt-in redacts user too", opts: SecretRedactionOptions{RedactInputMessages: boolPtr(true)}, wantUserRedacted: true},
		{name: "env enables when option nil", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "true"}, wantUserRedacted: true},
		{name: "explicit false beats env true", opts: SecretRedactionOptions{RedactInputMessages: boolPtr(false)}, env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "true"}, wantUserRedacted: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sanitized := newSecretRedactionSanitizer(mapLookup(tc.env), tc.opts)(build())

			userText := sanitized.Input[0].Parts[0].Text
			if tc.wantUserRedacted {
				if strings.Contains(userText, secretToken) {
					t.Errorf("user input not redacted: %q", userText)
				}
				if !strings.Contains(userText, "[REDACTED:grafana-cloud-token]") {
					t.Errorf("user input missing marker: %q", userText)
				}
			} else if !strings.Contains(userText, secretToken) {
				t.Errorf("user input should be unchanged, got %q", userText)
			}

			assistantText := sanitized.Input[1].Parts[0].Text
			if strings.Contains(assistantText, secretToken) {
				t.Errorf("assistant text not redacted: %q", assistantText)
			}
			toolCall := string(sanitized.Input[1].Parts[1].ToolCall.InputJSON)
			if strings.Contains(toolCall, "Bearer "+bearerToken) {
				t.Errorf("assistant tool-call bearer not redacted: %q", toolCall)
			}
			toolResult := sanitized.Input[2].Parts[0].ToolResult.Content
			if strings.Contains(toolResult, "hunter2secret123") {
				t.Errorf("tool result env secret not redacted: %q", toolResult)
			}
		})
	}
}

func TestSecretRedactionPatterns(t *testing.T) {
	sanitizer := NewSecretRedactionSanitizer(SecretRedactionOptions{})

	cases := []struct {
		id    string
		value string
	}{
		{"grafana-cloud-token", "glc_abcdefghijklmnopqrstuvwxyz1234"},
		{"grafana-service-account-token", "glsa_abcdefghijklmnopqrstuvwxyz1234"},
		{"aws-access-token", "AKIAIOSFODNN7EXAMPLE"},
		{"github-pat", "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"github-oauth", "gho_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"github-app-token", "ghs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"github-fine-grained-pat", "github_pat_" + strings.Repeat("a", 82)},
		{"anthropic-api-key", "sk-ant-api03-" + strings.Repeat("a", 93) + "AA"},
		{"anthropic-admin-key", "sk-ant-admin01-" + strings.Repeat("a", 93) + "AA"},
		{"openai-api-key", "sk-" + strings.Repeat("a", 20) + "T3BlbkFJ" + strings.Repeat("b", 20)},
		{"openai-project-key", "sk-proj-" + strings.Repeat("a", 40)},
		{"openai-svcacct-key", "sk-svcacct-" + strings.Repeat("a", 40)},
		{"gcp-api-key", "AIza" + strings.Repeat("a", 35)},
		{"private-key", "-----BEGIN RSA PRIVATE KEY-----\nfake-test-body\n-----END RSA PRIVATE KEY-----"},
		{"connection-string", "postgres://user:password@db.example.com:5432/app"},
		{"bearer-token", "Bearer " + strings.Repeat("a", 30)},
		{"slack-token", "xoxb-" + strings.Repeat("a", 20)},
		{"stripe-key", "sk_live_" + strings.Repeat("a", 24)},
		{"sendgrid-api-key", "SG." + strings.Repeat("a", 22) + "." + strings.Repeat("b", 43)},
		{"twilio-api-key", "SK" + strings.Repeat("a", 32)},
		{"npm-token", "npm_" + strings.Repeat("a", 36)},
		{"pypi-token", "pypi-" + strings.Repeat("a", 50)},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			input := "prefix " + tc.value + " suffix"
			gen := sanitizer(Generation{
				ID:    "gen-1",
				Mode:  GenerationModeSync,
				Model: ModelRef{Provider: "openai", Name: "gpt-5"},
				Output: []Message{{
					Role:  RoleTool,
					Parts: []Part{{Kind: PartKindToolResult, ToolResult: &ToolResult{Content: input}}},
				}},
			})

			got := gen.Output[0].Parts[0].ToolResult.Content
			marker := "[REDACTED:" + tc.id + "]"
			if !strings.Contains(got, marker) {
				t.Errorf("missing marker %q in %q", marker, got)
			}
			if strings.Contains(got, tc.value) {
				t.Errorf("raw value %q still present in %q", tc.value, got)
			}
		})
	}
}

func TestGenerationSanitizerPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	titleSecret := "glc_abcdefghijklmnopqrstuvwxyz1234"

	client, recorder, _ := newTestClient(t, Config{
		Logger: logger,
		GenerationSanitizer: func(_ Generation) Generation {
			panic("boom")
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:             ModelRef{Provider: "openai", Name: "gpt-5"},
		ConversationTitle: "title with " + titleSecret,
		SystemPrompt:      "system secret",
	})
	rec.SetResult(Generation{
		Input:  []Message{{Role: RoleUser, Parts: []Part{{Kind: PartKindText, Text: "hello"}}}},
		Output: []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindText, Text: "world"}}}},
		Usage:  TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, nil)
	rec.End()

	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}
	gen := rec.lastGeneration
	span := onlyGenerationSpan(t, recorder.Ended())
	spanAttrs := spanAttributeMap(span)

	stripped := []struct {
		name  string
		value string
	}{
		{"SystemPrompt", gen.SystemPrompt},
		{"ConversationTitle", gen.ConversationTitle},
		{"Input text", gen.Input[0].Parts[0].Text},
		{"Output text", gen.Output[0].Parts[0].Text},
	}
	for _, tc := range stripped {
		t.Run("stripped "+tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Errorf("%s should be stripped, got %q", tc.name, tc.value)
			}
		})
	}

	t.Run("content capture mode flagged metadata_only", func(t *testing.T) {
		if v, _ := gen.Metadata[metadataKeyContentCaptureMode].(string); v != contentCaptureModeValueMetaOnly {
			t.Errorf("got %q", v)
		}
	})
	t.Run("logger captures fallback warning", func(t *testing.T) {
		if !strings.Contains(buf.String(), "agento11y: generation sanitization failed, falling back to metadata_only") {
			t.Errorf("missing warning: %q", buf.String())
		}
	})
	t.Run("span conversation title does not leak", func(t *testing.T) {
		if title := spanAttrs[spanAttrConversationTitle].AsString(); strings.Contains(title, titleSecret) {
			t.Errorf("title attr leaks secret: %q", title)
		}
	})
}

func TestSecretRedactionSanitizerRedactsTitleAndCallErrorAcrossSinks(t *testing.T) {
	secret := "glc_abcdefghijklmnopqrstuvwxyz1234"
	client, recorder, _ := newTestClient(t, Config{
		GenerationSanitizer: NewSecretRedactionSanitizer(SecretRedactionOptions{}),
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:             ModelRef{Provider: "openai", Name: "gpt-5"},
		ConversationTitle: "title with " + secret,
	})
	rec.SetCallError(errors.New("api failure: " + secret))
	rec.End()

	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}
	gen := rec.lastGeneration
	span := onlyGenerationSpan(t, recorder.Ended())
	spanAttrs := spanAttributeMap(span)

	cases := []struct {
		name  string
		value string
	}{
		{"canonical ConversationTitle", gen.ConversationTitle},
		{"canonical CallError", gen.CallError},
		{"metadata " + spanAttrConversationTitle, metaString(gen, spanAttrConversationTitle)},
		{"metadata call_error", metaString(gen, "call_error")},
		{"span attr " + spanAttrConversationTitle, spanAttrs[spanAttrConversationTitle].AsString()},
		{"span status description", span.Status().Description},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.value, secret) {
				t.Errorf("sink leaks secret: %q", tc.value)
			}
		})
	}

	t.Run("span events", func(t *testing.T) {
		for _, ev := range span.Events() {
			for _, attr := range ev.Attributes {
				if strings.Contains(attr.Value.Emit(), secret) {
					t.Errorf("event %q attr %s leaks secret: %q", ev.Name, attr.Key, attr.Value.Emit())
				}
			}
		}
	})

	t.Run("mirrors match canonical", func(t *testing.T) {
		if got := metaString(gen, spanAttrConversationTitle); got != gen.ConversationTitle {
			t.Errorf("title mirror %q != canonical %q", got, gen.ConversationTitle)
		}
		if got := metaString(gen, "call_error"); got != gen.CallError {
			t.Errorf("call_error mirror %q != canonical %q", got, gen.CallError)
		}
	})
}

func TestGenerationSanitizerClearingCallErrorDoesNotLeak(t *testing.T) {
	secret := "glc_abcdefghijklmnopqrstuvwxyz1234"
	client, recorder, _ := newTestClient(t, Config{
		GenerationSanitizer: func(g Generation) Generation {
			g.CallError = ""
			return g
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetCallError(errors.New("api failure: " + secret))
	rec.End()

	span := onlyGenerationSpan(t, recorder.Ended())
	if got := span.Status().Description; strings.Contains(got, secret) {
		t.Errorf("span status description leaks secret: %q", got)
	}
	for _, ev := range span.Events() {
		for _, attr := range ev.Attributes {
			if strings.Contains(attr.Value.Emit(), secret) {
				t.Errorf("event %q attr %s leaks secret: %q", ev.Name, attr.Key, attr.Value.Emit())
			}
		}
	}
}

func TestGenerationSanitizerClearingTitleDoesNotLeak(t *testing.T) {
	secret := "glc_abcdefghijklmnopqrstuvwxyz1234"
	client, recorder, _ := newTestClient(t, Config{
		GenerationSanitizer: func(g Generation) Generation {
			g.ConversationTitle = ""
			return g
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:             ModelRef{Provider: "openai", Name: "gpt-5"},
		ConversationTitle: "title with " + secret,
	})
	rec.SetResult(Generation{
		Output: []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindText, Text: "ok"}}}},
		Usage:  TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, nil)
	rec.End()

	span := onlyGenerationSpan(t, recorder.Ended())
	if got := spanAttributeMap(span)[spanAttrConversationTitle].AsString(); strings.Contains(got, secret) {
		t.Errorf("title span attr leaks secret: %q", got)
	}
}

func TestGenerationSanitizerUnderFullWithMetadataSpansOmitsSpanTitle(t *testing.T) {
	var calls int
	client, recorder, _ := newTestClient(t, Config{
		ContentCapture: ContentCaptureModeFullWithMetadataSpans,
		GenerationSanitizer: func(g Generation) Generation {
			calls++
			return g
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:             ModelRef{Provider: "openai", Name: "gpt-5"},
		ConversationTitle: "Sensitive title",
	})
	rec.SetResult(Generation{
		Output: []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindText, Text: "ok"}}}},
		Usage:  TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, nil)
	rec.End()

	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected sanitizer to run once, got %d", calls)
	}
	span := onlyGenerationSpan(t, recorder.Ended())
	if _, ok := spanAttributeMap(span)[spanAttrConversationTitle]; ok {
		t.Errorf("expected %q to be absent under FullWithMetadataSpans even when sanitizer runs", spanAttrConversationTitle)
	}
}

func TestGenerationSanitizerPanicUnderFullWithMetadataSpansOmitsSpanTitle(t *testing.T) {
	var buf bytes.Buffer
	client, recorder, _ := newTestClient(t, Config{
		Logger:         log.New(&buf, "", 0),
		ContentCapture: ContentCaptureModeFullWithMetadataSpans,
		GenerationSanitizer: func(_ Generation) Generation {
			panic("boom")
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:             ModelRef{Provider: "openai", Name: "gpt-5"},
		ConversationTitle: "Sensitive title",
	})
	rec.SetResult(Generation{
		Output: []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindText, Text: "ok"}}}},
		Usage:  TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, nil)
	rec.End()

	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}
	span := onlyGenerationSpan(t, recorder.Ended())
	if _, ok := spanAttributeMap(span)[spanAttrConversationTitle]; ok {
		t.Errorf("expected %q to be absent under FullWithMetadataSpans when sanitizer panics", spanAttrConversationTitle)
	}
}

func TestGenerationSanitizerSkippedInMetadataOnlyMode(t *testing.T) {
	var calls int
	client, _, _ := newTestClient(t, Config{
		ContentCapture: ContentCaptureModeMetadataOnly,
		GenerationSanitizer: func(g Generation) Generation {
			calls++
			return g
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{
		Output: []Message{{Role: RoleAssistant, Parts: []Part{{Kind: PartKindText, Text: "ok"}}}},
		Usage:  TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, nil)
	rec.End()

	if calls != 0 {
		t.Errorf("sanitizer should be skipped in metadata-only mode; got %d calls", calls)
	}
}

func metaString(g Generation, key string) string {
	v, _ := g.Metadata[key].(string)
	return v
}

func TestResolveRedactInputMessages(t *testing.T) {
	cases := []struct {
		name     string
		explicit *bool
		env      map[string]string
		want     bool
	}{
		{name: "nil and unset defaults to false", want: false},
		{name: "explicit true wins over unset env", explicit: boolPtr(true), want: true},
		{name: "explicit false wins over env true", explicit: boolPtr(false), env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "true"}, want: false},
		{name: "explicit true wins over env false", explicit: boolPtr(true), env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "false"}, want: true},
		{name: "env true when option nil", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "true"}, want: true},
		{name: "env false when option nil", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "false"}, want: false},
		{name: "env 1 parses true", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "1"}, want: true},
		{name: "env ON case-insensitive", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "ON"}, want: true},
		{name: "env yes parses true", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "yes"}, want: true},
		{name: "env off parses false", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "off"}, want: false},
		{name: "blank env falls back to false", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "   "}, want: false},
		{name: "invalid env falls back to false", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "maybe"}, want: false},
		{name: "preferred env true when option nil", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "true"}, want: true},
		{name: "preferred false wins over legacy true", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "false", "SIGIL_REDACT_INPUT_MESSAGES": "true"}, want: false},
		{name: "blank preferred falls through to legacy true", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "   ", "SIGIL_REDACT_INPUT_MESSAGES": "true"}, want: true},
		{name: "invalid preferred blocks valid legacy fallback", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "maybe", "SIGIL_REDACT_INPUT_MESSAGES": "true"}, want: false},
		{name: "explicit false wins over preferred true", explicit: boolPtr(false), env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "true"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRedactInputMessages(mapLookup(tc.env), tc.explicit); got != tc.want {
				t.Errorf("resolveRedactInputMessages = %v, want %v", got, tc.want)
			}
		})
	}
}
