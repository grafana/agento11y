#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT_DIR="${1:-${ROOT_DIR}/go/proto}"
GO_PKG="github.com/grafana/sigil-sdk/go/proto/sigil/v1"

GOPATH_BIN="$(go env GOPATH 2>/dev/null || echo "${HOME}/go")/bin"
PATH="${GOPATH_BIN}:${PATH}"

missing=()
for tool in protoc protoc-gen-go protoc-gen-go-grpc; do
  command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
done

if [[ ${#missing[@]} -gt 0 ]]; then
  cat >&2 <<EOF
Missing required tools: ${missing[*]}

The pinned versions live in mise.toml. Install them with:
  mise install

Or install manually:
  protoc:             https://protobuf.dev/installation/  (pinned to 34.1)
  protoc-gen-go:      go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
  protoc-gen-go-grpc: go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1
EOF
  exit 1
fi

mkdir -p "${OUT_DIR}"

protoc \
  -I"${ROOT_DIR}/proto" \
  --go_out="${OUT_DIR}" \
  --go_opt=paths=source_relative \
  --go_opt="Msigil/v1/generation_ingest.proto=${GO_PKG}" \
  --go-grpc_out="${OUT_DIR}" \
  --go-grpc_opt=paths=source_relative \
  --go-grpc_opt="Msigil/v1/generation_ingest.proto=${GO_PKG}" \
  "${ROOT_DIR}/proto/sigil/v1/generation_ingest.proto"
