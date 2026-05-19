package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLatestAssistantTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	contents := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess-1\",\"copilotVersion\":\"1.0.48\"}}\n" +
		"{\"type\":\"session.model_change\",\"data\":{\"newModel\":\"gpt-5.4\",\"reasoningEffort\":\"medium\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"hello world\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"gpt-5.4\",\"content\":\"assistant answer\",\"interactionId\":\"int-1\",\"turnId\":\"4\",\"outputTokens\":12,\"requestId\":\"req-1\"}}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	got, ok, err := ReadLatestAssistantTurn(path)
	if err != nil {
		t.Fatalf("ReadLatestAssistantTurn: %v", err)
	}
	if !ok {
		t.Fatal("expected transcript snapshot")
	}
	if got.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q", got.SessionID)
	}
	if got.CopilotVersion != "1.0.48" {
		t.Fatalf("CopilotVersion = %q", got.CopilotVersion)
	}
	if got.Model != "gpt-5.4" {
		t.Fatalf("Model = %q", got.Model)
	}
	if got.ReasoningEffort != "medium" {
		t.Fatalf("ReasoningEffort = %q", got.ReasoningEffort)
	}
	if got.NativeTurnID != "4" {
		t.Fatalf("NativeTurnID = %q", got.NativeTurnID)
	}
	if got.InteractionID != "int-1" {
		t.Fatalf("InteractionID = %q", got.InteractionID)
	}
	if got.RequestID != "req-1" {
		t.Fatalf("RequestID = %q", got.RequestID)
	}
	if got.MessageID != "msg-1" {
		t.Fatalf("MessageID = %q", got.MessageID)
	}
	if got.AssistantText != "assistant answer" {
		t.Fatalf("AssistantText = %q", got.AssistantText)
	}
	if got.UserPrompt != "hello world" {
		t.Fatalf("UserPrompt = %q", got.UserPrompt)
	}
	if got.OutputTokens == nil || *got.OutputTokens != 12 {
		t.Fatalf("OutputTokens = %+v", got.OutputTokens)
	}
}

func TestReadAssistantTurnDoesNotReusePreviousTranscriptTurnWhenPromptChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	contents := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess-1\",\"copilotVersion\":\"1.0.49\"}}\n" +
		"{\"type\":\"session.model_change\",\"data\":{\"newModel\":\"auto\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"first prompt\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"claude-sonnet-4.6\",\"content\":\"first answer\",\"interactionId\":\"int-1\",\"turnId\":\"0\",\"outputTokens\":621,\"requestId\":\"req-1\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"second prompt\",\"interactionId\":\"int-2\"}}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	got, ok, err := ReadAssistantTurn(path, ReadHint{UserPrompt: "second prompt"})
	if err != nil {
		t.Fatalf("ReadAssistantTurn before second reply: %v", err)
	}
	if ok {
		t.Fatalf("got snapshot before second reply: %+v", got)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	_, _ = f.WriteString("{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-2\",\"model\":\"gpt-4.1\",\"content\":\"second answer\",\"interactionId\":\"int-2\",\"turnId\":\"0\",\"outputTokens\":123,\"requestId\":\"req-2\"}}\n")
	_ = f.Close()

	got, ok, err = ReadAssistantTurn(path, ReadHint{UserPrompt: "second prompt"})
	if err != nil {
		t.Fatalf("ReadAssistantTurn after second reply: %v", err)
	}
	if !ok {
		t.Fatal("expected transcript snapshot after second reply")
	}
	if got.Model != "gpt-4.1" {
		t.Fatalf("Model = %q", got.Model)
	}
	if got.RequestID != "req-2" {
		t.Fatalf("RequestID = %q", got.RequestID)
	}
	if got.AssistantText != "second answer" {
		t.Fatalf("AssistantText = %q", got.AssistantText)
	}
	if got.OutputTokens == nil || *got.OutputTokens != 123 {
		t.Fatalf("OutputTokens = %+v", got.OutputTokens)
	}
}

func TestReadAssistantTurnDoesNotReusePreviousTranscriptTurnWhenPromptRepeats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	contents := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess-1\",\"copilotVersion\":\"1.0.49\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"same prompt\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"claude-sonnet-4.6\",\"content\":\"first answer\",\"interactionId\":\"int-1\",\"turnId\":\"0\",\"outputTokens\":621,\"requestId\":\"req-1\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"same prompt\",\"interactionId\":\"int-2\"}}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	got, ok, err := ReadAssistantTurn(path, ReadHint{UserPrompt: "same prompt"})
	if err != nil {
		t.Fatalf("ReadAssistantTurn before repeated reply: %v", err)
	}
	if ok {
		t.Fatalf("got snapshot before repeated reply: %+v", got)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	_, _ = f.WriteString("{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-2\",\"model\":\"gpt-4.1\",\"content\":\"second answer\",\"interactionId\":\"int-2\",\"turnId\":\"0\",\"outputTokens\":123,\"requestId\":\"req-2\"}}\n")
	_ = f.Close()

	got, ok, err = ReadAssistantTurn(path, ReadHint{UserPrompt: "same prompt"})
	if err != nil {
		t.Fatalf("ReadAssistantTurn after repeated reply: %v", err)
	}
	if !ok {
		t.Fatal("expected transcript snapshot after repeated reply")
	}
	if got.InteractionID != "int-2" {
		t.Fatalf("InteractionID = %q", got.InteractionID)
	}
	if got.Model != "gpt-4.1" {
		t.Fatalf("Model = %q", got.Model)
	}
	if got.RequestID != "req-2" {
		t.Fatalf("RequestID = %q", got.RequestID)
	}
}

func TestReadAssistantTurnDoesNotReusePreviousTranscriptTurnWhenHintPromptNeverAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	contents := "" +
		"{\"type\":\"session.start\",\"data\":{\"sessionId\":\"sess-1\",\"copilotVersion\":\"1.0.49\"}}\n" +
		"{\"type\":\"user.message\",\"data\":{\"content\":\"first prompt\",\"interactionId\":\"int-1\"}}\n" +
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"msg-1\",\"model\":\"claude-sonnet-4.6\",\"content\":\"first answer\",\"interactionId\":\"int-1\",\"turnId\":\"0\",\"outputTokens\":621,\"requestId\":\"req-1\"}}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	got, ok, err := ReadAssistantTurn(path, ReadHint{UserPrompt: "missing prompt"})
	if err != nil {
		t.Fatalf("ReadAssistantTurn missing prompt: %v", err)
	}
	if ok {
		t.Fatalf("got stale snapshot for missing prompt: %+v", got)
	}
}
