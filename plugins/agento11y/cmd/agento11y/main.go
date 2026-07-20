// Command agento11y is the single binary used by the Claude Code, Codex,
// Copilot, Cursor, OpenCode, pi, and Vibe agent plugins. The CLI itself
// lives in internal/entry so the legacy cmd/sigil entrypoint can share it.
package main

import "github.com/grafana/agento11y/plugins/agento11y/internal/entry"

// version is overridden via -ldflags "-X main.version=..." at build time.
// It lives in the main package so the -X flag stays independent of the
// module's import path.
var version = "dev"

func main() { entry.Main(version) }
