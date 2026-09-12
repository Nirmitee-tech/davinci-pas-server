using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.TestHost;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.Hosting;
using Nirmitee.DaVinci.Pas.Tests.Support;

namespace Nirmitee.DaVinci.Pas.Tests;

public sealed class HttpBehaviorTests(PasAppFactory factory) : IClassFixture<PasAppFactory>
{
    private static JsonObject Approval() => (JsonObject)TestPaths.TestData("v221", "approval_submit.json").DeepClone();

    private static void AssertOutcome(Reply reply, int status, string code, string? expression = null)
    {
        Assert.True(reply.Code == status, $"expected {status}, got {reply.Code}: {reply.Text}");
        Assert.Equal("application/fhir+json", reply.MediaType);
        Assert.Equal("OperationOutcome", reply.Body!["resourceType"]!.GetValue<string>());
        var issue = reply.Body["issue"]![0]!;
        Assert.Equal(code, issue["code"]!.GetValue<string>());
        Assert.Equal(issue["diagnostics"]!.GetValue<string>(), issue["details"]!["text"]!.GetValue<string>());
        if (expression is not null)
        {
            Assert.Equal(expression, issue["expression"]![0]!.GetValue<string>());
        }
    }

    public static TheoryData<string, string, string, string> InvalidSubmissions()
    {
        return new TheoryData<string, string, string, string>
        {
            { "not-json", "structure", "Bundle", "submit" },
            { "patient", "structure", "Bundle", "submit" },
            { "batch", "invalid", "Bundle.type", "submit" },
            { "empty", "required", "Bundle.entry", "inquire" },
            { "first-not-claim", "invalid", "Bundle.entry[0].resource", "submit" },
            { "unresolved-patient", "invalid", "Claim.patient", "submit" },
            { "unresolved-provider", "invalid", "Claim.provider", "inquire" },
            { "no-items", "required", "Claim.item", "submit" },
        };
    }

    [Theory]
    [MemberData(nameof(InvalidSubmissions))]
    public async Task Unprocessable_bundles_get_400_operation_outcomes(string variant, string code, string expression, string operation)
    {
        using var client = factory.Client();
        var bundle = Approval();
        string? raw = null;
        switch (variant)
        {
            case "not-json":
                raw = "{ not json";
                break;
            case "patient":
                bundle = new JsonObject { ["resourceType"] = "Patient" };
                break;
            case "batch":
                bundle["type"] = "batch";
                break;
            case "empty":
                bundle["entry"] = new JsonArray();
                break;
            case "first-not-claim":
                var entries = bundle["entry"]!.AsArray();
                var claim = entries[0]!;
                entries.RemoveAt(0);
                entries.Add(claim);
                break;
            case "unresolved-patient":
                bundle["entry"]![0]!["resource"]!["patient"] = new JsonObject { ["reference"] = "Patient/unknown" };
                break;
            case "unresolved-provider":
                bundle["entry"]![0]!["resource"]!["provider"] = new JsonObject { ["reference"] = "urn:uuid:00000000-0000-4000-8000-000000000000" };
                break;
            case "no-items":
                bundle["entry"]![0]!["resource"]!.AsObject().Remove("item");
                break;
        }

        var reply = await PasAppFactory.SendAsync(client, HttpMethod.Post, $"/v221/fhir/Claim/${operation}", bundle, rawBody: raw);
        AssertOutcome(reply, 400, code, expression);
    }

    [Fact]
    public async Task Inquiry_accepts_a_claim_that_is_not_first_and_has_no_items()
    {
        using var client = factory.Client();
        var bundle = Approval();
        var entries = bundle["entry"]!.AsArray();
        var claim = entries[0]!;
        entries.RemoveAt(0);
        entries.Add(claim);
        claim["resource"]!.AsObject().Remove("item");

        var reply = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/Claim/$inquire", bundle);
        Assert.Equal(200, reply.Code);
        Assert.Equal("Bundle", reply.Body!["resourceType"]!.GetValue<string>());
        Assert.Equal("ClaimResponse", reply.Body["entry"]![0]!["resource"]!["resourceType"]!.GetValue<string>());
    }

