// Resolve the client tags a session opts into with
// AGENTO11Y_AUTO_CODING_AGENT_TAGS.
//
// Two variables drive it. AGENTO11Y_AUTO_CODING_AGENT_TAGS is the on/off switch
// and is off by default. AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES is an optional
// allowlist that narrows the switch to some of the supported names; with the
// switch on and no list, every name applies.
//
// Client tags are the only tag mechanism that becomes a metric label
// (docs/concepts/tags-and-metadata.md), which is why the resolved values are
// attached to the client rather than to each generation. Nothing resolves
// until the switch is on: with the switch off, `resolveAutoTags` runs no git
// commands and no account lookup.
//
// Mirrors plugins/agento11y/internal/autotag/autotag.go. The Go launcher owns
// the hook-based agents; this file covers the in-process opencode plugin, the
// same way tags.ts mirrors the Go built-in tag builder.

import { userInfo } from "node:os";
import { resolveGitBranch, resolveGitRepo } from "./git.js";

/** One value the plugin can resolve for the session. */
export type AutoTag = "user" | "repo" | "branch";

/** Supported names in the order diagnostics and tag maps use. */
export const AUTO_TAG_ORDER: readonly AutoTag[] = ["user", "repo", "branch"];

/** Shorthand that enables every supported name. */
export const AUTO_TAG_ALL = "all";

/**
 * Suffixes of the two branded variables that drive automatic tags:
 * AUTO_TAGS_SUFFIX is the on/off switch, AUTO_TAG_NAMES_SUFFIX the optional
 * allowlist.
 */
export const AUTO_TAGS_SUFFIX = "AUTO_CODING_AGENT_TAGS";
export const AUTO_TAG_NAMES_SUFFIX = "AUTO_CODING_AGENT_TAGS_NAMES";

/**
 * Cap on a resolved value, in Unicode code points. Every value becomes a
 * Prometheus label, so an accidentally long branch name or email stays
 * bounded. Truncation keeps the start of the value.
 */
export const MAX_AUTO_TAG_VALUE_LENGTH = 128;

/**
 * Tag key each name is written under. The branch reuses the per-generation
 * built-in key `git.branch` so the export carries one branch key: the
 * per-generation tag wins in the generation export, and this client tag
 * supplies the metric label.
 */
const TAG_KEYS: Record<AutoTag, string> = {
  user: "user",
  repo: "repo",
  branch: "git.branch",
};

/** The client tag key an auto-tag name is written under. */
export function autoTagKey(name: AutoTag): string {
  return TAG_KEYS[name];
}

export interface ParsedAutoTags {
  enabled: Set<AutoTag>;
  /** Names that are not supported, lowercased, for the caller to log. */
  unknown: string[];
}

/**
 * Parse AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES ("user,repo") into the set of
 * enabled names. Matching is case-insensitive and surrounding whitespace is
 * trimmed. "all" enables every supported name, which is also what an unset
 * allowlist means. Empty entries are skipped; unrecognized names are returned
 * separately so a caller can log them and keep going with the names it did
 * recognize.
 */
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

/** One branded variable resolved to its value and the spelling that won. */
export type BrandedLookup = (
  suffix: string,
) => { value: string; key: string } | undefined;

export interface AutoTagSelection {
  /** The switch holds a true value. It can be true with an empty `enabled`, when the allowlist named nothing supported. */
  on: boolean;
  /** Names to resolve: every supported name, unless the allowlist narrows it. */
  enabled: Set<AutoTag>;
  /** Allowlist entries that name no supported value, lowercased. */
  unknown: string[];
  /** The allowlist variable held a value, which separates "narrowed to nothing" from "not narrowed". */
  namesSet: boolean;
}

/**
 * Read the switch and the allowlist and report which names to resolve. Mirrors
 * autotag.Select on the Go side.
 *
 * Every problem is reported through `warn` and none of them throws: a switch
 * value that is not a boolean leaves the mechanism off, an unsupported name is
 * skipped, and an allowlist set while the switch is off attaches nothing.
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
        `invalid ${configured.key}="${configured.value}": expected a boolean, and the names go in AGENTO11Y_${AUTO_TAG_NAMES_SUFFIX}`,
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
      `${names.key} names no supported value, so no automatic tags are attached`,
    );
  }
  return selection;
}

// The same boolean whitelist config.ts applies to every other branded flag,
// kept here so the switch and its diagnostic live together.
function parseAutoTagsSwitch(raw: string): boolean | undefined {
  const normalized = raw.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off"].includes(normalized)) return false;
  return undefined;
}

/**
 * Parse a comma-separated "key=value" string (AGENTO11Y_TAGS) into a tag map.
 * Malformed entries (empty keys, missing '=', empty values) are skipped.
 * Mirrors envconfig.ParseExtraTags on the Go side.
 */
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
  /** Workspace root the repository and branch are resolved from. */
  cwd?: string;
  /**
   * Identity the caller already resolved, usually AGENTO11Y_USER_ID. It ranks
   * above the OS account name.
   */
  userId?: string;
  /**
   * Tags AGENTO11Y_TAGS defines. A key set there wins, so the resolver leaves
   * it out: the SDK merges caller tags over environment tags, which is the
   * opposite of the precedence this switch needs.
   */
  explicitTags?: Record<string, string>;
}

/**
 * Resolve the client tags for the enabled names. Values are trimmed and capped
 * at MAX_AUTO_TAG_VALUE_LENGTH; a name that resolves to nothing leaves its key
 * off, and so does a key AGENTO11Y_TAGS already defines. Returns undefined
 * when nothing resolves.
 */
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
 * Prefer the identity the user configured, then the OS account name. opencode
 * has no signed-in user of its own, so there is no middle source here; the Go
 * launcher inserts the agent-resolved identity between the two.
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
