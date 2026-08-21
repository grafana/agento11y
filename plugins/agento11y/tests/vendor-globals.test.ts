import * as npmReact from 'react';
import * as npmReactDOMClient from 'react-dom/client';
import { beforeAll, describe, expect, it } from 'vitest';
import markdownUMD from '../internal/local/web/vendor/markdown-to-jsx.js?raw';
import reactUMD from '../internal/local/web/vendor/react.production.min.js?raw';
import reactDOMUMD from '../internal/local/web/vendor/react-dom.production.min.js?raw';

// The one seam neither gate sees. The Go bundle resolves `react`,
// `react-dom/client` and `markdown-to-jsx` to shims over the globals the
// vendored UMD scripts set, while tsc and every other test here resolve the
// same imports to the npm packages in node_modules. So a vendored build that
// does not carry what the source imports is a blank page in the browser with
// all three gates green.
//
// These tests load the vendored builds the way index.html does and check them
// against the npm packages the rest of the suite runs on. A version bump that
// updates plugins/agento11y/package.json without re-running
// `mise run local:vendor-assets` fails here.

interface VendoredGlobals {
  React?: typeof npmReact;
  ReactDOM?: typeof npmReactDOMClient & { version?: string };
  MarkdownToJSX?: { Markdown?: unknown; compiler?: unknown };
}

// The npm builds are CommonJS, so their namespace object carries interop keys
// the UMD global has no reason to hold.
function exportedNames(namespace: object): string[] {
  return Object.keys(namespace).filter((key) => /^[A-Za-z_$][\w$]*$/.test(key) && key !== 'default');
}

function load(source: string) {
  // The UMD wrappers write to the global when they find no CommonJS module
  // object, which is what a <script> tag gives them and what a function body
  // evaluated here gives them too.
  new Function(source)();
}

const globals = globalThis as VendoredGlobals;

beforeAll(() => {
  load(reactUMD);
  load(reactDOMUMD);
  // markdown-to-jsx reads the React global at load time, which is why
  // index.html loads it last of the three.
  load(markdownUMD);
});

describe('the vendored builds carry what the shims hand the bundle', () => {
  it('sets the three globals', () => {
    expect(globals.React).toBeTruthy();
    expect(globals.ReactDOM).toBeTruthy();
    expect(globals.MarkdownToJSX).toBeTruthy();
  });

  // react-dom's UMD build reports its release-candidate build string
  // ('18.3.1-next-…') where the npm build reports the plain version, so the
  // vendored copy is checked for the pinned version as a prefix.
  it('matches the pinned react version', () => {
    expect(globals.React?.version).toBe(npmReact.version);
    expect(globals.ReactDOM?.version).toMatch(new RegExp(`^${npmReact.version}(-|$)`));
  });

  it('exports every name the npm react does', () => {
    const vendored = globals.React as unknown as Record<string, unknown>;
    // An empty list would make the loop below pass over anything.
    expect(exportedNames(npmReact).length).toBeGreaterThan(10);
    for (const name of exportedNames(npmReact)) {
      expect(vendored, `React.${name} is missing from the vendored build`).toHaveProperty(name);
    }
  });

  it('exports the react-dom/client entry points', () => {
    const vendored = globals.ReactDOM as unknown as Record<string, unknown>;
    expect(exportedNames(npmReactDOMClient)).toContain('createRoot');
    for (const name of exportedNames(npmReactDOMClient)) {
      expect(vendored, `ReactDOM.${name} is missing from the vendored build`).toHaveProperty(name);
    }
  });

  it('exports the markdown-to-jsx component ProseBlock renders', () => {
    expect(globals.MarkdownToJSX?.Markdown).toBeTypeOf('function');
  });
});
