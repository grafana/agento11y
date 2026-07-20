package hook

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
)

// Stop maps the accumulated fragment to an agento11y Generation and emits it.
// On flush failure the fragment is preserved on disk (with the stop payload's
// status/error stamped onto it) so the sessionEnd sweeper can retry rather
// than silently drop the data.
func Stop(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		logger.Print("stop: missing conversation_id or generation_id")
		return
	}

	if !config.HasCredentials() {
		logger.Print("stop: missing SIGIL_ENDPOINT/SIGIL_AUTH_TENANT_ID/SIGIL_AUTH_TOKEN — skipping emission")
		if err := fragment.Delete(p.ConversationID, p.GenerationID); err != nil {
			logger.Printf("stop: delete fragment: %v", err)
		}
		return
	}

	ts := p.ResolvedTimestamp()

	frag := fragment.LoadTolerant(p.ConversationID, p.GenerationID, logger)
	if frag == nil {
		// sessionEnd already swept this generation (or beforeSubmitPrompt
		// never ran). Don't fabricate an empty Generation — that would be a
		// contentless duplicate.
		logger.Printf("stop: no fragment for gen=%s (already swept) — skipping", p.GenerationID)
		return
	}

	fragment.Touch(frag, ts)
	if p.Model != "" && frag.Model == "" {
		frag.Model = p.Model
	}
	if p.Provider != "" && frag.Provider == "" {
		frag.Provider = p.Provider
	}
	// Stop's token counts reflect the full turn including cache + tool
	// rounds; prefer them over earlier afterAgentResponse counts.
	if p.InputTokens != nil || p.OutputTokens != nil ||
		p.CacheReadTokens != nil || p.CacheWriteTokens != nil {
		if frag.TokenUsage == nil {
			frag.TokenUsage = &fragment.TokenCounts{}
		}
		if p.InputTokens != nil {
			frag.TokenUsage.InputTokens = p.InputTokens
		}
		if p.OutputTokens != nil {
			frag.TokenUsage.OutputTokens = p.OutputTokens
		}
		if p.CacheReadTokens != nil {
			frag.TokenUsage.CacheReadTokens = p.CacheReadTokens
		}
		if p.CacheWriteTokens != nil {
			frag.TokenUsage.CacheWriteTokens = p.CacheWriteTokens
		}
	}

	// Persist stop's status/error before client construction so a build
	// failure (or anything else past this point) leaves sessionEnd enough
	// state to replay with the original status.
	frag.PendingStop = &fragment.PendingStop{
		Status: p.Status,
		Error:  stripJSONNull(p.Error),
	}
	if err := fragment.Save(frag); err != nil {
		logger.Printf("stop: save pending status: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	providers := setupOTelIfConfigured(ctx, p.ConversationID, logger)
	defer func() { _ = providers.Shutdown(ctx) }()

	client := buildClient(cfg, providers)
	session := fragment.LoadSession(p.ConversationID, logger)

	mapped := mapper.MapFragment(mapper.Inputs{
		Fragment:       frag,
		Session:        session,
		Stop:           &mapper.StopInput{Status: p.Status, Error: frag.PendingStop.Error},
		ContentCapture: cfg.ContentCapture,
		UserIDOverride: cfg.UserIDOverride,
		Now:            time.Now(),
	})

	if err := emitGeneration(ctx, client, frag, mapped, logger); err != nil {
		logger.Printf("stop: emit gen=%s (preserving fragment for sessionEnd retry): %v", p.GenerationID, err)
		_ = client.Shutdown(ctx)
		return
	}

	if err := client.Flush(ctx); err != nil {
		logger.Printf("stop: flush gen=%s (preserving fragment for sessionEnd retry): %v", p.GenerationID, err)
		_ = client.Shutdown(ctx)
		return
	}
	_ = client.Shutdown(ctx)

	if err := providers.ForceFlush(); err != nil {
		logger.Printf("stop: otel flush: %v", err)
	}

	if err := fragment.Delete(p.ConversationID, p.GenerationID); err != nil {
		logger.Printf("stop: delete fragment: %v", err)
	}
	logger.Printf("stop: emitted gen=%s stopReason=%s", p.GenerationID, mapped.StopStatus)
}

// stripJSONNull turns a literal JSON `null` into nil bytes so the mapper
// doesn't trip on `null` thinking it's a valid error.
func stripJSONNull(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	return b
}
