namespace Nirmitee.DaVinci.Common.Http;

/// <summary>Replaces a URL prefix on outbound requests, for example http://localhost with http://host.docker.internal.</summary>
public sealed class UrlRewriteRule
{
    public string From { get; set; } = string.Empty;
    public string To { get; set; } = string.Empty;
}

/// <summary>Applies prefix rewrites to outbound URLs and normalizes URLs for comparison.</summary>
public static class UrlRewriter
{
    public static string Rewrite(string url, IEnumerable<UrlRewriteRule>? rules)
    {
        if (rules is null)
        {
            return url;
        }

        // Longest prefix wins so that specific rules override general ones.
        foreach (var rule in rules.Where(r => !string.IsNullOrEmpty(r.From)).OrderByDescending(r => r.From.Length))
        {
            if (url.StartsWith(rule.From, StringComparison.OrdinalIgnoreCase))
            {
                return rule.To + url[rule.From.Length..];
            }
        }

        return url;
    }

    /// <summary>Lower-case scheme and host, default port removed, no trailing slash.</summary>
    public static string Normalize(string? url)
    {
        if (string.IsNullOrWhiteSpace(url))
        {
            return string.Empty;
        }

        var trimmed = url.Trim().TrimEnd('/');
        if (!Uri.TryCreate(trimmed, UriKind.Absolute, out var uri))
        {
            return trimmed;
        }

        var port = uri.IsDefaultPort ? string.Empty : $":{uri.Port}";
        var path = uri.AbsolutePath.TrimEnd('/');
        return $"{uri.Scheme.ToLowerInvariant()}://{uri.Host.ToLowerInvariant()}{port}{path}{uri.Query}";
    }
}
