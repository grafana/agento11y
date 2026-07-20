// Command sigil is the old name of the agento11y binary. It is kept so
// existing installs keep working and will be removed later. See
// cmd/agento11y and internal/entry for the actual CLI.
package main

import "github.com/grafana/agento11y/plugins/agento11y/internal/entry"

// version is overridden via -ldflags "-X main.version=..." at build time.
// It lives in the main package so the -X flag stays independent of the
// module's import path.
var version = "dev"

func main() { entry.Main(version) }
