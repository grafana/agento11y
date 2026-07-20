#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT_DIR="${1:-${ROOT_DIR}/python/agento11y/internal/gen}"
PYTHON_BIN="${PYTHON_BIN:-python3}"

# Pinned via mise.toml; bump both versions together and regenerate stubs.
GRPCIO_TOOLS_VERSION="${SIGIL_GRPCIO_TOOLS_VERSION:-1.80.0}"
PROTOBUF_VERSION="${SIGIL_PROTOBUF_VERSION:-6.31.1}"

if "${PYTHON_BIN}" -c "import grpc_tools" >/dev/null 2>&1; then
  PYTHON=("${PYTHON_BIN}")
elif command -v uv >/dev/null 2>&1; then
  # --no-project so uv resolves a standalone ephemeral env and does not
  # touch python/uv.lock during codegen.
  PYTHON=(uv run --quiet --no-project \
    --with "grpcio-tools==${GRPCIO_TOOLS_VERSION}" \
    --with "protobuf==${PROTOBUF_VERSION}" \
    python)
else
  cat >&2 <<EOF
grpcio-tools is required to regenerate protobuf stubs.
Either install uv (preferred — https://docs.astral.sh/uv/) and re-run,
or install the pinned tools into your Python:
  python3 -m pip install grpcio-tools==${GRPCIO_TOOLS_VERSION} protobuf==${PROTOBUF_VERSION}
EOF
  exit 1
fi

PROTO_INCLUDE="$("${PYTHON[@]}" -c 'import pathlib, grpc_tools; print(pathlib.Path(grpc_tools.__file__).parent / "_proto")')"

mkdir -p "${OUT_DIR}"

"${PYTHON[@]}" -m grpc_tools.protoc \
  -I"${ROOT_DIR}/proto" \
  -I"${PROTO_INCLUDE}" \
  --python_out="${OUT_DIR}" \
  --grpc_python_out="${OUT_DIR}" \
  "${ROOT_DIR}/proto/sigil/v1/generation_ingest.proto"

# The grpc plugin emits absolute import paths; normalize to relative package import.
TMP_FILE="$(mktemp)"
sed 's|from sigil.v1 import generation_ingest_pb2 as|from . import generation_ingest_pb2 as|' \
  "${OUT_DIR}/sigil/v1/generation_ingest_pb2_grpc.py" > "${TMP_FILE}"
mv "${TMP_FILE}" "${OUT_DIR}/sigil/v1/generation_ingest_pb2_grpc.py"

# protoc does not emit package __init__.py files; recreate them so the drift
# check and a fresh regenerate both produce the full committed tree.
cat > "${OUT_DIR}/__init__.py" <<'EOF'
"""Generated protobuf modules."""
EOF
cat > "${OUT_DIR}/sigil/__init__.py" <<'EOF'
"""Generated sigil protobuf namespace."""
EOF
cat > "${OUT_DIR}/sigil/v1/__init__.py" <<'EOF'
"""Generated sigil.v1 protobuf namespace."""
EOF
