package guardeval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// The rules file is TOML so a hand-written rule can keep a comment next to the
// pattern it explains, and so a regex can be a literal string rather than the
// double-escaped JSON form. The local viewer also writes this file: a save
// re-encodes the ruleset and drops comments, which is the same trade-off
// config.env already makes.
//
// Rules are converted to JSON once, at the read boundary, and the rest of the
// package works in JSON: the compile path is shared with rules that arrive over
// the wire, and a raw JSON rule round-trips fields the local evaluator ignores
// instead of dropping them.

// ConfigFile is the rules file that sits next to config.env.
const ConfigFile = "guards.toml"

// rulesKey is the array-of-tables the rules are read from. The file is a
// document rather than a bare list so it has room to grow other sections.
const rulesKey = "rules"

// FilePath is the ruleset that sits next to the given config.env. An empty
// config path yields an empty ruleset path, which the engine reads as no rules.
func FilePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), ConfigFile)
}

// ParseRules decodes a rules file into raw JSON rule objects. An empty document
// is an empty ruleset, not an error: a file holding only comments is a file the
// user is still writing.
func ParseRules(data []byte) ([]json.RawMessage, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	entry, ok := doc[rulesKey]
	if !ok {
		return nil, nil
	}
	items, ok := entry.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be a list of rules, written [[%s]]", rulesKey, rulesKey)
	}

	out := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a table", rulesKey, i)
		}
		if match, ok := fields["match"].(map[string]any); ok {
			fields["match"] = flattenMatch(match)
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", rulesKey, i, err)
		}
		out = append(out, encoded)
	}
	return out, nil
}

// flattenMatch turns the nested tables TOML produces into the dotted keys the
// matcher reads, so [rules.match.model] name = "..." arrives as "model.name".
// Nesting is what TOML gives you for free: writing the dotted key directly
// would need quoting, since an unquoted dot means a sub-table. A key that
// already holds a dot passes through, so both spellings work.
func flattenMatch(match map[string]any) map[string]any {
	out := make(map[string]any, len(match))
	for key, value := range match {
		nested, ok := value.(map[string]any)
		if !ok {
			out[key] = value
			continue
		}
		for suffix, leaf := range flattenMatch(nested) {
			out[key+"."+suffix] = leaf
		}
	}
	return out
}

const rulesFileHeader = "# Local guard rules. Saving from Settings → Local rewrites this file.\n\n"

// EncodeRules writes a ruleset as a guards.toml document. Empty input is a
// comment-only file, which ParseRules reads as an empty ruleset. Every rule is
// stamped postflight when phase is unset, which is the only phase the local
// viewer writes.
//
// Nested fields use dotted keys and inline tables so a save keeps the compact
// hand-written shape (`tool_filter.blocked_names = [...]`,
// `transform.patterns = [{ ... }]`). go-toml's default marshaler expands those
// into [rules.tool_filter] tables instead.
func EncodeRules(rules []Rule) ([]byte, error) {
	if len(rules) == 0 {
		return []byte(rulesFileHeader), nil
	}
	var b strings.Builder
	b.WriteString(rulesFileHeader)
	for i, rule := range rules {
		if i > 0 {
			b.WriteByte('\n')
		}
		if err := writeCompactRule(&b, rule); err != nil {
			return nil, err
		}
	}
	return []byte(b.String()), nil
}

