# Development

Notes for contributors working in this repo.

## Hermes plugin

[`plugins/hermes`](../plugins/hermes/README.md) is the independent Python distribution `agento11y-hermes`, with module `agento11y_hermes`. Install in Hermes's Python environment, not through the shared launcher.

Run these tasks from the repository root:

```sh
mise run format:py:plugin-hermes
mise run lint:py:plugin-hermes
mise run typecheck:py:plugin-hermes
mise run test:py:plugin-hermes
mise run test:py:plugin-hermes-release
mise run build:py:plugin-hermes
```

The build task validates the wheel and source distribution, including package identity, version, dependencies, entry point, and the full Apache license. Root `mise run check` includes artifact validation. CI tests Python 3.11, 3.12, 3.13, and 3.14 with branch coverage enabled and a 99% minimum. Keep the plugin's `uv.lock` synchronized with dependencies.

Tests must run without inherited Cloud/provider credentials or personal configuration. Real-Hermes tests are optional and separate from the normal checks. Read the [e2e skill](../plugins/hermes/.agents/skills/e2e-test/SKILL.md) and inspect scripts before running them. Use an explicit loopback model provider. Route both telemetry channels to loopback receivers, or disable unused channels. A local OTLP sink alone does not prevent paid model calls.

### Hermes releases

Hermes has its own version in `plugins/hermes/pyproject.toml` and `uv.lock`. Do not include it in Python SDK version bumps. The release line in `.github/sdk-releases.json` uses `plugins/hermes/v*` tags.

