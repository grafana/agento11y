package anthropic

import "maps"

// Option configures Anthropic mapper behavior.
type Option func(*mapperOptions)

type mapperOptions struct {
	providerName      string
	conversationID    string
	conversationTitle string
	agentName         string
	agentVersion      string
	tags              map[string]string
	metadata          map[string]any

	includeRequestArtifact  bool
	includeResponseArtifact bool
	includeToolsArtifact    bool
	includeEventsArtifact   bool
}

func defaultOptions() mapperOptions {
	return mapperOptions{
		providerName: "anthropic",
	}
}

func applyOptions(options []Option) mapperOptions {
	out := defaultOptions()
	for _, option := range options {
		option(&out)
	}
	return out
}

// WithProviderName overrides the provider name stored in Generation.Model.Provider.
func WithProviderName(provider string) Option {
	return func(options *mapperOptions) {
		options.providerName = provider
	}
}

// WithConversationID sets Generation.ConversationID.
func WithConversationID(conversationID string) Option {
	return func(options *mapperOptions) {
		options.conversationID = conversationID
	}
}

// WithConversationTitle sets Generation.ConversationTitle.
func WithConversationTitle(conversationTitle string) Option {
	return func(options *mapperOptions) {
		options.conversationTitle = conversationTitle
	}
}

// WithAgentName sets Generation.AgentName.
func WithAgentName(agentName string) Option {
	return func(options *mapperOptions) {
		options.agentName = agentName
	}
}

// WithAgentVersion sets Generation.AgentVersion.
func WithAgentVersion(agentVersion string) Option {
	return func(options *mapperOptions) {
		options.agentVersion = agentVersion
	}
}

// WithTags merges all provided tags into Generation.Tags.
func WithTags(tags map[string]string) Option {
	return func(options *mapperOptions) {
		if len(tags) == 0 {
			return
		}
		if options.tags == nil {
			options.tags = make(map[string]string, len(tags))
		}
		maps.Copy(options.tags, tags)
	}
}

// WithTag sets one key/value tag in Generation.Tags.
func WithTag(key, value string) Option {
	return func(options *mapperOptions) {
		if options.tags == nil {
			options.tags = map[string]string{}
		}
		options.tags[key] = value
	}
}

// WithMetadata merges all provided values into Generation.Metadata.
func WithMetadata(metadata map[string]any) Option {
	return func(options *mapperOptions) {
		if len(metadata) == 0 {
			return
		}
		if options.metadata == nil {
			options.metadata = make(map[string]any, len(metadata))
		}
		maps.Copy(options.metadata, metadata)
	}
}

// WithRawArtifacts enables all raw provider artifacts for debug workflows.
func WithRawArtifacts() Option {
	return func(options *mapperOptions) {
		options.includeRequestArtifact = true
		options.includeResponseArtifact = true
		options.includeToolsArtifact = true
		options.includeEventsArtifact = true
	}
}

// WithRequestArtifact enables request artifact emission.
func WithRequestArtifact() Option {
	return func(options *mapperOptions) {
		options.includeRequestArtifact = true
	}
}

// WithResponseArtifact enables response artifact emission.
func WithResponseArtifact() Option {
	return func(options *mapperOptions) {
		options.includeResponseArtifact = true
	}
}

// WithToolsArtifact enables tools artifact emission.
func WithToolsArtifact() Option {
	return func(options *mapperOptions) {
		options.includeToolsArtifact = true
	}
}

// WithEventsArtifact enables stream events artifact emission.
func WithEventsArtifact() Option {
	return func(options *mapperOptions) {
		options.includeEventsArtifact = true
	}
}

// WithoutRequestArtifact disables request artifact emission.
func WithoutRequestArtifact() Option {
	return func(options *mapperOptions) {
		options.includeRequestArtifact = false
	}
}

// WithoutResponseArtifact disables response artifact emission.
func WithoutResponseArtifact() Option {
	return func(options *mapperOptions) {
		options.includeResponseArtifact = false
	}
}

// WithoutToolsArtifact disables tools artifact emission.
func WithoutToolsArtifact() Option {
	return func(options *mapperOptions) {
		options.includeToolsArtifact = false
	}
}

// WithoutEventsArtifact disables stream events artifact emission.
func WithoutEventsArtifact() Option {
	return func(options *mapperOptions) {
		options.includeEventsArtifact = false
	}
}