func writeCompactRule(b *strings.Builder, rule Rule) error {
	phase := strings.TrimSpace(rule.Phase)
	if phase == "" {
		phase = hookPhasePostflight
	}
	b.WriteString("[[rules]]\n")
	writeTomlKey(b, "rule_id", rule.RuleID)
	if rule.Enabled != nil {
		writeTomlKey(b, "enabled", *rule.Enabled)
	}
	writeTomlKey(b, "phase", phase)
	if rule.Priority != 0 {
		writeTomlKey(b, "priority", rule.Priority)
	}
	if action := strings.TrimSpace(rule.ActionOnFail); action != "" {
		writeTomlKey(b, "action_on_fail", action)
	}
	if len(rule.extra) > 0 {
		keys := make([]string, 0, len(rule.extra))
		for key := range rule.extra {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeTomlKey(b, key, rule.extra[key])
		}
	}
	if len(rule.Match) > 0 {
		keys := make([]string, 0, len(rule.Match))
		for key := range rule.Match {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			b.WriteString("match.")
			b.WriteString(encodeTomlKeyPart(key))
			b.WriteString(" = ")
			b.WriteString(encodeTomlValue(rule.Match[key]))
			b.WriteByte('\n')
		}
	}
	if rule.ToolFilter != nil && len(rule.ToolFilter.BlockedNames) > 0 {
		writeTomlKey(b, "tool_filter.blocked_names", rule.ToolFilter.BlockedNames)
	}
	if rule.Transform != nil && rule.Transform.JSONMode != "" {
		writeTomlKey(b, "transform.json_mode", rule.Transform.JSONMode)
	}
	if rule.Transform != nil && len(rule.Transform.Patterns) > 0 {
		b.WriteString("transform.patterns = [")
		for i, p := range rule.Transform.Patterns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("{ ")
			first := true
			if id := strings.TrimSpace(p.ID); id != "" {
				b.WriteString("id = ")
				b.WriteString(encodeTomlString(id))
				first = false
			}
			if !first {
				b.WriteString(", ")
			}
			b.WriteString("regex = ")
			b.WriteString(encodeTomlString(p.Regex))
			if p.Replacement != "" {
				b.WriteString(", replacement = ")
				b.WriteString(encodeTomlString(p.Replacement))
			}
			b.WriteString(" }")
		}
		b.WriteString("]\n")
	}
	for _, ev := range rule.Evaluators {
		b.WriteString("\n[[rules.evaluators]]\n")
		writeTomlKey(b, "kind", ev.Kind)
		if len(ev.Config) == 0 {
			continue
		}
		keys := make([]string, 0, len(ev.Config))
		for key := range ev.Config {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeTomlKey(b, "config."+key, ev.Config[key])
		}
	}
	return nil
}

func writeTomlKey(b *strings.Builder, key string, value any) {
	b.WriteString(encodeTomlKeyPath(key))
	b.WriteString(" = ")
	b.WriteString(encodeTomlValue(value))
	b.WriteByte('\n')
}

func encodeTomlValue(value any) string {
	switch v := value.(type) {
	case nil:
		return `""`
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int(v)) {
			return strconv.Itoa(int(v))
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case string:
		return encodeTomlString(v)
	case []string:
		items := make([]any, len(v))
		for i, s := range v {
			items[i] = s
		}
		return encodeTomlArray(items)
	case []any:
		return encodeTomlArray(v)
	case map[string]any:
		return encodeTomlInlineTable(v)
	default:
		return encodeTomlString(fmt.Sprint(v))
	}
}

func encodeTomlArray(items []any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(encodeTomlValue(item))
	}
	b.WriteByte(']')
	return b.String()
}

func encodeTomlInlineTable(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(encodeTomlKeyPart(key))
		b.WriteString(" = ")
		b.WriteString(encodeTomlValue(m[key]))
	}
	b.WriteByte('}')
	return b.String()
}

func encodeTomlKeyPath(key string) string {
	parts := strings.Split(key, ".")
	for i, part := range parts {
		parts[i] = encodeTomlKeyPart(part)
	}
	return strings.Join(parts, ".")
}

func encodeTomlKeyPart(part string) string {
	if isBareTomlKey(part) {
		return part
	}
	return encodeTomlString(part)
}

func isBareTomlKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// encodeTomlString quotes a value the way a hand-written guards.toml does:
// literal quotes when the string holds backslashes (regexes), otherwise a
// basic double-quoted string.
func encodeTomlString(s string) string {
	if strings.Contains(s, `\`) && !strings.ContainsAny(s, "'\n\r") && !hasCtl(s) {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func hasCtl(s string) bool {
	for _, r := range s {
		if r < 0x20 {
			return true
		}
	}
	return false
}

// WriteRules encodes rules and writes them to path with 0600 permissions,
// creating the parent directory if needed.
func WriteRules(path string, rules []Rule) error {
	if path == "" {
		return fmt.Errorf("no guards.toml path")
	}
	data, err := EncodeRules(rules)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "guards-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
