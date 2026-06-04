// Package claudecode implements the Claude Code agent adapter for the
// consolidated sigil binary. The dispatcher in cmd/sigil routes
// `sigil claude-code hook` here.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/mapper"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/state"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/transcript"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/guard"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/otel"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/redact"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/useragent"
)

// Version is overridden via -ldflags at build time. The dispatcher prints it
// for --version and passes it through here as the agent version. We accept
// it as a package var so tests can override it freely.
var Version = "dev"

// AgentName is the Sigil identity attached to every generation this agent
// emits. Stable across versions so dashboards survive renames.
const AgentName = "claude-code"

func exportConfig(endpoint, tenantID, authToken string) sigil.GenerationExportConfig {
	return sigil.GenerationExportConfig{
		Protocol: sigil.GenerationExportProtocolHTTP,
		Endpoint: endpoint + "/api/v1/generations:export",
		Headers:  map[string]string{"User-Agent": useragent.For("claude-code")},
		Auth: sigil.AuthConfig{
			Mode:          sigil.ExportAuthModeBasic,
			BasicUser:     tenantID,
			BasicPassword: authToken,
			TenantID:      tenantID,
		},
	}
}

// otelInstrumentationName is the OTel instrumentation scope name attached
// to every span and metric this agent emits. Renamed from "sigil-cc" when
// the three agent plugins consolidated into one binary; dashboards that
// previously filtered on "sigil-cc" need to update to "sigil.claude-code".
const otelInstrumentationName = "sigil.claude-code"

// transcriptSettleInterval is the poll spacing used while waiting for Claude
// Code to finish flushing a turn to the transcript (see readTranscriptSettled).
const transcriptSettleInterval = 100 * time.Millisecond

// transcriptSettleWindow bounds how long readTranscriptSettled re-reads the
// transcript waiting for the final assistant turn to land on disk. It is a var
// so tests can zero it out and skip the wait. See readTranscriptSettled for why
// the wait exists.
var transcriptSettleWindow = 2 * time.Second

