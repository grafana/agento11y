import { redactSecretText } from "@grafana/agento11y-core";

/** Never retain catalog content or paths. */
export function verifyNativeSkill(
  rawID: string,
  catalog: unknown,
  directory: string,
): string | undefined {
  if (!catalog || typeof catalog !== "object") return undefined;
  const { location, data } = catalog as {
    location?: { directory?: unknown };
    data?: unknown;
  };
  if (location?.directory !== directory || !Array.isArray(data))
    return undefined;
  const matches = data.filter((entry) => entry?.id === rawID);
  if (matches.length !== 1) return undefined;
  const name = matches[0]?.name;
  return typeof name === "string" && detectSkillName("skill", { name }) === name
    ? name
    : undefined;
}

export function detectSkillName(
  toolName: string,
  input: unknown,
  field: "name" | "id" = "name",
): string | undefined {
  if (
    toolName !== "skill" ||
    !input ||
    typeof input !== "object" ||
    Array.isArray(input)
  )
    return undefined;
  const value = (input as Record<string, unknown>)[field];
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
  )
    return undefined;
  return name;
}
