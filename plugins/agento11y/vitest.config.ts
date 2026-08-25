import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';

const themeCSSModule = '\0theme-css-source';
const themeCSSPath = fileURLToPath(new URL('./internal/local/web/app.css', import.meta.url));

export default defineConfig({
  plugins: [
    {
      name: 'theme-css-raw-source',
      resolveId(id) {
        if (id === 'virtual:theme-css-source') return themeCSSModule;
      },
      load(id) {
        // Vitest stubs CSS imports. The theme contract needs the source text.
        if (id === themeCSSModule) return `export default ${JSON.stringify(readFileSync(themeCSSPath, 'utf8'))};`;
      },
    },
  ],
  // JSX comes from tsconfig.json (react-jsx), which is the same automatic
  // runtime the Go bundler compiles with. There it resolves to the shim over
  // the vendored UMD build; here it resolves to the react package on disk.
  test: {
    include: ['tests/**/*.test.ts', 'tests/**/*.test.tsx'],
    environment: 'happy-dom',
  },
});
