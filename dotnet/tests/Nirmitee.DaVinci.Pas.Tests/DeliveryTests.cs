using System.Text.Json.Nodes;
using Nirmitee.DaVinci.Pas.Tests.Support;

namespace Nirmitee.DaVinci.Pas.Tests;

/// <summary>Notification retry contract, delivery log, auto finalize and Subscription filters (SPEC 7.2 to 7.4, 11).</summary>
public sealed class DeliveryTests
{
    private const string Token = "delivery-test-token";

    private static JsonObject Subscription(string version, string? filter = null)
    {
        var subscription = (JsonObject)TestPaths.Example(version, "subscription-create-request.json").DeepClone();
        subscription["channel"]!["header"] = new JsonArray(JsonValue.Create($"Authorization: Bearer {Token}"));
        if (filter is not null)
        {
            subscription["_criteria"]!["extension"]![0]!["valueString"] = filter;
        }

        return subscription;
    }

    private static async Task<JsonArray> NotificationsAsync(HttpClient client) =>
        (await PasAppFactory.SendAsync(client, HttpMethod.Get, "/admin/notifications")).Body!["notifications"]!.AsArray();

    private static async Task ActiveAsync(HttpClient client, string version, string id) =>
        await PasAppFactory.EventuallyAsync(async () =>
            (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/{version}/fhir/Subscription/{id}")).Body?["status"]?.GetValue<string>() == "active",
            "the Subscription to become active");

    [Fact]
    public async Task Handshake_is_retried_until_2xx_then_never_resent()
    {
        await using var factory = new PasAppFactory();
        factory.Sink.Respond = n => n <= 2 ? 500 : 200;
        using var client = factory.Client();

        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", Subscription("v221"));
        var id = created.Body!["id"]!.GetValue<string>();
        await ActiveAsync(client, "v221", id);

        var log = await NotificationsAsync(client);
        var entry = Assert.Single(log)!;
        Assert.Equal("handshake", entry["kind"]!.GetValue<string>());
        Assert.Equal(id, entry["subscription_id"]!.GetValue<string>());
        Assert.Equal(Token, entry["endpoint_token"]!.GetValue<string>());
        Assert.Equal(3, entry["attempts"]!.GetValue<int>());
        Assert.Equal(200, entry["status"]!.GetValue<int>());
        Assert.True(entry["delivered"]!.GetValue<bool>());

        await Task.Delay(800);
        Assert.Equal(3, factory.Sink.Received.Count);
        Assert.All(factory.Sink.Received, n => Assert.Equal("handshake", n.Kind));
    }

    [Fact]
    public async Task Delivery_stops_after_the_maximum_retry_window()
    {
        await using var factory = new PasAppFactory(new Dictionary<string, string> { ["PAS_NOTIFICATION_MAX_SECONDS"] = "0.6" });
        factory.Sink.Respond = _ => 500;
        using var client = factory.Client();

        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/Subscription", Subscription("v201"));
        await PasAppFactory.EventuallyAsync(() => Task.FromResult(factory.Sink.Received.Count >= 2), "retries");
        await Task.Delay(1200);
        var attempts = factory.Sink.Received.Count;
        await Task.Delay(800);
        Assert.Equal(attempts, factory.Sink.Received.Count);

        var entry = Assert.Single(await NotificationsAsync(client))!;
        Assert.False(entry["delivered"]!.GetValue<bool>());
        Assert.Equal(500, entry["status"]!.GetValue<int>());
        Assert.Equal("requested", (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"/v201/fhir/Subscription/{created.Body!["id"]}")).Body!["status"]!.GetValue<string>());
    }

