package skilluri

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

func Scrub(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.ContainsFunc(raw, func(r rune) bool { return r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return ""
	}
	// net/url does not validate escapes in the query, even though it is discarded below.
	if _, err := url.PathUnescape(raw); err != nil {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return ""
	}
	authority := strings.TrimPrefix(raw[len(u.Scheme)+1:], "//")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	// net/url accepts unescaped @ characters in userinfo.
	if strings.Count(authority, "@") > 1 {
		return ""
	}
	// A source must be an absolute location, not just a scheme or relative path.
	if strings.EqualFold(u.Scheme, "file") {
		if !strings.HasPrefix(u.Path, "/") {
			return ""
		}
		u.Scheme = "file"
		u.OmitHost = false
	} else if u.Host == "" {
		return ""
	}
	if u.Host != "" {
		if u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
			return ""
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 0 || n > 65535 {
				return ""
			}
		}
	} else if u.User != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
