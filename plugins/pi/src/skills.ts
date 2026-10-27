import { accessSync, type BigIntStats, statSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { redactSecretText } from "@grafana/agento11y";

export interface SkillIdentity {
  name?: string;
  filePath: string;
}

/** Only structural names can enter span names, including in metadata-only mode. */
export function validateSkillName(value: unknown): string | undefined {
  if (typeof value !== "string" || /\p{Cc}/u.test(value)) return undefined;
  const name = value.trim();
  if (
    !name ||
    Buffer.byteLength(name, "utf8") > 256 ||
    !/^[\p{L}\p{N}][\p{L}\p{N}_.-]*(?::[\p{L}\p{N}][\p{L}\p{N}_.-]*)*$/u.test(
      name,
    ) ||
    /^(?:file|https?|ftp|ssh|git|npm|github):/i.test(name) ||
    redactSecretText(name) !== name
  ) {
    return undefined;
  }
  return name;
}

// Mirrors pi 0.87.1 utils/paths.js. Read arguments opt into space and @ handling;
// catalog file paths are already literal filesystem names.
function normalizePath(input: string, readArgument = false): string {
  let path = readArgument
    ? input.replace(/[\u00A0\u2000-\u200A\u202F\u205F\u3000]/g, " ")
    : input;
  if (readArgument && path.startsWith("@")) path = path.slice(1);
  if (process.platform === "win32" && !path.includes("\\")) {
    const match = path.match(/^\/(?:mnt\/|cygdrive\/)?([a-z])(?:\/(.*))?$/i);
    if (match?.[1])
      path = `${match[1].toUpperCase()}:\\${match[2]?.replaceAll("/", "\\") ?? ""}`;
  }
  if (path === "~") path = homedir();
  else if (
    path.startsWith("~/") ||
    (process.platform === "win32" && path.startsWith("~\\"))
  ) {
    path = join(homedir(), path.slice(2));
  }
  if (/^file:\/\//.test(path)) path = fileURLToPath(path);
  return path;
}

function resolvePath(input: string, cwd: string, readArgument = false): string {
  const path = normalizePath(input, readArgument);
  const base = normalizePath(cwd);
  return isAbsolute(path) ? resolve(path) : resolve(base, path);
}

/** Snapshot only identities, after all before_agent_start handlers have run. */
export function snapshotSkills(options: unknown, cwd: string): SkillIdentity[] {
  try {
    if (!options || typeof options !== "object") return [];
    const skills = (options as { skills?: unknown }).skills;
    if (!Array.isArray(skills)) return [];
    const catalog: SkillIdentity[] = [];
    for (const skill of skills) {
      if (!skill || typeof skill !== "object") continue;
      const name = validateSkillName(skill.name);
      const filePath = skill.filePath;
      if (typeof filePath !== "string" || !filePath) continue;
      // Unsafe names still make shared file identities ambiguous for verification.
      catalog.push({ name, filePath: resolvePath(filePath, cwd) });
    }
    return catalog;
  } catch {
    return [];
  }
}

function exists(path: string): boolean {
  try {
    accessSync(path);
    return true;
  } catch {
    return false;
  }
}

// pi selects one existing path, rather than matching every spelling against
// the catalog. An existing non-skill spelling must win over a skill variant.
function resolveReadPath(path: string, cwd: string): string {
  const original = resolvePath(path, cwd, true);
  const nfd = original.normalize("NFD");
  return (
    [
      original,
      original.replace(/ (AM|PM)\./gi, "\u202F$1."),
      nfd,
      original.replace(/'/g, "\u2019"),
      nfd.replace(/'/g, "\u2019"),
    ].find(
      (candidate, index) =>
        (index === 0 || candidate !== original) && exists(candidate),
    ) ?? original
  );
}

// Ask the filesystem, not Unicode normalization or case folding: distinct
// spellings may alias on APFS but name different files on other filesystems.
// stat follows symlinks and is deliberately refreshed for every read attempt.
function fileIdentity(path: string): BigIntStats | undefined {
  try {
    return statSync(path, { bigint: true });
  } catch {
    return undefined;
  }
}

/** Verify local file identity without reading instruction contents. */
export function verifySkillRead(
  catalog: readonly SkillIdentity[],
  toolName: string,
  args: unknown,
  cwd: string,
): string | undefined {
  try {
    if (
      catalog.length === 0 ||
      toolName !== "read" ||
      !args ||
      typeof args !== "object" ||
      Array.isArray(args)
    )
      return undefined;
    const path = (args as { path?: unknown }).path;
    if (typeof path !== "string" || !path || path.includes("\0"))
      return undefined;
    const selected = fileIdentity(resolveReadPath(path, cwd));
    if (!selected?.isFile()) return undefined;
    let match: SkillIdentity | undefined;
    for (const skill of catalog) {
      const candidate = fileIdentity(skill.filePath);
      if (
        !candidate?.isFile() ||
        candidate.dev !== selected.dev ||
        candidate.ino !== selected.ino
      )
        continue;
      // Exact paths cannot disambiguate catalog entries that share an inode.
      if (match) return undefined;
      match = skill;
    }
    return match?.name;
  } catch {
    return undefined;
  }
}

// Missing files still identify failed load attempts.
export function matchSkillRead(
  catalog: readonly SkillIdentity[],
  toolName: string,
  args: unknown,
  cwd: string,
): string | undefined {
  try {
    if (
      catalog.length === 0 ||
      toolName !== "read" ||
      !args ||
      typeof args !== "object" ||
      Array.isArray(args)
    )
      return undefined;
    const path = (args as { path?: unknown }).path;
    if (typeof path !== "string" || !path || path.includes("\0"))
      return undefined;
    const resolved = resolveReadPath(path, cwd);
    const exact = catalog.find(
      (skill) => skill.name && skill.filePath === resolved,
    );
    if (exact) return exact.name;
    const selected = fileIdentity(resolved);
    if (!selected) return undefined;
    return catalog.find((skill) => {
      if (!skill.name) return false;
      const candidate = fileIdentity(skill.filePath);
      return candidate?.dev === selected.dev && candidate.ino === selected.ino;
    })?.name;
  } catch {
    return undefined;
  }
}
