export const EXPORT_PATH = "/api/v1/generations:export";

/**
 * Accept a base URL or a copied generation-export URL, and return the API base
 * without trailing slashes. client.ts adds the export path again.
 */
export function normalizeBaseEndpoint(endpoint: string): string {
  if (!endpoint) return "";
  try {
    const url = new URL(endpoint);
    let pathname = url.pathname.replace(/\/+$/, "");
    if (pathname.endsWith(EXPORT_PATH)) {
      pathname = pathname.slice(0, pathname.length - EXPORT_PATH.length);
    }
    url.pathname = pathname;
    return url.toString().replace(/\/+$/, "");
  } catch {
    const trimmed = endpoint.replace(/\/+$/, "");
    return trimmed.endsWith(EXPORT_PATH)
      ? trimmed.slice(0, trimmed.length - EXPORT_PATH.length)
      : trimmed;
  }
}