    [Fact]
    public async Task Event_notification_waits_for_a_listener_without_duplicates()
    {
        await using var factory = new PasAppFactory();
        using var client = factory.Client();
        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", Subscription("v221"));
        await ActiveAsync(client, "v221", created.Body!["id"]!.GetValue<string>());

        // No test is waiting yet: the listener answers 500 twice, then accepts.
        var handshakes = factory.Sink.Received.Count;
        factory.Sink.Respond = n => n <= handshakes + 2 ? 500 : 200;
        await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", TestPaths.TestData("v221", "pended_submit.json"));
        await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize");

        await factory.Sink.WaitForAsync(n => n.Kind == "event-notification" && n.AnsweredStatus == 200);
        await Task.Delay(800);
        var events = factory.Sink.Received.Where(n => n.Kind == "event-notification").ToList();
        Assert.Equal(3, events.Count);
        Assert.Equal(1, events.Count(n => n.AnsweredStatus == 200));
        Assert.Equal(handshakes, factory.Sink.Received.Count(n => n.Kind == "handshake"));
    }

    [Fact]
    public async Task Pended_claims_are_finalized_automatically_after_the_configured_delay()
    {
        await using var factory = new PasAppFactory(new Dictionary<string, string> { ["PAS_PENDED_AUTO_FINALIZE_SECONDS"] = "0.5" });
        using var client = factory.Client();
        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/Subscription", Subscription("v201"));
        await ActiveAsync(client, "v201", created.Body!["id"]!.GetValue<string>());

        var submitted = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/Claim/$submit", TestPaths.TestData("v201", "pended_submit.json"));
        Assert.Equal("queued", submitted.Body!["entry"]![0]!["resource"]!["outcome"]!.GetValue<string>());

        var notification = await factory.Sink.WaitForAsync(n => n.Kind == "event-notification");
        var focus = notification.Body["entry"]![1]!["fullUrl"]!.GetValue<string>();
        var read = await PasAppFactory.SendAsync(client, HttpMethod.Get, new Uri(focus).AbsolutePath);
        Assert.Equal("complete", read.Body!["outcome"]!.GetValue<string>());

        var finalize = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize", new JsonObject { ["version"] = "v201" });
        Assert.Empty(finalize.Body!["finalized"]!.AsArray());
    }

    [Fact]
    public async Task Subscriptions_only_receive_events_for_matching_requestors()
    {
        await using var factory = new PasAppFactory();
        using var client = factory.Client();
        var matching = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", Subscription("v221", "org-identifier=http://hl7.org/fhir/sid/us-npi|1745023006"));
        var other = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Subscription", Subscription("v221", "org-identifier=0000000000"));
        var otherVersion = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v201/fhir/Subscription", Subscription("v201"));
        await ActiveAsync(client, "v221", matching.Body!["id"]!.GetValue<string>());
        await ActiveAsync(client, "v221", other.Body!["id"]!.GetValue<string>());
        await ActiveAsync(client, "v201", otherVersion.Body!["id"]!.GetValue<string>());

        await PasAppFactory.SendAsync(client, HttpMethod.Post, "/v221/fhir/Claim/$submit", TestPaths.TestData("v221", "pended_submit.json"));
        var finalize = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize");
        Assert.Equal(1, finalize.Body!["notifications"]!.GetValue<int>());

        var notification = await factory.Sink.WaitForAsync(n => n.Kind == "event-notification");
        Assert.EndsWith(matching.Body["id"]!.GetValue<string>(), notification.SubscriptionReference);

        // The nested ClaimResponse follows the notification copy rule (SPEC 7.4).
        var claimResponse = notification.Body["entry"]![1]!["resource"]!["entry"]![0]!["resource"]!;
        Assert.DoesNotContain(claimResponse["extension"]!.AsArray(), x => x!["url"]!.GetValue<string>().EndsWith("extension-itemAuthorizedProvider", StringComparison.Ordinal));
        Assert.All(claimResponse["item"]!.AsArray(), item =>
            Assert.DoesNotContain(item!["extension"]!.AsArray(), x => x!["url"]!.GetValue<string>().EndsWith("extension-communicatedDiagnosis", StringComparison.Ordinal)));
        Assert.Contains(claimResponse["adjudication"]![0]!["extension"]![0]!["extension"]!.AsArray(), x => x!["url"]!.GetValue<string>() == "secondSurgicalOpinionFlag");
    }
}