type hookInput struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Model          string          `json:"model,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
}

// Hook reads a single Claude Code hook payload from stdin, processes it, and
// optionally writes a decision response to stdout. Returns an error only on
// payload-shaped failures the dispatcher should log — runtime telemetry
// errors are logged internally because hooks must never crash the agent.
func Hook(ctx context.Context, stdin io.Reader, stdout io.Writer, logger *log.Logger) error {
	input, err := parseHookInput(stdin)
	if err != nil {
		logger.Printf("stdin: %v", err)
		return nil
	}
	logger.Printf("event=%s session=%s transcript=%s", input.HookEventName, input.SessionID, input.TranscriptPath)

	st := state.Load(input.SessionID)

	// Route lightweight events that do not need real Sigil credentials first.
	// SessionStart only updates local state; PreToolUse calls into the shared
	// guard helper which manages its own credentials, timeouts, and
	// fail-open/closed behaviour. Routing these before the missing-creds
	// gate below means strict guard mode can still deny when SIGIL_* env
	// vars are absent.
	switch strings.TrimSpace(input.HookEventName) {
	case "SessionStart":
		st.Model = strings.TrimSpace(input.Model)
		if err := state.Save(input.SessionID, st); err != nil {
			logger.Printf("save state: %v", err)
		}
		return nil
	case "PreToolUse":
		guardCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		handlePreToolUse(guardCtx, stdout, input, st, logger)
		return nil
	case "", "Stop", "SessionEnd":
		// Fall through to transcript export below.
	default:
		return nil
	}

	sigilEndpoint := os.Getenv("SIGIL_ENDPOINT")
	tenantID := os.Getenv("SIGIL_AUTH_TENANT_ID")
	authToken := os.Getenv("SIGIL_AUTH_TOKEN")

	// A local-mode endpoint never needs real Cloud credentials. The
	// launcher injects placeholders, but a user running the hook
	// directly (e.g. for testing) might leave them empty — fill in
	// stand-ins so the SDK proceeds.
	tenantID, authToken = envconfig.LocalAuthPlaceholders(sigilEndpoint, tenantID, authToken)

	missing := envconfig.MissingEnvVars(
		[]string{"SIGIL_ENDPOINT", "SIGIL_AUTH_TENANT_ID", "SIGIL_AUTH_TOKEN"},
		map[string]string{
			"SIGIL_ENDPOINT":       sigilEndpoint,
			"SIGIL_AUTH_TENANT_ID": tenantID,
			"SIGIL_AUTH_TOKEN":     authToken,
		},
	)
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "sigil claude-code: not exporting: missing %s\n", strings.Join(missing, ", "))
		logger.Printf("not exporting: missing %s", strings.Join(missing, ", "))
		return nil
	}

	extraTags := envconfig.ParseExtraTags(os.Getenv("SIGIL_TAGS"))
	userID := resolveUserID()
	contentMode := envconfig.ResolveContentMode(logger)

	hookCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hookCtx = sigil.WithUserID(hookCtx, userID)

	otelProviders, err := otel.Setup(hookCtx, input.SessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sigil claude-code: otel setup failed: %v\n", err)
		logger.Printf("otel setup: %v", err)
	} else if otelProviders != nil {
		logger.Printf("otel: endpoint=%s", otel.EndpointFromEnv())
	}
	defer func() { _ = otelProviders.Shutdown(hookCtx) }()

	lines, safeOffset, rawCount := readTranscriptSettled(hookCtx, input.TranscriptPath, st.Offset, logger)
	if rawCount == 0 {
		return nil
	}
	logger.Printf("read %d raw lines", rawCount)

	if safeOffset == 0 {
		logger.Printf("no completed assistant turn yet; keeping offset=%d", st.Offset)
		return nil
	}
	logger.Printf("coalesced to %d lines, safe offset=%d", len(lines), safeOffset)

	var r *redact.Redactor
	if contentMode != sigil.ContentCaptureModeMetadataOnly {
		r = redact.New()
	}

	gens := mapper.Process(lines, &st, mapper.Options{
		SessionID: input.SessionID,
		Logger:    logger,
		ExtraTags: extraTags,
	}, r)

	if len(gens) == 0 {
		logger.Printf("no generations produced; keeping offset=%d for next event", st.Offset)
		return nil
	}
	logger.Printf("produced %d generations", len(gens))

	cfg := sigil.Config{
		GenerationExport: exportConfig(sigilEndpoint, tenantID, authToken),
	}

	if otelProviders != nil {
		cfg.Tracer = otelProviders.Tracer(otelInstrumentationName)
		cfg.Meter = otelProviders.Meter(otelInstrumentationName)
	}

	cfg.ContentCapture = contentMode
	client := sigil.NewClient(cfg)
	t0 := time.Now()

	toolResults := buildToolResultMap(gens)

	for _, gen := range gens {
		genStart := sigil.GenerationStart{
			ID:                  gen.ID,
			ConversationID:      gen.ConversationID,
			ConversationTitle:   gen.ConversationTitle,
			AgentName:           gen.AgentName,
			AgentVersion:        gen.AgentVersion,
			Mode:                gen.Mode,
			OperationName:       gen.OperationName,
			Model:               gen.Model,
			ParentGenerationIDs: gen.ParentGenerationIDs,
			Tags:                gen.Tags,
			Metadata:            gen.Metadata,
		}

		genCtx, rec := client.StartGeneration(hookCtx, genStart)
		rec.SetResult(gen, nil)

		emitToolSpans(genCtx, client, gen, toolResults)

		rec.End()

		if err := rec.Err(); err != nil {
			logger.Printf("enqueue: %v", err)
		}
	}

	if err := client.Flush(hookCtx); err != nil {
		logger.Printf("flush: %v", err)
		_ = client.Shutdown(hookCtx)
		return nil
	}
	_ = client.Shutdown(hookCtx)

	if otelProviders != nil {
		if err := otelProviders.ForceFlush(); err != nil {
			logger.Printf("otel flush: %v", err)
		}
	}

	st.Offset = safeOffset
	if err := state.Save(input.SessionID, st); err != nil {
		logger.Printf("save state: %v", err)
	}
	logger.Printf("done: %d generations in %s", len(gens), time.Since(t0))
	return nil
}

// handlePreToolUse evaluates the tool call against Sigil guards and writes a
// PreToolUse deny envelope to stdout when the call is blocked. All Sigil
// transport, credential, fail-open/closed, and local-endpoint placeholder
// behaviour lives in the shared guard helper so this stays in lockstep with
// the codex and copilot agents.
func handlePreToolUse(ctx context.Context, stdout io.Writer, input *hookInput, st state.Session, logger *log.Logger) {
	res := guard.EvaluateToolCall(ctx, envconfig.ResolveGuards(logger), guard.ToolCallInput{
		AgentName:     AgentName,
		AgentVersion:  Version,
		ModelProvider: "anthropic",
		ModelName:     strings.TrimSpace(st.Model),
		ToolName:      strings.TrimSpace(input.ToolName),
		ToolCallID:    strings.TrimSpace(input.ToolUseID),
		ToolInputJSON: input.ToolInput,
	}, logger)
	if res.Blocked() {
		guard.WriteHookSpecificOutputDeny(stdout, res.Reason)
	}
}

// readTranscriptSettled reads transcript lines from offset and coalesces them
// into complete assistant turns, briefly re-reading to dodge a flush race.
//
// Claude Code fires the Stop hook before the closing assistant turn (and its
// preceding tool_result) is reliably flushed to the JSONL transcript. A single
// read therefore often misses the last turn of a session: the hook coalesces
// only through the prior tool-use turn, advances the offset there, and exits.
// Because export happens solely on Stop/SessionEnd, no later event re-reads the
// turn once it lands, so the final message is lost forever.
//
// To close the race we re-read until the transcript's tail is itself the last
// complete assistant turn — i.e. the safe offset reaches the end of the last
// meaningful line, meaning nothing is still landing — or transcriptSettleWindow
// elapses. The common case (the turn is already fully flushed when Stop fires)
// settles on the first read and adds no latency; only a detected trailing
// fragment triggers the bounded wait.
//
// Returns the coalesced lines, the safe offset, and the raw line count so the
// caller can distinguish "nothing to read" from "read but nothing complete".
func readTranscriptSettled(ctx context.Context, path string, offset int64, logger *log.Logger) ([]transcript.Line, int64, int) {
	deadline := time.Now().Add(transcriptSettleWindow)
	for {
		raw, _, err := transcript.Read(path, offset)
		if err != nil {
			logger.Printf("read transcript: %v", err)
			return nil, 0, 0
		}

		coalesced, safeOffset := mapper.Coalesce(raw)

		// Settle when there is nothing left to wait for: either the read is
		// empty (a redundant Stop/SessionEnd after a prior export — the user
		// prompt that begins any real turn lands well before Stop fires, so an
		// empty read means "nothing new", not "mid-flush"), or the tail is
		// already the last complete assistant turn. Only a trailing tool_result
		// or partial turn that Claude Code is still flushing warrants the wait.
		settled := len(raw) == 0 || raw[len(raw)-1].EndOffset == safeOffset
		if settled || !time.Now().Before(deadline) {
			return coalesced, safeOffset, len(raw)
		}

		timer := time.NewTimer(transcriptSettleInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return coalesced, safeOffset, len(raw)
		case <-timer.C:
		}
	}
}

func parseHookInput(r io.Reader) (*hookInput, error) {
	data, err := io.ReadAll(r)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("empty stdin")
	}

	var input hookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, err
	}

	input.SessionID = strings.TrimSpace(input.SessionID)
	input.TranscriptPath = strings.TrimSpace(input.TranscriptPath)
	if input.SessionID == "" || input.TranscriptPath == "" {
		return nil, fmt.Errorf("missing session_id or transcript_path")
	}

	return &input, nil
}

// buildToolResultMap indexes tool results by their call ID across all generations.
// Tool results appear in the Input of the generation that follows the tool call.
func buildToolResultMap(gens []sigil.Generation) map[string]*sigil.ToolResult {
	m := make(map[string]*sigil.ToolResult)
	for _, gen := range gens {
		for _, msg := range gen.Input {
			for i, part := range msg.Parts {
				if part.ToolResult != nil && part.ToolResult.ToolCallID != "" {
					m[part.ToolResult.ToolCallID] = msg.Parts[i].ToolResult
				}
			}
		}
	}
	return m
}

// emitToolSpans creates execute_tool spans for each tool call in the generation output.
func emitToolSpans(ctx context.Context, client *sigil.Client, gen sigil.Generation, results map[string]*sigil.ToolResult) {
	for _, msg := range gen.Output {
		for _, part := range msg.Parts {
			if part.ToolCall == nil {
				continue
			}
			tc := part.ToolCall
			start := sigil.ToolExecutionStart{
				ToolName:        tc.Name,
				ToolCallID:      tc.ID,
				ToolType:        "function",
				ConversationID:  gen.ConversationID,
				AgentName:       gen.AgentName,
				AgentVersion:    gen.AgentVersion,
				RequestModel:    gen.Model.Name,
				RequestProvider: gen.Model.Provider,
				StartedAt:       gen.CompletedAt,
			}
			_, toolRec := client.StartToolExecution(ctx, start)

			end := sigil.ToolExecutionEnd{
				CompletedAt: gen.CompletedAt,
				Arguments:   string(tc.InputJSON),
			}
			if tr, ok := results[tc.ID]; ok {
				if tr.Content != "" {
					end.Result = tr.Content
				} else if len(tr.ContentJSON) > 0 {
					end.Result = string(tr.ContentJSON)
				}
			}

			if tr, ok := results[tc.ID]; ok && tr.IsError {
				toolRec.SetExecError(fmt.Errorf("tool returned error"))
			}

			toolRec.SetResult(end)
			toolRec.End()
		}
	}
}