    [Fact]
    public async Task Unknown_routes_versions_and_methods_get_operation_outcomes()
    {
        using var client = factory.Client();
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v221/fhir/Patient/123"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v220/fhir/metadata"), 404, "not-found");

        var wrongMethod = await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v221/fhir/Claim/$submit");
        AssertOutcome(wrongMethod, 405, "not-supported");
        Assert.Equal("POST", wrongMethod.Header("Allow"));
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/metadata"), 405, "not-supported");
    }

    [Fact]
    public async Task Subscription_lifecycle_and_validation()
    {
        using var client = factory.Client();
        var subscription = (JsonObject)TestPaths.Example("v221", "subscription-create-request.json").DeepClone();

        var wrongTopic = (JsonObject)subscription.DeepClone();
        wrongTopic["criteria"] = "http://example.org/SubscriptionTopic/other";
        wrongTopic["channel"]!["type"] = "websocket";
        var invalid = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", wrongTopic);
        AssertOutcome(invalid, 422, "business-rule");
        Assert.Contains("criteria", invalid.Body!["issue"]![0]!["diagnostics"]!.GetValue<string>());
        Assert.Contains("rest-hook", invalid.Body["issue"]![0]!["diagnostics"]!.GetValue<string>());

        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", new JsonObject { ["resourceType"] = "Patient" }), 400, "structure");

        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/Subscription", subscription);
        Assert.Equal(201, created.Code);
        var id = created.Body!["id"]!.GetValue<string>();
        Assert.Equal("requested", created.Body["status"]!.GetValue<string>());

        var status = await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v201/fhir/Subscription/{id}/$status");
        Assert.Equal(200, status.Code);
        Assert.Equal("searchset", status.Body!["type"]!.GetValue<string>());
        var parameters = status.Body["entry"]![0]!["resource"]!["parameter"]!.AsArray();
        Assert.Contains(parameters, p => p!["name"]!.GetValue<string>() == "type" && p["valueCode"]!.GetValue<string>() == "query-status");

        var updated = (JsonObject)subscription.DeepClone();
        updated["reason"] = "updated";
        var put = await PasAppFactory.SendAsync(client, HttpMethod.Put, $"/v201/fhir/Subscription/{id}", updated);
        Assert.Equal(200, put.Code);
        Assert.Equal("updated", (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v201/fhir/Subscription/{id}")).Body!["reason"]!.GetValue<string>());

        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v221/fhir/Subscription/{id}"), 404, "not-found");
        Assert.Equal(204, (await PasAppFactory.SendAsync(client, HttpMethod.Delete, $"/v201/fhir/Subscription/{id}")).Code);
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v201/fhir/Subscription/{id}"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v201/fhir/ClaimResponse/unknown"), 404, "not-found");
    }

    [Fact]
    public async Task Bearer_tokens_are_required_when_configured_except_for_metadata()
    {
        await using var secured = new PasAppFactory(new Dictionary<string, string> { ["PAS_AUTH_TOKENS"] = "test-token-one, test-token-two" });
        using var client = secured.Client();

        Assert.Equal(200, (await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v221/fhir/metadata")).Code);

        var missing = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", Approval());
        AssertOutcome(missing, 401, "security");
        Assert.Equal("Bearer", missing.Header("WWW-Authenticate"));
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", Approval(), token: "wrong"), 401, "security");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v221/fhir/Subscription/x"), 401, "security");

        Assert.Equal(200, (await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", Approval(), token: "test-token-two")).Code);
    }

    [Fact]
    public async Task Auth_is_off_by_default()
    {
        using var client = factory.Client();
        Assert.Equal(200, (await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", Approval())).Code);
    }

    [Fact]
    public async Task Control_routes_refuse_proxied_requests_and_can_be_disabled()
    {
        using var client = factory.Client();
        var proxied = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/reset", configure: r => r.Headers.Add("X-Forwarded-For", "203.0.113.9"));
        AssertOutcome(proxied, 403, "forbidden");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, "/admin/notifications", configure: r => r.Headers.Add("Forwarded", "for=203.0.113.9")), 403, "forbidden");

        var health = await PasAppFactory.SendAsync(client, HttpMethod.Get, "/health");
        Assert.Equal(200, health.Code);
        Assert.Equal("ok", health.Body!["status"]!.GetValue<string>());

        await using var disabled = new PasAppFactory(new Dictionary<string, string> { ["PAS_ADMIN_ENABLED"] = "false" });
        using var disabledClient = disabled.Client();
        AssertOutcome(await PasAppFactory.SendAsync(disabledClient, HttpMethod.Post, "/admin/pended/finalize"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(disabledClient, HttpMethod.Post, "/admin/reset"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(disabledClient, HttpMethod.Get, "/admin/notifications"), 404, "not-found");
    }

    [Fact]
    public async Task Control_routes_are_disabled_by_default()
    {
        var builder = WebApplication.CreateBuilder(new WebApplicationOptions { EnvironmentName = Environments.Production });
        builder.WebHost.UseTestServer();
        builder.Configuration.AddInMemoryCollection(new Dictionary<string, string?> { ["PAS_DATA_DIR"] = factory.Root });
        builder.Services.AddPas(builder.Configuration);
        await using var app = builder.Build();
        app.MapPas();
        await app.StartAsync();

        using var client = app.GetTestClient();
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/reset"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Get, "/admin/notifications"), 404, "not-found");
        Assert.Equal(200, (await PasAppFactory.SendAsync(client, HttpMethod.Get, "/health")).Code);
    }

    [Fact]
    public async Task Control_routes_ignore_bound_EnableAdmin_setting()
    {
        var builder = WebApplication.CreateBuilder(new WebApplicationOptions { EnvironmentName = Environments.Production });
        builder.WebHost.UseTestServer();
        builder.Configuration.AddInMemoryCollection(new Dictionary<string, string?>
        {
            ["PAS_DATA_DIR"] = factory.Root,
            ["Pas:EnableAdmin"] = "true",
        });
        builder.Services.AddPas(builder.Configuration);
        await using var app = builder.Build();
        app.MapPas();
        await app.StartAsync();

        using var client = app.GetTestClient();
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize"), 404, "not-found");
        AssertOutcome(await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/reset"), 404, "not-found");
    }

    [Fact]
    public async Task Reset_clears_claims_and_subscriptions()
    {
        await using var isolated = new PasAppFactory();
        using var client = isolated.Client();
        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", TestPaths.Example("v221", "subscription-create-request.json"));
        var id = created.Body!["id"]!.GetValue<string>();
        var submitted = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", Approval());
        var claimResponseId = submitted.Body!["entry"]![0]!["resource"]!["id"]!.GetValue<string>();
        Assert.Equal(200, (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v221/fhir/ClaimResponse/{claimResponseId}")).Code);

        Assert.Equal(204, (await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/reset")).Code);
        Assert.Equal(404, (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v221/fhir/Subscription/{id}")).Code);
        Assert.Equal(404, (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v221/fhir/ClaimResponse/{claimResponseId}")).Code);
    }

    [Fact]
    public async Task Audit_log_records_requests_without_bearer_tokens()
    {
        await using var isolated = new PasAppFactory();
        using var client = isolated.Client();
        var subscription = (JsonObject)TestPaths.Example("v221", "subscription-create-request.json").DeepClone();
        subscription["channel"]!["header"] = new JsonArray(JsonValue.Create("Authorization: Bearer very-secret-notification-token"));
        Assert.Equal(201, (await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", subscription, token: "caller-secret-token")).Code);
        await isolated.Sink.WaitForAsync(n => n.Kind == "handshake");
        await PasAppFactory.SendAsync(client, HttpMethod.Get, "/admin/notifications");
        await Task.Delay(300);

        var text = string.Join("\n", Directory.GetFiles(isolated.AuditDir, "*.jsonl").Select(File.ReadAllText));
        Assert.Contains("\"operation\":\"subscription-create\"", text);
        Assert.Contains("\"operation\":\"notify-handshake\"", text);
        Assert.Contains("[redacted]", text);
        Assert.DoesNotContain("very-secret-notification-token", text);
        Assert.DoesNotContain("caller-secret-token", text);
    }

    [Fact]
    public async Task Public_base_url_and_forwarded_headers_change_the_fhir_base()
    {
        using var client = factory.Client("http://localhost:8086");
        var forwarded = await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v221/fhir/metadata", configure: r =>
        {
            r.Headers.Add("X-Forwarded-Proto", "https");
            r.Headers.Add("X-Forwarded-Host", "pas.example.ngrok-free.app");
        });
        Assert.Equal("https://pas.example.ngrok-free.app/v221/fhir", forwarded.Body!["implementation"]!["url"]!.GetValue<string>());

        await using var publicFactory = new PasAppFactory(new Dictionary<string, string> { ["PAS_PUBLIC_BASE_URL"] = "https://fixed.example.org/" });
        using var publicClient = publicFactory.Client();
        var metadata = await PasAppFactory.SendAsync(publicClient, HttpMethod.Get, "/v201/fhir/metadata");
        Assert.Equal("https://fixed.example.org/v201/fhir", metadata.Body!["implementation"]!["url"]!.GetValue<string>());
    }

    [Fact]
    public async Task Rules_file_setting_is_loaded_from_disk()
    {
        await using var fromFile = new PasAppFactory(new Dictionary<string, string> { ["PAS_RULES_FILE"] = TestPaths.RulesFile });
        using var client = fromFile.Client();
        var health = await PasAppFactory.SendAsync(client, HttpMethod.Get, "/health");
        Assert.Equal(TestPaths.RulesFile, health.Body!["rulesSource"]!.GetValue<string>());
    }

    [Fact]
    public async Task Module_mounts_under_a_prefix_in_another_host()
    {
        var builder = WebApplication.CreateBuilder(new WebApplicationOptions { EnvironmentName = Environments.Development });
        builder.WebHost.UseTestServer();
        builder.Configuration.AddInMemoryCollection(new Dictionary<string, string?> { ["PAS_DATA_DIR"] = factory.Root });
        builder.Services.AddPas(builder.Configuration);
        await using var app = builder.Build();
        app.MapPas("/burden-reduction/pas/");
        await app.StartAsync();

        using var client = app.GetTestClient();
        var metadata = await PasAppFactory.SendAsync(client, HttpMethod.Get, "/burden-reduction/pas/v221/fhir/metadata");
        Assert.Equal(200, metadata.Code);
        Assert.Equal("http://localhost/burden-reduction/pas/v221/fhir", metadata.Body!["implementation"]!["url"]!.GetValue<string>());
        Assert.Equal(200, (await PasAppFactory.SendAsync(client, HttpMethod.Get, "/burden-reduction/pas/health")).Code);
        Assert.Equal(404, (await PasAppFactory.SendAsync(client, HttpMethod.Get, "/v221/fhir/metadata")).Code);
    }
}
