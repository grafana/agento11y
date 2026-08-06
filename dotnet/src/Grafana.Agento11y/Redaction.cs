using System.Text;
using System.Text.RegularExpressions;

namespace Grafana.Agento11y;

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

    /// <summary>
    /// Alternating every tier 1 pattern into one regex scans each input once
    /// instead of once per pattern. Each pattern is wrapped in a capturing group;
    /// the matched group index identifies which pattern fired. The generator
    /// rejects capturing groups inside a tier 1 pattern, which would shift that
    /// mapping. Scanning once is also what keeps this output identical to the
    /// other SDKs': with per-pattern passes an earlier pattern can rewrite text a
    /// later one would have matched.
    /// </summary>
    private static readonly Regex Tier1Combined = new(
        string.Join("|", RedactionPatterns.Tier1.Select((p) => $"({p.Source})")),
        RedactionPatterns.BaseOptions
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
        var result = RedactLight(value, includeEmail);
        foreach (var pattern in RedactionPatterns.Tier2)
        {
            result = pattern.Regex.Replace(result, pattern.Replacement);
        }

        return result;
    }

    internal static string RedactLight(string value, bool includeEmail)
    {
        var result = RedactTier1(value);
        if (includeEmail)
        {
            result = RedactionPatterns.Email.Replace(result, $"[REDACTED:{RedactionPatterns.EmailId}]");
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
        return Tier1Combined.Replace(
            value,
            (match) =>
            {
                for (var group = 1; group <= RedactionPatterns.Tier1.Length; group++)
                {
                    if (match.Groups[group].Success)
                    {
                        return $"[REDACTED:{RedactionPatterns.Tier1[group - 1].Id}]";
                    }
                }

                return match.Value;
            }
        );
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

        logger?.Invoke($"agento11y: ignoring invalid {key}: {value}");
        return false;
    }

    private enum TextMode
    {
        Skip,
        Light,
        Full,
    }
}
