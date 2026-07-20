// Package useragent builds the generation-export User-Agent for the sigil
// binary's coding-agent plugins. Each plugin identifies itself with its own
// most-specific token followed by the SDK default, e.g.
// "sigil-plugin-claude-code/<ver> sigil-sdk-go/<ver>".
package useragent

import "github.com/grafana/agento11y/go/sigil"

// SigilVersion is the sigil binary build version, set once from main.
var SigilVersion = "dev"

// For returns the generation-export User-Agent for an agent plugin:
// "sigil-plugin-<agent>/<SigilVersion> sigil-sdk-go/<sdkVersion>".
func For(agent string) string {
	return "sigil-plugin-" + agent + "/" + SigilVersion + " " + sigil.UserAgent()
}
