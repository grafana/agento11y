package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	maxLineBytes       = 1024 * 1024
	maxTranscriptBytes = 32 * 1024 * 1024
)

type Snapshot struct {
	SessionID       string
	CopilotVersion  string
	Model           string
	ReasoningEffort string
	NativeTurnID    string
	InteractionID   string
	RequestID       string
	MessageID       string
	AssistantText   string
	OutputTokens    *int64
	UserPrompt      string
}

type ReadHint struct {
	TurnID     string
	UserPrompt string
}

type line struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type sessionStartData struct {
	SessionID      string `json:"sessionId"`
	CopilotVersion string `json:"copilotVersion"`
}

type sessionModelChangeData struct {
	NewModel          string `json:"newModel"`
	ReasoningEffort   string `json:"reasoningEffort"`
	PreviousReasoning string `json:"previousReasoningEffort"`
}

type userMessageData struct {
	Content       string `json:"content"`
	InteractionID string `json:"interactionId"`
}

type assistantMessageData struct {
	MessageID     string `json:"messageId"`
	Model         string `json:"model"`
	Content       string `json:"content"`
	InteractionID string `json:"interactionId"`
	TurnID        string `json:"turnId"`
	RequestID     string `json:"requestId"`
	OutputTokens  *int64 `json:"outputTokens"`
}

func ReadLatestAssistantTurn(path string) (Snapshot, bool, error) {
	return ReadAssistantTurn(path, ReadHint{})
}

func ReadAssistantTurn(path string, hint ReadHint) (Snapshot, bool, error) {
	return readTranscriptSnapshot(path, hint)
}

func readTranscriptSnapshot(path string, hint ReadHint) (Snapshot, bool, error) {
	if strings.TrimSpace(path) == "" {
		return Snapshot{}, false, nil
	}

	var (
		snap                Snapshot
		ok                  bool
		modelByInteraction  = map[string]string{}
		promptByInteraction = map[string]string{}
		lastModel           string
		lastReasoning       string
		copilotVersion      string
		sessionID           string
		hintPrompt          = strings.TrimSpace(hint.UserPrompt)
		hintInteractionID   string
		sawHintPrompt       bool
		matched             Snapshot
		matchedOK           bool
	)

	err := scanJSONLines(path, func(raw []byte) error {
		var item line
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil
		}
		switch item.Type {
		case "session.start":
			var data sessionStartData
			if err := json.Unmarshal(item.Data, &data); err != nil {
				return nil
			}
			sessionID = strings.TrimSpace(data.SessionID)
			copilotVersion = strings.TrimSpace(data.CopilotVersion)
		case "session.model_change":
			var data sessionModelChangeData
			if err := json.Unmarshal(item.Data, &data); err != nil {
				return nil
			}
			if model := strings.TrimSpace(data.NewModel); model != "" {
				lastModel = model
			}
			if reasoning := strings.TrimSpace(data.ReasoningEffort); reasoning != "" {
				lastReasoning = reasoning
			}
		case "user.message":
			var data userMessageData
			if err := json.Unmarshal(item.Data, &data); err != nil {
				return nil
			}
			prompt := strings.TrimSpace(data.Content)
			interactionID := strings.TrimSpace(data.InteractionID)
			if interactionID != "" {
				promptByInteraction[interactionID] = prompt
			}
			if hintPrompt != "" && prompt == hintPrompt {
				sawHintPrompt = true
				hintInteractionID = interactionID
				matched = Snapshot{}
				matchedOK = false
			}
		case "assistant.message":
			var data assistantMessageData
			if err := json.Unmarshal(item.Data, &data); err != nil {
				return nil
			}
			interactionID := strings.TrimSpace(data.InteractionID)
			if interactionID != "" && strings.TrimSpace(data.Model) != "" {
				modelByInteraction[interactionID] = strings.TrimSpace(data.Model)
			}
			candidate := Snapshot{
				SessionID:       sessionID,
				CopilotVersion:  copilotVersion,
				Model:           firstNonEmpty(strings.TrimSpace(data.Model), modelByInteraction[interactionID], lastModel),
				ReasoningEffort: lastReasoning,
				NativeTurnID:    strings.TrimSpace(data.TurnID),
				InteractionID:   interactionID,
				RequestID:       strings.TrimSpace(data.RequestID),
				MessageID:       strings.TrimSpace(data.MessageID),
				AssistantText:   strings.TrimSpace(data.Content),
				OutputTokens:    data.OutputTokens,
				UserPrompt:      promptByInteraction[interactionID],
			}
			snap = candidate
			ok = true
			if matchesHintPromptCandidate(hintPrompt, hintInteractionID, candidate) {
				matched = candidate
				matchedOK = true
			}
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, false, err
	}
	if hintPrompt != "" {
		if matchedOK {
			snap = matched
			ok = true
		} else if sawHintPrompt {
			return Snapshot{}, false, nil
		}
	}
	if !ok {
		return Snapshot{}, false, nil
	}
	if snap.Model == "" {
		snap.Model = lastModel
	}
	if snap.ReasoningEffort == "" {
		snap.ReasoningEffort = lastReasoning
	}
	if snap.CopilotVersion == "" {
		snap.CopilotVersion = copilotVersion
	}
	if snap.SessionID == "" {
		snap.SessionID = sessionID
	}
	return snap, true, nil
}

func scanJSONLines(path string, visit func(raw []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var read int64
	for scanner.Scan() {
		read += int64(len(scanner.Bytes())) + 1
		if read > maxTranscriptBytes {
			return fmt.Errorf("copilot transcript byte budget exceeded")
		}
		if err := visit(scanner.Bytes()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func matchesHintPromptCandidate(hintPrompt, hintInteractionID string, candidate Snapshot) bool {
	if strings.TrimSpace(hintPrompt) == "" {
		return false
	}
	if strings.TrimSpace(hintInteractionID) != "" {
		return strings.TrimSpace(candidate.InteractionID) == strings.TrimSpace(hintInteractionID)
	}
	return strings.TrimSpace(candidate.UserPrompt) == strings.TrimSpace(hintPrompt)
}
