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
