using System.Diagnostics;
using System.Net.Http.Headers;
using System.Text.Json;
using System.Text.Json.Nodes;
using Nirmitee.DaVinci.Common.Http;

namespace Nirmitee.DaVinci.Common.Fhir;

public sealed class FhirClientOptions
{
    /// <summary>Timeout for each request to a client FHIR server.</summary>
    public int TimeoutMs { get; set; } = 3000;

    /// <summary>Prefix rewrites applied to FHIR URLs before fetching.</summary>
    public List<UrlRewriteRule> UrlRewrites { get; set; } = [];
}

/// <summary>How a FHIR fetch ended.</summary>
public enum FhirFetchOutcome
{
    /// <summary>2xx with a parseable JSON object body.</summary>
    Success,

    /// <summary>404, 410 or another 4xx other than 401 and 403: the data is simply not there.</summary>
    Absent,

    /// <summary>401, 403, 5xx, timeout, connection error or unparseable body.</summary>
    Technical,
}

/// <param name="RelativePath">"Type/id" or "Type?query", used in failure messages.</param>
/// <param name="Failure">For technical failures: "GET {RelativePath} returned HTTP {status}" or "GET {RelativePath} failed: {error}".</param>
public sealed record FhirFetchResult(
    string RelativePath,
    string Url,
    int? Status,
    long DurationMs,
    FhirFetchOutcome Outcome,
    JsonObject? Body,
    string? Failure);

public sealed record FhirFetchLogEntry(string Url, int? Status, long Ms);

/// <summary>Reads FHIR JSON from a client's FHIR server. Never throws for HTTP or network problems; classifies them instead.</summary>
public sealed class FhirDataClient(
    HttpClient http,
    string baseUrl,
    string? accessToken,
    FhirClientOptions options,
    CancellationToken requestAborted = default)
{
    private readonly string _baseUrl = baseUrl.TrimEnd('/');
    private readonly List<FhirFetchLogEntry> _fetches = [];
    private readonly Lock _logLock = new();

    public string BaseUrl => _baseUrl;

    public IReadOnlyList<FhirFetchLogEntry> Fetches
    {
        get
        {
            lock (_logLock)
            {
                return _fetches.ToList();
            }
        }
    }

    public Task<FhirFetchResult> ReadAsync(string resourceType, string id, CancellationToken cancellationToken) =>
        GetAsync($"{resourceType}/{id}", $"{resourceType}/{Uri.EscapeDataString(id)}", cancellationToken);

    public Task<FhirFetchResult> SearchAsync(string resourceType, IReadOnlyList<KeyValuePair<string, string>> parameters, CancellationToken cancellationToken)
    {
        var display = $"{resourceType}?{string.Join("&", parameters.Select(p => $"{p.Key}={p.Value}"))}";
        var path = $"{resourceType}?{string.Join("&", parameters.Select(p => $"{Uri.EscapeDataString(p.Key)}={Uri.EscapeDataString(p.Value)}"))}";
        return GetAsync(display, path, cancellationToken);
    }

    private async Task<FhirFetchResult> GetAsync(string display, string path, CancellationToken cancellationToken)
    {
        var url = $"{_baseUrl}/{path}";
        var fetchUrl = UrlRewriter.Rewrite(url, options.UrlRewrites);
        var timeoutMs = Math.Max(100, options.TimeoutMs);
        using var timeoutSource = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeoutSource.CancelAfter(timeoutMs);

        using var request = new HttpRequestMessage(HttpMethod.Get, fetchUrl);
        request.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/fhir+json"));
        if (!string.IsNullOrWhiteSpace(accessToken))
        {
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", accessToken);
        }

        var started = Stopwatch.GetTimestamp();
        int? status = null;
        try
        {
            using var response = await http.SendAsync(request, timeoutSource.Token);
            status = (int)response.StatusCode;
            var body = await response.Content.ReadAsStringAsync(timeoutSource.Token);
            var ms = Elapsed(started);
            Log(url, status, ms);

            if (status is >= 200 and < 300)
            {
                try
                {
                    if (JsonNode.Parse(body) is JsonObject obj)
                    {
                        return new FhirFetchResult(display, url, status, ms, FhirFetchOutcome.Success, obj, null);
                    }
                }
                catch (JsonException)
                {
                    // fall through to the technical result below
                }

                return new FhirFetchResult(display, url, status, ms, FhirFetchOutcome.Technical, null, $"GET {display} failed: unparseable response body");
            }

            if (status is 401 or 403 or >= 500)
            {
                return new FhirFetchResult(display, url, status, ms, FhirFetchOutcome.Technical, null, $"GET {display} returned HTTP {status}");
            }

            return new FhirFetchResult(display, url, status, ms, FhirFetchOutcome.Absent, null, null);
        }
        catch (OperationCanceledException) when (!requestAborted.IsCancellationRequested)
        {
            var ms = Elapsed(started);
            Log(url, status, ms);
            return new FhirFetchResult(display, url, status, ms, FhirFetchOutcome.Technical, null, $"GET {display} failed: timeout after {timeoutMs} ms");
        }
        catch (HttpRequestException ex)
        {
            var ms = Elapsed(started);
            Log(url, status, ms);
            return new FhirFetchResult(display, url, status, ms, FhirFetchOutcome.Technical, null, $"GET {display} failed: {ex.Message}");
        }
    }

    private static long Elapsed(long started) => (long)Stopwatch.GetElapsedTime(started).TotalMilliseconds;

    private void Log(string url, int? status, long ms)
    {
        lock (_logLock)
        {
            _fetches.Add(new FhirFetchLogEntry(url, status, ms));
        }
    }
}

public sealed class FhirDataClientFactory(IHttpClientFactory httpClientFactory)
{
    public const string HttpClientName = "davinci-fhir";

    /// <summary>Creates a client for the given FHIR base URL, or null when none was supplied.</summary>
    public FhirDataClient? Create(string? fhirServer, string? accessToken, FhirClientOptions options, CancellationToken requestAborted = default)
    {
        if (string.IsNullOrWhiteSpace(fhirServer) || !Uri.TryCreate(fhirServer, UriKind.Absolute, out _))
        {
            return null;
        }

        return new FhirDataClient(httpClientFactory.CreateClient(HttpClientName), fhirServer, accessToken, options, requestAborted);
    }
}
