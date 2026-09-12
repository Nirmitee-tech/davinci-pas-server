using System.Collections.Concurrent;
using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Mvc.Testing;
using Microsoft.AspNetCore.TestHost;
using Microsoft.Extensions.DependencyInjection;
using Nirmitee.DaVinci.Pas.Notifications;

namespace Nirmitee.DaVinci.Pas.Tests.Support;

/// <summary>Locates spec/ and testdata/ above the test output directory.</summary>
public static class TestPaths
{
    private static readonly Lazy<string> Root = new(() =>
    {
        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
        {
            if (File.Exists(Path.Combine(dir.FullName, "spec", "SPEC.md")))
            {
                return dir.FullName;
            }
        }

        throw new InvalidOperationException("Could not find spec/SPEC.md above the test output directory.");
    });

    public static string RepoRoot => Root.Value;

    public static string RulesFile => Path.Combine(RepoRoot, "spec", "adjudication-rules.json");

    public static JsonNode Example(string version, string name) => Load(Path.Combine(RepoRoot, "spec", "examples", version, name));

    public static JsonNode TestData(string version, string name) => Load(Path.Combine(RepoRoot, "testdata", version, name));

    public static JsonNode Load(string path) => JsonNode.Parse(File.ReadAllText(path))!;
}

/// <summary>A notification POST captured by <see cref="NotificationSink"/>.</summary>
public sealed record CapturedNotification(string Url, IReadOnlyDictionary<string, string> Headers, JsonNode Body, int AnsweredStatus, DateTimeOffset At)
{
    /// <summary>handshake, event-notification, ... from the SubscriptionStatus type parameter.</summary>
    public string? Kind => Parameter("type")?["valueCode"]?.GetValue<string>();

    public string? SubscriptionReference => Parameter("subscription")?["valueReference"]?["reference"]?.GetValue<string>();

    private JsonNode? Parameter(string name) =>
        Body["entry"]?[0]?["resource"]?["parameter"]?.AsArray().FirstOrDefault(p => p?["name"]?.GetValue<string>() == name);
}

/// <summary>Stands in for the subscriber's rest-hook endpoint: records every POST and answers with <see cref="Respond"/>.</summary>
public sealed class NotificationSink : HttpMessageHandler
{
    private readonly ConcurrentQueue<CapturedNotification> _received = new();

    /// <summary>Status to answer for the n-th request (1-based) to any endpoint. Default 200.</summary>
    public Func<int, int> Respond { get; set; } = _ => 200;

    public IReadOnlyList<CapturedNotification> Received => _received.ToArray();

    public async Task<CapturedNotification> WaitForAsync(Func<CapturedNotification, bool> predicate, double seconds = 10)
    {
        var deadline = DateTime.UtcNow.AddSeconds(seconds);
        while (DateTime.UtcNow < deadline)
        {
            if (Received.FirstOrDefault(predicate) is { } match)
            {
                return match;
            }

            await Task.Delay(25);
        }

        throw new TimeoutException($"No matching notification within {seconds} s; received {_received.Count}.");
    }

    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var (name, values) in request.Headers)
        {
            headers[name] = string.Join(", ", values);
        }

        var text = string.Empty;
        if (request.Content is not null)
        {
            foreach (var (name, values) in request.Content.Headers)
            {
                headers[name] = string.Join(", ", values);
            }

            text = await request.Content.ReadAsStringAsync(cancellationToken);
        }

        var status = Respond(_received.Count + 1);
        _received.Enqueue(new CapturedNotification(request.RequestUri!.ToString(), headers, JsonNode.Parse(text)!, status, DateTimeOffset.UtcNow));
        return new HttpResponseMessage((HttpStatusCode)status);
    }
}

/// <summary>Hosts the real PAS host in memory with a temporary data directory and a capturing notification endpoint.</summary>
public class PasAppFactory : WebApplicationFactory<Program>
{
    public const string InfernoOrigin = "http://host.docker.internal:8085";

    private readonly IReadOnlyDictionary<string, string> _settings;

    public PasAppFactory()
        : this(new Dictionary<string, string>())
    {
    }

    internal PasAppFactory(IReadOnlyDictionary<string, string> settings)
    {
        _settings = settings;
        Directory.CreateDirectory(Root);
    }

    public NotificationSink Sink { get; } = new();

    public string Root { get; } = Path.Combine(Path.GetTempPath(), "nirmitee-pas-tests", Guid.NewGuid().ToString("N"));

    public string AuditDir => Path.Combine(Root, "pas-audit");

