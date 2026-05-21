// Package claudecode implements the Claude Code agent adapter for the
// consolidated sigil binary. The dispatcher in cmd/sigil routes
// `sigil claude-code hook` here.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/otel"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/redact"
)

// Version is overridden via -ldflags at build time. The dispatcher prints it
// for --version and passes it through here as the agent version. We accept
// it as a package var so tests can override it freely.
var Version = "dev"

// AgentName is the Sigil identity attached to every generation this agent
// emits. Stable across versions so dashboards survive renames.
const AgentName = "claude-code"

// otelInstrumentationName is the OTel instrumentation scope name attached
// to every span and metric this agent emits. Renamed from "sigil-cc" when
// the three agent plugins consolidated into one binary; dashboards that
// previously filtered on "sigil-cc" need to update to "sigil.claude-code".
const otelInstrumentationName = "sigil.claude-code"

type hookInput struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Model          string          `json:"model,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
}

type hookDecision struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
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

	st := state.Load(input.SessionID)

	switch strings.TrimSpace(input.HookEventName) {
	case "", "Stop", "SessionEnd":
		// Stop and SessionEnd hooks use the transcript to export generations.
	case "SessionStart":
		st.Model = strings.TrimSpace(input.Model)
		if err := state.Save(input.SessionID, st); err != nil {
			logger.Printf("save state: %v", err)
		}
		return nil
	case "PreToolUse":
		handlePreToolUse(hookCtx, stdout, input, st, sigilEndpoint, tenantID, authToken, logger)
		return nil
	default:
		return nil
	}

	otelProviders, err := otel.Setup(hookCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sigil claude-code: otel setup failed: %v\n", err)
		logger.Printf("otel setup: %v", err)
	} else if otelProviders != nil {
		logger.Printf("otel: endpoint=%s", otel.EndpointFromEnv())
	}
	defer func() { _ = otelProviders.Shutdown(hookCtx) }()

	lines, _, err := transcript.Read(input.TranscriptPath, st.Offset)
	if err != nil {
		logger.Printf("read transcript: %v", err)
		return nil
	}
	if len(lines) == 0 {
		return nil
	}
	logger.Printf("read %d raw lines", len(lines))

	lines, safeOffset := mapper.Coalesce(lines)
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
		GenerationExport: sigil.GenerationExportConfig{
			Protocol: sigil.GenerationExportProtocolHTTP,
			Endpoint: sigilEndpoint + "/api/v1/generations:export",
			Auth: sigil.AuthConfig{
				Mode:          sigil.ExportAuthModeBasic,
				BasicUser:     tenantID,
				BasicPassword: authToken,
				TenantID:      tenantID,
			},
		},
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

func handlePreToolUse(
	ctx context.Context,
	stdout io.Writer,
	input *hookInput,
	st state.Session,
	sigilEndpoint, tenantID, authToken string,
	logger *log.Logger,
) {
	toolName := strings.TrimSpace(input.ToolName)
	if toolName == "" {
		return
	}

	guards := envconfig.ResolveGuards(logger)
	if !guards.Enabled {
		return
	}

	modelName := strings.TrimSpace(st.Model)
	if modelName == "" {
		modelName = "unknown"
	}

	failOpen := guards.FailOpen
	cfg := sigil.Config{
		API: sigil.APIConfig{
			Endpoint: sigilEndpoint,
		},
		Hooks: sigil.HooksConfig{
			Enabled:  true,
			Phases:   []sigil.HookPhase{sigil.HookPhasePostflight},
			Timeout:  time.Duration(guards.TimeoutMs) * time.Millisecond,
			FailOpen: &failOpen,
		},
		GenerationExport: sigil.GenerationExportConfig{
			Auth: sigil.AuthConfig{
				Mode:          sigil.ExportAuthModeBasic,
				BasicUser:     tenantID,
				BasicPassword: authToken,
				TenantID:      tenantID,
			},
		},
	}

	client := sigil.NewClient(cfg)
	defer func() { _ = client.Shutdown(ctx) }()

	req := sigil.HookEvaluateRequest{
		Phase: sigil.HookPhasePostflight,
		Context: sigil.HookContext{
			AgentName:    AgentName,
			AgentVersion: Version,
			Model:        &sigil.HookModel{Provider: "anthropic", Name: modelName},
		},
		Input: sigil.HookInput{
			Output: []sigil.Message{{
				Role: sigil.RoleAssistant,
				Parts: []sigil.Part{{
					Kind: sigil.PartKindToolCall,
					ToolCall: &sigil.ToolCall{
						ID:        strings.TrimSpace(input.ToolUseID),
						Name:      toolName,
						InputJSON: input.ToolInput,
					},
				}},
			}},
		},
	}
	if payload, mErr := json.Marshal(req); mErr == nil {
		logger.Printf("pre_tool_use hook request: %s", string(payload))
	}

	resp, err := client.EvaluateHook(ctx, req)
	if err != nil {
		// The SDK only surfaces an error when FailOpen=false; the fail-open path
		// returns an allow response with nil err. So reaching here implies strict
		// mode and we should always emit the deny.
		logger.Printf("pre_tool_use hook eval: tool=%q endpoint=%q err=%v", toolName, sigilEndpoint, err)
		out := hookDecision{
			HookSpecificOutput: hookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: fmt.Sprintf("sigil guard evaluation failed: %v", err),
			},
		}
		_ = json.NewEncoder(stdout).Encode(out)
		return
	}

	if resp != nil {
		logger.Printf(
			"pre_tool_use hook eval: tool=%q endpoint=%q action=%q rule_id=%q reason=%q",
			toolName,
			sigilEndpoint,
			string(resp.Action),
			resp.RuleID,
			resp.Reason,
		)
	}

	if deniedErr := sigil.HookDeniedFromResponse(resp); deniedErr != nil {
		reason := "tool call denied by Sigil guard"
		var denied *sigil.HookDeniedError
		if errors.As(deniedErr, &denied) && strings.TrimSpace(denied.Reason) != "" {
			reason = denied.Reason
		}
		if strings.TrimSpace(reason) == "" {
			reason = "tool call denied by Sigil guard"
		}
		out := hookDecision{
			HookSpecificOutput: hookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: reason,
			},
		}
		_ = json.NewEncoder(stdout).Encode(out)
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
