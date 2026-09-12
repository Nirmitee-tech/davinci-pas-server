using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Routing;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.DependencyInjection.Extensions;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Options;
using Nirmitee.DaVinci.Common;
using Nirmitee.DaVinci.Pas.Claims;
using Nirmitee.DaVinci.Pas.Fhir;
using Nirmitee.DaVinci.Pas.Http;
using Nirmitee.DaVinci.Pas.Notifications;
using Nirmitee.DaVinci.Pas.Rules;
using Nirmitee.DaVinci.Pas.Storage;

namespace Nirmitee.DaVinci.Pas;

public static class PasServiceCollectionExtensions
{
    /// <summary>
    /// Registers the PAS module: options ("Pas" section plus PAS_* variables), adjudication rules, in-memory claim,
    /// subscription, outbox and delivery log stores (replace them by registering your own implementations first), the
    /// engine, the request pipeline, the notification delivery service and the pended auto finalize service.
    /// </summary>
    public static IServiceCollection AddPas(this IServiceCollection services, IConfiguration configuration)
    {
        services.AddDaVinciCommon();
        services.AddOptions<PasOptions>()
            .Bind(configuration.GetSection(PasOptions.SectionName))
            .PostConfigure(options => options.Finalize(configuration));
        services.AddHttpClient(NotificationDeliveryService.HttpClientName, client => client.Timeout = Timeout.InfiniteTimeSpan);

        services.TryAddSingleton(sp => AdjudicationRules.Load(sp.GetRequiredService<IOptions<PasOptions>>().Value.RulesFile));
        services.TryAddSingleton<IClaimStore, InMemoryClaimStore>();
        services.TryAddSingleton<ISubscriptionStore, InMemorySubscriptionStore>();
        services.TryAddSingleton<INotificationOutbox, InMemoryNotificationOutbox>();
        services.TryAddSingleton<INotificationLog, InMemoryNotificationLog>();
        services.TryAddSingleton<PasAuditLog>();
        services.TryAddSingleton<PasEngine>();
        services.TryAddSingleton<PasPipeline>();
        services.TryAddEnumerable(ServiceDescriptor.Singleton<IHostedService, NotificationDeliveryService>());
        services.TryAddEnumerable(ServiceDescriptor.Singleton<IHostedService, PendedFinalizerService>());
        return services;
    }
}

public static class PasEndpointRouteBuilderExtensions
{
    private delegate PasResponse Handler(PasCall call, PasEngine engine);

    private sealed record Method(string Verb, string Operation, bool Auth, bool Body, Handler Handle);

    /// <summary>
    /// Maps the PAS endpoints under <paramref name="prefix"/>: for each FHIR base {prefix}/{v221|v201}/fhir the
    /// CapabilityStatement, Claim/$submit, Claim/$inquire, Subscription CRUD and $status and ClaimResponse read;
    /// GET {prefix}/health; and the test-only control routes under {prefix}/admin (404 unless PAS_ADMIN_ENABLED=true).
    /// Unknown FHIR routes return 404 and unsupported methods 405, both with an OperationOutcome.
    /// </summary>
    public static RouteGroupBuilder MapPas(this IEndpointRouteBuilder endpoints, string? prefix = null)
    {
        var normalized = NormalizePrefix(prefix);
        var group = endpoints.MapGroup(normalized);
        const string Base = "/{version}/fhir";

        group.MapGet("/health", (PasEngine engine) => Results.Json(new
        {
            status = "ok",
            module = "pas",
            rulesVersion = engine.Rules.RulesVersion,
            rulesSource = engine.Rules.Source,
            fhirBases = PasVersions.All.Select(v => $"{normalized}/{v}/fhir").ToArray(),
        }));

        group.Map("/admin/{**rest}", (HttpContext context, PasPipeline pipeline, PasEngine engine, INotificationLog deliveries, IOptionsMonitor<PasOptions> options) =>
            MapAdmin(context, normalized, pipeline, engine, deliveries, options.CurrentValue));

        MapFhir(group, normalized, Base + "/metadata",
            new Method("GET", "metadata", false, false, (call, engine) => engine.Metadata(call.Version!, call.FhirBase)));

        MapFhir(group, normalized, Base + "/Claim/$submit",
            new Method("POST", "submit", true, true, (call, engine) => engine.Submit(call.Version!, call.FhirBase, call.Body)));

        MapFhir(group, normalized, Base + "/Claim/$inquire",
            new Method("POST", "inquire", true, true, (call, engine) => engine.Inquire(call.Version!, call.FhirBase, call.Body)));

        MapFhir(group, normalized, Base + "/Subscription",
            new Method("POST", "subscription-create", true, true, (call, engine) => engine.CreateSubscription(call.Version!, call.FhirBase, call.Body)));

        MapFhir(group, normalized, Base + "/Subscription/{id}",
            new Method("GET", "subscription-read", true, false, (call, engine) => engine.ReadSubscription(call.Version!, call.RouteValue("id"))),
            new Method("PUT", "subscription-update", true, true, (call, engine) => engine.UpdateSubscription(call.Version!, call.RouteValue("id"), call.Body)),
            new Method("DELETE", "subscription-delete", true, false, (call, engine) => engine.DeleteSubscription(call.Version!, call.RouteValue("id"))));

        MapFhir(group, normalized, Base + "/Subscription/{id}/$status",
            new Method("GET", "subscription-status", true, false, (call, engine) => engine.SubscriptionStatus(call.Version!, call.RouteValue("id"))));

        MapFhir(group, normalized, Base + "/ClaimResponse/{id}",
            new Method("GET", "claimresponse-read", true, false, (call, engine) => engine.ReadClaimResponse(call.Version!, call.RouteValue("id"))));

        group.MapFallback(Base + "/{**rest}", (HttpContext context, string version, PasPipeline pipeline) =>
            pipeline.RunAsync(context, normalized, version, "unknown", false, false, _ => NoRoute(context)));

        return group;
    }

