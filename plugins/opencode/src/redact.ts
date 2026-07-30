/**
 * Secret redaction engine for agento11y content capture.
 *
 * ~20 high-confidence patterns hand-curated from Gitleaks
 * (https://github.com/gitleaks/gitleaks). Two tiers:
 *   - Tier 1: definite secret formats — used by both redact() and redactLightweight()
 *   - Tier 2: heuristic env patterns — used only by redact()
 *
 * Add more patterns when concrete unredacted secrets are observed.
 */

interface SecretPattern {
  id: string;
  regex: RegExp;
  tier: 1 | 2;
}

// --- Tier 1: High-confidence patterns (definite secret formats) ---
const TIER1_PATTERNS: SecretPattern[] = [
  // Grafana
  { id: "grafana-cloud-token", regex: /\bglc_[A-Za-z0-9_-]{20,}/g, tier: 1 },
  {
    id: "grafana-service-account-token",
    regex: /\bglsa_[A-Za-z0-9_-]{20,}/g,
    tier: 1,
  },
  // AWS
  {
    id: "aws-access-token",
    regex: /\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b/g,
    tier: 1,
  },
  // GitHub
  { id: "github-pat", regex: /\bghp_[A-Za-z0-9_]{36,}/g, tier: 1 },
  { id: "github-oauth", regex: /\bgho_[A-Za-z0-9_]{36,}/g, tier: 1 },
  { id: "github-app-token", regex: /\bghs_[A-Za-z0-9_]{36,}/g, tier: 1 },
  {
    id: "github-fine-grained-pat",
    regex: /\bgithub_pat_[A-Za-z0-9_]{82}/g,
    tier: 1,
  },
  // Anthropic
  {
    id: "anthropic-api-key",
    regex: /\bsk-ant-api03-[a-zA-Z0-9_-]{93}AA/g,
    tier: 1,
  },
  {
    id: "anthropic-admin-key",
    regex: /\bsk-ant-admin01-[a-zA-Z0-9_-]{93}AA/g,
    tier: 1,
  },
  // OpenAI (legacy format + modern sk-proj-/sk-svcacct- formats)
  {
    id: "openai-api-key",
    regex: /\bsk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20}/g,
    tier: 1,
  },
  { id: "openai-project-key", regex: /\bsk-proj-[a-zA-Z0-9_-]{40,}/g, tier: 1 },
  {
    id: "openai-svcacct-key",
    regex: /\bsk-svcacct-[a-zA-Z0-9_-]{40,}/g,
    tier: 1,
  },
  // GCP
  { id: "gcp-api-key", regex: /\bAIza[A-Za-z0-9_-]{35}/g, tier: 1 },
  // PEM private keys
  {
    id: "private-key",
    regex:
      /-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----/g,
    tier: 1,
  },
  // Connection strings with embedded credentials
  {
    id: "connection-string",
    regex: /(?:postgres|mysql|mongodb|redis|amqp):\/\/[^\s'"]+@[^\s'"]+/g,
    tier: 1,
  },
  // Bearer tokens in Authorization headers
  {
    id: "bearer-token",
    regex: /[Bb]earer\s+[A-Za-z0-9_.\-~+/]{20,}={0,3}/g,
    tier: 1,
  },
  // Slack tokens
  { id: "slack-token", regex: /\bxox[bporas]-[A-Za-z0-9-]{10,}/g, tier: 1 },
  // Stripe keys
  {
    id: "stripe-key",
    regex: /\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}/g,
    tier: 1,
  },
  // SendGrid
  {
    id: "sendgrid-api-key",
    regex: /\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}/g,
    tier: 1,
  },
  // Twilio
  { id: "twilio-api-key", regex: /\bSK[a-f0-9]{32}/g, tier: 1 },
  // npm tokens
  { id: "npm-token", regex: /\bnpm_[A-Za-z0-9]{36}/g, tier: 1 },
  // PyPI tokens
  { id: "pypi-token", regex: /\bpypi-[A-Za-z0-9_-]{50,}/g, tier: 1 },
];

// --- Tier 2: Heuristic patterns (env file values) ---
const TIER2_PATTERNS: SecretPattern[] = [
  {
    id: "env-secret-value",
    regex:
      /(?<=(?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*)\S+/gi,
    tier: 2,
  },
];

/**
 * Note: Pattern arrays are shared by reference across Redactor instances.
 * This is safe because: (1) there's a single Redactor instance in production,
 * (2) JS is single-threaded so .replace() completes synchronously, and
 * (3) lastIndex is reset before each replace call. If this class is ever used
 * in workers or multiple instances, clone regexes in the constructor.
 */
export class Redactor {
  private tier1 = TIER1_PATTERNS;
  private tier2 = TIER2_PATTERNS;

  /** Full redaction: tier 1 + tier 2. Use for tool call args and tool results. */
  redact(text: string): string {
    let result = text;
    for (const pattern of this.tier1) {
      pattern.regex.lastIndex = 0;
      result = result.replace(pattern.regex, `[REDACTED:${pattern.id}]`);
    }
    for (const pattern of this.tier2) {
      pattern.regex.lastIndex = 0;
      result = result.replace(pattern.regex, `[REDACTED:${pattern.id}]`);
    }
    return result;
  }

  /** Lightweight redaction: tier 1 only. Use for assistant text and reasoning. */
  redactLightweight(text: string): string {
    let result = text;
    for (const pattern of this.tier1) {
      pattern.regex.lastIndex = 0;
      result = result.replace(pattern.regex, `[REDACTED:${pattern.id}]`);
    }
    return result;
  }
}
