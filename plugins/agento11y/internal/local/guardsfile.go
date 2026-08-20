package local

import (
	"log"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
)

func (s *Server) localGuardsStatus() localGuardsStatus {
	engine := guardeval.NewEngine(s.guards)
	return localGuardsStatus{
		Posture: engine.Status().Posture(displayConfigPath(s.guards.RulesPath)),
		Enabled: guardsEnvEnabled(s.configPath, s.logger),
	}
}

// guardsEnvEnabled reports whether config.env enables guards. It reads the file
// rather than os.Getenv because the daemon froze its own GUARDS_ENABLED at
// launch, and the "Enable guards" button writes config.env. ParseSettings gives
// the same AGENTO11Y_-then-SIGIL_ resolution the Settings page uses.
func guardsEnvEnabled(configEnvPath string, logger *log.Logger) bool {
	return ParseSettings(dotenv.LoadDotenv(configEnvPath, logger)).Guards != guardsOff
}
