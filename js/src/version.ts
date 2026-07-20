// SDK_VERSION is the released version of the Sigil JS SDK. It is stamped into
// the default generation-export User-Agent (see userAgent). Keep in sync with
// package.json on release.
export const SDK_VERSION = '0.6.0';

// The User-Agent product token is allowlisted by the ingest server, so it
// intentionally keeps the pre-rename name. Do not update it for the
// sigil-sdk -> agento11y rename without server-side dual-read support.
const SDK_USER_AGENT_PRODUCT = 'sigil-sdk-js';

/**
 * Returns the SDK's default generation-export User-Agent product token,
 * `sigil-sdk-js/<SDK_VERSION>`. Coding-agent plugins prepend their own token
 * (most-specific first).
 */
export function userAgent(): string {
  return `${SDK_USER_AGENT_PRODUCT}/${SDK_VERSION}`;
}
