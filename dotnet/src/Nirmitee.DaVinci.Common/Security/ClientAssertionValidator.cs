using System.Collections.Concurrent;
using Microsoft.Extensions.Logging;
using Microsoft.IdentityModel.JsonWebTokens;
using Microsoft.IdentityModel.Tokens;
using Nirmitee.DaVinci.Common.Http;

namespace Nirmitee.DaVinci.Common.Security;

/// <summary>How a SMART Backend Services token endpoint trusts client assertions (private_key_jwt).</summary>
public sealed class ClientAssertionOptions
{
    /// <summary>Registered client_id values; iss and sub must both equal one of them.</summary>
    public IReadOnlyCollection<string> ClientIds { get; init; } = [];

    /// <summary>JWKS URLs searched in order for the assertion kid.</summary>
    public IReadOnlyList<string> JwksUrls { get; init; } = [];

    /// <summary>Optional local JWKS file, searched before the URLs.</summary>
    public string? JwksFile { get; init; }

    public IReadOnlyCollection<string> AllowedAlgorithms { get; init; } = DefaultAlgorithms;

    public int ClockSkewSeconds { get; init; } = 60;

    /// <summary>Maximum distance of exp from now (SMART: no more than five minutes in the future).</summary>
    public int MaxLifetimeSeconds { get; init; } = 300;

    public int JwksCacheMinutes { get; init; } = 5;

    public IEnumerable<UrlRewriteRule>? UrlRewrites { get; init; }

    public static readonly string[] DefaultAlgorithms = ["RS384", "ES384"];
}

public sealed record ClientAssertionOutcome(bool IsValid, string? Error, string? ClientId, string? Kid, string? Jti)
{
    public static ClientAssertionOutcome Fail(string error, string? clientId = null, string? kid = null, string? jti = null) =>
        new(false, error, clientId, kid, jti);
}

