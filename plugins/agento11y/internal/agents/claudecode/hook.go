// Package claudecode implements the Claude Code agent adapter for the
// consolidated agento11y binary. The dispatcher in cmd/agento11y routes
// `agento11y claude-code hook` here.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/userid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/autotag"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
	"go.opentelemetry.io/otel/trace"
)

// Version is overridden via -ldflags at build time. The dispatcher prints it
// for --version and passes it through here as the agent version. We accept
// it as a package var so tests can override it freely.
var Version = "dev"

// AgentName is the default agent identity this adapter reports. Stable across
// versions so dashboards survive renames. AGENTO11Y_AGENT_NAME overrides it per
// run; see envconfig.ResolveAgentName.
const AgentName = "claude-code"

func exportConfig(endpoint, tenantID, authToken string) agento11y.GenerationExportConfig {
	return agento11y.GenerationExportConfig{
		Protocol: agento11y.GenerationExportProtocolHTTP,
		Endpoint: emit.ExportEndpointFor(endpoint),
		Headers:  map[string]string{"User-Agent": useragent.For("claude-code")},
		Auth: agento11y.AuthConfig{
			Mode:          agento11y.ExportAuthModeBasic,
			BasicUser:     tenantID,
			BasicPassword: authToken,
			TenantID:      tenantID,
		},
	}
}

