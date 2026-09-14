package guardeval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

// `shell_command` projects command strings from known shell tools' JSON
// arguments. Unlike flattened targets, it removes JSON escaping before
// matching. It does not parse shell syntax, strip wrappers, expand variables,
// or decode payloads. Quoted strings and heredoc bodies remain in the literal
// text and can match.

// defaultShellToolNames are the tool names treated as "runs a shell command"
// with no configuration. They cover the spellings the supported hosts emit
// (Claude Code's Bash, Cursor's run_terminal_cmd, Codex/Copilot's
// execute_command / run_command, and generic terminal / shell). Matching is
// case-insensitive, so "Bash" and "bash" are the same tool.
var defaultShellToolNames = []string{"bash", "shell", "run_terminal_cmd", "execute_command", "run_command", "terminal", "powershell", "pwsh"}

// defaultShellCommandKeys are the argument keys that hold the command line,
// in the order they are tried. The first key present with a non-empty string
// value wins.
var defaultShellCommandKeys = []string{"command", "cmd", "script"}

type shellConfig struct {
	// toolNames is lowercased for case-insensitive comparison.
	toolNames []string
	// commandKeys is lowercased and ordered by precedence.
	commandKeys []string
}

func parseShellConfig(config map[string]any) (shellConfig, error) {
	out := shellConfig{
		toolNames:   append([]string(nil), defaultShellToolNames...),
		commandKeys: append([]string(nil), defaultShellCommandKeys...),
	}
	if config == nil {
		return out, nil
	}
	if raw, ok := config["tool_names"]; ok {
		names, err := cfgStringList(raw, "tool_names")
		if err != nil {
			return shellConfig{}, err
		}
		out.toolNames = lowerCopy(names)
	}
	if raw, ok := config["command_keys"]; ok {
		keys, err := cfgStringList(raw, "command_keys")
		if err != nil {
			return shellConfig{}, err
		}
		out.commandKeys = lowerCopy(keys)
	}
	return out, nil
}

func cfgStringList(raw any, field string) ([]string, error) {
	switch typed := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, fmt.Errorf("%s has no value", field)
		}
		return []string{trimmed}, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, v := range typed {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s entries must be strings", field)
			}
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s has no value", field)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or a list of strings", field)
	}
}

func lowerCopy(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// matchesTool reports whether a tool name is one of the shell tools, comparing
// case-insensitively: hosts disagree on capitalization ("Bash" vs "bash") and a
// rule must not depend on which host produced the call.
func (c shellConfig) matchesTool(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	for _, candidate := range c.toolNames {
		if strings.EqualFold(trimmed, candidate) {
			return true
		}
	}
	return false
}

// shellCommands projects the shell commands of the tool call under evaluation,
// one entry per call, in the order they appear. It reads in.Output only: a
// verdict must not depend on a command from earlier in the session, which the
// prompt history in in.Messages carries.
//
// An input with no shell tool call in it projects nothing, and the caller skips
// the evaluator rather than judging it against no text.
func shellCommands(in agento11y.HookInput, cfg shellConfig) []string {
	return appendShellCommands(nil, in.Output, cfg)
}

func appendShellCommands(out []string, messages []agento11y.Message, cfg shellConfig) []string {
	for _, m := range messages {
		for i := range m.Parts {
			if command, ok := shellCommandOf(m.Parts[i].ToolCall, cfg); ok {
				out = append(out, command)
			}
		}
	}
	return out
}

// shellCommandOf projects one tool call. It reports false for a tool that is
// not a shell tool, for arguments that are not a JSON object, and for an object
// with no command key holding a non-empty string. All three mean "this call
// carries no command line", not "the command is empty".
func shellCommandOf(tc *agento11y.ToolCall, cfg shellConfig) (string, bool) {
	if tc == nil || !cfg.matchesTool(tc.Name) {
		return "", false
	}
	var args map[string]any
	if err := json.Unmarshal(tc.InputJSON, &args); err != nil {
		return "", false
	}
	for _, key := range cfg.commandKeys {
		raw, ok := lookupArg(args, key)
		if !ok {
			continue
		}
		// A string is used as-is. An argv array is joined with spaces so a
		// pattern written against `rm -rf /` still sees a Codex/Copilot
		// payload that sent ["rm","-rf","/"]. Quoting is not reconstructed:
		// this is a match subject, not a shell. Any other type falls through
		// to the next key.
		if command, ok := commandString(raw); ok {
			return command, true
		}
	}
	return "", false
}

// lookupArg finds an argument by key, exact match first and then
// case-insensitively. The fallback scans in sorted order so a payload carrying
// two keys that differ only in case resolves the same way on every run.
func lookupArg(args map[string]any, key string) (any, bool) {
	if v, ok := args[key]; ok {
		return v, true
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, key) {
			return args[k], true
		}
	}
	return nil, false
}

func commandString(raw any) (string, bool) {
	switch typed := raw.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed, true
		}
	case []any:
		if len(typed) == 0 {
			return "", false
		}
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
		if joined := strings.TrimSpace(strings.Join(parts, " ")); joined != "" {
			return joined, true
		}
	}
	return "", false
}
