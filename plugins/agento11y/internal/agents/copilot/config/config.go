package config

import (
	"log"

	"github.com/grafana/agento11y/go/sigil"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Config holds copilot-side knobs the agent adapter needs after dotenv has
// been applied. Endpoint, auth, and SIGIL_TAGS are read by the SDK directly.
// SIGIL_DEBUG is consumed by cli.InitLogger in the single-binary entrypoint
// before this struct is built, so it does not appear here.
type Config struct {
	ContentCapture sigil.ContentCaptureMode
	Guards         envconfig.GuardsConfig
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
		ContentCapture: envconfig.ResolveContentMode(logger),
		Guards:         envconfig.ResolveGuards(logger),
	}
}
