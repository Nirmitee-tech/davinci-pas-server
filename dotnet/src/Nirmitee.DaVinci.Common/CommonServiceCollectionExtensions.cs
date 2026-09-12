using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.DependencyInjection.Extensions;
using Nirmitee.DaVinci.Common.Fhir;
using Nirmitee.DaVinci.Common.Security;

namespace Nirmitee.DaVinci.Common;

public static class CommonServiceCollectionExtensions
{
    /// <summary>
    /// Registers the shared services (JWKS cache, jti replay cache, CDS Hooks JWT validator, SMART client assertion
    /// validator, FHIR client factory). Safe to call from several modules; registrations are only added once.
    /// </summary>
    public static IServiceCollection AddDaVinciCommon(this IServiceCollection services)
    {
        services.TryAddSingleton(TimeProvider.System);
        services.AddHttpClient(JwksProvider.HttpClientName, client => client.Timeout = TimeSpan.FromSeconds(5));
        services.AddHttpClient(FhirDataClientFactory.HttpClientName, client => client.Timeout = Timeout.InfiniteTimeSpan);
        services.TryAddSingleton<JwksProvider>();
        services.TryAddSingleton<JtiReplayCache>();
        services.TryAddSingleton<CdsHooksTokenValidator>();
        services.TryAddSingleton<ClientAssertionValidator>();
        services.TryAddSingleton<FhirDataClientFactory>();
        return services;
    }
}
