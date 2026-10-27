package hook

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// SessionEnd sweeps any stranded fragments left by crashed or aborted turns,
// emits each with `stopReason="aborted"` (or the pendingStop status saved by
// handleStop on flush failure), then removes the conversation directory.
//
// On any emission failure the conversation directory is preserved so a later
// sweep can retry rather than silently dropping the data.
func SessionEnd(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" {
		logger.Print("sessionEnd: missing conversation_id")
		return
	}

	protocol, err := envconfig.ResolveGenerationTransport()
	if err != nil {
		logger.Printf("sessionEnd: generation transport conv=%s (preserving conversation for retry): %v", p.ConversationID, err)
		return
	}

	ids := fragment.ListFragmentIDs(p.ConversationID, logger)

	if len(ids) == 0 {
		removeAndLog(p.ConversationID, logger)
		return
	}

	if protocol == agento11y.GenerationExportProtocolHTTP && !config.HasCredentials() {
		logger.Print("sessionEnd: missing credentials — wiping conversation dir without emission")
		removeAndLog(p.ConversationID, logger)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	providers := setupOTelIfConfigured(ctx, p.ConversationID, logger)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = providers.Shutdown(cleanupCtx)
	}()

	session := fragment.LoadSession(p.ConversationID, logger)
	client, err := buildClient(p, session, cfg, providers, logger)
	if err != nil {
		logger.Printf("sessionEnd: build client conv=%s (preserving conversation for retry): %v", p.ConversationID, err)
		return
	}

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = client.Shutdown(cleanupCtx)
	}()

	allEmitted := true
	var emittedIDs []string
	for _, gid := range ids {
		emitted, ok := emitOneStranded(ctx, client, session, p.ConversationID, gid, cfg, logger)
		if !ok {
			allEmitted = false
		}
		if emitted {
			emittedIDs = append(emittedIDs, gid)
		}
	}

	flushOK := true
	if err := client.Flush(ctx); err != nil {
		flushOK = false
		logger.Printf("sessionEnd: flush: %v", err)
	}
	if flushOK {
		for _, gid := range emittedIDs {
			if err := fragment.Delete(p.ConversationID, gid); err != nil {
				logger.Printf("sessionEnd: delete gen=%s: %v", gid, err)
				allEmitted = false
			}
		}
	}

	if err := providers.ForceFlush(); err != nil {
		logger.Printf("sessionEnd: otel flush: %v", err)
	}

	if allEmitted && flushOK {
		removeAndLog(p.ConversationID, logger)
	} else {
		logger.Printf("sessionEnd: preserving conv=%s for retry (emitted=%v flushed=%v)",
			p.ConversationID, allEmitted, flushOK)
	}
}

// Emitted fragments must remain on disk until the caller confirms the batch.
func emitOneStranded(
	ctx context.Context,
	client *agento11y.Client,
	session *fragment.Session,
	conversationID, generationID string,
	cfg config.Config,
	logger *log.Logger,
) (emitted, ok bool) {
	frag := fragment.LoadTolerant(conversationID, generationID, logger)
	if frag == nil {
		// Tolerant loader already logged the failure. Quarantine the
		// unreadable file rather than unlinking it so the data can be
		// inspected later instead of vanishing.
		if err := fragment.Quarantine(conversationID, generationID); err != nil {
			logger.Printf("sessionEnd: quarantine gen=%s: %v", generationID, err)
		} else {
			logger.Printf("sessionEnd: quarantined unreadable gen=%s", generationID)
		}
		// Treat as successfully processed so the dir cleanup proceeds — the corrupt file
		// has been moved out of the sweep path.
		return false, true
	}

	// Thought-only leftovers come from older afterAgentThought handlers that
	// keyed fragments by Cursor's per-step generation_id. They carry no
	// prompt, tools, or assistant text — emitting them floods the dashboard
	// with model=unknown generations. Drop them instead of exporting.
	if isThoughtOnlyFragment(frag) {
		if err := fragment.Delete(conversationID, generationID); err != nil {
			logger.Printf("sessionEnd: delete thought-only gen=%s: %v", generationID, err)
		} else {
			logger.Printf("sessionEnd: dropped thought-only gen=%s", generationID)
		}
		return false, true
	}

	// Prefer the status that handleStop saw if it preserved the fragment on
	// a flush failure. Otherwise this is a truly stranded turn — default to
	// "aborted".
	stop := &mapper.StopInput{Status: "aborted"}
	if frag.PendingStop != nil {
		stop.Status = frag.PendingStop.Status
		stop.Error = frag.PendingStop.Error
	}

	mapped := mapper.MapFragment(mapper.Inputs{
		Fragment:            frag,
		Session:             session,
		Stop:                stop,
		ContentCapture:      cfg.ContentCapture,
		UserIDOverride:      cfg.UserIDOverride,
		SkipPromptRedaction: cfg.SkipPromptRedaction,
		AgentName:           cfg.Agent(),
		AgentVersion:        cfg.AgentVersion,
		Now:                 time.Now(),
	})

	if err := emitGeneration(ctx, client, frag, mapped, logger); err != nil {
		logger.Printf("sessionEnd: emit gen=%s: %v", generationID, err)
		return false, false
	}

	logger.Printf("sessionEnd: recorded gen=%s stopReason=%s", generationID, mapped.StopStatus)
	return true, true
}

// isThoughtOnlyFragment reports leftovers from the old afterAgentThought
// per-step generation_id bug: a file keyed by Cursor's suffixed step id
// (`<turn-uuid>-<n>-<xxxx>`) that holds no prompt, tools, assistant text,
// tokens, pending stop, or model.
//
// Bare turn-level generation IDs are never dropped here — even when
// metadata_only left only thinkingPresent on disk — so a real aborted turn
// still gets swept.
func isThoughtOnlyFragment(frag *fragment.Fragment) bool {
	if frag == nil {
		return false
	}
	// Only step-id orphans. Collapsed/new turns use the bare turn id.
	if turnGenerationID(frag.GenerationID) == frag.GenerationID {
		return false
	}
	if len(frag.Tools) > 0 ||
		len(frag.Assistant) > 0 ||
		strings.TrimSpace(frag.UserPrompt) != "" ||
		frag.PendingStop != nil ||
		frag.TokenUsage != nil ||
		frag.Model != "" ||
		frag.Provider != "" {
		return false
	}
	return true
}

func removeAndLog(conversationID string, logger *log.Logger) {
	if err := fragment.RemoveConversationDir(conversationID); err != nil {
		logger.Printf("sessionEnd: cleanup conv=%s: %v", conversationID, err)
		return
	}
	logger.Printf("sessionEnd: cleaned conv=%s", conversationID)
}
