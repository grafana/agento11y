package guardeval

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

// An Engine is the user's ruleset compiled and ordered, ready to evaluate hook
// requests. It is immutable once built, so a request in flight always evaluates
// against one coherent ruleset and a reload builds a new Engine.

// Config is what an Engine is built from.
type Config struct {
	// ConfigDir holds config.env and guards.toml, the file that decides
	// what this machine enforces. When set, NewEngine reads that file.
	ConfigDir string
	// RulesPath overrides where the rules are read from. Empty is
	// guards.toml inside ConfigDir. A path set with no ConfigDir is still
	// read; with neither set nothing is.
	RulesPath string
	// Logger receives the problems that also land in Status. nil discards them.
	Logger *log.Logger
}

// Status is what the engine made of the ruleset, for the daemon's status
// endpoint.
type Status struct {
	// Errors are the problems found while reading and compiling: the file did
	// not read or parse, a rule was malformed, a regex did not compile. The
	// rules that did compile still enforce, so a non-empty Errors does not mean
	// nothing is guarding.
	Errors []string
	// Rules counts the compiled rules.
	Rules int
	// Enforcing counts the compiled rules that can act locally: a tool filter, a
	// transform, or a deterministic evaluator. A rule that only references Cloud
	// evaluator_ids loads and never acts, so counting it would report
	// enforcement that cannot happen.
	Enforcing int
}

// Posture is the part of a compiled ruleset that a status report shows, as one
// type so every caller counts and words it the same way. A caller embeds it and
// adds its own fields.
type Posture struct {
	// Path is the rules file.
	Path string `json:"path,omitempty"`
	// Rules counts the compiled rules that can act locally.
	Rules int `json:"rules"`
	// Error joins every problem with "; ". Rules can still enforce when it is
	// not empty.
	Error string `json:"error,omitempty"`
}

// Posture projects the status onto the fields a report shows. path is the rules
// file, which the status does not itself carry.
func (s Status) Posture(path string) Posture {
	return Posture{
		Path:  path,
		Rules: s.Enforcing,
		Error: strings.Join(s.Errors, "; "),
	}
}

// Engine holds the compiled ruleset in evaluation order.
type Engine struct {
	rules  []CompiledRule
	logger *log.Logger
	status Status
}

// NewRulesEngine compiles one explicit ruleset, with no file on disk. It is how
// a caller outside this package evaluates a ruleset it holds in memory. logger
// may be nil.
func NewRulesEngine(raw []json.RawMessage, logger *log.Logger) *Engine {
	e := newEngine(logger)
	e.compile(raw)
	return e
}

// NewEngine reads and compiles the configured rules file. It never fails: a
// malformed rule contributes nothing and is reported through Status, so a typo
// in one rule cannot disarm the rest. A file that does not read or parse
// compiles to an empty ruleset rather than an error, which is the same
// fail-open posture the loader takes.
func NewEngine(cfg Config) *Engine {
	e := newEngine(cfg.Logger)
	path := userRulesPath(cfg)
	raw, err := readRules(path)
	if err != nil {
		e.problem(err.Error())
		return e
	}
	e.compile(raw)
	return e
}

func newEngine(logger *log.Logger) *Engine {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Engine{logger: logger}
}

// problem records a problem in the status and logs it. A problem the daemon
// only records in a struct nobody prints is a problem nobody sees.
func (e *Engine) problem(msg string) {
	e.status.Errors = append(e.status.Errors, msg)
	e.logger.Printf("local guards: %s", msg)
}

// compile decodes, validates and compiles the raw rules into the engine. The
// step order matters: decode rule by rule so one malformed object does not stop
// the rest, validate ids before compiling so a duplicate is named as written,
// drop disabled rules, then compile.
func (e *Engine) compile(raw []json.RawMessage) {
	rules, decodeErrs := DecodeRules(raw)
	for _, err := range decodeErrs {
		e.problem(err.Error())
	}
	if err := ValidateRuleIDs(rules); err != nil {
		e.problem(err.Error())
	}

	enabled := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		if rule.Enabled != nil && !*rule.Enabled {
			continue
		}
		rule.RuleID = strings.TrimSpace(rule.RuleID)
		enabled = append(enabled, rule)
	}

	compiled, errs := compileGuardRules(enabled)
	for _, err := range errs {
		e.problem(err.Error())
	}
	sortCompiledRules(compiled)
	e.rules = compiled
	e.status.Rules = len(compiled)
	for _, rule := range compiled {
		if ruleEnforceable(rule) {
			e.status.Enforcing++
		}
	}
}

// userRulesPath is the file the rules are read from: the override, or
// guards.toml next to config.env. Empty when neither is set, and empty
// opens no file, because a relative default would resolve against the working
// directory of whoever started the process.
func userRulesPath(cfg Config) string {
	if strings.TrimSpace(cfg.RulesPath) != "" {
		return cfg.RulesPath
	}
	if strings.TrimSpace(cfg.ConfigDir) == "" {
		return ""
	}
	return filepath.Join(cfg.ConfigDir, ConfigFile)
}

// readRules reads and parses the rules file. A missing file is no rules and no
// complaint, which is the machine that has written none. An empty path opens
// nothing.
func readRules(path string) ([]json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %v (no rules)", path, err)
	}
	raw, err := ParseRules(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %v (no rules)", path, err)
	}
	return raw, nil
}

// Evaluate runs the ruleset for one hook request. A nil Engine allows
// everything, so a caller that has not compiled one need not branch.
func (e *Engine) Evaluate(req agento11y.HookEvaluateRequest) Response {
	resp, _ := e.EvaluateWithTransform(req)
	return resp
}

// EvaluateWithTransform is Evaluate, also returning the transform the matching
// rules applied so a caller can re-run those patterns over an input another
// stage rewrote. See evaluateWithTransform for the ordering rules.
func (e *Engine) EvaluateWithTransform(req agento11y.HookEvaluateRequest) (Response, *Transform) {
	if e == nil {
		return evaluateWithTransform(nil, nil, req)
	}
	return evaluateWithTransform(e.rules, e.logger, req)
}

// Status reports what the engine compiled. The value is a copy: a caller
// rendering it cannot reach into the ruleset a live daemon is evaluating.
func (e *Engine) Status() Status {
	if e == nil {
		return Status{}
	}
	out := e.status
	out.Errors = append([]string(nil), e.status.Errors...)
	return out
}
