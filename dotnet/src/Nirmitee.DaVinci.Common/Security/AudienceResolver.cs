using Microsoft.AspNetCore.Http;

namespace Nirmitee.DaVinci.Common.Security;

/// <summary>Computes the JWT audience values a request may legitimately carry.</summary>
public static class AudienceResolver
{
    /// <summary>
    /// {scheme}://{host}{path} where scheme is X-Forwarded-Proto or the connection scheme and host is
    /// X-Forwarded-Host or Host (path without query or trailing slash), the plain connection URL, and
    /// {base}{path} for each configured public base URL.
    /// </summary>
    public static List<string> AcceptedAudiences(HttpRequest request, IEnumerable<string>? publicBaseUrls)
    {
        var path = $"{request.PathBase}{request.Path}".TrimEnd('/');
        var scheme = FirstHeaderValue(request, "X-Forwarded-Proto") ?? request.Scheme;
        var host = FirstHeaderValue(request, "X-Forwarded-Host") ?? request.Host.ToString();

        var audiences = new List<string>
        {
            $"{scheme}://{host}{path}",
            $"{request.Scheme}://{request.Host}{path}",
        };

        foreach (var baseUrl in publicBaseUrls?.Where(u => !string.IsNullOrWhiteSpace(u)) ?? [])
        {
            audiences.Add(baseUrl.Trim().TrimEnd('/') + path);
        }

        return audiences.Distinct(StringComparer.Ordinal).ToList();
    }

    private static string? FirstHeaderValue(HttpRequest request, string name)
    {
        var value = request.Headers[name].ToString();
        return string.IsNullOrWhiteSpace(value) ? null : value.Split(',')[0].Trim();
    }
}
