using System.Collections.Concurrent;
using Microsoft.Extensions.Logging;
using Microsoft.IdentityModel.Tokens;
using Nirmitee.DaVinci.Common.Http;

namespace Nirmitee.DaVinci.Common.Security;

/// <summary>Fetches and caches JSON Web Key Sets published by trusted CDS clients.</summary>
public class JwksProvider(IHttpClientFactory httpClientFactory, TimeProvider time, ILogger<JwksProvider> logger)
{
    public const string HttpClientName = "davinci-jwks";
    private static readonly TimeSpan MinimumRefreshInterval = TimeSpan.FromSeconds(5);

    private readonly ConcurrentDictionary<string, CacheEntry> _cache = new(StringComparer.Ordinal);

    private sealed record CacheEntry(IReadOnlyList<JsonWebKey> Keys, DateTimeOffset FetchedAt);

    /// <summary>Returns the keys for the JWKS URL, using the cache unless a refresh is forced.</summary>
    public virtual async Task<IReadOnlyList<JsonWebKey>> GetKeysAsync(
        string jwksUrl,
        bool forceRefresh,
        TimeSpan cacheDuration,
        IEnumerable<UrlRewriteRule>? rewrites,
        CancellationToken cancellationToken)
    {
        var now = time.GetUtcNow();
        if (_cache.TryGetValue(jwksUrl, out var cached))
        {
            var age = now - cached.FetchedAt;
            if ((!forceRefresh && age < cacheDuration) || (forceRefresh && age < MinimumRefreshInterval))
            {
                return cached.Keys;
            }
        }

        var fetchUrl = UrlRewriter.Rewrite(jwksUrl, rewrites);
        var client = httpClientFactory.CreateClient(HttpClientName);
        using var response = await client.GetAsync(fetchUrl, cancellationToken);
        if (!response.IsSuccessStatusCode)
        {
            throw new InvalidOperationException($"JWKS request to {fetchUrl} returned HTTP {(int)response.StatusCode}");
        }

        var json = await response.Content.ReadAsStringAsync(cancellationToken);
        var keys = new JsonWebKeySet(json).Keys.ToList();
        logger.LogDebug("Fetched {Count} keys from {JwksUrl}", keys.Count, fetchUrl);
        _cache[jwksUrl] = new CacheEntry(keys, now);
        return keys;
    }
}