/// <summary>
/// Validates a SMART Backend Services client assertion: alg (RS384 or ES384) and kid, a key with that kid in the
/// trusted JWKS sets (refetched once when unknown), signature, iss equal to sub and to a registered client_id,
/// aud equal to the token endpoint URL, exp in the future and at most five minutes ahead, and a jti not used before.
/// </summary>
public sealed class ClientAssertionValidator(
    JwksProvider jwksProvider,
    JtiReplayCache replayCache,
    TimeProvider time,
    ILogger<ClientAssertionValidator> logger)
{
    private static readonly TimeSpan FailedUrlBackoff = TimeSpan.FromSeconds(30);

    private readonly JsonWebTokenHandler _handler = new() { MapInboundClaims = false };
    private readonly ConcurrentDictionary<string, DateTimeOffset> _failedUntil = new(StringComparer.Ordinal);

    public async Task<ClientAssertionOutcome> ValidateAsync(
        string? assertion,
        string tokenEndpointUrl,
        ClientAssertionOptions options,
        CancellationToken cancellationToken)
    {
        if (string.IsNullOrWhiteSpace(assertion))
        {
            return ClientAssertionOutcome.Fail("client_assertion is missing");
        }

        JsonWebToken token;
        try
        {
            token = _handler.ReadJsonWebToken(assertion);
        }
        catch (Exception)
        {
            return ClientAssertionOutcome.Fail("client_assertion is not a JWT");
        }

        var issuer = string.IsNullOrEmpty(token.Issuer) ? null : token.Issuer;
        var kid = string.IsNullOrEmpty(token.Kid) ? null : token.Kid;
        var jti = string.IsNullOrEmpty(token.Id) ? null : token.Id;
        ClientAssertionOutcome Fail(string error) => ClientAssertionOutcome.Fail(error, issuer, kid, jti);

        var algorithms = options.AllowedAlgorithms.Count > 0 ? options.AllowedAlgorithms : ClientAssertionOptions.DefaultAlgorithms;
        if (string.IsNullOrEmpty(token.Alg) || !algorithms.Contains(token.Alg, StringComparer.Ordinal))
        {
            return Fail($"client_assertion alg '{token.Alg}' is not accepted");
        }

        if (kid is null)
        {
            return Fail("client_assertion has no kid header");
        }

        if (issuer is null || !string.Equals(issuer, token.Subject, StringComparison.Ordinal))
        {
            return Fail("client_assertion iss and sub must both be the client_id");
        }

        if (!options.ClientIds.Contains(issuer, StringComparer.Ordinal))
        {
            return Fail($"client_id '{issuer}' is not registered");
        }

        var keys = await FindKeysAsync(kid, token.Alg, options, cancellationToken);
        if (keys.Count == 0)
        {
            return Fail("no trusted JWKS key matches the client_assertion kid and alg");
        }

        var skew = TimeSpan.FromSeconds(Math.Max(0, options.ClockSkewSeconds));
        var expectedAudience = UrlRewriter.Normalize(tokenEndpointUrl);
        var parameters = new TokenValidationParameters
        {
            ValidateIssuer = false,
            ValidateAudience = true,
            AudienceValidator = (audiences, _, _) => audiences.Any(a => UrlRewriter.Normalize(a) == expectedAudience),
            ValidateLifetime = true,
            RequireExpirationTime = true,
            ClockSkew = skew,
            IssuerSigningKeys = keys,
            TryAllIssuerSigningKeys = true,
            ValidAlgorithms = algorithms,
            RequireSignedTokens = true,
            ValidateIssuerSigningKey = false,
        };

        var result = await _handler.ValidateTokenAsync(assertion, parameters);
        if (!result.IsValid)
        {
            return Fail(DescribeFailure(result.Exception));
        }

        var expires = new DateTimeOffset(DateTime.SpecifyKind(token.ValidTo, DateTimeKind.Utc));
        if (expires > time.GetUtcNow().AddSeconds(Math.Max(0, options.MaxLifetimeSeconds)).Add(skew))
        {
            return Fail("client_assertion exp is more than five minutes in the future");
        }

        if (jti is null)
        {
            return Fail("client_assertion has no jti");
        }

        if (!replayCache.TryRegister("client-assertion\n" + issuer, jti, expires + skew))
        {
            return Fail("client_assertion jti has already been used");
        }

        return new ClientAssertionOutcome(true, null, issuer, kid, jti);
    }

    private async Task<List<SecurityKey>> FindKeysAsync(string kid, string alg, ClientAssertionOptions options, CancellationToken cancellationToken)
    {
        if (!string.IsNullOrWhiteSpace(options.JwksFile))
        {
            try
            {
                var fromFile = SelectKeys(new JsonWebKeySet(await File.ReadAllTextAsync(options.JwksFile, cancellationToken)).Keys, kid, alg);
                if (fromFile.Count > 0)
                {
                    return fromFile;
                }
            }
            catch (Exception ex) when (ex is not OperationCanceledException)
            {
                logger.LogWarning(ex, "Unable to read the JWKS file {JwksFile}", options.JwksFile);
            }
        }

        var cacheDuration = TimeSpan.FromMinutes(Math.Max(1, options.JwksCacheMinutes));
        foreach (var forceRefresh in new[] { false, true })
        {
            foreach (var url in options.JwksUrls.Where(u => !string.IsNullOrWhiteSpace(u)))
            {
                var now = time.GetUtcNow();
                if (!forceRefresh && _failedUntil.TryGetValue(url, out var until) && until > now)
                {
                    continue;
                }

                try
                {
                    var keys = SelectKeys(await jwksProvider.GetKeysAsync(url, forceRefresh, cacheDuration, options.UrlRewrites, cancellationToken), kid, alg);
                    _failedUntil.TryRemove(url, out _);
                    if (keys.Count > 0)
                    {
                        return keys;
                    }
                }
                catch (Exception ex) when (ex is not OperationCanceledException)
                {
                    _failedUntil[url] = now + FailedUrlBackoff;
                    logger.LogDebug(ex, "Unable to load JWKS from {JwksUrl}", url);
                }
            }
        }

        return [];
    }

    private static List<SecurityKey> SelectKeys(IEnumerable<JsonWebKey> keys, string kid, string alg)
    {
        var expectedKty = alg.StartsWith("ES", StringComparison.Ordinal) ? "EC" : "RSA";
        return keys
            .Where(k => string.Equals(k.Kty, expectedKty, StringComparison.Ordinal))
            .Where(k => string.IsNullOrEmpty(k.Alg) || string.Equals(k.Alg, alg, StringComparison.Ordinal))
            .Where(k => k.KeyOps.Count == 0 || k.KeyOps.Contains("verify"))
            .Where(k => string.Equals(k.Kid, kid, StringComparison.Ordinal))
            .Cast<SecurityKey>()
            .ToList();
    }

    private static string DescribeFailure(Exception? exception) => exception switch
    {
        SecurityTokenExpiredException => "client_assertion has expired",
        SecurityTokenNoExpirationException => "client_assertion has no exp",
        SecurityTokenInvalidAudienceException => "client_assertion aud is not the token endpoint URL",
        SecurityTokenNotYetValidException => "client_assertion is not yet valid",
        SecurityTokenInvalidLifetimeException => "client_assertion lifetime (nbf, exp) is not valid",
        SecurityTokenSignatureKeyNotFoundException => "client_assertion signature key was not found",
        SecurityTokenInvalidSignatureException => "client_assertion signature is invalid",
        SecurityTokenInvalidAlgorithmException => "client_assertion alg is not accepted",
        null => "client_assertion is not valid",
        _ => "client_assertion is not valid: " + exception.GetType().Name,
    };
}
