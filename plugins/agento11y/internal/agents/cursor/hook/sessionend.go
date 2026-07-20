package hook

import (
	"context"
	"log"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
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

	ids := fragment.ListFragmentIDs(p.ConversationID, logger)

	if len(ids) == 0 {
		removeAndLog(p.ConversationID, logger)
		return
	}

	if !config.HasCredentials() {
		logger.Print("sessionEnd: missing credentials — wiping conversation dir without emission")
		removeAndLog(p.ConversationID, logger)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	providers := setupOTelIfConfigured(ctx, p.ConversationID, logger)
	defer func() { _ = providers.Shutdown(ctx) }()

	client := buildClient(cfg, providers)
	session := fragment.LoadSession(p.ConversationID, logger)

	allEmitted := true
	for _, gid := range ids {
		if !emitOneStranded(ctx, client, session, p.ConversationID, gid, cfg, logger) {
			allEmitted = false
		}
	}

	flushOK := true
	if err := client.Flush(ctx); err != nil {
		flushOK = false
		logger.Printf("sessionEnd: flush: %v", err)
	}
	_ = client.Shutdown(ctx)

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

// emitOneStranded loads, maps, and emits one stranded fragment and deletes
// it on success. Returns true when the fragment reached the export path
// successfully (or was unreadable and quarantined out of the way), false
// when emission failed and the conversation dir should be preserved for
// retry.
func emitOneStranded(
	ctx context.Context,
	client *agento11y.Client,
	session *fragment.Session,
	conversationID, generationID string,
	cfg config.Config,
	logger *log.Logger,
) bool {
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
		// Treat as emitted so the dir cleanup proceeds — the corrupt file
		// has been moved out of the sweep path.
		return true
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
		Fragment:       frag,
		Session:        session,
		Stop:           stop,
		ContentCapture: cfg.ContentCapture,
		UserIDOverride: cfg.UserIDOverride,
		Now:            time.Now(),
	})

	if err := emitGeneration(ctx, client, frag, mapped, logger); err != nil {
		logger.Printf("sessionEnd: emit gen=%s: %v", generationID, err)
		return false
	}

	// Delete on per-fragment success so a later sweep doesn't re-emit it as
	// a duplicate if a sibling generation later fails.
	if err := fragment.Delete(conversationID, generationID); err != nil {
		logger.Printf("sessionEnd: delete gen=%s: %v", generationID, err)
	}
	logger.Printf("sessionEnd: swept gen=%s stopReason=%s", generationID, mapped.StopStatus)
	return true
}

func removeAndLog(conversationID string, logger *log.Logger) {
	if err := fragment.RemoveConversationDir(conversationID); err != nil {
		logger.Printf("sessionEnd: cleanup conv=%s: %v", conversationID, err)
		return
	}
	logger.Printf("sessionEnd: cleaned conv=%s", conversationID)
}
