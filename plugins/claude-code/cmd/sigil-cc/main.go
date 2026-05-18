package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"
	"github.com/grafana/sigil-sdk/plugins/claude-code/internal/mapper"
	"github.com/grafana/sigil-sdk/plugins/claude-code/internal/otel"
	"github.com/grafana/sigil-sdk/plugins/claude-code/internal/redact"
	"github.com/grafana/sigil-sdk/plugins/claude-code/internal/state"
	"github.com/grafana/sigil-sdk/plugins/claude-code/internal/transcript"
)

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

var (
	logger  *log.Logger
	version = "dev"
)

func initLogger() {
	logger = log.New(io.Discard, "sigil-cc: ", log.Ltime)

	if !parseBoolEnv(os.Getenv("SIGIL_DEBUG")) {
		return
	}

	dir := filepath.Join(os.Getenv("HOME"), ".claude", "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger = log.New(os.Stderr, "sigil-cc: ", log.Ldate|log.Ltime)
		logger.Printf("mkdir %s: %v (falling back to stderr)", dir, err)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "sigil-cc.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger = log.New(os.Stderr, "sigil-cc: ", log.Ldate|log.Ltime)
		logger.Printf("open log file: %v (falling back to stderr)", err)
		return
	}
	logger = log.New(f, "sigil-cc: ", log.Ldate|log.Ltime)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version":
			fmt.Println(version)
			return
		}
	}
	initLogger()
	run()
}

func run() {
	input, err := parseHookInput(os.Stdin)
	if err != nil {
		logger.Printf("stdin: %v", err)
		return
	}
	logger.Printf("event=%s session=%s transcript=%s", input.HookEventName, input.SessionID, input.TranscriptPath)

	// Canonical SIGIL_* schema only — no legacy aliases.
	sigilEndpoint := os.Getenv("SIGIL_ENDPOINT")
	tenantID := os.Getenv("SIGIL_AUTH_TENANT_ID")
	authToken := os.Getenv("SIGIL_AUTH_TOKEN")

	missing := missingEnvVars(map[string]string{
		"SIGIL_ENDPOINT":       sigilEndpoint,
		"SIGIL_AUTH_TENANT_ID": tenantID,
		"SIGIL_AUTH_TOKEN":     authToken,
	})
	if len(missing) > 0 {
		// Stderr regardless of SIGIL_DEBUG; the debug-gated logger would swallow this.
		fmt.Fprintf(os.Stderr, "sigil-cc: not exporting: missing %s\n", strings.Join(missing, ", "))
		logger.Printf("not exporting: missing %s", strings.Join(missing, ", "))
		return
	}

	extraTags := parseExtraTags(os.Getenv("SIGIL_TAGS"))
	userID := resolveUserID()
	contentMode := resolveContentMode()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = sigil.WithUserID(ctx, userID)

	st := state.Load(input.SessionID)

	switch strings.TrimSpace(input.HookEventName) {
	case "", "Stop":
		// Stop hook uses the transcript to export generations.
	case "SessionStart":
		st.Model = strings.TrimSpace(input.Model)
		if err := state.Save(input.SessionID, st); err != nil {
			logger.Printf("save state: %v", err)
		}
		return
	case "PreToolUse":
		handlePreToolUse(ctx, input, st, sigilEndpoint, tenantID, authToken)
		return
	default:
		// Ignore unsupported events; hooks are best-effort and should not crash
		// Claude Code.
		return
	}

	// SIGIL_OTEL_* takes precedence over OTEL_*; OTEL_* is the fallback.
	otelEndpoint := envOr("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	otelInsecure := parseBoolEnv(envOr("SIGIL_OTEL_EXPORTER_OTLP_INSECURE", os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")))
	otelToken := envOr("SIGIL_OTEL_AUTH_TOKEN", authToken)
	otelProviders, err := otel.Setup(ctx, otel.Config{
		Endpoint: otelEndpoint,
		User:     tenantID,
		Password: otelToken,
		Insecure: otelInsecure,
	})
	if err != nil {
		// Stderr so an OTel config error doesn't silently kill telemetry.
		fmt.Fprintf(os.Stderr, "sigil-cc: otel setup failed: %v\n", err)
		logger.Printf("otel setup: %v", err)
	} else if otelProviders != nil {
		logger.Printf("otel: endpoint=%s", otelEndpoint)
	}
	defer func() { _ = otelProviders.Shutdown(ctx) }()

	lines, _, err := transcript.Read(input.TranscriptPath, st.Offset)
	if err != nil {
		logger.Printf("read transcript: %v", err)
		return
	}
	if len(lines) == 0 {
		return
	}
	logger.Printf("read %d raw lines", len(lines))

	lines, safeOffset := mapper.Coalesce(lines)
	if safeOffset == 0 {
		return
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
		st.Offset = safeOffset
		_ = state.Save(input.SessionID, st)
		return
	}
	logger.Printf("produced %d generations", len(gens))

	// Build the SDK config. The plugin remains basic-only at the auth layer;
	// SIGIL_AUTH_MODE is hardcoded so users only configure the credentials.
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
		cfg.Tracer = otelProviders.Tracer("sigil-cc")
		cfg.Meter = otelProviders.Meter("sigil-cc")
	}

	// SDK reads SIGIL_DEBUG / SIGIL_USER_ID automatically via NewClient → resolveFromEnv.
	// The plugin still computes userID for sigil.WithUserID context propagation
	// (Claude Code-specific ~/.claude.json fallback via SIGIL_USER_ID_SOURCE).
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

		genCtx, rec := client.StartGeneration(ctx, genStart)
		rec.SetResult(gen, nil)

		emitToolSpans(genCtx, client, gen, toolResults)

		rec.End()

		if err := rec.Err(); err != nil {
			logger.Printf("enqueue: %v", err)
		}
	}

	if err := client.Flush(ctx); err != nil {
		logger.Printf("flush: %v", err)
		_ = client.Shutdown(ctx)
		return
	}
	_ = client.Shutdown(ctx)

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
}

