// SDK_VERSION is the version of the Agento11y JavaScript SDK, stamped into
// the default generation-export User-Agent (see userAgent). The placeholder
// assignment below is rewritten with the package.json version by
// scripts/stamp-version.mjs after every tsc build, so runtime code needs no
// filesystem access. Unstamped builds report the same fallback as the other
// SDKs. The explicit string annotation keeps the placeholder literal out of
// the emitted .d.ts.
export const SDK_VERSION: string = '0.0.0+unknown';

const SDK_USER_AGENT_PRODUCT = 'agento11y-sdk-js';

/**
 * Returns the SDK's default generation-export User-Agent product token,
 * `agento11y-sdk-js/<SDK_VERSION>`. Coding-agent plugins prepend their own token
 * (most-specific first).
 */
export function userAgent(): string {
  return `${SDK_USER_AGENT_PRODUCT}/${SDK_VERSION}`;
}
