using System.Text;
using System.Text.RegularExpressions;

namespace Grafana.Sigil;

/// <summary>
/// Mutates a normalized generation before export. Sanitizers may redact strings
/// and byte payloads, but should preserve message and part structure.
/// </summary>
public delegate Generation GenerationSanitizer(Generation generation);

/// <summary>Options for the built-in secret redaction sanitizer.</summary>
public sealed class SecretRedactionOptions
{
    /// <summary>
    /// Redact user messages in <see cref="Generation.Input"/>. <c>null</c>
    /// falls back to <c>AGENTO11Y_REDACT_INPUT_MESSAGES</c> (legacy
    /// <c>SIGIL_REDACT_INPUT_MESSAGES</c>), then <c>false</c>.
    /// Assistant and tool messages in input are always sanitized.
    /// </summary>
    public bool? RedactInputMessages { get; set; }

    /// <summary>
    /// Redact generic email addresses. Defaults to <c>true</c>.
    /// </summary>
    public bool RedactEmailAddresses { get; set; } = true;
}

/// <summary>Factory for the built-in regex-based secrets redactor.</summary>
public static class SecretRedactionSanitizer
{
    private static readonly EnvPair EnvRedactInputMessages = new(
        "AGENTO11Y_REDACT_INPUT_MESSAGES",
        "SIGIL_REDACT_INPUT_MESSAGES"
    );
    private static readonly HashSet<string> TrueTokens = new(StringComparer.OrdinalIgnoreCase)
    {
        "1", "true", "yes", "on",
    };
    private static readonly HashSet<string> FalseTokens = new(StringComparer.OrdinalIgnoreCase)
    {
        "0", "false", "no", "off",
    };

    private sealed record SecretPattern(string Id, Regex Regex);

    private static readonly SecretPattern[] Tier1Patterns =
    [
        new("grafana-cloud-token", new Regex(@"\bglc_[A-Za-z0-9_-]{20,}", RegexOptions.Compiled)),
        new("grafana-service-account-token", new Regex(@"\bglsa_[A-Za-z0-9_-]{20,}", RegexOptions.Compiled)),
        new("aws-access-token", new Regex(@"\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b", RegexOptions.Compiled)),
        new("github-pat", new Regex(@"\bghp_[A-Za-z0-9_]{36,}", RegexOptions.Compiled)),
        new("github-oauth", new Regex(@"\bgho_[A-Za-z0-9_]{36,}", RegexOptions.Compiled)),
        new("github-app-token", new Regex(@"\bghs_[A-Za-z0-9_]{36,}", RegexOptions.Compiled)),
        new("github-fine-grained-pat", new Regex(@"\bgithub_pat_[A-Za-z0-9_]{82}", RegexOptions.Compiled)),
        new("anthropic-api-key", new Regex(@"\bsk-ant-api03-[a-zA-Z0-9_-]{93}AA", RegexOptions.Compiled)),
        new("anthropic-admin-key", new Regex(@"\bsk-ant-admin01-[a-zA-Z0-9_-]{93}AA", RegexOptions.Compiled)),
        new("openai-api-key", new Regex(@"\bsk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20}", RegexOptions.Compiled)),
        new("openai-project-key", new Regex(@"\bsk-proj-[a-zA-Z0-9_-]{40,}", RegexOptions.Compiled)),
        new("openai-svcacct-key", new Regex(@"\bsk-svcacct-[a-zA-Z0-9_-]{40,}", RegexOptions.Compiled)),
        new("gcp-api-key", new Regex(@"\bAIza[A-Za-z0-9_-]{35}", RegexOptions.Compiled)),
        new("private-key", new Regex(@"-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----", RegexOptions.Compiled)),
        new("connection-string", new Regex(@"(?:postgres|mysql|mongodb|redis|amqp)://[^\s'""]+@[^\s'""]+", RegexOptions.Compiled)),
        new("bearer-token", new Regex(@"[Bb]earer\s+[A-Za-z0-9_.\-~+/]{20,}={0,3}", RegexOptions.Compiled)),
        new("slack-token", new Regex(@"\bxox[bporas]-[A-Za-z0-9-]{10,}", RegexOptions.Compiled)),
        new("stripe-key", new Regex(@"\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}", RegexOptions.Compiled)),
        new("sendgrid-api-key", new Regex(@"\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}", RegexOptions.Compiled)),
        new("twilio-api-key", new Regex(@"\bSK[a-f0-9]{32}", RegexOptions.Compiled)),
        new("npm-token", new Regex(@"\bnpm_[A-Za-z0-9]{36}", RegexOptions.Compiled)),
        new("pypi-token", new Regex(@"\bpypi-[A-Za-z0-9_-]{50,}", RegexOptions.Compiled)),
    ];

    private static readonly SecretPattern EmailPattern = new(
        "email",
        new Regex(@"\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b", RegexOptions.Compiled | RegexOptions.IgnoreCase)
    );

    private static readonly SecretPattern EnvSecretPattern = new(
        "env-secret-value",
        new Regex(
            @"((?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*)([^\s""{}\[\],]+)",
            RegexOptions.Compiled | RegexOptions.IgnoreCase
        )
    );

