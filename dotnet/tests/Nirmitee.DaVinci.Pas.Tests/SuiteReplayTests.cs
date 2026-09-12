using System.Text.Json.Nodes;
using Nirmitee.DaVinci.Pas.Tests.Support;

namespace Nirmitee.DaVinci.Pas.Tests;

/// <summary>
/// Replays the synthetic testdata in Inferno suite order (SPEC section 2) against the in-memory host and compares
/// every response and notification with the golden examples produced by the reference model, plus the offline
/// structure, decision, update, inquiry and notification checks.
/// </summary>
public sealed class SuiteReplayTests
{
    [Theory]
    [InlineData("v221")]
    [InlineData("v201")]
    public async Task Suite_replay_matches_the_reference_examples(string version)
    {
        await using var factory = new PasAppFactory();
        using var client = factory.Client();
        var fhir = $"/{version}/fhir";
        var expected = TestPaths.TestData(version, "expected.json");
        JsonNode Example(string name) => TestPaths.Example(version, name);

        // 1. CapabilityStatement
        var metadata = await PasAppFactory.SendAsync(client, HttpMethod.Get, fhir + "/metadata");
        Assert.Equal(200, metadata.Code);
        Assert.Equal("application/fhir+json", metadata.MediaType);
        Golden.AssertSame(Example("metadata.json"), metadata.Body, "metadata");

        // 1. Subscription Setup: 201 requested, then a handshake, then active
        var subscriptionRequest = Example("subscription-create-request.json");
        var created = await PasAppFactory.SendAsync(client, HttpMethod.Post, fhir + "/Subscription", subscriptionRequest);
        Assert.Equal(201, created.Code);
        Golden.AssertSame(Example("subscription-create-response.json"), created.Body, "subscription create");
        var subscriptionId = created.Body!["id"]!.GetValue<string>();
        Assert.Equal($"{PasAppFactory.InfernoOrigin}{fhir}/Subscription/{subscriptionId}", created.Header("Location"));

        var handshake = await factory.Sink.WaitForAsync(n => n.Kind == "handshake");
        var handshakeExample = Example("notification-handshake.json");
        Golden.AssertSame(handshakeExample["body"], handshake.Body, "handshake");
        AssertHeaders(handshakeExample["headers"]!, handshake);
        Assert.Empty(PasChecks.Notification(handshake, "handshake", null, created.Body!));
        await PasAppFactory.EventuallyAsync(async () =>
            (await PasAppFactory.SendAsync(client, HttpMethod.Get, $"{fhir}/Subscription/{subscriptionId}")).Body?["status"]?.GetValue<string>() == "active",
            "the Subscription to become active");

        // 2. Approval, denial, pended
        foreach (var (name, example) in new[] { ("approval_submit", "approval-submit-response.json"), ("denial_submit", "denial-submit-response.json"), ("pended_submit", "pended-submit-response.json") })
        {
            await SubmitAsync(client, version, name, TestPaths.TestData(version, name + ".json"), expected[name]!, Example(example));
        }

        // 2.3 finalize and event notification
        var finalize = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize", new JsonObject { ["version"] = version });
        Assert.Equal(200, finalize.Code);
        Assert.Single(finalize.Body!["finalized"]!.AsArray());
        Assert.Equal(1, finalize.Body["notifications"]!.GetValue<int>());

        var again = await PasAppFactory.SendAsync(client, HttpMethod.Post, "/admin/pended/finalize", new JsonObject { ["version"] = version });
        Assert.Empty(again.Body!["finalized"]!.AsArray());

        var content = version == "v221" ? "full-resource" : "id-only";
        var notification = await factory.Sink.WaitForAsync(n => n.Kind == "event-notification");
        var notificationExample = Example("notification-event.json");
        Golden.AssertSame(notificationExample["body"], notification.Body, "event notification");
        AssertHeaders(notificationExample["headers"]!, notification);
        Assert.Empty(PasChecks.Notification(notification, "event-notification", content, created.Body!));

        if (version == "v221")
        {
            var focusBundle = notification.Body["entry"]![1]!["resource"]!;
            Assert.Equal(PasChecks.ExpectedItems(new JsonObject { ["items"] = expected["pended_submit"]!["after_finalize"]!.DeepClone() }), PasChecks.Decisions(focusBundle));
        }
        else
        {
            var focus = notification.Body["entry"]![1]!["fullUrl"]!.GetValue<string>();
            var read = await PasAppFactory.SendAsync(client, HttpMethod.Get, new Uri(focus).AbsolutePath);
            Assert.Equal(200, read.Code);
            Assert.Equal("ClaimResponse", read.Body!["resourceType"]!.GetValue<string>());

            var inquiry = await PasAppFactory.SendAsync(client, HttpMethod.Post, fhir + "/Claim/$inquire", TestPaths.TestData(version, "pended_inquire.json"));
            Assert.Equal(200, inquiry.Code);
            Golden.AssertSame(Example("pended-inquire-response.json"), inquiry.Body, "pended inquire");
            Assert.Equal(PasChecks.ExpectedItems(expected["pended_inquire"]!), PasChecks.Decisions(inquiry.Body!));
        }

        // 2.4 Claim updates (v2.2.1)
        if (version == "v221")
        {
            foreach (var step in new[] { "claim_update_initial_submit", "claim_update_add_item_submit", "claim_update_modify_cancel_submit", "claim_update_cancel_all_submit" })
            {
                var request = TestPaths.TestData(version, step + ".json");
                var example = step.Replace("_submit", string.Empty).Replace('_', '-') + "-response.json";
                var response = await SubmitAsync(client, version, step, request, expected[step]!, Example(example));
                var submitted = request["entry"]![0]!["resource"]!["item"]!.AsArray().Select(i => i!["sequence"]!.ToJsonString()).ToList();
                var returned = response["entry"]![0]!["resource"]!["item"]!.AsArray().Select(i => i!["itemSequence"]!.ToJsonString()).ToList();
                Assert.Equal(submitted, returned);
            }
        }

        // 3.1 must support $submit
        var msSubmit = TestPaths.TestData(version, "must_support_submit.json").AsArray();
        for (var i = 0; i < msSubmit.Count; i++)
        {
            await SubmitAsync(client, version, $"must_support_submit[{i}]", msSubmit[i]!, expected["must_support_submit"]![i]!, Example($"must-support-submit-response-{i + 1}.json"));
        }

        // 3.2 must support $inquire
        var msInquire = TestPaths.TestData(version, "must_support_inquire.json").AsArray();
        var replacedSeen = false;
        for (var i = 0; i < msInquire.Count; i++)
        {
            var reply = await PasAppFactory.SendAsync(client, HttpMethod.Post, fhir + "/Claim/$inquire", msInquire[i]);
            Assert.Equal(200, reply.Code);
            Golden.AssertSame(Example($"must-support-inquire-response-{i + 1}.json"), reply.Body, $"must_support_inquire[{i}]");
            var bundle = version == "v221" ? UnwrapReturn(reply.Body!) : reply.Body!;
            Assert.Equal(PasChecks.ExpectedItems(expected["must_support_inquire"]![i]!), PasChecks.Decisions(bundle));
            Assert.Empty(PasChecks.ResponseStructure(bundle, msInquire[i]!, "inquire"));

            var claimResponse = bundle["entry"]![0]!["resource"]!;
            Assert.True(claimResponse["request"]?["reference"] is not null || claimResponse["request"]?["extension"] is not null, "spec-44");
            foreach (var (original, replacement) in expected["must_support_inquire"]![i]!["replacedAuthorization"]!.AsObject())
            {
                var numbers = claimResponse["item"]!.AsArray()
                    .SelectMany(item => item!["extension"]!.AsArray())
                    .Where(x => x!["url"]!.GetValue<string>().EndsWith("extension-authorizationNumber", StringComparison.Ordinal))
                    .Select(x => x!["valueString"]!.GetValue<string>())
                    .ToList();
                replacedSeen |= numbers.Contains(replacement!.GetValue<string>()) && !numbers.Contains(original);
            }
        }

        Assert.True(replacedSeen, "spec-47: an inquiry response must return a replaced authorization number");

        // 4. Error handling: the kit's empty Bundle
        var empty = new JsonObject { ["resourceType"] = "Bundle", ["type"] = "collection", ["entry"] = new JsonArray() };
        foreach (var operation in new[] { "submit", "inquire" })
        {
            var reply = await PasAppFactory.SendAsync(client, HttpMethod.Post, $"{fhir}/Claim/${operation}", empty);
            var example = Example($"error-{operation}-response.json");
            Assert.Equal(example["status"]!.GetValue<int>(), reply.Code);
            Golden.AssertSame(example["body"], reply.Body, $"error {operation}");
        }
    }

