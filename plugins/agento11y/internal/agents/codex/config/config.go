package config

import (
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Config holds codex-side knobs the agent adapter needs after dotenv has
// been applied. Endpoint, auth, and SIGIL_TAGS are read by the SDK directly.
type Config struct {
	ContentCapture agento11y.ContentCaptureMode
	// SkipPromptRedaction exports the user prompt without redaction. Set from
	// AGENTO11Y_REDACT_INPUT_MESSAGES=false, so the zero value redacts.
	SkipPromptRedaction bool
	Debug               bool
	Guards              envconfig.GuardsConfig
	// AgentName is the identity every generation and guard request reports.
	// Load resolves it from AGENTO11Y_AGENT_NAME, then SIGIL_AGENT_NAME, and
	// falls back to "codex". Read it through Agent.
	AgentName string
}

// Agent returns the resolved agent identity, or "codex" when AgentName is
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

// FilePath returns the dotenv config path for the consolidated binary.
func FilePath() string {
	return dotenv.FilePath()
}

// ApplyEnv loads the shared agento11y dotenv config and writes keys whose OS
// env value is empty.
func ApplyEnv(logger *log.Logger) map[string]string {
	return dotenv.ApplyEnv(logger)
}

// LoadDotenv parses a dotenv file at path. Exported for tests that need
// to drive the parser directly.
func LoadDotenv(path string, logger *log.Logger) map[string]string {
	return dotenv.LoadDotenv(path, logger)
}

// AllowedDotenvKey forwards to the shared dotenv allow-list.
func AllowedDotenvKey(key string) bool {
	return dotenv.AllowedDotenvKey(key)
}

// Load returns the codex-local subset of config from OS env. Call ApplyEnv
// first so dotenv-only values are reflected in the OS env.
func Load(logger *log.Logger) Config {
	return Config{
		ContentCapture:      envconfig.ResolveContentMode(logger),
		SkipPromptRedaction: !envconfig.ResolveRedactInput(logger),
		Debug:               envconfig.ParseBool(envconfig.Getenv("DEBUG")),
		Guards:              envconfig.ResolveGuards(logger),
		AgentName:           envconfig.ResolveAgentName(mapper.AgentName),
	}
}
