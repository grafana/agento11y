package config

import (
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Config holds copilot-side knobs the agent adapter needs after dotenv has
// been applied. Endpoint, auth, and SIGIL_TAGS are read by the SDK directly.
// SIGIL_DEBUG is consumed by cli.InitLogger in the single-binary entrypoint
// before this struct is built, so it does not appear here.
type Config struct {
	ContentCapture agento11y.ContentCaptureMode
	// SkipPromptRedaction exports the user prompt without redaction. Set from
	// AGENTO11Y_REDACT_INPUT_MESSAGES=false, so the zero value redacts.
	SkipPromptRedaction bool
	Guards              envconfig.GuardsConfig
	// AgentName is the identity every generation and guard request reports.
	// Load resolves it from AGENTO11Y_AGENT_NAME, then SIGIL_AGENT_NAME, and
	// falls back to "copilot". Read it through Agent.
	AgentName string
}

// Agent returns the resolved agent identity, or "copilot" when AgentName is
// blank because the Config was built without Load. The mapper applies the same
// fallback, so a guard request and the generation it guards cannot disagree.
func (c Config) Agent() string {
	if n := strings.TrimSpace(c.AgentName); n != "" {
		return n
	}
	return mapper.AgentName
}

// HasCredentials reports whether the canonical SIGIL_* credentials are
// populated. Delegates to the shared dotenv helper for parity across agents.
func HasCredentials() bool {
	return dotenv.HasCredentials()
}

// Load returns the copilot-local subset of config from OS env. Call
// dotenv.ApplyEnv(logger) first so dotenv-only values are
// reflected in the OS env.
func Load(logger *log.Logger) Config {
	return Config{
		ContentCapture:      envconfig.ResolveContentMode(logger),
		SkipPromptRedaction: !envconfig.ResolveRedactInput(logger),
		Guards:              envconfig.ResolveGuards(logger),
		AgentName:           envconfig.ResolveAgentName(mapper.AgentName),
	}
}
