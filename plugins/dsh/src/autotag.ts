// Automatic tags are client tags because only client tags become metric
// labels (docs/concepts/tags-and-metadata.md). Resolve them only after opt-in
// to avoid account and Git lookups. Keep behavior aligned with
// plugins/agento11y/internal/autotag/autotag.go.

import { userInfo } from "node:os";
import { resolveGitBranch, resolveGitRepo } from "./git.js";

export type AutoTag = "user" | "repo" | "branch";

export const AUTO_TAG_ORDER: readonly AutoTag[] = ["user", "repo", "branch"];

export const AUTO_TAG_ALL = "all";

export const AUTO_TAGS_SUFFIX = "AUTO_CODING_AGENT_TAGS";
export const AUTO_TAG_NAMES_SUFFIX = "AUTO_CODING_AGENT_TAGS_NAMES";

/** Limit Prometheus label values by Unicode code points, preserving the prefix. */
export const MAX_AUTO_TAG_VALUE_LENGTH = 128;

/**
 * Reuse `git.branch`: generation tags override this client tag in exports,
 * while the client value remains available as a metric label.
 */
const TAG_KEYS: Record<AutoTag, string> = {
  user: "user",
  repo: "repo",
  branch: "git.branch",
};

export function autoTagKey(name: AutoTag): string {
  return TAG_KEYS[name];
}

export interface ParsedAutoTags {
  enabled: Set<AutoTag>;
  /** Lowercased unsupported names for diagnostics. */
  unknown: string[];
}

/** Matching is case-insensitive; unsupported names are returned lowercased. */
export function parseAutoTags(raw: string): ParsedAutoTags {
  const enabled = new Set<AutoTag>();
  const unknown: string[] = [];
  if (raw.trim() === "") return { enabled, unknown };
  for (const field of raw.split(",")) {
    const name = field.trim().toLowerCase();
    if (name === "") continue;
    if (name === AUTO_TAG_ALL) {
      for (const supported of AUTO_TAG_ORDER) enabled.add(supported);
      continue;
    }
    if (!isAutoTag(name)) {
      if (!unknown.includes(name)) unknown.push(name);
      continue;
    }
    enabled.add(name);
  }
  return { enabled, unknown };
}

function isAutoTag(name: string): name is AutoTag {
  return (AUTO_TAG_ORDER as readonly string[]).includes(name);
}

export type BrandedLookup = (
  suffix: string,
) => { value: string; key: string } | undefined;

export interface AutoTagSelection {
  /** May be true with no enabled names when the allowlist has no matches. */
  on: boolean;
  /** Names to resolve: every supported name, unless the allowlist narrows it. */
  enabled: Set<AutoTag>;
  /** Allowlist entries that name no supported value, lowercased. */
  unknown: string[];
  /** Distinguishes an absent allowlist from one narrowed to nothing. */
  namesSet: boolean;
}

/**
 * Must match Go `autotag.Select`: invalid configuration warns instead of
 * throwing. Invalid switches disable auto-tags, and unsupported names are
 * skipped.
 */
export function selectAutoTags(
  lookup: BrandedLookup,
  warn: (message: string) => void,
): AutoTagSelection {
  const names = lookup(AUTO_TAG_NAMES_SUFFIX);
  const selection: AutoTagSelection = {
    on: false,
    enabled: new Set<AutoTag>(),
    unknown: [],
    namesSet: names !== undefined,
  };

  const configured = lookup(AUTO_TAGS_SUFFIX);
  if (configured !== undefined) {
    const on = parseAutoTagsSwitch(configured.value);
    if (on === undefined) {
      warn(
        `invalid ${configured.key}="${configured.value}": expected a boolean. To select names, set AGENTO11Y_${AUTO_TAG_NAMES_SUFFIX} to a comma-separated subset of user, repo, and branch, or to all`,
      );
    }
    selection.on = on ?? false;
  }
  if (!selection.on) {
    if (names !== undefined) {
      warn(
        `${names.key} is set but AGENTO11Y_${AUTO_TAGS_SUFFIX} is off, so no automatic tags are attached`,
      );
    }
    return selection;
  }
  if (names === undefined) {
    selection.enabled = new Set(AUTO_TAG_ORDER);
    return selection;
  }

  const parsed = parseAutoTags(names.value);
  selection.enabled = parsed.enabled;
  selection.unknown = parsed.unknown;
  if (parsed.unknown.length > 0) {
    warn(
      `${names.key} has unsupported names ${parsed.unknown.join(", ")}; supported: ${[...AUTO_TAG_ORDER, AUTO_TAG_ALL].join(", ")}`,
    );
  }
  if (parsed.enabled.size === 0) {
    warn(
      `${names.key} contains no supported tag names, so no automatic tags are attached`,
    );
  }
  return selection;
}

function parseAutoTagsSwitch(raw: string): boolean | undefined {
  const normalized = raw.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off"].includes(normalized)) return false;
  return undefined;
}

/** Keep AGENTO11Y_TAGS parsing aligned with Go `envconfig.ParseExtraTags`. */
export function parseTagPairs(raw: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  if (!raw || raw.trim() === "") return out;
  for (const pair of raw.split(",")) {
    const eq = pair.indexOf("=");
    if (eq === -1) continue;
    const key = pair.slice(0, eq).trim();
    const value = pair.slice(eq + 1).trim();
    if (key === "" || value === "") continue;
    out[key] = value;
  }
  return out;
}

export interface AutoTagInputs {
  cwd?: string;
  /** Configured identity, which takes precedence over the OS account name. */
  userId?: string;
  /**
   * Existing AGENTO11Y_TAGS keys suppress auto-tags because the SDK gives
   * caller tags precedence over environment tags.
   */
  explicitTags?: Record<string, string>;
}

export function resolveAutoTags(
  enabled: Set<AutoTag>,
  inputs: AutoTagInputs = {},
): Record<string, string> | undefined {
  if (enabled.size === 0) return undefined;
  const explicit = inputs.explicitTags ?? {};
  const tags: Record<string, string> = {};
  for (const name of AUTO_TAG_ORDER) {
    if (!enabled.has(name)) continue;
    const key = autoTagKey(name);
    if (Object.hasOwn(explicit, key)) continue;
    const value = clean(resolveOne(name, inputs));
    if (value === "") continue;
    tags[key] = value;
  }
  return Object.keys(tags).length > 0 ? tags : undefined;
}

function resolveOne(name: AutoTag, inputs: AutoTagInputs): string | undefined {
  switch (name) {
    case "user":
      return resolveUser(inputs);
    case "repo":
      return inputs.cwd ? resolveGitRepo(inputs.cwd) : undefined;
    case "branch":
      return inputs.cwd ? resolveGitBranch(inputs.cwd) : undefined;
  }
}

/**
 * dsh has no signed-in identity. Unlike the Go launcher, it falls back
 * directly from the configured user ID to the operating-system account name.
 */
function resolveUser(inputs: AutoTagInputs): string | undefined {
  const configured = (inputs.userId ?? "").trim();
  if (configured !== "") return configured;
  try {
    return userInfo().username;
  } catch {
    return undefined;
  }
}

function clean(value: string | undefined): string {
  const trimmed = (value ?? "").trim();
  const points = Array.from(trimmed);
  return points.length > MAX_AUTO_TAG_VALUE_LENGTH
    ? points.slice(0, MAX_AUTO_TAG_VALUE_LENGTH).join("")
    : trimmed;
}
