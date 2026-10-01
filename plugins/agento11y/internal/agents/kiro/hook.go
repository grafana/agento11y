package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/fragmentstore"
	"github.com/grafana/agento11y/plugins/agento11y/internal/mapperutil"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// Payload is the documented CLI hook envelope. We deliberately do not guess
// transcript formats or treat host/model names as reported model information.
type Payload struct {
	Event        string          `json:"hook_event_name"`
	SessionID    string          `json:"session_id"`
	CWD          string          `json:"cwd"`
	Prompt       string          `json:"prompt"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

type tool struct {
	Name     string
	Input    json.RawMessage `json:",omitempty"`
	Response json.RawMessage `json:",omitempty"`
	At       time.Time
}
type turn struct {
	ID          string
	CWD         string
	Prompt      string `json:",omitempty"`
	StartedAt   time.Time
	CompletedAt time.Time
	Tools       []tool
}
type session struct{ Turns []turn }

var exportTurn = sendTurn

func statePath(id string) string {
	return filepath.Join(xdg.AppStateRoot(), "kiro", xdg.SafeComponent(id)+".json")
}

// Hook is observational: no stdout and no nonzero exit, including on export failures.
func Hook(ctx context.Context, stdin io.Reader, _ io.Writer, logger *log.Logger) error {
	raw, err := io.ReadAll(io.LimitReader(stdin, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		logger.Print("kiro: cannot read hook payload (limit 8 MiB)")
		return nil
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		logger.Print("kiro: invalid hook JSON")
		return nil
	}
	if strings.TrimSpace(p.SessionID) == "" {
		logger.Print("kiro: missing session_id")
		return nil
	}
	event := strings.ToLower(p.Event)
	switch event {
	case "userpromptsubmit", "posttooluse", "stop", "sessionend":
	default:
		return nil
	}
	if event == "posttooluse" && p.ToolName == "" {
		return nil
	}
	if p.Prompt == "" {
		p.Prompt = os.Getenv("USER_PROMPT")
	}
	path := statePath(p.SessionID)
	err = fragmentstore.WithFileLock(path, func() error {
		s, _, err := fragmentstore.ReadJSON[session](path)
		if err != nil {
			return err
		}
		if s == nil {
			s = &session{}
		}
		mode := envconfig.ResolveContentMode(logger)
		payloadMode := mapperutil.NormalizePayloadContentMode(mode)
		// Clamp previously stored content as well when the user tightens capture.
		for i := range s.Turns {
			clamp(&s.Turns[i], mode)
		}
		now := time.Now().UTC()
		active := len(s.Turns) > 0 && s.Turns[len(s.Turns)-1].CompletedAt.IsZero()
		switch event {
		case "userpromptsubmit", "posttooluse":
			if event == "userpromptsubmit" && active {
				s.Turns[len(s.Turns)-1].CompletedAt = now
				active = false
			}
			if !active {
				if len(s.Turns) >= 128 {
					return fmt.Errorf("pending Kiro turn queue full")
				}
				s.Turns = append(s.Turns, turn{ID: "kiro-" + uuid.NewString(), CWD: p.CWD, StartedAt: now})
			}
			t := &s.Turns[len(s.Turns)-1]
			if event == "userpromptsubmit" && payloadMode != agento11y.ContentCaptureModeMetadataOnly {
				t.Prompt = p.Prompt
				if envconfig.ResolveRedactInput(logger) {
					t.Prompt = redact.New().Prompt(t.Prompt)
				}
			}
			if event == "posttooluse" {
				rec := tool{Name: p.ToolName, At: now}
				if payloadMode == agento11y.ContentCaptureModeFull {
					rec.Input = redact.New().ToolPayloadJSON(p.ToolInput)
					rec.Response = redact.New().ToolPayloadJSON(p.ToolResponse)
				}
				t.Tools = append(t.Tools, rec)
			}
		case "stop", "sessionend":
			if active {
				s.Turns[len(s.Turns)-1].CompletedAt = now
			}
		}
		// Persist before export; a failed export keeps the same generation ID for retry.
		if err := fragmentstore.WriteJSON(path, s); err != nil {
			return err
		}
		if event != "stop" && event != "sessionend" {
			return nil
		}
		exportCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		for len(s.Turns) > 0 && !s.Turns[0].CompletedAt.IsZero() {
			if err := exportTurn(exportCtx, p.SessionID, s.Turns[0], mode, logger); err != nil {
				return err
			}
			s.Turns = s.Turns[1:]
			if err := fragmentstore.WriteJSON(path, s); err != nil {
				return err
			}
		}
		if len(s.Turns) == 0 {
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		logger.Printf("kiro: %v", err)
	}
	return nil
}

func clamp(t *turn, mode agento11y.ContentCaptureMode) {
	mode = mapperutil.NormalizePayloadContentMode(mode)
	if mode == agento11y.ContentCaptureModeMetadataOnly {
		t.Prompt = ""
	}
	if mode != agento11y.ContentCaptureModeFull {
		for i := range t.Tools {
			t.Tools[i].Input = nil
			t.Tools[i].Response = nil
		}
	}
}