    protected override void ConfigureWebHost(IWebHostBuilder builder)
    {
        builder.UseEnvironment("Testing");
        builder.UseSetting("PAS_DATA_DIR", Root);
        builder.UseSetting("PAS_ADMIN_ENABLED", "true");
        builder.UseSetting("PAS_PENDED_AUTO_FINALIZE_SECONDS", "0");
        builder.UseSetting("PAS_NOTIFICATION_RETRY_SECONDS", "0.2");
        builder.UseSetting("PAS_NOTIFICATION_MAX_SECONDS", "10");
        builder.UseSetting("Pas:HandshakeDelaySeconds", "0.05");
        foreach (var (key, value) in _settings)
        {
            builder.UseSetting(key, value);
        }

        builder.ConfigureTestServices(services =>
            services.AddHttpClient(NotificationDeliveryService.HttpClientName)
                .ConfigurePrimaryHttpMessageHandler(() => Sink)
                .SetHandlerLifetime(Timeout.InfiniteTimeSpan));
    }

    public HttpClient Client(string origin = InfernoOrigin) =>
        CreateClient(new WebApplicationFactoryClientOptions { BaseAddress = new Uri(origin.TrimEnd('/') + "/"), AllowAutoRedirect = false });

    public static async Task<Reply> SendAsync(HttpClient client, HttpMethod method, string path, JsonNode? body = null, string? token = null,
        Action<HttpRequestMessage>? configure = null, string? rawBody = null)
    {
        using var request = new HttpRequestMessage(method, path);
        if (body is not null || rawBody is not null)
        {
            request.Content = new StringContent(rawBody ?? body!.ToJsonString(), Encoding.UTF8, "application/fhir+json");
        }

        if (token is not null)
        {
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);
        }

        configure?.Invoke(request);
        using var response = await client.SendAsync(request);
        var text = await response.Content.ReadAsStringAsync();
        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var (name, values) in response.Headers.Concat(response.Content.Headers))
        {
            headers[name] = string.Join(", ", values);
        }

        return new Reply(response.StatusCode, string.IsNullOrWhiteSpace(text) ? null : JsonNode.Parse(text), text,
            response.Content.Headers.ContentType?.MediaType, headers);
    }

    /// <summary>Polls until <paramref name="condition"/> holds.</summary>
    public static async Task EventuallyAsync(Func<Task<bool>> condition, string what, double seconds = 10)
    {
        var deadline = DateTime.UtcNow.AddSeconds(seconds);
        while (DateTime.UtcNow < deadline)
        {
            if (await condition())
            {
                return;
            }

            await Task.Delay(25);
        }

        Assert.Fail($"Timed out waiting for {what}.");
    }

    protected override void Dispose(bool disposing)
    {
        base.Dispose(disposing);
        try
        {
            Directory.Delete(Root, recursive: true);
        }
        catch (IOException)
        {
        }
        catch (UnauthorizedAccessException)
        {
        }
    }
}

public sealed record Reply(HttpStatusCode Status, JsonNode? Body, string Text, string? MediaType, IReadOnlyDictionary<string, string> Headers)
{
    public int Code => (int)Status;

    public string? Header(string name) => Headers.TryGetValue(name, out var value) ? value : null;
}

/// <summary>
/// Compares a server response with a golden example from spec/examples: same keys in the same order, same arrays and
/// values, after UUIDs are renamed by order of first appearance and dates and instants are masked.
/// </summary>
public static partial class Golden
{
    [GeneratedRegex("[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")]
    private static partial Regex Uuid();

    [GeneratedRegex(@"^\d{4}-\d{2}-\d{2}$")]
    private static partial Regex Date();

    [GeneratedRegex(@"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")]
    private static partial Regex Instant();

    public static void AssertSame(JsonNode? expected, JsonNode? actual, string label)
    {
        var difference = FirstDifference(Normalize(expected), Normalize(actual), "$");
        Assert.True(difference is null, $"{label}: {difference}");
    }

