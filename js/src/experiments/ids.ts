import { sha1Hex } from '../utils.js';

/** The separator `stable_id` joins parts with in every SDK. */
const SEPARATOR = '\u001f';

/**
 * Deterministic identifier from `parts`, so a retry reuses the same row.
 *
 * The derivation is a cross-SDK contract and must stay byte-identical with Go's
 * `StableID` and Python's `stable_id`: SHA-1 over the parts joined with `\x1f`,
 * lowercase hex truncated to sixteen characters, prefixed with the kind and a
 * hyphen. A `null` or `undefined` part keeps its separator slot, so dropping a
 * blank field would produce a different id.
 *
 * Numbers use JavaScript's integer string form, so an attempt of `1` renders as
 * `1`, matching Python's `str(1)` and Go's `%v`. A float such as `1.0` is a
 * different input and yields a different id.
 */
export function stableId(prefix: string, ...parts: unknown[]): string {
  const joined = parts.map(stringifyPart).join(SEPARATOR);
  return `${prefix}-${sha1Hex(joined).slice(0, 16)}`;
}

function stringifyPart(part: unknown): string {
  if (part === null || part === undefined) {
    return '';
  }
  return String(part);
}
