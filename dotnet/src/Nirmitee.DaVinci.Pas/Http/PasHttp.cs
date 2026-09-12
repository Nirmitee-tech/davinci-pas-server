using System.Diagnostics;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using Nirmitee.DaVinci.Common.Audit;
using Nirmitee.DaVinci.Pas.Fhir;

namespace Nirmitee.DaVinci.Pas.Http;

/// <summary>A status, JSON body, content type and extra headers produced by a PAS handler.</summary>
public sealed class PasResponse(int status, JsonNode? body, string contentType = PasResponse.FhirJsonType)
{
    public const string FhirJsonType = "application/fhir+json; charset=utf-8";
    public const string JsonType = "application/json; charset=utf-8";

    public int Status { get; } = status;

    public JsonNode? Body { get; } = body;

    public string ContentType { get; } = contentType;

    public Dictionary<string, string> Headers { get; } = new(StringComparer.OrdinalIgnoreCase);

    public static PasResponse Fhir(int status, JsonNode body) => new(status, body);

    public static PasResponse Outcome(int status, string code, string message, string? expression = null) =>
        new(status, Outcomes.Create(code, message, expression));

    public static PasResponse NoContent() => new(204, null);

    public async Task WriteAsync(HttpResponse response, CancellationToken cancellationToken)
    {
        response.StatusCode = Status;
        foreach (var (name, value) in Headers)
        {
            response.Headers[name] = value;
        }

        if (Body is null)
        {
            return;
        }

        response.ContentType = ContentType;
        await response.WriteAsync(Body.ToJsonString(FhirJson.Options), cancellationToken);
    }
}

/// <summary>Everything a PAS handler needs about the current request.</summary>
public sealed class PasCall
{
    public required HttpContext Context { get; init; }

    /// <summary>v221, v201, or null for control routes.</summary>
    public string? Version { get; init; }

    /// <summary>The external FHIR base, for example http://host.docker.internal:8086/v221/fhir (empty for control routes).</summary>
    public required string FhirBase { get; init; }

    /// <summary>Parsed JSON body; null when absent or not JSON.</summary>
    public JsonNode? Body { get; init; }

    public string? RawBody { get; init; }

    /// <summary>Optional note for the audit entry.</summary>
    public string? AuditDetail { get; set; }

    public string RouteValue(string name) => Context.Request.RouteValues[name]?.ToString() ?? string.Empty;
}

/// <summary>The external FHIR base of a PAS version endpoint (SPEC section 0).</summary>
public static class FhirBase
{
    /// <summary>
    /// {PAS_PUBLIC_BASE_URL}{pathBase}{prefix}/{version}/fhir when a public base is configured, otherwise
    /// {scheme}://{host}{pathBase}{prefix}/{version}/fhir with scheme from X-Forwarded-Proto and host from
    /// X-Forwarded-Host (first values) falling back to the connection scheme and Host header.
    /// </summary>
    public static string Compute(HttpRequest request, string prefix, string version, string? publicBaseUrl)
    {
        var path = $"{request.PathBase}{prefix}/{version}/fhir";
        if (!string.IsNullOrWhiteSpace(publicBaseUrl))
        {
            return publicBaseUrl.Trim().TrimEnd('/') + path;
        }

        var scheme = FirstHeaderValue(request, "X-Forwarded-Proto") ?? request.Scheme;
        var host = FirstHeaderValue(request, "X-Forwarded-Host") ?? request.Host.ToString();
        return $"{scheme}://{host}{path}";
    }

    private static string? FirstHeaderValue(HttpRequest request, string name)
    {
        var value = request.Headers[name].ToString();
        return string.IsNullOrWhiteSpace(value) ? null : value.Split(',')[0].Trim();
    }
}

/// <summary>Removes bearer tokens from values written to the audit log.</summary>
public static partial class AuditRedactor
{
    [GeneratedRegex(@"(?i)\b(bearer)\s+[^\s""',;]+")]
    private static partial Regex BearerToken();

    public static string Redact(string text) => BearerToken().Replace(text, "$1 [redacted]");

    /// <summary>Deep copy with every string containing a bearer token redacted and access_token values replaced.</summary>
    public static JsonNode? Redact(JsonNode? node)
    {
        var copy = node?.DeepClone();
        Walk(copy);
        return copy;
    }

    private static void Walk(JsonNode? node)
    {
        switch (node)
        {
            case JsonObject obj:
                foreach (var key in obj.Select(p => p.Key).ToList())
                {
                    if (key is "access_token" or "refresh_token" or "client_secret" or "endpoint_token")
                    {
                        obj[key] = "[redacted]";
                    }
                    else if (J.Str(obj[key]) is { } text)
                    {
                        var redacted = Redact(text);
                        if (!ReferenceEquals(redacted, text) && redacted != text)
                        {
                            obj[key] = redacted;
                        }
                    }
                    else
                    {
                        Walk(obj[key]);
                    }
                }

                break;
            case JsonArray array:
                for (var i = 0; i < array.Count; i++)
                {
                    if (J.Str(array[i]) is { } text)
                    {
                        var redacted = Redact(text);
                        if (redacted != text)
                        {
                            array[i] = redacted;
                        }
                    }
                    else
                    {
                        Walk(array[i]);
                    }
                }

                break;
        }
    }
}

