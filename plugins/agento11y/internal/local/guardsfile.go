package local

import (
	"crypto/sha256"
	"log"
	"os"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
)

func (s *Server) localGuardsStatus() localGuardsStatus {
	engine := s.localEngine()
	return localGuardsStatus{
		Posture: engine.Status().Posture(displayConfigPath(s.guards.RulesPath)),
		Enabled: guardsEnvEnabled(s.configPath, s.logger),
	}
}

// localEngine returns the compiled ruleset for the current guards.toml
// contents. Missing files compile to an empty ruleset and are cached as such.
// Other read errors are not cached, so a later request retries.
func (s *Server) localEngine() *guardeval.Engine {
	path := s.guards.RulesPath
	var data []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return guardeval.NewEngine(s.guards)
			}
		} else {
			data = b
		}
	}
	sum := sha256.Sum256(data)
	s.guardsMu.Lock()
	defer s.guardsMu.Unlock()
	if s.guardsEngine != nil && s.guardsDigest == sum {
		return s.guardsEngine
	}
	engine := guardeval.NewEngineFromContents(path, data, s.guards.Logger)
	s.guardsEngine = engine
	s.guardsDigest = sum
	return engine
}

// guardsEnvEnabled reports whether config.env enables guards. It reads the file
// rather than os.Getenv because the daemon froze its own GUARDS_ENABLED at
// launch, and the "Enable guards" button writes config.env. ParseSettings gives
// the same AGENTO11Y_-then-SIGIL_ resolution the Settings page uses.
func guardsEnvEnabled(configEnvPath string, logger *log.Logger) bool {
	return ParseSettings(dotenv.LoadDotenv(configEnvPath, logger)).Guards != guardsOff
}
