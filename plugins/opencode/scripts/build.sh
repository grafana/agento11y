#!/usr/bin/env bash
set -euo pipefail

mise run build:ts:sdk-js

tsc --noEmit

npx esbuild src/index.ts \
  --bundle \
  --format=esm \
  --platform=node \
  --target=es2022 \
  --outfile=dist/index.js \
  --external:@opencode-ai/plugin \
  --external:@opencode-ai/sdk

tsc --project tsconfig.build.json --emitDeclarationOnly --declaration --declarationMap --outDir dist
