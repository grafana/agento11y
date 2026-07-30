package main

import "testing"

func TestExtractCandidates(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		wantPrompt string
		wantLabel  string
		wantErr    bool
	}{
		{
			name:       "raw JSON",
			response:   `{"prompts":[{"prompt":[{"role":"system","content":"raw prompt"}],"improvement_focus":"rules"}]}`,
			wantPrompt: "raw prompt",
			wantLabel:  "rules",
		},
		{
			name:       "fenced prose wrapper",
			response:   "Here is the result:\n```json\n{\"prompts\":[{\"prompt\":[{\"role\":\"system\",\"content\":\"fenced prompt\"}]}]}\n```\nDone.",
			wantPrompt: "fenced prompt",
			wantLabel:  "candidate 1",
		},
		{
			name:       "skips unrelated JSON object",
			response:   "For example, {\"temperature\":0.2}. Use this proposal: {\"prompts\":[{\"prompt\":[{\"role\":\"system\",\"content\":\"usable prompt\"}]}]}",
			wantPrompt: "usable prompt",
			wantLabel:  "candidate 1",
		},
		{
			name:     "malformed response",
			response: `not JSON {"prompts":[`,
			wantErr:  true,
		},
		{
			name:       "user role fallback",
			response:   `{"prompts":[{"prompt":[{"role":"user","content":"fallback prompt"}],"improvement_focus":"fallback"}]}`,
			wantPrompt: "fallback prompt",
			wantLabel:  "fallback",
		},
		{
			name:       "system role preference",
			response:   `{"prompts":[{"prompt":[{"role":"user","content":"first prompt"},{"role":"system","content":"preferred prompt"}]}]}`,
			wantPrompt: "preferred prompt",
			wantLabel:  "candidate 1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates, err := extractCandidates(test.response)
			if test.wantErr {
				if err == nil {
					t.Fatal("extractCandidates() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("extractCandidates() error = %v", err)
			}
			if len(candidates) != 1 {
				t.Fatalf("extractCandidates() count = %d, want 1", len(candidates))
			}
			if candidates[0].SystemPrompt != test.wantPrompt {
				t.Errorf("candidate prompt = %q, want %q", candidates[0].SystemPrompt, test.wantPrompt)
			}
			if candidates[0].Label != test.wantLabel {
				t.Errorf("candidate label = %q, want %q", candidates[0].Label, test.wantLabel)
			}
		})
	}
}

func TestPromptVersion(t *testing.T) {
	candidate := promptCandidate{SystemPrompt: "hello"}
	const want = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := candidate.promptVersion(); got != want {
		t.Errorf("promptVersion() = %q, want %q", got, want)
	}
}
