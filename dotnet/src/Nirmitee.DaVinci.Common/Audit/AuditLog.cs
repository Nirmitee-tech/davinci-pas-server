using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging;
using Nirmitee.DaVinci.Common.Json;

namespace Nirmitee.DaVinci.Common.Audit;

public sealed class AuditOptions
{
    /// <summary>Directory for the daily JSONL files ({Directory}/{yyyy-MM-dd}.jsonl). Relative paths resolve against the working directory.</summary>
    public string Directory { get; set; } = "data/audit";

    public bool Enabled { get; set; } = true;
}

/// <summary>Appends one JSON object per line to a daily JSONL file.</summary>
public class AuditLog(Func<AuditOptions> options, TimeProvider time, ILogger logger)
{
    private readonly SemaphoreSlim _gate = new(1, 1);

    public string CurrentFilePath => Path.Combine(ResolveDirectory(), $"{time.GetUtcNow():yyyy-MM-dd}.jsonl");

    public async Task AppendAsync(JsonObject entry)
    {
        if (!options().Enabled)
        {
            return;
        }

        try
        {
            var path = CurrentFilePath;
            System.IO.Directory.CreateDirectory(Path.GetDirectoryName(path)!);
            var line = entry.ToJsonString(JsonUtil.Compact) + "\n";

            await _gate.WaitAsync();
            try
            {
                await File.AppendAllTextAsync(path, line);
            }
            finally
            {
                _gate.Release();
            }
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "Failed to write the audit log entry");
        }
    }

    /// <summary>Deep copy of a CDS Hooks request body with fhirAuthorization.access_token replaced by "[redacted]".</summary>
    public static JsonNode? RedactTokens(JsonNode? body)
    {
        var copy = body?.DeepClone();
        if (copy is JsonObject obj && obj["fhirAuthorization"] is JsonObject authorization && authorization.ContainsKey("access_token"))
        {
            authorization["access_token"] = "[redacted]";
        }

        return copy;
    }

    private string ResolveDirectory()
    {
        var dir = options().Directory;
        return Path.GetFullPath(string.IsNullOrWhiteSpace(dir) ? "data/audit" : dir);
    }
}
