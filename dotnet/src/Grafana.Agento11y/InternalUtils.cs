using System.Text;
using System.Text.Json;

namespace Grafana.Sigil;

internal static class InternalUtils
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web)
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
    };

#if NET
    private static readonly Random RandomSource = Random.Shared;
#else
    private static readonly Random RandomSource = new();
    private static readonly object RandomLock = new();
#endif

    public static T DeepClone<T>(T value)
    {
        if (value == null)
        {
            return value!;
        }

        var json = JsonSerializer.Serialize(value, JsonOptions);
        return JsonSerializer.Deserialize<T>(json, JsonOptions)!;
    }

    public static string NewRandomId(string prefix)
    {
        var bytes = new byte[8];

#if !NET
        lock (RandomLock)
#endif
        {
            RandomSource.NextBytes(bytes);
        }

        var buffer = new StringBuilder(prefix.Length + 1 + 16);
        buffer.Append(prefix);
        buffer.Append('_');
        for (var i = 0; i < bytes.Length; i++)
        {
            buffer.Append(bytes[i].ToString("x2"));
        }

        return buffer.ToString();
    }

    public static DateTimeOffset Utc(DateTimeOffset value)
    {
        return value.ToUniversalTime();
    }

    public static string SerializeJson(object? value)
    {
        if (value == null)
        {
            return string.Empty;
        }

        return JsonSerializer.Serialize(value, JsonOptions);
    }

    public static byte[] SerializeJsonBytes(object? value)
    {
        if (value == null)
        {
            return [];
        }

        return JsonSerializer.SerializeToUtf8Bytes(value, JsonOptions);
    }

    public static JsonElement SerializeJsonElement(object? value)
    {
        return JsonSerializer.SerializeToElement(value, JsonOptions);
    }
}
