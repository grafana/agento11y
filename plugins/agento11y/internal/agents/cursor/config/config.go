// Package config resolves the cursor adapter's runtime knobs from OS env
// and the shared agento11y dotenv file.
//
// Cursor launches hooks under a stripped environment (shell rc files do not
// run), so the dotenv is the reliable place to put credentials. ApplyEnv
// copies dotenv values into the process env so the SDK's own canonical
// SIGIL_* env-var resolution sees them too.
package config

import (
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Config holds the cursor-side knobs the plugin needs before constructing
// the SDK client. Endpoint, auth, and SIGIL_TAGS are read by the SDK
// directly. OTel transport (OTEL_EXPORTER_OTLP_*) is read by the
// OpenTelemetry SDK exporters natively, so this struct doesn't carry it.
type Config struct {
	ContentCapture agento11y.ContentCaptureMode
	UserIDOverride string
	// SkipPromptRedaction exports the user prompt without redaction. Set from
	// AGENTO11Y_REDACT_INPUT_MESSAGES=false, so the zero value redacts.
	SkipPromptRedaction bool
	Debug               bool
	// AgentName is the identity every generation and guard request reports.
	// Load resolves it from AGENTO11Y_AGENT_NAME, then SIGIL_AGENT_NAME, and
	// falls back to "cursor". Read it through Agent.
	AgentName string
}

// Agent returns the resolved agent identity, or "cursor" when AgentName is
// blank because the Config was built without Load. The mapper applies the same
// fallback, so a guard request and the generation it guards cannot disagree.
func (c Config) Agent() string {
	if n := strings.TrimSpace(c.AgentName); n != "" {
		return n
	}
	return mapper.AgentName
}

// HasCredentials reports whether the canonical SIGIL_* credentials are
// populated in the OS env.
func HasCredentials() bool {
	return dotenv.HasCredentials()
}

// FilePath is the dotenv config path for the consolidated binary.
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

// Load resolves the cursor-local subset of config from OS env. Call
// ApplyEnv first so dotenv-only values are reflected in the OS env.
func Load(logger *log.Logger) Config {
	return Config{
		ContentCapture:      envconfig.ResolveContentMode(logger),
		UserIDOverride:      envconfig.Getenv("USER_ID"),
		SkipPromptRedaction: !envconfig.ResolveRedactInput(logger),
		Debug:               envconfig.ParseBool(envconfig.Getenv("DEBUG")),
		AgentName:           envconfig.ResolveAgentName(mapper.AgentName),
	}
}