/// <summary>The PAS audit log: {PAS_DATA_DIR}/pas-audit/{yyyy-MM-dd}.jsonl.</summary>
public sealed class PasAuditLog(IOptionsMonitor<PasOptions> options, TimeProvider time, ILogger<PasAuditLog> logger)
    : AuditLog(() => options.CurrentValue.Audit, time, logger);

/// <summary>
/// The shared request pipeline: version check, bearer auth, body parsing, error mapping to OperationOutcome,
/// response writing and one audit line per request.
/// </summary>
public sealed class PasPipeline(IOptionsMonitor<PasOptions> options, PasAuditLog audit, TimeProvider time, ILogger<PasPipeline> logger)
{
    private const int MaxAuditRawLength = 8192;

    public async Task RunAsync(
        HttpContext context,
        string prefix,
        string? version,
        string operation,
        bool requireAuth,
        bool readBody,
        Func<PasCall, PasResponse> handler)
    {
        var started = Stopwatch.GetTimestamp();
        var request = context.Request;
        JsonNode? body = null;
        string? raw = null;
        string? detail = null;
        PasResponse response;

        try
        {
            if (version is not null && !PasVersions.IsKnown(version))
            {
                response = PasResponse.Outcome(404, "not-found", $"Unknown PAS FHIR base version '{version}'. Use v221 or v201.");
            }
            else if (requireAuth && !IsAuthorized(request, out detail))
            {
                response = PasResponse.Outcome(401, "security", "A valid bearer token is required: send Authorization: Bearer <token>.");
                response.Headers["WWW-Authenticate"] = "Bearer";
            }
            else
            {
                if (readBody)
                {
                    using var reader = new StreamReader(request.Body, Encoding.UTF8);
                    raw = await reader.ReadToEndAsync(context.RequestAborted);
                    body = FhirJson.TryParse(raw);
                }

                var call = new PasCall
                {
                    Context = context,
                    Version = version,
                    FhirBase = version is null ? string.Empty : FhirBase.Compute(request, prefix, version, options.CurrentValue.PublicBaseUrl),
                    Body = body,
                    RawBody = raw,
                };
                response = handler(call);
                detail = call.AuditDetail;
            }
        }
        catch (OperationCanceledException) when (context.RequestAborted.IsCancellationRequested)
        {
            return;
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "PAS {Operation} failed", operation);
            response = PasResponse.Outcome(500, "exception", "The server could not process the request.");
            detail = ex.GetType().Name + ": " + ex.Message;
        }

        await response.WriteAsync(context.Response, context.RequestAborted);

        await audit.AppendAsync(new JsonObject
        {
            ["timestamp"] = time.GetUtcNow().UtcDateTime.ToString("O"),
            ["module"] = "pas",
            ["version"] = version,
            ["operation"] = operation,
            ["method"] = request.Method,
            ["path"] = $"{request.PathBase}{request.Path}{request.QueryString}",
            ["status"] = response.Status,
            ["durationMs"] = Math.Round(Stopwatch.GetElapsedTime(started).TotalMilliseconds, 1),
            ["remoteIp"] = context.Connection.RemoteIpAddress?.ToString(),
            ["detail"] = detail is null ? null : AuditRedactor.Redact(detail),
            ["request"] = AuditRequest(body, raw),
            ["response"] = AuditRedactor.Redact(response.Body),
        });
    }

    private bool IsAuthorized(HttpRequest request, out string? detail)
    {
        var tokens = options.CurrentValue.AuthTokens;
        detail = null;
        if (tokens.Count == 0)
        {
            return true;
        }

        var header = request.Headers.Authorization.ToString();
        if (!header.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase))
        {
            detail = "missing bearer token";
            return false;
        }

        var presented = Encoding.UTF8.GetBytes(header["Bearer ".Length..].Trim());
        foreach (var token in tokens)
        {
            if (CryptographicOperations.FixedTimeEquals(presented, Encoding.UTF8.GetBytes(token)))
            {
                return true;
            }
        }

        detail = "unknown bearer token";
        return false;
    }

    private static JsonNode? AuditRequest(JsonNode? body, string? raw)
    {
        if (body is not null)
        {
            return AuditRedactor.Redact(body);
        }

        if (string.IsNullOrEmpty(raw))
        {
            return null;
        }

        return JsonValue.Create(AuditRedactor.Redact(raw.Length > MaxAuditRawLength ? raw[..MaxAuditRawLength] : raw));
    }
}
