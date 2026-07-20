// Package useragent builds the generation-export User-Agent for the agento11y
// binary's coding-agent plugins. Each plugin identifies itself with its own
// most-specific token followed by the SDK default, e.g.
// "agento11y-plugin-claude-code/<ver> agento11y-sdk-go/<ver>".
package useragent

import "github.com/grafana/agento11y/go/agento11y"

// Version is the agento11y binary build version, set once from main.
var Version = "dev"

// For returns the generation-export User-Agent for an agent plugin:
// "agento11y-plugin-<agent>/<Version> agento11y-sdk-go/<sdkVersion>".
func For(agent string) string {
	return "agento11y-plugin-" + agent + "/" + Version + " " + agento11y.UserAgent()
}