Before the first release, a PyPI owner must configure a [pending Trusted Publisher](https://docs.pypi.org/trusted-publishers/creating-a-project-through-oidc/) for the new project:

| Setting | Value |
| --- | --- |
| PyPI project | `agento11y-hermes` |
| GitHub owner | `grafana` |
| Repository | `agento11y` |
| Workflow filename | `hermes-publish.yml` |
| Environment | `pypi` |

A pending publisher does not reserve the package name. Use the intended Grafana maintainer account; project ownership and GitHub publishing permission are separate. Confirm the GitHub `pypi` environment permits `plugins/hermes/v*` tags. Configure required reviewers without blocking the SDK workflows that also use this environment. These are administrative changes, not effects of merging repository files.

The first activation PR includes version `0.11.0`, its changelog section, release registration, and the publishing workflow together. Configure PyPI before merging it. Do not register Hermes against the imported `0.10.0` heading: the shared tagger reads every release line on any changelog push.

For subsequent releases:

1. Run **Publish Hermes to PyPI** on `main`, choosing a patch, minor, or major bump. Keep `dry-run` enabled to preview without remote writes.
2. Run again with `dry-run` disabled to open a release PR. Review and merge that PR; the workflow does not auto-merge.
3. `tag-releases-on-merge.yml` creates the tag at the merge commit. The Hermes workflow tests the tag, checks that it is reachable from `main`, and validates matching tag, manifest, changelog, and artifact versions.
4. The publishing job uploads the exact validated wheel and sdist through the `pypi` environment. `sdk-github-releases.yml` creates the GitHub release notes independently.
5. Verify the PyPI artifacts and installation inside Hermes's environment. The old distribution must be uninstalled first; pip does not treat a different project name as an upgrade.

Keep the existing `grafana-agento11y-hermes` project and historical releases. Remove its old Trusted Publisher only after the replacement release is verified. Do not delete or reuse published filenames.

To retain locally validated upload artifacts, pass an empty output directory:

```sh
bash plugins/hermes/scripts/run-check.sh build --expected-version 0.11.0 --output-dir /tmp/hermes-dist
```

The checker includes the full `plugins/hermes/LICENSE` in both artifacts and validates a wheel rebuilt from the sdist. Keep that license identical to the repository-root `LICENSE`.

## Regenerating protobuf stubs

The proto lives at [`proto/agento11y/v1/generation_ingest.proto`](../proto/agento11y/v1/generation_ingest.proto). After editing it, regenerate every language's stubs from the repo root:

```bash
mise run generate:proto
```

This runs three subtasks:

| Task | Outputs | Tooling |
| --- | --- | --- |
| `generate:proto:go` | `go/proto/agento11y/v1/*.pb.go` | `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` |
| `generate:proto:python` | `python/agento11y/internal/gen/agento11y/v1/*_pb2*.py` | `grpcio-tools` (auto-fetched via `uv` if needed) |
| `generate:proto:js` | `js/proto/agento11y/v1/*.proto` | none — copies the proto for the runtime loader |

The Go stubs live under the Go SDK module at `github.com/grafana/agento11y/go/proto/agento11y/v1` so external producers can import the wire schema without importing the SDK client package. They share the Go SDK module version.

Java and .NET compile the proto on build (gradle protobuf plugin and `Grpc.Tools` respectively), so they pick up changes automatically once the `.proto` is updated.

### Pinned tool versions

All codegen tools are pinned in [`mise.toml`](../mise.toml) so regenerated stubs are byte-identical across machines and CI:

| Tool | Version | Where it's pinned |
| --- | --- | --- |
| `protoc` | `34.1` | `[tools]` |
| `protoc-gen-go` | `v1.36.11` | `[tools]` (go install) |
| `protoc-gen-go-grpc` | `v1.6.1` | `[tools]` (go install) |
| `grpcio-tools` (Python) | `1.80.0` | `SIGIL_GRPCIO_TOOLS_VERSION` env |
| `protobuf` (Python) | `6.31.1` | `SIGIL_PROTOBUF_VERSION` env |

Install everything with:

```bash
mise install
```

Go and Python pins match the runtime deps in `go/go.mod` and `python/pyproject.toml`. Bumping a generator version means regenerating the stubs and committing the diff.

### Drift check

`mise run check:proto` regenerates the Go, Python, and JS stubs into a temporary directory and diffs them against the committed tree. It runs in CI as the `Protobuf drift` job and fails the build if anyone edits the proto without running `mise run generate:proto`, or if the local tool versions don't match the pins above.

### Manual installs (no mise)

If you prefer not to use `mise`:

```bash
# Go tools
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

# protoc — install 34.1 via your package manager:
#   brew install protobuf            # macOS
#   apt install protobuf-compiler    # Debian/Ubuntu (version varies)

# Python tools
python3 -m pip install grpcio-tools==1.80.0 protobuf==6.31.1
```

The Python script prefers an existing Python that already has `grpcio-tools` installed (`PYTHON_BIN`, defaults to `python3`); otherwise it falls back to `uv run --with grpcio-tools==<pinned> --with protobuf==<pinned>`. Install [uv](https://docs.astral.sh/uv/) and you don't need to install the Python tools globally.

## Regenerating secret-redaction patterns

The secret patterns live at [`redaction/patterns.json`](../redaction/patterns.json). It is the only editable copy. After editing it, regenerate every engine's table from the repo root:

```bash
mise run generate:redaction
```

That writes five files:

| Output | Consumer |
| --- | --- |
| `go/agento11y/redaction_patterns_gen.go` | Go SDK |
| `python/agento11y/_redaction_patterns.py` | Python SDK; reused by the Hermes plugin's SDK redaction |
| `js/src/redaction-patterns.generated.ts` | JS SDK and, through `@grafana/agento11y-core`, the opencode plugin |
| `dotnet/src/Grafana.Agento11y/RedactionPatterns.g.cs` | .NET SDK |
| `plugins/agento11y/internal/redact/patterns_gen.go` | shared `agento11y` binary |

The generator validates the table before it writes anything. It rejects:

- Lookahead, lookbehind and backreferences, which RE2 cannot compile.
- `\s`, `\w` and `\d`, which mean ASCII in Go and JavaScript and Unicode in Python and .NET.
- A capturing group in a tier 1 pattern. Every engine maps the index of the matched group back to a pattern id, and an extra group shifts that mapping.

`redaction/README.md` lists the rules in full.

The generator formats its Go, Python and TypeScript output with `gofmt`, `ruff format` and `biome`, all through stdin, so those three tools must be on `PATH` (`mise install` provides them). The drift job has no .NET SDK, so the generator emits the C# output already formatted. `mise run lint:cs` checks it with a second `dotnet format` pass that names the file, because `dotnet format` skips `*.g.cs` by default.

The plugin gets its own generated table rather than importing the SDK's: `plugins/agento11y` pins a released SDK version and builds with `GOWORK=off`, so importing would tie it to the SDK release cadence.

### Drift check

`mise run check:redaction` runs the generator's invariant tests, regenerates every table into a temporary directory, and diffs them against the committed tree. It diffs the paths the generator prints, so adding an output target needs no change here. It runs in CI in the `Protobuf drift` job and fails the build if anyone edits a generated table by hand or edits `patterns.json` without regenerating.

### Conformance

`mise run test:sdk:redaction-conformance` runs the shared fixtures in `redaction/fixtures/` through all six engines. [`redaction/README.md`](../redaction/README.md) covers how to add a pattern and what the fixtures assert.