    public static JsonNode? Normalize(JsonNode? node) => Build(node, new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase));

    private static JsonNode? Build(JsonNode? node, Dictionary<string, string> uuids) => node switch
    {
        null => null,
        JsonObject obj => new JsonObject(obj.Select(p => KeyValuePair.Create(p.Key, Build(p.Value, uuids))).ToList()),
        JsonArray array => new JsonArray(array.Select(x => Build(x, uuids)).ToArray()),
        JsonValue value when value.GetValueKind() == JsonValueKind.String => JsonValue.Create(Mask(value.GetValue<string>(), uuids)),
        _ => node.DeepClone(),
    };

    private static string Mask(string text, Dictionary<string, string> uuids)
    {
        if (Date().IsMatch(text))
        {
            return "<date>";
        }

        if (Instant().IsMatch(text))
        {
            return "<instant>";
        }

        return Uuid().Replace(text, m =>
        {
            if (!uuids.TryGetValue(m.Value, out var name))
            {
                name = $"uuid-{uuids.Count + 1}";
                uuids[m.Value] = name;
            }

            return name;
        });
    }

    public static string? FirstDifference(JsonNode? expected, JsonNode? actual, string path)
    {
        switch (expected)
        {
            case null:
                return actual is null ? null : $"{path}: expected null, got {Describe(actual)}";
            case JsonObject expectedObject:
                if (actual is not JsonObject actualObject)
                {
                    return $"{path}: expected an object, got {Describe(actual)}";
                }

                foreach (var (key, value) in expectedObject)
                {
                    if (!actualObject.ContainsKey(key))
                    {
                        return $"{path}.{key}: missing (expected {Describe(value)})";
                    }

                    if (FirstDifference(value, actualObject[key], $"{path}.{key}") is { } difference)
                    {
                        return difference;
                    }
                }

                if (actualObject.Select(p => p.Key).FirstOrDefault(k => !expectedObject.ContainsKey(k)) is { } extra)
                {
                    return $"{path}.{extra}: unexpected {Describe(actualObject[extra])}";
                }

                var expectedKeys = expectedObject.Select(p => p.Key).ToList();
                var actualKeys = actualObject.Select(p => p.Key).ToList();
                return expectedKeys.SequenceEqual(actualKeys)
                    ? null
                    : $"{path}: key order [{string.Join(", ", expectedKeys)}] != [{string.Join(", ", actualKeys)}]";
            case JsonArray expectedArray:
                if (actual is not JsonArray actualArray)
                {
                    return $"{path}: expected an array, got {Describe(actual)}";
                }

                for (var i = 0; i < Math.Min(expectedArray.Count, actualArray.Count); i++)
                {
                    if (FirstDifference(expectedArray[i], actualArray[i], $"{path}[{i}]") is { } difference)
                    {
                        return difference;
                    }
                }

                return expectedArray.Count == actualArray.Count ? null : $"{path}: expected {expectedArray.Count} items, got {actualArray.Count}";
            case JsonValue expectedValue:
                if (actual is not JsonValue actualValue)
                {
                    return $"{path}: expected {Describe(expected)}, got {Describe(actual)}";
                }

                if (expectedValue.GetValueKind() == JsonValueKind.Number && actualValue.GetValueKind() == JsonValueKind.Number)
                {
                    return decimal.Parse(expectedValue.ToJsonString(), NumberStyles.Float, CultureInfo.InvariantCulture) ==
                           decimal.Parse(actualValue.ToJsonString(), NumberStyles.Float, CultureInfo.InvariantCulture)
                        ? null
                        : $"{path}: expected {Describe(expected)}, got {Describe(actual)}";
                }

                return JsonNode.DeepEquals(expectedValue, actualValue) ? null : $"{path}: expected {Describe(expected)}, got {Describe(actual)}";
            default:
                return null;
        }
    }

    private static string Describe(JsonNode? node)
    {
        var text = node?.ToJsonString() ?? "null";
        return text.Length > 300 ? text[..300] + "..." : text;
    }
}

/// <summary>Ports of the offline checks in spec/tools/pas_checks.py and spec/reference/pas_reference.py.</summary>
public static partial class PasChecks
{
    private const string Pas = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/";

    [GeneratedRegex(@"^[a-zA-Z][a-zA-Z0-9+.-]*:")]
    private static partial Regex AbsoluteScheme();

    /// <summary>Item sequence to review action code of the first ClaimResponse.</summary>
    public static Dictionary<string, string> Decisions(JsonNode bundle)
    {
        var items = new Dictionary<string, string>();
        foreach (var entry in bundle["entry"]!.AsArray())
        {
            var resource = entry?["resource"];
            if (resource?["resourceType"]?.GetValue<string>() != "ClaimResponse")
            {
                continue;
            }

            foreach (var item in resource["item"]?.AsArray() ?? [])
            {
                foreach (var adjudication in item!["adjudication"]?.AsArray() ?? [])
                {
                    foreach (var extension in adjudication!["extension"]?.AsArray() ?? [])
                    {
                        foreach (var sub in extension!["extension"]?.AsArray() ?? [])
                        {
                            if (sub!["url"]?.GetValue<string>() == Pas + "extension-reviewActionCode")
                            {
                                items[item["itemSequence"]!.ToJsonString()] = sub["valueCodeableConcept"]!["coding"]![0]!["code"]!.GetValue<string>();
                            }
                        }
                    }
                }
            }
        }

        return items;
    }

