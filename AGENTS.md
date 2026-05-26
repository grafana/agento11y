# Working on sigil-sdk

This file is for agents working *on* this repo (the SDKs in `go/`, `python/`, `js/`, `java/`, `dotnet/` and the launchers in `plugins/`).

For agents working in a *consumer* project (instrumenting their app, or installing one of our plugins), point them at [`llms.txt`](llms.txt) instead. That is the file we ship to users.

Read the README and `mise tasks` for the obvious stuff: layout, package names, where languages live. This file only documents what you can't discover by looking.

## Proto is canonical

`proto/sigil/v1/*.proto` is the source of truth. Generated stubs live under each language tree:

- Go: `go/sigil/internal/gen/`
- Python: `python/sigil_sdk/internal/gen/`
- JS: `js/proto/` (the runtime loads `.proto` files directly, no codegen)
- Java, .NET: compiled on build via the gradle protobuf plugin and `Grpc.Tools`; no committed stubs.

Never edit generated files. Edit the `.proto`, then:

```sh
mise run generate:proto
```

CI runs `mise run check:proto` and fails the build if the committed stubs drift from the proto. Tool versions are pinned in `mise.toml` so output is byte-identical across machines. See `docs/development.md` for the full table.

## Workspace gotchas

- The Go workspace (`go.work`) covers `go/`, `go-providers/*`, `go-frameworks/google-adk`, and `plugins/sigil`. Adding a new Go module means updating `go.work` *and* `go.work.sum`. Lint tasks use `GOWORK=off` and iterate per-module via `find . -name go.mod`, so each module must also lint and build on its own.
- The pnpm workspace covers `js/` and `plugins/*`. Use `pnpm --filter <name>` from the root; `mise.toml` does this via tasks like `lint:ts:sdk-js`.
- Java uses a single gradle multi-project rooted at `java/`; modules are listed in `java/settings.gradle.kts`.
- .NET uses a single solution at `dotnet/Sigil.DotNet.slnx`; projects are listed there.

## Plugins layout

`plugins/` ships two flavors of launcher. They are not uniform; don't assume they are.

| Plugin dir | What it actually is |
|------------|---------------------|
| `plugins/sigil/` | The shared Go binary (`brew install grafana/grafana/sigil`). Has subcommands `claude`, `codex`, `copilot`, `cursor`, `pi`, `login`. This is also what consumers use. |
| `plugins/claude-code/`, `plugins/codex/`, `plugins/copilot/`, `plugins/cursor/` | Thin glue: hook scripts and READMEs that wire the host agent to the shared `sigil` binary. No independent code paths. |
| `plugins/opencode/` | Independent npm package `@grafana/sigil-opencode`. Does *not* use the shared binary. |
| `plugins/pi/` | Independent npm package `@grafana/sigil-pi`. Does *not* use the shared binary, but `sigil pi` will install it. |

If you change shared-binary behavior, the four glue plugins all see it. If you change OpenCode or Pi, nothing else moves.

## Cross-language conventions

- Use `cache_write_input_tokens`, not `cache_creation_input_tokens`. This was renamed in cbe0363; pretrained models tend to suggest the old name, so don't follow them.
- Conformance suites cross-check the SDKs. `mise run test:sdk:conformance` runs core, provider-wrapper, and framework-adapter conformance across Go/Python/JS/Java/.NET. If you change behavior in one SDK, expect to update fixtures or matching code in the others.
- Python has one package per framework (`sigil-sdk-langgraph`, `sigil-sdk-openai`, …). JS has one package with subpath exports (`@grafana/sigil-sdk-js/langgraph`). Don't reflexively assume one layout for the other.
- Python version bumps go through `mise run sdk:py:bump <VERSION>`. It updates all 10 `pyproject.toml` files and their internal `sigil-sdk>=…` pins atomically. Hand-editing one file leaves the other nine inconsistent.

## Consumer prompt lives in two places

[`llms.txt`](llms.txt) is what this repo ships. There is a second copy of the same prompt rendered by the AI Observability onboarding wizard (a separate Grafana product). When you change user-facing semantics here (new SDK field, renamed env var, new framework adapter), the wizard copy needs the same change. If you're only fixing this repo's internals, the wizard copy doesn't move.

## Running checks

`mise run check` is the full local CI gate: lint + typecheck + proto-drift + every SDK suite. For a focused change, run the matching narrow task (e.g. `mise run test:py:sdk-langgraph`); the full gate is slow.
