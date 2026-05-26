import { readFileSync, realpathSync } from "node:fs";
import { dirname, join } from "node:path";

// Pi ships under two package names; both are valid host installs.
const SUPPORTED_PACKAGE_NAMES = new Set([
  "@mariozechner/pi-coding-agent",
  "@earendil-works/pi-coding-agent",
]);

/**
 * Returns the host pi package version, or `undefined` when no matching
 * package is found or any filesystem operation fails.
 */
export function detectPiVersion(): string | undefined {
  try {
    const entry = process.argv[1];
    if (!entry) return undefined;
    let dir = dirname(realpathSync(entry));
    while (true) {
      try {
        const pkg = JSON.parse(
          readFileSync(join(dir, "package.json"), "utf-8"),
        ) as { name?: string; version?: string };
        if (pkg.name && SUPPORTED_PACKAGE_NAMES.has(pkg.name)) {
          return pkg.version;
        }
      } catch {
        // No package.json at this level (or unreadable); keep walking.
      }
      const parent = dirname(dir);
      if (parent === dir) return undefined;
      dir = parent;
    }
  } catch {
    return undefined;
  }
}