    public static Dictionary<string, string> ExpectedItems(JsonNode expected) =>
        expected["items"]!.AsObject().ToDictionary(p => p.Key, p => p.Value!.GetValue<string>());

    /// <summary>cross_suite/pas_bundle_validation.rb: first entry ClaimResponse, references resolve exactly once, echoed resources keep fullUrl, id and identifier.</summary>
    public static List<string> ResponseStructure(JsonNode response, JsonNode request, string operation)
    {
        var errors = new List<string>();
        var entries = response["entry"]!.AsArray().OfType<JsonObject>().ToList();
        if (entries.Count == 0)
        {
            return ["Response Bundle has no entries"];
        }

        var first = entries[0]["resource"];
        if (first?["resourceType"]?.GetValue<string>() != "ClaimResponse")
        {
            errors.Add("The first bundle entry must be a ClaimResponse");
        }

        if (operation != "submit")
        {
            return errors;
        }

        var baseUrl = Fhir.References.ExtractBaseUrl(entries[^1]["fullUrl"]?.GetValue<string>());
        Walk(first, baseUrl, entries, errors, false, 0);
        foreach (var responseEntry in entries)
        {
            var responseResource = responseEntry["resource"];
            foreach (var requestEntry in request["entry"]!.AsArray().OfType<JsonObject>())
            {
                var requestResource = requestEntry["resource"];
                if (requestResource?["resourceType"]?.GetValue<string>() != responseResource?["resourceType"]?.GetValue<string>())
                {
                    continue;
                }

                var sameUrl = Str(requestEntry["fullUrl"]) is { Length: > 0 } u && u == Str(responseEntry["fullUrl"]);
                var sameId = Str(requestResource?["id"]) is { Length: > 0 } id && id == Str(responseResource?["id"]);
                var sameIdentifier = requestResource?["identifier"] is { } identifier && JsonNode.DeepEquals(identifier, responseResource?["identifier"]);
                if (!(sameUrl || sameId || sameIdentifier))
                {
                    continue;
                }

                if (!(Str(requestEntry["fullUrl"]) == Str(responseEntry["fullUrl"]) && Str(requestResource?["id"]) == Str(responseResource?["id"]) &&
                      JsonNode.DeepEquals(requestResource?["identifier"], responseResource?["identifier"])))
                {
                    errors.Add($"{Str(responseResource?["resourceType"])}/{Str(responseResource?["id"])} echoed with a different fullUrl or identifiers");
                }

                break;
            }
        }

        return errors;
    }

    private static void Walk(JsonNode? node, string baseUrl, List<JsonObject> entries, List<string> errors, bool skipClaimRelated, int depth)
    {
        if (depth > 40)
        {
            errors.Add("Reference traversal exceeded depth 40");
            return;
        }

        switch (node)
        {
            case JsonArray array:
                foreach (var child in array)
                {
                    Walk(child, baseUrl, entries, errors, skipClaimRelated, depth + 1);
                }

                break;
            case JsonObject obj:
                if (Str(obj["reference"]) is { Length: > 0 } reference)
                {
                    var absolute = baseUrl.Length == 0 || reference.StartsWith("urn:uuid:", StringComparison.Ordinal) || AbsoluteScheme().IsMatch(reference)
                        ? reference
                        : $"{baseUrl}/{reference}";
                    var matches = entries.Where(e => Str(e["fullUrl"]) == absolute).ToList();
                    if (matches.Count != 1)
                    {
                        errors.Add($"The referenced {absolute} resource SHALL appear exactly once in the Bundle, but found {matches.Count}.");
                    }

                    if (matches.Count > 0)
                    {
                        Walk(matches[0]["resource"], baseUrl, entries, errors, true, depth + 1);
                    }

                    return;
                }

                var type = Str(obj["resourceType"]);
                foreach (var (key, value) in obj)
                {
                    if (key.StartsWith('_') || key == "resourceType" || (key == "request" && type == "ClaimResponse") ||
                        (key == "related" && type == "Claim" && skipClaimRelated))
                    {
                        continue;
                    }

                    if (value is JsonObject or JsonArray)
                    {
                        Walk(value, baseUrl, entries, errors, skipClaimRelated, depth + 1);
                    }
                }

                break;
        }
    }

