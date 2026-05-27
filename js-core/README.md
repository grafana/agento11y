# Grafana Sigil JavaScript Core SDK

Slim JavaScript package for Sigil generation export and core recording APIs.

Use `@grafana/sigil-sdk-js-core` when you want the core `SigilClient` without the provider and framework integration dependencies included by `@grafana/sigil-sdk-js`.

```bash
pnpm add @grafana/sigil-sdk-js-core
```

```ts
import { SigilClient } from "@grafana/sigil-sdk-js-core";

const client = new SigilClient({
  generationExport: {
    protocol: "http",
    endpoint: "https://sigil.example.com",
  },
});
```

The default HTTP export path has no provider SDK dependencies. If you configure `generationExport.protocol: "grpc"`, install the optional gRPC peer packages:

```bash
pnpm add @grpc/grpc-js @grpc/proto-loader
```

## Edge runtime support

The default HTTP export path is designed to load and run in edge-like runtimes (for example Cloudflare Workers, Vercel Edge, Deno Deploy) that do not provide Node-only globals (`process`, `Buffer`) or built-in modules (`node:async_hooks`, `node:crypto`). The gRPC exporter is loaded lazily and is only required when `generationExport.protocol: "grpc"` is set.

A few caveats:

- **Bundler externals for the gRPC code path.** Even though gRPC is dynamically imported, some bundlers (esbuild, webpack, wrangler) statically trace `import('./grpc.js')` and may include `@grpc/grpc-js`, `@grpc/proto-loader`, and `node:*` modules in the output. If you target an edge runtime, mark these as external in your bundler configuration. They are only needed at runtime when you opt in to `protocol: "grpc"`.
- **Context propagation requires `AsyncLocalStorage`.** The `withConversationId`, `withUserId`, `withAgentName`, and similar helpers rely on `node:async_hooks`. In runtimes without it, the helpers become no-ops and the SDK emits a one-time warning. Pass the identifiers explicitly on each `startGeneration` / `startToolExecution` call when running there.
- **`generation.effectiveVersion` requires `node:crypto`.** Setting `effectiveVersion` makes the background exporter compute a SHA-256 digest with `node:crypto`. In runtimes without it the next flush of any generation with this field set will throw. Leave the field unset when running in such a runtime.
