using Nirmitee.DaVinci.Common.Http;

namespace Nirmitee.DaVinci.Common.Security;

/// <summary>How a server trusts the JWTs presented by CDS clients.</summary>
public sealed class ClientAuthOptions
{
    /// <summary>CDS clients whose JWTs are accepted. The JWKS URL defaults to {issuer}/jwks.json.</summary>
    public List<TrustedIssuerOptions> TrustedIssuers { get; set; } = [];

    /// <summary>JWT signing algorithms accepted. Defaults to ES384, RS384, ES256 and RS256.</summary>
    public List<string> AllowedAlgorithms { get; set; } = [];

    /// <summary>Allowed clock skew for exp and iat.</summary>
    public int ClockSkewSeconds { get; set; } = 60;

    /// <summary>How long a fetched JWKS is cached.</summary>
    public int JwksCacheMinutes { get; set; } = 5;

    /// <summary>
    /// Externally visible base URLs of the server (for example an HTTPS tunnel), used with the request URL and
    /// X-Forwarded headers to compute accepted audience values.
    /// </summary>
    public List<string> PublicBaseUrls { get; set; } = [];

    /// <summary>Prefix rewrites applied to JWKS URLs before fetching.</summary>
    public List<UrlRewriteRule> UrlRewrites { get; set; } = [];

    public static readonly string[] DefaultAlgorithms = ["ES384", "RS384", "ES256", "RS256"];

    public void ApplyDefaults(IEnumerable<string> defaultIssuers)
    {
        if (AllowedAlgorithms.Count == 0)
        {
            AllowedAlgorithms.AddRange(DefaultAlgorithms);
        }

        TrustedIssuers.RemoveAll(i => string.IsNullOrWhiteSpace(i.Issuer));
        if (TrustedIssuers.Count == 0)
        {
            TrustedIssuers.AddRange(defaultIssuers.Select(i => new TrustedIssuerOptions { Issuer = i }));
        }
    }
}

public sealed class TrustedIssuerOptions
{
    /// <summary>The "iss" value sent by the CDS client (compared exactly, trailing slash ignored).</summary>
    public string Issuer { get; set; } = string.Empty;

    /// <summary>Optional JWKS URL. Defaults to {Issuer}/jwks.json.</summary>
    public string? JwksUrl { get; set; }

    public string EffectiveJwksUrl =>
        string.IsNullOrWhiteSpace(JwksUrl) ? $"{Issuer.TrimEnd('/')}/jwks.json" : JwksUrl!;
}