// otelInstrumentationName is the OTel instrumentation scope name attached
// to every span and metric this agent emits. Renamed from "sigil-cc" when
// the three agent plugins consolidated into one binary; dashboards that
// previously filtered on "sigil-cc" need to update to "agento11y.claude-code".
const otelInstrumentationName = "agento11y.claude-code"

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
	Cwd            string          `json:"cwd,omitempty"`
	Model          string          `json:"model,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
}

// Hook reads a single Claude Code hook payload from stdin, processes it, and
// optionally writes a decision response to stdout. Returns an error only on
// payload-shaped failures the dispatcher should log — runtime telemetry
// errors are logged internally because hooks must never crash the agent.
func Hook(ctx context.Context, stdin io.Reader, stdout io.Writer, logger *log.Logger) error {
	input, err := parseHookInput(stdin)
	if input != nil && input.SessionID != "" && strings.TrimSpace(input.HookEventName) == "SessionEnd" {
		defer discardSessionTools(input.SessionID, logger)
	}
	if err != nil {
		logger.Printf("stdin: %v", err)
		return nil
	}
	logger.Printf("event=%s session=%s transcript=%s", input.HookEventName, input.SessionID, input.TranscriptPath)

	st := state.Load(input.SessionID)

	// One name for both branches below: the guard request and the exported
	// generations have to agree, otherwise a rule scoped to a per-run name
	// never matches the generations that run produces. The binary runs once per
	// hook event, so the name is resolved per invocation and a session that
	// changes the variable mid-run splits across two names.
	resolvedAgentName := envconfig.ResolveAgentName(AgentName)

	// Route lightweight events that do not need real agento11y credentials first.
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
		handlePreToolUse(guardCtx, stdout, input, st, resolvedAgentName, logger)
		return nil
	case "UserPromptSubmit":
		guardCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		handleUserPromptSubmit(guardCtx, stdout, input, st, resolvedAgentName, logger)
		return nil
	case "", "Stop", "SessionEnd":
		// Fall through to transcript export below.
	default:
		return nil
	}

	protocol, err := envconfig.ResolveGenerationTransport()
	if err != nil {
		logger.Printf("not exporting: %v", err)
		return nil
	}

	endpoint := envconfig.Getenv("ENDPOINT")
	tenantID := envconfig.Getenv("AUTH_TENANT_ID")
	authToken := envconfig.Getenv("AUTH_TOKEN")

	// A local-mode endpoint never needs real Cloud credentials. The
	// launcher injects placeholders, but a user running the hook
	// directly (e.g. for testing) might leave them empty — fill in
	// stand-ins so the SDK proceeds.
	tenantID, authToken = envconfig.LocalAuthPlaceholders(endpoint, tenantID, authToken)

	missing := envconfig.MissingEnvVars(
		[]string{"AGENTO11Y_ENDPOINT", "AGENTO11Y_AUTH_TENANT_ID", "AGENTO11Y_AUTH_TOKEN"},
		map[string]string{
			"AGENTO11Y_ENDPOINT":       endpoint,
			"AGENTO11Y_AUTH_TENANT_ID": tenantID,
			"AGENTO11Y_AUTH_TOKEN":     authToken,
		},
	)
	if protocol == agento11y.GenerationExportProtocolHTTP && len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "agento11y claude-code: not exporting: missing %s\n", strings.Join(missing, ", "))
		logger.Printf("not exporting: missing %s", strings.Join(missing, ", "))
		return nil
	}

	extraTags := envconfig.ParseExtraTags(envconfig.Getenv("TAGS"))
	userID := userid.Resolve()
	contentMode := envconfig.ResolveContentMode(logger)
	skipPromptRedaction := !envconfig.ResolveRedactInput(logger)

	hookCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hookCtx = agento11y.WithUserID(hookCtx, userID)

	otelProviders, err := otel.SetupWithOptions(hookCtx, input.SessionID, otel.Options{GenerationProtocol: protocol})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agento11y claude-code: otel setup failed: %v\n", err)
		logger.Printf("otel setup: %v", err)
		if protocol == agento11y.GenerationExportProtocolOTel {
			return nil
		}
	} else if otelProviders != nil {
		logger.Printf("otel: endpoint=%s", otel.EndpointFromEnv())
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = otelProviders.Shutdown(cleanupCtx)
	}()

	subFiles, err := liveSubagentFiles(input.TranscriptPath, logger)
	if err != nil {
		logger.Printf("list subagent transcripts: %v", err)
		return nil
	}
	initToolState(&st, input.TranscriptPath, subFiles)
	stop := strings.TrimSpace(input.HookEventName) == "Stop"
	// SessionEnd reads once. Stop already waited for the closing turn, and
	// Claude Code cancels SessionEnd hooks 1.5s into its exit, so a wait here
	// loses the turns that did complete when a session ends mid-turn.
	settle := transcriptSettleWindow
	if strings.TrimSpace(input.HookEventName) == "SessionEnd" {
		settle = 0
	}
	readFrom := toolReadOffset(&st, input.TranscriptPath, st.Offset)
	raw, readPos, titles, err := readLiveSettled(hookCtx, input.TranscriptPath, readFrom, st.Offset, settle, stop, logger)
	if err != nil {
		return nil
	}
	// Read every required file before changing checkpoints or exporting parents.
	// SessionEnd has no later retry, so export readable files despite child failures.
	reads, err := readLiveSubagentFiles(subFiles, input.TranscriptPath, &st, logger)
	if err != nil && strings.TrimSpace(input.HookEventName) != "SessionEnd" {
		return nil
	}
	observeTools(&st, input.TranscriptPath, raw, readPos, subFiles)
	for _, item := range reads {
		observeTools(&st, item.path, item.raw, item.readPos, subFiles)
	}
	resolvePendingToolChildren(&st, subFiles)
	lines, safeOffset := mapper.Coalesce(afterOffset(raw, st.Offset))
	rawCount := len(afterOffset(raw, st.Offset))
	parentReady := rawCount > 0 && safeOffset > st.Offset
	switch {
	case parentReady:
		logger.Printf("read %d raw lines", rawCount)
		logger.Printf("coalesced to %d lines, safe offset=%d", len(lines), safeOffset)
	case rawCount > 0:
		logger.Printf("read %d raw lines", rawCount)
		logger.Printf("no completed assistant turn yet; keeping offset=%d", st.Offset)
		lines = nil
	default:
		// Parent already exported; background and nested subagent files may
		// still have new turns (or SessionEnd still has a trailing turn).
		logger.Printf("no new parent turns; checking subagent transcripts")
		lines = nil
	}

	var r *redact.Redactor
	if contentMode != agento11y.ContentCaptureModeMetadataOnly {
		r = redact.New()
	}

	opts := mapper.Options{
		SessionID:           input.SessionID,
		Titles:              titles,
		Logger:              logger,
		ExtraTags:           extraTags,
		AgentName:           resolvedAgentName,
		AgentVersion:        envconfig.ResolveAgentVersion(""),
		SkipPromptRedaction: skipPromptRedaction,
		TitleDropped:        contentMode == agento11y.ContentCaptureModeMetadataOnly,
		GenerationSources:   map[int]transcript.Line{},
	}

	var parentGens []agento11y.Generation
	if parentReady {
		lines = subagent.DropCoveredSidechain(lines, subFiles, input.TranscriptPath)
		opts.SuppressSyntheticSubagentToolCallIDs = subagent.SyntheticToolCallIDs(lines, subagent.RefsForFiles(subFiles))
		parentGens, _ = mapper.Process(lines, &st, opts, r)
	}
	parents := associateTools(&st, input.TranscriptPath, opts.GenerationSources, parentGens)
	rememberToolChildren(&st, resolvedAgentName)
	subGens, subParents := liveSubagentGens(input, raw, lines, parentGens, subFiles, reads, &st, opts, r, logger)
	parents = append(parents, subParents...)
	gens := make([]agento11y.Generation, 0, len(parentGens)+len(subGens))
	gens = append(gens, parentGens...)
	gens = append(gens, subGens...)

	logger.Printf("produced %d generations", len(gens))

	cfg := agento11y.Config{
		GenerationExport: exportConfig(endpoint, tenantID, authToken),
		// Client tags, so AGENTO11Y_AUTO_CODING_AGENT_TAGS values reach OTel
		// metrics as well as the export. Resolved here rather than at launch so a
		// session the user starts with plain `claude` gets them too.
		Tags: autotag.FromEnv(autotag.Inputs{Cwd: input.Cwd, UserID: userID}, logger),
	}

	if otelProviders != nil {
		cfg.Tracer = otelProviders.Tracer(otelInstrumentationName)
		cfg.Meter = otelProviders.Meter(otelInstrumentationName)
	}

	cfg.ContentCapture = contentMode
	if err := emit.ConfigureGenerationTransport(&cfg, otelProviders, protocol); err != nil {
		logger.Printf("client setup: %v", err)
		return nil
	}
	client := agento11y.NewClient(cfg)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = client.Shutdown(cleanupCtx)
	}()
	clients := newRecordClients(client, cfg, st.MetricsRecorded)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		clients.shutdownQuiet(cleanupCtx)
	}()
	// An export failure keeps the offset, so the next hook sends these records
	// again. Remember the ones whose metrics were delivered, so that hook does
	// not count them a second time.
	exportFailed := func() {
		if otelProviders == nil {
			return // no OTLP endpoint, so no metrics were recorded
		}
		// Send what is still pending. If that fails, an earlier periodic
		// export may still have delivered some records.
		_ = otelProviders.FlushMetrics()
		rememberRecordedMetrics(input.SessionID, clients.keys(otelProviders.MetricsDeliveredAt()), logger)
	}
	t0 := time.Now()

	for i, gen := range gens {
		var parentContext trace.SpanContext
		recordClient, recorded := clients.forRecord(generationRecordKey(gen.ID))
		err := emit.Record(hookCtx, recordClient, generationStart(gen), gen, nil, func(genCtx context.Context) {
			parentContext = trace.SpanContextFromContext(genCtx)
		})
		// The recorder records metrics when it ends, even when it then fails.
		recorded()
		if err != nil {
			logger.Printf("enqueue: %v", err)
			if protocol == agento11y.GenerationExportProtocolOTel {
				emitPendingTools(hookCtx, clients, &st, contentMode, r)
				exportFailed()
				return nil
			}
			continue
		}
		// Record invokes the callback before validation. Failed records must not
		// make their pending tools ready, even if a span context was captured.
		captureToolParent(parents[i], parentContext)
	}
	emitPendingTools(hookCtx, clients, &st, contentMode, r)

	if err := clients.flush(hookCtx); err != nil {
		logger.Printf("flush: %v", err)
		exportFailed()
		return nil
	}

	if otelProviders != nil {
		if err := otelProviders.ForceFlush(); err != nil {
			logger.Printf("otel flush: %v", err)
		}
	}

	if parentReady {
		st.Offset = safeOffset
	}
	// Every record sent again this time is now exported.
	st.MetricsRecorded = nil
	pruneToolSpawns(&st, input.TranscriptPath)
	if strings.TrimSpace(input.HookEventName) == "SessionEnd" {
		st.Tools.Pending = nil
		st.Tools.Spawns = nil
		st.Tools.CompletedChildren = nil
	}
	if err := state.Save(input.SessionID, st); err != nil {
		logger.Printf("save state: %v", err)
	}
	logger.Printf("done: %d generations in %s", len(gens), time.Since(t0))
	return nil
}

// Reload persisted state: failed reads and mapping may mutate working checkpoints.
// A shallow snapshot would share maps and save those uncommitted changes.
func discardSessionTools(sessionID string, logger *log.Logger) {
	persisted := state.Load(sessionID)
	if len(persisted.Tools.Pending) == 0 && len(persisted.Tools.Spawns) == 0 && len(persisted.Tools.CompletedChildren) == 0 {
		return
	}
	persisted.Tools.Pending = nil
	persisted.Tools.Spawns = nil
	persisted.Tools.CompletedChildren = nil
	if err := state.Save(sessionID, persisted); err != nil {
		logger.Printf("discard session tools: %v", err)
	}
}

// handlePreToolUse evaluates the tool call against agento11y guards and writes a
// PreToolUse deny envelope to stdout when the call is blocked, or an
// allow+updatedInput envelope when a Transform rule redacted the tool
// arguments. A plain allow stays silent on stdout. All agento11y transport,
// credential, fail-open/closed, and local-endpoint placeholder behaviour
// lives in the shared guard helper so this stays in lockstep with the codex
// and copilot agents.
func handlePreToolUse(ctx context.Context, stdout io.Writer, input *hookInput, st state.Session, agentName string, logger *log.Logger) {
	res := guard.EvaluateToolCall(ctx, envconfig.ResolveGuards(logger), guard.ToolCallInput{
		AgentName:      agentName,
		AgentVersion:   Version,
		ConversationID: input.SessionID,
		ModelProvider:  "anthropic",
		ModelName:      strings.TrimSpace(st.Model),
		ToolName:       strings.TrimSpace(input.ToolName),
		ToolCallID:     strings.TrimSpace(input.ToolUseID),
		ToolInputJSON:  input.ToolInput,
	}, logger)
	if res.Blocked() {
		guard.WriteHookSpecificOutputDeny(stdout, res.Reason)
		return
	}
	if len(res.UpdatedInputJSON) > 0 {
		guard.WriteHookSpecificOutputUpdatedInput(stdout, res.UpdatedInputJSON)
	}
}

// handleUserPromptSubmit writes a block response when a preflight guard denies
// the prompt or fails closed. Claude Code shows the reason and stops the turn.
//
// Prompt capture still happens from the transcript at Stop.
func handleUserPromptSubmit(ctx context.Context, stdout io.Writer, input *hookInput, st state.Session, agentName string, logger *log.Logger) {
	res := guard.EvaluatePrompt(ctx, envconfig.ResolveGuards(logger), guard.PromptInput{
		AgentName:      agentName,
		AgentVersion:   Version,
		ConversationID: input.SessionID,
		ModelProvider:  "anthropic",
		ModelName:      strings.TrimSpace(st.Model),
		Prompt:         input.Prompt,
	}, logger)
	if res.Blocked() {
		guard.WritePromptBlock(stdout, res.Reason)
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
// To close the race we re-read until the tail no longer looks like an
// assistant turn that is still landing (see tailNeedsSettle) or
// transcriptSettleWindow elapses. The common case (the turn is already fully
// flushed when Stop fires) settles on the first read and adds no latency; only
// a trailing tool_result, a partial assistant line, or a lone prompt awaiting
// its first assistant reply triggers the bounded wait.
//
// A Stop means the turn is over, so on Stop the read also waits until the last
// complete assistant turn ended the turn. Without CLAUDE_CODE_EAGER_FLUSH, a
// headless `claude -p` run fires Stop before its transcript writes land: the
// file may not exist yet, or may end at the turn that called a tool, and the
// SessionEnd that would catch up is cancelled when -p exits.
//
// Returns the coalesced lines, the safe offset, and the raw line count so the
// caller can distinguish "nothing to read" from "read but nothing complete",
// and the title lines Claude Code wrote in the range.
func readTranscriptSettled(ctx context.Context, path string, offset int64, stop bool, logger *log.Logger) ([]transcript.Line, int64, int, transcript.Titles) {
	raw, _, titles, err := readLiveSettled(ctx, path, offset, offset, transcriptSettleWindow, stop, logger)
	if err != nil {
		return nil, 0, 0, transcript.Titles{}
	}
	lines, safe := mapper.Coalesce(raw)
	return lines, safe, len(raw), titles
}

// A test seam for transient I/O faults. Each accepted batch feeds both consumers.
var readLiveComplete = transcript.ReadCompleteWithTitles

func readLiveSettled(ctx context.Context, path string, offset, generationOffset int64, settle time.Duration, stop bool, logger *log.Logger) ([]transcript.Line, int64, transcript.Titles, error) {
	deadline := time.Now().Add(settle)
	for {
		raw, pos, titles, err := readLiveComplete(path, offset)
		if err != nil && (!stop || !errors.Is(err, fs.ErrNotExist)) {
			logger.Printf("read transcript: %v", err)
			return nil, offset, transcript.Titles{}, err
		}

		mappingRaw := afterOffset(raw, generationOffset)
		coalesced, safeOffset := mapper.Coalesce(mappingRaw)

		// Settle unless the tail is an assistant turn Claude Code is still
		// flushing. An empty read (redundant Stop/SessionEnd after a prior
		// export) has nothing to wait for, and tailNeedsSettle decides the rest.
		settled := len(mappingRaw) == 0 || !tailNeedsSettle(mappingRaw[len(mappingRaw)-1], safeOffset)
		if stop {
			settled = settled && endsTurn(coalesced)
		}
		if settled || !time.Now().Before(deadline) {
			if err != nil {
				logger.Printf("read transcript: %v", err)
			}
			return raw, pos, titles, err
		}

		timer := time.NewTimer(transcriptSettleInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return raw, pos, titles, err
		case <-timer.C:
		}
	}
}

// endsTurn reports whether the last coalesced line is an assistant turn that
// ended the turn rather than stopping to call a tool.
func endsTurn(lines []transcript.Line) bool {
	if len(lines) == 0 || lines[len(lines)-1].Type != "assistant" {
		return false
	}
	var msg transcript.AssistantMessage
	return json.Unmarshal(lines[len(lines)-1].Message, &msg) == nil && msg.StopReason != "tool_use"
}

// tailNeedsSettle reports whether the last raw transcript line indicates an
// assistant turn that Claude Code is still flushing, so re-reading may recover
// it. safeOffset is the end of the last complete assistant turn (from Coalesce).
//
//   - Tail is the last complete assistant turn (EndOffset == safeOffset): fully
//     landed, nothing to wait for.
//   - Tail is an assistant line that did not coalesce (no terminal stop_reason):
//     a partial/streaming turn still landing — wait.
//   - Tail is a tool_result: the assistant called a tool and will emit a follow
//     -up turn that has not landed yet — wait (this is the diagnosed bug).
//   - Tail is a plain user prompt: the start of a turn. Wait only when no
//     completed assistant turn has landed in this batch (safeOffset == 0) — that
//     is the symmetric race for a tool-free final turn whose assistant reply is
//     still flushing. When a completed turn precedes the prompt (safeOffset > 0)
//     the prompt belongs to a *future* turn, so the current event already has
//     all it needs and must not block on it.
func tailNeedsSettle(last transcript.Line, safeOffset int64) bool {
	if last.EndOffset == safeOffset {
		return false
	}
	if last.Type == "assistant" {
		return true
	}

	var msg transcript.UserMessage
	if err := json.Unmarshal(last.Message, &msg); err != nil {
		// Unknown shape: be conservative and wait rather than risk dropping
		// an assistant turn that is mid-flush behind it.
		return true
	}
	_, blocks, err := transcript.ParseUserContent(msg.Content)
	if err == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return true
			}
		}
	}
	return safeOffset == 0
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
		// SessionEnd cleanup still needs the session ID when the transcript path is missing.
		return &input, fmt.Errorf("missing session_id or transcript_path")
	}

	return &input, nil
}

// generationStart projects a mapped generation onto the span seed the SDK
// needs to open the generation span.
//
// StartedAt has to travel with it. When it is zero the SDK stamps the span
// start with time.Now(), while End stamps the span end with the transcript's
// CompletedAt. The hook runs after the turn finished, so the window inverts and
// a trace store renders the unsigned wrap of a negative duration: 2^64
// nanoseconds, or 213504 days.
func generationStart(gen agento11y.Generation) agento11y.GenerationStart {
	return agento11y.GenerationStart{
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
		StartedAt:           gen.StartedAt,
	}
}
