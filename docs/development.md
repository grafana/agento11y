# Development

Notes for contributors working in this repo.

## Regenerating protobuf stubs

The canonical proto lives at [`proto/sigil/v1/generation_ingest.proto`](../proto/sigil/v1/generation_ingest.proto). After editing it, regenerate every language's stubs from the repo root:

```bash
mise run generate:proto
```

This runs three subtasks:

| Task | Outputs | Tooling |
| --- | --- | --- |
| `generate:proto:go` | `go/proto/sigil/v1/*.pb.go` | `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` |
| `generate:proto:python` | `python/sigil_sdk/internal/gen/sigil/v1/*_pb2*.py` | `grpcio-tools` (auto-fetched via `uv` if needed) |
| `generate:proto:js` | `js/proto/sigil/v1/*.proto` | none — copies the proto for the runtime loader |

The Go stubs live under the Go SDK module at `github.com/grafana/sigil-sdk/go/proto/sigil/v1` so external producers can import the wire schema without importing the SDK client package. They share the Go SDK module version.

Java and .NET compile the proto on build (gradle protobuf plugin and `Grpc.Tools` respectively), so they pick up changes automatically once the canonical `.proto` is updated.

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
