using Microsoft.Extensions.Logging;
using Microsoft.IdentityModel.JsonWebTokens;
using Microsoft.IdentityModel.Tokens;
using Nirmitee.DaVinci.Common.Http;

namespace Nirmitee.DaVinci.Common.Security;

public sealed record TokenValidationOutcome(bool IsValid, string? Error, string? Issuer, string? Kid, string? Jti)
{
    public static TokenValidationOutcome Fail(string error, string? issuer = null, string? kid = null, string? jti = null) =>
        new(false, error, issuer, kid, jti);
}

/// <summary>
/// Validates the bearer JWT a CDS client sends, following CDS Hooks 2.0 "Trusting CDS Clients":
/// allowed alg, trusted iss, signature from the issuer JWKS (a jku header must be that JWKS URL or live under the
/// issuer), exp and iat within the clock skew, aud containing the called URL, and a jti not seen before.
/// </summary>
public sealed class CdsHooksTokenValidator(
    JwksProvider jwksProvider,
    JtiReplayCache replayCache,
    TimeProvider time,
    ILogger<CdsHooksTokenValidator> logger)
{
    private const string BearerPrefix = "Bearer ";
    private readonly JsonWebTokenHandler _handler = new() { MapInboundClaims = false };

    public async Task<TokenValidationOutcome> ValidateAsync(
        string? authorizationHeader,
        IReadOnlyCollection<string> acceptedAudiences,
        ClientAuthOptions options,
        CancellationToken cancellationToken)
    {
        if (string.IsNullOrWhiteSpace(authorizationHeader))
        {
            return TokenValidationOutcome.Fail("missing bearer token");
        }

        if (!authorizationHeader.StartsWith(BearerPrefix, StringComparison.OrdinalIgnoreCase))
        {
            return TokenValidationOutcome.Fail("authorization header is not a bearer token");
        }

        var raw = authorizationHeader[BearerPrefix.Length..].Trim();
        JsonWebToken token;
        try
        {
            token = _handler.ReadJsonWebToken(raw);
        }
        catch (Exception)
        {
            return TokenValidationOutcome.Fail("malformed JWT");
        }

        var issuer = token.Issuer;
        var kid = string.IsNullOrEmpty(token.Kid) ? null : token.Kid;
        var jti = string.IsNullOrEmpty(token.Id) ? null : token.Id;
        TokenValidationOutcome Fail(string error) => TokenValidationOutcome.Fail(error, issuer, kid, jti);

        var algorithms = options.AllowedAlgorithms.Count > 0 ? options.AllowedAlgorithms : [.. ClientAuthOptions.DefaultAlgorithms];
        if (string.IsNullOrEmpty(token.Alg) || !algorithms.Contains(token.Alg, StringComparer.Ordinal))
        {
            return Fail($"JWT alg '{token.Alg}' is not accepted");
        }

        if (string.IsNullOrWhiteSpace(issuer))
        {
            return Fail("JWT has no iss");
        }

        var trusted = options.TrustedIssuers.FirstOrDefault(
            t => string.Equals(t.Issuer.TrimEnd('/'), issuer.TrimEnd('/'), StringComparison.Ordinal));
        if (trusted is null)
        {
            return Fail($"JWT issuer '{issuer}' is not trusted");
        }

        var jwksUrl = trusted.EffectiveJwksUrl;
        if (token.TryGetHeaderValue<string>("jku", out var jku) && !string.IsNullOrWhiteSpace(jku))
        {
            var jkuTrimmed = jku.TrimEnd('/');
            var underIssuer = jkuTrimmed.StartsWith(issuer.TrimEnd('/') + "/", StringComparison.Ordinal);
            if (!underIssuer && !string.Equals(jkuTrimmed, jwksUrl.TrimEnd('/'), StringComparison.Ordinal))
            {
                return Fail("JWT jku is not under the trusted issuer");
            }
        }

        var cacheDuration = TimeSpan.FromMinutes(Math.Max(1, options.JwksCacheMinutes));
        List<SecurityKey> keys;
        try
        {
            keys = SelectKeys(await jwksProvider.GetKeysAsync(jwksUrl, false, cacheDuration, options.UrlRewrites, cancellationToken), kid, token.Alg);
            if (keys.Count == 0 && kid is not null)
            {
                keys = SelectKeys(await jwksProvider.GetKeysAsync(jwksUrl, true, cacheDuration, options.UrlRewrites, cancellationToken), kid, token.Alg);
            }
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            logger.LogWarning(ex, "Unable to load JWKS from {JwksUrl}", jwksUrl);
            return Fail("unable to load the issuer JWKS");
        }

        if (keys.Count == 0)
        {
            return Fail("no JWKS key matches the JWT kid and alg");
        }

        var skew = TimeSpan.FromSeconds(Math.Max(0, options.ClockSkewSeconds));
        var normalizedAudiences = acceptedAudiences.Select(UrlRewriter.Normalize).ToHashSet(StringComparer.Ordinal);
        var parameters = new TokenValidationParameters
        {
            ValidateIssuer = true,
            ValidIssuer = issuer,
            ValidateAudience = true,
            AudienceValidator = (audiences, _, _) =>
                audiences.Any(a => normalizedAudiences.Contains(UrlRewriter.Normalize(a))),
            ValidateLifetime = true,
            RequireExpirationTime = true,
            ClockSkew = skew,
            IssuerSigningKeys = keys,
            TryAllIssuerSigningKeys = true,
            ValidAlgorithms = algorithms,
            RequireSignedTokens = true,
            ValidateIssuerSigningKey = false,
        };

        var result = await _handler.ValidateTokenAsync(raw, parameters);
        if (!result.IsValid)
        {
            return Fail(DescribeFailure(result.Exception));
        }

        if (!token.TryGetPayloadValue<long>("iat", out var iat))
        {
            return Fail("JWT has no iat");
        }

        if (DateTimeOffset.FromUnixTimeSeconds(iat) >= time.GetUtcNow().Add(skew))
        {
            return Fail("JWT iat is in the future");
        }

        if (jti is null)
        {
            return Fail("JWT has no jti");
        }

        var expiry = new DateTimeOffset(DateTime.SpecifyKind(token.ValidTo, DateTimeKind.Utc));
        if (!replayCache.TryRegister(issuer, jti, expiry + skew))
        {
            return Fail("JWT jti has already been used");
        }

        return new TokenValidationOutcome(true, null, issuer, kid, jti);
    }

    private static List<SecurityKey> SelectKeys(IReadOnlyList<JsonWebKey> keys, string? kid, string alg)
    {
        var expectedKty = alg.StartsWith("ES", StringComparison.Ordinal) ? "EC" : "RSA";
        return keys
            .Where(k => string.Equals(k.Kty, expectedKty, StringComparison.Ordinal))
            .Where(k => string.IsNullOrEmpty(k.Alg) || string.Equals(k.Alg, alg, StringComparison.Ordinal))
            .Where(k => k.KeyOps.Count == 0 || k.KeyOps.Contains("verify"))
            .Where(k => kid is null || string.Equals(k.Kid, kid, StringComparison.Ordinal))
            .Cast<SecurityKey>()
            .ToList();
    }

    private static string DescribeFailure(Exception? exception) => exception switch
    {
        SecurityTokenExpiredException => "JWT has expired",
        SecurityTokenNoExpirationException => "JWT has no exp",
        SecurityTokenInvalidAudienceException => "JWT aud does not match the requested URL",
        SecurityTokenInvalidIssuerException => "JWT iss is not valid",
        SecurityTokenNotYetValidException => "JWT is not yet valid",
        SecurityTokenInvalidLifetimeException => "JWT lifetime (nbf, exp) is not valid",
        SecurityTokenSignatureKeyNotFoundException => "JWT signature key was not found",
        SecurityTokenInvalidSignatureException => "JWT signature is invalid",
        SecurityTokenInvalidAlgorithmException => "JWT alg is not accepted",
        null => "JWT is not valid",
        _ => "JWT is not valid: " + exception.GetType().Name,
    };
}