    /// <summary>check_notification from the reference model (handshake and event-notification rules).</summary>
    public static List<string> Notification(CapturedNotification message, string kind, string? content, JsonNode subscription)
    {
        var errors = new List<string>();
        var body = message.Body;
        if (Str(body["type"]) != "history")
        {
            errors.Add($"{kind}: Bundle.type must be history");
        }

        var entries = body["entry"]?.AsArray().OfType<JsonObject>().ToList() ?? [];
        if (entries.Count == 0)
        {
            return [.. errors, $"{kind}: notification Bundle is empty"];
        }

        var id = Str(subscription["id"])!;
        errors.AddRange(entries.Where(e => e["request"] is null || e["response"] is null).Select(_ => $"{kind}: every entry needs request and response"));
        if (!(Str(entries[0]["request"]?["url"]) ?? string.Empty).EndsWith($"Subscription/{id}/$status", StringComparison.Ordinal))
        {
            errors.Add($"{kind}: request.url must end with Subscription/{id}/$status");
        }

        var parameters = entries[0]["resource"]?["parameter"]?.AsArray().OfType<JsonObject>().ToList() ?? [];
        string? Param(string name, string value) => Str(parameters.FirstOrDefault(p => Str(p["name"]) == name)?[value]);
        if (Param("type", "valueCode") != kind)
        {
            errors.Add($"{kind}: type parameter is {Param("type", "valueCode")}");
        }

        var expectedStatus = kind == "handshake" ? "requested" : "active";
        if (Param("status", "valueCode") != expectedStatus)
        {
            errors.Add($"{kind}: status must be {expectedStatus}");
        }

        var subscriptionReference = Str(parameters.FirstOrDefault(p => Str(p["name"]) == "subscription")?["valueReference"]?["reference"]) ?? string.Empty;
        if (subscriptionReference.Split('/')[^1] != id)
        {
            errors.Add($"{kind}: subscription reference must end with the Subscription id");
        }

        if (kind != "event-notification")
        {
            return errors;
        }

        var payload = Str(subscription["channel"]?["payload"]);
        if (payload is not null && !(message.Headers.GetValueOrDefault("Content-Type") ?? string.Empty).StartsWith(payload, StringComparison.Ordinal))
        {
            errors.Add("event-notification: Content-Type must start with channel.payload");
        }

        foreach (var header in subscription["channel"]?["header"]?.AsArray().Select(Str).OfType<string>() ?? [])
        {
            var separator = header.IndexOf(": ", StringComparison.Ordinal);
            var name = separator < 0 ? header : header[..separator];
            var value = separator < 0 ? string.Empty : header[(separator + 2)..];
            if (message.Headers.GetValueOrDefault(name) != value)
            {
                errors.Add($"event-notification: header {name} must equal {value}");
            }
        }

        var focus = parameters.FirstOrDefault(p => Str(p["name"]) == "notification-event")?["part"]?.AsArray()
            .FirstOrDefault(p => Str(p?["name"]) == "focus")?["valueReference"]?["reference"];
        if (Str(focus) is not { } focusReference)
        {
            errors.Add("event-notification: focus required");
            return errors;
        }

        var rest = entries.Skip(1).ToList();
        if (content == "full-resource")
        {
            var target = rest.FirstOrDefault(e => Str(e["fullUrl"]) == focusReference);
            if (target?["resource"] is not JsonObject focusBundle || Str(focusBundle["resourceType"]) != "Bundle" || Str(focusBundle["type"]) != "collection")
            {
                errors.Add("event-notification: focus must resolve to a PAS Response Bundle entry");
            }
            else if (!focusBundle["entry"]!.AsArray().Any(e => Str(e?["resource"]?["resourceType"]) == "ClaimResponse"))
            {
                errors.Add("event-notification: focus Bundle must contain a ClaimResponse");
            }
        }
        else
        {
            errors.AddRange(rest.Where(e => e["resource"] is not null).Select(_ => "id-only: entries must not contain resource"));
            errors.AddRange(rest.Where(e => Str(e["fullUrl"]) is null).Select(_ => "id-only: entries must contain fullUrl"));
        }

        return errors;
    }

    public static string? Str(JsonNode? node) => node is JsonValue value && value.GetValueKind() == JsonValueKind.String ? value.GetValue<string>() : null;
}
