package agento11y

import (
	"strconv"
	"strings"
)

// Reserved generation metadata keys for Anthropic-style cache diagnostics.
const (
	CacheDiagnosticsMissReasonKey        = "agento11y.cache_diagnostics.miss_reason"
	CacheDiagnosticsMissedInputTokensKey = "agento11y.cache_diagnostics.missed_input_tokens"
	CacheDiagnosticsPreviousMessageIDKey = "agento11y.cache_diagnostics.previous_message_id"
)

// CacheDiagnosticsOption configures optional fields for SetCacheDiagnostics.
type CacheDiagnosticsOption func(*cacheDiagnosticsConfig)

type cacheDiagnosticsConfig struct {
	missedInputTokens *int64
	previousMessageID string
}

// WithMissedInputTokens sets the estimated tokens after the divergence point.
func WithMissedInputTokens(n int64) CacheDiagnosticsOption {
	return func(c *cacheDiagnosticsConfig) {
		c.missedInputTokens = &n
	}
}

// WithPreviousMessageID sets the provider response ID compared against.
func WithPreviousMessageID(id string) CacheDiagnosticsOption {
	return func(c *cacheDiagnosticsConfig) {
		c.previousMessageID = id
	}
}

// SetCacheDiagnostics stamps cache diagnostic metadata on the generation recorder.
// missReason must be non-empty (e.g. model_changed, system_changed). Optional fields
// are only written when their options are set (non-empty previous_message_id).
//
// It is safe to call on a nil recorder. Call before GenerationRecorder.End, typically
// after the provider response is available and before or with SetResult.
func SetCacheDiagnostics(rec *GenerationRecorder, missReason string, opts ...CacheDiagnosticsOption) {
	if rec == nil {
		return
	}
	missReason = strings.TrimSpace(missReason)
	if missReason == "" {
		return
	}

	var cfg cacheDiagnosticsConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.ended {
		return
	}
	if rec.extraMetadata == nil {
		rec.extraMetadata = make(map[string]any, 3)
	}
	// Replace prior optional keys so a second call does not leave stale values.
	delete(rec.extraMetadata, CacheDiagnosticsMissedInputTokensKey)
	delete(rec.extraMetadata, CacheDiagnosticsPreviousMessageIDKey)
	rec.extraMetadata[CacheDiagnosticsMissReasonKey] = missReason
	if cfg.missedInputTokens != nil {
		rec.extraMetadata[CacheDiagnosticsMissedInputTokensKey] = strconv.FormatInt(*cfg.missedInputTokens, 10)
	}
	if id := strings.TrimSpace(cfg.previousMessageID); id != "" {
		rec.extraMetadata[CacheDiagnosticsPreviousMessageIDKey] = id
	}
}
