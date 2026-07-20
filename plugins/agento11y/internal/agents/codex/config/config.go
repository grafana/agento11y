package config

import (
	"log"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Config holds codex-side knobs the agent adapter needs after dotenv has
// been applied. Endpoint, auth, and SIGIL_TAGS are read by the SDK directly.
type Config struct {
	ContentCapture agento11y.ContentCaptureMode
	Debug          bool
	Guards         envconfig.GuardsConfig
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
		ContentCapture: envconfig.ResolveContentMode(logger),
		Debug:          envconfig.ParseBool(envconfig.Getenv("DEBUG")),
		Guards:         envconfig.ResolveGuards(logger),
	}
}
