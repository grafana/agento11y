// SDK_VERSION is the released version of the Sigil JS SDK. It is stamped into
// the default generation-export User-Agent (see userAgent). Keep in sync with
// package.json on release.
export const SDK_VERSION = '0.6.0';

const SDK_USER_AGENT_PRODUCT = 'sigil-sdk-js';

/**
 * Returns the SDK's default generation-export User-Agent product token,
 * `sigil-sdk-js/<SDK_VERSION>`. Coding-agent plugins prepend their own token
 * (most-specific first).
 */
export function userAgent(): string {
  return `${SDK_USER_AGENT_PRODUCT}/${SDK_VERSION}`;
}
