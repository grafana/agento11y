#!/usr/bin/env bash
set -euo pipefail

mise run build:ts:sdk-js

pnpm exec tsc --noEmit

pnpm exec esbuild src/index.ts src/server.ts \
  --bundle \
  --format=esm \
  --platform=node \
  --target=es2022 \
  --banner:js="import { createRequire as __cr } from 'node:module'; const require = __cr(import.meta.url);" \
  --outdir=dist \
  --external:@opencode-ai/plugin \
  --external:@opencode-ai/sdk \
  --external:@opencode/plugin

pnpm exec tsc --project tsconfig.build.json --emitDeclarationOnly --declaration --declarationMap --outDir dist
