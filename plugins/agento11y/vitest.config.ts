import { defineConfig } from 'vitest/config';

export default defineConfig({
  // JSX comes from tsconfig.json (react-jsx), which is the same automatic
  // runtime the Go bundler compiles with. There it resolves to the shim over
  // the vendored UMD build; here it resolves to the react package on disk.
  test: {
    include: ['tests/**/*.test.ts', 'tests/**/*.test.tsx'],
    environment: 'happy-dom',
  },
});
