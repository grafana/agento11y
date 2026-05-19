package redact

import (
	"encoding/json"
	"strings"
	"unicode"
)

func (r *Redactor) RedactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err == nil {
		data, _ := json.Marshal(r.redactJSONValue("", v))
		return data
	}
	data, _ := json.Marshal(r.Redact(string(raw)))
	return data
}

func (r *Redactor) RedactJSONForText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return r.Redact(s)
	}
	return string(r.RedactJSON(raw))
}

func (r *Redactor) redactJSONValue(key string, v any) any {
	if isSensitiveJSONKey(key) && !isEmptyJSONValue(v) {
		return "[REDACTED:json-secret-field]"
	}
	switch x := v.(type) {
	case string:
		return r.Redact(x)
	case []any:
		for i := range x {
			x[i] = r.redactJSONValue("", x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[r.Redact(k)] = r.redactJSONValue(k, v)
		}
		return out
	default:
		return x
	}
}

func isSensitiveJSONKey(key string) bool {
	parts := jsonKeyParts(key)
	if len(parts) == 0 {
		return false
	}
	k := strings.Join(parts, "_")
	switch k {
	case "authorization", "proxy_authorization", "cookie", "set_cookie":
		return true
	}
	for _, p := range parts {
		switch p {
		case "password", "passwd", "pwd", "secret", "credential":
			return true
		}
	}
	if isSensitiveTokenKey(parts) {
		return true
	}
	compact := strings.Join(parts, "")
	for _, needle := range []string{
		"password",
		"passwd",
		"secret",
		"credential",
		"apikey",
		"privatekey",
		"accesskey",
		"accesstoken",
		"apitoken",
		"clientsecret",
		"authtoken",
		"bearertoken",
		"idtoken",
		"jwttoken",
		"refreshtoken",
		"sessiontoken",
		"secretkey",
	} {
		if strings.Contains(compact, needle) {
			return true
		}
	}
	return false
}

func jsonKeyParts(key string) []string {
	var parts []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		parts = append(parts, strings.ToLower(string(current)))
		current = current[:0]
	}
	runes := []rune(strings.TrimSpace(key))
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(current) > 0 && startsJSONKeyWord(runes, i) {
			flush()
		}
		current = append(current, r)
	}
	flush()
	return parts
}

func startsJSONKeyWord(runes []rune, i int) bool {
	if i == 0 || !unicode.IsUpper(runes[i]) {
		return false
	}
	prev := runes[i-1]
	if unicode.IsLower(prev) || unicode.IsDigit(prev) {
		return true
	}
	if !unicode.IsUpper(prev) || i+1 >= len(runes) {
		return false
	}
	return unicode.IsLower(runes[i+1])
}

func isSensitiveTokenKey(parts []string) bool {
	for _, part := range parts {
		if part != "token" && part != "tokens" {
			continue
		}
		if !isTokenMetricKey(parts) {
			return true
		}
	}
	return false
}

func isTokenMetricKey(parts []string) bool {
	hasPluralToken := false
	for _, part := range parts {
		switch part {
		case "count", "counts", "usage", "used", "limit", "budget", "remaining":
			return true
		case "tokens":
			hasPluralToken = true
		}
	}
	if !hasPluralToken {
		return false
	}
	for _, part := range parts {
		switch part {
		case "cache", "cached", "candidate", "candidates", "completion", "content", "input", "max", "output", "prompt", "reasoning", "thought", "thoughts", "total":
			return true
		}
	}
	return false
}

func isEmptyJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}
