/**
 * True when `err` looks like a node:fs ENOENT (missing file/directory).
 * Used to swallow expected "file not found" cases from optional config reads.
 */
export function isMissingFileError(err: unknown): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    "code" in err &&
    (err as { code?: string }).code === "ENOENT"
  );
}