func handlePreToolUse(
	ctx context.Context,
	input *hookInput,
	st state.Session,
	sigilEndpoint, tenantID, authToken string,
) {
	toolName := strings.TrimSpace(input.ToolName)
	if toolName == "" {
		return
	}

	modelName := strings.TrimSpace(st.Model)
	if modelName == "" {
		modelName = "unknown"
	}

	failOpen := true
	cfg := sigil.Config{
		API: sigil.APIConfig{
			Endpoint: sigilEndpoint,
		},
		Hooks: sigil.HooksConfig{
			Enabled:  true,
			Phases:   []sigil.HookPhase{sigil.HookPhasePostflight},
			Timeout:  1500 * time.Millisecond,
			FailOpen: &failOpen,
		},
		// Hook evaluation needs auth headers; reuse the export auth inputs.
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
			AgentName:    "claude-code",
			AgentVersion: version,
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
		// Fail-open means tool execution continues.
		logger.Printf("pre_tool_use hook eval: tool=%q endpoint=%q err=%v", toolName, sigilEndpoint, err)
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
		_ = json.NewEncoder(os.Stdout).Encode(out)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv mirrors the SDK's parseBool whitelist (1/true/yes/on).
func parseBoolEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// missingEnvVars returns the keys of vars whose values are empty, sorted by the
// order in which the caller listed them.
func missingEnvVars(vars map[string]string) []string {
	// preserve a stable order matching the canonical schema doc.
	order := []string{"SIGIL_ENDPOINT", "SIGIL_AUTH_TENANT_ID", "SIGIL_AUTH_TOKEN"}
	var out []string
	for _, k := range order {
		if v, ok := vars[k]; ok && v == "" {
			out = append(out, k)
		}
	}
	return out
}

// parseExtraTags parses a comma-separated "key=value" string into a tag map.
// Malformed entries (empty keys, missing '=', empty values) are silently skipped —
// this runs in a stop hook where logging warnings adds noise for no user benefit.
// Empty input returns nil so the mapper can short-circuit on the zero-extras path.
func parseExtraTags(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// resolveContentMode returns the effective ContentCaptureMode from canonical env.
// The plugin's default differs from the SDK default — claude-code defaults to
// metadata_only because user content is sensitive and the typical opt-in path
// is "I want full capture, set the env var".
func resolveContentMode() sigil.ContentCaptureMode {
	v := os.Getenv("SIGIL_CONTENT_CAPTURE_MODE")
	if v == "" {
		return sigil.ContentCaptureModeMetadataOnly
	}
	var mode sigil.ContentCaptureMode
	if err := mode.UnmarshalText([]byte(v)); err != nil {
		// Stderr so a typo doesn't silently downgrade to metadata_only.
		fmt.Fprintf(os.Stderr, "sigil-cc: ignoring invalid SIGIL_CONTENT_CAPTURE_MODE %q, using metadata_only\n", v)
		return sigil.ContentCaptureModeMetadataOnly
	}
	return mode
}
