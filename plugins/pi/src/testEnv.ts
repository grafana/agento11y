import { resetSigilDotenvStateForTests } from "./sigilDotenv.js";

/**
 * Reset every SIGIL_*, SIGIL_PI_*, OTEL_* env var and XDG_CONFIG_HOME between
 * test cases so on-disk fixtures (config.env, JSON config) and shell exports
 * cannot leak across cases. Used by both config.test.ts and sigilDotenv.test.ts.
 *
 * Also clears the dotenv loader's per-process "owned keys" tracking so a
 * test that calls applySigilDotenv doesn't poison the next case's view of
 * which env values came from the shell vs. config.env.
 */
export function clearSigilEnv(): void {
  for (const key of Object.keys(process.env)) {
    if (
      key.startsWith("SIGIL_PI_") ||
      key.startsWith("SIGIL_") ||
      key.startsWith("OTEL_")
    ) {
      delete process.env[key];
    }
  }
  delete process.env.XDG_CONFIG_HOME;
  resetSigilDotenvStateForTests();
}