    private static async Task<JsonNode> SubmitAsync(HttpClient client, string version, string label, JsonNode request, JsonNode expected, JsonNode example)
    {
        var reply = await PasAppFactory.SendAsync(client, HttpMethod.Post, $"/{version}/fhir/Claim/$submit", request);
        Assert.True(reply.Code == 200, $"{label}: HTTP {reply.Code} {reply.Text}");
        Assert.Equal("application/fhir+json", reply.MediaType);
        Golden.AssertSame(example, reply.Body, label);
        Assert.Equal(PasChecks.ExpectedItems(expected), PasChecks.Decisions(reply.Body!));
        Assert.Empty(PasChecks.ResponseStructure(reply.Body!, request, "submit"));
        return reply.Body!;
    }

    private static JsonNode UnwrapReturn(JsonNode parameters)
    {
        Assert.Equal("Parameters", parameters["resourceType"]!.GetValue<string>());
        return parameters["parameter"]!.AsArray().Single(p => p!["name"]!.GetValue<string>() == "return")!["resource"]!;
    }

    private static void AssertHeaders(JsonNode expected, CapturedNotification notification)
    {
        foreach (var (name, value) in expected.AsObject())
        {
            Assert.Equal(value!.GetValue<string>(), notification.Headers.GetValueOrDefault(name));
        }
    }
}
