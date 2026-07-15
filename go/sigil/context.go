package sigil

import (
	"context"
	"maps"
)

type conversationIDContextKey struct{}
type conversationTitleContextKey struct{}
type userIDContextKey struct{}
type agentNameContextKey struct{}
type agentVersionContextKey struct{}
type tagsContextKey struct{}
type contentCaptureModeContextKey struct{}
type experimentRunContextKey struct{}
type experimentRunIDContextKey struct{}

// WithConversationID stores a conversation ID in the context.
// StartGeneration, StartStreamingGeneration, and StartToolExecution read it when
// the explicit field is empty.
func WithConversationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationIDContextKey{}, id)
}

// ConversationIDFromContext retrieves the conversation ID stored by WithConversationID.
func ConversationIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(conversationIDContextKey{}).(string)
	return id, ok && id != ""
}

// WithConversationTitle stores a conversation title in the context.
// StartGeneration, StartStreamingGeneration, and StartToolExecution read it when
// the explicit field is empty.
func WithConversationTitle(ctx context.Context, title string) context.Context {
	return context.WithValue(ctx, conversationTitleContextKey{}, title)
}

// ConversationTitleFromContext retrieves the conversation title stored by WithConversationTitle.
func ConversationTitleFromContext(ctx context.Context) (string, bool) {
	title, ok := ctx.Value(conversationTitleContextKey{}).(string)
	return title, ok && title != ""
}

// WithUserID stores a user ID in the context.
// StartGeneration and StartStreamingGeneration read it when the explicit field
// is empty.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDContextKey{}, userID)
}

// UserIDFromContext retrieves the user ID stored by WithUserID.
func UserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDContextKey{}).(string)
	return userID, ok && userID != ""
}

// WithAgentName stores an agent name in the context.
// StartGeneration, StartStreamingGeneration, and StartToolExecution read it when
// the explicit field is empty.
func WithAgentName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, agentNameContextKey{}, name)
}

// AgentNameFromContext retrieves the agent name stored by WithAgentName.
func AgentNameFromContext(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(agentNameContextKey{}).(string)
	return name, ok && name != ""
}

// WithAgentVersion stores an agent version in the context.
// StartGeneration, StartStreamingGeneration, and StartToolExecution read it when
// the explicit field is empty.
func WithAgentVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, agentVersionContextKey{}, version)
}

// AgentVersionFromContext retrieves the agent version stored by WithAgentVersion.
func AgentVersionFromContext(ctx context.Context) (string, bool) {
	version, ok := ctx.Value(agentVersionContextKey{}).(string)
	return version, ok && version != ""
}

// WithTag stores a single per-request tag in the context. Unlike the
// GenerationStart.Tags field (which is export-only), context tags are treated
// as dimensions: StartGeneration and StartStreamingGeneration merge them into
// the generation's tags for export AND emit them on the generation span and
// metrics as sigil.tag.<key>, alongside the static Config.Tags.
//
// Successive WithTag / WithTags calls accumulate; a later call overrides an
// earlier value for the same key. An empty key is ignored.
func WithTag(ctx context.Context, key, value string) context.Context {
	if key == "" {
		return ctx
	}
	merged := cloneContextTags(ctx)
	merged[key] = value
	return context.WithValue(ctx, tagsContextKey{}, merged)
}

// WithTags stores multiple per-request tags in the context. See WithTag for
// how context tags propagate. Entries with an empty key are ignored; a nil or
// empty map is a no-op.
func WithTags(ctx context.Context, tags map[string]string) context.Context {
	if len(tags) == 0 {
		return ctx
	}
	merged := cloneContextTags(ctx)
	for key, value := range tags {
		if key == "" {
			continue
		}
		merged[key] = value
	}
	return context.WithValue(ctx, tagsContextKey{}, merged)
}

// TagsFromContext returns a copy of the per-request tags stored by WithTag /
// WithTags, or nil when none are set.
func TagsFromContext(ctx context.Context) map[string]string {
	existing, ok := ctx.Value(tagsContextKey{}).(map[string]string)
	if !ok || len(existing) == 0 {
		return nil
	}
	return maps.Clone(existing)
}

func cloneContextTags(ctx context.Context) map[string]string {
	if existing, ok := ctx.Value(tagsContextKey{}).(map[string]string); ok && len(existing) > 0 {
		return maps.Clone(existing)
	}
	return map[string]string{}
}

// withContentCaptureMode stores the resolved ContentCaptureMode in the context.
// StartToolExecution reads it to inherit the parent generation's mode.
func withContentCaptureMode(ctx context.Context, mode ContentCaptureMode) context.Context {
	return context.WithValue(ctx, contentCaptureModeContextKey{}, mode)
}

// contentCaptureModeFromContext retrieves the ContentCaptureMode stored by
// withContentCaptureMode. Returns ContentCaptureModeDefault and false if not set.
func contentCaptureModeFromContext(ctx context.Context) (ContentCaptureMode, bool) {
	mode, ok := ctx.Value(contentCaptureModeContextKey{}).(ContentCaptureMode)
	return mode, ok
}

func withExperimentRun(ctx context.Context, run *ExperimentRun) context.Context {
	return context.WithValue(ctx, experimentRunContextKey{}, run)
}

func experimentRunFromContext(ctx context.Context) (*ExperimentRun, bool) {
	run, ok := ctx.Value(experimentRunContextKey{}).(*ExperimentRun)
	return run, ok && run != nil
}

// WithExperimentRunID stores a Sigil experiment run ID in the context.
// StartGeneration and StartStreamingGeneration read it and tag normal
// instrumentation with experiment.run_id / experiment_run_id.
//
// Use ExperimentRun.Context when the experiment runner and instrumented code
// are in the same process; it also captures generation IDs for score export.
// Use WithExperimentRunID when only the run ID crosses a process boundary, such
// as an HTTP request into an already-instrumented service.
func WithExperimentRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, experimentRunIDContextKey{}, runID)
}

// ExperimentRunIDFromContext retrieves the experiment run ID stored by
// WithExperimentRunID.
func ExperimentRunIDFromContext(ctx context.Context) (string, bool) {
	runID, ok := ctx.Value(experimentRunIDContextKey{}).(string)
	return runID, ok && runID != ""
}
