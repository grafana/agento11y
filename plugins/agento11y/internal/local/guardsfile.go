package local

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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

type guardsFileResponse struct {
	Path      string           `json:"path,omitempty"`
	Exists    bool             `json:"exists"`
	Enabled   bool             `json:"enabled"`
	Errors    []string         `json:"errors,omitempty"`
	Enforcing int              `json:"enforcing"`
	Packs     []guardPack      `json:"packs"`
	Rules     []guardeval.Rule `json:"rules"`
}

type guardsFileRequest struct {
	Enabled *bool           `json:"enabled"`
	Packs   map[string]bool `json:"packs"`
}

func (s *Server) handleGetGuards(w http.ResponseWriter, _ *http.Request) {
	s.writeGuardsResponse(w)
}

func (s *Server) handlePutGuards(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxHookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req guardsFileRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Enabled == nil && len(req.Packs) == 0 {
		http.Error(w, "enabled or packs is required", http.StatusBadRequest)
		return
	}
	// Turning the master switch off also turns every catalog pack off so a
	// later re-enable does not silently start enforcing the previous set.
	disableGuards := req.Enabled != nil && !*req.Enabled
	if disableGuards {
		if req.Packs == nil {
			req.Packs = make(map[string]bool, len(catalogPacks()))
		}
		for _, p := range catalogPacks() {
			req.Packs[p.ID] = false
		}
	}
	if req.Enabled != nil && s.configPath == "" {
		http.Error(w, "config persistence disabled", http.StatusServiceUnavailable)
		return
	}
	if len(req.Packs) > 0 && s.guards.RulesPath == "" {
		http.Error(w, "guards persistence disabled", http.StatusServiceUnavailable)
		return
	}
	// Write packs before the enabled flag. If both are in the request and the
	// toml write fails, leaving GUARDS_ENABLED already flipped would disable
	// (or enable) hooks against a file that still has the old packs.
	var next []guardeval.Rule
	wrotePacks := false
	if len(req.Packs) > 0 {
		s.guardsMu.Lock()
		rules, _, errs, err := readGuardRules(s.guards.RulesPath)
		if err != nil {
			s.guardsMu.Unlock()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(errs) > 0 {
			s.guardsMu.Unlock()
			// Disable is the escape hatch from a broken file. Skip the pack
			// rewrite so GUARDS_ENABLED can still flip off.
			if !disableGuards {
				http.Error(w, "guards.toml is invalid; fix it before changing packs: "+strings.Join(errs, "; "), http.StatusBadRequest)
				return
			}
		} else {
			next, err = applyPackUpdates(rules, req.Packs)
			if err != nil {
				s.guardsMu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := guardeval.WriteRules(s.guards.RulesPath, next); err != nil {
				s.guardsMu.Unlock()
				s.logger.Printf("local: write guards: %v", err)
				http.Error(w, "write guards: "+err.Error(), http.StatusInternalServerError)
				return
			}
			s.guardsEngine = nil
			s.guardsDigest = [sha256.Size]byte{}
			wrotePacks = true
			s.guardsMu.Unlock()
		}
	}
	if req.Enabled != nil {
		value := "false"
		if *req.Enabled {
			value = "true"
		}
		s.configMu.Lock()
		err := dotenv.UpdateDotenv(s.configPath, func(stored map[string]string) map[string]string {
			return envconfig.UpdateExistingLegacyAliases(stored, map[string]string{
				"AGENTO11Y_GUARDS_ENABLED": value,
			})
		}, s.logger)
		s.configMu.Unlock()
		if err != nil {
			s.logger.Printf("local: write guards enabled: %v", err)
			http.Error(w, "write config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if wrotePacks {
		s.guardsMu.Lock()
		s.writeGuardsPutOKLocked(w, next)
		s.guardsMu.Unlock()
		return
	}
	// After GUARDS_ENABLED has been written, never fail the response on a
	// broken guards.toml: the UI treats a non-OK PUT as "switch unchanged".
	s.writeGuardsResponse(w)
}

func (s *Server) writeGuardsResponse(w http.ResponseWriter) {
	s.guardsMu.Lock()
	defer s.guardsMu.Unlock()
	s.writeGuardsResponseLocked(w)
}

func (s *Server) writeGuardsResponseLocked(w http.ResponseWriter) {
	path := s.guards.RulesPath
	rules, exists, decodeErrs, err := readGuardRules(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	engine := guardeval.NewEngineFromContents(path, mustReadGuards(path), s.guards.Logger)
	if rules == nil {
		rules = []guardeval.Rule{}
	}
	errors := engine.Status().Errors
	if len(errors) == 0 && len(decodeErrs) > 0 {
		errors = decodeErrs
	}
	s.writeJSON(w, http.StatusOK, guardsFileResponse{
		Path:      displayConfigPath(path),
		Exists:    exists,
		Enabled:   guardsEnvEnabled(s.configPath, s.logger),
		Errors:    errors,
		Enforcing: engine.Status().Enforcing,
		Packs:     packsFromRules(rules),
		Rules:     rules,
	})
}

// writeGuardsPutOKLocked answers a successful pack write from the ruleset that
// just landed, so a follow-up read error cannot turn a completed mutation into
// HTTP 500.
func (s *Server) writeGuardsPutOKLocked(w http.ResponseWriter, rules []guardeval.Rule) {
	path := s.guards.RulesPath
	if rules == nil {
		rules = []guardeval.Rule{}
	}
	engine := guardeval.NewEngineFromContents(path, mustReadGuards(path), s.guards.Logger)
	s.writeJSON(w, http.StatusOK, guardsFileResponse{
		Path:      displayConfigPath(path),
		Exists:    true,
		Enabled:   guardsEnvEnabled(s.configPath, s.logger),
		Errors:    engine.Status().Errors,
		Enforcing: engine.Status().Enforcing,
		Packs:     packsFromRules(rules),
		Rules:     rules,
	})
}

func mustReadGuards(path string) []byte {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// readGuardRules loads the on-disk ruleset. A missing file is empty, not an
// error. A parse or decode fault is reported in errs with a nil error so GET
// can still render the page; PUT must refuse to write when errs is non-empty,
// or it would rebuild the file from the rules that decoded and drop the rest.
func readGuardRules(path string) (rules []guardeval.Rule, exists bool, errs []string, err error) {
	if path == "" {
		return nil, false, nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, false, nil, err
	}
	raw, err := guardeval.ParseRules(data)
	if err != nil {
		return nil, true, []string{err.Error()}, nil
	}
	decoded, decodeErrs := guardeval.DecodeRules(raw)
	for _, e := range decodeErrs {
		errs = append(errs, e.Error())
	}
	return decoded, true, errs, nil
}