    /// <summary>
    /// Returns a reusable generation sanitizer that redacts known secret formats.
    /// </summary>
    public static GenerationSanitizer Create(SecretRedactionOptions? options = null)
    {
        return Create(options, Environment.GetEnvironmentVariable, null);
    }

    internal static GenerationSanitizer Create(
        SecretRedactionOptions? options,
        Func<string, string?> envLookup,
        Action<string>? logger
    )
    {
        var resolved = options ?? new SecretRedactionOptions();
        var redactInputs = ResolveRedactInputMessages(resolved.RedactInputMessages, envLookup, logger);
        var includeEmail = resolved.RedactEmailAddresses;

        return generation =>
        {
            if (!string.IsNullOrEmpty(generation.SystemPrompt))
            {
                generation.SystemPrompt = RedactFull(generation.SystemPrompt, includeEmail);
            }

            if (!string.IsNullOrEmpty(generation.ConversationTitle))
            {
                generation.ConversationTitle = RedactLight(generation.ConversationTitle, includeEmail);
            }

            if (!string.IsNullOrEmpty(generation.CallError))
            {
                generation.CallError = RedactLight(generation.CallError, includeEmail);
            }

            foreach (var message in generation.Input)
            {
                SanitizeMessage(message, InputTextMode(message.Role, redactInputs), includeEmail);
            }

            foreach (var message in generation.Output)
            {
                SanitizeMessage(message, OutputTextMode(message.Role), includeEmail);
            }

            return generation;
        };
    }

    internal static string RedactFull(string value, bool includeEmail)
    {
        var result = RedactTier1(value);
        if (includeEmail)
        {
            result = ApplyPattern(result, EmailPattern);
        }

        return EnvSecretPattern.Regex.Replace(result, $"$1[REDACTED:{EnvSecretPattern.Id}]");
    }

    internal static string RedactLight(string value, bool includeEmail)
    {
        var result = RedactTier1(value);
        if (includeEmail)
        {
            result = ApplyPattern(result, EmailPattern);
        }

        return result;
    }

    private static byte[] RedactFullBytes(byte[] value, bool includeEmail)
    {
        if (value.Length == 0)
        {
            return value;
        }

        return Encoding.UTF8.GetBytes(RedactFull(Encoding.UTF8.GetString(value), includeEmail));
    }

    private static string RedactTier1(string value)
    {
        var result = value;
        foreach (var pattern in Tier1Patterns)
        {
            result = ApplyPattern(result, pattern);
        }

        return result;
    }

    private static string ApplyPattern(string value, SecretPattern pattern)
    {
        return pattern.Regex.Replace(value, $"[REDACTED:{pattern.Id}]");
    }

    private static void SanitizeMessage(Message message, TextMode mode, bool includeEmail)
    {
        if (mode == TextMode.Skip)
        {
            return;
        }

        foreach (var part in message.Parts)
        {
            switch (part.Kind)
            {
                case PartKind.Text:
                    part.Text = mode == TextMode.Full
                        ? RedactFull(part.Text, includeEmail)
                        : RedactLight(part.Text, includeEmail);
                    break;
                case PartKind.Thinking:
                    part.Thinking = RedactLight(part.Thinking, includeEmail);
                    break;
                case PartKind.ToolCall:
                    if (part.ToolCall?.InputJson is { Length: > 0 })
                    {
                        part.ToolCall.InputJson = RedactFullBytes(part.ToolCall.InputJson, includeEmail);
                    }
                    break;
                case PartKind.ToolResult:
                    if (part.ToolResult != null)
                    {
                        if (!string.IsNullOrEmpty(part.ToolResult.Content))
                        {
                            part.ToolResult.Content = RedactFull(part.ToolResult.Content, includeEmail);
                        }
                        if (part.ToolResult.ContentJson.Length > 0)
                        {
                            part.ToolResult.ContentJson = RedactFullBytes(part.ToolResult.ContentJson, includeEmail);
                        }
                    }
                    break;
            }
        }
    }

    private static TextMode InputTextMode(MessageRole role, bool redactUserInput)
    {
        return role switch
        {
            MessageRole.User => redactUserInput ? TextMode.Full : TextMode.Skip,
            MessageRole.Assistant => TextMode.Light,
            MessageRole.Tool => TextMode.Full,
            _ => TextMode.Skip,
        };
    }

    private static TextMode OutputTextMode(MessageRole role)
    {
        return role switch
        {
            MessageRole.Assistant => TextMode.Light,
            MessageRole.Tool => TextMode.Full,
            _ => TextMode.Skip,
        };
    }

    private static bool ResolveRedactInputMessages(
        bool? explicitValue,
        Func<string, string?> envLookup,
        Action<string>? logger
    )
    {
        if (explicitValue.HasValue)
        {
            return explicitValue.Value;
        }

        var value = EnvConfig.EnvTrimmed(envLookup, EnvRedactInputMessages, out var key);
        if (value == null)
        {
            return false;
        }

        if (TrueTokens.Contains(value))
        {
            return true;
        }

        if (FalseTokens.Contains(value))
        {
            return false;
        }

        logger?.Invoke($"sigil: ignoring invalid {key}: {value}");
        return false;
    }

    private enum TextMode
    {
        Skip,
        Light,
        Full,
    }
}