    public static string NormalizePrefix(string? prefix) =>
        string.IsNullOrWhiteSpace(prefix) || prefix.Trim() == "/" ? string.Empty : "/" + prefix.Trim().Trim('/');

    /// <summary>One endpoint per path that dispatches on the HTTP method, so unsupported methods get a 405 OperationOutcome.</summary>
    private static void MapFhir(RouteGroupBuilder group, string prefix, string pattern, params Method[] methods)
    {
        var allow = string.Join(", ", methods.Select(m => m.Verb));
        group.Map(pattern, (HttpContext context, string version, PasPipeline pipeline, PasEngine engine) =>
        {
            var method = methods.FirstOrDefault(m => string.Equals(m.Verb, context.Request.Method, StringComparison.OrdinalIgnoreCase));
            if (method is null)
            {
                return pipeline.RunAsync(context, prefix, version, "method-not-allowed", false, false, _ =>
                {
                    var response = PasResponse.Outcome(405, "not-supported", $"{context.Request.Method} is not supported here. Allowed: {allow}.");
                    response.Headers["Allow"] = allow;
                    return response;
                });
            }

            return pipeline.RunAsync(context, prefix, version, method.Operation, method.Auth, method.Body, call => method.Handle(call, engine));
        });
    }

    /// <summary>
    /// Test-only control routes (SPEC section 11). Disabled unless PAS_ADMIN_ENABLED is exactly "true", in which case
    /// every /admin path answers 404 like an unknown route. When enabled, proxied requests are refused with 403.
    /// </summary>
    private static Task MapAdmin(HttpContext context, string prefix, PasPipeline pipeline, PasEngine engine, INotificationLog deliveries, PasOptions options)
    {
        var path = (context.Request.RouteValues["rest"]?.ToString() ?? string.Empty).Trim('/');
        var verb = context.Request.Method.ToUpperInvariant();
        var operation = (path, verb) switch
        {
            ("pended/finalize", "POST") => "admin-finalize",
            ("reset", "POST") => "admin-reset",
            ("notifications", "GET") => "admin-notifications",
            _ => "admin-unknown",
        };

        return pipeline.RunAsync(context, prefix, null, operation, false, options.EnableAdmin && operation == "admin-finalize", call =>
        {
            if (!options.EnableAdmin)
            {
                return NoRoute(context);
            }

            var headers = context.Request.Headers;
            if (headers.ContainsKey("X-Forwarded-For") || headers.ContainsKey("Forwarded"))
            {
                return PasResponse.Outcome(403, "forbidden", "Control routes are not available through a proxy.");
            }

            switch (operation)
            {
                case "admin-finalize":
                    var version = J.Str(call.Body, "version");
                    if (version is not null && !PasVersions.IsKnown(version))
                    {
                        return PasResponse.Outcome(400, "invalid", $"Unknown version '{version}'. Use v221 or v201.", "version");
                    }

                    var outcome = engine.FinalizeAll(version);
                    return new PasResponse(200, new JsonObject
                    {
                        ["finalized"] = new JsonArray(outcome.Finalized.Select(id => (JsonNode?)JsonValue.Create(id)).ToArray()),
                        ["notifications"] = outcome.Notifications,
                    }, PasResponse.JsonType);
                case "admin-reset":
                    engine.Reset();
                    return PasResponse.NoContent();
                case "admin-notifications":
                    var notifications = deliveries.List().Select(d => (JsonNode?)new JsonObject
                    {
                        ["version"] = d.Version,
                        ["kind"] = d.Kind,
                        ["subscription_id"] = d.SubscriptionId,
                        ["endpoint_token"] = d.EndpointToken,
                        ["attempts"] = d.Attempts,
                        ["status"] = d.Status,
                        ["delivered"] = d.Delivered,
                        ["queued_at"] = FhirJson.Instant(d.QueuedAt),
                    }).ToArray();
                    return new PasResponse(200, new JsonObject { ["notifications"] = new JsonArray(notifications) }, PasResponse.JsonType);
                default:
                    return path is "pended/finalize" or "reset" or "notifications"
                        ? PasResponse.Outcome(405, "not-supported", $"{context.Request.Method} is not supported here.")
                        : NoRoute(context);
            }
        });
    }

    private static PasResponse NoRoute(HttpContext context) =>
        PasResponse.Outcome(404, "not-found", $"No PAS route for {context.Request.Method} {context.Request.PathBase}{context.Request.Path}.");
}
